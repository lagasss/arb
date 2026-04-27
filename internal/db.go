package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS arb_observations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    observed_at INTEGER NOT NULL,   -- unix timestamp
    hops        INTEGER NOT NULL,   -- number of edges in cycle
    log_profit  REAL NOT NULL,      -- sum of log weights (spot-rate estimate)
    profit_pct  REAL NOT NULL,      -- (exp(log_profit)-1)*100
    profit_usd  REAL NOT NULL,      -- simulated profit in USD
    dexes       TEXT NOT NULL,      -- e.g. "UniV3,Camelot,PancakeV3"
    tokens      TEXT NOT NULL,      -- e.g. "WETH,USDC,ARB,WETH"
    pools       TEXT NOT NULL,      -- comma-separated pool addresses
    executor    TEXT NOT NULL,      -- executor contract address
    executed    INTEGER NOT NULL DEFAULT 0,  -- 1 if tx was submitted
    tx_hash     TEXT                -- null if not executed
);

CREATE INDEX IF NOT EXISTS idx_obs_observed_at ON arb_observations(observed_at);
CREATE INDEX IF NOT EXISTS idx_obs_hops        ON arb_observations(hops);
CREATE INDEX IF NOT EXISTS idx_obs_executed    ON arb_observations(executed);

CREATE TABLE IF NOT EXISTS our_trades (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    submitted_at       INTEGER NOT NULL,       -- unix timestamp when tx was sent
    tx_hash            TEXT UNIQUE NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending', -- pending/success/reverted/dropped
    block_number       INTEGER,                -- null until mined
    block_position     INTEGER,                -- our tx index within the block
    block_total_txs    INTEGER,                -- total txs in the block
    gas_used           INTEGER,
    hops               INTEGER NOT NULL,
    log_profit         REAL NOT NULL,          -- spot-rate estimate at submission
    profit_usd_est     REAL NOT NULL,          -- simulated USD profit at submission
    dexes              TEXT NOT NULL,
    tokens             TEXT NOT NULL,
    pools              TEXT NOT NULL,          -- comma-sep pool addresses in cycle
    executor           TEXT NOT NULL,          -- our executor contract address
    detect_rpc         TEXT NOT NULL,          -- RPC endpoint that delivered the swap event
    submit_rpc         TEXT NOT NULL,          -- RPC endpoint used to broadcast the tx
    competitors_before INTEGER,                -- # of txs to our cycle pools before our index
    competitor_hashes  TEXT                    -- comma-sep hashes of competitor txs
);

CREATE INDEX IF NOT EXISTS idx_trades_submitted  ON our_trades(submitted_at);
CREATE INDEX IF NOT EXISTS idx_trades_status     ON our_trades(status);
CREATE INDEX IF NOT EXISTS idx_trades_block      ON our_trades(block_number);

CREATE TABLE IF NOT EXISTS tokens (
    address    TEXT PRIMARY KEY,
    symbol     TEXT NOT NULL,
    decimals   INTEGER NOT NULL,
    is_junk    INTEGER NOT NULL DEFAULT 0,
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pools (
    address      TEXT PRIMARY KEY,
    dex          TEXT NOT NULL,
    token0       TEXT NOT NULL,
    token1       TEXT NOT NULL,
    fee_bps      INTEGER NOT NULL DEFAULT 0,
    tvl_usd      REAL NOT NULL DEFAULT 0,
    is_dead      INTEGER NOT NULL DEFAULT 0,
    first_seen   INTEGER NOT NULL,
    last_updated INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_pools_token0 ON pools(token0);
CREATE INDEX IF NOT EXISTS idx_pools_token1 ON pools(token1);
CREATE INDEX IF NOT EXISTS idx_pools_dex    ON pools(dex);

CREATE TABLE IF NOT EXISTS competitor_arbs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    seen_at          INTEGER NOT NULL,   -- unix timestamp (block time)
    block_number     INTEGER NOT NULL,
    tx_hash          TEXT UNIQUE NOT NULL,
    sender           TEXT NOT NULL,      -- tx.From (the bot's wallet)
    bot_contract     TEXT NOT NULL,      -- tx.To (the executor contract)
    flash_loan_src   TEXT NOT NULL,      -- Aave / Balancer / UniV3 / own_capital
    borrow_token     TEXT NOT NULL DEFAULT '',
    borrow_symbol    TEXT NOT NULL DEFAULT '',
    borrow_amount    TEXT NOT NULL DEFAULT '',
    path_str         TEXT NOT NULL,      -- e.g. "WETH→USDC→ARB→WETH"
    hop_count        INTEGER NOT NULL,
    dexes            TEXT NOT NULL,      -- comma-sep DEX names from hops
    profit_usd       REAL NOT NULL DEFAULT 0,
    net_usd          REAL NOT NULL DEFAULT 0,
    gas_used         INTEGER NOT NULL DEFAULT 0,
    hops_json        TEXT NOT NULL DEFAULT '[]'  -- JSON array of SwapHop
);

CREATE INDEX IF NOT EXISTS idx_comp_seen_at      ON competitor_arbs(seen_at);
CREATE INDEX IF NOT EXISTS idx_comp_sender       ON competitor_arbs(sender);
CREATE INDEX IF NOT EXISTS idx_comp_block        ON competitor_arbs(block_number);
`

// DB wraps the SQLite connection and exposes pool/token persistence.
type DB struct {
	db *sql.DB
}

// OpenDB opens (or creates) the SQLite database at path and applies the schema.
func OpenDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite with WAL supports one writer; serialise writes through a single conn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Additive migrations — ignored if the column already exists.
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN detect_rpc      TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN reject_reason  TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN arb_type       TEXT NOT NULL DEFAULT 'unknown'`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN amount_in_wei    TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN sim_profit_wei   TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN sim_profit_bps   INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN gas_price_gwei   REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN min_profit_usd_at_decision REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN pool_states_json TEXT NOT NULL DEFAULT '[]'`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN arb_type        TEXT NOT NULL DEFAULT 'unknown'`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)
	// Comparison: did we detect/execute the same arb? Filled in by arbscan's
	// comparison loop a few seconds after the competitor row is inserted, once
	// our arb_observations has had time to record its own decision.
	// comparison_result is one of:
	//   ''                         — not yet compared (default)
	//   'executed'                 — we submitted a tx for an equivalent cycle
	//   'detected_cooldown'        — we detected it but cooldown blocked submission
	//   'detected_minprofit'       — profit below dynamic minProfit floor
	//   'detected_sim_reject'      — eth_call rejected
	//   'detected_buildhops_reject'— buildHops sim said unprofitable
	//   'detected_other_reject'    — other reject_reason
	//   'missing_pool'             — at least one pool isn't in our registry
	//   'no_cycle'                 — pools known but no cycle through them in cache
	//   'not_profitable_for_us'    — cycle exists but spot-rate scoring filtered it
	//   'unknown'                  — couldn't classify
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN comparison_result TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN comparison_detail TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN compared_at INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_comp_compared_at ON competitor_arbs(compared_at)`)
	// cycle_in_memory: was the competitor's cycle in our CycleCache at the time
	// of recording? Populated by competitorCycleMatchLoop in the bot shortly
	// after the row is inserted, while the cycle cache state is still close
	// to its value at the observed block. Values: -1 unevaluated (default),
	// 0 not in cache, 1 in cache. The window column records the unix ts at
	// which we made the determination — rows stay at -1 forever if the bot
	// wasn't running when they were recorded.
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN cycle_in_memory INTEGER NOT NULL DEFAULT -1`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN cycle_in_memory_at INTEGER NOT NULL DEFAULT 0`)
	// Head-to-head comparison columns. Fill from arbscan when it inserts
	// a competitor row: receipt.effectiveGasPrice, block position, a
	// retrospective pool-state snapshot at the inclusion block, and what
	// our own floors would have said at that block.
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN notional_usd              REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN profit_bps                REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN gas_price_gwei            REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN gas_cost_usd              REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN block_position            INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN block_total_txs           INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN pool_states_json          TEXT NOT NULL DEFAULT '[]'`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN our_min_profit_usd_at_block REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN our_lpfloor_bps           REAL NOT NULL DEFAULT 0`)

	db.Exec(`ALTER TABLE arb_observations ADD COLUMN net_profit_usd REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN gas_cost_usd   REAL NOT NULL DEFAULT 0`)

	// Per-pool SIM_PHANTOM accumulator — the classifier upserts into this
	// table on every SIM_PHANTOM revert so we can rank pools by how much
	// they drive sim inaccuracy overnight. Single pool key because the
	// interesting question is "which specific pool is our sim wrong
	// about", not a per-cycle breakdown.
	db.Exec(`CREATE TABLE IF NOT EXISTS sim_phantom_pool_stats (
		pool_address        TEXT PRIMARY KEY,
		dex                 TEXT NOT NULL DEFAULT '',
		last_token_in       TEXT NOT NULL DEFAULT '',
		last_token_out      TEXT NOT NULL DEFAULT '',
		phantom_count       INTEGER NOT NULL DEFAULT 0,
		sum_overshoot_bps   INTEGER NOT NULL DEFAULT 0,
		max_overshoot_bps   INTEGER NOT NULL DEFAULT 0,
		first_seen          INTEGER NOT NULL DEFAULT 0,
		last_seen           INTEGER NOT NULL DEFAULT 0
	)`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN amount_in_wei    TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN sim_profit_wei   TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN sim_profit_bps   INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN gas_price_gwei   REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN min_profit_usd_at_decision REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN pool_states_json TEXT NOT NULL DEFAULT '[]'`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN revert_reason      TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN hop_forensics_json TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN optimizations     TEXT NOT NULL DEFAULT ''`)

	// Additive migrations for v4_pool_tokens (safe to run repeatedly).
	db.Exec(`CREATE TABLE IF NOT EXISTS v4_pool_tokens (
		pool_id TEXT PRIMARY KEY,  -- bytes32 hex (0x-prefixed, lowercase)
		token0  TEXT NOT NULL,     -- lowercase hex address
		token1  TEXT NOT NULL      -- lowercase hex address
	)`)

	// ── Pool state persistence: store everything needed for warm-start ──

	// Extend pools table with full state columns (ALTER TABLE is safe to run repeatedly).
	db.Exec(`ALTER TABLE pools ADD COLUMN disabled       INTEGER NOT NULL DEFAULT 0`)
	// pinned: manually-curated pools that should ALWAYS be in the registry, even
	// if their TVL/volume drops below pruning thresholds. Replaces the old
	// config.yaml `seeds:` section. Set via the `pinned_pools:` config list at
	// startup or directly via SQL.
	db.Exec(`ALTER TABLE pools ADD COLUMN pinned         INTEGER NOT NULL DEFAULT 0`)
	// verified: 1 if the pool has passed VerifyPool's DEX-specific sanity
	// checks (correct fee, non-zero state, sim accuracy within 1% of on-chain
	// quoter, etc.). Pools with verified=0 are loaded into the DB but excluded
	// from cycle building until verification passes. Set by VerifyPool after
	// resolution; re-verified periodically.
	db.Exec(`ALTER TABLE pools ADD COLUMN verified       INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN verified_at    INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN verify_reason  TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN fee_ppm        INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN tick_spacing    INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN sqrt_price_x96  TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN tick            INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN liquidity       TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN reserve0        TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN reserve1        TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN token0_fee_bps  INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN token1_fee_bps  INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN is_stable       INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN amp_factor      INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN curve_fee_1e10  INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN pool_id_hex     TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE pools ADD COLUMN weight0         REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN weight1         REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN volume_24h_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE pools ADD COLUMN spot_price      REAL NOT NULL DEFAULT 0`)

	// V3/V4 tick data for multi-tick simulation.
	db.Exec(`CREATE TABLE IF NOT EXISTS pool_ticks (
		pool_address TEXT NOT NULL,
		tick_index   INTEGER NOT NULL,
		liquidity_net TEXT NOT NULL,
		updated_at   INTEGER NOT NULL,
		PRIMARY KEY (pool_address, tick_index)
	)`)

	// V4 pool state (keyed by poolId, not address).
	db.Exec(`CREATE TABLE IF NOT EXISTS v4_pools (
		pool_id        TEXT PRIMARY KEY,
		token0         TEXT NOT NULL,
		token1         TEXT NOT NULL,
		fee_ppm        INTEGER NOT NULL DEFAULT 0,
		tick_spacing   INTEGER NOT NULL DEFAULT 0,
		hooks          TEXT NOT NULL DEFAULT '',
		sqrt_price_x96 TEXT NOT NULL DEFAULT '',
		tick           INTEGER NOT NULL DEFAULT 0,
		liquidity      TEXT NOT NULL DEFAULT '',
		tvl_usd        REAL NOT NULL DEFAULT 0,
		disabled       INTEGER NOT NULL DEFAULT 0,
		last_updated   INTEGER NOT NULL DEFAULT 0
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS v4_pool_ticks (
		pool_id       TEXT NOT NULL,
		tick_index    INTEGER NOT NULL,
		liquidity_net TEXT NOT NULL,
		updated_at    INTEGER NOT NULL,
		PRIMARY KEY (pool_id, tick_index)
	)`)
	// V4 volume column is added separately so older DBs upgrade safely.
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN volume_24h_usd REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN spot_price     REAL NOT NULL DEFAULT 0`)
	// V4 verification: same purpose as the pools.verified column. Hooks with
	// non-trivial swap permissions (BEFORE_SWAP / AFTER_SWAP / *_RETURNS_DELTA)
	// fail verification because we can't safely simulate them.
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN verified      INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN verified_at   INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE v4_pools ADD COLUMN verify_reason TEXT NOT NULL DEFAULT ''`)

	// Health status table (single-row, upserted by the bot).
	db.Exec(`CREATE TABLE IF NOT EXISTS bot_health (
		id              INTEGER PRIMARY KEY DEFAULT 1 CHECK(id=1),
		updated_at      INTEGER NOT NULL,
		tick_data_at    INTEGER NOT NULL DEFAULT 0,
		multicall_at    INTEGER NOT NULL DEFAULT 0,
		swap_event_at   INTEGER NOT NULL DEFAULT 0,
		cycle_rebuild_at INTEGER NOT NULL DEFAULT 0
	)`)
	// Tick coverage columns (added later — idempotent ALTERs).
	db.Exec(`ALTER TABLE bot_health ADD COLUMN ticked_pools_have  INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN ticked_pools_total INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN tracked_pool_addrs TEXT NOT NULL DEFAULT '[]'`)
	db.Exec(`ALTER TABLE bot_health ADD COLUMN rpc_state TEXT NOT NULL DEFAULT '[]'`)

	// pending_dex_support: DEX factories that arbscan has observed in competitor
	// trades but the bot can't trade through yet. Each row records what we know
	// so far (factory, label, best-guess DEXType, router candidate) and the
	// blockers we hit during research, so the user can confirm out-of-band before
	// wiring the DEX into executor.go + verify_pool.go + internal.Factories.
	//
	// Intentionally NOT wired into the bot's pool resolver — presence in this
	// table does nothing automatically. It's a task list for humans.
	//
	// status values:
	//   'research_needed'        — router/interface/quoter not yet identified
	//   'router_unknown'         — research done but no public router found
	//                              (pools observed only via MEV bots calling pair.swap() directly)
	//   'needs_contract_redeploy'— router known but uses a non-standard ABI that
	//                              requires a new Solidity dispatch type + redeploy
	//   'ready_to_add'           — everything verified, just needs the Go wire-up
	//   'enabled'                — already in internal.Factories and executor.go
	db.Exec(`CREATE TABLE IF NOT EXISTS pending_dex_support (
		factory          TEXT PRIMARY KEY,      -- factory contract address (lowercase)
		label            TEXT NOT NULL,         -- human-readable DEX name
		proposed_dex     TEXT NOT NULL DEFAULT '', -- proposed DEXType enum name (e.g. "DEXPancakeV2")
		category         TEXT NOT NULL DEFAULT '', -- "v2" | "v3" | "v3Algebra" | "custom"
		router           TEXT NOT NULL DEFAULT '', -- router address if known, empty if unknown
		router_interface TEXT NOT NULL DEFAULT '', -- "standard_v2" | "camelot_v2" | "uniswap_v3" | "algebra_v3" | "unknown"
		example_pool     TEXT NOT NULL DEFAULT '', -- one known pool address using this factory
		example_symbol   TEXT NOT NULL DEFAULT '', -- LP token symbol of the example pool
		tvl_usd_hint     REAL NOT NULL DEFAULT 0,  -- rough TVL of the example pool (best-effort)
		observed_txs     INTEGER NOT NULL DEFAULT 0, -- count of competitor_arbs rows touching this DEX
		last_seen_at     INTEGER NOT NULL DEFAULT 0, -- unix ts of most recent observation
		status           TEXT NOT NULL DEFAULT 'research_needed',
		notes            TEXT NOT NULL DEFAULT '',  -- free-form research log
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_pending_dex_status ON pending_dex_support(status)`)

	// contract_ledger: tracks every contract we've deployed or use, its purpose,
	// which trade types it handles, and its current status. Single source of
	// truth for "what contract do we route this trade through?"
	db.Exec(`CREATE TABLE IF NOT EXISTS contract_ledger (
		address          TEXT PRIMARY KEY,      -- lowercase hex, 0x-prefixed
		name             TEXT NOT NULL,         -- human name (e.g. "ArbitrageExecutor")
		kind             TEXT NOT NULL,         -- "executor" | "flash_source" | "router" | "infra" | "oracle"
		trade_types      TEXT NOT NULL DEFAULT '', -- comma-sep: "multi_hop_mixed", "v3_2hop", "v3_3hop", "own_capital", "split_route", "cex_dex"
		supported_dexes  TEXT NOT NULL DEFAULT '', -- comma-sep DEX names this contract can swap through
		flash_sources    TEXT NOT NULL DEFAULT '', -- comma-sep: "balancer", "v3_flash", "aave", "own_capital"
		max_hops         INTEGER NOT NULL DEFAULT 0, -- 0 = unlimited
		deployer         TEXT NOT NULL DEFAULT '',
		deploy_tx        TEXT NOT NULL DEFAULT '',
		deploy_block     INTEGER NOT NULL DEFAULT 0,
		chain_id         INTEGER NOT NULL DEFAULT 42161,
		gas_estimate     INTEGER NOT NULL DEFAULT 0,     -- typical gas usage for a trade through this contract (0 = N/A)
		status           TEXT NOT NULL DEFAULT 'active', -- "active" | "disabled" | "deprecated" | "not_deployed"
		notes            TEXT NOT NULL DEFAULT '',
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	)`)

	db.Exec(`CREATE TABLE IF NOT EXISTS hook_registry (
		address         TEXT PRIMARY KEY,         -- lowercase hex, 0x-prefixed; 20-byte V4 hook contract
		permission_bits INTEGER NOT NULL DEFAULT 0, -- bottom 14 bits of the hook address (V4 encodes perms here)
		has_delta_flag  INTEGER NOT NULL DEFAULT 0, -- 1 if beforeSwapReturnDelta OR afterSwapReturnDelta bit set
		classification  TEXT NOT NULL DEFAULT 'unknown', -- "safe" | "fee_only" | "delta_rewriting" | "unsafe" | "unknown"
		bytecode_hash   TEXT NOT NULL DEFAULT '', -- keccak256 of runtime code
		verified_url    TEXT NOT NULL DEFAULT '', -- etherscan/arbiscan source link if available
		on_chain_status TEXT NOT NULL DEFAULT 'pending', -- "pending" | "allowed" | "rejected" | "manual"
		reviewer_note   TEXT NOT NULL DEFAULT '',
		classified_at   INTEGER NOT NULL DEFAULT 0,
		pushed_at       INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	)`)

	d := &DB{db: db}
	d.backfillArbTypes()
	d.seedPendingDEXResearch()
	d.seedContractLedger()
	return d, nil
}

func (d *DB) seedContractLedger() {
	now := time.Now().Unix()
	d.db.Exec(`ALTER TABLE contract_ledger ADD COLUMN gas_estimate INTEGER NOT NULL DEFAULT 0`)

	seed := func(addr, name, kind, tradeTypes, dexes, flash string, maxHops, gasEstimate int, status, notes string) {
		var exists int
		d.db.QueryRow(`SELECT COUNT(*) FROM contract_ledger WHERE address=?`, addr).Scan(&exists)
		if exists > 0 {
			d.db.Exec(`UPDATE contract_ledger SET gas_estimate=? WHERE address=? AND gas_estimate!=?`, gasEstimate, addr, gasEstimate)
			return
		}
		d.db.Exec(`INSERT INTO contract_ledger
			(address, name, kind, trade_types, supported_dexes, flash_sources, max_hops,
			 gas_estimate, deployer, chain_id, status, notes, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,42161,?,?,?,?)`,
			addr, name, kind, tradeTypes, dexes, flash, maxHops, gasEstimate,
			"0x612fb8be2a1f0fd904ca45e5bba46f11b35c2c57",
			status, notes, now, now)
	}

	// ── Our executor contracts ──
	//                                                                                    maxHops gasEst
	seed("0x73af65fe487ac4d8b2fb7dc420c15913e652e9aa",
		"ArbitrageExecutor", "executor",
		"multi_hop_mixed",
		"UniV2,UniV3,SushiV2,SushiV3,Camelot,CamelotV3,RamsesV2,RamsesV3,PancakeV3,ZyberV3,Chronos,DeltaSwap,Swapr,ArbSwap,Curve,BalancerW,UniV4",
		"balancer,v3_flash,aave", 0, 900_000,
		"active",
		"Main multi-source executor. Supports all DEXes and flash sources. Deployed 2026-04-12, replacing 0x6D808C46.")

	seed("0x33347af466ce0adc4abe27ff84388facf64d43ce",
		"V3FlashMini", "executor",
		"v3_2hop,v3_3hop",
		"UniV3,SushiV3,RamsesV3,PancakeV3,CamelotV3,ZyberV3",
		"v3_flash", 3, 380_000,
		"active",
		"Gas-optimized V3-only executor. 2-3 hops max, V3 flash borrow only. Deploy block 451900987.")

	seed("0x6d808c4670a50f7de224791e1b2a98c590157aea",
		"ArbitrageExecutor (old)", "executor",
		"multi_hop_mixed",
		"UniV2,UniV3,SushiV2,SushiV3,Camelot,CamelotV3,RamsesV2,RamsesV3,PancakeV3,Curve,BalancerW",
		"balancer", 0, 900_000,
		"deprecated",
		"Original Balancer-only executor. Replaced 2026-04-12 by 0x73aF65fe with V3 flash + Aave support + UniV4 dispatch.")

	seed("0x0000000000000000000000000000000000000000",
		"HookRegistry", "infra",
		"", "UniV4", "", 0, 0,
		"not_deployed",
		"Shared V4 hook whitelist. V4Mini + MixedV3V4Executor both delegate isAllowed(hook) here. Address updated post-deploy via UPDATE contract_ledger.")

	// ── Flash loan sources ──
	seed("0xba12222222228d8ba445958a75a0704d566bf2c8",
		"Balancer Vault", "flash_source",
		"", "", "balancer", 0, 0,
		"active",
		"Zero-fee flash loans. ~70 borrowable tokens. Primary flash source.")

	seed("0x794a61358d6845594f94dc1db02a252b5b4814ad",
		"Aave V3 Pool", "flash_source",
		"", "", "aave", 0, 0,
		"active",
		"5 bps flash loan fee. ~20 borrowable tokens. Tertiary flash source.")

	// ── Infrastructure ──
	seed("0xca11bde05977b3631167028862be2a173976ca11",
		"Multicall3", "infra",
		"", "", "", 0, 0,
		"active",
		"Batch state reads (tryAggregate). Used for pool reserves, sqrtPrice, ticks, liquidity.")

	seed("0x61ffe014ba17989e743c5f6cb21bf9697530b21e",
		"UniV3 QuoterV2", "infra",
		"", "", "", 0, 0,
		"active",
		"Off-chain price quotations for simulator calibration and pool verification.")

	seed("0x76fd297e2d437cd7f76d50f01afe6160f86e9990",
		"UniV4 StateView", "infra",
		"", "", "", 0, 0,
		"active",
		"V4 pool state reader (ticks, liquidity, sqrtPriceX96).")

	seed("0x360e68faccca8ca495c1b759fd9eee466db9fb32",
		"UniV4 PoolManager", "infra",
		"", "", "", 0, 0,
		"active",
		"V4 singleton. Swap execution + settlement via unlock() callbacks.")

	// ── Not yet deployed ──
	seed("not_deployed:split_arb",
		"SplitArb", "executor",
		"split_route,cex_dex",
		"UniV3,PancakeV3,CamelotV3,RamsesV3",
		"balancer", 0, 600_000,
		"not_deployed",
		"Split-route directional arb using Balancer flash loans. Code in contracts/SplitArb.sol. For CEX-DEX arbitrage.")

	seed("not_deployed:own_capital_mini",
		"OwnCapitalMini", "executor",
		"own_capital_2hop",
		"UniV3,PancakeV3,CamelotV3,SushiV3,RamsesV3",
		"own_capital", 3, 340_000,
		"not_deployed",
		"Minimal executor for own-capital 2-3 hop arbs. No flash loan overhead. Target: match competitor gas at ~340K. Needed for $0.01-$0.10 thin-margin trades.")

	seed("not_deployed:v4_mini",
		"V4Mini", "executor",
		"v4_2hop,v4_3hop,v4_4hop,v4_5hop",
		"UniV4",
		"v3_flash", 5, 300_000,
		"not_deployed",
		"V4-only executor. Single PoolManager.unlock for the whole cycle (vs one unlock per hop in the generic executor); native-ETH-aware (currency=0x0 routed via pm.settle{value:}); per-pool hooks gate (allowedHooks whitelist starts empty, owner-toggleable). Code in contracts/V4Mini.sol. Target gas: 220k 2-hop / 280k 3-hop / 340k 4-hop / 395k 5-hop. Closes the V4_HANDLER reject class on V4-ETH and active-hook pools.")

	seed("not_deployed:mixed_v3v4",
		"MixedV3V4Executor", "executor",
		"mixed_v3v4_2hop,mixed_v3v4_3hop,mixed_v3v4_4hop,mixed_v3v4_5hop",
		"UniV3,SushiV3,RamsesV3,PancakeV3,CamelotV3,ZyberV3,UniV4",
		"v3_flash", 5, 450_000,
		"not_deployed",
		"Mixed V3+V4 executor. Wraps the entire cycle in ONE PoolManager.unlock; V4 hops use pm.swap+settle/take, V3 hops use direct pool.swap (callback runs inside the same unlock context). Targets the V4_HANDLER reject class on mixed-DEX cycles like UniV4-UniV4-UniV3 that V4Mini can't take. Native-ETH + hooks-gate inherited from V4Mini's V4 path. Code in contracts/MixedV3V4Executor.sol. Target gas: 350k 2-hop / 430k 3-hop / 510k 4-hop / 590k 5-hop.")
}

// seedPendingDEXResearch inserts known-but-unsupported DEX factories into
// pending_dex_support. Runs idempotently at startup — only inserts if the
// factory doesn't already exist (ON CONFLICT DO NOTHING via UpsertPendingDEX
// would overwrite, so we check first).
func (d *DB) seedPendingDEXResearch() {
	seeds := []PendingDEX{
		{
			Factory:         "0x02a84c1b3bbd7401a5f7fa98a384ebc70bb5749e",
			Label:           "PancakeV2",
			ProposedDEX:     "DEXPancakeV2",
			Category:        "v2",
			RouterInterface: "unknown",
			ExamplePool:     "0xa59bd260f9707ea44551c510f714ccd482ec75d8",
			ExampleSymbol:   "Cake-LP",
			TVLUSDHint:      190,
			ObservedTxs:     1,
			Status:          "router_unknown",
			Notes: "LP symbol=Cake-LP (WETH/USDC). Pool has 0.036 WETH + $84 USDC (~$190 TVL). " +
				"6 swap events observed in recent 2k blocks, ALL from MEV bot addresses " +
				"(0x07964f13…, 0x8a1ba3d5…, 0x71e688f6…) calling pair.swap() directly — " +
				"zero router-routed swaps. Config comment says '27× blocked cycles' historically. " +
				"PancakeSwap has a V2 router on Arbitrum but address unverified from on-chain data alone. " +
				"Needs: (1) find and verify PancakeV2Router02 address on Arbitrum, " +
				"(2) confirm router.factory()==0x02a84c1b, " +
				"(3) confirm router uses standard IUniswapV2Router interface, " +
				"(4) add DEXPancakeV2 enum + dexRouter entry + VerifyV2RouterReachable pass, " +
				"(5) add to internal.Factories.",
		},
		{
			Factory:         "0x1c6e968f2e6c9dec61db874e28589fd5ce3e1f2c",
			Label:           "ArbIdex",
			ProposedDEX:     "DEXArbIdex",
			Category:        "v2",
			RouterInterface: "unknown",
			ExamplePool:     "0xc4e56bfe61a5259cbd5ad1b9c98c75a97c337b7b",
			ExampleSymbol:   "ARX-LP",
			TVLUSDHint:      0,
			ObservedTxs:     0,
			Status:          "router_unknown",
			Notes: "LP symbol=ARX-LP. 1 swap event observed in recent 10k blocks from MEV bot " +
				"(0x63242a4e…) calling pair.swap() directly — zero router-routed swaps. " +
				"Config comment says '15× blocked cycles' historically but zero recent competitor_arbs rows. " +
				"Needs: (1) find and verify ArbIdex router address, " +
				"(2) confirm router.factory()==0x1c6e968f, " +
				"(3) confirm router uses standard IUniswapV2Router interface, " +
				"(4) add DEXArbIdex enum + dexRouter entry + VerifyV2RouterReachable pass, " +
				"(5) add to internal.Factories.",
		},
	}
	for _, s := range seeds {
		var exists int
		d.db.QueryRow(`SELECT COUNT(*) FROM pending_dex_support WHERE factory=?`,
			strings.ToLower(s.Factory)).Scan(&exists)
		if exists == 0 {
			_ = d.UpsertPendingDEX(s)
		}
	}
}

// PendingDEX captures a factory that arbscan has observed in competitor trades
// but the bot can't trade through yet. See the pending_dex_support schema.
type PendingDEX struct {
	Factory         string
	Label           string
	ProposedDEX     string
	Category        string
	Router          string
	RouterInterface string
	ExamplePool     string
	ExampleSymbol   string
	TVLUSDHint      float64
	ObservedTxs     int64
	LastSeenAt      int64
	Status          string
	Notes           string
}

// UpsertPendingDEX inserts or updates a pending_dex_support row, keyed on
// factory address. Intentionally idempotent — re-running research reruns
// safely without clobbering created_at.
func (d *DB) UpsertPendingDEX(p PendingDEX) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO pending_dex_support
			(factory, label, proposed_dex, category, router, router_interface,
			 example_pool, example_symbol, tvl_usd_hint, observed_txs, last_seen_at,
			 status, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(factory) DO UPDATE SET
			label            = excluded.label,
			proposed_dex     = excluded.proposed_dex,
			category         = excluded.category,
			router           = excluded.router,
			router_interface = excluded.router_interface,
			example_pool     = excluded.example_pool,
			example_symbol   = excluded.example_symbol,
			tvl_usd_hint     = excluded.tvl_usd_hint,
			observed_txs     = excluded.observed_txs,
			last_seen_at     = excluded.last_seen_at,
			status           = excluded.status,
			notes            = excluded.notes,
			updated_at       = excluded.updated_at
	`, strings.ToLower(p.Factory), p.Label, p.ProposedDEX, p.Category, strings.ToLower(p.Router),
		p.RouterInterface, strings.ToLower(p.ExamplePool), p.ExampleSymbol, p.TVLUSDHint,
		p.ObservedTxs, p.LastSeenAt, p.Status, p.Notes, now, now)
	return err
}

// ListPendingDEX returns all rows in pending_dex_support, ordered by status
// then label.
func (d *DB) ListPendingDEX() ([]PendingDEX, error) {
	rows, err := d.db.Query(`
		SELECT factory, label, proposed_dex, category, router, router_interface,
		       example_pool, example_symbol, tvl_usd_hint, observed_txs, last_seen_at,
		       status, notes
		FROM pending_dex_support
		ORDER BY status, label
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingDEX
	for rows.Next() {
		var p PendingDEX
		if err := rows.Scan(&p.Factory, &p.Label, &p.ProposedDEX, &p.Category, &p.Router,
			&p.RouterInterface, &p.ExamplePool, &p.ExampleSymbol, &p.TVLUSDHint,
			&p.ObservedTxs, &p.LastSeenAt, &p.Status, &p.Notes); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// UpdateHealth upserts the single-row bot_health table with current timestamps,
// tick-coverage stats, the JSON-encoded list of tracked V3/V4 pool addresses,
// and the JSON-encoded RPC endpoint health probes from rpcHealthLoop.
func (d *DB) UpdateHealth(tickAt, multicallAt, swapAt, cycleAt, tickedHave, tickedTotal int64, trackedAddrsJSON, rpcStateJSON string) {
	if trackedAddrsJSON == "" {
		trackedAddrsJSON = "[]"
	}
	if rpcStateJSON == "" {
		rpcStateJSON = "[]"
	}
	d.db.Exec(`INSERT INTO bot_health (id, updated_at, tick_data_at, multicall_at, swap_event_at, cycle_rebuild_at, ticked_pools_have, ticked_pools_total, tracked_pool_addrs, rpc_state)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			updated_at=excluded.updated_at,
			tick_data_at=excluded.tick_data_at,
			multicall_at=excluded.multicall_at,
			swap_event_at=excluded.swap_event_at,
			cycle_rebuild_at=excluded.cycle_rebuild_at,
			ticked_pools_have=excluded.ticked_pools_have,
			ticked_pools_total=excluded.ticked_pools_total,
			tracked_pool_addrs=excluded.tracked_pool_addrs,
			rpc_state=excluded.rpc_state`,
		time.Now().Unix(), tickAt, multicallAt, swapAt, cycleAt, tickedHave, tickedTotal, trackedAddrsJSON, rpcStateJSON)
}

// ── competitor_arbs ───────────────────────────────────────────────────────────

// CompetitorArb is one observed on-chain arbitrage by another bot.
type CompetitorArb struct {
	SeenAt        int64
	BlockNumber   uint64
	TxHash        string
	Sender        string
	BotContract   string
	FlashLoanSrc  string
	BorrowToken   string
	BorrowSymbol  string
	BorrowAmount  string
	PathStr       string
	HopCount      int
	Dexes         string // comma-sep
	ProfitUSD     float64
	NetUSD        float64
	GasUsed       uint64
	HopsJSON      interface{} // marshalled to JSON
	TokenPriceUSD float64     // USD price per unit of profit token used in calculation
	HopPricesJSON string      // JSON map: token_address → usd_price for every token in path
	// ── Comparison-with-our-bot fields (populated by arbscan) ──────────
	// NotionalUSD is borrow amount × token price, decimals-adjusted.
	// Directly comparable to our_trades.profit_usd_est / sim_profit_bps
	// calculations which use the same formula.
	NotionalUSD float64
	// ProfitBps = ProfitUSD / NotionalUSD × 10000. Our scoring pipeline
	// is bps-based; without this field every comparison query has to
	// re-derive it, and edge cases (NotionalUSD=0) produce junk.
	ProfitBps float64
	// GasPriceGwei is the effectiveGasPrice from the tx receipt. Lets us
	// compare competitor gas strategy vs our gas_price_gwei at decision time.
	GasPriceGwei float64
	// GasCostUSD = GasUsed × GasPriceGwei × 1e-9 × ETH_price. Directly
	// comparable to our txCost used in the dynamic min_profit threshold.
	GasCostUSD float64
	// BlockPosition is the competitor's tx index within its block. Tells
	// us whether they won the block (position 0) or lost a race to an
	// earlier bot. Mirrors our_trades.block_position.
	BlockPosition uint
	// BlockTotalTxs is the total tx count in the inclusion block.
	BlockTotalTxs int
	// PoolStatesJSON captures each cycle pool's slot0/liquidity/reserves
	// at the competitor's inclusion block. Snapshot is taken via
	// eth_call with blockNumber=competitor's block so we can
	// retrospectively re-run OUR simulator against the exact state they
	// faced, and quantify sim drift vs competitor's realized output.
	// JSON array mirroring arb_observations.pool_states_json.
	PoolStatesJSON string
	// OurMinProfitUSDAtBlock is what our bot's dynamic min-profit floor
	// (gas_safety_mult × txCost) would have been at this competitor's
	// block gas price. If their NetUSD > this, we SHOULD have accepted
	// it; if not, we correctly skipped. Precomputed so "would we have
	// taken this trade?" is an instant SQL query.
	OurMinProfitUSDAtBlock float64
	// OurLPFloorBps is what our dynamic LP floor would have computed for
	// this specific cycle composition (per-DEX base_bps × TVL scale ×
	// vol_mult + latency_overhead). If ProfitBps < this, we'd have
	// rejected the cycle at lpfloor even if it cleared min-profit.
	OurLPFloorBps float64
}

// InsertCompetitorArb saves a competitor arb, ignoring duplicates (same tx_hash).
func (d *DB) InsertCompetitorArb(a *CompetitorArb) error {
	hopsJSON, err := json.Marshal(a.HopsJSON)
	if err != nil {
		hopsJSON = []byte("[]")
	}
	arbType := ClassifyCompetitorArbType(string(hopsJSON))
	hopPrices := a.HopPricesJSON
	if hopPrices == "" {
		hopPrices = "{}"
	}
	poolStates := a.PoolStatesJSON
	if poolStates == "" {
		poolStates = "[]"
	}
	_, err = d.db.Exec(`
		INSERT OR IGNORE INTO competitor_arbs
			(seen_at, block_number, tx_hash, sender, bot_contract,
			 flash_loan_src, borrow_token, borrow_symbol, borrow_amount,
			 path_str, hop_count, dexes, profit_usd, net_usd, gas_used, hops_json, arb_type, token_price_usd, hop_prices_json,
			 notional_usd, profit_bps, gas_price_gwei, gas_cost_usd, block_position, block_total_txs,
			 pool_states_json, our_min_profit_usd_at_block, our_lpfloor_bps)
		VALUES (?,?,?,?,?, ?,?,?,?, ?,?,?,?,?,?,?,?,?,?,
			    ?,?,?,?,?,?, ?,?,?)`,
		a.SeenAt, a.BlockNumber, a.TxHash, a.Sender, a.BotContract,
		a.FlashLoanSrc, a.BorrowToken, a.BorrowSymbol, a.BorrowAmount,
		a.PathStr, a.HopCount, a.Dexes, a.ProfitUSD, a.NetUSD, a.GasUsed, string(hopsJSON),
		arbType, a.TokenPriceUSD, hopPrices,
		a.NotionalUSD, a.ProfitBps, a.GasPriceGwei, a.GasCostUSD, a.BlockPosition, a.BlockTotalTxs,
		poolStates, a.OurMinProfitUSDAtBlock, a.OurLPFloorBps,
	)
	return err
}

// CompetitorArbRow is the read-side view used by compwatcher (includes rowid and raw JSON string).
type CompetitorArbRow struct {
	ID          int64
	ProfitUSD   float64
	HopsJSONStr string
}

// PoolTVL returns the cached TVL (USD) for a pool address, or 0 if unknown.
// Used by arbscan to reconstruct competitor cycle LP-floor math without
// needing the bot's in-memory registry. Best-effort: on any error returns 0,
// which callers interpret as "unknown TVL, apply neutral multiplier".
// UpsertSimPhantomStat records a SIM_PHANTOM hit against a specific pool.
// Called by the bot classifier every time a cycle reverts on-chain while
// the fresh re-sim still says profitable. overshootBps is the bps by which
// our sim over-estimated final-hop output (equal to the failing cycle's
// sim_bps since fresh_bps equals sim_bps by definition for this class).
// Lets the operator rank pools overnight by "which one is our sim wrong
// about" via a single ORDER BY.
func (d *DB) UpsertSimPhantomStat(poolAddr, dex, tokenIn, tokenOut string, overshootBps int) {
	addr := strings.ToLower(poolAddr)
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO sim_phantom_pool_stats
			(pool_address, dex, last_token_in, last_token_out,
			 phantom_count, sum_overshoot_bps, max_overshoot_bps,
			 first_seen, last_seen)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(pool_address) DO UPDATE SET
			dex              = excluded.dex,
			last_token_in    = excluded.last_token_in,
			last_token_out   = excluded.last_token_out,
			phantom_count    = phantom_count + 1,
			sum_overshoot_bps = sum_overshoot_bps + ?,
			max_overshoot_bps = MAX(max_overshoot_bps, ?),
			last_seen        = ?
	`, addr, dex, tokenIn, tokenOut, overshootBps, overshootBps, now, now,
		overshootBps, overshootBps, now)
	if err != nil {
		log.Printf("[db] UpsertSimPhantomStat: %v", err)
	}
}

func (d *DB) PoolTVL(address string) float64 {
	var tvl float64
	err := d.db.QueryRow(`SELECT COALESCE(tvl_usd,0) FROM pools WHERE address=?`, strings.ToLower(address)).Scan(&tvl)
	if err != nil {
		return 0
	}
	return tvl
}

// maxCompetitorArbID returns the current max rowid in competitor_arbs, or 0.
func (d *DB) maxCompetitorArbID() (int64, error) {
	var id int64
	err := d.db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM competitor_arbs`).Scan(&id)
	return id, err
}

// newCompetitorArbs returns rows with rowid > afterID and profit_usd >= minProfit.
func (d *DB) newCompetitorArbs(afterID int64, minProfit float64) ([]CompetitorArbRow, error) {
	rows, err := d.db.Query(
		`SELECT rowid, profit_usd, hops_json FROM competitor_arbs
		 WHERE rowid > ? AND profit_usd >= ?
		 ORDER BY rowid ASC LIMIT 100`,
		afterID, minProfit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompetitorArbRow
	for rows.Next() {
		var r CompetitorArbRow
		if err := rows.Scan(&r.ID, &r.ProfitUSD, &r.HopsJSONStr); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// UnannotatedCompetitorsForCycleMatch returns recent competitor_arbs rows that
// haven't yet been checked against our cycle cache. Only rows newer than
// `sinceUnix` are returned — rows older than that are past their evaluation
// window and stay at cycle_in_memory=-1 forever, because the cycle cache has
// since rebuilt and the check would no longer reflect the state at recording.
func (d *DB) UnannotatedCompetitorsForCycleMatch(sinceUnix int64, limit int) ([]CompetitorArbRow, error) {
	rows, err := d.db.Query(
		`SELECT rowid, profit_usd, hops_json FROM competitor_arbs
		 WHERE cycle_in_memory = -1 AND seen_at >= ?
		 ORDER BY rowid ASC LIMIT ?`,
		sinceUnix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompetitorArbRow
	for rows.Next() {
		var r CompetitorArbRow
		if err := rows.Scan(&r.ID, &r.ProfitUSD, &r.HopsJSONStr); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// SetCompetitorCycleInMemory writes the result of the cycle-cache membership
// check for a competitor_arbs row. val should be 0 (not in cache) or 1 (in
// cache). Stamps the current unix time in cycle_in_memory_at.
func (d *DB) SetCompetitorCycleInMemory(id int64, val int) error {
	_, err := d.db.Exec(
		`UPDATE competitor_arbs SET cycle_in_memory = ?, cycle_in_memory_at = ? WHERE rowid = ?`,
		val, time.Now().Unix(), id)
	return err
}

func (d *DB) Close() error {
	return d.db.Close()
}

// UpsertToken inserts or updates a token record.
// On conflict, symbol/decimals/last_seen are refreshed; is_junk is preserved.
func (d *DB) UpsertToken(t *Token) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO tokens (address, symbol, decimals, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(address) DO UPDATE SET
			symbol    = excluded.symbol,
			decimals  = excluded.decimals,
			last_seen = excluded.last_seen
	`, strings.ToLower(t.Address), t.Symbol, int(t.Decimals), now, now)
	return err
}

// MarkTokenJunk flags an address as a junk/honeypot token.
// Junk tokens are excluded from future cycle discovery whitelist snapshots.
func (d *DB) MarkTokenJunk(address string) error {
	_, err := d.db.Exec(`UPDATE tokens SET is_junk=1 WHERE address=?`, strings.ToLower(address))
	return err
}

// JunkTokenAddresses returns all token addresses flagged as junk.
func (d *DB) JunkTokenAddresses() ([]string, error) {
	rows, err := d.db.Query(`SELECT address FROM tokens WHERE is_junk=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, rows.Err()
}

// LoadTokens returns every non-junk token persisted in the `tokens` table.
// Used by the startup path to rehydrate the in-memory TokenRegistry with
// tokens discovered live in previous sessions (via PoolCreated events,
// subgraph fetches, competitor watcher, etc.).
//
// Without this rehydrate step, every restart would shrink the registry back
// down to whatever's hardcoded in cfg.Tokens, and LoadPools would silently
// drop every persisted pool whose token0/token1 was discovered live — see
// the missing-pools investigation in project_test_plan.md.
func (d *DB) LoadTokens() ([]*Token, error) {
	rows, err := d.db.Query(`SELECT address, symbol, decimals FROM tokens WHERE is_junk=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		var addr, symbol string
		var decimals int
		if err := rows.Scan(&addr, &symbol, &decimals); err != nil {
			return nil, err
		}
		out = append(out, NewToken(addr, symbol, uint8(decimals)))
	}
	return out, rows.Err()
}

// UpsertPool persists pool metadata and flags only. Volatile state (prices,
// reserves, tick data, calibrated fees) is NOT stored — it's re-derived from
// the chain within seconds of startup via multicall + calibration.
// The disabled and is_stable flags are preserved on conflict (never overwritten).
func (d *DB) UpsertPool(p *Pool) error {
	now := time.Now().Unix()

	// Always upsert both tokens before the pool itself. This guarantees that
	// every persisted pool has its token rows present, regardless of which
	// discovery path called UpsertPool. Without this, paths that bypass the
	// PoolStore.sync() loop (compwatcher, EnsurePoolPinned, on-the-fly resolvers)
	// would persist a pool whose token0/token1 references rows that don't exist
	// in the tokens table — and on the next restart LoadPools would silently
	// drop the pool because tokens.Get(t0Addr) returns false. See the
	// project_token_rehydrate_bug.md memory entry for the original bug.
	if p.Token0 != nil {
		_ = d.UpsertToken(p.Token0)
	}
	if p.Token1 != nil {
		_ = d.UpsertToken(p.Token1)
	}

	isDead := 0
	var lastUpdatedUnix int64
	if p.LastUpdated.IsZero() {
		isDead = 1
		lastUpdatedUnix = 0
	} else {
		lastUpdatedUnix = p.LastUpdated.Unix()
		if time.Since(p.LastUpdated) > poolStaleness {
			isDead = 1
		}
	}

	// Volume only updates when the in-memory value is non-zero. SavePool runs
	// constantly (every multicall pass), but Volume24hUSD only gets populated
	// every 4h by RefreshPoolMetrics — without this guard, every routine save
	// between refreshes would clobber volume back to 0.
	// TVL has the same property but is recomputed on-chain every 5s, so it's
	// always fresh and doesn't need this guard.
	_, err := d.db.Exec(`
		INSERT INTO pools (address, dex, token0, token1, fee_bps, fee_ppm, tvl_usd, volume_24h_usd,
			is_dead, first_seen, last_updated, is_stable, pool_id_hex, weight0, weight1)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(address) DO UPDATE SET
			tvl_usd        = excluded.tvl_usd,
			volume_24h_usd = CASE WHEN excluded.volume_24h_usd > 0 THEN excluded.volume_24h_usd ELSE pools.volume_24h_usd END,
			is_dead        = excluded.is_dead,
			last_updated   = excluded.last_updated,
			fee_bps      = CASE WHEN excluded.fee_bps > 0 THEN excluded.fee_bps ELSE pools.fee_bps END,
			fee_ppm      = CASE WHEN excluded.fee_ppm > 0 THEN excluded.fee_ppm ELSE pools.fee_ppm END,
			pool_id_hex  = CASE WHEN excluded.pool_id_hex != '' THEN excluded.pool_id_hex ELSE pools.pool_id_hex END,
			weight0      = CASE WHEN excluded.weight0 > 0 THEN excluded.weight0 ELSE pools.weight0 END,
			weight1      = CASE WHEN excluded.weight1 > 0 THEN excluded.weight1 ELSE pools.weight1 END
	`,
		strings.ToLower(p.Address),
		p.DEX.String(),
		strings.ToLower(p.Token0.Address),
		strings.ToLower(p.Token1.Address),
		int(p.FeeBps),
		int(p.FeePPM),
		p.TVLUSD,
		p.Volume24hUSD,
		isDead,
		now,
		lastUpdatedUnix,
		boolToInt(p.IsStable),
		p.PoolID,
		p.Weight0,
		p.Weight1,
	)
	return err
}

// MarkPoolDead marks a pool as dead (zero liquidity, deregistered, etc.).
// Pinned pools are protected — they cannot be marked dead by automated pruning.
func (d *DB) MarkPoolDead(address string) error {
	_, err := d.db.Exec(`UPDATE pools SET is_dead=1 WHERE address=? AND pinned=0`, strings.ToLower(address))
	return err
}

// DisablePool flags a pool as disabled — it will be loaded but excluded from cycles.
func (d *DB) DisablePool(address string) error {
	_, err := d.db.Exec(`UPDATE pools SET disabled=1 WHERE address=?`, strings.ToLower(address))
	return err
}

// EnablePool removes the disabled flag from a pool.
func (d *DB) EnablePool(address string) error {
	_, err := d.db.Exec(`UPDATE pools SET disabled=0 WHERE address=?`, strings.ToLower(address))
	return err
}

// SetPoolVerified updates the verified/verified_at/verify_reason columns
// for a pool. Called by VerifyPool / re-verification loops.
func (d *DB) SetPoolVerified(address string, ok bool, reason string) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := d.db.Exec(`UPDATE pools SET verified=?, verify_reason=?, verified_at=? WHERE address=?`,
		v, reason, time.Now().Unix(), strings.ToLower(address))
	return err
}

// SetV4PoolVerified updates the verified columns for a V4 pool.
func (d *DB) SetV4PoolVerified(poolID string, ok bool, reason string) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := d.db.Exec(`UPDATE v4_pools SET verified=?, verify_reason=?, verified_at=? WHERE pool_id=?`,
		v, reason, time.Now().Unix(), strings.ToLower(poolID))
	return err
}

// PinPool marks a pool as pinned — protected from TVL/volume pruning.
// Pinned pools always survive in the registry, even if their TVL drops to zero.
// Used to replace the legacy config.yaml `seeds:` mechanism.
func (d *DB) PinPool(address string) error {
	_, err := d.db.Exec(`UPDATE pools SET pinned=1, is_dead=0 WHERE address=?`, strings.ToLower(address))
	return err
}

// UnpinPool removes the pinned flag.
func (d *DB) UnpinPool(address string) error {
	_, err := d.db.Exec(`UPDATE pools SET pinned=0 WHERE address=?`, strings.ToLower(address))
	return err
}

// PoolExists returns true if a pool with the given address is in the table
// (regardless of dead/disabled state).
func (d *DB) PoolExists(address string) (bool, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM pools WHERE address=?`, strings.ToLower(address)).Scan(&n)
	return n > 0, err
}

// EnsurePoolPinned guarantees a pool exists in the DB and is flagged pinned.
// If the pool is missing, it queries the chain to discover its DEX/tokens/fee
// via ResolvePoolFromChain, then inserts a new row. Used by the bot's startup
// pinned_pools loop.
func (d *DB) EnsurePoolPinned(ctx context.Context, client *ethclient.Client, registry *PoolRegistry, tokens *TokenRegistry, address string) (*Pool, error) {
	addr := strings.ToLower(address)

	// Already in DB? Just pin it (idempotent).
	exists, err := d.PoolExists(addr)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := d.PinPool(addr); err != nil {
			return nil, err
		}
		// Reload from DB so the caller gets the up-to-date Pool.
		// We rely on the caller's normal warm-start flow to materialise it.
		return nil, nil
	}

	// Not in DB — resolve from chain. ResolvePoolFromChain calls VerifyPool
	// internally and sets p.Verified / p.VerifyReason.
	p := ResolvePoolFromChain(ctx, client, registry, tokens, addr)
	if p == nil {
		return nil, fmt.Errorf("could not resolve pool %s from chain", addr)
	}
	p.Pinned = true
	// Mark LastUpdated so UpsertPool doesn't immediately flag it dead.
	p.LastUpdated = time.Now()
	if err := d.UpsertPool(p); err != nil {
		return nil, err
	}
	if err := d.PinPool(addr); err != nil {
		return nil, err
	}
	// Persist the verification result the resolver computed.
	_ = d.SetPoolVerified(addr, p.Verified, p.VerifyReason)
	return p, nil
}

// LoadPools reads pool metadata and flags from SQLite for warm-start.
// Only loads non-dead, non-disabled pools. Volatile state (prices, reserves,
// tick data, TVL, volume) is NOT loaded — it's re-derived from chain via
// multicall + recomputeV2TVL or refreshed via subgraph metrics.
//
// resolveMissingToken is called for any token referenced by a pool that isn't
// in the in-memory registry. If non-nil and it returns a non-nil Token, that
// token is added to the registry and persisted to the DB. Pass nil to keep
// the old "silently drop pools with missing tokens" behavior. The bot startup
// passes a closure backed by FetchTokenMeta + the live ethclient so the load
// path is self-healing for the orphaned-token class of bugs (see
// project_token_rehydrate_bug.md).
func (d *DB) LoadPools(tokens *TokenRegistry, resolveMissingToken func(addr string) *Token) ([]*Pool, error) {
	rows, err := d.db.Query(`
		SELECT address, dex, token0, token1, fee_bps, fee_ppm,
			is_stable, disabled, pinned, verified, COALESCE(verify_reason,''),
			pool_id_hex, weight0, weight1
		FROM pools
		WHERE is_dead = 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// IMPORTANT: don't call UpsertToken (or any other db.Exec) from inside the
	// row iteration. The bot's SQLite is configured with MaxOpenConns=1, and a
	// `*sql.Rows` cursor holds the only connection until it's closed. Issuing
	// a second query/exec from within the loop would block forever in
	// `(*DB).conn` waiting for a connection that will never be released. So we
	// drain the entire query into a slice first, then process below.
	type rawPoolRow struct {
		addr, dexStr, t0Addr, t1Addr, poolIDHex             string
		feeBps, feePPM, isStable, disabled, pinned, verified int
		verifyReason                                          string
		w0, w1                                                float64
	}
	var raw []rawPoolRow
	for rows.Next() {
		var r rawPoolRow
		if err := rows.Scan(
			&r.addr, &r.dexStr, &r.t0Addr, &r.t1Addr, &r.feeBps, &r.feePPM,
			&r.isStable, &r.disabled, &r.pinned, &r.verified, &r.verifyReason,
			&r.poolIDHex, &r.w0, &r.w1,
		); err != nil {
			continue
		}
		raw = append(raw, r)
	}
	rows.Close() // explicit so the connection is released before we run Execs below

	// Negative cache for tokens we've already tried to resolve and failed.
	// Without this, every pool referencing an unresolvable token would trigger
	// a fresh ~150ms RPC call (× 3 retries inside FetchTokenMeta), and pools
	// with the same orphaned token in 1000+ rows would hang the startup for
	// minutes. With it, we pay at most one resolution attempt per unique
	// missing address.
	resolveFailed := make(map[string]bool)
	getOrResolve := func(addr string) (*Token, bool) {
		if t, ok := tokens.Get(addr); ok {
			return t, true
		}
		if resolveMissingToken == nil {
			return nil, false
		}
		key := strings.ToLower(addr)
		if resolveFailed[key] {
			return nil, false
		}
		t := resolveMissingToken(addr)
		if t == nil {
			resolveFailed[key] = true
			return nil, false
		}
		tokens.Add(t)
		_ = d.UpsertToken(t)
		return t, true
	}

	var pools []*Pool
	var resolvedTokens int
	for _, r := range raw {
		addr := r.addr
		dexStr := r.dexStr
		t0Addr := r.t0Addr
		t1Addr := r.t1Addr
		feeBps := r.feeBps
		feePPM := r.feePPM
		isStable := r.isStable
		disabled := r.disabled
		pinned := r.pinned
		verified := r.verified
		verifyReason := r.verifyReason
		poolIDHex := r.poolIDHex
		w0 := r.w0
		w1 := r.w1

		if disabled != 0 {
			continue
		}

		tok0WasMissing := false
		if _, ok := tokens.Get(t0Addr); !ok {
			tok0WasMissing = true
		}
		tok0, ok0 := getOrResolve(t0Addr)
		if tok0WasMissing && ok0 {
			resolvedTokens++
		}
		tok1WasMissing := false
		if _, ok := tokens.Get(t1Addr); !ok {
			tok1WasMissing = true
		}
		tok1, ok1 := getOrResolve(t1Addr)
		if tok1WasMissing && ok1 {
			resolvedTokens++
		}
		if !ok0 || !ok1 {
			continue
		}

		// TVL and Volume24h start at 0 and are recomputed by multicall + subgraph
		// refresh. Loading stale persisted values causes display jitter and
		// "phantom" placeholder values to keep coming back.
		p := &Pool{
			Address:      addr,
			DEX:          ParseDEXType(dexStr),
			FeeBps:       uint32(feeBps),
			FeePPM:       uint32(feePPM),
			Token0:       tok0,
			Token1:       tok1,
			IsStable:     isStable != 0,
			Pinned:       pinned != 0,
			Verified:     verified != 0,
			VerifyReason: verifyReason,
			PoolID:       poolIDHex,
			Weight0:      w0,
			Weight1:      w1,
			TVLUSD:       0,
			Volume24hUSD: 0,
		}
		pools = append(pools, p)
	}
	if resolvedTokens > 0 {
		log.Printf("[db] LoadPools: resolved %d previously-orphaned token(s) on the fly", resolvedTokens)
	}
	return pools, rows.Err()
}

// LoadDisabledPools returns addresses of pools flagged as disabled.
func (d *DB) LoadDisabledPools() ([]string, error) {
	rows, err := d.db.Query(`SELECT address FROM pools WHERE disabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var addrs []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs, rows.Err()
}

// Note: tick data (pool_ticks, v4_pool_ticks) tables still exist in the schema
// but are no longer written to. Tick data is volatile — re-fetched via
// FetchTickMaps every 5 seconds from the chain. Persisting it caused stale
// data issues on restart.

// ── V4 pool persistence ─────────────────────────────────────────────────────

// UpsertV4Pool inserts or updates a V4 pool with full state.
// Immutable PoolKey fields (fee_ppm, tick_spacing, hooks) are preserved if they
// are already non-zero/non-empty in the row — we only fill them in when missing.
// This lets a backfill pass set them once without ever clobbering correct values.
// spotPrice is the cached spot rate (token1 per token0) — passed as 0 from arbscan
// at pool discovery (price isn't known yet), refreshed by the bot every multicall.
func (d *DB) UpsertV4Pool(poolID, token0, token1, hooks string, feePPM, tickSpacing, tick int, sqrtPriceX96, liquidity string, tvl, volume24h, spotPrice float64) error {
	_, err := d.db.Exec(`
		INSERT INTO v4_pools (pool_id, token0, token1, fee_ppm, tick_spacing, hooks,
			sqrt_price_x96, tick, liquidity, tvl_usd, volume_24h_usd, spot_price, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pool_id) DO UPDATE SET
			fee_ppm        = CASE WHEN v4_pools.fee_ppm = 0 AND excluded.fee_ppm > 0 THEN excluded.fee_ppm ELSE v4_pools.fee_ppm END,
			tick_spacing   = CASE WHEN v4_pools.tick_spacing = 0 AND excluded.tick_spacing > 0 THEN excluded.tick_spacing ELSE v4_pools.tick_spacing END,
			hooks          = CASE WHEN v4_pools.hooks = '' AND excluded.hooks != '' THEN excluded.hooks ELSE v4_pools.hooks END,
			sqrt_price_x96 = excluded.sqrt_price_x96,
			tick           = excluded.tick,
			liquidity      = excluded.liquidity,
			tvl_usd        = excluded.tvl_usd,
			volume_24h_usd = CASE WHEN excluded.volume_24h_usd > 0 THEN excluded.volume_24h_usd ELSE v4_pools.volume_24h_usd END,
			spot_price     = CASE WHEN excluded.spot_price > 0 THEN excluded.spot_price ELSE v4_pools.spot_price END,
			last_updated   = excluded.last_updated
	`, strings.ToLower(poolID), strings.ToLower(token0), strings.ToLower(token1),
		feePPM, tickSpacing, strings.ToLower(hooks),
		sqrtPriceX96, tick, liquidity, tvl, volume24h, spotPrice, time.Now().Unix())
	return err
}

// V4PoolRow holds the data needed to create a Pool from the v4_pools table.
type V4PoolRow struct {
	PoolID      string
	Token0      string
	Token1      string
	FeePPM      uint32
	TickSpacing int32
	Hooks       string
	Verified    bool
}

// LoadV4PoolsFull returns all enabled V4 pools with fee/tickSpacing/hooks.
func (d *DB) LoadV4PoolsFull() ([]V4PoolRow, error) {
	rows, err := d.db.Query(`
		SELECT pool_id, token0, token1, fee_ppm, tick_spacing, hooks, verified
		FROM v4_pools WHERE disabled=0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []V4PoolRow
	for rows.Next() {
		var r V4PoolRow
		var verified int
		if err := rows.Scan(&r.PoolID, &r.Token0, &r.Token1, &r.FeePPM, &r.TickSpacing, &r.Hooks, &verified); err == nil {
			r.Verified = verified == 1
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// DisableV4Pool flags a V4 pool as disabled.
func (d *DB) DisableV4Pool(poolID string) error {
	_, err := d.db.Exec(`UPDATE v4_pools SET disabled=1 WHERE pool_id=?`, strings.ToLower(poolID))
	return err
}

// EnableV4Pool removes the disabled flag from a V4 pool.
func (d *DB) EnableV4Pool(poolID string) error {
	_, err := d.db.Exec(`UPDATE v4_pools SET disabled=0 WHERE pool_id=?`, strings.ToLower(poolID))
	return err
}

// V4PoolIDsMissingTickSpacing returns poolIds for V4 pools that have no
// tickSpacing recorded yet (legacy rows from before the Initialize event
// data was persisted). Used by the arbscan startup backfill.
func (d *DB) V4PoolIDsMissingTickSpacing() ([]string, error) {
	rows, err := d.db.Query(`SELECT pool_id FROM v4_pools WHERE tick_spacing = 0 AND disabled = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// Note: UpsertV4PoolTicks removed — tick data is volatile, re-fetched from chain.

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PoolCount returns the total number of pools stored.
func (d *DB) PoolCount() (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM pools`).Scan(&n)
	return n, err
}

// TokenCount returns the total number of tokens stored.
func (d *DB) TokenCount() (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM tokens`).Scan(&n)
	return n, err
}

// LivePoolCount returns pools that are not marked dead.
func (d *DB) LivePoolCount() (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM pools WHERE is_dead=0`).Scan(&n)
	return n, err
}

// ArbObservation is a single profitable cycle detection to be persisted.
type ArbObservation struct {
	ObservedAt             time.Time
	Hops                   int
	LogProfit              float64
	ProfitPct              float64
	ProfitUSD              float64
	TokenPriceUSD          float64 // USD price per unit of start token
	HopPricesJSON          string  // JSON map: token_address → usd_price
	AmountInWei            string  // optimal amountIn as decimal string
	SimProfitWei           string  // simulated profit in token base units
	SimProfitBps           float64 // simulated profit in basis points (sub-bp resolution)
	GasPriceGwei           float64 // gas price at decision time
	MinProfitUSDAtDecision float64 // dynamic min profit threshold at decision time
	PoolStatesJSON         string  // JSON array of pool states used in simulation
	Dexes                  string  // "UniV3,Camelot,PancakeV3"
	Tokens                 string  // "WETH,USDC,ARB,WETH"
	Pools                  string  // "0xabc,0xdef,0x123"
	Executor               string  // executor contract address
	DetectRPC              string  // RPC endpoint that delivered the swap event
	Executed               bool
	TxHash                 string // empty if not executed
	RejectReason           string // why it wasn't executed
	ArbType                string // "cyclic", "split_route", or "unknown"
	// GasCostUSD is the per-contract gas estimate used at decision time
	// (cpu × 380k for V3FlashMini, cpu × 900k for ArbitrageExecutor). This
	// is the actual gate cost — NOT the global 600k estimate written to
	// MinProfitUSDAtDecision. NetProfitUSD subtracts this value.
	GasCostUSD float64
	// NetProfitUSD is the post-gas profit we WOULD have kept: ProfitUSD
	// (gross, already net of flash-loan fee) minus GasCostUSD. Mirrors
	// competitor_arbs.net_usd.
	NetProfitUSD float64
}

// OurTrade is a submitted arbitrage transaction record.
type OurTrade struct {
	SubmittedAt            time.Time
	TxHash                 string
	Hops                   int
	LogProfit              float64
	ProfitUSDEst           float64
	TokenPriceUSD          float64 // USD price per unit of start token at submission
	HopPricesJSON          string  // JSON map: token_address → usd_price
	AmountInWei            string  // optimal amountIn as decimal string
	SimProfitWei           string  // simulated profit in token base units
	SimProfitBps           float64 // simulated profit in basis points (sub-bp resolution)
	GasPriceGwei           float64 // gas price at decision time
	MinProfitUSDAtDecision float64 // dynamic min profit threshold at decision time
	PoolStatesJSON         string  // JSON array of pool states used in simulation
	Dexes                  string
	Tokens                 string
	Pools                  string // comma-sep pool addresses
	Executor               string
	DetectRPC              string
	SubmitRPC              string
	// Optimizations lists comma-separated tags of fast-path shortcuts that
	// were applied to this specific trade. Used by the dashboard to measure
	// the contribution of each optimization to successful executions.
	// Known tags:
	//   "skip_dryrun_all_fresh" — all edge pools at current block + zero tick
	//                             drift, so the eth_call validation was skipped
	//   "skip_sim_above_usd"    — profit USD exceeded skip-sim threshold
	//   "skip_sim_above_bps"    — profit bps exceeded skip-sim threshold
	Optimizations string
}

// TradeReceipt holds the on-chain result of a trade, populated after mining.
type TradeReceipt struct {
	TxHash            string
	Status            string // "success" | "reverted" | "dropped"
	BlockNumber       uint64
	BlockPosition     uint   // tx index in block
	BlockTotalTxs     int
	GasUsed           uint64
	CompetitorsBefore int
	CompetitorHashes  string // comma-sep
	// Set on revert only:
	RevertReason     string // eth_call re-simulation error at time of diagnosis
	HopForensicsJSON string // JSON-encoded RevertForensics
}

// InsertArbObservation persists a single arb detection.
func (d *DB) InsertArbObservation(o ArbObservation) error {
	executed := 0
	if o.Executed {
		executed = 1
	}
	var txHash *string
	if o.TxHash != "" {
		txHash = &o.TxHash
	}
	arbType := o.ArbType
	if arbType == "" {
		arbType = ClassifyArbTypeFromTokens(o.Tokens)
	}
	hopPrices := o.HopPricesJSON
	if hopPrices == "" {
		hopPrices = "{}"
	}
	poolStates := o.PoolStatesJSON
	if poolStates == "" {
		poolStates = "[]"
	}
	_, err := d.db.Exec(`
		INSERT INTO arb_observations
			(observed_at, hops, log_profit, profit_pct, profit_usd,
			 token_price_usd, hop_prices_json,
			 amount_in_wei, sim_profit_wei, sim_profit_bps,
			 gas_price_gwei, min_profit_usd_at_decision, pool_states_json,
			 dexes, tokens, pools, executor, detect_rpc, executed, tx_hash, reject_reason, arb_type, net_profit_usd, gas_cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		o.ObservedAt.Unix(),
		o.Hops,
		o.LogProfit,
		o.ProfitPct,
		o.ProfitUSD,
		o.TokenPriceUSD,
		hopPrices,
		o.AmountInWei,
		o.SimProfitWei,
		int64(math.Round(o.SimProfitBps)),
		o.GasPriceGwei,
		o.MinProfitUSDAtDecision,
		poolStates,
		o.Dexes,
		o.Tokens,
		o.Pools,
		strings.ToLower(o.Executor),
		o.DetectRPC,
		executed,
		txHash,
		o.RejectReason,
		arbType,
		o.NetProfitUSD,
		o.GasCostUSD,
	)
	return err
}

// InsertTrade records a newly submitted transaction (status=pending).
func (d *DB) InsertTrade(t OurTrade) error {
	hopPrices := t.HopPricesJSON
	if hopPrices == "" {
		hopPrices = "{}"
	}
	poolStates := t.PoolStatesJSON
	if poolStates == "" {
		poolStates = "[]"
	}
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO our_trades
			(submitted_at, tx_hash, status, hops, log_profit, profit_usd_est,
			 token_price_usd, hop_prices_json,
			 amount_in_wei, sim_profit_wei, sim_profit_bps,
			 gas_price_gwei, min_profit_usd_at_decision, pool_states_json,
			 dexes, tokens, pools, executor, detect_rpc, submit_rpc, optimizations)
		VALUES (?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		t.SubmittedAt.Unix(),
		strings.ToLower(t.TxHash),
		t.Hops,
		t.LogProfit,
		t.ProfitUSDEst,
		t.TokenPriceUSD,
		hopPrices,
		t.AmountInWei,
		t.SimProfitWei,
		int64(math.Round(t.SimProfitBps)),
		t.GasPriceGwei,
		t.MinProfitUSDAtDecision,
		poolStates,
		t.Dexes,
		t.Tokens,
		t.Pools,
		strings.ToLower(t.Executor),
		t.DetectRPC,
		t.SubmitRPC,
		t.Optimizations,
	)
	return err
}

// UpdateTradeReceipt fills in on-chain result fields after the tx is mined.
func (d *DB) UpdateTradeReceipt(r TradeReceipt) error {
	_, err := d.db.Exec(`
		UPDATE our_trades SET
			status             = ?,
			block_number       = ?,
			block_position     = ?,
			block_total_txs    = ?,
			gas_used           = ?,
			competitors_before = ?,
			competitor_hashes  = ?,
			revert_reason      = ?,
			hop_forensics_json = ?
		WHERE tx_hash = ?
	`,
		r.Status,
		r.BlockNumber,
		r.BlockPosition,
		r.BlockTotalTxs,
		r.GasUsed,
		r.CompetitorsBefore,
		r.CompetitorHashes,
		r.RevertReason,
		r.HopForensicsJSON,
		strings.ToLower(r.TxHash),
	)
	return err
}

// TradeRecord is the minimal view of our_trades used by LPCalibrator.
type TradeRecord struct {
	Status string
	Dexes  string
}

// QueryTradesSince returns all our_trades rows with submitted_at >= since (unix timestamp).
func (d *DB) QueryTradesSince(since int64) ([]TradeRecord, error) {
	rows, err := d.db.Query(
		`SELECT status, dexes FROM our_trades WHERE submitted_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TradeRecord
	for rows.Next() {
		var r TradeRecord
		if err := rows.Scan(&r.Status, &r.Dexes); err == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// TradeCount returns total submitted trades and success count.
func (d *DB) TradeCount() (total, success int, err error) {
	err = d.db.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) FROM our_trades`).Scan(&total, &success)
	return
}

// PruneOldObservations deletes observations older than retentionHours and trims
// to maxRows (keeping the most recent rows). Returns number of rows deleted.
func (d *DB) PruneOldObservations(retentionHours int, maxRows int) (int64, error) {
	cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour).Unix()
	res, err := d.db.Exec(`DELETE FROM arb_observations WHERE observed_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	byAge, _ := res.RowsAffected()

	// Trim to maxRows, keeping newest.
	var byCount int64
	if maxRows > 0 {
		res2, err := d.db.Exec(`
			DELETE FROM arb_observations WHERE id IN (
				SELECT id FROM arb_observations
				ORDER BY id ASC
				LIMIT MAX(0, (SELECT COUNT(*) FROM arb_observations) - ?)
			)`, maxRows)
		if err == nil {
			byCount, _ = res2.RowsAffected()
		}
	}
	return byAge + byCount, nil
}

// ObservationCount returns total rows in arb_observations.
func (d *DB) ObservationCount() (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM arb_observations`).Scan(&n)
	return n, err
}

// ── V4 pool token cache ───────────────────────────────────────────────────────

// UpsertV4PoolToken persists a V4 poolId → (token0, token1) mapping.
// All values should be lowercase hex strings. Ignores duplicates.
func (d *DB) UpsertV4PoolToken(poolId, token0, token1 string) error {
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO v4_pool_tokens (pool_id, token0, token1) VALUES (?, ?, ?)`,
		strings.ToLower(poolId), strings.ToLower(token0), strings.ToLower(token1),
	)
	return err
}

// LoadV4PoolTokens returns all stored V4 pool token mappings as
// [][3]string{poolId, token0, token1}.
func (d *DB) LoadV4PoolTokens() ([][3]string, error) {
	rows, err := d.db.Query(`SELECT pool_id, token0, token1 FROM v4_pool_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r [3]string
		if err := rows.Scan(&r[0], &r[1], &r[2]); err == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ── Arb type classification ───────────────────────────────────────────────────

// ClassifyArbTypeFromTokens classifies an arb_observations row using its tokens
// field (e.g. "WETH,USDC,ARB,WETH").
//
//   cyclic      — first token == last token (the path returns to its origin)
//   split_route — first token == last token but all intermediate tokens are the
//                 same (degenerate single-pair split; unlikely from our bot)
//   unknown     — anything else
func ClassifyArbTypeFromTokens(tokens string) string {
	parts := strings.Split(tokens, ",")
	if len(parts) < 2 {
		return "unknown"
	}
	first := strings.TrimSpace(parts[0])
	last := strings.TrimSpace(parts[len(parts)-1])
	if first == "" || last == "" {
		return "unknown"
	}
	if strings.EqualFold(first, last) {
		return "cyclic"
	}
	return "unknown"
}

// ClassifyCompetitorArbType classifies a competitor_arbs row using its hops_json.
//
//   cyclic        — strictly continuous chain: hop[i].out == hop[i+1].in for all i,
//                   AND first token_in == last token_out, AND amount_out[i] ≈
//                   amount_in[i+1] within a few percent tolerance (so the value
//                   actually flows through the cycle — not just the token labels).
//   split_route   — 2+ hops share the same token_in (parallel legs consuming the same
//                   intermediate token, then merging back). Multi-leg arbs that fan
//                   out and back in. Profit numbers are reliable when all tokens are
//                   stablecoins; may be unreliable otherwise.
//   discontinuous — chain has breaks (hop[i].out != hop[i+1].in OR amounts don't
//                   flow) AND no parallel legs share the same input. Indicates
//                   hidden hops, unsupported DEX swaps that arbscan didn't capture,
//                   or multiple independent swaps in the same tx that arbscan
//                   mislabeled as a cycle (the historical bug that inflated
//                   "missed $400-$4000 opportunities" — every single top-20
//                   entry traced back to amount-chain mismatches like
//                   "hop0 outputs 3207 USDT but hop1 consumes 12827 USDT").
//   unknown       — single hop or unparseable data
//
// Amount-chain tolerance: 2% — real cyclic arbs may differ slightly hop-to-hop
// due to decimal rounding in arbscan's human-amount display (decimals_out may
// round before hops_json is serialised), but the flow is always within a bp or
// two. A 2% gap is the smallest threshold that cleanly separates real cycles
// from non-cycles while forgiving display-layer rounding on wei-heavy hops.
func ClassifyCompetitorArbType(hopsJSON string) string {
	if hopsJSON == "" || hopsJSON == "[]" || hopsJSON == "null" {
		return "unknown"
	}

	var hops []struct {
		TokenIn     string `json:"token_in"`
		TokenOut    string `json:"token_out"`
		AmountIn    string `json:"amount_in_human"`
		AmountOut   string `json:"amount_out_human"`
	}
	if err := json.Unmarshal([]byte(hopsJSON), &hops); err != nil || len(hops) == 0 {
		return "unknown"
	}
	if len(hops) == 1 {
		return "unknown"
	}

	// Check token-level chain continuity: hop[i].token_out must == hop[i+1].token_in.
	tokenContinuous := true
	for i := 0; i < len(hops)-1; i++ {
		if strings.ToLower(hops[i].TokenOut) != strings.ToLower(hops[i+1].TokenIn) {
			tokenContinuous = false
			break
		}
	}

	// Check amount-level chain continuity: hop[i].amount_out must be within 2%
	// of hop[i+1].amount_in. This is the real test — token labels matching isn't
	// enough when a transaction contains multiple independent swaps through the
	// same token.
	amountContinuous := true
	if tokenContinuous {
		for i := 0; i < len(hops)-1; i++ {
			out, err1 := strconv.ParseFloat(hops[i].AmountOut, 64)
			in, err2 := strconv.ParseFloat(hops[i+1].AmountIn, 64)
			if err1 != nil || err2 != nil || out <= 0 || in <= 0 {
				amountContinuous = false
				break
			}
			// Relative difference: |out - in| / max(out, in)
			diff := out - in
			if diff < 0 {
				diff = -diff
			}
			max := out
			if in > max {
				max = in
			}
			if diff/max > 0.02 {
				amountContinuous = false
				break
			}
		}
	}

	firstIn := strings.ToLower(hops[0].TokenIn)
	lastOut := strings.ToLower(hops[len(hops)-1].TokenOut)

	if tokenContinuous && amountContinuous && firstIn == lastOut {
		return "cyclic"
	}

	// Not a clean cycle. Check for split-route pattern: 2+ hops share the same
	// input token (parallel sell legs consuming the same intermediate token).
	// This is the dominant pattern in flash-loan multi-DEX arbs where the bot
	// borrows X, splits it across multiple pools, and merges back.
	inputCounts := make(map[string]int)
	for _, h := range hops {
		inputCounts[strings.ToLower(h.TokenIn)]++
	}
	maxSameIn := 0
	for _, c := range inputCounts {
		if c > maxSameIn {
			maxSameIn = c
		}
	}
	if maxSameIn >= 2 {
		return "split_route"
	}

	// Chain broken (either tokens or amounts) AND no parallel legs — hidden
	// hops, unsupported DEXes, or multiple independent swaps in the same tx.
	return "discontinuous"
}

// backfillArbTypes updates rows whose arb_type is still 'unknown' after a migration.
// Runs once at DB open; safe to call multiple times.
func (d *DB) backfillArbTypes() {
	// arb_observations: classify from tokens column.
	rows, err := d.db.Query(`SELECT id, tokens FROM arb_observations WHERE arb_type = 'unknown'`)
	if err == nil {
		type row struct {
			id     int64
			tokens string
		}
		var todo []row
		for rows.Next() {
			var r row
			if rows.Scan(&r.id, &r.tokens) == nil {
				todo = append(todo, r)
			}
		}
		rows.Close()
		for _, r := range todo {
			t := ClassifyArbTypeFromTokens(r.tokens)
			d.db.Exec(`UPDATE arb_observations SET arb_type = ? WHERE id = ?`, t, r.id)
		}
		if len(todo) > 0 {
			fmt.Printf("[db] backfilled arb_type for %d arb_observations rows\n", len(todo))
		}
	}

	// competitor_arbs: classify from hops_json column.
	rows2, err := d.db.Query(`SELECT id, hops_json FROM competitor_arbs WHERE arb_type = 'unknown'`)
	if err == nil {
		type row struct {
			id       int64
			hopsJSON string
		}
		var todo []row
		for rows2.Next() {
			var r row
			if rows2.Scan(&r.id, &r.hopsJSON) == nil {
				todo = append(todo, r)
			}
		}
		rows2.Close()
		for _, r := range todo {
			t := ClassifyCompetitorArbType(r.hopsJSON)
			d.db.Exec(`UPDATE competitor_arbs SET arb_type = ? WHERE id = ?`, t, r.id)
		}
		if len(todo) > 0 {
			fmt.Printf("[db] backfilled arb_type for %d competitor_arbs rows\n", len(todo))
		}
	}
}
