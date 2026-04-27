package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Config holds all runtime configuration.
type Config struct {
	ArbitrumRPC    string `yaml:"arbitrum_rpc"`
	L1RPC          string `yaml:"l1_rpc"`
	DBPath         string `yaml:"db_path"`          // absolute path to arb.db
	UniswapAPIKey  string `yaml:"uniswap_api_key"`  // Uniswap gateway API key

	// PauseTracking stops the swap-event-driven cycle scorer. Swap events
	// still update pool state (so the graph stays warm), but fastEvalCycles
	// returns immediately — no cycle scoring, no candidate evaluation, no
	// arb_log entries. Used while tweaking the pool set / cycle cache
	// without a flood of re-scoring noise. Checked inside fastEvalCycles.
	PauseTracking bool `yaml:"pause_tracking"`

	// PauseTrading lets the scorer run normally but blocks submission at
	// trySubmitCandidate. Candidates are still logged to arb_observations
	// with reject_reason="paused: trading" so you can see what would have
	// been submitted. Flip this to true while iterating on pool quality /
	// filter rules without paying for gas on every experiment.
	PauseTrading bool `yaml:"pause_trading"`

	TestMode bool `yaml:"test_mode"`

	Major struct {
		Tokens []struct {
			Address  string `yaml:"address"`
			Symbol   string `yaml:"symbol"`
			Decimals uint8  `yaml:"decimals"`
		} `yaml:"tokens"`
		Pools []string `yaml:"pools"`
	} `yaml:"major"`

	// CexDex configures the split-route CEX-DEX arbitrage bot (cmd/cexdex).
	// It shares this config file with the main cyclic-arb bot.
	CexDex struct {
		// Enable the CEX-DEX bot. Set false to disable without removing config.
		Enabled bool `yaml:"enabled"`

		// Binance WebSocket base URL.
		// Default: wss://stream.binance.com:9443/stream
		BinanceWS string `yaml:"binance_ws"`

		// Binance trading pairs to subscribe to (e.g. ["ETHUSDT","ARBUSDT"]).
		Pairs []string `yaml:"pairs"`

		// Minimum spread between effective DEX price and CEX mid price (in bps)
		// before attempting a trade. Covers fees + slippage.
		// Default: 15 (0.15%)
		MinSpreadBps float64 `yaml:"min_spread_bps"`

		// Maximum USD value to borrow per trade. Actual size is also capped at
		// 5% of the buy pool's liquidity.
		// Default: 50000
		MaxTradeUSD float64 `yaml:"max_trade_usd"`

		// Maximum number of pools in the sell-split leg.
		// Default: 4
		MaxSellHops int `yaml:"max_sell_hops"`

		// Minimum net profit in USD (after gas) to submit a transaction.
		// Default: 1.0
		MinProfitUSD float64 `yaml:"min_profit_usd"`

		// Estimated gas cost in USD for a split-arb transaction (~800k gas).
		// Used to filter unprofitable opportunities before submission.
		// Default: 0.50
		GasCostUSD float64 `yaml:"gas_cost_usd"`

		// How often to scan for opportunities (milliseconds).
		// Default: 500
		ScanIntervalMs int `yaml:"scan_interval_ms"`

		// Minimum seconds between consecutive submissions.
		// Default: 2
		CooldownSecs int `yaml:"cooldown_secs"`

		// Deployed address of SplitArb.sol on Arbitrum.
		// Leave empty to run in monitor-only mode.
		ExecutorContract string `yaml:"executor_contract"`
	} `yaml:"cex_dex"`

	Wallet struct {
		Address string `yaml:"address"`
	} `yaml:"wallet"`

	Pools struct {
		MinTVLUSD      float64            `yaml:"min_tvl_usd"`
		MinVolume24h   float64            `yaml:"min_volume_24h_usd"`
		// AbsoluteMinTVLUSD is a HARD floor that even ForceAdd-path pools
		// (subgraph seeds, pinned addresses, competitor-watcher auto-adds)
		// must clear. The regular MinTVLUSD only filters discovered pools
		// (PoolCreated events), not pools loaded from the subgraph or DB,
		// so without this floor a $580 TVL scam pool can sneak in via the
		// subgraph and pollute the cycle cache. Set to 0 to disable.
		// Default: 5000 — covers the Unishop.ai class ($580) plus the
		// long tail of sub-$5k pump-and-dump pools that have repeatedly
		// triggered "hop simulation returned 0" sim-rejects.
		AbsoluteMinTVLUSD float64 `yaml:"absolute_min_tvl_usd"`

		// Pool quality composite filter — extends the absolute TVL floor
		// with three independent signals that together catch low-quality /
		// pump-and-dump pools with minimal risk of rejecting legitimate
		// thin-liquidity pools.
		//
		// Each check is applied ONLY when the underlying field has been
		// populated (non-zero), so pools at ForceAdd time that haven't
		// been through multicall yet are not rejected prematurely. The
		// cyclecache rebuild + hourly prune pass both re-evaluate after
		// state populates.
		//
		// MinTickCount: minimum initialized-tick count for a V3/V4 pool.
		// Pools with fewer initialized ticks have almost no LP diversity
		// across the price range, so the multi-tick simulator can't walk
		// the swap correctly and the pool is effectively one-LP deep.
		// 0 = disabled. Default 30. Unishop.ai had 25.
		MinTickCount int `yaml:"min_tick_count"`

		// MinTickCountBypassTVLUSD: TVL above which the tick-count floor
		// is skipped. The tick-count gate catches single-LP trap pools,
		// but niche legitimate pairs (tBTC/USDC $27k, 5 ticks; seen in
		// competitor_arbs id 15982) still support the small notionals
		// competitors trade. TVL proxies trade capacity — above this
		// value, the pool has enough depth that a narrow tick distribution
		// is irrelevant. Below it, the tick-count filter still runs.
		// Default $1M. Lower (e.g. $10k) admits low-TVL niche pairs.
		MinTickCountBypassTVLUSD float64 `yaml:"min_tick_count_bypass_tvl_usd"`

		// HighFeeTierMinTVLUSD: the 1% (10000 ppm) fee tier is designed
		// for exotic / volatile pairs. Legitimate 1% pools still attract
		// meaningful LP capital because the high fee compensates for the
		// impermanent loss risk; a 1% fee pool with < $50k TVL is a
		// pump-and-dump signature. Pools below this threshold at the 1%
		// fee tier are rejected. Set to 0 to disable. Default 50000.
		HighFeeTierMinTVLUSD float64 `yaml:"high_fee_tier_min_tvl_usd"`

		// MinVolumeTVLRatio: minimum 24h volume / TVL ratio. Real pools
		// cycle 0.1-3x their TVL daily during normal activity. Below 0.01
		// (1%) is a dead pool — no organic users, no price movement, no
		// arb opportunities. We apply ONLY the dead-pool floor, not an
		// upper bound, because legitimate volatile pairs can spike to
		// 10x+ daily turnover during market events and we don't want to
		// reject them. Set to 0 to disable. Default 0.01.
		MinVolumeTVLRatio float64 `yaml:"min_volume_tvl_ratio"`
		// MinVolumeTVLRatioExemptTVLUSD: pools with TVL above this amount
		// are exempt from the dead-pool volume/TVL ratio check. The ratio
		// check misfires on multi-billion-dollar stable pools (USDC/USDT,
		// USDC/USDC.e at $6B+ TVL) which naturally sit at 0.01%-0.1% daily
		// turnover — below the 0.1% floor — but are among the highest-
		// activity pools on chain in absolute USD swap volume. Without
		// this exemption they get pruned, FetchTickMaps never fetches
		// their bitmaps (only cycle-cache pools are tracked), the sim
		// returns 0 on every cycle routing through them, and the bot
		// silently drops them. Empirically caught by the smoketest's
		// size-parameterized sim probes on 2026-04-18. Set to 0 to
		// disable the exemption. Default $50M.
		MinVolumeTVLRatioExemptTVLUSD float64 `yaml:"min_volume_tvl_ratio_exempt_tvl_usd"`
		PruneTVLUSD    float64            `yaml:"prune_tvl_usd"`
		PruneVolumeUSD float64            `yaml:"prune_volume_usd"`
		MaxHops        int                `yaml:"max_hops"`
		MaxTVLUSD      float64            `yaml:"max_pool_tvl_usd"`
		// Subgraph TVL/volume values above this are treated as garbage (clamped to 0).
		// Some indexers occasionally return absurd values (e.g. $1e22) when their price
		// feed for an exotic token is broken. Default 1e12 ($1T) — anything above is bogus.
		SanityMaxTVLUSD float64           `yaml:"sanity_max_tvl_usd"`
		Subgraph       SubgraphSeedConfig `yaml:"subgraph"`
		// PinnedPools is a list of pool addresses that the bot must always include
		// in its registry, even if their TVL/volume drops below pruning thresholds.
		// At startup, each address is resolved on-chain (factory→DEX, token0/1, fee)
		// and inserted into the SQLite pools table with pinned=1. This replaces the
		// legacy `seeds:` section which required hand-typing fee/token/dex per pool
		// and led to copy-paste errors that broke cycle detection.
		//
		// To add a new pool: paste its address here and restart. To remove: delete
		// the address (still in DB but no longer protected from pruning) or call
		// db.UnpinPool(addr).
		PinnedPools []string `yaml:"pinned_pools"`
	} `yaml:"pools"`

	Trading struct {
		MinProfitUSD       float64 `yaml:"min_profit_usd"`
		FlashLoanAmountUSD float64 `yaml:"flash_loan_amount_usd"`
		BalancerVault      string  `yaml:"balancer_vault"`
		// Aave V3 Pool address on Arbitrum. Used for flash loans when Balancer
		// and V3 pools don't cover the borrow token. Set to empty to disable.
		// Arbitrum mainnet: 0x794a61358D6845594F94dc1DB02A252b5b4814aD
		AavePoolAddress    string  `yaml:"aave_pool_address"`
		ExecutorContract   string  `yaml:"executor_contract"`
		// Specialized executor contracts, each tuned for a specific cycle shape.
		// When set, the bot will prefer them for matching cycles and fall through
		// to ExecutorContract (the full multi-source executor) for everything else.
		// Empty = disabled (that path is not used; all traffic stays on the full
		// contract). See pickExecutor() for the routing rules and
		// cfg.Strategy.ExecutorStrategies for per-contract LP-floor tuning.
		//
		// ExecutorV3Mini: V3-flash loan + 2-3 V3-family hops (UniV3/SushiV3/
		// RamsesV3/PancakeV3/CamelotV3/ZyberV3), direct pool.swap() calls, no
		// router overhead. Target gas ~350-400k vs ~900k for the full contract
		// — unlocks thinner-margin cycles that the full contract's gas floor
		// filters out. Deploy via contracts/script/DeployV3FlashMini.s.sol.
		ExecutorV3Mini     string  `yaml:"executor_v3_mini"`
		// ExecutorV4Mini: V3-flash + 2-5 V4-only hops, single PoolManager.unlock
		// for the whole cycle, native-ETH-aware. Cuts a 3-hop V4 cycle from
		// ~880k gas (generic) to ~280k. Cycles routing through V4-ETH pools
		// only work via this contract — the generic path still uses
		// IERC20.transfer which reverts on the zero-address currency. Empty =
		// disabled (V4 cycles fall through to the generic ArbitrageExecutor
		// and continue producing V4_HANDLER reverts).
		// Deploy via contracts/script/DeployV4Mini.s.sol.
		ExecutorV4Mini     string  `yaml:"executor_v4_mini"`
		// ExecutorMixedV3V4: 2-5 hop cycles where each hop is either UniV4
		// or any V3-family pool (UniV3/SushiV3/RamsesV3/PancakeV3/Camelot
		// V3/ZyberV3). Wraps the entire cycle in ONE PoolManager.unlock so
		// V4 hops use pm.swap+settle/take and V3 hops use direct pool.swap
		// (with the V3 swap callback running inside the same unlock
		// context). Closes the V4_HANDLER reject class on mixed-V4+V3
		// cycles that V4Mini can't take. Empty = disabled, those cycles
		// fall through to the generic ArbitrageExecutor (which still
		// reverts via the buggy unlockCallback). Deploy via
		// contracts/script/DeployMixedV3V4Executor.s.sol.
		ExecutorMixedV3V4  string  `yaml:"executor_mixed_v3v4"`
		// HookRegistry: shared V4 hook whitelist contract. When set, V4Mini
		// and MixedV3V4Executor both delegate `isAllowed(hook)` to this
		// address instead of their local `allowedHooks` maps. Lets the
		// off-chain hook-sync loop auto-whitelist safe hooks (no
		// `*ReturnDelta` permission bits) without per-executor migration.
		// Empty = local fallback mode. See internal/hookclass.go.
		HookRegistry       string  `yaml:"hook_registry"`
		// HookSyncIntervalSec: how often the hook sync loop scans live V4
		// pools, classifies new hooks, and pushes safe ones on-chain. 0 =
		// disabled.
		HookSyncIntervalSec int    `yaml:"hook_sync_interval_sec"`
		// Direct sequencer endpoint for lowest-latency tx submission.
		// Only accepts eth_sendRawTransaction. Falls back to ArbitrumRPC if empty.
		SequencerRPC string `yaml:"sequencer_rpc"`
		// Dedicated RPC for eth_call simulations. Keeps simulation traffic off the
		// live swap-tracking RPC to avoid rate-limit failures. Falls back to
		// ArbitrumRPC if empty. Use a paid Alchemy/Infura endpoint here.
		SimulationRPC string `yaml:"simulation_rpc"`
		// RPC for bulk tick data fetches (tickBitmap + ticks multicall).
		// Uses ~1700 calls every 5s — needs a high-rate-limit endpoint.
		// Falls back to SimulationRPC then ArbitrumRPC if empty.
		TickDataRPC string `yaml:"tick_data_rpc"`

		// Health gate: max age (seconds) of each subsystem before trades are blocked.
		// Set 0 to disable a specific check. Trade submission is halted if ANY
		// subsystem exceeds its max age — prevents trading on stale data.
		MaxTickAgeSec      int64 `yaml:"max_tick_age_sec"`      // default 30 — tick data must be <30s old
		MaxMulticallAgeSec int64 `yaml:"max_multicall_age_sec"` // default 15 — pool state must be <15s old
		MaxSwapAgeSec      int64 `yaml:"max_swap_age_sec"`      // default 10 — must have seen a swap event in last 10s
		// Per-pool STATE freshness gate (slot0 / liquidity / reserves / dynamic
		// fee). Measured in chain blocks behind the latest header seen by the
		// bot's block watcher: a cycle is rejected if any edge pool's StateBlock
		// is more than MaxPoolStateBlockLag blocks behind.
		//
		// On Arbitrum (~250 ms blocks) a value of 1 means "pool was fetched at
		// current block or current-1"; higher values tolerate multicall sweeps
		// that span more than one block during RPC back-pressure. 0 is
		// DISALLOWED — bot fatals at startup so the gate cannot silently turn
		// off. Replaces the deprecated time-based `pool_state_max_age_sec`.
		MaxPoolStateBlockLag uint64 `yaml:"max_pool_state_block_lag"`

		// Tick-bitmap coverage gate (V3 / V4 / Algebra). The bitmap is valid
		// iff the pool's current tick sits inside the fetched word window
		// [centerWord - TicksWordRadius, centerWord + TicksWordRadius] AND the
		// TicksBlock is no more than MaxPoolStateBlockLag behind latest. This
		// value doubles as the fetch radius used by FetchTickMaps, so
		// increasing it widens both the fetched range AND the eval gate
		// tolerance. 0 is DISALLOWED — bot fatals at startup. Replaces the
		// deprecated time-based `tick_pool_max_age_sec`.
		TickBitmapCoverageWords int `yaml:"tick_bitmap_coverage_words"`

		// Set to true ONLY when executor_contract has been redeployed with the
		// _swapPancakeV3 handler (DEX_PANCAKE_V3=8). When false, PancakeV3 hops
		// dispatch to the broken DEX_V3 handler — the failures are visible but
		// at least the routing is consistent with the deployed bytecode. Flipping
		// this to true against the OLD contract will misroute every PancakeV3
		// hop into the V2 fallback branch, breaking even the cycles that
		// currently work. See dexTypeOnChain in executor.go.
		ExecutorSupportsPancakeV3 bool `yaml:"executor_supports_pancake_v3"`

		// Eager tick re-fetch threshold: when a V3/V4 Swap event lands, the swap
		// listener compares the post-swap tick to the tick recorded the last time
		// FetchTickMaps populated this pool's bitmap (TickAtFetch). If the absolute
		// distance is ≥ this many tickSpacing units, the pool is sent to the eager
		// re-fetch channel for an immediate single-pool tick map refresh — bypassing
		// the 5s periodic sweep. Set to 0 to disable eager re-fetch entirely (rely
		// on the 5s periodic refresh only). Default: 8 — corresponds to about
		// one-third of the ±25-word bitmap radius (±25×256/spacing÷spacing) used by
		// FetchTickMaps; lower values fire more often (higher RPC load), higher
		// values risk the simulator running on a stale bitmap during fast moves.
		TickEagerRefetchSpacings int `yaml:"tick_eager_refetch_spacings"`
		// Per-pool rate limit for the eager re-fetch path. After firing an eager
		// re-fetch for a pool, suppress further eager re-fetches for that pool for
		// this many milliseconds. Prevents thrashing on pools that swap many times
		// per block (the periodic 5s sweep + the next eager fire after cooldown
		// will still cover them). Default: 750 ms — roughly 3 Arbitrum blocks.
		TickEagerRefetchCooldownMs int `yaml:"tick_eager_refetch_cooldown_ms"`

		// Buffer size of the TickRefetchCh channel (swap listener → tick fetch
		// loop). MUST be large enough that bursty markets don't drop sends
		// silently. A drop means a pool's bitmap window stays stale until the
		// next 5 s periodic sweep — up to 5 s of mis-simulated cycles routing
		// through that pool. Raised from 256 (previous hard-coded value) to
		// 2048 to match swap_logs_chan_cap. Set via config so it can be tuned
		// up under volatile markets without a code change. 0 = fatal at
		// startup (gate against silent regression to the hard-coded default).
		TickRefetchChanCap int `yaml:"tick_refetch_chan_cap"`

		// Maximum per-pool bitmap fetch radius (in 256-tick words). Must be
		// > tick_bitmap_coverage_words. The "auto-expand" path doubles the
		// fetch radius for hot pools whose tick has drifted past the basic
		// coverage bound; this cap keeps the RPC cost bounded. 0 disables
		// auto-expand (fall back to the single coverage radius). Default: 50.
		TickBitmapMaxAutoRadius int `yaml:"tick_bitmap_max_auto_radius"`

		// Maximum allowed block-height skew between the bot's ArbitrumRPC
		// (state + headers) and TickDataRPC (bitmap fetches). If the tick RPC
		// lags by more than this many blocks, the bot refuses to submit
		// trades because the bitmap was fetched on a state the sim doesn't
		// otherwise see. 0 disables the skew gate. Default: 3 (= ~750 ms).
		TickRPCMaxSkewBlocks int64 `yaml:"tick_rpc_max_skew_blocks"`

		// Read-only introspection HTTP server. Listens on 127.0.0.1:<port>
		// (loopback only — never bind 0.0.0.0). Exposes /debug/health,
		// /debug/pools, /debug/cycles, /debug/executor, /debug/swap-listener
		// and /debug/config so the test plan and external probes can read the
		// bot's in-memory state without going through SQLite. Set to 0 to
		// disable. Default: 0 (disabled). Recommended port: 6060.
		DebugHTTPPort int `yaml:"debug_http_port"`
	} `yaml:"trading"`

	Monitoring struct {
		InfluxURL      string `yaml:"influxdb_url"`
		InfluxToken    string `yaml:"influxdb_token"`
		InfluxOrg      string `yaml:"influxdb_org"`
		InfluxBucket   string `yaml:"influxdb_bucket"`
		TelegramToken  string `yaml:"telegram_token"`
		TelegramChatID string `yaml:"telegram_chat_id"`
	} `yaml:"monitoring"`

	YieldMonitoring struct {
		MinDivergenceBps float64        `yaml:"min_divergence_bps"`
		MinProfitUSD     float64        `yaml:"min_profit_usd"`
		Pairs            []YieldPairCfg `yaml:"pairs"`
	} `yaml:"yield_monitoring"`

	Factories struct {
		UniswapV3 string `yaml:"uniswap_v3"`
		Camelot   string `yaml:"camelot"`
		TraderJoe string `yaml:"trader_joe"`
		SushiSwap string `yaml:"sushiswap"`
	} `yaml:"factories"`

	Tokens []struct {
		Address  string `yaml:"address"`
		Symbol   string `yaml:"symbol"`
		Decimals uint8  `yaml:"decimals"`
	} `yaml:"tokens"`

	Strategy struct {
		// Throttle: skip cycle rescoring if the swapped pool's spot price moved
		// less than this in log space. 0.0005 ≈ 0.05%. Set 0 to disable.
		MinPriceDelta float64 `yaml:"min_price_delta"`

		// Sanity cap on log-profit: cycles with lp above this are rejected as
		// likely stale data (price feed lag, not a real opportunity).
		LPSanityCap float64 `yaml:"lp_sanity_cap"`

		// Reject simulated profit above this threshold (protects against
		// simulator bugs reporting impossibly large profits).
		MaxProfitUSDCap float64 `yaml:"max_profit_usd_cap"`

		// Maximum trade size as a fraction of the shallowest pool's TVL.
		// 0.15 = 15% of TVL — avoids excessive price impact.
		TVLTradeFraction float64 `yaml:"tvl_trade_cap_fraction"`

		// "Shallow pool" sizing override: pools whose TVL is below
		// ShallowPoolTVLUSD use ShallowPoolTradeFraction instead of
		// TVLTradeFraction when computing the maxAmountUSD cap. Shallow V3
		// pools have non-linear tick-crossing slippage that can move price
		// 50–100 bps on a $5–10k trade, which is enough to flip a positive
		// fast-eval result into a negative buildHops re-sim. Tightening this
		// fraction prevents the bot from sizing up into a shallow pool just
		// because it has high TVL on its OTHER hop.
		// Default: ShallowPoolTVLUSD=200000, ShallowPoolTradeFraction=0.03 (3%).
		// Set ShallowPoolTradeFraction=0 to disable the shallow override.
		ShallowPoolTVLUSD       float64 `yaml:"shallow_pool_tvl_usd"`
		ShallowPoolTradeFraction float64 `yaml:"shallow_pool_trade_fraction"`

		// Minimum seconds between consecutive transaction submissions.
		// Prevents hammering the sequencer during volatile periods.
		SubmitCooldownSecs int `yaml:"submit_cooldown_secs"`

		// How often (seconds) the cycle cache DFS rebuild runs.
		CycleRebuildSecs int `yaml:"cycle_rebuild_secs"`

		// Best pools per (tokenOut, DEX) pair kept during DFS adjacency building.
		// 1 = only the best pool per DEX per destination token.
		MaxDexPerDest int `yaml:"max_dex_per_dest"`

		// Max outgoing edges per token node after per-DEX filtering.
		// Bounds the DFS branching factor.
		MaxEdgesPerNode int `yaml:"max_edges_per_node"`

		// Seconds before a pool with no swap/multicall update has its edges
		// zeroed in the graph (treated as stale).
		PoolStalenessSecs int `yaml:"pool_staleness_secs"`

		// How many hours of arb observations to retain in SQLite.
		ArbLogRetentionHours int `yaml:"arb_log_retention_hours"`

		// Maximum rows in the arb_observations table (oldest pruned first).
		ArbLogMaxRows int `yaml:"arb_log_max_rows"`

		// On-chain minProfit divisor passed to the executor contract.
		// minProfitNative = simulatedProfit / divisor.
		// Set to 1 (default) to use 1 wei — avoids "profit below minimum" reverts
		// when price moves between eth_call and SendTransaction.
		// Set higher (e.g. 3) only if simulation accuracy improves significantly.
		ContractMinProfitDivisor int `yaml:"contract_min_profit_divisor"`

		// Per-hop slippage tolerance in basis points (1 bps = 0.01%) passed to the
		// executor contract as amountOutMin on each swap leg. If any pool moves more
		// than this between simulation and execution the tx reverts at the hop level
		// rather than at flash-loan repayment. The last hop is additionally floored
		// at amountIn so the contract can always repay Balancer regardless of slippage.
		// Default: 50 (0.5%). Tighten toward 10–20 bps once you trust the simulator.
		SlippageBps int `yaml:"slippage_bps"`

		// Skip the eth_call dry-run simulation for cycles whose estimated profit
		// exceeds this USD value. The contract reverts safely on-chain if the
		// opportunity has vanished — the gas cost of a failed tx is far less than
		// a missed $7+ opportunity. Set to 0 to always simulate (default).
		SkipSimAboveUSD float64 `yaml:"skip_sim_above_usd"`

		// SkipSimAboveBps: skip eth_call when the simulated profit margin (in bps)
		// exceeds this threshold. Complements SkipSimAboveUSD — either condition
		// triggers skip. Useful for fat-margin trades where the Go sim is highly
		// confident. Set to 0 to disable bps-based skipping.
		SkipSimAboveBps float64 `yaml:"skip_sim_above_bps"`

		// Competitor pool watcher: minimum profit (USD) in competitor_arbs that
		// triggers an on-chain pool lookup and auto-add to the live registry.
		// Set to 0 to disable.
		CompetitorWatchMinProfitUSD float64 `yaml:"competitor_watch_min_profit_usd"`

		// Multiplier applied to estimated gas cost to compute the dynamic minimum
		// profit threshold. E.g. 3.0 means profit must be ≥ 3× the gas cost of
		// the transaction. Lower values let smaller trades through; higher values
		// reserve the submission slot for genuinely profitable cycles.
		// Default: 3.0
		GasSafetyMult float64 `yaml:"gas_safety_mult"`

		// LastHopShortfallBps: how many basis points below the flash-borrow
		// amount the last hop's amountOutMin may go without reverting. 0 =
		// strict clamp at borrow (old behavior); any value > 0 accepts a
		// microscopic dust loss on trades where sim overestimated final
		// output, in exchange for NOT reverting when actual vs sim diverge
		// by <bps. SIM_PHANTOM analysis 2026-04-18 showed 100% of SIM_PHANTOM
		// reverts hit this last-hop clamp with fresh_bps within 1-9 bps of
		// sim_bps. Default 1 (= 0.01% shortfall = $0.01 on a $100 trade).
		// Clamped to 50 bps in SetLastHopShortfallBps.
		LastHopShortfallBps int64 `yaml:"last_hop_shortfall_bps"`

		// Absolute hard floor on log-profit (bps). Cycles below this are always
		// rejected regardless of the dynamic floor. Acts as a safety net for
		// misconfiguration. Set to 0 to rely entirely on the dynamic floor.
		// Default: 2.0.
		MinCycleLPBps float64 `yaml:"min_cycle_lp_bps"`

		// Dynamic LP floor: per-hop buffer parameters. The effective per-cycle
		// threshold is computed as Σ(base_bps[dex] × tvl_scale × vol_mult) + latency,
		// with a gas-price multiplier applied when the network is congested.
		DynamicLPFloor dynamicLPFloorConfig `yaml:"dynamic_lp_floor"`

		// Minimum simulated profit in basis points to submit. Cycles below this
		// are rejected even if profitable in USD — the margin is too thin to survive
		// latency between detection and on-chain execution. Set 0 to disable.
		// Default: 75 (0.75%).
		MinSimProfitBps float64 `yaml:"min_sim_profit_bps"`

		// Number of goroutines used to score and simulate candidate cycles
		// inside fastEvalCycles. Each candidate is independent until the
		// submission phase, so this parallelises the CPU-bound hot path that
		// runs on every swap event. Set to 0 to auto-use runtime.NumCPU().
		// Set to 1 to run serially (old behavior). Typical values: 4–16.
		FastEvalWorkers int `yaml:"fast_eval_workers"`

		// optimalAmountIn ternary-search controls. The search narrows a
		// bracket [minUSD, maxUSD] by splitting into thirds each iteration.
		// Each iteration costs one extra SimulateCycle call. Profit curves
		// for cyclic arb are strongly concave on log scale (the peak
		// typically sits near the smallest pool's "knee" notional, much
		// closer to $100-$1k than to $100k), so linear-scale search wastes
		// most iterations on the large-notional tail.
		//
		// OptimalAmountTernaryIterations: iteration count. (2/3)^N = residual
		// fraction of the initial bracket. 15 → 0.23%, 25 → 0.0024%, 30 →
		// 0.00053%. The 15 default missed thin-margin arbs like #15022 where
		// the peak is at $164 on a [1, 336000] bracket ($773 precision at 15
		// iters). MUST be > 0 (log.Fatal at startup otherwise). Default 25.
		OptimalAmountTernaryIterations int `yaml:"optimal_amount_ternary_iterations"`

		// OptimalAmountLogScale: when true, the ternary search operates in
		// log-of-notional space instead of linear. Profit peaks near the
		// shallowest pool's "knee" get found even when the bracket spans
		// 4+ orders of magnitude ($1 to $336k). Linear search wastes
		// iterations above $10k when the real peak is at $164. Default true.
		OptimalAmountLogScale bool `yaml:"optimal_amount_log_scale"`

		// OptimalAmountRefinement: when > 0, after ternary converges, run N
		// extra iterations of a golden-section refine around the best point
		// at 1/10th the bracket width. Pins the peak to sub-dollar precision
		// for small arbs. Set 0 to disable. Default 5.
		OptimalAmountRefinement int `yaml:"optimal_amount_refinement"`

		// Maximum number of candidates trySubmitCandidate may attempt per swap
		// event. Hard upper bound on the serial submission loop's wall-clock
		// budget — defends the swap-listener goroutine against starvation when
		// a volatile burst produces hundreds of profitable variants of the same
		// arb cluster. Combined with the prefix-dedup, the realistic worst case
		// is K × ~50ms per eth_call. Set to 0 to disable the cap (not advised).
		// Default: 8.
		FastEvalMaxSubmitsPerSwap int `yaml:"fast_eval_max_submits_per_swap"`

		// Block scoring: how often (in blocks) to sweep all cached cycles for
		// drift-based opportunities that no swap event triggered. Catches
		// cross-DEX price divergence that accumulates between swaps.
		// Set 0 to disable. Default: 4 (~1 second on Arbitrum).
		BlockScoringInterval int `yaml:"block_scoring_interval"`

		// Run executor.Diagnose on every sim-reject and emit a one-line per-hop
		// forensic to bot.log. Useful while investigating why a class of cycles
		// keeps failing on chain — leave OFF in steady state because each
		// invocation costs another eth_call. Default: false.
		DiagnoseSimRejects bool `yaml:"diagnose_sim_rejects"`

		// Auto-disable: when a pool causes ≥ AutoDisableRejectThreshold
		// sim-rejects within AutoDisableWindowSecs (per-hop attribution via
		// parseFailedHop), the bot calls DisablePool() on it. The next cycle
		// cache rebuild (~15s) then drops every cycle touching that pool, so
		// the blast radius is bounded. Covers pool-attributable errors only:
		// "hop N simulation returned 0", "execution reverted: hop N: ...".
		// Not counted: 429s, health gates, cooldowns, "cycle unprofitable"
		// (not per-pool).
		//
		// Defaults: threshold=5 in a 3600s window. The Unishop.ai scam pool
		// produced 8 "hop 0 simulation returned 0" rejects in one hour on
		// 2026-04-11 — comfortably above 5. Legit pools under normal
		// operation produce 0-2 per hour even during volatility.
		//
		// Set AutoDisableEnabled=false to turn the mechanism off entirely.
		AutoDisableEnabled         bool  `yaml:"auto_disable_enabled"`
		AutoDisableRejectThreshold int32 `yaml:"auto_disable_reject_threshold"`
		AutoDisableWindowSecs      int64 `yaml:"auto_disable_window_secs"`

		// Fee assumed for Algebra (CamelotV3/ZyberV3) pools that report fee_bps=0
		// because they use dynamic fees not captured by globalState(). Applied in
		// the simulator so cycle scoring and amountOutMin both use a realistic fee.
		CamelotV3DefaultFeeBps int `yaml:"camelot_v3_default_fee_bps"`

		// LP floor auto-calibrator: adjusts per-DEX base_bps to hit a target revert rate.
		LPCalibrator lpCalibratorConfig `yaml:"lp_calibrator"`
	} `yaml:"strategy"`
}

// Bot orchestrates all components.
type Bot struct {
	cfg          *Config
	node         *ethclient.Client
	tickClient   *ethclient.Client // dedicated client for tick data fetches (HTTPS, dialed once at startup)
	tokens       *TokenRegistry
	registry     *PoolRegistry
	graph        *Graph
	executor     *Executor
	yieldMon     *YieldPegMonitor
	swapListener *SwapListener
	cycleCache   *CycleCache
	db           *DB
	poolStore    *PoolStore
	arbLogger    *ArbLogger

	lastPoolPrice      map[string]float64 // pool addr → last SpotPrice seen; throttle in fastEvalCycles
	blockScorePrice    map[string]float64 // pool addr → SpotPrice at last block-scoring pass (hot-cycle filter)

	lastSubmitByKey    map[string]time.Time // per-cycle cooldown: cycleKey → last submit time
	lastMulticallLog   time.Time    // last time we logged multicall V3 state summary
	minProfitUSD       atomic.Value // float64 — updated by gasMonitorLoop, read by fastEvalCycles
	lastGasPriceGwei   atomic.Value // float64 — updated by gasMonitorLoop, read by fastEvalCycles
	gasCostPerUnit     atomic.Value // float64 — USD per gas unit (gasPriceGwei × 1e-9 × WETH_price), for per-contract minProfit

	// borrowable tracks which tokens the Balancer Vault currently holds and
	// can therefore flash-loan. The cycle cache uses this set to filter cycle
	// start tokens, eliminating the BAL#528 sim-reject failure class. Refreshed
	// hourly by RunBorrowableRefreshLoop. See borrowable.go.
	borrowable    *borrowableTracker
	flashSelector *FlashSourceSelector

	// startup readiness barrier — blocks trade submission until each
	// background loop has run at least one successful pass after restart.
	// Without this, the bot has a window of ~30-90s after every restart where
	// it would happily try to submit cycles based on stale, partial, or
	// not-yet-loaded state and waste RPC budget on doomed eth_calls. See
	// readyOrReason() for the actual gate.
	ready Readiness

	health Health // subsystem health tracker — gates trade submission

	// Candidate reject counters. Every fastEvalCandidate entry is classified
	// into one of these buckets so the periodic [stats] line can show the
	// reject rate per gate — useful for tuning thresholds and spotting when
	// a gate is silently killing all submissions. All counters reset to zero
	// on each stats flush.
	candTotal           atomic.Uint64 // every cycle fed into fastEvalCandidate
	candRejectScore     atomic.Uint64 // ScoreCycle returned non-positive or > lpSanityCap
	candRejectLPFloor   atomic.Uint64 // lp below dynamicLPFloor
	candRejectEmpty     atomic.Uint64 // cycle has zero edges (defensive)
	// candRejectTickStale is the sum of three sub-classes below — left in
	// place so existing dashboards keep reading a single combined counter.
	// The sub-class counters are what gets surfaced to /debug/tick-health
	// and the tick-test suite.
	candRejectTickStale            atomic.Uint64
	candRejectTickNeverFetched     atomic.Uint64 // TicksBlock==0 or TicksWordRadius==0 (never had a pass)
	candRejectTickFetchFailed      atomic.Uint64 // last FetchTickMaps attempt stamped OK=false for this pool
	candRejectTickCoverageDrift    atomic.Uint64 // current tick outside fetched ±radius word window
	candRejectTickEmptyVerified    atomic.Uint64 // OK=true but Ticks==nil (bitmap genuinely had nothing in range)
	candRejectPoolStale atomic.Uint64 // rejected by block-lag gate (Pool.StateBlock lags latest header by > MaxPoolStateBlockLag)
	candRejectUnviable  atomic.Uint64 // optimalAmountIn said not profitable
	candRejectMinBps    atomic.Uint64 // profit bps below MinSimProfitBps
	candRejectNoFlash   atomic.Uint64 // no flash source available for borrow token
	candRejectUnverified atomic.Uint64 // cycle contains a pool with Verified=false
	candAccepted        atomic.Uint64 // cleared all pre-submit checks
}

// Readiness tracks one-shot "first successful pass" flags for every startup
// dependency that the trade submission gate cares about. Each field is
// flipped to true the first time the corresponding background loop completes
// a non-trivial run; once set, it stays set for the lifetime of the process.
//
// Use Bot.readyOrReason() to query the gate; it returns (true, "") when every
// dependency is ready, or (false, "first-unmet condition") otherwise. This is
// the human-readable reason that gets surfaced to arb_observations on a
// startup-time submission rejection.
type Readiness struct {
	MulticallDone   atomic.Bool // BatchUpdatePools has run at least once
	TickMapsDone    atomic.Bool // FetchTickMaps has run at least once (or no V3-cycle pools)
	BorrowableDone  atomic.Bool // RefreshBorrowableTokens has run at least once
	VerifierDone    atomic.Bool // runOnePoolVerificationPass has completed at least once
	CycleRebuildDone atomic.Bool // cycleCache.rebuild has produced > 0 cycles at least once
	ExecutorWarmDone atomic.Bool // Executor.Warmup has completed (nonce + baseFee + vault cache)
}

// readyOrReason reports whether every startup dependency is ready. Returns
// (true, "") on success, or (false, reason) where reason names the first
// unmet condition. Used by trySubmitCandidate as the trade-submission gate.
//
// Note: VerifierDone is intentionally NOT in the gate. The verifier runs at
// 5 pools/sec and a full pass for ~1700 pools takes ~6 minutes — way too long
// to block startup. Pools that haven't been verified yet (verify_reason == "")
// are still allowed by the cycle cache filter, so the bot can safely trade on
// them while the verifier catches up in the background.
func (b *Bot) readyOrReason() (bool, string) {
	if !b.ready.MulticallDone.Load() {
		return false, "startup: multicall has not yet run a full pass"
	}
	if !b.ready.ExecutorWarmDone.Load() {
		return false, "startup: executor warmup (nonce + baseFee) not yet done"
	}
	if !b.ready.BorrowableDone.Load() {
		return false, "startup: borrowable token set not yet refreshed"
	}
	if !b.ready.CycleRebuildDone.Load() {
		return false, "startup: cycle cache has not yet produced any cycles"
	}
	if !b.ready.TickMapsDone.Load() {
		return false, "startup: tick maps have not yet been fetched"
	}
	return true, ""
}

// Health tracks the last-successful timestamp for each subsystem.
// Trade submission is blocked if any critical subsystem is stale.
type Health struct {
	TickDataAt    atomic.Int64 // unix timestamp of last successful FetchTickMaps
	MulticallAt   atomic.Int64 // unix timestamp of last successful BatchUpdatePools
	SwapEventAt   atomic.Int64 // unix timestamp of last swap event received
	CycleRebuildAt atomic.Int64 // unix timestamp of last cycle cache rebuild
	// LatestBlock is the highest block number the bot has observed (header sub
	// on main node, or BlockNumber() polling fallback). Used as the reference
	// for the block-lag freshness gate — a pool's StateBlock must be within
	// MaxPoolStateBlockLag of this value to score.
	LatestBlock atomic.Uint64

	// Tick coverage stats — written after each FetchTickMaps pass.
	// Updated atomically so the dashboard flush can read them without locks.
	TickedPoolsHave  atomic.Int64 // V3/V4 cycle pools that currently have non-empty Ticks
	TickedPoolsTotal atomic.Int64 // total V3/V4 pools we're trying to track ticks for

	// Tick-fetch pass stats — snapshot of the most recent FetchTickMaps pass.
	// Used by /debug/tick-health and the tick-test suite to detect regressions.
	TickFetchPassTotal      atomic.Int64 // pools included in last pass
	TickFetchPassSucceeded  atomic.Int64 // pools stamped OK=true on last pass
	TickFetchPassEmpty      atomic.Int64 // verified-empty pools on last pass
	TickFetchPassFailed     atomic.Int64 // OK=false pools on last pass
	TickFetchPassSkipped    atomic.Int64 // skipped (no TS / no sqrtP / no poolID)
	TickFetchPassRTMismatch atomic.Int64 // algebra round-trip mismatches
	TickFetchPassDurMs      atomic.Int64 // wall-clock ms of last pass
	TickFetchPassBlockArb   atomic.Uint64 // latest block from arbitrum_rpc at pass time
	TickFetchPassBlockTick  atomic.Uint64 // latest block from tick_data_rpc at pass time
	TickFetchEagerDropped   atomic.Uint64 // cumulative count of dropped eager refetch sends
	TickFetchEagerEnqueued  atomic.Uint64 // cumulative count of eager refetch sends that made it into the channel
	// TickRPCBlock is the latest block height observed on the tick_data_rpc
	// endpoint (every call to currentTickBlock stores here). Compared against
	// LatestBlock (main RPC) by the skew gate — if the tick RPC lags by more
	// than cfg.Trading.TickRPCMaxSkewBlocks, trade submission is blocked
	// because the bitmap state would be on a stale branch.
	TickRPCBlock atomic.Uint64
	// JSON-encoded []string of currently-tracked V3/V4 pool addresses, for the
	// dashboard's "click Ticks → filter Pools tab" navigation.
	TrackedAddrsJSON atomic.Value
	// JSON-encoded list of RPC endpoint health probes (one per configured RPC).
	// Updated by rpcHealthLoop every 30s with reachability + latency + last block.
	RPCStateJSON atomic.Value

	// Per-RPC rate-limit counters. Bumped by RecordRPC429() whenever an actual
	// RPC call returns a 429 / "Too Many Requests" error. The probe loop reads
	// these and rolls them into the rpc_state JSON so the dashboard can show
	// throttling between probe rounds.
	RPCRateLimitMu     sync.Mutex
	RPCRateLimit       map[string]int64 // rpc name → cumulative 429 count
	RPCRateLimitLastAt map[string]int64 // rpc name → unix ts of most recent 429
}

// RecordRPC429 bumps the 429 counter for the given RPC endpoint name. Safe to
// call from any goroutine. The dashboard surfaces these counts via rpc_state.
func (h *Health) RecordRPC429(name string) {
	h.RPCRateLimitMu.Lock()
	defer h.RPCRateLimitMu.Unlock()
	if h.RPCRateLimit == nil {
		h.RPCRateLimit = make(map[string]int64)
		h.RPCRateLimitLastAt = make(map[string]int64)
	}
	h.RPCRateLimit[name]++
	h.RPCRateLimitLastAt[name] = time.Now().Unix()
}

// is429 returns true if the error string looks like an HTTP 429 / rate-limit
// response from any of the major RPC providers (Chainstack, Alchemy, Infura).
// We pattern-match on text because go-ethereum unwraps the HTTP status code
// into the error message before returning.
func is429(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") ||
		strings.Contains(s, "too many requests") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rps limit") ||
		strings.Contains(s, "exceeded")
}

// IsReady returns (true, "") if all subsystems are fresh enough to trade,
// or (false, reason) with a human-readable explanation of what's stale.
// maxTickRPCSkewBlocks: if > 0 and both arbitrum_rpc and tick_data_rpc have
// been observed, rejects when the tick RPC lags the main RPC by more than
// this many blocks (state + ticks would be on different chain views).
func (h *Health) IsReady(maxTickAgeSec, maxMulticallAgeSec, maxSwapAgeSec int64, maxTickRPCSkewBlocks int64) (bool, string) {
	now := time.Now().Unix()

	if age := now - h.TickDataAt.Load(); maxTickAgeSec > 0 && age > maxTickAgeSec {
		return false, fmt.Sprintf("tick data stale (%ds ago, max %ds)", age, maxTickAgeSec)
	}
	if age := now - h.MulticallAt.Load(); maxMulticallAgeSec > 0 && age > maxMulticallAgeSec {
		return false, fmt.Sprintf("multicall stale (%ds ago, max %ds)", age, maxMulticallAgeSec)
	}
	if age := now - h.SwapEventAt.Load(); maxSwapAgeSec > 0 && age > maxSwapAgeSec {
		return false, fmt.Sprintf("no swap events (%ds ago, max %ds)", age, maxSwapAgeSec)
	}
	if maxTickRPCSkewBlocks > 0 {
		arbBN := h.LatestBlock.Load()
		tickBN := h.TickRPCBlock.Load()
		// Only the case where the tick RPC LAGS behind is dangerous: bitmap
		// state is on an older branch than slot0 state and the sim would
		// disagree with on-chain execution. When the tick RPC LEADS the main
		// RPC the bitmap is fresher than state, which is harmless (state
		// catches up within a block or two). Separate providers routinely
		// disagree by 5-20 blocks on Arbitrum's 250 ms cadence so a
		// bi-directional gate would block trading permanently.
		if arbBN > 0 && tickBN > 0 && arbBN > tickBN {
			lag := int64(arbBN - tickBN)
			if lag > maxTickRPCSkewBlocks {
				return false, fmt.Sprintf("tick RPC lag: arbitrum=%d tick=%d lag=%d max=%d", arbBN, tickBN, lag, maxTickRPCSkewBlocks)
			}
		}
	}
	return true, ""
}

// applyStrategyDefaults fills zero-value strategy fields with sensible defaults
// and propagates values into package-level vars used by other subsystems.
func applyStrategyDefaults(cfg *Config) {
	s := &cfg.Strategy
	if s.MinPriceDelta <= 0 {
		s.MinPriceDelta = 0.0005
	}
	if s.LPSanityCap <= 0 {
		s.LPSanityCap = 0.05
	}
	if s.MaxProfitUSDCap <= 0 {
		s.MaxProfitUSDCap = 50_000
	}
	if s.TVLTradeFraction <= 0 {
		s.TVLTradeFraction = 0.15
	}
	if s.ShallowPoolTVLUSD <= 0 {
		s.ShallowPoolTVLUSD = 200_000
	}
	if s.ShallowPoolTradeFraction <= 0 {
		s.ShallowPoolTradeFraction = 0.03
	}
	if s.SubmitCooldownSecs <= 0 {
		s.SubmitCooldownSecs = 30
	}
	if s.CycleRebuildSecs <= 0 {
		s.CycleRebuildSecs = 15
	}
	if s.MaxDexPerDest <= 0 {
		s.MaxDexPerDest = 1
	}
	if s.MaxEdgesPerNode <= 0 {
		s.MaxEdgesPerNode = 8
	}
	if s.PoolStalenessSecs <= 0 {
		s.PoolStalenessSecs = 300
	}
	if s.ArbLogRetentionHours <= 0 {
		s.ArbLogRetentionHours = 72
	}
	if s.ArbLogMaxRows <= 0 {
		s.ArbLogMaxRows = 500_000
	}
	if s.ContractMinProfitDivisor <= 0 {
		s.ContractMinProfitDivisor = 1
	}
	if s.SlippageBps <= 0 {
		s.SlippageBps = 50
	}
	if s.FastEvalMaxSubmitsPerSwap <= 0 {
		s.FastEvalMaxSubmitsPerSwap = 8
	}
	if s.OptimalAmountTernaryIterations <= 0 {
		s.OptimalAmountTernaryIterations = 25
	}
	if s.OptimalAmountRefinement < 0 {
		s.OptimalAmountRefinement = 0
	}
	if s.BlockScoringInterval <= 0 {
		s.BlockScoringInterval = 4
	}
	if s.AutoDisableRejectThreshold <= 0 {
		s.AutoDisableRejectThreshold = 5
	}
	if s.AutoDisableWindowSecs <= 0 {
		s.AutoDisableWindowSecs = 3600
	}
	if s.CompetitorWatchMinProfitUSD <= 0 {
		// $0.05 floor — anything that cleared gas reveals a pool worth knowing
		// about. The earlier $20 default missed 96% of competitor arbs because
		// most arbs are sub-$1. Pool discovery is the goal, not profit chasing.
		s.CompetitorWatchMinProfitUSD = 0.05
	}
	if s.LastHopShortfallBps < 0 {
		s.LastHopShortfallBps = 0
	}
	if s.LastHopShortfallBps == 0 {
		s.LastHopShortfallBps = 1 // 0.01% default
	}
	SetLastHopShortfallBps(s.LastHopShortfallBps)
	if s.GasSafetyMult <= 0 {
		s.GasSafetyMult = 3.0
	}
	// MinSimProfitBps: no default — with multi-tick V3 simulation the go_sim
	// is accurate enough that we can trust thin margins. Set > 0 in config
	// only if reverting trades return (e.g. min_sim_profit_bps: 75).

	// Health gate defaults
	t := &cfg.Trading
	if t.MaxTickAgeSec <= 0 {
		t.MaxTickAgeSec = 30
	}
	if t.MaxMulticallAgeSec <= 0 {
		t.MaxMulticallAgeSec = 15
	}
	if t.MaxSwapAgeSec <= 0 {
		t.MaxSwapAgeSec = 10
	}
	// MaxPoolStateBlockLag and TickBitmapCoverageWords MUST be set explicitly.
	// 0 is fatal at startup — see ValidateFreshnessConfig. No default is
	// applied here precisely so that a config missing these keys surfaces
	// immediately rather than silently disabling the sim-freshness
	// invariant (see feedback_verified_means_fresh.md for rationale).
	if t.TickEagerRefetchSpacings <= 0 {
		t.TickEagerRefetchSpacings = 8
	}
	if t.TickEagerRefetchCooldownMs <= 0 {
		t.TickEagerRefetchCooldownMs = 750
	}

	// Propagate to package-level vars used in graph and cyclecache subsystems.
	poolStaleness = time.Duration(s.PoolStalenessSecs) * time.Second
	rebuildInterval = time.Duration(s.CycleRebuildSecs) * time.Second
	if s.CamelotV3DefaultFeeBps > 0 {
		camelotV3DefaultFeeBps = uint32(s.CamelotV3DefaultFeeBps)
	}
	optimalAmountTernaryIters = s.OptimalAmountTernaryIterations
	optimalAmountLogScale = s.OptimalAmountLogScale
	optimalAmountRefinement = s.OptimalAmountRefinement
}

// optimalAmountIn ternary-search knobs, driven by cfg.Strategy (applied in
// applyStrategyDefaults). Exposed as package vars so the free function
// optimalAmountIn can read them without threading config through every call.
var (
	optimalAmountTernaryIters int  = 25
	optimalAmountLogScale     bool = true
	optimalAmountRefinement   int  = 0
)

func mergeMajorIntoConfig(cfg *Config) {
	seenToken := make(map[string]bool)
	for _, t := range cfg.Tokens {
		seenToken[strings.ToLower(t.Address)] = true
	}
	for _, t := range cfg.Major.Tokens {
		a := strings.ToLower(t.Address)
		if seenToken[a] {
			continue
		}
		seenToken[a] = true
		cfg.Tokens = append(cfg.Tokens, struct {
			Address  string `yaml:"address"`
			Symbol   string `yaml:"symbol"`
			Decimals uint8  `yaml:"decimals"`
		}{Address: t.Address, Symbol: t.Symbol, Decimals: t.Decimals})
	}

	seenPool := make(map[string]bool)
	for _, p := range cfg.Pools.PinnedPools {
		seenPool[strings.ToLower(p)] = true
	}
	for _, p := range cfg.Major.Pools {
		a := strings.ToLower(p)
		if seenPool[a] {
			continue
		}
		seenPool[a] = true
		cfg.Pools.PinnedPools = append(cfg.Pools.PinnedPools, p)
	}

	if cfg.TestMode {
		majorTokens := make(map[string]bool, len(cfg.Major.Tokens))
		for _, t := range cfg.Major.Tokens {
			majorTokens[strings.ToLower(t.Address)] = true
		}
		var filteredTokens []struct {
			Address  string `yaml:"address"`
			Symbol   string `yaml:"symbol"`
			Decimals uint8  `yaml:"decimals"`
		}
		for _, t := range cfg.Tokens {
			if majorTokens[strings.ToLower(t.Address)] {
				filteredTokens = append(filteredTokens, t)
			}
		}
		cfg.Tokens = filteredTokens

		majorPools := make(map[string]bool, len(cfg.Major.Pools))
		for _, p := range cfg.Major.Pools {
			majorPools[strings.ToLower(p)] = true
		}
		var filteredPools []string
		for _, p := range cfg.Pools.PinnedPools {
			if majorPools[strings.ToLower(p)] {
				filteredPools = append(filteredPools, p)
			}
		}
		cfg.Pools.PinnedPools = filteredPools

		log.Printf("[test_mode] ON: registry restricted to %d major tokens and %d major pools",
			len(cfg.Tokens), len(cfg.Pools.PinnedPools))
	}
}

// classifyValidateRevert produces a structured reject_reason string for a
// failed eth_call at 99% slippage. It distinguishes:
//
//   - LATENCY_DRIFT: cycle was profitable at detection but flipped unprofitable
//     by the time eth_call ran. Detected by re-simulating against a fresh
//     snapshot of the cycle's pools and checking whether profit now <= 0.
//     Last-hop router/slippage reverts with freshSim<=0 fall into this bucket.
//
//   - SIM_PHANTOM: cycle is still "profitable" in our fresh sim but eth_call
//     rejects. Indicates our sim is wrong (bitmap drift, wrong fee, fee-on-
//     transfer token, wrong decimals). Includes the failing hop info.
//
//   - V4_HANDLER: V4 hop reverted inside unlockCallback. Usually a contract
//     bug (BalanceDelta decode, sync ordering, etc.), not a sim bug.
//
//   - FLASH_BORROW: flash loan couldn't be obtained (BAL#528, Aave revert).
//
//   - UNKNOWN: fell through all pattern matches.
//
// The classification is appended to the "paused: trading (eth_call REVERTED
// even at 99%slippage: ...)" prefix so the dashboard + comparison_compare.go
// can categorize without parsing the raw revert text.
func (b *Bot) classifyValidateRevert(r *fastEvalCandidate, err error) string {
	errStr := err.Error()
	prefix := fmt.Sprintf("paused: trading (eth_call REVERTED even at 99%%slippage: %v", errStr)

	// Per-pool freshness snapshot — captured BEFORE any other classifier
	// work so the view reflects the state at the moment eth_call returned.
	// Format per pool: "dex@state_block_lag=Nb/age=Xs,ticks_block_lag=Mb/age=Ys".
	// A pool with tick_block_lag in the hundreds on a V3 pool is expected
	// (5s periodic sweep cadence); a state_block_lag > 1 on any pool means
	// the per-block multicall fell behind and the freshness gate let through
	// a stale edge.
	latest := b.health.LatestBlock.Load()
	now := time.Now()
	freshParts := make([]string, 0, len(r.cycle.Edges))
	for i, edge := range r.cycle.Edges {
		p := edge.Pool
		p.mu.RLock()
		sb := p.StateBlock
		tb := p.TicksBlock
		lu := p.LastUpdated
		tu := p.TicksUpdatedAt
		dex := p.DEX
		p.mu.RUnlock()
		var sbLag int64
		if latest > 0 {
			sbLag = int64(latest) - int64(sb)
		}
		// tb_lag only meaningful for V3-family pools (V2/Curve/Balancer
		// don't have tick bitmaps; tb=0 there is by design, not staleness).
		// Previously this printed `tb_lag=<latest-0>` for non-V3 pools —
		// misleading noise in classifier output, producing false "stale
		// bitmap" signals during analysis. Now we omit tb_lag for non-V3.
		hasBitmap := false
		switch dex {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3,
			DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
			hasBitmap = true
		}
		luAge := int64(-1)
		if !lu.IsZero() {
			luAge = int64(now.Sub(lu).Seconds())
		}
		if hasBitmap {
			var tbLag int64
			if latest > 0 {
				tbLag = int64(latest) - int64(tb)
			}
			tuAge := int64(-1)
			if !tu.IsZero() {
				tuAge = int64(now.Sub(tu).Seconds())
			}
			freshParts = append(freshParts, fmt.Sprintf("h%d:%s[sb_lag=%db/%ds,tb_lag=%db/%ds]",
				i, dex, sbLag, luAge, tbLag, tuAge))
		} else {
			freshParts = append(freshParts, fmt.Sprintf("h%d:%s[sb_lag=%db/%ds]",
				i, dex, sbLag, luAge))
		}
	}
	freshStr := strings.Join(freshParts, " ")

	// Flash-borrow failures — happen before any hop runs.
	if strings.Contains(errStr, "BAL#528") {
		return prefix + ") [FLASH_BORROW: vault unavailable] " + freshStr
	}

	// Locate the failing hop in the "hop N:" pattern. If not present (e.g.
	// bare "execution reverted"), it's likely a V4 unlockCallback revert.
	hopRe := regexp.MustCompile(`hop (\d+): (swap reverted|slippage)`)
	m := hopRe.FindStringSubmatch(errStr)
	hopIdx := -1
	kind := ""
	if m != nil {
		hopIdx, _ = strconv.Atoi(m[1])
		kind = m[2]
	}

	// V4 bare revert (no "hop N:" prefix) almost always means the V4 unlock
	// callback reverted with a custom error selector that doesn't decode to
	// a string. Treat as contract-handler bug class.
	dl := make([]string, len(r.cycle.Edges))
	for i, e := range r.cycle.Edges {
		dl[i] = e.Pool.DEX.String()
	}
	if hopIdx < 0 {
		hasV4 := false
		for _, d := range dl {
			if d == string(DEXUniswapV4) {
				hasV4 = true
				break
			}
		}
		if hasV4 {
			return prefix + ") [V4_HANDLER: unlockCallback reverted] " + freshStr
		}
		return prefix + ") [UNKNOWN: no hop tag, no V4] " + freshStr
	}
	failingDex := "?"
	if hopIdx < len(dl) {
		failingDex = dl[hopIdx]
	}
	isLastHop := hopIdx == len(dl)-1

	// Re-simulate against current live pool state. This costs one Snapshot
	// clone per hop (cheap — same as what the scoring pipeline already did)
	// and one SimulateCycle call. We are NOT refreshing from chain; we only
	// compare against whatever pools the event/multicall stream has delivered
	// since the detection moment. If the latest delivered state is enough to
	// flip sim profit <= 0, we conclude the cycle drifted out in the window
	// between detection and eth_call.
	freshCycle := r.cc.Cycle.WithSnapshots()
	freshResult := SimulateCycle(freshCycle, r.amountIn)
	freshProfit := big.NewInt(0)
	if freshResult.Profit != nil {
		freshProfit.Set(freshResult.Profit)
	}
	if freshProfit.Sign() <= 0 {
		return fmt.Sprintf("%s) [LATENCY_DRIFT: hop=%d dex=%s last=%v sim_bps_was=%.2f fresh_bps=%.2f] %s",
			prefix, hopIdx, failingDex, isLastHop, r.result.ProfitBps, freshResult.ProfitBps, freshStr)
	}

	// Cycle is STILL profitable in our Go sim against the latest state, but
	// the on-chain eth_call rejects. Sim is wrong about something.

	// Accumulate per-pool scorecard for overnight analysis. Overshoot in
	// bps is the sim's predicted profit for the cycle — since fresh_bps
	// equals sim_bps in the SIM_PHANTOM class, that quantity equals how
	// much the sim overestimated the last-hop output. The per-pool rank
	// by count+avg overshoot is the fastest way to tell which specific
	// pool's sim math is out-of-whack.
	var failingPoolAddr, failingTokenIn, failingTokenOut string
	if hopIdx >= 0 && hopIdx < len(r.cycle.Edges) {
		failingPoolAddr = r.cycle.Edges[hopIdx].Pool.Address
		failingTokenIn = r.cycle.Edges[hopIdx].TokenIn.Symbol
		failingTokenOut = r.cycle.Edges[hopIdx].TokenOut.Symbol
	}
	if b.db != nil && failingPoolAddr != "" {
		b.db.UpsertSimPhantomStat(failingPoolAddr, failingDex,
			failingTokenIn, failingTokenOut, int(r.result.ProfitBps))
	}

	if failingDex == DEXUniswapV4.String() {
		return fmt.Sprintf("%s) [V4_HANDLER: hop=%d kind=%s (sim still +%.2fbps)] %s",
			prefix, hopIdx, kind, freshResult.ProfitBps, freshStr)
	}
	return fmt.Sprintf("%s) [SIM_PHANTOM: hop=%d dex=%s kind=%s last=%v sim_bps=%.2f fresh_bps=%.2f — sim accuracy bug] %s",
		prefix, hopIdx, failingDex, kind, isLastHop, r.result.ProfitBps, freshResult.ProfitBps, freshStr)
}

// validateFreshnessConfig enforces the sim-freshness invariant described in
// feedback_verified_means_fresh.md: every pool/token entering the scoring
// pipeline must have sim-dependent state fresh at scoring time. The two gates
// (block-lag for state, bitmap-coverage for ticks) must be explicitly
// configured — a missing or zero value is fatal so the gate can never silently
// turn off via a config edit or a forgotten yaml key. Callers run this AFTER
// applyStrategyDefaults so the check sees the final values.
func validateFreshnessConfig(cfg *Config) {
	t := &cfg.Trading
	if t.MaxPoolStateBlockLag == 0 {
		log.Fatalf("config: trading.max_pool_state_block_lag is required and must be > 0 (0 disables the pool-state freshness gate and violates the verified-and-fresh invariant — see feedback_verified_means_fresh.md)")
	}
	if t.TickBitmapCoverageWords <= 0 {
		log.Fatalf("config: trading.tick_bitmap_coverage_words is required and must be > 0 (0 disables the tick-bitmap coverage gate and violates the verified-and-fresh invariant — see feedback_verified_means_fresh.md)")
	}
	if t.TickRefetchChanCap <= 0 {
		log.Fatalf("config: trading.tick_refetch_chan_cap is required and must be > 0 (0 hard-codes the old default which silently drops eager refetches on bursty markets)")
	}
	if t.TickBitmapMaxAutoRadius > 0 && t.TickBitmapMaxAutoRadius < t.TickBitmapCoverageWords {
		log.Fatalf("config: trading.tick_bitmap_max_auto_radius (%d) must be >= tick_bitmap_coverage_words (%d) or 0 to disable auto-expand",
			t.TickBitmapMaxAutoRadius, t.TickBitmapCoverageWords)
	}
}

func NewBot(cfg *Config, privKey string) (*Bot, error) {
	applyStrategyDefaults(cfg)
	validateFreshnessConfig(cfg)
	mergeMajorIntoConfig(cfg)

	// Propagate the subgraph sanity cap (config override → package var).
	if cfg.Pools.SanityMaxTVLUSD > 0 {
		SanityMaxTVLUSD = cfg.Pools.SanityMaxTVLUSD
	}

	client, err := ethclient.Dial(cfg.ArbitrumRPC)
	if err != nil {
		return nil, fmt.Errorf("dial node: %w", err)
	}
	// ArbitrumRPC is WSS — HTTP/2 doesn't apply (WebSockets tunnel over
	// HTTP/1.1 by design). Simulation + tick-data RPCs are HTTPS and DO
	// benefit from HTTP/2 connection multiplexing when we fan out parallel
	// batch multicalls. Chainstack supports ALPN h2 on HTTPS.

	// Open DB early so we can load junk addresses before building the token registry.
	var db *DB
	var poolStore *PoolStore
	var arbLogger *ArbLogger
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = "arb.db"
	}
	junkAddrs := make(map[string]bool)
	if d, err := OpenDB(dbPath); err != nil {
		log.Printf("[bot] WARNING: could not open pool database: %v", err)
	} else {
		db = d
		if addrs, err := d.JunkTokenAddresses(); err != nil {
			log.Printf("[bot] WARNING: could not load junk tokens: %v", err)
		} else if len(addrs) > 0 {
			for _, a := range addrs {
				junkAddrs[strings.ToLower(a)] = true
			}
			log.Printf("[bot] loaded %d junk token(s) — will be excluded from cycles", len(addrs))
		}
	}

	tokens := NewTokenRegistry()
	// Seed native ETH as a first-class token. V4 PoolKeys use address(0) for
	// ETH; the DB stores these pools canonically with token0=0x0000..., so the
	// loader resolves them cleanly instead of needing an alias trick that
	// misrepresents Pool.Token0.Address as WETH.
	tokens.Add(NewToken(NativeETHAddress, "ETH", 18))
	for _, t := range cfg.Tokens {
		if junkAddrs[strings.ToLower(t.Address)] {
			log.Printf("[bot] skipping junk token %s (%s)", t.Symbol, t.Address)
			continue
		}
		tokens.Add(NewToken(t.Address, t.Symbol, t.Decimals))
	}
	cfgTokenCount := len(tokens.All())

	// Rehydrate the registry with non-junk tokens that were discovered live in
	// previous sessions and persisted to the DB. Without this step, the registry
	// would shrink back to the cfg.Tokens seed on every restart, and LoadPools
	// would silently drop every persisted pool whose token0/token1 was discovered
	// live (often hundreds of pools — see project_test_plan.md). Config-seeded
	// tokens are added BEFORE this loop, so they take precedence on conflict
	// (the registry's Add is a plain map insert; we skip duplicates here to
	// preserve config metadata over potentially-stale DB rows).
	if db != nil {
		dbTokens, err := db.LoadTokens()
		if err != nil {
			log.Printf("[bot] WARNING: could not rehydrate tokens from DB: %v", err)
		} else {
			added := 0
			for _, t := range dbTokens {
				if junkAddrs[strings.ToLower(t.Address)] {
					continue
				}
				if _, exists := tokens.Get(t.Address); exists {
					continue
				}
				tokens.Add(t)
				added++
			}
			log.Printf("[bot] token registry: %d from config + %d rehydrated from DB = %d total",
				cfgTokenCount, added, len(tokens.All()))
		}
	}

	registry := NewPoolRegistry(
		MinTVLFilter(cfg.Pools.MinTVLUSD),
		MinVolumeFilter(cfg.Pools.MinVolume24h),
		MaxTVLFilter(cfg.Pools.MaxTVLUSD),
		// Require at least one token to be in our registry — rejects unknown/honeypot tokens
		// discovered via PoolCreated events. Subgraph/config seeds use ForceAdd and bypass this.
		WhitelistFilter(tokens),
	)
	// Hard TVL floor — applies to BOTH Add (discovery) and ForceAdd (subgraph
	// seeds, pinned pools, competitor watcher). The regular MinTVLFilter only
	// runs in Add. Without this floor sub-$5k scam pools from the subgraph
	// repeatedly cause "hop simulation returned 0" sim-rejects (Unishop.ai
	// $580 TVL pool was the trigger that motivated this). 0 = disabled.
	// Absolute TVL floor + pool-quality filter knobs are applied via the
	// poolQualityReason helper in graph.go, which is called from
	// registry.ForceAdd/Add, cyclecache.rebuild, and bot.prune. Set the
	// globals here so all three paths see the same values.
	absoluteMinTVLUSD = cfg.Pools.AbsoluteMinTVLUSD
	// Pool-quality composite filter knobs — defaults applied when zero.
	// Set the globals before NewBot returns so every downstream path
	// (registry.Add, cyclecache.rebuild, bot.prune) sees consistent values.
	if cfg.Pools.MinTickCount == 0 {
		cfg.Pools.MinTickCount = 30
	}
	if cfg.Pools.HighFeeTierMinTVLUSD == 0 {
		cfg.Pools.HighFeeTierMinTVLUSD = 50_000
	}
	if cfg.Pools.MinVolumeTVLRatio == 0 {
		cfg.Pools.MinVolumeTVLRatio = 0.01
	}
	minTickCount = cfg.Pools.MinTickCount
	highFeeTierMinTVLUSD = cfg.Pools.HighFeeTierMinTVLUSD
	minVolumeTVLRatio = cfg.Pools.MinVolumeTVLRatio
	if cfg.Pools.MinVolumeTVLRatioExemptTVLUSD == 0 {
		cfg.Pools.MinVolumeTVLRatioExemptTVLUSD = 50_000_000 // $50M default
	}
	minVolumeTVLRatioExemptTVLUSD = cfg.Pools.MinVolumeTVLRatioExemptTVLUSD
	if cfg.Pools.MinTickCountBypassTVLUSD == 0 {
		cfg.Pools.MinTickCountBypassTVLUSD = 1_000_000 // $1M default, matches pre-2026-04-19 hardcode
	}
	minTickCountBypassTVLUSD = cfg.Pools.MinTickCountBypassTVLUSD
	if cfg.Pools.AbsoluteMinTVLUSD > 0 {
		log.Printf("[bot] absolute TVL floor: $%.0f (applied in Add + ForceAdd + cyclecache + prune)", cfg.Pools.AbsoluteMinTVLUSD)
	}
	log.Printf("[bot] pool-quality filter: min_ticks=%d (bypass_tvl=$%.0f) high_fee_min_tvl=$%.0f min_vol_tvl_ratio=%.4f",
		minTickCount, minTickCountBypassTVLUSD, highFeeTierMinTVLUSD, minVolumeTVLRatio)
	if cfg.PauseTracking {
		log.Printf("[bot] PAUSE_TRACKING=true — cycle scoring disabled; pool state still updates from swap events")
	}
	if cfg.PauseTrading {
		log.Printf("[bot] PAUSE_TRADING=true — submission disabled; candidates logged to arb_observations with reason 'paused: trading'")
	}

	graph := NewGraph()

	swapListener := NewSwapListener(client, registry, graph)
	// Eager tick re-fetch channel: the swap listener writes a pool here when
	// a Swap event indicates the tick has crossed enough boundaries that the
	// cached bitmap may not cover the new active range. watchTickMapsLoop
	// drains this channel and does single-pool re-fetches in between its
	// regular 5s sweeps. Sized from cfg.Trading.TickRefetchChanCap (mandatory
	// config param; validateFreshnessConfig fatals on 0). Drops are observable
	// via b.health.TickFetchEagerDropped and the [candstats] log.
	swapListener.TickRefetchCh = make(chan *Pool, cfg.Trading.TickRefetchChanCap)
	swapListener.TickEagerRefetchSpacings = int32(cfg.Trading.TickEagerRefetchSpacings)
	swapListener.TickEagerRefetchCooldown = time.Duration(cfg.Trading.TickEagerRefetchCooldownMs) * time.Millisecond
	// Algebra (CamelotV3/ZyberV3) dynamic-fee refresh channel: the swap
	// listener writes a pool here on every Algebra Swap event. A background
	// worker drains the channel and calls globalState() to refresh the
	// post-swap dynamic fee that the event payload doesn't carry.
	swapListener.AlgebraFeeRefreshCh = make(chan *Pool, cfg.Trading.TickRefetchChanCap)
	// Reuse the tick eager-refetch cooldown for the Algebra fee path — the
	// motivation (avoid hammering the RPC on hot pools) is identical and we
	// don't want to introduce yet another knob until empirically warranted.
	swapListener.AlgebraFeeRefreshCooldown = time.Duration(cfg.Trading.TickEagerRefetchCooldownMs) * time.Millisecond

	var yieldMon *YieldPegMonitor
	if len(cfg.YieldMonitoring.Pairs) > 0 {
		yieldMon = NewYieldPegMonitor(client, registry, tokens,
			cfg.YieldMonitoring.Pairs,
			cfg.YieldMonitoring.MinDivergenceBps,
			cfg.YieldMonitoring.MinProfitUSD,
		)
	}

	// Wire the PancakeV3-handler feature flag into dexTypeOnChain BEFORE the
	// executor (and any cycle simulations) start using it. See the comment on
	// executorSupportsPancakeV3 in executor.go for why this is gated.
	executorSupportsPancakeV3.Store(cfg.Trading.ExecutorSupportsPancakeV3)
	if cfg.Trading.ExecutorSupportsPancakeV3 {
		log.Println("[bot] PancakeV3 dispatch: DEX_PANCAKE_V3 (8) — assumes contract has _swapPancakeV3 handler")
	} else {
		log.Println("[bot] PancakeV3 dispatch: DEX_V3 (1, broken) — set trading.executor_supports_pancake_v3=true after redeploying the contract")
	}

	var exec *Executor
	if privKey != "" && cfg.Trading.ExecutorContract != "" {
		chainID, err := client.ChainID(context.Background())
		if err != nil {
			return nil, fmt.Errorf("chain id: %w", err)
		}
		exec, err = NewExecutor(client, privKey, cfg.Trading.ExecutorContract, chainID, cfg.Trading.BalancerVault, cfg.Trading.SequencerRPC, cfg.Trading.SimulationRPC)
		if err != nil {
			return nil, fmt.Errorf("executor: %w", err)
		}
		// Wire the V3FlashMini contract if configured. Empty string = the
		// mini path stays disabled and all cycles route to the full executor.
		if cfg.Trading.ExecutorV3Mini != "" {
			exec.SetV3MiniAddress(cfg.Trading.ExecutorV3Mini)
			log.Printf("[executor] V3FlashMini enabled at %s", cfg.Trading.ExecutorV3Mini)
		}
		// V4Mini: single-unlock multi-hop V4-only executor. Empty = disabled,
		// V4 cycles fall through to the generic ArbitrageExecutor and continue
		// reverting on native-ETH legs / active-hook pools (V4_HANDLER class).
		if cfg.Trading.ExecutorV4Mini != "" {
			exec.SetV4MiniAddress(cfg.Trading.ExecutorV4Mini)
			log.Printf("[executor] V4Mini enabled at %s", cfg.Trading.ExecutorV4Mini)
		}
		// MixedV3V4Executor: 2-5 hop cycles with at least one V4 hop AND at
		// least one V3 hop. Wraps everything in one PoolManager.unlock.
		// Empty = disabled, mixed cycles fall to the generic executor.
		if cfg.Trading.ExecutorMixedV3V4 != "" {
			exec.SetMixedV3V4Address(cfg.Trading.ExecutorMixedV3V4)
			log.Printf("[executor] MixedV3V4Executor enabled at %s", cfg.Trading.ExecutorMixedV3V4)
		}
	}

	maxHops := cfg.Pools.MaxHops
	if maxHops <= 0 {
		maxHops = 4
	}

	// DB was opened early above (before token registry) to load junk addresses.
	// Now wire up the pool store and arb logger which need registry+tokens.
	if db != nil {
		poolStore = NewPoolStore(db, registry, tokens)
		arbLogger = NewArbLogger(db, cfg.Trading.ExecutorContract, ArbLoggerConfig{
			RetentionHours: cfg.Strategy.ArbLogRetentionHours,
			MaxRows:        cfg.Strategy.ArbLogMaxRows,
		})
	}

	// Dedicated HTTPS client for tick data fetches. Dialed once at startup
	// (vs every iteration) to avoid TCP/TLS handshake overhead and the
	// "context canceled" errors that come from leaked idle connections.
	// Uses DialHTTP2 so parallel batch multicalls multiplex over a single
	// TLS connection (Chainstack negotiates h2 via ALPN).
	tickClient := client
	if rpc := cfg.Trading.TickDataRPC; rpc != "" {
		if tc, err := DialHTTP2(rpc); err == nil {
			tickClient = tc
			log.Printf("[bot] tick_data_rpc dialed with HTTP/2")
		} else {
			log.Printf("[bot] tick_data_rpc dial failed (%v); falling back to main node", err)
		}
	} else if rpc := cfg.Trading.SimulationRPC; rpc != "" {
		if tc, err := DialHTTP2(rpc); err == nil {
			tickClient = tc
		}
	}

	bot := &Bot{
		cfg:           cfg,
		node:          client,
		tickClient:    tickClient,
		tokens:        tokens,
		registry:      registry,
		graph:         graph,
		executor:      exec,
		yieldMon:      yieldMon,
		swapListener:  swapListener,
		cycleCache:    NewCycleCache(graph, tokens, maxHops, cfg.Strategy.MaxDexPerDest, cfg.Strategy.MaxEdgesPerNode),
		db:            db,
		poolStore:     poolStore,
		arbLogger:     arbLogger,
		lastPoolPrice:   make(map[string]float64),
		blockScorePrice: make(map[string]float64),
		lastSubmitByKey: make(map[string]time.Time),
		borrowable:      &borrowableTracker{},
		flashSelector:   NewFlashSourceSelector(),
	}

	if cfg.TestMode && len(cfg.Major.Tokens) > 0 {
		addrs := make([]string, len(cfg.Major.Tokens))
		for i, t := range cfg.Major.Tokens {
			addrs[i] = t.Address
		}
		bot.cycleCache.SetWhitelistOverride(addrs)
		log.Printf("[test_mode] cycle cache whitelist = %d major tokens", len(addrs))
	}

	// Wire borrowable token tracker into the cycle cache so the next rebuild
	// can filter start tokens by Balancer flash-loan eligibility.
	bot.cycleCache.SetBorrowable(bot.borrowable)
	bot.cycleCache.SetFlashSelector(bot.flashSelector)

	// Wire eager-refetch observability counters on the swap listener so every
	// send (whether enqueued or dropped on full channel) is countable. The
	// periodic [candstats] logger reads + resets these; /debug/tick-health
	// exposes them live. Without this wiring we had no way to know whether a
	// bursty market was silently dropping refetch signals.
	swapListener.TickDropCounter = &bot.health.TickFetchEagerDropped
	swapListener.TickEnqueueCounter = &bot.health.TickFetchEagerEnqueued

	// Wire up health tracking callbacks.
	swapListener.OnSwap = func() {
		bot.health.SwapEventAt.Store(time.Now().Unix())
	}
	bot.cycleCache.OnRebuild = func() {
		bot.health.CycleRebuildAt.Store(time.Now().Unix())
		// Mark CycleRebuildDone the first time we get a non-empty rebuild.
		// (OnRebuild only fires from the rebuild() function when len(all) > 0,
		// so if we got here, the cache has at least one cycle.)
		if !bot.ready.CycleRebuildDone.Load() {
			bot.ready.CycleRebuildDone.Store(true)
			log.Println("[ready] cycle cache — first non-empty rebuild complete")
		}
	}

	return bot, nil
}

func (b *Bot) Run(ctx context.Context) {
	log.Println("[bot] starting...")

	// Seed min profit from config; gasMonitorLoop will override it dynamically.
	b.minProfitUSD.Store(b.cfg.Trading.MinProfitUSD)
	b.lastGasPriceGwei.Store(0.0)
	b.gasCostPerUnit.Store(0.0)

	// Read-only introspection HTTP server (loopback only). Started before
	// bootstrap so it can answer health probes during the (slow) initial
	// multicall pass — useful for distinguishing "bot is starting" from
	// "bot is wedged" during the first ~30s.
	startDebugHTTP(ctx, b, b.cfg.Trading.DebugHTTPPort)

	if b.cfg.Trading.HookRegistry != "" && b.cfg.Trading.HookSyncIntervalSec > 0 {
		registryAddr := common.HexToAddress(b.cfg.Trading.HookRegistry)
		interval := time.Duration(b.cfg.Trading.HookSyncIntervalSec) * time.Second
		hs := NewHookSync(b.db, b.node, registryAddr, b.executor.privateKey, b.executor.chainID, interval)
		SetGlobalHookGate(hs)
		go hs.Run(ctx, func() []common.Address {
			all := b.registry.All()
			out := make([]common.Address, 0, 64)
			for _, p := range all {
				if p.DEX != DEXUniswapV4 {
					continue
				}
				h := strings.TrimSpace(p.V4Hooks)
				if h == "" || h == "0x0000000000000000000000000000000000000000" {
					continue
				}
				out = append(out, common.HexToAddress(h))
			}
			return out
		})
		log.Printf("[hooksync] enabled: registry=%s interval=%s", registryAddr.Hex(), interval)
	}

	b.bootstrapPools()

	// Wire the fast-path evaluator into the swap listener. It runs inline in
	// handleSwap, re-scoring all cached cycles that touch the updated pool.
	b.swapListener.FastEval = b.fastEvalCycles
	// Advance LatestBlock opportunistically from swap events — see comment
	// on SwapListener.OnBlockSeen for rationale (WSS header drops).
	b.swapListener.OnBlockSeen = func(bn uint64) {
		if bn > b.health.LatestBlock.Load() {
			b.health.LatestBlock.Store(bn)
		}
	}
	// Cross-pool propagation: when pool P swaps, also score cycles through
	// sibling pools (same token pair, different DEX). Uses the unthrottled
	// variant because the peer's SpotPrice hasn't changed — the standard
	// MinPriceDelta throttle would suppress it.
	b.swapListener.FastEvalPeer = b.fastEvalCyclesUnthrottled
	// Targeted refresh: before scoring peers, fetch their current on-chain
	// state via a single Multicall3 batch. Eliminates the 5s staleness that
	// causes phantom cross-DEX spreads (one pool fresh from swap event, the
	// other 5s stale from the last bulk multicall → sim sees fake opportunity).
	b.swapListener.RefreshPools = func(pools []*Pool) {
		// Fast path: if every peer's StateBlock already matches latest (the
		// watchNewBlocks multicall already refreshed them this block), the
		// multicall+V4-fetch round-trip is pure waste — it would just
		// re-read the same values. Skip it to save 80-250 ms of critical-
		// path latency on roughly 70% of swap-event evaluations. If any
		// peer is even 1 block behind, fall through to the authoritative
		// refresh below.
		if latest := b.health.LatestBlock.Load(); latest > 0 {
			allCurrent := true
			for _, p := range pools {
				p.mu.RLock()
				sb := p.StateBlock
				p.mu.RUnlock()
				if sb != latest {
					allCurrent = false
					break
				}
			}
			if allCurrent {
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		bn := b.currentStateBlock(ctx)
		if err := BatchUpdatePoolsAtBlock(ctx, b.node, pools, bn); err != nil {
			// Non-fatal: if refresh fails, we still score with stale data
			// (same behavior as before this optimization). The eth_call
			// validation will catch phantom opportunities downstream.
			return
		}
		// V4 pools need a separate state fetch through the PoolManager.
		if err := FetchV4PoolStates(ctx, b.node, pools, bn); err != nil {
			return
		}
		// Update graph edge weights after the refresh so ScoreCycle sees
		// the fresh spotRate.
		for _, p := range pools {
			b.graph.UpdateEdgeWeights(p)
		}
	}

	go b.watchNewBlocks(ctx)
	go b.blockScoringLoop(ctx)
	go b.watchTickMapsLoop(ctx)
	go b.algebraFeeRefreshLoop(ctx)
	go b.watchPoolCreation(ctx)
	go b.pruneLoop(ctx)
	go b.subgraphRefreshLoop(ctx)
	go b.swapListener.Run(ctx)
	go b.watchStats(ctx)
	go b.gasMonitorLoop(ctx)
	go b.rpcHealthLoop(ctx)
	go b.competitorCycleMatchLoop(ctx)
	go b.cycleCache.Run(ctx)

	// Refresh the borrowable token set every hour. This eliminates the BAL#528
	// sim-reject failure class by ensuring the cycle cache only generates
	// cycles whose start token is something Balancer can actually flash-loan.
	// See borrowable.go and project_test_plan.md.
	go RunBorrowableRefreshLoop(
		ctx,
		b.node,
		common.HexToAddress(b.cfg.Trading.BalancerVault),
		b.tokens,
		b.borrowable,
		nil, // minBalanceWei = 0 → any non-zero balance qualifies
		1*time.Hour,
		func(set map[string]bool) {
			if !b.ready.BorrowableDone.Load() {
				b.ready.BorrowableDone.Store(true)
				log.Println("[ready] borrowable token set — first refresh complete")
			}
			// Update flash source selector with the Balancer borrowable set.
			b.flashSelector.SetBalancerBorrowable(set)
			// Refresh V3 flash pools — scan the registry for the cheapest V3
			// pool per token. Runs in the same goroutine as the borrowable
			// refresh so the flash selector is consistent.
			v3Pools := RefreshV3FlashPools(b.registry)
			b.flashSelector.SetV3FlashPools(v3Pools)
			// Refresh Aave reserves — query getReservesList() for all lendable tokens.
			if aaveAddr := b.cfg.Trading.AavePoolAddress; aaveAddr != "" {
				aaveReserves := RefreshAaveReserves(context.Background(), b.node, common.HexToAddress(aaveAddr))
				if aaveReserves != nil {
					b.flashSelector.SetAaveReserves(aaveReserves)
				}
				log.Printf("[flash] sources: balancer=%d v3=%d aave=%d", len(set), len(v3Pools), len(aaveReserves))
			} else {
				log.Printf("[flash] sources: balancer=%d v3=%d aave=disabled", len(set), len(v3Pools))
			}
			// Pre-warm the executor's vault balance cache for every borrowable
			// token so the first Submit per token doesn't pay ~60ms for a
			// synchronous RPC cold-miss. Trade 96 lost 61ms here on UNI.
			if b.executor != nil {
				addrs := make([]string, 0, len(set))
				for addr := range set {
					addrs = append(addrs, addr)
				}
				go b.executor.WarmVaultBalances(context.Background(), addrs)
			}
		},
	)
	// Pre-fetch nonce + baseFee + start vault balance cache refresh.
	// Eliminates ~30-50ms of synchronous RPC round-trips per trade submission.
	if b.executor != nil {
		b.executor.Warmup(ctx)
		// Primary vault refresh path: block-tick batched multicall. On every
		// new block header, onNewBlock kicks a single eth_call that reads all
		// borrowable token balances at once (~10-20ms, 1 RPC). This gives us
		// a 1-block staleness bound (~250ms) instead of the 20s of the
		// ticker-based refresher, which is what produced trade 124's BAL#528.
		b.executor.SetVaultRefreshTokens(b.flashSelector.BalancerBorrowableList)
		// Safety-net refresher: a 5-minute sequential re-fetch in case the
		// block-header subscription breaks for an extended window. Normal
		// operation sees block-tick refreshes every 250ms long before this
		// ticker ever fires — it's just belt-and-suspenders for RPC outages.
		b.executor.StartVaultCacheRefresher(ctx, b.flashSelector.BalancerBorrowableList, 5*time.Minute)
		b.ready.ExecutorWarmDone.Store(true)
		log.Println("[ready] executor warmup — nonce + baseFee loaded + vault refresher started (block-tick + 5m safety)")
	} else {
		// No executor configured (privKey or contract address missing). The
		// gate will block all submissions anyway because trySubmitCandidate
		// has its own `b.executor == nil` early-return, but we mark this
		// flag done so the readiness reason isn't misleading.
		b.ready.ExecutorWarmDone.Store(true)
	}

	if b.poolStore != nil {
		go b.poolStore.Run(ctx)
	}
	if b.arbLogger != nil {
		go b.arbLogger.Run(ctx)
	}
	if b.db != nil && b.cfg.Strategy.CompetitorWatchMinProfitUSD > 0 {
		cw := NewCompWatcher(b.db, b.node, b.registry, b.graph, b.tokens,
			b.swapListener, b.cycleCache, b.cfg.Strategy.CompetitorWatchMinProfitUSD)
		go cw.Run(ctx)
		log.Printf("[compwatcher] started (min_profit=$%.0f)", b.cfg.Strategy.CompetitorWatchMinProfitUSD)
	}
	// Periodic pool re-verification: walks every pool in the registry and
	// runs VerifyPool. Updates the verified flag in the DB so the cycle cache
	// knows which pools are safe to include. Runs once at startup (after a
	// small delay to let multicall populate state) and every hour after.
	go b.runPoolVerificationLoop(ctx)
	if b.yieldMon != nil {
		go b.yieldMon.Run(ctx)
	}
	if b.executor == nil {
		log.Println("[bot] no executor configured -- running in monitor-only mode")
	}

	// SIGUSR1 dumps the current cycle cache to the log.
	// Usage: kill -USR1 $(pgrep -f arb-bot-enum)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				cycles := b.cycleCache.All()
				log.Printf("[dump] %d cached cycles:", len(cycles))
				for i, c := range cycles {
					lp, _ := ScoreCycle(c)
					log.Printf("[dump] cycle[%d] lp=%.6f path=%s", i, lp, c.Cycle.Path())
				}
			}
		}
	}()

	<-ctx.Done()
	log.Println("[bot] shutting down")
}

// gasMonitorLoop polls eth_gasPrice every 30s and sets minProfitUSD to 2× estimated
// tx cost, so the execution threshold always tracks real network conditions.
// The config's min_profit_usd acts as a floor — we never go below it.
// rpcHealthLoop probes every configured RPC endpoint every 30s and publishes
// the result (reachable, latency, last block) to b.health.RPCStateJSON for the
// competitorCycleMatchLoop polls recently-inserted competitor_arbs rows and
// records whether our CycleCache currently contains a matching cycle. Runs on
// a tight cadence (every 3s) so we catch rows while the cache state is still
// close to what it was at the observed block. Rows older than 30s since the
// competitor tx are skipped — past that point the cycle cache has rebuilt
// multiple times and the check would no longer reflect "at time of recording".
//
// Matching is set-based: we consider two cycles equal if they have the same
// set of pool addresses (ignoring traversal order and rotation). This is the
// right semantic for "did we have the opportunity" — directional differences
// would surface as different cycles in the cache, but we care about whether
// the pool combination was known to us.
func (b *Bot) competitorCycleMatchLoop(ctx context.Context) {
	if b.db == nil {
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.annotateCompetitorCycleMatches(ctx)
		}
	}
}

// annotateCompetitorCycleMatches runs a single batch of the cycle-match check.
func (b *Bot) annotateCompetitorCycleMatches(ctx context.Context) {
	if b.cycleCache == nil {
		return
	}
	// Only evaluate rows observed in the last 30s. Beyond that, our cache
	// has moved on and the answer is no longer representative.
	since := time.Now().Unix() - 30
	rows, err := b.db.UnannotatedCompetitorsForCycleMatch(since, 200)
	if err != nil || len(rows) == 0 {
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		result := competitorCycleInCache(b.cycleCache, r.HopsJSONStr)
		_ = b.db.SetCompetitorCycleInMemory(r.ID, result)
	}
}

// competitorCycleInCache returns 1 if the competitor's cycle (as determined
// by its hops_json pool sequence) is present in our cycle cache, 0 otherwise.
// Uses set-based matching: same pool count + same pool address set.
func competitorCycleInCache(cc *CycleCache, hopsJSON string) int {
	if cc == nil || hopsJSON == "" {
		return 0
	}
	// Parse out the pool addresses in the competitor's hop sequence.
	var hops []struct {
		Pool string `json:"pool"`
	}
	if err := json.Unmarshal([]byte(hopsJSON), &hops); err != nil || len(hops) == 0 {
		return 0
	}
	wantSet := make(map[string]struct{}, len(hops))
	for _, h := range hops {
		if h.Pool == "" {
			continue
		}
		wantSet[strings.ToLower(h.Pool)] = struct{}{}
	}
	if len(wantSet) == 0 {
		return 0
	}
	// Narrow the search space by looking up cycles containing the first pool.
	// If that pool isn't in the cache index, no cycle can match.
	firstPool := strings.ToLower(hops[0].Pool)
	candidates := cc.CyclesForPool(firstPool)
	for _, c := range candidates {
		if len(c.PoolAddrs) != len(hops) {
			continue
		}
		match := true
		for _, addr := range c.PoolAddrs {
			if _, ok := wantSet[strings.ToLower(addr)]; !ok {
				match = false
				break
			}
		}
		if match {
			return 1
		}
	}
	return 0
}

// dashboard. Each probe runs in its own goroutine so a slow endpoint doesn't
// block the others. The persistent ethclient for ArbitrumRPC is reused; the
// other endpoints get a fresh dial each round (their RPS budget can absorb it
// and we want to detect connection-level failures, not just transport hiccups).
func (b *Bot) rpcHealthLoop(ctx context.Context) {
	type rpcEndpoint struct {
		Name string
		URL  string
		Role string
	}
	endpoints := []rpcEndpoint{
		{"arbitrum_rpc", b.cfg.ArbitrumRPC, "live tracking"},
		{"l1_rpc", b.cfg.L1RPC, "L1 sync"},
		{"sequencer_rpc", b.cfg.Trading.SequencerRPC, "tx submit"},
		{"simulation_rpc", b.cfg.Trading.SimulationRPC, "eth_call sim"},
		{"tick_data_rpc", b.cfg.Trading.TickDataRPC, "tick fetches"},
	}

	type rpcStatus struct {
		Name             string `json:"name"`
		URL              string `json:"url"`
		Role             string `json:"role"`
		OK               bool   `json:"ok"`
		LatencyMs        int64  `json:"latency_ms"`
		BlockNum         uint64 `json:"block_num"`
		Error            string `json:"error,omitempty"`
		CheckedAt        int64  `json:"checked_at"`
		RateLimit429     int64  `json:"rate_limit_429"`     // cumulative count since bot started
		LastRateLimitAt  int64  `json:"last_rate_limit_at"` // unix ts of most recent 429, 0 if none
	}

	probe := func(ep rpcEndpoint) rpcStatus {
		st := rpcStatus{Name: ep.Name, URL: ep.URL, Role: ep.Role, CheckedAt: time.Now().Unix()}
		if ep.URL == "" {
			st.Error = "not configured"
			return st
		}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var client *ethclient.Client
		var err error
		// Reuse the persistent connection for the main RPC; redial for the others.
		if ep.Name == "arbitrum_rpc" && b.node != nil {
			client = b.node
		} else {
			client, err = ethclient.DialContext(dialCtx, ep.URL)
			if err != nil {
				st.Error = "dial: " + err.Error()
				return st
			}
			defer client.Close()
		}
		// The Arbitrum sequencer endpoint is *truly* submission-only — every
		// read RPC (eth_blockNumber, eth_chainId, net_version, web3_clientVersion,
		// eth_syncing) returns -32601 "method not available". The only way to
		// probe it is to actually call eth_sendRawTransaction with deliberately
		// invalid data and check what error comes back:
		//   - "typed transaction too short" (or similar parse error) → endpoint
		//     is alive and accepting submissions (probe passes)
		//   - "method not available" → broken / wrong endpoint (probe fails)
		//   - dial / network error → unreachable (probe fails)
		t0 := time.Now()
		if ep.Name == "sequencer_rpc" {
			// We bypass ethclient here because it doesn't expose a way to send
			// a bare hex string — it would marshal a real types.Transaction.
			// Use a one-shot http.Post with a JSON-RPC envelope.
			body := `{"jsonrpc":"2.0","method":"eth_sendRawTransaction","params":["0x00"],"id":1}`
			req, _ := http.NewRequestWithContext(dialCtx, http.MethodPost, ep.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			st.LatencyMs = time.Since(t0).Milliseconds()
			if err != nil {
				st.Error = "dial: " + err.Error()
				return st
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			text := string(raw)
			if resp.StatusCode != 200 {
				st.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
				return st
			}
			// We expect a parse error (typed tx too short / rlp / invalid sender).
			// Any of those mean the endpoint is alive. The one error we DON'T
			// want to see is -32601 method-not-available.
			if strings.Contains(text, "-32601") || strings.Contains(text, "method") && strings.Contains(text, "not available") {
				st.Error = "rejects eth_sendRawTransaction"
				return st
			}
			st.OK = true
			return st
		}
		bn, err := client.BlockNumber(dialCtx)
		st.LatencyMs = time.Since(t0).Milliseconds()
		if err != nil {
			st.Error = err.Error()
			return st
		}
		st.OK = true
		st.BlockNum = bn
		return st
	}

	publish := func() {
		results := make([]rpcStatus, len(endpoints))
		var wg sync.WaitGroup
		for i, ep := range endpoints {
			wg.Add(1)
			go func(i int, ep rpcEndpoint) {
				defer wg.Done()
				results[i] = probe(ep)
			}(i, ep)
		}
		wg.Wait()
		// Snapshot the rate-limit counters and roll them into each rpcStatus.
		// Done after probes so we don't hold the lock during slow network calls.
		b.health.RPCRateLimitMu.Lock()
		for i := range results {
			if c, ok := b.health.RPCRateLimit[results[i].Name]; ok {
				results[i].RateLimit429 = c
			}
			if t, ok := b.health.RPCRateLimitLastAt[results[i].Name]; ok {
				results[i].LastRateLimitAt = t
			}
		}
		b.health.RPCRateLimitMu.Unlock()
		if data, err := json.Marshal(results); err == nil {
			b.health.RPCStateJSON.Store(string(data))
		}
	}

	// Run once immediately so the dashboard isn't empty for the first 30s.
	publish()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

func (b *Bot) gasMonitorLoop(ctx context.Context) {
	const (
		gasLimit      = 600_000 // conservative estimate for a flash loan arb tx
		wethPriceFall = 3400.0  // fallback WETH/USD if no pool available
		interval      = 30 * time.Second
	)
	safetyMult := b.cfg.Strategy.GasSafetyMult

	update := func() {
		gasPrice, err := b.node.SuggestGasPrice(ctx)
		if err != nil {
			return // keep current value on error
		}
		// cost in ETH = gasPrice (wei) * gasLimit / 1e18
		gasCostETH := new(big.Float).Quo(
			new(big.Float).Mul(
				new(big.Float).SetInt(gasPrice),
				new(big.Float).SetFloat64(float64(gasLimit)),
			),
			new(big.Float).SetFloat64(1e18),
		)
		gasCostETHf, _ := gasCostETH.Float64()

		// Get live WETH price from a known pool if possible
		wethUSD := wethPriceFall
		for _, p := range b.registry.All() {
			if (p.Token0.Symbol == "WETH" && (p.Token1.Symbol == "USDC" || p.Token1.Symbol == "USDC.e" || p.Token1.Symbol == "USDT")) ||
				(p.Token1.Symbol == "WETH" && (p.Token0.Symbol == "USDC" || p.Token0.Symbol == "USDC.e" || p.Token0.Symbol == "USDT")) {
				spot := p.SpotRate()
				if spot > 100 && spot < 100_000 {
					if p.Token0.Symbol == "WETH" {
						wethUSD = spot
					} else {
						wethUSD = 1.0 / spot
					}
					break
				}
			}
		}

		gasCostUSD := gasCostETHf * wethUSD
		dynamicMin := gasCostUSD * safetyMult

		// Never go below the config floor
		floor := b.cfg.Trading.MinProfitUSD
		if dynamicMin < floor {
			dynamicMin = floor
		}

		prev := b.minProfitUSD.Load().(float64)
		b.minProfitUSD.Store(dynamicMin)

		gasPriceGwei, _ := new(big.Float).Quo(new(big.Float).SetInt(gasPrice), new(big.Float).SetFloat64(1e9)).Float64()
		b.lastGasPriceGwei.Store(gasPriceGwei)
		b.gasCostPerUnit.Store(gasPriceGwei * 1e-9 * wethUSD)
		if prev == 0 || fmt.Sprintf("%.4f", prev) != fmt.Sprintf("%.4f", dynamicMin) {
			log.Printf("[gas] price=%.4f gwei  txCost=$%.4f  minProfit=$%.4f (%.1f× cost, floor=$%.3f)",
				gasPriceGwei, gasCostUSD, dynamicMin, safetyMult, floor,
			)
		}
	}

	update() // run immediately on start
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}

func (b *Bot) subgraphRefreshLoop(ctx context.Context) {
	cfg := b.cfg.Pools.Subgraph
	if !cfg.Enabled {
		return
	}
	hours := cfg.RefreshHours
	if hours <= 0 {
		hours = 4
	}
	// Run once at startup so the dashboard shows volume immediately instead
	// of waiting up to 4h for the first ticker fire. Done in a goroutine so
	// startup isn't blocked by GraphQL latency.
	go func() {
		log.Println("[subgraph] initial pool metrics refresh...")
		RefreshPoolMetrics(ctx, cfg, b.cfg.UniswapAPIKey, b.registry, b.tokens)
	}()
	ticker := time.NewTicker(time.Duration(hours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Println("[subgraph] refreshing pool metrics...")
			RefreshPoolMetrics(ctx, cfg, b.cfg.UniswapAPIKey, b.registry, b.tokens)
		}
	}
}

func (b *Bot) bootstrapPools() {
	log.Printf("[bot] token registry: %d tokens loaded", len(b.tokens.All()))

	// ── Phase 0: warm-start from SQLite (instant, metadata + flags only) ────
	// Volatile state (prices, reserves, ticks) is NOT loaded — re-derived from
	// chain via multicall within 5 seconds. This avoids stale-data conflicts.
	//
	// Self-healing token resolver: when LoadPools encounters a pool whose token0
	// or token1 isn't in the in-memory TokenRegistry (and wasn't rehydrated from
	// the tokens table because it was orphaned by a buggy past discovery path),
	// fall back to fetching the token's symbol+decimals directly from chain.
	// This recovers BalancerW (and any other) pools whose tokens never made it
	// into the tokens table due to historical persistence gaps. The resolver is
	// rate-limited indirectly by the per-pool cost (~150ms each) — orphans are
	// rare so the total startup overhead is bounded.
	resolveMissingToken := func(addr string) *Token {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t := FetchTokenMeta(ctx, b.node, addr)
		if t == nil || t.Symbol == "" || t.Symbol == "UNK" {
			return nil
		}
		return t
	}
	if b.db != nil {
		dbPools, err := b.db.LoadPools(b.tokens, resolveMissingToken)
		if err != nil {
			log.Printf("[bot] DB warm-start failed: %v", err)
		} else if len(dbPools) > 0 {
			dbAdded := 0
			for _, p := range dbPools {
				if canonical, ok := b.registry.ForceAdd(p); ok {
					b.graph.AddPool(canonical)
					dbAdded++
				}
			}
			log.Printf("[bot] DB warm-start: %d pools loaded (metadata only, awaiting multicall)", dbAdded)
		}
	}

	// ── Phase 0b: V4 pools from v4_pools table (with fee/tickSpacing/hooks) ──
	// V4 pools are keyed by poolId (not contract address). We use the poolId as
	// the Address field so the registry can look them up from V4 Swap events.
	if b.db != nil {
		v4Rows, err := b.db.LoadV4PoolsFull()
		if err == nil && len(v4Rows) > 0 {
			v4Added := 0
			for _, r := range v4Rows {
				tok0, ok0 := b.tokens.Get(r.Token0)
				tok1, ok1 := b.tokens.Get(r.Token1)
				if !ok0 || !ok1 {
					continue // skip V4 pools with unknown tokens
				}
				ts := r.TickSpacing
				if ts == 0 {
					// Backfill: use Uniswap convention. Vanilla V4 pools (no exotic hooks)
					// follow the same fee→tickSpacing mapping as V3.
					switch r.FeePPM {
					case 100:
						ts = 1
					case 500:
						ts = 10
					case 3000:
						ts = 60
					case 10000:
						ts = 200
					default:
						// Unknown fee tier — guess based on magnitude
						if r.FeePPM <= 100 {
							ts = 1
						} else if r.FeePPM <= 500 {
							ts = 10
						} else if r.FeePPM <= 3000 {
							ts = 60
						} else {
							ts = 200
						}
					}
				}
				p := &Pool{
					Address:     r.PoolID,
					DEX:         DEXUniswapV4,
					Token0:      tok0,
					Token1:      tok1,
					PoolID:      r.PoolID,
					FeePPM:      r.FeePPM,
					FeeBps:      r.FeePPM / 100,
					TickSpacing: ts,
					V4Hooks:     r.Hooks,
					Verified:    r.Verified,
				}
				if canonical, ok := b.registry.ForceAdd(p); ok {
					b.graph.AddPool(canonical)
					v4Added++
				}
			}
			if v4Added > 0 {
				log.Printf("[bot] V4 pools: %d loaded with exact fees from v4_pools", v4Added)
			}
		}
	}

	// ── Phase 1: subgraph seeds (top pools by TVL, real metrics) ─────────────
	if b.cfg.Pools.Subgraph.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		pools := FetchAllSubgraphPools(ctx, b.cfg.Pools.Subgraph, b.tokens)
		cancel()
		sgAdded := 0
		for _, p := range pools {
			if canonical, ok := b.registry.ForceAdd(p); ok {
				b.graph.AddPool(canonical)
				sgAdded++
			}
		}
		log.Printf("[bot] subgraph seeded %d pools", sgAdded)
	}

	// ── Phase 2: pinned pools (curated address list — always present) ───────
	// For each address in pinned_pools: ensure it exists in the DB (resolving
	// DEX/tokens/fee from the chain if missing) and mark it pinned. Pinned pools
	// are protected from TVL/volume pruning. After EnsurePoolPinned writes the
	// row, we reload from DB so the in-memory Pool reflects the canonical state.
	pinned := 0
	resolved := 0
	for _, addr := range b.cfg.Pools.PinnedPools {
		addrLo := strings.ToLower(addr)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		newP, err := b.db.EnsurePoolPinned(ctx, b.node, b.registry, b.tokens, addrLo)
		cancel()
		if err != nil {
			log.Printf("[bot] pinned pool %s: %v", addrLo[:14], err)
			continue
		}
		pinned++
		// If a new pool was just resolved from chain, add it directly to the
		// in-memory registry+graph so it's available immediately (without waiting
		// for the next bootstrap warm-start cycle).
		if newP != nil {
			if canonical, ok := b.registry.ForceAdd(newP); ok {
				b.graph.AddPool(canonical)
				resolved++
			}
		}
	}
	log.Printf("[bot] pinned %d pools (%d newly resolved from chain), registry total: %d",
		pinned, resolved, b.registry.Len())
}

// watchNewBlocks runs the fast pool-state refresh loop, **block-driven** via
// a WSS new-head subscription so the bot's view of pool state is at most one
// Arbitrum block (~250ms) behind chain. Falls back to a 5s ticker if the WSS
// subscription fails or drops, so the loop never goes silent.
//
// The work is coalesced through a 1-buffered trigger channel: each new block
// header tries to non-blocking send a "go!" token. If a multicall pass is
// already in flight (the channel is full), the token is dropped — that's
// fine because the in-flight pass will already see whatever state the
// dropped block produced by the time it finishes. This caps the in-flight
// work at one pass at a time and prevents unbounded queueing.
//
// Responsibilities of each pass: BatchUpdatePools + FetchV4PoolStates +
// MulticallAt update + V2/V3 TVL recompute + graph weight refresh. The
// slower tick-map refresh runs in its own watchTickMapsLoop goroutine.

// currentStateBlock returns the latest block number from the main node RPC
// as a *big.Int suitable for BatchUpdatePoolsAtBlock / FetchV4PoolStates.
// Returns nil if the call fails — callers must treat nil as "stamp nothing,
// caller is responsible for handling gate-fail downstream".
func (b *Bot) currentStateBlock(ctx context.Context) *big.Int {
	bn64, err := b.node.BlockNumber(ctx)
	if err != nil {
		return nil
	}
	return new(big.Int).SetUint64(bn64)
}

// currentTickBlock returns the latest block number from the tick-data RPC
// (which may be distinct from the main node) so FetchTickMaps pins its
// multicall reads to a block we can stamp onto Pool.TicksBlock.
//
// Also records the observed block height for the RPC skew gate: if the
// tick_data RPC lags arbitrum_rpc by more than cfg.Trading.TickRPCMaxSkewBlocks,
// the health gate blocks trade submission (bitmap state would be on a stale
// branch relative to slot0 state). Without this probe a slow tick RPC would
// produce sim outputs that silently disagree with on-chain reality.
func (b *Bot) currentTickBlock(ctx context.Context) *big.Int {
	client := b.tickClient
	if client == nil {
		client = b.node
	}
	bn64, err := client.BlockNumber(ctx)
	if err != nil {
		return nil
	}
	b.health.TickRPCBlock.Store(bn64)
	return new(big.Int).SetUint64(bn64)
}

func (b *Bot) watchNewBlocks(ctx context.Context) {
	// Coalesced trigger. Buffer of 1 so a new block can wake the worker
	// without blocking, but multiple blocks arriving while a pass is in
	// flight collapse to a single pending wake-up. Carries the block number
	// of the header that triggered the pass so multicall/v4 reads happen
	// pinned to that block and StateBlock is stamped correctly for the
	// block-lag freshness gate.
	trigger := make(chan *big.Int, 1)

	// Header subscription goroutine. Pushes the header's block number into
	// `trigger` on every new block. On error/disconnect, falls back to
	// polling the latest block via BlockNumber() so the loop keeps refreshing
	// even if the WSS subscription dies.
	go func() {
		headers := make(chan *types.Header, 8)
		sub, err := b.node.SubscribeNewHead(ctx, headers)
		if err != nil {
			log.Printf("[bot] watchNewBlocks: header sub failed (%v) — falling back to 5s ticker", err)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					bn64, err := b.node.BlockNumber(ctx)
					if err != nil {
						continue
					}
					bn := new(big.Int).SetUint64(bn64)
					select {
					case trigger <- bn:
					default:
					}
				}
			}
		}
		defer sub.Unsubscribe()
		// Also run a slow safety-net ticker so a stuck/silent subscription
		// doesn't freeze multicall freshness. 5s is well under the 15s
		// max_multicall_age_sec gate.
		safetyTicker := time.NewTicker(5 * time.Second)
		defer safetyTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-sub.Err():
				log.Printf("[bot] watchNewBlocks: header sub error (%v) — re-subscribing in 2s", err)
				time.Sleep(2 * time.Second)
				newSub, err2 := b.node.SubscribeNewHead(ctx, headers)
				if err2 != nil {
					log.Printf("[bot] watchNewBlocks: re-subscribe failed: %v", err2)
					continue
				}
				sub = newSub
			case h := <-headers:
				bn := new(big.Int).Set(h.Number)
				select {
				case trigger <- bn:
				default:
				}
			case <-safetyTicker.C:
				bn64, err := b.node.BlockNumber(ctx)
				if err != nil {
					continue
				}
				bn := new(big.Int).SetUint64(bn64)
				select {
				case trigger <- bn:
				default:
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case bn := <-trigger:
			if bn != nil && bn.IsUint64() {
				bnU := bn.Uint64()
				if bnU > b.health.LatestBlock.Load() {
					b.health.LatestBlock.Store(bnU)
				}
			}
			pools := b.registry.All()
			if len(pools) == 0 {
				continue
			}
			// Pass nil so BatchUpdatePoolsAtBlock reads `latest` and stamps
			// pools with the block the RPC actually observed at read time,
			// NOT the trigger block bn. Prevents the "multicall for block B,
			// returned 750 ms later when B+3 is head" regate-reject class.
			if err := BatchUpdatePoolsAtBlock(ctx, b.node, pools, nil); err != nil {
				log.Printf("[bot] multicall error: %v", err)
				if is429(err) {
					b.health.RecordRPC429("arbitrum_rpc")
				}
				continue
			}
			b.health.MulticallAt.Store(time.Now().Unix())
			if !b.ready.MulticallDone.Load() {
				b.ready.MulticallDone.Store(true)
				log.Println("[ready] multicall — first pass complete")
			}
			// V4 lives behind a singleton PoolManager and is not handled by
			// BatchUpdatePools. Fetch sqrtPrice/liquidity/tick via the StateView
			// helper using the same RPC budget as the tick-data fetcher.
			if err := FetchV4PoolStates(ctx, b.node, pools, bn); err != nil {
				log.Printf("[bot] v4 state fetch error: %v", err)
				if is429(err) {
					b.health.RecordRPC429("arbitrum_rpc")
				}
			}
			// Count how many V3 pools have valid state after multicall (logged at most every 5 min)
			v3total, v3withLiq, v3withPrice := 0, 0, 0
			for _, p := range pools {
				switch p.DEX {
				case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
					v3total++
					if p.SqrtPriceX96 != nil && p.SqrtPriceX96.Sign() > 0 {
						v3withPrice++
					}
					if p.Liquidity != nil && p.Liquidity.Sign() > 0 {
						v3withLiq++
					}
				}
			}
			if v3total > 0 && time.Since(b.lastMulticallLog) >= 5*time.Minute {
				log.Printf("[multicall] v3pools=%d withPrice=%d withLiquidity=%d", v3total, v3withPrice, v3withLiq)
				b.lastMulticallLog = time.Now()
			}
			// Recompute TVL on-chain. V2-style: from reserves. V3/V4: from
			// active liquidity + sqrtPriceX96. Subgraph TVL is never trusted —
			// see recomputeV2TVL / recomputeV3TVL docs for the formulas.
			v2Recomputed, v3Recomputed := 0, 0
			for _, p := range pools {
				switch p.DEX {
				case DEXUniswapV2, DEXSushiSwap, DEXCamelot, DEXTraderJoe,
					DEXRamsesV2, DEXChronos, DEXDeltaSwap, DEXSwapr, DEXArbSwap:
					if newTVL := recomputeV2TVL(p, b.registry); newTVL > 0 {
						p.mu.Lock()
						p.TVLUSD = newTVL
						p.mu.Unlock()
						v2Recomputed++
					}
				case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3,
					DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
					if newTVL := recomputeV3TVL(p, b.registry); newTVL > 0 {
						p.mu.Lock()
						p.TVLUSD = newTVL
						p.mu.Unlock()
						v3Recomputed++
					}
					// V4 lives in its own table — push current state so the
					// dashboard reflects fresh TVL/volume without restarting.
					if p.DEX == DEXUniswapV4 && b.db != nil {
						p.mu.RLock()
						sqrtStr, liqStr := "", ""
						if p.SqrtPriceX96 != nil {
							sqrtStr = p.SqrtPriceX96.String()
						}
						if p.Liquidity != nil {
							liqStr = p.Liquidity.String()
						}
						_ = b.db.UpsertV4Pool(p.Address, p.Token0.Address, p.Token1.Address,
							p.V4Hooks, int(p.FeePPM), int(p.TickSpacing), int(p.Tick),
							sqrtStr, liqStr, p.TVLUSD, p.Volume24hUSD, p.SpotPrice)
						p.mu.RUnlock()
					}
				}
			}
			if (v2Recomputed > 0 || v3Recomputed > 0) && time.Since(b.lastMulticallLog) >= 5*time.Minute {
				log.Printf("[tvl] recomputed v2=%d v3+v4=%d pools from on-chain state", v2Recomputed, v3Recomputed)
			}
			for _, p := range pools {
				b.graph.UpdateEdgeWeights(p)
			}
		}
	}
}

// watchTickMapsLoop runs the slow V3-cycle-pool tick refresh on its own
// independent ticker so that its ~5–10s wall-clock cost doesn't lag the
// MulticallAt health timestamp. Updates Health.TickDataAt + TickedPools{Have,Total}
// + TrackedAddrsJSON. The dashboard's tick freshness display reads these.
func (b *Bot) watchTickMapsLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// Eager re-fetch channel may be nil when there's no swap listener wired
	// (e.g. early test paths). Use a nil channel in the select so the case
	// blocks forever instead of busy-firing on closed/nil receive.
	var refetchCh <-chan *Pool
	if b.swapListener != nil {
		refetchCh = b.swapListener.TickRefetchCh
	}
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-refetchCh:
			// Eager single-pool tick re-fetch path. Triggered by the swap
			// listener when a pool's tick has drifted past the bitmap window
			// captured at the last FetchTickMaps pass. Drain any additional
			// pools that arrived in the same instant so we batch them into
			// one multicall instead of issuing N back-to-back round trips.
			pools := []*Pool{p}
			seen := map[string]bool{strings.ToLower(p.Address): true}
		drain:
			for {
				select {
				case q := <-refetchCh:
					k := strings.ToLower(q.Address)
					if !seen[k] {
						seen[k] = true
						pools = append(pools, q)
					}
				default:
					break drain
				}
			}
			start := time.Now()
			bn := b.currentTickBlock(ctx)
			radius := b.cfg.Trading.TickBitmapCoverageWords
			stats, err := FetchTickMaps(ctx, b.tickClient, pools, radius, bn)
			if err != nil {
				log.Printf("[bot] eager tick re-fetch (%d pools) error: %v failed=%d", len(pools), err, stats.PoolsFailed)
				if is429(err) {
					tickRPCName := "arbitrum_rpc"
					if b.cfg.Trading.TickDataRPC != "" {
						tickRPCName = "tick_data_rpc"
					} else if b.cfg.Trading.SimulationRPC != "" {
						tickRPCName = "simulation_rpc"
					}
					b.health.RecordRPC429(tickRPCName)
				}
			} else if time.Since(start) > 250*time.Millisecond || len(pools) > 4 || stats.PoolsFailed > 0 {
				log.Printf("[tickmap] eager re-fetch %d pool(s) in %v ok=%d empty=%d failed=%d rt_mismatch=%d",
					len(pools), time.Since(start), stats.PoolsSucceeded, stats.PoolsEmpty, stats.PoolsFailed, stats.AlgebraRTMismatches)
			}
		case <-ticker.C:
			if b.cycleCache == nil {
				continue
			}
			pools := b.registry.All()
			if len(pools) == 0 {
				continue
			}
			v3cyclePoolSet := make(map[string]bool)
			for _, cc := range b.cycleCache.All() {
				for _, e := range cc.Cycle.Edges {
					switch e.Pool.DEX {
					case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
						v3cyclePoolSet[strings.ToLower(e.Pool.Address)] = true
					}
				}
			}
			if len(v3cyclePoolSet) == 0 {
				// Nothing to fetch yet (cycle cache hasn't built or has no V3
				// pools). If the cycle cache HAS already rebuilt, mark
				// TickMapsDone so the readiness gate doesn't block forever
				// in the unlikely "all-V2-cycles" configuration.
				if b.ready.CycleRebuildDone.Load() && !b.ready.TickMapsDone.Load() {
					b.ready.TickMapsDone.Store(true)
					log.Println("[ready] tick maps — no V3 pools in cycles, gate cleared")
				}
				continue
			}
			var v3cyclePools []*Pool
			for _, p := range pools {
				if v3cyclePoolSet[strings.ToLower(p.Address)] {
					v3cyclePools = append(v3cyclePools, p)
				}
			}
			// Use the persistent tick data client (dialed once at startup).
			// Avoids per-iteration TCP/TLS handshake and connection leaks
			// that caused "context canceled" errors and stale-tick flicker.
			passStart := time.Now()
			bn := b.currentTickBlock(ctx)
			radius := b.cfg.Trading.TickBitmapCoverageWords
			stats, err := FetchTickMaps(ctx, b.tickClient, v3cyclePools, radius, bn)
			b.health.TickFetchPassTotal.Store(int64(stats.Pools))
			b.health.TickFetchPassSucceeded.Store(int64(stats.PoolsSucceeded))
			b.health.TickFetchPassEmpty.Store(int64(stats.PoolsEmpty))
			b.health.TickFetchPassFailed.Store(int64(stats.PoolsFailed))
			b.health.TickFetchPassSkipped.Store(int64(stats.PoolsSkipped))
			b.health.TickFetchPassRTMismatch.Store(int64(stats.AlgebraRTMismatches))
			b.health.TickFetchPassDurMs.Store(time.Since(passStart).Milliseconds())
			if bn != nil && bn.IsUint64() {
				b.health.TickFetchPassBlockTick.Store(bn.Uint64())
			}
			b.health.TickFetchPassBlockArb.Store(b.health.LatestBlock.Load())
			if err != nil {
				log.Printf("[bot] tick map fetch error: %v failed=%d/%d", err, stats.PoolsFailed, stats.Pools)
				if is429(err) {
					tickRPCName := "arbitrum_rpc"
					if b.cfg.Trading.TickDataRPC != "" {
						tickRPCName = "tick_data_rpc"
					} else if b.cfg.Trading.SimulationRPC != "" {
						tickRPCName = "simulation_rpc"
					}
					b.health.RecordRPC429(tickRPCName)
				}
			} else {
				b.health.TickDataAt.Store(time.Now().Unix())
				if !b.ready.TickMapsDone.Load() {
					b.ready.TickMapsDone.Store(true)
					log.Println("[ready] tick maps — first pass complete")
				}
				if stats.AlgebraRTMismatches > 0 {
					log.Printf("[tickmap] WARN: %d algebra round-trip mismatches detected — bitmap index math regression", stats.AlgebraRTMismatches)
				}
				if stats.PoolsFailed > 0 {
					log.Printf("[tickmap] %d/%d pools failed verification this pass (OK=false; coverage gate will reject them)", stats.PoolsFailed, stats.Pools)
				}
			}
			// Tick coverage stats
			tickPoolCount, totalTicks := 0, 0
			for _, p := range v3cyclePools {
				p.mu.RLock()
				n := len(p.Ticks)
				p.mu.RUnlock()
				if n > 0 {
					tickPoolCount++
					totalTicks += n
				}
			}
			b.health.TickedPoolsHave.Store(int64(tickPoolCount))
			b.health.TickedPoolsTotal.Store(int64(len(v3cyclePools)))
			// Per-pool freshness for the dashboard's "click Ticks → filter Pools"
			// navigation. Store the absolute unix timestamp of TicksUpdatedAt —
			// NOT an "age" delta. The dashboard computes the age at render time.
			type poolStatus struct {
				A string `json:"a"` // address (lowercase)
				U int64  `json:"u"` // unix seconds when ticks were last refreshed; 0 if never
				T int    `json:"t"` // 1 if Ticks slice is non-empty, else 0
			}
			trackedStatus := make([]poolStatus, 0, len(v3cyclePools))
			for _, p := range v3cyclePools {
				p.mu.RLock()
				var u int64
				if !p.TicksUpdatedAt.IsZero() {
					u = p.TicksUpdatedAt.Unix()
				}
				hasT := 0
				if len(p.Ticks) > 0 {
					hasT = 1
				}
				p.mu.RUnlock()
				trackedStatus = append(trackedStatus, poolStatus{
					A: strings.ToLower(p.Address),
					U: u,
					T: hasT,
				})
			}
			if data, err := json.Marshal(trackedStatus); err == nil {
				b.health.TrackedAddrsJSON.Store(string(data))
			}
			if totalTicks > 0 && time.Since(b.lastMulticallLog) >= 5*time.Minute {
				log.Printf("[tickmap] %d ticks across %d/%d cycle pools", totalTicks, tickPoolCount, len(v3cyclePools))
			}
		}
	}
}

// algebraFeeRefreshLoop drains b.swapListener.AlgebraFeeRefreshCh and re-reads
// the dynamic fee for each Algebra pool that just emitted a Swap event. The
// Algebra Swap event payload doesn't include the post-swap fee, so without
// this the simulator scores the next cycle on the previous tick's fee — which
// can be off by 50–200% during volatile windows.
//
// Reuses BatchUpdatePools with a single-element slice: for DEXCamelotV3 /
// DEXZyberV3 it packs globalState() (which decodeGlobalState already maps to
// FeePPM/FeeBps), liquidity(), and tickSpacing() in one Multicall2 call —
// ~50ms round trip. Drains the channel between calls so a hot pool that
// fired several events still costs us only one RPC.
func (b *Bot) algebraFeeRefreshLoop(ctx context.Context) {
	if b.swapListener == nil || b.swapListener.AlgebraFeeRefreshCh == nil {
		return
	}
	ch := b.swapListener.AlgebraFeeRefreshCh
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-ch:
			pools := []*Pool{p}
			seen := map[string]bool{strings.ToLower(p.Address): true}
		drain:
			for {
				select {
				case q := <-ch:
					k := strings.ToLower(q.Address)
					if !seen[k] {
						seen[k] = true
						pools = append(pools, q)
					}
				default:
					break drain
				}
			}
			start := time.Now()
			bn := b.currentStateBlock(ctx)
			if err := BatchUpdatePoolsAtBlock(ctx, b.node, pools, bn); err != nil {
				log.Printf("[bot] algebra fee refresh (%d pools) error: %v", len(pools), err)
				if is429(err) {
					b.health.RecordRPC429("arbitrum_rpc")
				}
				continue
			}
			// Recompute graph edge weights for any pool whose fee just changed,
			// since edge weight = log(price * (1 - fee)) and the simulator's
			// next fastEval pass may already be running on stale weights.
			for _, q := range pools {
				b.graph.UpdateEdgeWeights(q)
			}
			if time.Since(start) > 250*time.Millisecond || len(pools) > 4 {
				log.Printf("[algebra-fee] refreshed %d pool(s) in %v", len(pools), time.Since(start))
			}
		}
	}
}

func (b *Bot) watchPoolCreation(ctx context.Context) {
	disc := NewDiscoverer(b.node, b.tokens, b.registry, b.graph)
	disc.onPoolAdded = b.swapListener.NotifyPoolAdded
	disc.Run(ctx)
}

func (b *Bot) watchStats(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pools := b.registry.All()
			edges := b.graph.AllEdges()
			live := 0
			for _, e := range edges {
				if e.LogWeight > -1e300 {
					live++
				}
			}
			swaps := atomic.LoadUint64(&b.swapListener.swapsTotal)
			log.Printf("[stats] pools=%d edges=%d live=%d swaps=%d cached_cycles=%d",
				len(pools), len(edges), live, swaps, b.cycleCache.Len())
			// Candidate reject-rate breakdown. Resets to zero each flush so the
			// numbers are per-interval (not cumulative), making it easy to spot
			// which gate is doing the heavy lifting and which trades are
			// passing all gates in any given window.
			cTotal := b.candTotal.Swap(0)
			cUnverified := b.candRejectUnverified.Swap(0)
			cScore := b.candRejectScore.Swap(0)
			cLPFloor := b.candRejectLPFloor.Swap(0)
			cEmpty := b.candRejectEmpty.Swap(0)
			cTickStale := b.candRejectTickStale.Swap(0)
			cTickNeverFetched := b.candRejectTickNeverFetched.Swap(0)
			cTickFetchFailed := b.candRejectTickFetchFailed.Swap(0)
			cTickCoverageDrift := b.candRejectTickCoverageDrift.Swap(0)
			cTickEmptyVerified := b.candRejectTickEmptyVerified.Swap(0)
			cPoolStale := b.candRejectPoolStale.Swap(0)
			cUnviable := b.candRejectUnviable.Swap(0)
			cMinBps := b.candRejectMinBps.Swap(0)
			cNoFlash := b.candRejectNoFlash.Swap(0)
			cAccepted := b.candAccepted.Swap(0)
			eagerDropped := b.health.TickFetchEagerDropped.Swap(0)
			eagerEnqueued := b.health.TickFetchEagerEnqueued.Swap(0)
			if cTotal > 0 {
				log.Printf("[candstats] total=%d unverified=%d score=%d lpfloor=%d empty=%d tick_stale=%d(never=%d fail=%d drift=%d empty=%d) pool_stale=%d unviable=%d min_bps=%d no_flash=%d accepted=%d",
					cTotal, cUnverified, cScore, cLPFloor, cEmpty, cTickStale, cTickNeverFetched, cTickFetchFailed, cTickCoverageDrift, cTickEmptyVerified, cPoolStale, cUnviable, cMinBps, cNoFlash, cAccepted)
			}
			if eagerDropped > 0 || eagerEnqueued > 0 {
				log.Printf("[tickmap] eager refetch: enqueued=%d dropped=%d (drop_rate=%.2f%%)",
					eagerEnqueued, eagerDropped, 100.0*float64(eagerDropped)/float64(eagerDropped+eagerEnqueued+1))
			}
			// Flush health status to SQLite for the dashboard.
			if b.db != nil {
				trackedJSON, _ := b.health.TrackedAddrsJSON.Load().(string)
				rpcJSON, _ := b.health.RPCStateJSON.Load().(string)
				b.db.UpdateHealth(
					b.health.TickDataAt.Load(),
					b.health.MulticallAt.Load(),
					b.health.SwapEventAt.Load(),
					b.health.CycleRebuildAt.Load(),
					b.health.TickedPoolsHave.Load(),
					b.health.TickedPoolsTotal.Load(),
					trackedJSON,
					rpcJSON,
				)
			}
		}
	}
}

func (b *Bot) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.prune()
		}
	}
}

func (b *Bot) prune() {
	pools := b.registry.All()
	removed := 0
	qualityRemoved := 0
	for _, p := range pools {
		// Composite quality filter — applies even to pinned pools, because
		// the filter exists specifically to keep scam/honeypot pools out of
		// the cycle cache regardless of why they were added. A pinned pool
		// that fails the quality gate was either added by mistake or has
		// rugged since it was pinned; either way we want it out.
		if reason := poolQualityReason(p); reason != "" {
			b.registry.Remove(p.Address)
			b.graph.RemovePool(p.Address)
			qualityRemoved++
			removed++
			log.Printf("[quality-reject] prune removed %s %s/%s (%s): %s",
				p.DEX, symOr(p.Token0), symOr(p.Token1), p.Address, reason)
			continue
		}
		if p.Pinned {
			continue // pinned pools are protected from soft TVL pruning
		}
		if p.TVLUSD < b.cfg.Pools.PruneTVLUSD || p.Volume24hUSD < b.cfg.Pools.PruneVolumeUSD {
			b.registry.Remove(p.Address)
			b.graph.RemovePool(p.Address)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[bot] pruned %d stale pools (%d via quality filter), registry size: %d",
			removed, qualityRemoved, b.registry.Len())
	}
}

// symOr returns the token's Symbol or "?" if the token is nil. Tiny helper
// used by log lines that need to be resilient against malformed pools where
// Token0 or Token1 is unexpectedly nil.
func symOr(t *Token) string {
	if t == nil {
		return "?"
	}
	return t.Symbol
}

// runPoolVerificationLoop walks every pool in the registry and runs VerifyPool
// on it, persisting the result to the DB. Runs once at startup (after a 60s
// delay so multicall has populated the volatile state) and every hour after.
//
// Each verification call costs 3-5 RPC round-trips, so a full pass over ~1500
// pools takes 5-10 minutes worst case. We rate-limit to 5 verifications per
// second to avoid hammering the RPC.
func (b *Bot) runPoolVerificationLoop(ctx context.Context) {
	// Initial delay so multicall has time to populate state for newly-loaded
	// pools. Without this, verifyV3Pool would reject every pool with
	// "liquidity is zero" because we haven't read it from chain yet.
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}

	for {
		b.runOnePoolVerificationPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Hour):
		}
	}
}

func (b *Bot) runOnePoolVerificationPass(ctx context.Context) {
	if b.db == nil {
		return
	}
	pools := b.registry.All()
	if len(pools) == 0 {
		return
	}
	t0 := time.Now()
	verified, failed, errored := 0, 0, 0
	failureReasons := make(map[string]int)

	// Rate limit: 5 pools/sec → 200ms per pool
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for _, p := range pools {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		// V4 pools have a different verification path (uses StateView).
		if p.DEX == DEXUniswapV4 {
			ok, reason := VerifyV4Pool(ctx, b.node, p.PoolID, p.TickSpacing, p.FeePPM, p.V4Hooks)
			p.Verified = ok
			p.VerifyReason = reason
			_ = b.db.SetV4PoolVerified(p.Address, ok, reason)
			if ok {
				verified++
			} else {
				failed++
				failureReasons[reason]++
			}
			continue
		}
		ok, reason := VerifyPool(ctx, b.node, p)
		p.Verified = ok
		p.VerifyReason = reason
		if err := b.db.SetPoolVerified(p.Address, ok, reason); err != nil {
			errored++
			continue
		}
		if ok {
			verified++
		} else {
			failed++
			failureReasons[reason]++
		}
	}

	log.Printf("[verify] pool verification pass done: %d verified / %d failed / %d errored in %s",
		verified, failed, errored, time.Since(t0).Round(time.Second))
	if !b.ready.VerifierDone.Load() {
		b.ready.VerifierDone.Store(true)
		log.Println("[ready] verifier — first pass complete")
	}
	// Log top 5 failure reasons so we can spot systematic issues
	if len(failureReasons) > 0 {
		type kv struct {
			reason string
			count  int
		}
		var sorted []kv
		for r, c := range failureReasons {
			sorted = append(sorted, kv{r, c})
		}
		// Simple top-5: not worth a sort.Sort call for ~20 entries
		for i := 0; i < 5 && i < len(sorted); i++ {
			max := i
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].count > sorted[max].count {
					max = j
				}
			}
			sorted[i], sorted[max] = sorted[max], sorted[i]
			log.Printf("[verify]   %4d × %s", sorted[i].count, sorted[i].reason)
		}
	}
}

// fastEvalCandidate holds the per-cycle result of the parallel scoring/simulation
// phase of fastEvalCycles. A nil return from evalOneCandidate means the candidate
// was filtered out before simulation; a non-nil value has already passed ScoreCycle,
// dynamicLPFloor, tick-freshness, the sim, and MinSimProfitBps — so it's ready
// for the serial submission phase.
type fastEvalCandidate struct {
	cc             cachedCycle
	cycle          Cycle
	lp             float64
	amountIn       *big.Int
	profitUSD      float64
	tokenPriceUSD  float64
	result         SimulationResult
	hopPricesJSON  string
	poolStatesJSON string
	amountInWei    string
	simProfitWei   string
	flashSel       FlashSelection // which flash source to use + its fee
	// useV3Mini is true when the cycle qualifies for routing to the
	// V3FlashMini contract. Set by evalOneCandidate when qualifyForV3Mini
	// passes AND the executor has a mini address configured AND the cycle
	// passed the lighter mini-LP floor. Read by trySubmitCandidate to
	// decide which executor path to dispatch to.
	useV3Mini      bool
	// useV4Mini is true when every hop is UniV4 and a V3-pool flash source is
	// available. Mutually exclusive with useV3Mini. Threaded through to
	// Submit/DryRun so the executor picks the V4Mini contract.
	useV4Mini      bool
	// useMixedV3V4 is true when the cycle has at least one V4 hop AND at
	// least one V3-family hop. Mutually exclusive with useV3Mini and
	// useV4Mini. Routes to MixedV3V4Executor (single PoolManager.unlock
	// wrapping both V4 and V3 dispatch).
	useMixedV3V4   bool
	durScore       time.Duration
	durLPFloor     time.Duration
	durOptimal     time.Duration
	tStart         time.Time
}

// fastEvalCycles is called inline by SwapListener immediately after a pool
// state update. It scores all pre-cached cycles (built by CycleCache) that
// involve the updated pool and fires a submission if any are profitable.
//
// Concurrency model:
//   - The CPU-bound phase (ScoreCycle + dynamicLPFloor + tick-freshness +
//     optimalAmountIn + SimulateCycle) runs across N workers in parallel — each
//     cycle is independent until submission, so this is embarrassingly parallel.
//   - The submission phase (cooldown check, health gate, executor.Submit, arb
//     logging) runs serially on the dispatcher goroutine so nonce ordering and
//     the "at most one submit per swap event" invariant are preserved.
//
// N is configured by strategy.fast_eval_workers (0 = runtime.NumCPU()).
// Pool state reads from workers are safe because handleSwap is single-threaded:
// the pool was just updated by this dispatcher goroutine and no concurrent swap
// can mutate it while fastEvalCycles is running.
// logSimRejectForensics emits a multi-line per-hop forensic for a sim-reject.
// Compares the bot's go-sim output (computed against current pool state at
// diagnose time) against amountOutMin to identify where the model diverges.
// ReSimError reflects whether the cycle would still revert at this exact moment
// with fresh state — if it's empty, the opportunity reopened during the
// rebuild and the failure was a transient state-drift issue; if it matches
// the original error, the failure is structural (encoding, router, paused
// pool, broken pair).
func logSimRejectForensics(path string, origErr error, f *RevertForensics) {
	resim := f.ReSimError
	if resim == "" {
		resim = "PASSES NOW (transient drift)"
	}
	log.Printf("[diagnose] path=%s orig=%v re-sim=%s", path, origErr, resim)
	for _, h := range f.Hops {
		marker := "  "
		if h.GoSimFails {
			marker = "**"
		}
		log.Printf("[diagnose] %s hop%d %s/%s in=%s gosim_out=%s amt_min=%s%s",
			marker, h.Idx, h.DEX, h.TokenIn+"->"+h.TokenOut,
			h.AmountInRaw, h.GoSimOutRaw, h.AmtOutMinRaw,
			func() string {
				if h.GoSimFails {
					return " ← BELOW MIN"
				}
				return ""
			}())
	}
}

// hopIdxRE matches the hop index in revert reasons from two distinct code paths:
//   - ArbitrageExecutor._executeHops: "hop 2: swap reverted", "hop 0: slippage"
//     (emitted from on-chain reverts with a trailing colon)
//   - buildHopsOpt in executor.go: "hop 0 simulation returned 0"
//     (emitted from local sim BEFORE eth_call, no trailing colon)
// Captured group 1 is the 0-indexed hop number.
var hopIdxRE = regexp.MustCompile(`hop (\d+)[: ]`)

// maybeAutoDisablePool attributes a sim-reject to the pool at the failing hop
// and calls DisablePool() if the pool has tripped AutoDisableRejectThreshold
// rejects within AutoDisableWindowSecs. Looked up via the registry because
// r.cycle.Edges[hop].Pool is a snapshot (frozen at evalOneCandidate time) and
// the sliding-window counter lives on the LIVE pool.
//
// Safe to call from the hot-path sim-reject handler — the atomic counter and
// DB write are both cheap (~10us and ~1ms respectively), and the DB write
// only fires once per pool in its lifetime (the threshold is a one-shot trip).
func (b *Bot) maybeAutoDisablePool(snapshotPool *Pool, hop int, simErr error) {
	if !b.cfg.Strategy.AutoDisableEnabled || snapshotPool == nil {
		return
	}
	threshold := b.cfg.Strategy.AutoDisableRejectThreshold
	if threshold <= 0 {
		return
	}
	live, ok := b.registry.Get(snapshotPool.Address)
	if !ok {
		return
	}
	// If the pool is already disabled, nothing to do — a previous sim-reject
	// tripped the threshold. The cycle cache rebuild may not have caught up
	// yet, so we can still see cycles touching it for up to ~15s.
	if live.Disabled {
		return
	}
	count := live.RecordSimReject(b.cfg.Strategy.AutoDisableWindowSecs)
	if count < threshold {
		return
	}
	// Tripped. Flip the in-memory flag first so the next cycle cache rebuild
	// and any concurrent trySubmitCandidate call observe it immediately, then
	// persist to DB so the flag survives restart. Take p.mu for the write:
	// the cycle cache reads p.Disabled without a lock (plain bool, atomic at
	// machine level), so the worst race is one extra rebuild iteration that
	// still includes the pool — the NEXT rebuild ~15s later excludes it.
	live.mu.Lock()
	live.Disabled = true
	live.mu.Unlock()
	if b.db != nil {
		if err := b.db.DisablePool(live.Address); err != nil {
			log.Printf("[auto-disable] DB write failed for %s: %v (in-memory flag still set)", live.Address, err)
		}
	}
	log.Printf("[auto-disable] %s %s/%s (hop %d, %d rejects in %ds window) err=%v",
		live.DEX, live.Token0.Symbol, live.Token1.Symbol, hop, count,
		b.cfg.Strategy.AutoDisableWindowSecs, simErr)
}

// parseFailedHop extracts the 0-indexed hop position from a sim-reject error
// surfaced by executor.Submit. Returns -1 if the error doesn't reference a
// specific hop (cooldown, health gate, broadcast failures, etc.).
func parseFailedHop(err error) int {
	if err == nil {
		return -1
	}
	m := hopIdxRE.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return -1
	}
	n, perr := strconv.Atoi(m[1])
	if perr != nil {
		return -1
	}
	return n
}

// edgePrefixKey returns a deterministic key for the first n hops of a cycle,
// encoding (pool address, swap direction). Two prefixes match iff they touch
// the same pools in the same order with the same in-token at each hop, meaning
// a hop-N revert in the first will repeat in the second.
func edgePrefixKey(edges []Edge, n int) string {
	if n > len(edges) {
		n = len(edges)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(strings.ToLower(edges[i].Pool.Address))
		sb.WriteByte(':')
		sb.WriteString(strings.ToLower(edges[i].TokenIn.Address))
		sb.WriteByte(';')
	}
	return sb.String()
}

// blockScoringLoop runs every BlockScoringInterval×250ms and re-scores cycles
// through pools whose SpotPrice changed since the last pass. This catches
// drift-based arbitrage that no swap event triggered — e.g., two pools for the
// same token pair that gradually diverge in price without either one swapping.
//
// Hot-cycle optimization: instead of sweeping all ~220 pools with ~400K cycles,
// only pools whose SpotPrice differs from the last pass are scored. Typical
// multicall updates ~50-100 pools per block, cutting per-sweep work by 50-75%.
// Combined with interval=1 (every 250ms), this gives <100ms latency from
// multicall to scoring for changed pools.
func (b *Bot) blockScoringLoop(ctx context.Context) {
	if b.cycleCache == nil || b.cfg.Strategy.BlockScoringInterval <= 0 {
		return
	}
	interval := b.cfg.Strategy.BlockScoringInterval
	blockCount := 0
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			blockCount++
			if blockCount < interval {
				continue
			}
			blockCount = 0
			if b.cfg.PauseTracking {
				continue
			}
			addrs := b.cycleCache.PoolAddrs()
			scored := 0
			for _, addr := range addrs {
				p, ok := b.registry.Get(addr)
				if !ok {
					continue
				}
				spot := p.SpotPrice
				if spot == 0 {
					continue
				}
				last, seen := b.blockScorePrice[strings.ToLower(addr)]
				if seen && spot == last {
					continue
				}
				b.blockScorePrice[strings.ToLower(addr)] = spot
				b.doFastEvalCycles(p, true)
				scored++
			}
			_ = scored
		}
	}
}

func (b *Bot) fastEvalCycles(pool *Pool) {
	b.doFastEvalCycles(pool, false)
}

// fastEvalCyclesUnthrottled is the cross-pool propagation entry point.
// Identical to fastEvalCycles but bypasses the MinPriceDelta throttle.
// Used when a SIBLING pool swapped (not this one) — this pool's SpotPrice
// hasn't changed, so the throttle would suppress evaluation, but cycles
// through this pool may now be profitable because the sibling moved.
func (b *Bot) fastEvalCyclesUnthrottled(pool *Pool) {
	b.doFastEvalCycles(pool, true)
}

func (b *Bot) doFastEvalCycles(pool *Pool, skipThrottle bool) {
	if b.cycleCache == nil {
		return
	}

	// Pause-tracking gate: when set in config.yaml, skip the entire
	// scoring + candidate pipeline. Swap events have already updated
	// pool state by the time we get here, so the graph stays warm for
	// whenever tracking is re-enabled.
	if b.cfg.PauseTracking {
		return
	}

	// Throttle: skip rescoring if this pool's spot price hasn't moved enough
	// since the last evaluation. Prevents burning CPU on high-frequency tiny
	// swaps that don't meaningfully change any cycle's profitability.
	// Bypassed for cross-pool propagation (skipThrottle=true) since the
	// trigger is a SIBLING pool's price change, not this pool's.
	poolKey := strings.ToLower(pool.Address)
	if !skipThrottle && pool.SpotPrice > 0 && b.cfg.Strategy.MinPriceDelta > 0 {
		if last, ok := b.lastPoolPrice[poolKey]; ok && last > 0 {
			if math.Abs(math.Log(pool.SpotPrice/last)) < b.cfg.Strategy.MinPriceDelta {
				return
			}
		}
		b.lastPoolPrice[poolKey] = pool.SpotPrice
	}

	candidates := b.cycleCache.CyclesForPool(pool.Address)
	if len(candidates) == 0 {
		return
	}

	// Targeted cycle-pool refresh: collect every unique pool across all
	// candidate cycles and fetch their current on-chain state in one
	// Multicall3 batch. Without this, multi-hop cycles have stale state on
	// hops that aren't the triggering pool or its same-pair peers — causing
	// the sim to see phantom spreads that don't exist on-chain.
	//
	// Cost: ~50-200 unique pools × 2-3 calls each = one Multicall3 batch
	// (~100-400ms). This runs once per fastEvalCycles invocation, not per
	// candidate. Pools that were already refreshed (by the swap event or
	// peer refresh) will just get a redundant update — harmless.
	if skipThrottle {
		seen := make(map[string]bool)
		var toRefresh []*Pool
		for _, cc := range candidates {
			for _, e := range cc.Cycle.Edges {
				addr := strings.ToLower(e.Pool.Address)
				if seen[addr] {
					continue
				}
				seen[addr] = true
				toRefresh = append(toRefresh, e.Pool)
			}
		}
		if len(toRefresh) > 0 {
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
			bn := b.currentStateBlock(rctx)
			if err := BatchUpdatePoolsAtBlock(rctx, b.node, toRefresh, bn); err == nil {
				_ = FetchV4PoolStates(rctx, b.node, toRefresh, bn)

				var needTicks []*Pool
				for _, p := range toRefresh {
					switch p.DEX {
					case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
						p.mu.RLock()
						curTick := p.Tick
						fetchTick := p.TickAtFetch
						spacing := p.TickSpacing
						p.mu.RUnlock()
						if spacing > 0 {
							delta := curTick - fetchTick
							if delta < 0 {
								delta = -delta
							}
							if delta >= spacing {
								needTicks = append(needTicks, p)
							}
						}
					}
				}
				if len(needTicks) > 0 {
					tickBN := b.currentTickBlock(rctx)
					radius := b.cfg.Trading.TickBitmapCoverageWords
					_, _ = FetchTickMaps(rctx, b.tickClient, needTicks, radius, tickBN)
				}

				for _, p := range toRefresh {
					b.graph.UpdateEdgeWeights(p)
				}
			}
			rcancel()
		}
	}

	// Snapshot config/atomics once — workers read from these copies, avoiding
	// repeated atomic loads and keeping the worker function lock-free.
	hardCapUSD := b.cfg.Trading.FlashLoanAmountUSD
	if hardCapUSD == 0 {
		hardCapUSD = 2_000_000
	}
	minProfit := b.minProfitUSD.Load().(float64)
	lpSanityCap := b.cfg.Strategy.LPSanityCap
	maxProfitCap := b.cfg.Strategy.MaxProfitUSDCap
	tvlFraction := b.cfg.Strategy.TVLTradeFraction
	hardFloor := b.cfg.Strategy.MinCycleLPBps / 10000.0 // absolute safety-net floor
	gasPriceGwei := b.lastGasPriceGwei.Load().(float64)

	// Worker count: 0 = auto-size to NumCPU, but never exceed len(candidates).
	workers := b.cfg.Strategy.FastEvalWorkers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(candidates) {
		workers = len(candidates)
	}
	if workers < 1 {
		workers = 1
	}

	// Fan candidates out across workers. Each worker runs the CPU-bound pipeline
	// (score → LP floor → tick freshness → optimalAmountIn → MinSimProfitBps) and
	// sends a *fastEvalCandidate to results for any cycle that survives.
	work := make(chan cachedCycle, len(candidates))
	results := make(chan *fastEvalCandidate, len(candidates))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cc := range work {
				if r := b.evalOneCandidate(cc, hardCapUSD, minProfit, lpSanityCap, maxProfitCap, tvlFraction, hardFloor, gasPriceGwei); r != nil {
					results <- r
				}
			}
		}()
	}
	for _, cc := range candidates {
		work <- cc
	}
	close(work)
	wg.Wait()
	close(results)

	// Collect survivors into a slice and sort by profitUSD descending. The
	// submission phase then walks them best-first — if a gate rejects the top
	// candidate (cooldown, health, sim-reject) we fall through to the next,
	// preserving the old "try alternatives until one actually submits" behavior.
	var ready []*fastEvalCandidate
	for r := range results {
		ready = append(ready, r)
	}
	if len(ready) == 0 {
		return
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].profitUSD > ready[j].profitUSD })

	// Serial submission phase — runs on the dispatcher goroutine so nonce
	// ordering and cooldown/lastSubmitByKey are race-free.
	//
	// Two layered defenses keep this loop from blocking the swap listener
	// when a volatile burst floods us with dozens of profitable variants of
	// the same arb cluster:
	//   1. failedPrefixes — once a candidate sim-rejects at hop N, every
	//      later candidate sharing its first N+1 edges is skipped without
	//      issuing another eth_call (hop K reverts are pool/state-shared).
	//   2. fast_eval_max_submits_per_swap — hard cap on attempted submits
	//      per swap event. Bounds the worst case to K × ~50ms regardless of
	//      what the dedup catches.
	maxSubmits := b.cfg.Strategy.FastEvalMaxSubmitsPerSwap
	failedPrefixes := make(map[string]bool)
	var skipped, rejects, attempts int
	for _, r := range ready {
		// Top-K cap: stop entirely once we've hit the per-swap budget.
		if maxSubmits > 0 && attempts >= maxSubmits {
			break
		}

		// Incremental prefix check: walk this candidate's edges, building the
		// running prefix key, bail as soon as any prefix matches a known failure.
		if len(failedPrefixes) > 0 {
			var sb strings.Builder
			skip := false
			for _, e := range r.cycle.Edges {
				sb.WriteString(strings.ToLower(e.Pool.Address))
				sb.WriteByte(':')
				sb.WriteString(strings.ToLower(e.TokenIn.Address))
				sb.WriteByte(';')
				if failedPrefixes[sb.String()] {
					skip = true
					break
				}
			}
			if skip {
				skipped++
				continue
			}
		}

		attempts++
		submitted, failedHop := b.trySubmitCandidate(r, minProfit, gasPriceGwei)
		if submitted {
			// Successful on-chain submission — stop, don't double-submit this swap.
			if skipped > 0 || rejects > 0 {
				log.Printf("[fast] submit dedup: %d ready, %d attempted, %d sim-reject, %d prefix-skipped", len(ready), attempts, rejects, skipped)
			}
			return
		}
		if failedHop >= 0 {
			rejects++
			failedPrefixes[edgePrefixKey(r.cycle.Edges, failedHop+1)] = true
		}
	}
	if skipped > 0 || rejects > 0 || (maxSubmits > 0 && len(ready) > attempts) {
		log.Printf("[fast] submit dedup: %d ready, %d attempted, %d sim-reject, %d prefix-skipped, %d capped", len(ready), attempts, rejects, skipped, max(0, len(ready)-attempts-skipped))
	}
}

// evalOneCandidate runs the CPU-bound scoring/simulation pipeline for a single
// cycle. Returns a ready-to-submit fastEvalCandidate if the cycle passes every
// pre-submission filter, or nil to discard it.
//
// This function is called concurrently from multiple workers. It reads pool
// state (safe — no concurrent writes during fastEvalCycles) but touches no Bot
// mutable state except b.registry and b.cfg, both read-only in this path.
func (b *Bot) evalOneCandidate(cc cachedCycle, hardCapUSD, minProfit, lpSanityCap, maxProfitCap, tvlFraction, hardFloor, gasPriceGwei float64) *fastEvalCandidate {
	tTotal := time.Now()
	b.candTotal.Add(1)

	for _, e := range cc.Cycle.Edges {
		if e.Pool == nil || !e.Pool.Verified {
			b.candRejectUnverified.Add(1)
			return nil
		}
	}

	// Step 1: ScoreCycle
	tStep := time.Now()
	lp, positive := ScoreCycle(cc)
	durScore := time.Since(tStep)
	if !positive || lp > lpSanityCap {
		b.candRejectScore.Add(1)
		return nil
	}

	// Step 2: DynamicLPFloor check.
	//
	// We compute TWO floors when the V3FlashMini path is potentially
	// available for this cycle: one for the full executor (default 1.0x
	// hop multiplier) and one for the mini executor (0.5x — see
	// dynamicLPFloorScaled's comment). The lower of the two is the effective
	// floor: cycles that wouldn't clear the full-executor floor but DO clear
	// the mini-executor floor will still proceed, and trySubmitCandidate
	// will route them to the mini contract.
	//
	// Only tries the mini floor when:
	//   - cfg.Trading.ExecutorV3Mini is configured (mini contract deployed)
	//   - cycle is structurally eligible (cycleIsV3MiniShape passes)
	// The flash source check (FlashV3Pool) happens later in the pipeline
	// and is the final gate for actually using the mini — if the cycle
	// passes the mini floor but can't get a V3 flash source, it still
	// proceeds via the full executor (which DOES accept other flash sources).
	tStep = time.Now()
	lpFloor := dynamicLPFloorScaled(cc.Cycle, gasPriceGwei, b.cfg, 1.0)
	miniShapeOK := b.cfg.Trading.ExecutorV3Mini != "" && cycleIsV3MiniShape(cc.Cycle)
	if miniShapeOK {
		miniFloor := dynamicLPFloorScaled(cc.Cycle, gasPriceGwei, b.cfg, 0.5)
		if miniFloor < lpFloor {
			lpFloor = miniFloor
		}
	}
	if hardFloor > lpFloor {
		lpFloor = hardFloor
	}
	durLPFloor := time.Since(tStep)
	if lp < lpFloor {
		b.candRejectLPFloor.Add(1)
		return nil
	}

	if len(cc.Cycle.Edges) == 0 {
		b.candRejectEmpty.Add(1)
		return nil
	}
	// ── Block-lag STATE freshness gate ──
	//
	// Every edge pool must have its StateBlock within MaxPoolStateBlockLag of
	// the bot's LatestBlock view. This is the block-accurate replacement for
	// the deprecated time-based pool_state_max_age_sec gate and covers every
	// simulator-dependent state field (slot0 / liquidity / reserves / dynamic
	// fee) because BatchUpdatePoolsAtBlock + FetchV4PoolStates + swap events
	// all stamp StateBlock atomically with the field writes. A pool with
	// StateBlock = 0 (never fetched, or initial resolution path without block
	// context) always fails this gate.
	//
	// Reasoning: at Arbitrum's ~250 ms block time a lag of 1 means the pool
	// was fetched at the current block or the immediately preceding one —
	// the tightest bound achievable given multicall round-trip time. Anything
	// slower means we're simulating against state the chain has moved past
	// and the eth_call validation will (correctly) revert.
	latest := b.health.LatestBlock.Load()
	maxLag := b.cfg.Trading.MaxPoolStateBlockLag
	if latest > 0 {
		maxLagInt := int64(maxLag)
		for _, edge := range cc.Cycle.Edges {
			edge.Pool.mu.RLock()
			sb := edge.Pool.StateBlock
			edge.Pool.mu.RUnlock()
			if sb == 0 {
				b.candRejectPoolStale.Add(1)
				return nil
			}
			diff := int64(latest) - int64(sb)
			if diff > maxLagInt || diff < -maxLagInt {
				b.candRejectPoolStale.Add(1)
				return nil
			}
		}
	}

	// ── Tick bitmap coverage gate ──
	//
	// A V3/V4/Algebra pool is only safe to simulate when its current tick
	// still lies inside the fetched bitmap word range captured by the last
	// FetchTickMaps pass. The fetch covered
	//   [centerWord - TicksWordRadius, centerWord + TicksWordRadius]
	// with centerWord computed from the tick at fetch time (TickAtFetch).
	// If the current tick drifts outside that window the cached Ticks slice
	// no longer represents the liquidity the chain will cross, and the
	// multi-tick simulator falls back to a wrong answer.
	//
	// The gate is COVERAGE-BASED, not block-lag-based: the bitmap is valid
	// for as long as the current tick is still inside the fetched word range
	// captured at the last FetchTickMaps pass. A bitmap fetched 100 blocks
	// ago is perfectly fine IF the current tick hasn't drifted outside
	// [TickAtFetch ± TicksWordRadius words]. The 5-second tick-sweep cadence
	// inherently lags state_block by ~20 blocks on Arbitrum, so gating tick
	// freshness on block-lag (like state) would reject every cycle. The
	// Mint/Burn hazard (bitmap missing newly-initialized ticks) is accepted
	// as a minor sim-inaccuracy risk — it produces missed profit, not
	// reverts — and would require subscribing to Mint/Burn events to fix.
	coverageRadius := int16(b.cfg.Trading.TickBitmapCoverageWords)
	for _, edge := range cc.Cycle.Edges {
		switch edge.Pool.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
			edge.Pool.mu.RLock()
			tick := edge.Pool.Tick
			tickAt := edge.Pool.TickAtFetch
			radius := edge.Pool.TicksWordRadius
			tblk := edge.Pool.TicksBlock
			tickCount := len(edge.Pool.Ticks)
			spacing := edge.Pool.TickSpacing
			dex := edge.Pool.DEX
			fetchOK := edge.Pool.TicksFetchOK
			edge.Pool.mu.RUnlock()
			if radius == 0 || spacing <= 0 || tblk == 0 {
				b.candRejectTickStale.Add(1)
				b.candRejectTickNeverFetched.Add(1)
				return nil
			}
			if !fetchOK {
				b.candRejectTickStale.Add(1)
				b.candRejectTickFetchFailed.Add(1)
				return nil
			}
			if tickCount == 0 {
				b.candRejectTickStale.Add(1)
				b.candRejectTickEmptyVerified.Add(1)
				return nil
			}
			var curWord, centerWord int16
			if dex == DEXCamelotV3 || dex == DEXZyberV3 {
				curWord = int16(tick >> 8)
				centerWord = int16(tickAt >> 8)
			} else {
				curWord = int16((tick / spacing) >> 8)
				centerWord = int16((tickAt / spacing) >> 8)
			}
			delta := curWord - centerWord
			if delta < 0 {
				delta = -delta
			}
			effRadius := radius
			if coverageRadius > 0 && coverageRadius < effRadius {
				effRadius = coverageRadius
			}
			if delta > effRadius {
				b.candRejectTickStale.Add(1)
				b.candRejectTickCoverageDrift.Add(1)
				return nil
			}
		}
	}

	// Freeze pool state for the rest of the candidate's evaluation. This
	// is the structural fix for the "build hops: cycle unprofitable: last hop
	// short" reject class — see Pool.Snapshot() and Cycle.WithSnapshots() for
	// the full rationale. Both optimalAmountIn (which calls SimulateCycle) and
	// the downstream executor.Submit (which calls buildHops) read pool fields
	// via this frozen Cycle, so they're guaranteed to see identical state
	// regardless of what the multicall / eager-tick / Algebra-fee loops do
	// in the background between the two simulator calls.
	cycle := cc.Cycle.WithSnapshots()
	startToken := cycle.Edges[0].TokenIn
	pool0 := cycle.Edges[0].Pool

	// Trade-size cap: tvlFraction (default 0.15 = 15%) of the SHALLOWEST pool's
	// TVL. For very small pools (sub-$200k) this loosens to a much tighter cap
	// because (a) every additional bp of price impact eats directly into the
	// profit margin and (b) tick-crossing slippage is highly non-linear in
	// shallow V3 pools — a $10k trade through a $66k pool can move ≥50 bps,
	// which is enough to flip a "+40 bps" cycle into a "-50 bps" loss between
	// fastEval and buildHops. The split threshold and shallow fraction live in
	// config so they're tunable per environment; the defaults below are applied
	// in applyStrategyDefaults.
	shallowThreshold := b.cfg.Strategy.ShallowPoolTVLUSD
	shallowFrac := b.cfg.Strategy.ShallowPoolTradeFraction
	maxAmountUSD := hardCapUSD
	for _, edge := range cycle.Edges {
		tvl := edge.Pool.TVLUSD
		if tvl <= 0 {
			continue
		}
		frac := tvlFraction
		if shallowThreshold > 0 && tvl < shallowThreshold && shallowFrac > 0 && shallowFrac < frac {
			frac = shallowFrac
		}
		if cap := tvl * frac; cap < maxAmountUSD {
			maxAmountUSD = cap
		}
	}
	if maxAmountUSD < 100 {
		maxAmountUSD = 100
	}

	// Resolve the flash source NOW (before the gas/profit gate) so we know
	// the actual route the cycle will take. V3FlashMini requires BOTH the
	// shape check (miniShapeOK) AND a V3-pool flash source. If the flash
	// source ends up being Balancer/Aave, dispatch falls back to the full
	// 900k ArbitrageExecutor — and the gate must use that 900k cost, not the
	// 380k mini cost. Previously the gate used 380k whenever miniShapeOK
	// was true, admitting cycles that profitably cleared the mini gate but
	// lost money against the full executor's higher gas cost.
	var flashSel FlashSelection
	if b.flashSelector != nil {
		flashSel = b.flashSelector.Select(startToken.Address)
	} else {
		flashSel = FlashSelection{Source: FlashBalancer, FeePPM: 0, Available: true}
	}
	if !flashSel.Available {
		b.candRejectNoFlash.Add(1)
		return nil
	}
	useV3Mini := miniShapeOK && qualifyForV3Mini(cycle, flashSel)
	useV4Mini := !useV3Mini && qualifyForV4Mini(cycle, flashSel)
	// MixedV3V4 routing: cycles with at least one V4 hop AND at least one
	// V3-family hop, 2-5 total. The single-DEX minis don't qualify; the
	// generic executor would but its V4 path produces V4_HANDLER reverts.
	// Same diagnostic rationale as V4Mini above — set the flag in eval so
	// "would have used MixedV3V4" surfaces even when the contract isn't
	// deployed.
	useMixedV3V4 := !useV3Mini && !useV4Mini && qualifyForMixedV3V4(cycle, flashSel)

	// Per-contract gas estimate. Keyed off the ACTUAL routing decision so
	// the per-cycle profit floor reflects the contract that will actually
	// execute. Numbers track the contract_ledger gas_estimate column.
	effectiveMinProfit := minProfit
	if cpu, ok := b.gasCostPerUnit.Load().(float64); ok && cpu > 0 {
		gasEst := 900_000.0
		switch {
		case useV3Mini:
			gasEst = 380_000.0
		case useV4Mini:
			gasEst = 300_000.0
		case useMixedV3V4:
			gasEst = 450_000.0
		}
		contractMin := cpu * gasEst * b.cfg.Strategy.GasSafetyMult
		floor := b.cfg.Trading.MinProfitUSD
		if contractMin < floor {
			contractMin = floor
		}
		effectiveMinProfit = contractMin
	}

	// Step 3: optimalAmountIn (ternary search + SimulateCycle)
	tStep = time.Now()
	amountIn, profitUSD, tokenPriceUSD, result := optimalAmountIn(cycle, startToken, pool0, 1, maxAmountUSD, b.registry)
	durOptimal := time.Since(tStep)
	if !result.Viable || profitUSD < effectiveMinProfit || profitUSD > maxProfitCap {
		b.candRejectUnviable.Add(1)
		return nil
	}
	// Reject cycles where the profit margin is too thin to survive latency.
	// Even if the USD profit clears the gas floor, sub-75bps margins get killed
	// by pool state movement between detection and on-chain execution.
	if b.cfg.Strategy.MinSimProfitBps > 0 && result.ProfitBps < b.cfg.Strategy.MinSimProfitBps {
		b.candRejectMinBps.Add(1)
		return nil
	}

	// Step 4: deduct flash-loan fee from profit (flash source already selected above).
	if flashSel.FeePPM > 0 && amountIn != nil && amountIn.Sign() > 0 {
		feeCost := new(big.Int).Mul(amountIn, big.NewInt(int64(flashSel.FeePPM)))
		feeCost.Div(feeCost, big.NewInt(1_000_000))
		feeCostUSD := float64(0)
		if tokenPriceUSD > 0 {
			feeF, _ := new(big.Float).SetInt(feeCost).Float64()
			feeCostUSD = feeF * startToken.Scalar * tokenPriceUSD
		}
		profitUSD -= feeCostUSD
		if result.Profit != nil {
			result.Profit = new(big.Int).Sub(result.Profit, feeCost)
		}
		// Recompute ProfitBps after fee deduction (float64, sub-bp resolution).
		if amountIn.Sign() > 0 && result.Profit != nil && result.Profit.Sign() > 0 {
			pf, _ := new(big.Float).SetInt(result.Profit).Float64()
			af, _ := new(big.Float).SetInt(amountIn).Float64()
			if af > 0 {
				result.ProfitBps = (pf / af) * 10_000
			}
		} else if result.Profit != nil && result.Profit.Sign() <= 0 {
			return nil // flash fee ate the entire profit
		}
		// Re-check minimum profit after fee deduction (per-route gate)
		if profitUSD < effectiveMinProfit {
			return nil
		}
		if b.cfg.Strategy.MinSimProfitBps > 0 && result.ProfitBps < b.cfg.Strategy.MinSimProfitBps {
			return nil
		}
	}

	// Build logging snapshots while pool state is still consistent with the
	// simulation result. Doing this in the worker (not the serial phase) means
	// subsequent swap events can't mutate the pools before we capture prices.
	hopPricesJSON := cycleHopPricesJSON(result, cycle, b.registry)
	poolStatesJSON := cyclePoolStatesJSON(cycle)
	amountInWei := ""
	if amountIn != nil {
		amountInWei = amountIn.String()
	}
	simProfitWei := ""
	if result.Profit != nil {
		simProfitWei = result.Profit.String()
	}

	b.candAccepted.Add(1)
	return &fastEvalCandidate{
		cc:             cc,
		cycle:          cycle,
		lp:             lp,
		amountIn:       amountIn,
		profitUSD:      profitUSD,
		tokenPriceUSD:  tokenPriceUSD,
		result:         result,
		hopPricesJSON:  hopPricesJSON,
		poolStatesJSON: poolStatesJSON,
		amountInWei:    amountInWei,
		simProfitWei:   simProfitWei,
		flashSel:       flashSel,
		useV3Mini:      useV3Mini,
		useV4Mini:      useV4Mini,
		useMixedV3V4:   useMixedV3V4,
		durScore:       durScore,
		durLPFloor:     durLPFloor,
		durOptimal:     durOptimal,
		tStart:         tTotal,
	}
}

// trySubmitCandidate runs the serial submission phase for a single candidate:
// cooldown, health gate, skip-sim decision, executor.Submit, and arb logging.
// Returns (true, -1) if a transaction was successfully broadcast (caller should
// stop trying other candidates to preserve the "at most one submit per swap" rule).
// Returns (false, hopIdx) on a per-hop sim-reject so the caller can dedup later
// candidates that share the same prefix; returns (false, -1) for any rejection
// not tied to a specific hop (cooldown, health, broadcast errors, etc.).
func (b *Bot) trySubmitCandidate(r *fastEvalCandidate, currentMinProfit, gasPriceGwei float64) (bool, int) {
	log.Printf("[fast] profitable cycle detected: path=%s profit=$%.4f (lp=%.6f)",
		r.result.Path, r.profitUSD, r.lp)

	// Per-contract gas cost: the actual gate (evalOneCandidate) used
	// cpu × 380k for V3FlashMini vs cpu × 900k for ArbitrageExecutor.
	// Persist that exact number so net_profit_usd reflects the real
	// breakeven, not the 600k global estimate in currentMinProfit.
	gasEstForRoute := 900_000.0
	switch {
	case r.useV3Mini:
		gasEstForRoute = 380_000.0
	case r.useV4Mini:
		gasEstForRoute = 300_000.0
	case r.useMixedV3V4:
		gasEstForRoute = 450_000.0
	}
	gasCostUSD := 0.0
	if cpu, ok := b.gasCostPerUnit.Load().(float64); ok && cpu > 0 {
		gasCostUSD = cpu * gasEstForRoute
	}

	tStep := time.Now()
	if b.executor == nil {
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", "no-executor")
		}
		return false, -1
	}
	cooldown := time.Duration(b.cfg.Strategy.SubmitCooldownSecs) * time.Second
	ck := cycleKey(r.cc.Cycle.Edges)
	if last, ok := b.lastSubmitByKey[ck]; ok && time.Since(last) < cooldown {
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", "cooldown")
		}
		return false, -1
	}

	// minProfitNative is the on-chain floor passed to the contract.
	// We use 1 wei — the USD-level check above already ensures the trade is
	// worth doing. Setting this to result.Profit/N caused "profit below minimum"
	// reverts whenever the price moved slightly between eth_call and SendTransaction,
	// wasting gas on failed txs with no upside.
	divisor := b.cfg.Strategy.ContractMinProfitDivisor
	var minProfitNative *big.Int
	if divisor > 1 && r.result.Profit != nil && r.result.Profit.Sign() > 0 {
		minProfitNative = new(big.Int).Div(r.result.Profit, big.NewInt(int64(divisor)))
	} else {
		minProfitNative = big.NewInt(1)
	}
	// Startup readiness gate: block submission until every background loop
	// has run at least once. This eliminates the post-restart window where
	// the bot would happily submit cycles based on stale or partial state.
	if ok, reason := b.readyOrReason(); !ok {
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", reason)
		}
		return false, -1
	}
	// Health gate: block submission if any critical subsystem is stale.
	if ok, reason := b.health.IsReady(b.cfg.Trading.MaxTickAgeSec, b.cfg.Trading.MaxMulticallAgeSec, b.cfg.Trading.MaxSwapAgeSec, b.cfg.Trading.TickRPCMaxSkewBlocks); !ok {
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", "health: "+reason)
		}
		return false, -1
	}

	// Pause-trading gate: block submission but still log the would-be
	// trade so the dashboard shows what the bot *would* have submitted.
	// All earlier checks have already run, so if this fires we know the
	// candidate passed cooldown, readiness, and health — the trade is
	// otherwise legitimate.
	//
	// eth_call validation: run the full buildHops + encode + eth_call pipeline
	// to compare our Go sim against on-chain execution. If the sim says
	// +$2 but eth_call reverts, we have a sim accuracy bug. The revert
	// reason is logged as the reject_reason so we can diagnose.
	// Pre-eth_call block-lag regate. Between evalOneCandidate (where the
	// state gate first fired) and here, multiple Arbitrum blocks typically
	// pass (~250 ms/block, CPU + channel hops). If any edge pool's
	// StateBlock now lags LatestBlock by more than MaxPoolStateBlockLag,
	// drop the candidate before we burn an eth_call: the cycle we scored
	// isn't the cycle we'd submit. 18/100 LATENCY_DRIFT rejects in the
	// last 15-min sample were caused by exactly this gap — re-checking
	// here converts them into cheap pre-flight skips instead of reverted
	// eth_calls. Cost: one atomic load + one RLock per edge (sub-µs total
	// for a 4-hop cycle); saves the 30-100 ms eth_call RTT on every skip.
	// Regate + all-fresh check in a single pass. Two levels:
	//   - FAIL (sb_lag > maxLag on any edge): drop candidate, skip eth_call.
	//   - STRICT ALL-FRESH (sb_lag == 0 on every edge AND current_tick ==
	//     TickAtFetch for every V3/V4 edge): no state changed since the last
	//     authoritative read; skip the DryRun / eth_call entirely.
	preDryRunOpts := []string{}
	allFresh := true
	if latest := b.health.LatestBlock.Load(); latest > 0 {
		maxLag := b.cfg.Trading.MaxPoolStateBlockLag
		maxLagInt := int64(maxLag)
		for i, edge := range r.cycle.Edges {
			edge.Pool.mu.RLock()
			sb := edge.Pool.StateBlock
			tick := edge.Pool.Tick
			tickAt := edge.Pool.TickAtFetch
			dex := edge.Pool.DEX
			edge.Pool.mu.RUnlock()
			diff := int64(latest) - int64(sb)
			if sb == 0 || diff > maxLagInt || diff < -maxLagInt {
				reason := fmt.Sprintf("pre-dryrun regate: edge h%d sb_lag=%db abs > max=%db (latency-drift preempt)",
					i, diff, maxLag)
				if b.arbLogger != nil {
					b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
						r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
						b.cfg.ArbitrumRPC, false, "", reason)
				}
				return false, -1
			}
			if sb != latest {
				allFresh = false
			}
			switch dex {
			case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
				if tick != tickAt {
					allFresh = false
				}
			}
		}
		if allFresh {
			preDryRunOpts = append(preDryRunOpts, "skip_dryrun_all_fresh")
		}
	} else {
		allFresh = false
	}

	if b.cfg.PauseTrading {
		rejectReason := "paused: trading"
		// Under the strict-all-fresh invariant we would normally skip the
		// eth_call entirely — and in live trading we do. Here in pause mode
		// we STILL run the DryRun so we can observe whether eth_call would
		// have passed, recording the opt tag in the reject_reason for later
		// measurement of its hit rate.
		if allFresh {
			rejectReason = "paused: trading [opt=skip_dryrun_all_fresh would have skipped DryRun]"
		}
		if b.executor != nil {
			dryCtx, dryCancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := b.executor.DryRun(dryCtx, r.cycle, r.amountIn, r.flashSel,
				int64(b.cfg.Strategy.SlippageBps), minProfitNative, r.useV3Mini, r.useV4Mini, r.useMixedV3V4)
			if err != nil {
				errLoose := b.executor.DryRun(dryCtx, r.cycle, r.amountIn, r.flashSel,
					9900, big.NewInt(1), r.useV3Mini, r.useV4Mini, r.useMixedV3V4)
				if errLoose != nil {
					rejectReason = b.classifyValidateRevert(r, errLoose)
					log.Printf("[validate] sim=$%.4f bps=%.2f %s path=%s",
						r.profitUSD, r.result.ProfitBps, rejectReason, r.result.Path)
				} else {
					rejectReason = fmt.Sprintf("paused: trading (eth_call REVERTED at %dbps but OK at 99%%: %v)", b.cfg.Strategy.SlippageBps, err)
					log.Printf("[validate] sim=$%.4f bps=%.2f SLIPPAGE_OVERESTIMATE (trade fillable but sim too optimistic) path=%s",
						r.profitUSD, r.result.ProfitBps, r.result.Path)
				}
			} else {
				rejectReason = "paused: trading (eth_call OK)"
				log.Printf("[validate] sim=$%.4f bps=%.2f eth_call OK path=%s",
					r.profitUSD, r.result.ProfitBps, r.result.Path)
			}
			dryCancel()
		}
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", rejectReason)
		}
		return false, -1
	}

	// Compose the optimization tag list for this submission, starting from
	// the pre-DryRun "all_fresh" hit detected above. Additional skip-sim
	// paths (profit-threshold based) can also apply and are tagged here.
	opts := append([]string{}, preDryRunOpts...)
	skipSim := allFresh
	skipReason := ""
	if allFresh {
		skipReason = fmt.Sprintf("all_fresh (all %d edges at block=%d, zero tick drift)",
			len(r.cycle.Edges), b.health.LatestBlock.Load())
	} else if b.cfg.Strategy.SkipSimAboveUSD > 0 && r.profitUSD >= b.cfg.Strategy.SkipSimAboveUSD {
		skipSim = true
		skipReason = fmt.Sprintf("profit=$%.4f >= skip_sim_above_usd=%.2f", r.profitUSD, b.cfg.Strategy.SkipSimAboveUSD)
		opts = append(opts, "skip_sim_above_usd")
	} else if b.cfg.Strategy.SkipSimAboveBps > 0 && r.result.ProfitBps >= b.cfg.Strategy.SkipSimAboveBps {
		skipSim = true
		skipReason = fmt.Sprintf("sim_bps=%.2f >= skip_sim_above_bps=%.2f", r.result.ProfitBps, b.cfg.Strategy.SkipSimAboveBps)
		opts = append(opts, "skip_sim_above_bps")
	}
	if skipSim {
		log.Printf("[fast] skipping simulation (%s) path=%s", skipReason, r.result.Path)
	}
	durGates := time.Since(tStep)

	// Step 5: executor.Submit (sub-timings captured inside)
	tStep = time.Now()
	txHash, submitTiming, err := b.executor.Submit(context.Background(), r.cycle, r.amountIn, r.flashSel, int64(b.cfg.Strategy.SlippageBps), minProfitNative, skipSim, r.useV3Mini, r.useV4Mini, r.useMixedV3V4)
	durSubmit := time.Since(tStep)
	durTotalElapsed := time.Since(r.tStart)

	// Build sub-timing string for the submit step. Broadcast is further
	// broken down into nonce/header/sign/send so we can diagnose slow
	// submits: Ohio→arb1 should show send=~11ms; anything higher means TLS
	// renegotiation, provider queueing, or nonce/header cache miss.
	subTimings := ""
	if submitTiming != nil {
		subTimings = fmt.Sprintf("vault=%dus refresh=%dus hops=%dus encode=%dus ethcall=%dus broadcast=%dus[nonce=%dus header=%dus sign=%dus send=%dus]",
			submitTiming.Vault.Microseconds(),
			submitTiming.Refresh.Microseconds(),
			submitTiming.Hops.Microseconds(),
			submitTiming.Encode.Microseconds(),
			submitTiming.EthCall.Microseconds(),
			submitTiming.Broadcast.Microseconds(),
			submitTiming.BcastNonce.Microseconds(),
			submitTiming.BcastHeader.Microseconds(),
			submitTiming.BcastSign.Microseconds(),
			submitTiming.BcastSend.Microseconds())
	}

	// Log the timing line on every submission attempt (success or sim-reject)
	log.Printf("[timing] score=%dus lpfloor=%dus optimal=%dus gates=%dus submit=%dus (%s) total=%dus",
		r.durScore.Microseconds(),
		r.durLPFloor.Microseconds(),
		r.durOptimal.Microseconds(),
		durGates.Microseconds(),
		durSubmit.Microseconds(),
		subTimings,
		durTotalElapsed.Microseconds())

	if err != nil {
		log.Printf("[fast] sim-reject path=%s err=%v", r.result.Path, err)
		// 429s here come from the simulation RPC (or arbitrum_rpc if no
		// simulation_rpc is configured). Track them so the dashboard can
		// flag throttling that's killing trade submissions.
		if is429(err) {
			simRPCName := "arbitrum_rpc"
			if b.cfg.Trading.SimulationRPC != "" {
				simRPCName = "simulation_rpc"
			}
			b.health.RecordRPC429(simRPCName)
		}
		// Optional per-hop forensic dump. Runs Diagnose (re-sim eth_call + Go
		// trace) so we can see whether the failing hop's go-sim output is
		// below amountOutMin (slippage from state drift) or whether the on-chain
		// re-sim now passes (the opportunity moved before we caught up). Costs
		// an extra eth_call per failure — gated on diagnose_sim_rejects.
		if b.cfg.Strategy.DiagnoseSimRejects && !is429(err) {
			if f := b.executor.Diagnose(context.Background(), r.cycle, r.amountIn, int64(b.cfg.Strategy.SlippageBps)); f != nil {
				logSimRejectForensics(r.result.Path, err, f)
			}
		}
		if b.arbLogger != nil {
			b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
				r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
				b.cfg.ArbitrumRPC, false, "", "sim-reject: "+err.Error())
		}
		// Auto-disable accounting: attribute this sim-reject to the specific
		// pool at the failing hop and, if it's tripped the threshold too many
		// times within the sliding window, call DisablePool() so the cycle
		// cache rebuild drops it within ~15s. Only run for errors that are
		// actually pool-attributable (parseFailedHop returned a valid hop
		// index) — not for 429s, cooldowns, or health gates.
		if hop := parseFailedHop(err); hop >= 0 && hop < len(r.cycle.Edges) && !is429(err) {
			b.maybeAutoDisablePool(r.cycle.Edges[hop].Pool, hop, err)
		}
		return false, parseFailedHop(err)
	}

	log.Printf("[fast] tx submitted: %s path=%s profit=$%.4f", txHash, r.result.Path, r.profitUSD)
	b.lastSubmitByKey[ck] = time.Now()
	if b.arbLogger != nil {
		b.arbLogger.Record(r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON,
			r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, gasCostUSD, r.poolStatesJSON,
			b.cfg.ArbitrumRPC, true, txHash, "")
	}
	submitRPC := b.cfg.Trading.SequencerRPC
	if submitRPC == "" {
		submitRPC = b.cfg.ArbitrumRPC
	}
	executorAddr := b.cfg.Trading.ExecutorContract
	if submitTiming != nil && submitTiming.TargetAddr != (common.Address{}) {
		executorAddr = submitTiming.TargetAddr.Hex()
	}
	go b.trackTrade(context.Background(), txHash, r.cc, r.lp, r.profitUSD, r.tokenPriceUSD, r.hopPricesJSON, r.amountInWei, r.simProfitWei, r.result.ProfitBps, gasPriceGwei, currentMinProfit, r.poolStatesJSON, submitRPC, strings.Join(opts, ","), executorAddr)
	return true, -1
}

// trackTrade runs as a goroutine after a tx is submitted. It:
//  1. Inserts the trade as pending in our_trades.
//  2. Waits for the receipt (up to 30s).
//  3. Fetches the full block to count competitor txs that touched our cycle's pools.
//  4. Updates the DB with status, block position, and competitor info.
func (b *Bot) trackTrade(ctx context.Context, txHash string, cc cachedCycle, lp, profitUSD, tokenPriceUSD float64, hopPricesJSON, amountInWei, simProfitWei string, simProfitBps float64, gasPriceGwei, minProfitUSD float64, poolStatesJSON, submitRPC, optimizations, executorAddr string) {
	if b.db == nil {
		// No DB — fall back to plain receipt logging
		b.executor.CheckReceipt(ctx, txHash, cc.Cycle.Path())
		return
	}

	// Build a set of our cycle's pool addresses for competitor lookup.
	poolSet := make(map[string]bool, len(cc.PoolAddrs))
	for _, addr := range cc.PoolAddrs {
		poolSet[strings.ToLower(addr)] = true
	}

	// Insert pending record immediately so even dropped txs are visible.
	_ = b.db.InsertTrade(OurTrade{
		SubmittedAt:            time.Now(),
		TxHash:                 txHash,
		Hops:                   len(cc.Cycle.Edges),
		LogProfit:              lp,
		ProfitUSDEst:           profitUSD,
		TokenPriceUSD:          tokenPriceUSD,
		HopPricesJSON:          hopPricesJSON,
		AmountInWei:            amountInWei,
		SimProfitWei:           simProfitWei,
		SimProfitBps:           simProfitBps,
		GasPriceGwei:           gasPriceGwei,
		MinProfitUSDAtDecision: minProfitUSD,
		PoolStatesJSON:         poolStatesJSON,
		Dexes:                  buildDexes(cc),
		Tokens:                 buildTokens(cc),
		Pools:                  strings.Join(cc.PoolAddrs, ","),
		Executor:               executorAddr,
		DetectRPC:              b.cfg.ArbitrumRPC,
		SubmitRPC:              submitRPC,
		Optimizations:          optimizations,
	})

	// Wait for receipt.
	hash := common.HexToHash(txHash)
	deadline := time.Now().Add(30 * time.Second)
	var receipt *types.Receipt
	for time.Now().Before(deadline) {
		r, err := b.node.TransactionReceipt(ctx, hash)
		if err == nil {
			receipt = r
			break
		}
		time.Sleep(2 * time.Second)
	}

	if receipt == nil {
		log.Printf("[trade] tx TIMEOUT (not mined in 30s) hash=%s", txHash[:12])
		_ = b.db.UpdateTradeReceipt(TradeReceipt{TxHash: txHash, Status: "dropped"})
		return
	}

	status := "reverted"
	if receipt.Status == 1 {
		status = "success"
	}
	log.Printf("[trade] tx %s hash=%s block=%d pos=%d gasUsed=%d",
		strings.ToUpper(status), txHash[:12], receipt.BlockNumber.Uint64(), receipt.TransactionIndex, receipt.GasUsed)

	// Fetch the full block to analyse competitors.
	block, err := b.node.BlockByNumber(ctx, receipt.BlockNumber)
	compBefore := 0
	var compHashes []string
	totalTxs := 0
	if err == nil {
		txs := block.Transactions()
		totalTxs = len(txs)
		ourIdx := receipt.TransactionIndex
		for i, tx := range txs {
			if uint(i) >= ourIdx {
				break
			}
			if tx.To() == nil {
				continue
			}
			if poolSet[strings.ToLower(tx.To().Hex())] {
				compBefore++
				compHashes = append(compHashes, tx.Hash().Hex())
			}
		}
	}

	// On revert: run Diagnose to get the exact hop-level failure reason.
	var revertReason, hopForensicsJSON string
	if status == "reverted" {
		amountIn := new(big.Int)
		if _, ok := amountIn.SetString(amountInWei, 10); ok && amountIn.Sign() > 0 {
			if f := b.executor.Diagnose(ctx, cc.Cycle, amountIn, int64(b.cfg.Strategy.SlippageBps)); f != nil {
				reSimMsg := "re-sim: PASSES at current state (opportunity reopened)"
				if f.ReSimError != "" {
					reSimMsg = "re-sim: " + f.ReSimError
				}
				log.Printf("[revert-forensics] tx=%s %s", txHash[:12], reSimMsg)
				for _, h := range f.Hops {
					marker := ""
					if h.GoSimFails {
						marker = "  ← GO-SIM FAILS HERE"
					}
					log.Printf("[revert-forensics]   hop%d/%d %-12s %-10s %s→%s  in=%-18s  goSim=%-18s  min=%-18s  fee=%dbps%s",
						h.Idx, len(f.Hops)-1,
						h.DEX, h.Pool[:10],
						h.TokenIn, h.TokenOut,
						formatAmount(h.AmountInRaw, h.TokenInDec, h.TokenIn),
						formatAmount(h.GoSimOutRaw, h.TokenOutDec, h.TokenOut),
						formatAmount(h.AmtOutMinRaw, h.TokenOutDec, h.TokenOut),
						h.FeeBps,
						marker,
					)
				}
				revertReason = f.ReSimError
				if fJSON, err := json.Marshal(f); err == nil {
					hopForensicsJSON = string(fJSON)
				}
			}
		}
	}

	_ = b.db.UpdateTradeReceipt(TradeReceipt{
		TxHash:            txHash,
		Status:            status,
		BlockNumber:       receipt.BlockNumber.Uint64(),
		BlockPosition:     receipt.TransactionIndex,
		BlockTotalTxs:     totalTxs,
		GasUsed:           receipt.GasUsed,
		CompetitorsBefore: compBefore,
		CompetitorHashes:  strings.Join(compHashes, ","),
		RevertReason:      revertReason,
		HopForensicsJSON:  hopForensicsJSON,
	})
}

// formatAmount converts a raw big-integer amount (as decimal string) to a
// human-readable value adjusted for token decimals, e.g. "4154.7106 ARB".
func formatAmount(rawStr string, decimals int, symbol string) string {
	n := new(big.Int)
	if _, ok := n.SetString(rawStr, 10); !ok || n.Sign() == 0 {
		return "0 " + symbol
	}
	f := new(big.Float).SetPrec(128).SetInt(n)
	denom := new(big.Float).SetPrec(128).SetInt(
		new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil),
	)
	human, _ := new(big.Float).Quo(f, denom).Float64()
	return fmt.Sprintf("%.4f %s", human, symbol)
}

// poolState is the snapshot of a pool's price state at a point in time.
type poolState struct {
	Address      string  `json:"address"`
	DEX          string  `json:"dex"`
	SqrtPriceX96 string  `json:"sqrt_price_x96,omitempty"`
	Liquidity    string  `json:"liquidity,omitempty"`
	Reserve0     string  `json:"reserve0,omitempty"`
	Reserve1     string  `json:"reserve1,omitempty"`
	FeeBps       uint32  `json:"fee_bps"`
	SpotRate     float64 `json:"spot_rate"`
	LastUpdated  int64   `json:"last_updated,omitempty"`
	TickCount    int     `json:"tick_count"`
	TickSpacing  int32   `json:"tick_spacing"`
	Tick         int32   `json:"tick"`
}

// cyclePoolStatesJSON snapshots the current price state of every pool in the cycle.
// This allows re-running SimulateCycle with identical inputs at any future time.
func cyclePoolStatesJSON(cycle Cycle) string {
	seen := make(map[string]bool)
	var states []poolState
	for _, e := range cycle.Edges {
		addr := strings.ToLower(e.Pool.Address)
		if seen[addr] {
			continue
		}
		seen[addr] = true
		e.Pool.mu.RLock()
		s := poolState{
			Address:     addr,
			DEX:         e.Pool.DEX.String(),
			FeeBps:      e.Pool.FeeBps,
			SpotRate:    e.Pool.spotRateLocked(),
			TickCount:   len(e.Pool.Ticks),
			TickSpacing: e.Pool.TickSpacing,
			Tick:        e.Pool.Tick,
		}
		if !e.Pool.LastUpdated.IsZero() {
			s.LastUpdated = e.Pool.LastUpdated.Unix()
		}
		if e.Pool.SqrtPriceX96 != nil {
			s.SqrtPriceX96 = e.Pool.SqrtPriceX96.String()
		}
		if e.Pool.Liquidity != nil {
			s.Liquidity = e.Pool.Liquidity.String()
		}
		if e.Pool.Reserve0 != nil {
			s.Reserve0 = e.Pool.Reserve0.String()
		}
		if e.Pool.Reserve1 != nil {
			s.Reserve1 = e.Pool.Reserve1.String()
		}
		e.Pool.mu.RUnlock()
		states = append(states, s)
	}
	b, err := json.Marshal(states)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// hopPricesFromSim propagates stablecoin seed prices ($1) through simulated hop
// exchange rates — identical method to arbscan's buildHopPrices.
// Falls back to usdPriceOf for tokens not reachable via stablecoin path.
func hopPricesFromSim(result SimulationResult, cycle Cycle, reg *PoolRegistry) map[string]float64 {
	stableAddrs := map[string]bool{
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true, // USDC.e
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true, // USDC
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true, // USDT
		"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true, // DAI
		"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true, // FRAX
	}
	prices := make(map[string]float64)
	for addr := range stableAddrs {
		prices[addr] = 1.0
	}
	for changed := true; changed; {
		changed = false
		for _, h := range result.HopAmounts {
			if h.AmountIn <= 0 || h.AmountOut <= 0 {
				continue
			}
			if pIn, ok := prices[h.TokenIn]; ok {
				if _, known := prices[h.TokenOut]; !known {
					prices[h.TokenOut] = pIn * h.AmountIn / h.AmountOut
					changed = true
				}
			} else if pOut, ok := prices[h.TokenOut]; ok {
				if _, known := prices[h.TokenIn]; !known {
					prices[h.TokenIn] = pOut * h.AmountOut / h.AmountIn
					changed = true
				}
			}
		}
	}
	// Fill in any remaining tokens via registry fallback.
	for _, e := range cycle.Edges {
		for _, tok := range []*Token{e.TokenIn, e.TokenOut} {
			addr := strings.ToLower(tok.Address)
			if _, ok := prices[addr]; !ok {
				if p := usdPriceOf(tok, e.Pool, reg); p > 0 {
					prices[addr] = p
				}
			}
		}
	}
	return prices
}

// cycleHopPricesJSON returns a JSON object mapping each token address in the cycle
// to its USD price, derived from simulated hop amounts (arbscan-compatible method).
func cycleHopPricesJSON(result SimulationResult, cycle Cycle, reg *PoolRegistry) string {
	prices := hopPricesFromSim(result, cycle, reg)
	b, err := json.Marshal(prices)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// effectiveV2Reserves returns (reserveIn, reserveOut) suitable for
// constant-product arb optimization math. For V2 pools these are the real
// reserves. For V3 pools (UniV3, Pancake, Sushi, Camelot, Ramses, V4) these
// are "virtual reserves" computed from sqrtPriceX96 and liquidity — valid
// only within the current tick range, but for most small arb trades the
// optimal size stays inside a single tick so the math is exact.
//
// Virtual reserve formulas for V3 (at current price P = sqrtPriceX96² / 2¹⁹²):
//   x (token0) = L / sqrt(P) = L · 2⁹⁶ / sqrtPriceX96
//   y (token1) = L · sqrt(P) = L · sqrtPriceX96 / 2⁹⁶
//
// Returns (nil, nil) for unsupported pool types (Curve, Balancer, stable
// pairs) — callers fall back to ternary search.
func effectiveV2Reserves(pool *Pool, tokenIn *Token) (rIn, rOut *big.Int) {
	if pool == nil || tokenIn == nil || pool.Token0 == nil || pool.Token1 == nil {
		return nil, nil
	}
	inIsT0 := strings.EqualFold(tokenIn.Address, pool.Token0.Address)
	switch pool.DEX {
	case DEXUniswapV2, DEXSushiSwap, DEXCamelot, DEXRamsesV2,
		DEXTraderJoe, DEXDeltaSwap, DEXSwapr, DEXArbSwap, DEXChronos:
		if pool.IsStable {
			// Solidly stable swap doesn't fit constant-product math.
			return nil, nil
		}
		if pool.Reserve0 == nil || pool.Reserve1 == nil ||
			pool.Reserve0.Sign() == 0 || pool.Reserve1.Sign() == 0 {
			return nil, nil
		}
		if inIsT0 {
			return pool.Reserve0, pool.Reserve1
		}
		return pool.Reserve1, pool.Reserve0
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3,
		DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
		if pool.SqrtPriceX96 == nil || pool.SqrtPriceX96.Sign() == 0 ||
			pool.Liquidity == nil || pool.Liquidity.Sign() == 0 {
			return nil, nil
		}
		// x (token0) = L * 2^96 / sqrtPriceX96
		x := new(big.Int).Mul(pool.Liquidity, Q96)
		x.Div(x, pool.SqrtPriceX96)
		// y (token1) = L * sqrtPriceX96 / 2^96
		y := new(big.Int).Mul(pool.Liquidity, pool.SqrtPriceX96)
		y.Div(y, Q96)
		if x.Sign() == 0 || y.Sign() == 0 {
			return nil, nil
		}
		if inIsT0 {
			return x, y
		}
		return y, x
	default:
		return nil, nil
	}
}

// poolFeeFraction returns the pool's swap fee as a float (e.g. 0.003 for 30 bps).
func poolFeeFraction(p *Pool) float64 {
	if p.FeePPM > 0 {
		return float64(p.FeePPM) / 1_000_000
	}
	return float64(p.FeeBps) / 10_000
}

// solve2HopOptimal returns the closed-form optimal amountIn for a 2-hop arb
// cycle using the Flash Boys 2.0 Appendix A formula for constant-product pools.
//
// For cycle A → pool1 → B → pool2 → A with effective reserves (a1,b1) in pool1,
// (a2,b2) in pool2 (a-side = token being borrowed), and fee fractions f1, f2:
//
//	Δa* = (sqrt(γ1·γ2·a1·a2·b1·b2) - a1·b2) / (γ1·(b2 + γ2·b1))
//
// where γ_i = 1 - f_i.  Returns nil if:
//   - Not a 2-hop cycle
//   - Either pool is unsupported (Curve, Balancer, Solidly stable)
//   - Cycle is unprofitable (γ1·γ2·a2·b1 ≤ a1·b2, i.e. fees exceed spread)
//   - Computed Δa is non-positive or NaN
//
// Converts final result to raw big.Int using float64 math. Precision loss is
// acceptable here because the caller runs SimulateCycle once at this amount
// for verification — the closed-form is a "smart initial guess", not the
// final answer. For 2-hop cycles this replaces 30+ SimulateCycle calls with
// exactly 1, cutting optimal-phase time from ~84ms to ~1ms.
func solve2HopOptimal(cycle Cycle) *big.Int {
	if len(cycle.Edges) != 2 {
		return nil
	}
	e1, e2 := cycle.Edges[0], cycle.Edges[1]

	// Pool 1: A in, B out → (a1, b1) = (reserveIn, reserveOut)
	a1Bi, b1Bi := effectiveV2Reserves(e1.Pool, e1.TokenIn)
	// Pool 2: B in, A out → (b2, a2) = (reserveIn, reserveOut)
	b2Bi, a2Bi := effectiveV2Reserves(e2.Pool, e2.TokenIn)
	if a1Bi == nil || b1Bi == nil || a2Bi == nil || b2Bi == nil {
		return nil
	}

	// Convert to float64 for the optimization math. Precision loss is
	// acceptable — caller runs SimulateCycle for exact verification.
	a1, _ := new(big.Float).SetInt(a1Bi).Float64()
	b1, _ := new(big.Float).SetInt(b1Bi).Float64()
	a2, _ := new(big.Float).SetInt(a2Bi).Float64()
	b2, _ := new(big.Float).SetInt(b2Bi).Float64()

	if a1 <= 0 || b1 <= 0 || a2 <= 0 || b2 <= 0 {
		return nil
	}

	f1 := poolFeeFraction(e1.Pool)
	f2 := poolFeeFraction(e2.Pool)
	g1 := 1.0 - f1
	g2 := 1.0 - f2
	if g1 <= 0 || g2 <= 0 {
		return nil
	}

	// Arbitrage profitability condition: γ1·γ2·a2·b1 > a1·b2
	if g1*g2*a2*b1 <= a1*b2 {
		return nil // not a positive-EV cycle at current state
	}

	// Δa* = (sqrt(γ1·γ2·a1·a2·b1·b2) - a1·b2) / (γ1·(b2 + γ2·b1))
	product := g1 * g2 * a1 * a2 * b1 * b2
	if product <= 0 || math.IsInf(product, 0) || math.IsNaN(product) {
		return nil
	}
	numerator := math.Sqrt(product) - a1*b2
	denominator := g1 * (b2 + g2*b1)
	if numerator <= 0 || denominator <= 0 {
		return nil
	}
	deltaA := numerator / denominator
	if deltaA <= 0 || math.IsInf(deltaA, 0) || math.IsNaN(deltaA) {
		return nil
	}

	result, _ := new(big.Float).SetFloat64(deltaA).Int(nil)
	if result == nil || result.Sign() <= 0 {
		return nil
	}
	return result
}

// optimalAmountIn uses ternary search to find the trade size that maximises profit for a cycle.
// Returns (amountIn, profitUSD, tokenPriceUSD, result).
//
// CONSISTENCY INVARIANT: the registryPrice captured at the top is used for
// BOTH sizing (tokenAmountFromUSD → how many raw tokens = $N) AND profit
// valuation (native profit → USD). Without this, the ternary search
// compares p1 (USD profit at m1) against p2 (USD profit at m2) using two
// potentially different conversion factors, and the returned bestIn is
// computed with yet another fresh usdPriceOf call that can take a
// different fallback branch in the live registry and produce an amountIn
// 100x different from the one the simulator ran with. The observed symptom
// was arb_observations rows where sim_profit_wei / amount_in_wei didn't
// match sim_profit_bps by orders of magnitude.
//
// tokenAmountFromUSDWithPrice (below) is used inside this function instead
// of tokenAmountFromUSD to enforce the invariant.
func optimalAmountIn(cycle Cycle, token *Token, pool *Pool, minUSD, maxUSD float64, reg *PoolRegistry) (*big.Int, float64, float64, SimulationResult) {
	registryPrice := usdPriceOf(token, pool, reg)
	if registryPrice <= 0 {
		registryPrice = 1
	}

	profitForUSD := func(usd float64) (float64, SimulationResult) {
		in := tokenAmountFromUSDWithPrice(usd, token, registryPrice)
		r := SimulateCycle(cycle, in)
		if !r.Viable {
			return 0, r
		}
		pNative, _ := new(big.Float).Mul(new(big.Float).SetInt(r.Profit), new(big.Float).SetFloat64(token.Scalar)).Float64()
		return pNative * registryPrice, r
	}
	profitForAmount := func(in *big.Int) (float64, SimulationResult) {
		r := SimulateCycle(cycle, in)
		if !r.Viable {
			return 0, r
		}
		pNative, _ := new(big.Float).Mul(new(big.Float).SetInt(r.Profit), new(big.Float).SetFloat64(token.Scalar)).Float64()
		return pNative * registryPrice, r
	}

	// ── FAST PATH: 2-hop closed-form ─────────────────────────────────────
	// For 2-hop cycles (70% of real arb opportunities), compute the optimal
	// amountIn analytically from the pool reserves instead of ternary-
	// searching it. Runs SimulateCycle EXACTLY ONCE for verification.
	// Saves ~30-60 simulator calls per candidate compared to the ternary
	// path, cutting optimal-phase time from ~80ms to ~1ms.
	if len(cycle.Edges) == 2 {
		if closedAmt := solve2HopOptimal(cycle); closedAmt != nil {
			// Cap at maxUSD to respect the caller's trade-size ceiling
			// (shallow-pool cap, flash loan limit, etc.)
			maxAmt := tokenAmountFromUSDWithPrice(maxUSD, token, registryPrice)
			if maxAmt != nil && maxAmt.Sign() > 0 && closedAmt.Cmp(maxAmt) > 0 {
				closedAmt = new(big.Int).Set(maxAmt)
			}
			// Also clamp at minUSD
			minAmt := tokenAmountFromUSDWithPrice(minUSD, token, registryPrice)
			if minAmt != nil && minAmt.Sign() > 0 && closedAmt.Cmp(minAmt) < 0 {
				closedAmt = new(big.Int).Set(minAmt)
			}
			_, bestResult := profitForAmount(closedAmt)
			if bestResult.Viable && bestResult.Profit != nil && bestResult.Profit.Sign() > 0 {
				// Closed-form succeeded. Compute final profitUSD/tokenPriceUSD.
				bestIn := new(big.Int).Set(bestResult.AmountIn)
				simPrices := hopPricesFromSim(bestResult, cycle, reg)
				tokenPriceUSD := simPrices[strings.ToLower(token.Address)]
				if tokenPriceUSD <= 0 {
					tokenPriceUSD = registryPrice
				}
				var pNative float64
				if bestResult.Profit != nil {
					pNative, _ = new(big.Float).Mul(new(big.Float).SetInt(bestResult.Profit), new(big.Float).SetFloat64(token.Scalar)).Float64()
				}
				profitUSD := pNative * tokenPriceUSD
				return bestIn, profitUSD, tokenPriceUSD, bestResult
			}
			// Closed-form gave a bad result (likely due to V3 tick crossing
			// invalidating the constant-product approximation). Fall through
			// to ternary search.
		}
	}

	// ── SLOW PATH: ternary search ───────────────────────────────────────
	// Controlled by strategy.optimal_amount_ternary_iterations + _log_scale +
	// _refinement. Log-scale is the default because the profit peak in cyclic
	// arb sits at the shallowest pool's knee, typically $100-$1k, while the
	// bracket runs up to $336k (15% of shallowest TVL). Linear-scale search
	// wastes most iterations on the large-notional tail where profit is
	// already dropping steeply due to slippage.
	iters := optimalAmountTernaryIters
	if iters <= 0 {
		iters = 25
	}
	useLog := optimalAmountLogScale
	refinementIters := optimalAmountRefinement

	var bestUSD float64
	if useLog && minUSD > 0 && maxUSD > minUSD {
		loL, hiL := math.Log(minUSD), math.Log(maxUSD)
		for i := 0; i < iters; i++ {
			m1L := loL + (hiL-loL)/3
			m2L := hiL - (hiL-loL)/3
			p1, _ := profitForUSD(math.Exp(m1L))
			p2, _ := profitForUSD(math.Exp(m2L))
			if p1 < p2 {
				loL = m1L
			} else {
				hiL = m2L
			}
		}
		bestUSD = math.Exp((loL + hiL) / 2)
	} else {
		lo, hi := minUSD, maxUSD
		for i := 0; i < iters; i++ {
			m1 := lo + (hi-lo)/3
			m2 := hi - (hi-lo)/3
			p1, _ := profitForUSD(m1)
			p2, _ := profitForUSD(m2)
			if p1 < p2 {
				lo = m1
			} else {
				hi = m2
			}
		}
		bestUSD = (lo + hi) / 2
	}

	// Local refinement: narrow linear search in a 10% window around bestUSD,
	// same iteration count but now on a tight range. Catches cases where the
	// log-scale pass landed near the peak but not on it (common for very
	// narrow peaks like #15022-style thin-margin arbs at $164).
	if refinementIters > 0 {
		w := bestUSD * 0.10
		rlo, rhi := bestUSD-w, bestUSD+w
		if rlo < minUSD {
			rlo = minUSD
		}
		if rhi > maxUSD {
			rhi = maxUSD
		}
		for i := 0; i < refinementIters; i++ {
			m1 := rlo + (rhi-rlo)/3
			m2 := rhi - (rhi-rlo)/3
			p1, _ := profitForUSD(m1)
			p2, _ := profitForUSD(m2)
			if p1 < p2 {
				rlo = m1
			} else {
				rhi = m2
			}
		}
		bestUSD = (rlo + rhi) / 2
	}

	_, bestResult := profitForUSD(bestUSD)
	// CRITICAL: use the amountIn that was actually run through the simulator,
	// not a second call to tokenAmountFromUSD(bestUSD, ...). The second call
	// reads usdPriceOf(token, pool, reg), which consults the LIVE registry
	// rather than the snapshot — if the registry moved between the two calls
	// (another swap event fires on the pool that usdPriceOf uses for pricing,
	// or usdPriceOf takes a different fallback branch), the recomputed amount
	// will differ from the one the simulator used. The observed symptom:
	// arb_observations rows where sim_profit_wei / amount_in_wei doesn't match
	// sim_profit_bps, sometimes by 100x because usdPriceOf took a stablecoin
	// 1-hop branch on one call and a 2-hop-via-ETH branch on the other.
	// SimulationResult.AmountIn captures the exact input the simulator used,
	// so we just lift it directly — guaranteed consistent with result.Profit
	// and result.ProfitBps.
	var bestIn *big.Int
	if bestResult.AmountIn != nil {
		bestIn = new(big.Int).Set(bestResult.AmountIn)
	} else {
		bestIn = tokenAmountFromUSD(bestUSD, token, pool, reg)
	}

	// Final profitUSD and tokenPriceUSD use sim-based prices (arbscan-compatible).
	simPrices := hopPricesFromSim(bestResult, cycle, reg)
	tokenPriceUSD := simPrices[strings.ToLower(token.Address)]
	if tokenPriceUSD <= 0 {
		tokenPriceUSD = registryPrice
	}
	var pNative float64
	if bestResult.Profit != nil {
		pNative, _ = new(big.Float).Mul(new(big.Float).SetInt(bestResult.Profit), new(big.Float).SetFloat64(token.Scalar)).Float64()
	}
	profitUSD := pNative * tokenPriceUSD

	return bestIn, profitUSD, tokenPriceUSD, bestResult
}


// tokenAmountFromUSD returns an on-chain amount for ~amountUSD worth of token,
// using the first pool's spot price as an approximation. Looks up the USD
// price via usdPriceOf on every call — callers that need to hold the price
// constant across a series of invocations must use tokenAmountFromUSDWithPrice
// and capture the price themselves.
func tokenAmountFromUSD(amountUSD float64, token *Token, pool *Pool, reg *PoolRegistry) *big.Int {
	return tokenAmountFromUSDWithPrice(amountUSD, token, usdPriceOf(token, pool, reg))
}

// tokenAmountFromUSDWithPrice converts a USD amount to an on-chain raw token
// amount using a caller-supplied USD price. This exists so optimalAmountIn
// can capture the price ONCE at entry and use it for all subsequent calls
// inside the ternary search and the final bestIn computation, guaranteeing
// consistency with bestResult.Profit regardless of what happens in the live
// registry during evaluation. See the comment on optimalAmountIn for the
// bug this was originally a fix for.
func tokenAmountFromUSDWithPrice(amountUSD float64, token *Token, priceUSD float64) *big.Int {
	if priceUSD <= 0 {
		priceUSD = 1
	}
	// native amount = (amountUSD / priceUSD) / scalar
	nativeFloat := amountUSD / priceUSD / token.Scalar
	if nativeFloat <= 0 || nativeFloat > 1e36 {
		// fallback: 100k in token's base units
		result := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(token.Decimals)+5), nil)
		return result
	}
	f := new(big.Float).SetFloat64(nativeFloat)
	result, _ := f.Int(nil)
	return result
}

// isUnknownToken returns true when a token has no resolved metadata in the
// registry. The bot uses default decimals=18 and symbol="unknown" as fallbacks
// when token metadata can't be fetched. Computing TVL using these defaults is
// dangerous: the wrong decimals scaling produces wildly inflated USD values
// (often 10^12 too large). For these tokens we refuse to compute TVL.
func isUnknownToken(t *Token) bool {
	if t == nil {
		return true
	}
	sym := strings.ToLower(strings.TrimSpace(t.Symbol))
	return sym == "" || sym == "unknown"
}

// recomputeV2TVL calculates a pool's USD TVL from on-chain reserves and current
// token USD prices. Returns 0 if the pool has no reserves, the tokens are
// unknown (decimals fallback can't be trusted), or no price is available.
//
// Formula: TVL = (reserve0 / 10^dec0) * priceUSD0 + (reserve1 / 10^dec1) * priceUSD1
// If only one side has a known price, doubles it (V2 invariant: equal value both sides).
//
// The result is clamped to SanityMaxTVLUSD as a final guard against ghost pools
// with absurd reserve values (e.g. scam tokens seeded with 10^27 wei).
func recomputeV2TVL(p *Pool, reg *PoolRegistry) float64 {
	p.mu.RLock()
	r0, r1 := p.Reserve0, p.Reserve1
	t0, t1 := p.Token0, p.Token1
	p.mu.RUnlock()
	if r0 == nil || r1 == nil || r0.Sign() <= 0 || r1.Sign() <= 0 || t0 == nil || t1 == nil {
		return 0
	}
	if isUnknownToken(t0) || isUnknownToken(t1) {
		return 0
	}
	r0f, _ := new(big.Float).SetInt(r0).Float64()
	r1f, _ := new(big.Float).SetInt(r1).Float64()
	amt0 := r0f / math.Pow(10, float64(t0.Decimals))
	amt1 := r1f / math.Pow(10, float64(t1.Decimals))

	price0 := usdPriceOf(t0, p, reg)
	price1 := usdPriceOf(t1, p, reg)

	var tvl float64
	switch {
	case price0 > 0 && price1 > 0:
		tvl = amt0*price0 + amt1*price1
	case price0 > 0:
		tvl = amt0 * price0 * 2 // V2 invariant: both sides equal value
	case price1 > 0:
		tvl = amt1 * price1 * 2
	default:
		return 0
	}
	if tvl > SanityMaxTVLUSD {
		return 0
	}
	return tvl
}

// recomputeV3TVL calculates the in-range USD value held by a V3/V4 concentrated-liquidity
// pool from its current `liquidity` and `sqrtPriceX96`. The result is the value of the
// active liquidity at the current tick — not the total value across all positions.
// This is conservative (≤ true TVL) but stable, monotonic, and never depends on subgraphs.
//
// V3/V4 active-liquidity formulas at the current tick:
//
//	amount0 = L / sqrtP            (in token0 raw units, scaled by Q96)
//	amount1 = L * sqrtP            (in token1 raw units, scaled by Q96)
//
// where sqrtP = sqrtPriceX96 / 2^96. Combining the two and converting to human units:
//
//	humanAmount0 = (L * 2^96 / sqrtPriceX96) / 10^dec0
//	humanAmount1 = (L * sqrtPriceX96 / 2^96) / 10^dec1
//
// Returns 0 if state is missing or no token has a known USD price.
func recomputeV3TVL(p *Pool, reg *PoolRegistry) float64 {
	p.mu.RLock()
	sqrtP := p.SqrtPriceX96
	liq := p.Liquidity
	t0, t1 := p.Token0, p.Token1
	p.mu.RUnlock()
	if sqrtP == nil || liq == nil || sqrtP.Sign() <= 0 || liq.Sign() <= 0 || t0 == nil || t1 == nil {
		return 0
	}
	// Refuse to compute when either token is unknown — the default decimals=18
	// fallback would scale the math by ~10^12 wrong, producing $sextillion TVLs
	// for ghost pools backed by scam tokens (vanity-address spam).
	if isUnknownToken(t0) || isUnknownToken(t1) {
		return 0
	}

	// Q96 = 2^96
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	liqF := new(big.Float).SetInt(liq)
	sqrtF := new(big.Float).SetInt(sqrtP)
	q96F := new(big.Float).SetInt(q96)

	// raw amount0 = L * Q96 / sqrtP
	a0 := new(big.Float).Mul(liqF, q96F)
	a0.Quo(a0, sqrtF)
	// raw amount1 = L * sqrtP / Q96
	a1 := new(big.Float).Mul(liqF, sqrtF)
	a1.Quo(a1, q96F)

	a0f, _ := a0.Float64()
	a1f, _ := a1.Float64()
	amt0 := a0f / math.Pow(10, float64(t0.Decimals))
	amt1 := a1f / math.Pow(10, float64(t1.Decimals))

	price0 := usdPriceOf(t0, p, reg)
	price1 := usdPriceOf(t1, p, reg)

	var tvl float64
	switch {
	case price0 > 0 && price1 > 0:
		tvl = amt0*price0 + amt1*price1
	case price0 > 0:
		tvl = amt0 * price0 * 2
	case price1 > 0:
		tvl = amt1 * price1 * 2
	default:
		return 0
	}
	if tvl > SanityMaxTVLUSD {
		return 0
	}
	return tvl
}

// usdPriceOf returns the USD price of a token using live on-chain pool data.
//
// Priority:
//  1. Stablecoins → $1
//  2. Registry search: find any pool pairing token vs stablecoin (1-hop, best TVL)
//  3. Current pool: if the other token is a stablecoin or ETH-like, derive directly
//  4. Registry search: find token→ETH pool, then ETH→stablecoin pool (2-hop)
//  5. ETH-like tokens → live WETH price from registry
//  6. Zero (unknown — no hardcoded fallback to avoid phantom profits)
func usdPriceOf(token *Token, pool *Pool, reg *PoolRegistry) float64 {
	isStable := func(sym string) bool {
		switch sym {
		case "USDC", "USDC.e", "USDT", "DAI", "FRAX":
			return true
		}
		return false
	}
	isEth := func(sym string) bool {
		switch sym {
		case "WETH", "frxETH", "rETH", "wstETH", "cbETH":
			return true
		}
		return false
	}

	// 1. Stablecoins
	if isStable(token.Symbol) {
		return 1.0
	}

	// Helper: extract USD price from a pool given we know the other token's USD price.
	spotUSD := func(p *Pool, tok *Token, otherPriceUSD float64) float64 {
		s := p.SpotRate()
		if s <= 0 {
			return 0
		}
		if strings.EqualFold(tok.Address, p.Token0.Address) {
			return s * otherPriceUSD
		}
		return otherPriceUSD / s
	}

	// wethPriceFromRegistry finds the live WETH/stablecoin price using the best-TVL pool.
	// Returns 0 if the registry has no WETH/stablecoin pools.
	wethPriceFromRegistry := func() float64 {
		if reg == nil {
			return 0
		}
		const wethAddr = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
		var best float64
		var bestTVL float64
		for _, p := range reg.PoolsForToken(wethAddr) {
			other := p.Token1
			if strings.EqualFold(wethAddr, p.Token1.Address) {
				other = p.Token0
			}
			if !isStable(other.Symbol) {
				continue
			}
			wethTok := p.Token0
			if strings.EqualFold(wethAddr, p.Token1.Address) {
				wethTok = p.Token1
			}
			if pr := spotUSD(p, wethTok, 1.0); pr > 0 && p.TVLUSD > bestTVL {
				best = pr
				bestTVL = p.TVLUSD
			}
		}
		return best
	}

	if reg != nil {
		// 2. Registry 1-hop: token↔stablecoin, best TVL.
		var best1Hop float64
		var best1TVL float64
		for _, p := range reg.PoolsForToken(token.Address) {
			other := p.Token1
			if strings.EqualFold(token.Address, p.Token1.Address) {
				other = p.Token0
			}
			if !isStable(other.Symbol) {
				continue
			}
			if price := spotUSD(p, token, 1.0); price > 0 && p.TVLUSD > best1TVL {
				best1Hop = price
				best1TVL = p.TVLUSD
			}
		}
		if best1Hop > 0 {
			return best1Hop
		}
	}

	// 3. Current pool fast path — derive from the pool we're already looking at.
	if pool != nil {
		other := pool.Token1
		if strings.EqualFold(token.Address, pool.Token1.Address) {
			other = pool.Token0
		}
		if isStable(other.Symbol) {
			if p := spotUSD(pool, token, 1.0); p > 0 {
				return p
			}
		}
		if isEth(other.Symbol) {
			if weth := wethPriceFromRegistry(); weth > 0 {
				if p := spotUSD(pool, token, weth); p > 0 {
					return p
				}
			}
		}
	}

	if reg != nil {
		// 4. Registry 2-hop: token→ETH pool, ETH→stablecoin pool.
		wethPrice := wethPriceFromRegistry()
		if wethPrice > 0 {
			var best2Hop float64
			var best2TVL float64
			for _, p := range reg.PoolsForToken(token.Address) {
				other := p.Token1
				if strings.EqualFold(token.Address, p.Token1.Address) {
					other = p.Token0
				}
				if !isEth(other.Symbol) {
					continue
				}
				if price := spotUSD(p, token, wethPrice); price > 0 && p.TVLUSD > best2TVL {
					best2Hop = price
					best2TVL = p.TVLUSD
				}
			}
			if best2Hop > 0 {
				return best2Hop
			}
		}

		// 5. ETH-like token — return live WETH price.
		if isEth(token.Symbol) {
			if weth := wethPriceFromRegistry(); weth > 0 {
				return weth
			}
		}
	}

	// 6. Unknown token with no path to a stablecoin — return 0.
	// Returning a hardcoded constant here causes phantom profits.
	return 0
}


