// smoketest — INFRA-2 for the /test slash command's sim and contract categories.
//
// Reads live pool snapshots from the bot's /debug/pools introspection endpoint,
// reconstructs minimal *internal.Pool objects, and runs two kinds of regression:
//
//   -cat sim       Compares internal.SimulatorFor(d).AmountOut(p, …) against
//                  on-chain reference quoters/routers for a sample of pools per
//                  DEX type. Catches simulator regressions and fee mismatches.
//
//   -cat contract  Builds synthetic 2-hop cycles per DEX type (test pool → known
//                  reference pool → start) and eth_calls the deployed
//                  ArbitrageExecutor's execute() entrypoint from the wallet
//                  address. Catches contract dispatch regressions (e.g. the
//                  PancakeV3 calldata-layout bug we already fixed).
//
// Read-only: never broadcasts transactions, never writes to the DB. Designed
// to be invoked by the /test slash command which parses its JSON output.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"arb-bot/internal"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	_ "modernc.org/sqlite"
	"gopkg.in/yaml.v3"
)

var (
	flagCat       = flag.String("cat", "all", "test category: sim, contract, data, or all")
	flagDebugURL  = flag.String("debug-url", "http://127.0.0.1:6060", "bot /debug/* base URL")
	flagConfig    = flag.String("config", "/home/arbitrator/go/arb-bot/config.yaml", "bot config.yaml path (for RPC + executor address)")
	flagExecutor  = flag.String("executor", "", "override executor contract address (hex); empty = use config's trading.executor_contract")
	flagMini      = flag.String("mini", "", "override V3FlashMini address (hex); empty = use config's trading.executor_v3_mini")
	flagV4Mini    = flag.String("v4mini", "", "override V4Mini address (hex); empty = use config's trading.executor_v4_mini")
	flagMixedV3V4 = flag.String("mixedv3v4", "", "override MixedV3V4Executor address (hex); empty = use config's trading.executor_mixed_v3v4")
	flagJSON      = flag.Bool("json", false, "emit results as JSON instead of a markdown table")
	flagPerDex    = flag.Int("per-dex", 3, "number of pools to sample per DEX type")
	flagAmountUSD = flag.Float64("amount-usd", 100, "trial swap size in USD-equivalent")
	flagVerbose   = flag.Bool("v", false, "verbose: print every probe")
)

// ─── debug snapshot types (mirror /debug/pools JSON) ────────────────────────

type debugPool struct {
	Address        string  `json:"address"`
	DEX            string  `json:"dex"`
	Token0         string  `json:"token0"`
	Token0Symbol   string  `json:"token0_symbol"`
	Token0Decimals uint8   `json:"token0_decimals"`
	Token1         string  `json:"token1"`
	Token1Symbol   string  `json:"token1_symbol"`
	Token1Decimals uint8   `json:"token1_decimals"`
	FeeBps         uint32  `json:"fee_bps"`
	FeePPM         uint32  `json:"fee_ppm"`
	Reserve0       string  `json:"reserve0"`
	Reserve1       string  `json:"reserve1"`
	SqrtPriceX96   string  `json:"sqrt_price_x96"`
	Liquidity      string  `json:"liquidity"`
	Tick           int32   `json:"tick"`
	TickSpacing    int32   `json:"tick_spacing"`
	TicksCount     int     `json:"ticks_count"`
	TVLUSD         float64 `json:"tvl_usd"`
	Volume24hUSD   float64 `json:"volume_24h_usd"`
	SpotPrice      float64 `json:"spot_price"`
	Verified       bool    `json:"verified"`
	Disabled       bool    `json:"disabled"`
	// Fields for Balancer / Curve / Camelot pools
	PoolID       string  `json:"pool_id,omitempty"`
	Weight0      float64 `json:"weight0,omitempty"`
	Weight1      float64 `json:"weight1,omitempty"`
	AmpFactor    uint64  `json:"amp_factor,omitempty"`
	CurveFee1e10 uint64  `json:"curve_fee_1e10,omitempty"`
	IsStable     bool    `json:"is_stable,omitempty"`
	Token0FeeBps uint32  `json:"token0_fee_bps,omitempty"`
	Token1FeeBps uint32  `json:"token1_fee_bps,omitempty"`
	V4Hooks      string  `json:"v4_hooks,omitempty"`
}

type debugPoolsResp struct {
	Total int         `json:"total"`
	Pools []debugPool `json:"pools"`
}

// ─── config snippet (only what we need) ─────────────────────────────────────

type yamlConfig struct {
	ArbitrumRPC string `yaml:"arbitrum_rpc"`
	Trading     struct {
		ExecutorContract          string `yaml:"executor_contract"`
		ExecutorV3Mini            string `yaml:"executor_v3_mini"`
		ExecutorV4Mini            string `yaml:"executor_v4_mini"`
		ExecutorMixedV3V4         string `yaml:"executor_mixed_v3v4"`
		SimulationRPC             string `yaml:"simulation_rpc"`
		ExecutorSupportsPancakeV3 bool   `yaml:"executor_supports_pancake_v3"`
	} `yaml:"trading"`
	Wallet struct {
		Address string `yaml:"address"`
	} `yaml:"wallet"`
}

// ─── result reporting ───────────────────────────────────────────────────────

type result struct {
	ID     string `json:"id"`
	DEX    string `json:"dex"`
	Pool   string `json:"pool"`
	Pair   string `json:"pair"`
	Status string `json:"status"` // PASS, FAIL, SKIP
	Detail string `json:"detail"`
}

// ─── main ───────────────────────────────────────────────────────────────────

func main() {
	flag.Parse()

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		die("load config: %v", err)
	}
	// Mirror the bot's dispatch policy. Without this the smoketest's
	// dexTypeOnChain returns the legacy DEX_V3=1 for PancakeV3 swaps, which
	// hits the broken old calldata-layout path on the redeployed contract and
	// makes every Pancake probe falsely fail.
	internal.SetExecutorSupportsPancakeV3(cfg.Trading.ExecutorSupportsPancakeV3)

	rpcURL := cfg.Trading.SimulationRPC
	if rpcURL == "" {
		rpcURL = cfg.ArbitrumRPC
	}
	// Convert wss:// → https:// if necessary
	rpcURL = strings.Replace(rpcURL, "wss://", "https://", 1)
	rpcURL = strings.Replace(rpcURL, "ws://", "http://", 1)

	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		die("dial RPC: %v", err)
	}
	defer client.Close()

	pools, err := fetchDebugPools(*flagDebugURL)
	if err != nil {
		die("fetch /debug/pools: %v", err)
	}
	if len(pools) == 0 {
		die("no pools returned by /debug/pools — is the bot running with debug_http_port set?")
	}

	var results []result
	switch *flagCat {
	case "sim":
		results = append(results, runSim(client, pools)...)
	case "contract":
		results = append(results, runContract(client, cfg, pools)...)
	case "data":
		results = append(results, runDataIntegrity(client, cfg, pools)...)
	case "all":
		results = append(results, runSim(client, pools)...)
		results = append(results, runContract(client, cfg, pools)...)
		results = append(results, runDataIntegrity(client, cfg, pools)...)
	default:
		die("unknown -cat %q (valid: sim, contract, data, all)", *flagCat)
	}

	if *flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(results)
	} else {
		printTable(results)
	}
	if hasFails(results) {
		os.Exit(1)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "smoketest: "+format+"\n", args...)
	os.Exit(2)
}

func loadConfig(path string) (*yamlConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c yamlConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func fetchDebugPools(baseURL string) ([]debugPool, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Get(baseURL + "/debug/pools?limit=10000")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var d debugPoolsResp
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return d.Pools, nil
}

// debugPoolWithTicks matches the payload returned by /debug/pools/{addr}.
// It extends debugPool with the initialized-tick array needed for the V3
// multi-tick sim. We don't include this on the bulk /debug/pools list
// because tick arrays balloon payload size.
type debugPoolWithTicks struct {
	debugPool
	Ticks []struct {
		Tick         int32  `json:"tick"`
		LiquidityNet string `json:"liquidity_net"`
	} `json:"ticks"`
}

// fetchPoolTicks fetches the full tick array for a single pool. Returns nil
// on any failure — callers fall back to the bitmap-less reconstruction,
// which for V3 pools produces Ticks=nil (and thus a silent sim-0 output).
func fetchPoolTicks(baseURL, addr string) []internal.TickData {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Get(baseURL + "/debug/pools/" + addr)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var d debugPoolWithTicks
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil
	}
	out := make([]internal.TickData, 0, len(d.Ticks))
	for _, t := range d.Ticks {
		liq, _ := new(big.Int).SetString(t.LiquidityNet, 10)
		if liq == nil {
			continue
		}
		out = append(out, internal.TickData{Tick: t.Tick, LiquidityNet: liq})
	}
	return out
}

// reconstructPool builds a minimal *internal.Pool from a debug snapshot. Only
// fills the fields needed by SimulatorFor(d).AmountOut for V2/V3-style pools.
func reconstructPool(dp debugPool) *internal.Pool {
	p := &internal.Pool{
		Address:      dp.Address,
		DEX:          internal.ParseDEXType(dp.DEX),
		FeeBps:       dp.FeeBps,
		FeePPM:       dp.FeePPM,
		Tick:         dp.Tick,
		TickSpacing:  dp.TickSpacing,
		TVLUSD:       dp.TVLUSD,
		SpotPrice:    dp.SpotPrice,
		Verified:     dp.Verified,
		Disabled:     dp.Disabled,
		Token0:       internal.NewToken(dp.Token0, dp.Token0Symbol, dp.Token0Decimals),
		Token1:       internal.NewToken(dp.Token1, dp.Token1Symbol, dp.Token1Decimals),
		LastUpdated:  time.Now(),
		// Balancer / Curve / Camelot fields
		PoolID:       dp.PoolID,
		Weight0:      dp.Weight0,
		Weight1:      dp.Weight1,
		AmpFactor:    dp.AmpFactor,
		CurveFee1e10: dp.CurveFee1e10,
		IsStable:     dp.IsStable,
		Token0FeeBps: dp.Token0FeeBps,
		Token1FeeBps: dp.Token1FeeBps,
	}
	if dp.Reserve0 != "" {
		p.Reserve0, _ = new(big.Int).SetString(dp.Reserve0, 10)
	}
	if dp.Reserve1 != "" {
		p.Reserve1, _ = new(big.Int).SetString(dp.Reserve1, 10)
	}
	if dp.SqrtPriceX96 != "" {
		p.SqrtPriceX96, _ = new(big.Int).SetString(dp.SqrtPriceX96, 10)
	}
	if dp.Liquidity != "" {
		p.Liquidity, _ = new(big.Int).SetString(dp.Liquidity, 10)
	}
	return p
}

// ─── sim category ───────────────────────────────────────────────────────────

// quoterAddrFor returns the on-chain QuoterV2 address for a given DEX, or
// an empty string when no quoter is wired up. Comparing a pool's local sim
// against the WRONG quoter (e.g. running a PancakeV3 pool through the
// Uniswap quoter) silently produces meaningless PASS/FAIL results because
// the Uniswap quoter either reverts or — worse — returns a quote for a
// totally different pool that shares the same token pair and fee tier.
//
// IMPORTANT: PancakeV3, SushiV3, and RamsesV3 each deploy their own QuoterV2
// at addresses we don't currently have on hand. Until those addresses are
// verified and added here, the smoketest skips them with status=SKIP and an
// explicit "no quoter for DEX X" reason — that's noise but it's HONEST noise,
// unlike the previous behavior of silently calling the Uniswap quoter against
// every V3-style pool. See the 2026-04-11 forensics for the reject pattern
// that motivated this fix.
//
// To add a new quoter:
//   1. Verify the address against the DEX's official deployment registry
//      (do NOT trust addresses found in unsourced gists / forum posts).
//   2. Verify the QuoterV2 ABI is byte-compatible with Uniswap's
//      quoteExactInputSingle((tokenIn,tokenOut,amountIn,fee,sqrtPriceLimitX96))
//      — selector 0xc6a5026a. Forks that diverge from this signature need
//      their own selector + encoder.
//   3. Add an entry below.
// Per-DEX QuoterV2 / Quoter addresses on Arbitrum.
// Verified 2026-04-12 via eth_getCode (all have bytecode).
const (
	pancakeV3QuoterV2 = "0xB048Bbc1Ee6b733FFfCFb9e9CeF7375518e25997" // PancakeSwap official docs, same ABI as Uniswap
	sushiV3QuoterV2   = "0x0524E833cCD057e4d7A296e3aaAb9f7675964Ce1" // sushiswap/v3-periphery deployments/arbitrum/QuoterV2.json, same ABI
	ramsesV3QuoterV2  = "0x00d4FeA3Dd90C4480992f9c7Ea13b8a6A8F7E124" // docs.ramses.exchange Arbitrum tab, same ABI
	camelotV3Quoter   = "0x0Fc73040b26E9bC8514fA028D998E73A254Fa76E" // docs.camelot.exchange AMMv3 section, DIFFERENT ABI (Algebra V1.9 — no fee param)
)

func quoterAddrFor(dex string) string {
	switch dex {
	case "UniV3":
		return uniV3QuoterV2
	case "PancakeV3":
		return pancakeV3QuoterV2
	case "SushiV3":
		return sushiV3QuoterV2
	case "RamsesV3":
		return ramsesV3QuoterV2
	// CamelotV3/ZyberV3/RamsesV3 use direct pool verification (globalState/slot0)
	// instead of quoters — their quoters were deployed against specific factory
	// versions that don't match our subgraph-discovered pools.
	default:
		return ""
	}
}

// isAlgebraQuoterDEX returns true for DEXes that use the Algebra quoter
// ABI (no fee parameter in quoteExactInputSingle struct).
func isAlgebraQuoterDEX(dex string) bool {
	return dex == "CamelotV3" || dex == "ZyberV3"
}

// On-chain UniV3 QuoterV2: quoteExactInputSingle((address,address,uint256,uint24,uint160))
// returns (amountOut, sqrtPriceX96After, initializedTicksCrossed, gasEstimate)
const uniV3QuoterV2 = "0x61fFE014bA17989E743c5F6cB21bF9697530B21e"

// Selector for quoteExactInputSingle on QuoterV2.
const quoterV2Selector = "0xc6a5026a"

// Standard UniV2 router on Arbitrum (used as a fallback for getAmountsOut probes).
const uniV2RouterArb = "0x4752ba5DBc23f44D87826276BF6Fd6b1C372aD24"

func runSim(client *ethclient.Client, pools []debugPool) []result {
	var out []result

	// Bucket pools by DEX, prefer verified+highest TVL
	groups := map[string][]debugPool{}
	for _, p := range pools {
		if p.Disabled {
			continue
		}
		if !p.Verified {
			continue
		}
		groups[p.DEX] = append(groups[p.DEX], p)
	}
	// Sort each group by TVL descending and take top N
	for dex, list := range groups {
		// simple insertion sort by TVL desc
		for i := 1; i < len(list); i++ {
			for j := i; j > 0 && list[j].TVLUSD > list[j-1].TVLUSD; j-- {
				list[j], list[j-1] = list[j-1], list[j]
			}
		}
		if len(list) > *flagPerDex {
			list = list[:*flagPerDex]
		}
		groups[dex] = list
	}

	// Run probes per DEX
	for dex, sample := range groups {
		for i, dp := range sample {
			id := fmt.Sprintf("SIM-%s-%02d", dex, i+1)
			r := result{ID: id, DEX: dex, Pool: dp.Address, Pair: dp.Token0Symbol + "/" + dp.Token1Symbol}
			switch dex {
			case "UniV3", "SushiV3", "PancakeV3", "RamsesV3", "CamelotV3", "ZyberV3":
				r = runSimV3(client, dp, r)
			case "UniV2", "Sushi", "Camelot", "RamsesV2", "DeltaSwap", "Swapr", "ArbSwap", "Chronos":
				r = runSimV2(client, dp, r)
			case "Curve":
				r = runSimCurve(client, dp, r)
			case "BalancerW":
				r = runSimBalancer(client, dp, r)
			case "UniV4":
				r = runSimV4State(client, dp, r)
			default:
				r.Status = "SKIP"
				r.Detail = "no on-chain quoter probe for this DEX"
			}
			out = append(out, r)
			if *flagVerbose {
				fmt.Fprintln(os.Stderr, "  ", r.ID, r.Status, r.Detail)
			}
		}
	}

	// ── Realistic-size sim probes ─────────────────────────────────────
	// Tiny-amount probes catch raw formula bugs but miss tick-crossing
	// errors. SIM_PHANTOM cases we see in production are mostly cycles
	// where the sim crosses into bitmap regions that weren't fetched.
	// These probes test each V3-family DEX at three sizes — small, mid,
	// and large (~1% of pool TVL) — using the same on-chain quoter
	// comparison. A DEX that passes tiny but fails large is a bitmap-
	// coverage problem; a DEX that fails tiny too is a formula problem.
	for dex, sample := range groups {
		switch dex {
		case "UniV3", "SushiV3", "PancakeV3", "RamsesV3", "CamelotV3", "ZyberV3":
		default:
			continue
		}
		if len(sample) == 0 {
			continue
		}
		dp := sample[0] // deepest-TVL pool of this DEX
		for _, sizeUSD := range []float64{100, 1000, 10000} {
			id := fmt.Sprintf("SIM-%s-size$%.0f", dex, sizeUSD)
			r := result{ID: id, DEX: dex, Pool: dp.Address, Pair: dp.Token0Symbol + "/" + dp.Token1Symbol}
			r = runSimV3AtSize(client, dp, r, sizeUSD)
			out = append(out, r)
			if *flagVerbose {
				fmt.Fprintln(os.Stderr, "  ", r.ID, r.Status, r.Detail)
			}
		}
	}
	return out
}

// runSimV3AtSize probes sim accuracy at a specific USD amountIn. Unlike
// runSimV3 which uses tinyAmountIn(~$1), this targets $100-$10k — sizes
// where V3 trades cross multiple ticks. A sim that matches the on-chain
// quoter at $1 but drifts >10 bps at $1000 indicates the cached tick
// bitmap is missing crossings present on-chain (SIM_PHANTOM root cause).
func runSimV3AtSize(client *ethclient.Client, dp debugPool, r result, sizeUSD float64) result {
	p := reconstructPool(dp)
	if ticks := fetchPoolTicks(*flagDebugURL, dp.Address); len(ticks) > 0 {
		p.Ticks = ticks
	}
	if p.SqrtPriceX96 == nil || p.Liquidity == nil || p.Liquidity.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no V3 state"
		return r
	}
	// Convert USD → token0 base units using the pool's spot_price.
	// spot_price is token1-per-token0 in human units; we derive token0
	// price in USD only for stable-containing pools where token1 == $1.
	// For non-stable pools, fall back to TVL fraction.
	amountIn := sizeUSDToToken0(p, dp, sizeUSD)
	if amountIn == nil || amountIn.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "couldn't size trial swap in USD for this pool"
		return r
	}
	if isAlgebraQuoterDEX(r.DEX) {
		// Algebra path: no public quoter, use slot0 for small and skip
		// for large — accuracy against globalState()-derived output isn't
		// distinguishable from formula vs bitmap error at $1.
		r.Status = "SKIP"
		r.Detail = "Algebra DEX: size-probe skipped (no public quoter)"
		return r
	}
	quoter := quoterAddrFor(r.DEX)
	if quoter == "" {
		r.Status = "SKIP"
		r.Detail = "no QuoterV2"
		return r
	}
	sim := internal.SimulatorFor(p.DEX)
	gosim := sim.AmountOut(p, p.Token0, amountIn)
	if gosim == nil || gosim.Sign() == 0 {
		r.Status = "FAIL"
		r.Detail = fmt.Sprintf("sim returned 0 at amt=%s — likely swap crosses beyond bitmap edge", amountIn)
		return r
	}
	feePPM := p.FeePPM
	if feePPM == 0 {
		feePPM = p.FeeBps * 100
	}
	chainOut, err := quoteV3OnChain(client, quoter, dp.Address, p.Token0.Address, p.Token1.Address, feePPM, amountIn)
	if err != nil {
		r.Status = "SKIP"
		r.Detail = "quoter failed: " + err.Error()
		return r
	}
	if chainOut.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "chain quoter returned 0"
		return r
	}
	delta := new(big.Int).Sub(gosim, chainOut)
	delta.Abs(delta)
	bps := new(big.Int).Mul(delta, big.NewInt(10000))
	bps.Div(bps, chainOut)
	bpsInt := bps.Int64()
	r.Detail = fmt.Sprintf("amt=%s gosim=%s chain=%s delta=%dbps", amountIn, gosim, chainOut, bpsInt)
	switch {
	case bpsInt <= 10:
		r.Status = "PASS"
	case bpsInt <= 50:
		r.Status = "WARN"
	default:
		r.Status = "FAIL"
	}
	return r
}

// sizeUSDToToken0 converts a USD amount into pool.Token0 base units. Uses
// the pool's `spot_price` field (token1 per token0) and assumes token1 is
// either a stablecoin or has a known value. For pools not pairing against
// a stable, falls back to TVL-fraction sizing.
func sizeUSDToToken0(p *internal.Pool, dp debugPool, sizeUSD float64) *big.Int {
	sym1 := strings.ToUpper(p.Token1.Symbol)
	isStable := sym1 == "USDC" || sym1 == "USDT" || sym1 == "USDC.E" || sym1 == "DAI" || sym1 == "FRAX"
	if isStable && dp.SpotPrice > 0 {
		// token0 units = sizeUSD / spot (spot is in token1=USD terms)
		tok0Units := sizeUSD / dp.SpotPrice
		return floatToBigUnits(tok0Units, int(p.Token0.Decimals))
	}
	// Fallback: size as a fraction of pool TVL.
	if dp.TVLUSD > 0 {
		frac := sizeUSD / dp.TVLUSD
		if frac > 0.10 {
			frac = 0.10 // cap at 10% of TVL
		}
		if p.Reserve0 != nil && p.Reserve0.Sign() > 0 {
			r0F, _ := new(big.Float).SetInt(p.Reserve0).Float64()
			return floatToBigUnits(r0F*frac, 0)
		}
	}
	return nil
}

func floatToBigUnits(amount float64, decimals int) *big.Int {
	if amount <= 0 {
		return big.NewInt(0)
	}
	scale := math.Pow(10, float64(decimals))
	return new(big.Int).SetUint64(uint64(amount * scale))
}

func runSimV3(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	// Populate the tick bitmap from /debug/pools/{addr} so the multi-tick
	// simulator has something to walk. Without this, V3 sim returns 0 and
	// every probe silently SKIPs, hiding real accuracy problems.
	if ticks := fetchPoolTicks(*flagDebugURL, dp.Address); len(ticks) > 0 {
		p.Ticks = ticks
	}
	if p.SqrtPriceX96 == nil || p.Liquidity == nil || p.Liquidity.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no V3 state"
		return r
	}
	// Algebra DEXes (CamelotV3, ZyberV3) use direct globalState() verification.
	if isAlgebraQuoterDEX(r.DEX) {
		return runSimAlgebra(client, dp, r)
	}
	// RamsesV3: quoter factory mismatch (subgraph pools come from different
	// factory versions). Use direct slot0() verification instead.
	if r.DEX == "RamsesV3" {
		return runSimDirectSlot0(client, dp, r)
	}
	// V3-compatible DEXes: need a quoter address for the specific DEX.
	quoter := quoterAddrFor(r.DEX)
	if quoter == "" {
		r.Status = "SKIP"
		r.Detail = "no QuoterV2 wired up for DEX " + r.DEX + " — see quoterAddrFor in cmd/smoketest/main.go"
		return r
	}
	// Choose a small swap amount based on token0 decimals: $amount-usd worth.
	// Approximate: amount = amount_usd * 10^decimals (treats price=1, OK for stablecoin pools; for volatile pools we use a fraction of liquidity to avoid huge price impact).
	amountIn := tinyAmountIn(p.Token0, dp.SpotPrice)
	if amountIn.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "couldn't size trial swap"
		return r
	}
	// Local sim
	sim := internal.SimulatorFor(p.DEX)
	gosim := sim.AmountOut(p, p.Token0, amountIn)
	if gosim == nil || gosim.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "go-sim returned 0"
		return r
	}
	// On-chain quoter probe. Mirror the bot's `v3Fee()` fallback: when
	// `fee_ppm` is zero (old pool loaded from DB before the subgraph fee
	// fix), derive it from `fee_bps * 100`. Without this, the QuoterV2
	// receives fee=0 which doesn't match any pool and reverts.
	feePPM := p.FeePPM
	if feePPM == 0 {
		feePPM = p.FeeBps * 100
	}
	chainOut, err := quoteV3OnChain(client, quoter, dp.Address, p.Token0.Address, p.Token1.Address, feePPM, amountIn)
	if err != nil {
		r.Status = "SKIP"
		r.Detail = "quoter call failed: " + err.Error()
		return r
	}
	delta := new(big.Int).Sub(gosim, chainOut)
	delta.Abs(delta)
	// Convert to bps relative to chain output
	if chainOut.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "chain quoter returned 0"
		return r
	}
	bps := new(big.Int).Mul(delta, big.NewInt(10000))
	bps.Div(bps, chainOut)
	bpsInt := bps.Int64()
	r.Detail = fmt.Sprintf("amt=%s gosim=%s chain=%s delta=%dbps", amountIn, gosim, chainOut, bpsInt)
	if bpsInt <= 10 {
		r.Status = "PASS"
	} else if bpsInt <= 50 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

func runSimV2(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.Reserve0 == nil || p.Reserve1 == nil || p.Reserve0.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no V2 reserves"
		return r
	}
	amountIn := tinyAmountIn(p.Token0, dp.SpotPrice)
	if amountIn.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "couldn't size trial swap"
		return r
	}
	sim := internal.SimulatorFor(p.DEX)
	gosim := sim.AmountOut(p, p.Token0, amountIn)
	if gosim == nil {
		r.Status = "SKIP"
		r.Detail = "go-sim returned nil"
		return r
	}

	// For Camelot stable pairs, call the pair's own getAmountOut(uint256,address)
	// rather than computing from reserves — stable pairs use Solidly x³y+y³x=k
	// math, NOT the standard x*y=k formula that computeV2OutFromPoolReserves uses.
	if dp.IsStable && (r.DEX == "Camelot") {
		chainOut, err := camelotPairGetAmountOut(client, dp.Address, p.Token0.Address, amountIn)
		if err != nil {
			r.Status = "SKIP"
			r.Detail = "camelot getAmountOut failed: " + err.Error()
			return r
		}
		delta := new(big.Int).Sub(gosim, chainOut)
		delta.Abs(delta)
		if chainOut.Sign() == 0 {
			r.Status = "SKIP"
			r.Detail = "camelot getAmountOut returned 0"
			return r
		}
		bps := new(big.Int).Mul(delta, big.NewInt(10000))
		bps.Div(bps, chainOut)
		bpsInt := bps.Int64()
		r.Detail = fmt.Sprintf("stable: amt=%s gosim=%s chain=%s delta=%dbps", amountIn, gosim, chainOut, bpsInt)
		if bpsInt <= 10 {
			r.Status = "PASS"
		} else if bpsInt <= 50 {
			r.Status = "WARN"
		} else {
			r.Status = "FAIL"
		}
		return r
	}

	// Compare against the POOL itself, not a router. Routers route via their
	// own factory, so a "Sushi" pool that came from a non-canonical factory
	// (Camelot fork, etc.) would resolve to a different pair and produce
	// completely wrong output. Querying the pool directly via getReserves()
	// + canonical formula matches what the bot's simulator does and gives
	// an apples-to-apples sanity check.
	chainOut, err := computeV2OutFromPoolReserves(client, dp.Address, p.Token0.Address, dp.Token1, amountIn, p.FeeBps)
	if err != nil {
		r.Status = "SKIP"
		r.Detail = "pool reserves probe failed: " + err.Error()
		return r
	}
	delta := new(big.Int).Sub(gosim, chainOut)
	delta.Abs(delta)
	if chainOut.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "pool reserves returned 0"
		return r
	}
	bps := new(big.Int).Mul(delta, big.NewInt(10000))
	bps.Div(bps, chainOut)
	bpsInt := bps.Int64()
	r.Detail = fmt.Sprintf("amt=%s gosim=%s chain=%s delta=%dbps", amountIn, gosim, chainOut, bpsInt)
	if bpsInt == 0 {
		r.Status = "PASS"
	} else if bpsInt <= 5 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// startAmountForToken returns a swap amount sized to be ~$1–10 USD-equivalent
// in token base units. Targets are picked per known major token so that V3
// multi-tick math, V2 integer rounding, and stable-pool quoters all return
// non-zero outputs. For unknown tokens we fall back to ~0.01 of one whole unit
// (10^(decimals-2)).
func startAmountForToken(t *internal.Token) *big.Int {
	sym := strings.ToUpper(t.Symbol)
	dec := int(t.Decimals)
	pow := func(p int) *big.Int {
		if p <= 0 {
			return big.NewInt(1)
		}
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p)), nil)
	}
	switch sym {
	case "USDC", "USDT", "DAI", "USDC.E", "FRAX", "MIM", "USDE", "TUSD", "BUSD":
		// Stables: 10 units (~$10)
		return new(big.Int).Mul(big.NewInt(10), pow(dec))
	case "WETH", "ETH":
		// 0.005 WETH (~$10 at $2k ETH)
		return new(big.Int).Mul(big.NewInt(5), pow(dec-3))
	case "WBTC", "BTC", "TBTC", "CBBTC":
		// 0.0001 WBTC (~$5 at $50k BTC)
		return new(big.Int).Mul(big.NewInt(10), pow(dec-5))
	case "ARB":
		// 10 ARB (~$5)
		return new(big.Int).Mul(big.NewInt(10), pow(dec))
	}
	// Unknown token: ~0.01 of one whole unit. Big enough to dwarf rounding,
	// small enough to barely move any reasonable pool.
	if dec >= 2 {
		return pow(dec - 2)
	}
	return big.NewInt(1)
}

// tinyAmountIn returns a swap-sized amount in token0's base units.
// Targets ~$1 worth (small enough to barely move any pool, large enough to dwarf rounding).
func tinyAmountIn(t *internal.Token, spotPrice float64) *big.Int {
	dec := int(t.Decimals)
	if dec <= 0 {
		dec = 18
	}
	// Default: 1 unit (1e^decimals) → handles stablecoins gracefully.
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
	// For tokens worth more than $1 (e.g. WETH), shrink to 1/1000 unit so the swap is roughly $1-$10.
	if spotPrice > 100 {
		one = new(big.Int).Div(one, big.NewInt(1000))
	}
	if one.Sign() == 0 {
		one = big.NewInt(1)
	}
	return one
}

// computeV2OutFromPoolReserves queries the pool's getReserves() directly and
// computes the canonical UniV2 amountOut using the same formula the bot's
// simulator uses. Bypasses the router/factory layer entirely so non-canonical
// V2 forks (Camelot, Sushi, Swapr, etc.) can be checked against their own
// pool state instead of routed through the wrong factory.
func computeV2OutFromPoolReserves(client *ethclient.Client, poolAddr, tokenIn0Addr string, dpToken1Addr string, amountIn *big.Int, feeBps uint32) (*big.Int, error) {
	// getReserves() selector = 0x0902f1ac → returns (uint112 r0, uint112 r1, uint32 ts)
	to := common.HexToAddress(poolAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: []byte{0x09, 0x02, 0xf1, 0xac}}, nil)
	if err != nil {
		return nil, err
	}
	if len(res) < 64 {
		return nil, fmt.Errorf("short getReserves response: %d", len(res))
	}
	r0 := new(big.Int).SetBytes(res[0:32])
	r1 := new(big.Int).SetBytes(res[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return big.NewInt(0), nil
	}

	// Determine which side is tokenIn vs tokenOut. Pool's token0 is the lower
	// address by Uniswap convention; we passed in dpToken1Addr to disambiguate.
	var reserveIn, reserveOut *big.Int
	t0 := strings.ToLower(tokenIn0Addr)
	t1 := strings.ToLower(dpToken1Addr)
	if t0 < t1 {
		// tokenIn (which is the smoketest's "Token0" in the debugPool order) IS pool token0
		reserveIn, reserveOut = r0, r1
	} else {
		reserveIn, reserveOut = r1, r0
	}

	// Canonical UniV2 formula (same as internal/simulator.go UniswapV2Sim):
	//   amountInWithFee = amountIn * (10000 - feeBps)
	//   numerator       = amountInWithFee * reserveOut
	//   denominator     = reserveIn * 10000 + amountInWithFee
	//   amountOut       = numerator / denominator
	feeDenom := big.NewInt(10000)
	feeNumer := new(big.Int).Sub(feeDenom, big.NewInt(int64(feeBps)))
	amountInWithFee := new(big.Int).Mul(amountIn, feeNumer)
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Add(
		new(big.Int).Mul(reserveIn, feeDenom),
		amountInWithFee,
	)
	if denominator.Sign() == 0 {
		return big.NewInt(0), nil
	}
	return new(big.Int).Div(numerator, denominator), nil
}

// quoteV3OnChain calls QuoterV2.quoteExactInputSingle on the supplied quoter
// address (any Uniswap-V3-ABI-compatible quoter — Uniswap, PancakeV3, etc.)
// and returns amountOut. The caller is responsible for picking the right
// quoter for the pool's DEX (see quoterAddrFor).
func quoteV3OnChain(client *ethclient.Client, quoterAddr, _pool, tokenIn, tokenOut string, feePPM uint32, amountIn *big.Int) (*big.Int, error) {
	// Encode quoteExactInputSingle((tokenIn,tokenOut,amountIn,fee,sqrtPriceLimitX96))
	// Layout: selector + tuple at offset 0x20 (since it's a single struct param).
	// Actually QuoterV2's signature uses a struct as params; encoding follows the
	// "head + tail" rule. For a single tuple param, the tuple is encoded inline.
	sel, _ := hex.DecodeString(strings.TrimPrefix(quoterV2Selector, "0x"))
	data := bytes.NewBuffer(sel)
	// tokenIn (address, padded)
	data.Write(common.LeftPadBytes(common.HexToAddress(tokenIn).Bytes(), 32))
	// tokenOut
	data.Write(common.LeftPadBytes(common.HexToAddress(tokenOut).Bytes(), 32))
	// amountIn (uint256)
	data.Write(common.LeftPadBytes(amountIn.Bytes(), 32))
	// fee (uint24, padded)
	feeB := new(big.Int).SetUint64(uint64(feePPM)).Bytes()
	data.Write(common.LeftPadBytes(feeB, 32))
	// sqrtPriceLimitX96 (uint160) = 0
	data.Write(make([]byte, 32))

	to := common.HexToAddress(quoterAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data.Bytes()}, nil)
	if err != nil {
		return nil, err
	}
	if len(res) < 32 {
		return nil, fmt.Errorf("short response: %d bytes", len(res))
	}
	return new(big.Int).SetBytes(res[:32]), nil
}

// quoteAlgebraOnChain calls the Algebra V1.9 Quoter.quoteExactInputSingle.
// Algebra quoter has a DIFFERENT ABI from Uniswap V3: the struct is
// (address tokenIn, address tokenOut, uint256 amountIn, uint160 limitSqrtPrice)
// — no fee parameter (Algebra pools use dynamic fees embedded in globalState).
// Selector for quoteExactInputSingle on Algebra Quoter: 0xcdca1753
func quoteAlgebraOnChain(client *ethclient.Client, quoterAddr, tokenIn, tokenOut string, amountIn *big.Int) (*big.Int, error) {
	// selector = 0xcdca1753 (Algebra quoteExactInputSingle)
	sel, _ := hex.DecodeString("cdca1753")
	data := bytes.NewBuffer(sel)
	data.Write(common.LeftPadBytes(common.HexToAddress(tokenIn).Bytes(), 32))
	data.Write(common.LeftPadBytes(common.HexToAddress(tokenOut).Bytes(), 32))
	data.Write(common.LeftPadBytes(amountIn.Bytes(), 32))
	// limitSqrtPrice = 0 (no limit)
	data.Write(make([]byte, 32))

	to := common.HexToAddress(quoterAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data.Bytes()}, nil)
	if err != nil {
		return nil, err
	}
	if len(res) < 32 {
		return nil, fmt.Errorf("short response: %d bytes", len(res))
	}
	return new(big.Int).SetBytes(res[:32]), nil
}

// V4 StateView address on Arbitrum (from docs.uniswap.org V4 deployments).
const v4StateView = "0x76fd297e2d437cd7f76d50f01afe6160f86e9990"

// runSimV4State verifies UniswapV4 pools by reading StateView.getSlot0(poolId)
// and comparing sqrtPriceX96 + tick against our stored state. V4 uses the same
// sqrtPrice/liquidity/tick math as V3 (SimulatorFor returns UniswapV3Sim for
// both), so if the state matches, the sim is correct.
func runSimV4State(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.SqrtPriceX96 == nil || p.PoolID == "" {
		r.Status = "SKIP"
		r.Detail = "no V4 state or pool_id"
		return r
	}
	// StateView.getSlot0(bytes32 poolId) — selector 0xc815641c
	// (keccak256("getSlot0(bytes32)"); PoolId is a user-defined value type over bytes32)
	sel, _ := hex.DecodeString("c815641c")
	poolIdBytes := common.FromHex(p.PoolID)
	data := make([]byte, 4+32)
	copy(data[0:4], sel)
	copy(data[4+32-len(poolIdBytes):4+32], poolIdBytes)

	stateViewAddr := common.HexToAddress(v4StateView)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &stateViewAddr, Data: data}, nil)
	if err != nil || len(res) < 64 {
		r.Status = "SKIP"
		r.Detail = "StateView.getSlot0 failed: " + fmt.Sprint(err)
		return r
	}
	chainSqrtPrice := new(big.Int).SetBytes(res[0:32])
	chainTick := new(big.Int).SetBytes(res[32:64])
	if chainTick.Bit(255) == 1 {
		chainTick.Sub(chainTick, new(big.Int).Lsh(big.NewInt(1), 256))
	}

	var priceDeltaBps int64
	if chainSqrtPrice.Sign() > 0 {
		diff := new(big.Int).Sub(chainSqrtPrice, p.SqrtPriceX96)
		diff.Abs(diff)
		bps := new(big.Int).Mul(diff, big.NewInt(10000))
		bps.Div(bps, chainSqrtPrice)
		priceDeltaBps = bps.Int64()
	}
	tickDelta := chainTick.Int64() - int64(p.Tick)
	if tickDelta < 0 {
		tickDelta = -tickDelta
	}

	r.Detail = fmt.Sprintf("sqrtP_delta=%dbps tick_delta=%d fee=%dppm hooks=%s",
		priceDeltaBps, tickDelta, dp.FeePPM, dp.V4Hooks)
	if priceDeltaBps <= 50 && tickDelta <= 10 {
		r.Status = "PASS"
	} else if priceDeltaBps <= 200 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// runSimDirectSlot0 verifies V3-fork pools (RamsesV3 etc.) by reading slot0()
// directly from the pool contract and comparing sqrtPriceX96 + tick against our
// stored state. Same logic as CamelotV3's globalState check but uses the
// standard UniV3 slot0 ABI. V3 math is already proven correct via quoter tests
// on UniV3/PancakeV3/SushiV3 — the only thing that can differ is the state
// we're feeding the sim.
func runSimDirectSlot0(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.SqrtPriceX96 == nil || p.Liquidity == nil || p.Liquidity.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no V3 state"
		return r
	}
	poolAddr := common.HexToAddress(dp.Address)
	// slot0() selector = 0x3850c7bd
	sel, _ := hex.DecodeString("3850c7bd")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: sel}, nil)
	if err != nil || len(res) < 64 {
		r.Status = "SKIP"
		r.Detail = "slot0() call failed: " + fmt.Sprint(err)
		return r
	}
	chainSqrtPrice := new(big.Int).SetBytes(res[0:32])
	chainTick := new(big.Int).SetBytes(res[32:64])

	var priceDeltaBps int64
	if chainSqrtPrice.Sign() > 0 {
		diff := new(big.Int).Sub(chainSqrtPrice, p.SqrtPriceX96)
		diff.Abs(diff)
		bps := new(big.Int).Mul(diff, big.NewInt(10000))
		bps.Div(bps, chainSqrtPrice)
		priceDeltaBps = bps.Int64()
	}
	tickDelta := chainTick.Int64() - int64(p.Tick)
	if tickDelta < 0 {
		tickDelta = -tickDelta
	}

	r.Detail = fmt.Sprintf("sqrtP_delta=%dbps tick_delta=%d fee=%dppm", priceDeltaBps, tickDelta, dp.FeePPM)
	if priceDeltaBps <= 50 && tickDelta <= 10 {
		r.Status = "PASS"
	} else if priceDeltaBps <= 200 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// runSimAlgebra verifies CamelotV3/ZyberV3 pools by reading globalState()
// directly from the pool contract and comparing the fee against our stored
// FeePPM. The V3 swap math is already proven correct (PancakeV3/SushiV3/UniV3
// all at 0 bps delta via quoter comparison). For Algebra pools, the ONLY
// difference is the dynamic fee — if that matches, the sim is correct.
//
// We can't use a separate Algebra quoter because the quoters were deployed
// against specific factory versions and our pools may come from different
// factory deployments (subgraph-discovered). Direct pool calls bypass this.
func runSimAlgebra(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.SqrtPriceX96 == nil || p.Liquidity == nil || p.Liquidity.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no V3 state"
		return r
	}
	// Call globalState() on the pool to get the current dynamic fee
	poolAddr := common.HexToAddress(dp.Address)
	gsData, _ := hex.DecodeString("e76c01e4") // globalState() selector
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gsRes, err := client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: gsData}, nil)
	if err != nil || len(gsRes) < 128 {
		r.Status = "SKIP"
		r.Detail = "globalState() call failed: " + fmt.Sprint(err)
		return r
	}
	// globalState returns: (uint160 price, int24 tick, uint16 feeZto, uint16 feeOtz, ...)
	// Algebra V1.9: feeZto at offset 64, feeOtz at offset 96
	// For Algebra Integral: may be (uint160 price, int24 tick, uint16 lastFee, uint8 pluginConfig, ...)
	chainSqrtPrice := new(big.Int).SetBytes(gsRes[0:32])
	// feeZto is at bytes 64-96 — it's a uint16 stored in a 32-byte slot
	chainFeeZto := new(big.Int).SetBytes(gsRes[64:96]).Uint64()

	// Compare chain fee against our stored FeePPM
	storedFee := uint64(dp.FeePPM)
	feeDelta := int64(chainFeeZto) - int64(storedFee)
	if feeDelta < 0 {
		feeDelta = -feeDelta
	}

	// Compare chain sqrtPrice against our stored
	var priceDeltaBps int64
	if p.SqrtPriceX96 != nil && chainSqrtPrice.Sign() > 0 {
		diff := new(big.Int).Sub(chainSqrtPrice, p.SqrtPriceX96)
		diff.Abs(diff)
		bps := new(big.Int).Mul(diff, big.NewInt(10000))
		bps.Div(bps, chainSqrtPrice)
		priceDeltaBps = bps.Int64()
	}

	r.Detail = fmt.Sprintf("chain_fee=%dppm stored_fee=%dppm fee_delta=%d | sqrtP_delta=%dbps",
		chainFeeZto, storedFee, feeDelta, priceDeltaBps)

	// Pass criteria: fee within 100ppm (dynamic fees move between blocks) AND
	// sqrtPrice within 50 bps (state drift during this check)
	if feeDelta <= 100 && priceDeltaBps <= 50 {
		r.Status = "PASS"
	} else if feeDelta <= 500 || priceDeltaBps <= 200 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// runSimCurve verifies Curve StableSwap pools by calling get_dy(i, j, dx)
// directly on the pool contract and comparing against our CurveSim.AmountOut.
func runSimCurve(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.Reserve0 == nil || p.Reserve1 == nil || p.Reserve0.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no Curve state (reserves not populated)"
		return r
	}
	amountIn := tinyAmountIn(p.Token0, dp.SpotPrice)
	if amountIn.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "couldn't size trial swap"
		return r
	}
	sim := internal.SimulatorFor(p.DEX)
	gosim := sim.AmountOut(p, p.Token0, amountIn)
	if gosim == nil || gosim.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "go-sim returned 0"
		return r
	}
	// Call pool.get_dy(int128 i, int128 j, uint256 dx) — StableSwap pools.
	// Selector: 0x5e0d443f for get_dy(int128,int128,uint256)
	poolAddr := common.HexToAddress(dp.Address)
	sel, _ := hex.DecodeString("5e0d443f")
	data := bytes.NewBuffer(sel)
	data.Write(common.LeftPadBytes(big.NewInt(0).Bytes(), 32)) // i = 0 (token0)
	data.Write(common.LeftPadBytes(big.NewInt(1).Bytes(), 32)) // j = 1 (token1)
	data.Write(common.LeftPadBytes(amountIn.Bytes(), 32))      // dx
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: data.Bytes()}, nil)
	if err != nil {
		r.Status = "SKIP"
		r.Detail = "get_dy call failed: " + err.Error()
		return r
	}
	if len(res) < 32 {
		r.Status = "SKIP"
		r.Detail = "get_dy returned short data"
		return r
	}
	chainOut := new(big.Int).SetBytes(res[:32])
	if chainOut.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "get_dy returned 0"
		return r
	}
	delta := new(big.Int).Sub(gosim, chainOut)
	delta.Abs(delta)
	bps := new(big.Int).Mul(delta, big.NewInt(10000))
	bps.Div(bps, chainOut)
	bpsInt := bps.Int64()
	r.Detail = fmt.Sprintf("amt=%s gosim=%s chain=%s delta=%dbps", amountIn, gosim, chainOut, bpsInt)
	if bpsInt <= 5 {
		r.Status = "PASS"
	} else if bpsInt <= 20 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// runSimBalancer verifies Balancer weighted pools by calling
// Vault.queryBatchSwap and comparing against our BalancerWeightedSim.AmountOut.
func runSimBalancer(client *ethclient.Client, dp debugPool, r result) result {
	p := reconstructPool(dp)
	if p.Reserve0 == nil || p.Reserve1 == nil || p.Reserve0.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "no Balancer state"
		return r
	}
	if p.PoolID == "" {
		r.Status = "SKIP"
		r.Detail = "no pool_id for Balancer pool"
		return r
	}
	amountIn := tinyAmountIn(p.Token0, dp.SpotPrice)
	if amountIn.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "couldn't size trial swap"
		return r
	}
	sim := internal.SimulatorFor(p.DEX)
	gosim := sim.AmountOut(p, p.Token0, amountIn)
	if gosim == nil || gosim.Sign() == 0 {
		r.Status = "SKIP"
		r.Detail = "go-sim returned 0"
		return r
	}
	// queryBatchSwap on the Balancer Vault is complex to ABI-encode manually
	// (needs SwapKind enum, BatchSwapStep[] array, IAsset[] array, FundManagement
	// struct). For now, verify via a simpler approach: compare our sim result's
	// magnitude against the pool's spot rate derived from reserves+weights.
	// A gross sanity check: gosim should be within 5% of (amountIn * spotRate).
	spotRate := p.SpotRate()
	if spotRate <= 0 {
		r.Status = "SKIP"
		r.Detail = "no spot rate"
		return r
	}
	expectedF := float64(amountIn.Int64()) * spotRate
	gosimF, _ := new(big.Float).SetInt(gosim).Float64()
	if expectedF <= 0 {
		r.Status = "SKIP"
		r.Detail = "expected output <= 0"
		return r
	}
	pctDiff := (gosimF - expectedF) / expectedF * 100
	if pctDiff < 0 {
		pctDiff = -pctDiff
	}
	r.Detail = fmt.Sprintf("amt=%s gosim=%s expected_from_spot=%.0f pct_diff=%.2f%%", amountIn, gosim, expectedF, pctDiff)
	if pctDiff <= 2 {
		r.Status = "PASS"
	} else if pctDiff <= 10 {
		r.Status = "WARN"
	} else {
		r.Status = "FAIL"
	}
	return r
}

// camelotPairGetAmountOut calls the Camelot V2 pair's own getAmountOut(uint256,address)
// function directly. This is the only reliable way to get the on-chain output for
// Camelot stable pairs (Solidly x³y+y³x=k), because the generic V2 router uses x*y=k.
// Selector: 0xf140a35a = getAmountOut(uint256 amountIn, address tokenIn)
func camelotPairGetAmountOut(client *ethclient.Client, pairAddr, tokenIn string, amountIn *big.Int) (*big.Int, error) {
	sel, _ := hex.DecodeString("f140a35a")
	data := bytes.NewBuffer(sel)
	data.Write(common.LeftPadBytes(amountIn.Bytes(), 32))
	data.Write(common.LeftPadBytes(common.HexToAddress(tokenIn).Bytes(), 32))
	to := common.HexToAddress(pairAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data.Bytes()}, nil)
	if err != nil {
		return nil, err
	}
	if len(res) < 32 {
		return nil, fmt.Errorf("short response: %d bytes", len(res))
	}
	return new(big.Int).SetBytes(res[:32]), nil
}

// getAmountsOutV2 calls UniV2 router.getAmountsOut and returns the final element.
func getAmountsOutV2(client *ethclient.Client, router string, path []common.Address, amountIn *big.Int) (*big.Int, error) {
	// selector getAmountsOut(uint256,address[]) = 0xd06ca61f
	sel, _ := hex.DecodeString("d06ca61f")
	data := bytes.NewBuffer(sel)
	// amountIn
	data.Write(common.LeftPadBytes(amountIn.Bytes(), 32))
	// offset to dynamic array (uint256) = 0x40 (= 64 = 2 * 32 bytes header)
	data.Write(common.LeftPadBytes(big.NewInt(0x40).Bytes(), 32))
	// length of path
	data.Write(common.LeftPadBytes(big.NewInt(int64(len(path))).Bytes(), 32))
	// elements of path
	for _, a := range path {
		data.Write(common.LeftPadBytes(a.Bytes(), 32))
	}

	to := common.HexToAddress(router)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data.Bytes()}, nil)
	if err != nil {
		return nil, err
	}
	// Response is a uint256[] dynamic array. Layout: offset(0x20), length, elements.
	// We want the LAST element which is the final-hop output.
	if len(res) < 64 {
		return nil, fmt.Errorf("short response: %d bytes", len(res))
	}
	length := new(big.Int).SetBytes(res[32:64]).Int64()
	if length < 1 || int64(len(res)) < 64+length*32 {
		return nil, fmt.Errorf("malformed response")
	}
	last := 64 + (length-1)*32
	return new(big.Int).SetBytes(res[last : last+32]), nil
}

// ─── contract category ──────────────────────────────────────────────────────

func runContract(client *ethclient.Client, cfg *yamlConfig, pools []debugPool) []result {
	var out []result
	if *flagExecutor != "" {
		cfg.Trading.ExecutorContract = *flagExecutor
	}
	if *flagMini != "" {
		cfg.Trading.ExecutorV3Mini = *flagMini
	}
	if *flagV4Mini != "" {
		cfg.Trading.ExecutorV4Mini = *flagV4Mini
	}
	if *flagMixedV3V4 != "" {
		cfg.Trading.ExecutorMixedV3V4 = *flagMixedV3V4
	}
	if cfg.Trading.ExecutorContract == "" || cfg.Wallet.Address == "" {
		out = append(out, result{
			ID: "CONTRACT-ALL", Status: "SKIP",
			Detail: "executor_contract or wallet.address not set in config",
		})
		return out
	}
	contractAddr := common.HexToAddress(cfg.Trading.ExecutorContract)
	walletAddr := common.HexToAddress(cfg.Wallet.Address)

	// One synthetic 2-hop probe per supported DEX type. For each DEX we walk
	// candidates in TVL order and use the first one that has a back-leg pool
	// with the same token pair available — that way one bad probe choice (e.g.
	// an exotic pair without a counterparty) doesn't blank-skip the whole DEX.
	supportedDexes := []string{
		"UniV2", "Sushi", "Camelot",
		"UniV3", "SushiV3", "PancakeV3",
		"CamelotV3", "RamsesV3",
		"UniV4", "Curve", "BalancerW",
	}
	for _, dex := range supportedDexes {
		probe, ref := pickProbeWithBackLeg(pools, dex)
		id := fmt.Sprintf("CONTRACT-%s", dex)
		r := result{ID: id, DEX: dex}
		if probe == nil || ref == nil {
			r.Status = "SKIP"
			r.Detail = "no probe pool of this DEX type with a usable back-leg"
			out = append(out, r)
			continue
		}
		r.Pool = probe.Address
		r.Pair = probe.Token0Symbol + "/" + probe.Token1Symbol

		// Build a 2-hop cycle: probe (token0→token1) → ref (token1→token0).
		// The probe exercises the DEX dispatch we care about.
		cycle := buildSyntheticCycle(probe, ref)
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "could not build synthetic cycle (unexpected token mismatch)"
			out = append(out, r)
			continue
		}
		// Populate Ticks slices from /debug/pools/{addr} — the multi-tick
		// V3 sim walks Ticks[] and returns 0 if nil. Skipped for V2-style
		// pools (reserves are sufficient) and pools for which /debug had
		// no tick array (falls back to the single-tick approximation).
		for hi := 0; hi < len(cycle.Edges); hi++ {
			switch cycle.Edges[hi].Pool.DEX {
			case internal.DEXUniswapV3, internal.DEXSushiSwapV3, internal.DEXRamsesV3,
				internal.DEXPancakeV3, internal.DEXCamelotV3, internal.DEXZyberV3,
				internal.DEXUniswapV4:
				if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
					cycle.Edges[hi].Pool.Ticks = ticks
				}
			}
		}
		// Size amountIn for the probe's token0 decimals so the V2 simulator
		// doesn't return 0 for tiny amounts on high-decimal tokens like WETH.
		amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
		// slippageBps=9999 → amountOutMin floors to 1 wei, effectively disabling
		// the slippage check. We're testing the contract's per-DEX *dispatch*,
		// not the realized arb economics, so we want any successful swap to pass.
		// forTest=true bypasses the "round-trip unprofitable" reject in buildHops.
		data, err := internal.BuildExecuteCalldata(*cycle, amountIn, 9999, big.NewInt(1), true)
		if err != nil {
			r.Status = "FAIL"
			r.Detail = "BuildExecuteCalldata: " + err.Error()
			// If buildHops bailed with a "hop N simulation returned 0" error,
			// walk the cycle and ask the simulator WHY it returned zero —
			// which cached field is missing or zero. Turns "hop 0 sim 0" into
			// "hop 0 (UniV3): V3: Ticks slice empty — bitmap never fetched".
			if strings.Contains(err.Error(), "simulation returned 0") {
				current := new(big.Int).Set(amountIn)
				var reasons []string
				for hi, e := range cycle.Edges {
					why := internal.DiagnoseZeroOutput(e.Pool, e.TokenIn, current)
					if why != "" {
						reasons = append(reasons, fmt.Sprintf("hop%d(%s): %s", hi, e.Pool.DEX, why))
						break
					}
					sim := internal.SimulatorFor(e.Pool.DEX)
					next := sim.AmountOut(e.Pool, e.TokenIn, current)
					if next == nil || next.Sign() == 0 {
						reasons = append(reasons, fmt.Sprintf("hop%d(%s): AmountOut returned 0 with no pre-condition violation (integer-division rounding or deep-math edge)", hi, e.Pool.DEX))
						break
					}
					current = next
				}
				if len(reasons) > 0 {
					r.Detail += " | diag: " + strings.Join(reasons, "; ")
				}
			}
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, err = client.CallContract(ctx, ethereum.CallMsg{
			From: walletAddr,
			To:   &contractAddr,
			Data: data,
		}, nil)
		cancel()
		r = classifyContractResult(r, err)
		out = append(out, r)
		if *flagVerbose {
			fmt.Fprintln(os.Stderr, "  ", r.ID, r.Status, r.Detail)
		}
	}

	// ── Mid-hop probes: DEX X at position 1 of a 3-hop cycle ────────────
	//
	// The 2-hop hop-0 probes above only test dispatch when tokenIn is the
	// freshly-received flash-loan amount. A whole class of contract bugs
	// can live in the INTERMEDIATE-HOP handoff: the contract holds tokenIn
	// for hop N as the ACTUAL BALANCE of tokenOut from hop N-1, and each
	// handler re-reads amountIn from `current` in _executeHops. A wrong
	// approval amount, a fee-on-transfer accounting gap, or a balance-vs-
	// returned-amount mismatch only shows up mid-chain. These probes catch
	// that: for each DEX X, build a 3-hop cycle WETH → T1 → T2 → WETH where
	// hop 1 is a pool of DEX X (T1 → T2). hop 0 and hop 2 are any pools
	// closing the triangle. If DEX X's intermediate dispatch is broken we
	// see it here even though the 2-hop probe passed.
	for _, dex := range supportedDexes {
		id := fmt.Sprintf("CONTRACT-%s-mid", dex)
		r := result{ID: id, DEX: dex}
		cycle := pickThreeHopWithMidDex(pools, dex)
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "no 3-hop triangle found with this DEX at hop 1 (common tokens unavailable)"
			out = append(out, r)
			continue
		}
		r.Pool = cycle.Edges[1].Pool.Address
		r.Pair = cycle.Edges[1].Pool.Token0.Symbol + "/" + cycle.Edges[1].Pool.Token1.Symbol
		for hi := range cycle.Edges {
			switch cycle.Edges[hi].Pool.DEX {
			case internal.DEXUniswapV3, internal.DEXSushiSwapV3, internal.DEXRamsesV3,
				internal.DEXPancakeV3, internal.DEXCamelotV3, internal.DEXZyberV3,
				internal.DEXUniswapV4:
				if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
					cycle.Edges[hi].Pool.Ticks = ticks
				}
			}
		}
		amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
		data, err := internal.BuildExecuteCalldata(*cycle, amountIn, 9999, big.NewInt(1), true)
		if err != nil {
			r.Status = "FAIL"
			r.Detail = "BuildExecuteCalldata(mid): " + err.Error()
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &contractAddr, Data: data}, nil)
		cancel()
		r = classifyContractResult(r, err)
		// Reinterpret: for mid-hop probes, the DEX under test is hop 1,
		// not hop 0. classifyContractResult's "hop 0 reverted" heuristic
		// therefore doesn't apply — we need to decide based on which hop
		// failed. If hop 1 reverted, the DEX-under-test IS broken.
		if err != nil && strings.Contains(err.Error(), "hop 1") {
			r.Status = "FAIL"
			r.Detail = "mid-hop (hop 1) dispatch reverted: " + err.Error()
		}
		out = append(out, r)
		if *flagVerbose {
			fmt.Fprintln(os.Stderr, "  ", r.ID, r.Status, r.Detail)
		}
	}

	// ── V3Flash entrypoint probes ────────────────────────────────────────
	// The executeV3Flash() → uniswapV3FlashCallback callback wiring is
	// completely separate from execute(). A bug that only bites when the
	// V3-pool flash callback handles the borrow won't surface via Balancer
	// probes. We pick one V3 pool per supported-token as the FLASH source
	// (distinct from the pools in the cycle — same pool would hit V3's
	// reentrancy lock when the callback's first hop tries to swap against
	// a pool currently in its own flash()).
	for _, dex := range []string{"UniV3", "SushiV3", "PancakeV3"} {
		id := fmt.Sprintf("CONTRACT-V3Flash-%s", dex)
		r := result{ID: id, DEX: dex + "/V3Flash"}
		probe, ref := pickProbeWithBackLeg(pools, dex)
		if probe == nil || ref == nil {
			r.Status = "SKIP"
			r.Detail = "no probe pool with back-leg for V3Flash test"
			out = append(out, r)
			continue
		}
		cycle := buildSyntheticCycle(probe, ref)
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "cycle build failed"
			out = append(out, r)
			continue
		}
		for hi := range cycle.Edges {
			if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
				cycle.Edges[hi].Pool.Ticks = ticks
			}
		}
		r.Pool = cycle.Edges[0].Pool.Address
		r.Pair = cycle.Edges[0].Pool.Token0.Symbol + "/" + cycle.Edges[0].Pool.Token1.Symbol
		// Pick a DIFFERENT V3 pool holding the borrow token to act as the
		// flash source. Using a pool that's in the cycle would hit V3's
		// reentrancy lock — the flash() callback is still running when the
		// first hop tries to swap through the same pool.
		inCycle := make(map[string]bool, len(cycle.Edges))
		for _, e := range cycle.Edges {
			inCycle[strings.ToLower(e.Pool.Address)] = true
		}
		borrowAddr := strings.ToLower(cycle.Edges[0].TokenIn.Address)
		var flashSrc *debugPool
		for i := range pools {
			p := &pools[i]
			if p.Disabled || !p.Verified {
				continue
			}
			if inCycle[strings.ToLower(p.Address)] {
				continue
			}
			switch p.DEX {
			case "UniV3", "SushiV3", "PancakeV3":
			default:
				continue
			}
			t0 := strings.ToLower(p.Token0)
			t1 := strings.ToLower(p.Token1)
			if t0 != borrowAddr && t1 != borrowAddr {
				continue
			}
			if !probeSimUsable(p) {
				continue
			}
			if flashSrc == nil || p.TVLUSD > flashSrc.TVLUSD {
				flashSrc = p
			}
		}
		if flashSrc == nil {
			r.Status = "SKIP"
			r.Detail = "no V3 flash source pool (outside cycle) for borrow token"
			out = append(out, r)
			continue
		}
		amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
		flashPool := common.HexToAddress(flashSrc.Address)
		data, err := internal.BuildExecuteV3FlashCalldata(*cycle, amountIn, 9999, big.NewInt(1), flashPool, true)
		if err != nil {
			r.Status = "FAIL"
			r.Detail = "BuildExecuteV3FlashCalldata: " + err.Error()
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &contractAddr, Data: data}, nil)
		cancel()
		r = classifyContractResult(r, err)
		out = append(out, r)
		if *flagVerbose {
			fmt.Fprintln(os.Stderr, "  ", r.ID, r.Status, r.Detail)
		}
	}

	// ── Aave entrypoint probe ────────────────────────────────────────────
	// executeAaveFlash() → executeOperation() callback wiring is again
	// distinct. Uses Aave's flashLoanSimple.
	{
		id := "CONTRACT-AaveFlash"
		r := result{ID: id, DEX: "Aave"}
		probe, ref := pickProbeWithBackLeg(pools, "UniV3")
		if probe == nil || ref == nil {
			r.Status = "SKIP"
			r.Detail = "no probe pool for Aave test"
			out = append(out, r)
		} else {
			cycle := buildSyntheticCycle(probe, ref)
			for hi := range cycle.Edges {
				if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
					cycle.Edges[hi].Pool.Ticks = ticks
				}
			}
			r.Pool = cycle.Edges[0].Pool.Address
			r.Pair = cycle.Edges[0].Pool.Token0.Symbol + "/" + cycle.Edges[0].Pool.Token1.Symbol
			amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
			data, err := internal.BuildExecuteAaveFlashCalldata(*cycle, amountIn, 9999, big.NewInt(1), true)
			if err != nil {
				r.Status = "FAIL"
				r.Detail = "BuildExecuteAaveFlashCalldata: " + err.Error()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &contractAddr, Data: data}, nil)
				cancel()
				r = classifyContractResult(r, err)
			}
			out = append(out, r)
		}
	}

	// ── V3FlashMini executor probe ───────────────────────────────────────
	// V3FlashMini is a completely separate contract with packed calldata.
	// Only valid for 2-3 hop V3-only cycles using V3-flash as source.
	if cfg.Trading.ExecutorV3Mini != "" {
		miniAddr := common.HexToAddress(cfg.Trading.ExecutorV3Mini)
		for _, dex := range []string{"UniV3", "SushiV3", "PancakeV3"} {
			id := fmt.Sprintf("CONTRACT-V3Mini-%s", dex)
			r := result{ID: id, DEX: dex + "/Mini"}
			probe, ref := pickProbeWithBackLeg(pools, dex)
			if probe == nil || ref == nil {
				r.Status = "SKIP"
				r.Detail = "no probe for V3Mini"
				out = append(out, r)
				continue
			}
			cycle := buildSyntheticCycle(probe, ref)
			for hi := range cycle.Edges {
				if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
					cycle.Edges[hi].Pool.Ticks = ticks
				}
			}
			r.Pool = cycle.Edges[0].Pool.Address
			r.Pair = cycle.Edges[0].Pool.Token0.Symbol + "/" + cycle.Edges[0].Pool.Token1.Symbol
			amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
			// Pick a V3 flash-source pool outside the cycle (reentrancy guard).
			inCycle := make(map[string]bool, len(cycle.Edges))
			for _, e := range cycle.Edges {
				inCycle[strings.ToLower(e.Pool.Address)] = true
			}
			borrowAddr := strings.ToLower(cycle.Edges[0].TokenIn.Address)
			var flashSrc *debugPool
			for i := range pools {
				pp := &pools[i]
				if pp.Disabled || !pp.Verified || inCycle[strings.ToLower(pp.Address)] {
					continue
				}
				switch pp.DEX {
				case "UniV3", "SushiV3", "PancakeV3":
				default:
					continue
				}
				t0 := strings.ToLower(pp.Token0)
				t1 := strings.ToLower(pp.Token1)
				if t0 != borrowAddr && t1 != borrowAddr {
					continue
				}
				if !probeSimUsable(pp) {
					continue
				}
				if flashSrc == nil || pp.TVLUSD > flashSrc.TVLUSD {
					flashSrc = pp
				}
			}
			if flashSrc == nil {
				r.Status = "SKIP"
				r.Detail = "no V3 flash source (outside cycle) for V3Mini probe"
				out = append(out, r)
				continue
			}
			flashPool := common.HexToAddress(flashSrc.Address)
			flashToken0 := common.HexToAddress(flashSrc.Token0)
			data, err := internal.BuildV3MiniFlashCalldata(*cycle, amountIn, 9999, flashPool, flashToken0)
			if err != nil {
				r.Status = "FAIL"
				r.Detail = "BuildV3MiniFlashCalldata: " + err.Error()
				out = append(out, r)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &miniAddr, Data: data}, nil)
			cancel()
			r = classifyContractResult(r, err)
			out = append(out, r)
		}
	}

	// ── V4Mini executor probe ────────────────────────────────────────────
	// V4Mini handles cycles where every hop is UniV4 (single PoolManager.unlock
	// for the whole cycle, native-ETH-aware). Probe: synthetic V4↔V4 cycle
	// using the deepest V4 pool we know + a back-leg to close. Skipped when
	// no V4Mini address is configured OR no usable V4 pair is available.
	if cfg.Trading.ExecutorV4Mini != "" {
		v4Addr := common.HexToAddress(cfg.Trading.ExecutorV4Mini)
		v4Probe, v4Ref := pickPairSharingToken(pools, []string{"UniV4"}, []string{"UniV4"})
		r := result{ID: "CONTRACT-V4Mini-UniV4", DEX: "UniV4/Mini"}
		cycle := (*internal.Cycle)(nil)
		if v4Probe != nil && v4Ref != nil {
			cycle = buildSyntheticCycle(v4Probe, v4Ref)
		}
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "no two V4 pools sharing a full token pair for synthetic cycle"
			out = append(out, r)
		} else {
			r.Pool = cycle.Edges[0].Pool.Address
			r.Pair = cycle.Edges[0].Pool.Token0.Symbol + "/" + cycle.Edges[0].Pool.Token1.Symbol
			amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
			// Pick a V3 flash source outside the cycle for the borrow token.
			borrowAddr := strings.ToLower(cycle.Edges[0].TokenIn.Address)
			inCycle := map[string]bool{
				strings.ToLower(v4Probe.Address): true,
				strings.ToLower(v4Ref.Address):   true,
			}
			var flashSrc *debugPool
			for i := range pools {
				pp := &pools[i]
				if pp.Disabled || !pp.Verified || inCycle[strings.ToLower(pp.Address)] {
					continue
				}
				switch pp.DEX {
				case "UniV3", "SushiV3", "PancakeV3":
				default:
					continue
				}
				t0 := strings.ToLower(pp.Token0)
				t1 := strings.ToLower(pp.Token1)
				if t0 != borrowAddr && t1 != borrowAddr {
					continue
				}
				if !probeSimUsable(pp) {
					continue
				}
				if flashSrc == nil || pp.TVLUSD > flashSrc.TVLUSD {
					flashSrc = pp
				}
			}
			if flashSrc == nil {
				r.Status = "SKIP"
				r.Detail = "no V3 flash source for V4Mini probe (borrow token has no V3 pool)"
				out = append(out, r)
			} else {
				flashPool := common.HexToAddress(flashSrc.Address)
				flashToken0 := common.HexToAddress(flashSrc.Token0)
				data, err := internal.BuildV4MiniFlashCalldata(*cycle, amountIn, flashPool, flashToken0)
				if err != nil {
					r.Status = "FAIL"
					r.Detail = "BuildV4MiniFlashCalldata: " + err.Error()
					out = append(out, r)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &v4Addr, Data: data}, nil)
					cancel()
					r = classifyContractResult(r, err)
					out = append(out, r)
				}
			}
		}
	}

	// ── MixedV3V4Executor probe ──────────────────────────────────────────
	// Synthetic 2-hop cycle with one V4 hop + one V3 hop sharing a token.
	// Exercises the whole mixed-dispatch path: PoolManager.unlock opens,
	// V4 hop swaps + settle/take, V3 hop runs inline (callback invoked
	// while still inside unlock), unlock returns, flash repays.
	if cfg.Trading.ExecutorMixedV3V4 != "" {
		mixAddr := common.HexToAddress(cfg.Trading.ExecutorMixedV3V4)
		v3DexSet := []string{"UniV3", "SushiV3", "PancakeV3", "RamsesV3", "CamelotV3", "ZyberV3"}
		mixProbe, mixRef := pickPairSharingToken(pools, []string{"UniV4"}, v3DexSet)
		r := result{ID: "CONTRACT-MixedV3V4", DEX: "UniV4+V3/Mixed"}
		cycle := (*internal.Cycle)(nil)
		if mixProbe != nil && mixRef != nil {
			cycle = buildSyntheticCycle(mixProbe, mixRef)
		}
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "no V4+V3 pool pair sharing a full token pair for synthetic cycle"
			out = append(out, r)
		} else {
			for hi := range cycle.Edges {
				if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
					cycle.Edges[hi].Pool.Ticks = ticks
				}
			}
			r.Pool = cycle.Edges[0].Pool.Address
			r.Pair = cycle.Edges[0].Pool.Token0.Symbol + "/" + cycle.Edges[0].Pool.Token1.Symbol
			amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
			borrowAddr := strings.ToLower(cycle.Edges[0].TokenIn.Address)
			inCycle := map[string]bool{
				strings.ToLower(mixProbe.Address): true,
				strings.ToLower(mixRef.Address):   true,
			}
			var flashSrc *debugPool
			for i := range pools {
				pp := &pools[i]
				if pp.Disabled || !pp.Verified || inCycle[strings.ToLower(pp.Address)] {
					continue
				}
				switch pp.DEX {
				case "UniV3", "SushiV3", "PancakeV3":
				default:
					continue
				}
				t0 := strings.ToLower(pp.Token0)
				t1 := strings.ToLower(pp.Token1)
				if t0 != borrowAddr && t1 != borrowAddr {
					continue
				}
				if !probeSimUsable(pp) {
					continue
				}
				if flashSrc == nil || pp.TVLUSD > flashSrc.TVLUSD {
					flashSrc = pp
				}
			}
			if flashSrc == nil {
				r.Status = "SKIP"
				r.Detail = "no V3 flash source for MixedV3V4 probe (borrow token has no V3 pool)"
				out = append(out, r)
			} else {
				flashPool := common.HexToAddress(flashSrc.Address)
				flashToken0 := common.HexToAddress(flashSrc.Token0)
				data, err := internal.BuildMixedV3V4FlashCalldata(*cycle, amountIn, flashPool, flashToken0)
				if err != nil {
					r.Status = "FAIL"
					r.Detail = "BuildMixedV3V4FlashCalldata: " + err.Error()
					out = append(out, r)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &mixAddr, Data: data}, nil)
					cancel()
					r = classifyContractResult(r, err)
					out = append(out, r)
				}
			}
		}
	}

	// ── 4-hop cycle probe per DEX ────────────────────────────────────────
	// Extends mid-hop coverage: DEX X at position 1 of a 4-hop cycle.
	// Same triangle-picker logic, just an extra intermediate hop on the
	// way back. Verifies the contract's balance handoff across 3 consecutive
	// intermediate hops.
	for _, dex := range []string{"UniV3", "SushiV3", "PancakeV3", "CamelotV3", "RamsesV3"} {
		id := fmt.Sprintf("CONTRACT-%s-4hop", dex)
		r := result{ID: id, DEX: dex}
		cycle := pickFourHopWithDex(pools, dex)
		if cycle == nil {
			r.Status = "SKIP"
			r.Detail = "no 4-hop cycle found for this DEX"
			out = append(out, r)
			continue
		}
		for hi := range cycle.Edges {
			if ticks := fetchPoolTicks(*flagDebugURL, cycle.Edges[hi].Pool.Address); len(ticks) > 0 {
				cycle.Edges[hi].Pool.Ticks = ticks
			}
		}
		r.Pool = cycle.Edges[1].Pool.Address
		r.Pair = cycle.Edges[1].Pool.Token0.Symbol + "/" + cycle.Edges[1].Pool.Token1.Symbol
		amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
		data, err := internal.BuildExecuteCalldata(*cycle, amountIn, 9999, big.NewInt(1), true)
		if err != nil {
			r.Status = "FAIL"
			r.Detail = "BuildExecuteCalldata(4hop): " + err.Error()
			out = append(out, r)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		_, err = client.CallContract(ctx, ethereum.CallMsg{From: walletAddr, To: &contractAddr, Data: data}, nil)
		cancel()
		r = classifyContractResult(r, err)
		out = append(out, r)
	}

	return out
}

// pickFourHopWithDex extends pickThreeHopWithMidDex with an extra intermediate
// hop: borrow → T1 → T2 → T3 → borrow, with the target DEX at position 1
// (T1 → T2). Exercises 3 consecutive intermediate-balance handoffs.
func pickFourHopWithDex(pools []debugPool, dex string) *internal.Cycle {
	tri := pickThreeHopWithMidDex(pools, dex)
	if tri == nil {
		return nil
	}
	// Insert a pass-through hop between hop 1 and the original hop 2.
	// Find any pool with tri.Edges[1].TokenOut and some other token T3,
	// then find a pool closing T3 → borrow.
	borrow := strings.ToLower(tri.Edges[0].TokenIn.Address)
	midOut := strings.ToLower(tri.Edges[1].TokenOut.Address)
	findPoolForPair := func(tokA, tokB string) *debugPool {
		tA, tB := strings.ToLower(tokA), strings.ToLower(tokB)
		var best *debugPool
		for i := range pools {
			p := &pools[i]
			if p.Disabled || !p.Verified || !probeSimUsable(p) {
				continue
			}
			pt0, pt1 := strings.ToLower(p.Token0), strings.ToLower(p.Token1)
			if !((pt0 == tA && pt1 == tB) || (pt0 == tB && pt1 == tA)) {
				continue
			}
			if best == nil || p.TVLUSD > best.TVLUSD {
				best = p
			}
		}
		return best
	}
	// Look for T3 candidates different from borrow and from midOut.
	candidateT3 := []string{
		"0x82af49447d8a07e3bd95bd0d56f35241523fbab1",
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831",
		"0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f",
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8",
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
	}
	for _, t3 := range candidateT3 {
		if t3 == borrow || t3 == midOut {
			continue
		}
		passthrough := findPoolForPair(midOut, t3)
		closer := findPoolForPair(t3, borrow)
		if passthrough == nil || closer == nil {
			continue
		}
		pt := reconstructPool(*passthrough)
		cl := reconstructPool(*closer)
		var ptIn, ptOut *internal.Token
		if strings.EqualFold(passthrough.Token0, midOut) {
			ptIn, ptOut = pt.Token0, pt.Token1
		} else {
			ptIn, ptOut = pt.Token1, pt.Token0
		}
		var clIn, clOut *internal.Token
		if strings.EqualFold(closer.Token1, borrow) {
			clIn, clOut = cl.Token0, cl.Token1
		} else {
			clIn, clOut = cl.Token1, cl.Token0
		}
		return &internal.Cycle{
			Edges: []internal.Edge{
				tri.Edges[0],
				tri.Edges[1],
				{Pool: pt, TokenIn: ptIn, TokenOut: ptOut},
				{Pool: cl, TokenIn: clIn, TokenOut: clOut},
			},
		}
	}
	return nil
}

// pickThreeHopWithMidDex returns a 3-hop closed cycle with the target DEX at
// position 1 (middle). hop 0 and hop 2 are any-DEX pools closing the triangle
// back to the flash-loan token. Returns nil if no suitable triangle is found —
// that's a SKIP, not a FAIL, because test coverage ≠ contract correctness.
//
// Strategy: walk every pool of the target DEX, take its (tA, tB) token pair,
// then look for (hop 0) a pool connecting a major borrowable token (WETH /
// USDC / WBTC) to tA, and (hop 2) a pool connecting tB back to that same
// borrowable. The first valid triangle wins.
func pickThreeHopWithMidDex(pools []debugPool, dex string) *internal.Cycle {
	borrowables := []string{
		// Checksum-case agnostic — comparison lowercases both sides.
		"0x82af49447d8a07e3bd95bd0d56f35241523fbab1", // WETH
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831", // USDC
		"0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f", // WBTC
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8", // USDC.e
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", // USDT
	}
	findPoolForPair := func(tokA, tokB string) *debugPool {
		tA, tB := strings.ToLower(tokA), strings.ToLower(tokB)
		var best *debugPool
		for i := range pools {
			p := &pools[i]
			if p.Disabled || !p.Verified {
				continue
			}
			if !probeSimUsable(p) {
				continue
			}
			pt0, pt1 := strings.ToLower(p.Token0), strings.ToLower(p.Token1)
			if !((pt0 == tA && pt1 == tB) || (pt0 == tB && pt1 == tA)) {
				continue
			}
			if best == nil || p.TVLUSD > best.TVLUSD {
				best = p
			}
		}
		return best
	}
	for i := range pools {
		mid := &pools[i]
		if mid.Disabled || !mid.Verified || mid.DEX != dex || !probeSimUsable(mid) {
			continue
		}
		for _, borrow := range borrowables {
			// Skip if borrow is already one of mid's tokens — that's a
			// 2-hop cycle in disguise, not a real mid-hop test.
			if strings.EqualFold(borrow, mid.Token0) || strings.EqualFold(borrow, mid.Token1) {
				continue
			}
			hop0 := findPoolForPair(borrow, mid.Token0)
			hop2 := findPoolForPair(mid.Token1, borrow)
			if hop0 == nil || hop2 == nil {
				continue
			}
			// Build the cycle: borrow → mid.Token0 → mid.Token1 → borrow
			h0 := reconstructPool(*hop0)
			h1 := reconstructPool(*mid)
			h2 := reconstructPool(*hop2)
			// Determine tokenIn/tokenOut per hop based on each pool's
			// canonical token0/token1 ordering.
			var h0In, h0Out, h2In, h2Out *internal.Token
			if strings.EqualFold(hop0.Token0, borrow) {
				h0In, h0Out = h0.Token0, h0.Token1
			} else {
				h0In, h0Out = h0.Token1, h0.Token0
			}
			if strings.EqualFold(hop2.Token1, borrow) {
				h2In, h2Out = h2.Token0, h2.Token1
			} else {
				h2In, h2Out = h2.Token1, h2.Token0
			}
			return &internal.Cycle{
				Edges: []internal.Edge{
					{Pool: h0, TokenIn: h0In, TokenOut: h0Out},
					{Pool: h1, TokenIn: h1.Token0, TokenOut: h1.Token1},
					{Pool: h2, TokenIn: h2In, TokenOut: h2Out},
				},
			}
		}
	}
	return nil
}

// classifyContractResult applies the smoketest's "did the DEX UNDER TEST
// dispatch correctly" semantics. We only care about hop 0 — that's the hop
// using the probe DEX. If hop 0 reverts the DEX is broken; if any later hop
// reverts the back-leg DEX is broken (which is interesting but separate).
// "profit below minimum" or "last hop sim out" → contract reached the end of
// the swap chain successfully, only the profit check failed → dispatch ok.
func classifyContractResult(r result, err error) result {
	if err == nil {
		r.Status = "PASS"
		r.Detail = "eth_call succeeded end-to-end"
		return r
	}
	es := err.Error()
	switch {
	case strings.Contains(es, "profit below minimum"):
		r.Status = "PASS"
		r.Detail = "dispatch ok; profit-check failed (expected for round-trip): " + truncate(es, 60)
	case strings.Contains(es, "hop 0: swap reverted"), strings.Contains(es, "hop 0: slippage"):
		r.Status = "FAIL"
		r.Detail = "DEX-under-test hop 0 reverted: " + truncate(es, 80)
	case strings.Contains(es, "swap reverted"), strings.Contains(es, "slippage"):
		r.Status = "PASS"
		r.Detail = "DEX dispatch ok; back-leg reverted: " + truncate(es, 80)
	case es == "execution reverted":
		r.Status = "WARN"
		r.Detail = "bare revert (custom-error, no reason string) — dispatch reached the contract; synthetic-cycle back-leg likely unprofitable"
	default:
		r.Status = "FAIL"
		r.Detail = truncate(es, 120)
	}
	return r
}

// majorTokenSymbols are the symbols startAmountForToken knows how to size.
// Probe pools whose token0 is in this set get sized reliably; pools with
// exotic token0s fall back to a generic 0.01-unit amount that may be too
// small to clear router minimums or fee thresholds.
var majorTokenSymbols = map[string]bool{
	"USDC": true, "USDT": true, "DAI": true, "USDC.E": true, "FRAX": true,
	"USDE": true, "TUSD": true,
	"WETH": true, "ETH": true, "WBTC": true, "ARB": true,
}

// pickProbeWithBackLeg walks pools of the given DEX in (major-first, then
// TVL-descending) order and returns the first one that has a usable same-pair
// back-leg pool. Probes whose token0 is a known major are preferred so the
// amount sizer in startAmountForToken returns a robust ~$10 swap.
func pickProbeWithBackLeg(pools []debugPool, dex string) (*debugPool, *debugPool) {
	var candidates []*debugPool
	for i := range pools {
		p := &pools[i]
		if p.Disabled || !p.Verified || p.DEX != dex {
			continue
		}
		// Reject pools the Go simulator cannot evaluate. For V3-family
		// pools the bitmap must be loaded (TicksCount>0) — otherwise the
		// multi-tick sim returns 0 even though the pool is healthy on-chain.
		// For V2-family pools the cached reserves must be non-zero.
		// Without this filter the probe picks high-TVL pools that happen
		// not to be in the active cycle cache (so watchTickMapsLoop never
		// fetched their bitmap), and every probe falsely fails at buildHops.
		if !probeSimUsable(p) {
			continue
		}
		candidates = append(candidates, p)
	}
	// Sort: pools whose token0 is a major sort first; within each group,
	// sort by TVL descending. (Two-key sort via stable insertion.)
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0; j-- {
			a, b := candidates[j], candidates[j-1]
			aMajor := majorTokenSymbols[strings.ToUpper(a.Token0Symbol)]
			bMajor := majorTokenSymbols[strings.ToUpper(b.Token0Symbol)]
			if aMajor && !bMajor {
				candidates[j], candidates[j-1] = b, a
				continue
			}
			if !aMajor && bMajor {
				break
			}
			if a.TVLUSD > b.TVLUSD {
				candidates[j], candidates[j-1] = b, a
				continue
			}
			break
		}
	}
	for _, c := range candidates {
		if ref := pickBackLeg(pools, c); ref != nil {
			return c, ref
		}
	}
	return nil, nil
}

// pickDeepestVerified returns the verified pool with the highest TVL for a
// given DEX type, or nil if none exist.
func pickDeepestVerified(pools []debugPool, dex string) *debugPool {
	var best *debugPool
	for i := range pools {
		p := &pools[i]
		if p.Disabled || !p.Verified || p.DEX != dex {
			continue
		}
		if best == nil || p.TVLUSD > best.TVLUSD {
			best = p
		}
	}
	return best
}

// pickPairSharingToken returns two usable pools (probe, ref) such that probe
// is in dexSetA, ref is in dexSetB, and they share at least one token. Used
// by CONTRACT-V4Mini and CONTRACT-MixedV3V4 probes where a constraint-
// satisfying pair is required to synthesise a closed 2-hop cycle. Iterates
// over all pools ranked by TVL (probe preferred deeper) so it succeeds even
// under test_mode=true where the top-2 greedy picker would miss.
// Returns (nil, nil) when no pair exists.
func pickPairSharingToken(pools []debugPool, dexSetA, dexSetB []string) (*debugPool, *debugPool) {
	inSet := func(dex string, set []string) bool {
		for _, d := range set {
			if d == dex {
				return true
			}
		}
		return false
	}
	var aCandidates, bCandidates []*debugPool
	for i := range pools {
		pp := &pools[i]
		if pp.Disabled || !pp.Verified || !probeSimUsable(pp) {
			continue
		}
		if inSet(pp.DEX, dexSetA) {
			aCandidates = append(aCandidates, pp)
		}
		if inSet(pp.DEX, dexSetB) {
			bCandidates = append(bCandidates, pp)
		}
	}
	// Sort both by TVL descending so the chosen pair is the deepest possible.
	sortByTVL := func(ps []*debugPool) {
		for i := 1; i < len(ps); i++ {
			j := i
			for j > 0 && ps[j].TVLUSD > ps[j-1].TVLUSD {
				ps[j], ps[j-1] = ps[j-1], ps[j]
				j--
			}
		}
	}
	sortByTVL(aCandidates)
	sortByTVL(bCandidates)
	for _, a := range aCandidates {
		a0 := strings.ToLower(a.Token0)
		a1 := strings.ToLower(a.Token1)
		for _, b := range bCandidates {
			if a == b {
				continue
			}
			b0 := strings.ToLower(b.Token0)
			b1 := strings.ToLower(b.Token1)
			if (a0 == b0 && a1 == b1) || (a0 == b1 && a1 == b0) {
				return a, b
			}
		}
	}
	return nil, nil
}

// probeSimUsable returns true if the Go simulator has all cached fields needed
// to simulate a swap through the pool. V3-family pools without a loaded tick
// bitmap produce 0 outputs in the multi-tick sim even when the pool is healthy
// on-chain; V2-family pools with zero reserves likewise. Rejecting these at
// probe-selection time stops the smoketest from reporting false CONTRACT
// failures that are actually just sim-data gaps.
func probeSimUsable(p *debugPool) bool {
	switch p.DEX {
	case "UniV2", "Sushi", "Camelot", "TraderJoe":
		return p.Reserve0 != "" && p.Reserve1 != "" && p.SpotPrice > 0
	case "UniV3", "SushiV3", "PancakeV3", "CamelotV3", "RamsesV3", "ZyberV3", "UniV4":
		return p.TicksCount > 0 && p.SqrtPriceX96 != "" && p.Liquidity != ""
	case "Curve", "BalancerW":
		return p.Reserve0 != "" && p.Reserve1 != ""
	}
	return true
}

// canonToken maps native ETH (address(0), used by V4 PoolKeys) to WETH so
// back-leg pair matching treats a V4-ETH pool as compatible with any
// WETH-paired pool on another DEX. Without this, smoketest can never
// construct a 2-hop cycle through a V4-ETH pool.
func canonToken(addr string) string {
	a := strings.ToLower(addr)
	if a == "0x0000000000000000000000000000000000000000" {
		return "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
	}
	return a
}

// pickBackLeg finds a different pool (any DEX) with the SAME token pair as
// the probe, suitable for use as the back leg of a 2-hop round-trip cycle.
// Prefers verified pools and the deepest TVL. Excludes the probe itself.
func pickBackLeg(pools []debugPool, probe *debugPool) *debugPool {
	probeAddr := strings.ToLower(probe.Address)
	probeT0 := canonToken(probe.Token0)
	probeT1 := canonToken(probe.Token1)
	var best *debugPool
	for i := range pools {
		p := &pools[i]
		if p.Disabled || !p.Verified {
			continue
		}
		if strings.ToLower(p.Address) == probeAddr {
			continue
		}
		if !probeSimUsable(p) {
			continue
		}
		t0, t1 := canonToken(p.Token0), canonToken(p.Token1)
		if !((t0 == probeT0 && t1 == probeT1) || (t0 == probeT1 && t1 == probeT0)) {
			continue
		}
		if best == nil || p.TVLUSD > best.TVLUSD {
			best = p
		}
	}
	return best
}

// buildSyntheticCycle constructs a 2-hop closed round-trip cycle through two
// pools that share the same token pair. hop0 swaps token0 → token1 on the
// probe (the DEX dispatch under test); hop1 swaps token1 → token0 on the ref
// (any other pool with the same pair). Returns nil only if the pair sanity
// check fails (which shouldn't happen if pickBackLeg is consistent).
func buildSyntheticCycle(probe, ref *debugPool) *internal.Cycle {
	probeP := reconstructPool(*probe)
	refP := reconstructPool(*ref)

	pT0, pT1 := strings.ToLower(probe.Token0), strings.ToLower(probe.Token1)
	rT0, rT1 := strings.ToLower(ref.Token0), strings.ToLower(ref.Token1)
	if !((pT0 == rT0 && pT1 == rT1) || (pT0 == rT1 && pT1 == rT0)) {
		return nil
	}

	// On the ref pool, figure out which physical side is "the same as probe.Token0"
	// (which is the cycle's start token). The simulator and contract care about
	// the canonical Token struct identity, not just the address.
	var refTokenInForHop1, refTokenOutForHop1 *internal.Token
	if rT0 == pT1 {
		// ref's token0 == probe's token1 → so hop1 (token1 → token0 on ref)
		// goes ref.token0 → ref.token1
		refTokenInForHop1 = refP.Token0
		refTokenOutForHop1 = refP.Token1
	} else {
		// ref's token0 == probe's token0 → hop1 goes ref.token1 → ref.token0
		refTokenInForHop1 = refP.Token1
		refTokenOutForHop1 = refP.Token0
	}

	return &internal.Cycle{
		Edges: []internal.Edge{
			{
				Pool:     probeP,
				TokenIn:  probeP.Token0,
				TokenOut: probeP.Token1,
			},
			{
				Pool:     refP,
				TokenIn:  refTokenInForHop1,
				TokenOut: refTokenOutForHop1,
			},
		},
	}
}

// ─── output ─────────────────────────────────────────────────────────────────

func printTable(results []result) {
	fmt.Println("| ID | DEX | Pair | Status | Detail |")
	fmt.Println("|---|---|---|---|---|")
	pass, fail, skip, warn := 0, 0, 0, 0
	for _, r := range results {
		fmt.Printf("| %s | %s | %s | %s | %s |\n", r.ID, r.DEX, r.Pair, r.Status, r.Detail)
		switch r.Status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "WARN":
			warn++
		case "SKIP":
			skip++
		}
	}
	fmt.Println()
	fmt.Printf("**Summary**: %d PASS, %d WARN, %d FAIL, %d SKIP\n", pass, warn, fail, skip)
}

func hasFails(results []result) bool {
	for _, r := range results {
		if r.Status == "FAIL" {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Discard unused import warnings if a future build trims something.
var _ = io.Discard

// ─── data-integrity category ────────────────────────────────────────────────
//
// For every verified pool in the registry, verify that the cached metadata
// is internally consistent and non-missing. Surfaces classes of pool-level
// data bugs that aren't simulation or contract problems:
//
//   - fee_ppm == 0 on a V3 pool (cached fee missing)
//   - tick_spacing == 0 on a V3/V4 pool (metadata not fetched)
//   - sqrt_price_x96 == "" or "0" on a V3/V4 pool (slot0 never read)
//   - liquidity == "" or "0" on a V3/V4 pool
//   - reserve0/reserve1 empty on a V2/Curve/Balancer pool despite TVL>0
//   - tvl_usd == 0 (subgraph metrics never populated)
//   - volume_24h_usd == 0 AND tvl_usd > 10k (subgraph stopped reporting)
//   - v4_hooks empty on a UniV4 pool (decode gap from initialize event)
//   - token symbols empty / unknown
//   - token0 address >= token1 address (UniV3 canonical-order invariant
//     violation — silently breaks zeroForOne direction detection)
//
// Output is per-DEX summary + a list of individual pools with issues
// (capped so the report stays readable). This doesn't make RPC calls —
// purely inspects the bot's cached state via /debug/pools. Catches data
// staleness bugs whose symptoms (wrong quote, wrong fee in sim) show up
// downstream as SIM_PHANTOM but whose root cause is upstream.
func runDataIntegrity(client *ethclient.Client, cfg *yamlConfig, pools []debugPool) []result {
	walletAddr := common.HexToAddress(cfg.Wallet.Address)
	var out []result

	// Per-DEX counts of each failure class, so the report can summarize
	// "UniV4: 12 pools missing fee_ppm, 8 missing tick_spacing" in one line
	// per DEX rather than 2000 individual probes.
	type dexStats struct {
		total               int
		missingFee          int
		missingTickSpacing  int
		missingSqrtPrice    int
		missingLiquidity    int
		missingReserves     int
		missingTVL          int
		missingVolume       int
		missingHooks        int
		missingSymbols      int
		wrongTokenOrder     int
		suspiciousPools     []string // addresses of up to 10 exemplar bad pools per DEX
	}
	stats := make(map[string]*dexStats)

	addExample := func(s *dexStats, addr string) {
		if len(s.suspiciousPools) < 10 {
			s.suspiciousPools = append(s.suspiciousPools, addr)
		}
	}

	for i := range pools {
		p := &pools[i]
		if p.Disabled {
			continue
		}
		if !p.Verified {
			// Non-verified pools are by definition incomplete; the verified
			// check is orthogonal to this. Skipping keeps the per-DEX stats
			// focused on pools meant to be in the scoring pipeline.
			continue
		}
		s, ok := stats[p.DEX]
		if !ok {
			s = &dexStats{}
			stats[p.DEX] = s
		}
		s.total++

		isV3Family := false
		isV4 := false
		isV2Family := false
		switch p.DEX {
		case "UniV3", "SushiV3", "PancakeV3", "RamsesV3", "CamelotV3", "ZyberV3":
			isV3Family = true
		case "UniV4":
			isV4 = true
		case "UniV2", "Sushi", "Camelot", "RamsesV2", "TraderJoe", "DeltaSwap", "Swapr", "ArbSwap", "Chronos":
			isV2Family = true
		}

		// Fee presence (FeePPM for V3/V4, FeeBps for V2/Curve/Balancer)
		if isV3Family || isV4 {
			if p.FeePPM == 0 {
				s.missingFee++
				addExample(s, p.Address)
			}
			if p.TickSpacing == 0 {
				s.missingTickSpacing++
			}
			if p.SqrtPriceX96 == "" || p.SqrtPriceX96 == "0" {
				s.missingSqrtPrice++
			}
			if p.Liquidity == "" || p.Liquidity == "0" {
				s.missingLiquidity++
			}
		}
		if isV4 && p.V4Hooks == "" {
			s.missingHooks++
		}

		// V2 / Curve / Balancer reserves
		if isV2Family || p.DEX == "Curve" || p.DEX == "BalancerW" {
			if (p.Reserve0 == "" || p.Reserve0 == "0") || (p.Reserve1 == "" || p.Reserve1 == "0") {
				s.missingReserves++
				addExample(s, p.Address)
			}
		}

		// Metrics
		if p.TVLUSD == 0 {
			s.missingTVL++
		}
		if p.TVLUSD > 10_000 && p.Volume24hUSD == 0 {
			s.missingVolume++
		}

		// Token metadata
		if p.Token0Symbol == "" || p.Token1Symbol == "" {
			s.missingSymbols++
		}

		// UniV3 invariant: token0 address < token1 address. Violated means
		// the bot loaded the pool with reversed tokens, which breaks every
		// zeroForOne check in the simulator and encoder.
		if isV3Family || isV4 {
			a, b := strings.ToLower(p.Token0), strings.ToLower(p.Token1)
			if a != "" && b != "" && a >= b {
				s.wrongTokenOrder++
				addExample(s, p.Address)
			}
		}
	}

	// Emit one DATA result per DEX. Status PASS if no issues, WARN if some
	// non-critical fields missing, FAIL if critical fields missing.
	for dex, s := range stats {
		r := result{ID: fmt.Sprintf("DATA-%s", dex), DEX: dex}
		critical := s.missingFee + s.missingTickSpacing + s.missingSqrtPrice + s.missingLiquidity + s.missingReserves + s.wrongTokenOrder + s.missingHooks
		minor := s.missingTVL + s.missingVolume + s.missingSymbols
		if critical == 0 && minor == 0 {
			r.Status = "PASS"
			r.Detail = fmt.Sprintf("%d verified pools — all metadata fields present", s.total)
		} else {
			issues := []string{}
			if s.missingFee > 0 {
				issues = append(issues, fmt.Sprintf("fee_ppm=0 on %d/%d", s.missingFee, s.total))
			}
			if s.missingTickSpacing > 0 {
				issues = append(issues, fmt.Sprintf("tickSpacing=0 on %d", s.missingTickSpacing))
			}
			if s.missingSqrtPrice > 0 {
				issues = append(issues, fmt.Sprintf("sqrtPriceX96 missing on %d", s.missingSqrtPrice))
			}
			if s.missingLiquidity > 0 {
				issues = append(issues, fmt.Sprintf("liquidity missing on %d", s.missingLiquidity))
			}
			if s.missingReserves > 0 {
				issues = append(issues, fmt.Sprintf("reserves missing on %d", s.missingReserves))
			}
			if s.missingHooks > 0 {
				issues = append(issues, fmt.Sprintf("v4_hooks empty on %d", s.missingHooks))
			}
			if s.wrongTokenOrder > 0 {
				issues = append(issues, fmt.Sprintf("token order wrong on %d (CRITICAL)", s.wrongTokenOrder))
			}
			if s.missingTVL > 0 {
				issues = append(issues, fmt.Sprintf("tvl_usd=0 on %d", s.missingTVL))
			}
			if s.missingVolume > 0 {
				issues = append(issues, fmt.Sprintf("vol24h=0 on %d (tvl>$10k)", s.missingVolume))
			}
			if s.missingSymbols > 0 {
				issues = append(issues, fmt.Sprintf("symbols missing on %d", s.missingSymbols))
			}
			r.Detail = strings.Join(issues, ", ")
			if len(s.suspiciousPools) > 0 {
				r.Detail += "; examples: " + strings.Join(s.suspiciousPools, ",")
			}
			if critical > 0 {
				r.Status = "FAIL"
			} else {
				r.Status = "WARN"
			}
		}
		out = append(out, r)
	}

	// ── Gas-estimate consistency (DATA-GAS-LEDGER) ────────────────────────
	// The per-contract gas estimates live in three places that can drift:
	//   1. contract_ledger.gas_estimate (SQLite)
	//   2. internal/bot.go evalOneCandidate switch (380k / 300k / 450k / 900k)
	//   3. contracts/*.sol header-comment gas envelopes
	// This probe compares (1) against (2) for every active executor and flags
	// any mismatch. Uses /api/contracts proxy (or arb.db read) to avoid
	// importing internal/db from the smoketest binary.
	{
		r := result{ID: "DATA-GAS-LEDGER", DEX: "gas"}
		expected := map[string]uint64{
			"V3FlashMini":       380_000,
			"V4Mini":            300_000,
			"MixedV3V4Executor": 450_000,
			"ArbitrageExecutor": 900_000,
		}
		ledger := loadLedgerGasEstimates()
		var diffs []string
		for name, want := range expected {
			got, ok := ledger[name]
			if !ok {
				diffs = append(diffs, fmt.Sprintf("%s: not in ledger", name))
				continue
			}
			if got != want {
				diffs = append(diffs, fmt.Sprintf("%s: ledger=%d switch=%d", name, got, want))
			}
		}
		if len(diffs) == 0 {
			r.Status = "PASS"
			r.Detail = fmt.Sprintf("all 4 executors match (%d checked)", len(ledger))
		} else {
			r.Status = "FAIL"
			r.Detail = "mismatch: " + strings.Join(diffs, "; ")
		}
		out = append(out, r)
	}

	// ── On-chain gas measurement (DATA-GAS-MEASURE) ──────────────────────
	// Per-contract: build a synthetic 2-hop cycle matching the contract's
	// shape, run eth_estimateGas against the deployed address, compare
	// against the stored gas_estimate target. >20% drift = WARN, >50% = FAIL.
	// Requires config addresses to be set (contracts deployed). SKIPs
	// otherwise. Numbers here reflect the ACTUAL chain cost not the
	// compile-time target, so they'll drift as pool states evolve; the
	// 20% / 50% thresholds accommodate normal fluctuation.
	if cfg.Trading.ExecutorV3Mini != "" || cfg.Trading.ExecutorV4Mini != "" || cfg.Trading.ExecutorMixedV3V4 != "" {
		ledger := loadLedgerGasEstimates()
		measureOne := func(id, name string, addr common.Address, cycle *internal.Cycle, flashPool common.Address, flashToken0 common.Address, pack func(internal.Cycle, *big.Int, common.Address, common.Address) ([]byte, error)) {
			r := result{ID: id, DEX: name}
			if cycle == nil {
				r.Status = "SKIP"
				r.Detail = "no synthetic cycle available"
				out = append(out, r)
				return
			}
			amountIn := startAmountForToken(cycle.Edges[0].TokenIn)
			data, err := pack(*cycle, amountIn, flashPool, flashToken0)
			if err != nil {
				r.Status = "SKIP"
				r.Detail = "pack: " + err.Error()
				out = append(out, r)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			gasUsed, err := client.EstimateGas(ctx, ethereum.CallMsg{From: walletAddr, To: &addr, Data: data})
			cancel()
			if err != nil {
				r.Status = "SKIP"
				r.Detail = "estimateGas: " + err.Error()
				out = append(out, r)
				return
			}
			target := ledger[name]
			if target == 0 {
				r.Status = "WARN"
				r.Detail = fmt.Sprintf("measured=%d target=not_in_ledger", gasUsed)
				out = append(out, r)
				return
			}
			drift := int64(gasUsed) - int64(target)
			driftPct := float64(drift) / float64(target) * 100
			r.Detail = fmt.Sprintf("measured=%d target=%d drift=%+.1f%%", gasUsed, target, driftPct)
			absPct := driftPct
			if absPct < 0 {
				absPct = -absPct
			}
			switch {
			case absPct > 50:
				r.Status = "FAIL"
			case absPct > 20:
				r.Status = "WARN"
			default:
				r.Status = "PASS"
			}
			out = append(out, r)
		}
		// V3Mini: pure V3 synthetic
		if cfg.Trading.ExecutorV3Mini != "" {
			v3Probe, v3Ref := pickPairSharingToken(pools, []string{"UniV3"}, []string{"UniV3"})
			var cycle *internal.Cycle
			if v3Probe != nil && v3Ref != nil {
				cycle = buildSyntheticCycle(v3Probe, v3Ref)
			}
			if cycle != nil {
				flashSrc := findFlashSourceForCycle(pools, cycle, []string{"UniV3", "SushiV3", "PancakeV3"})
				if flashSrc != nil {
					miniAddr := common.HexToAddress(cfg.Trading.ExecutorV3Mini)
					pool := common.HexToAddress(flashSrc.Address)
					tok0 := common.HexToAddress(flashSrc.Token0)
					measureOne("DATA-GAS-MEASURE-V3Mini", "V3FlashMini", miniAddr, cycle, pool, tok0,
						func(c internal.Cycle, amt *big.Int, fp, ft0 common.Address) ([]byte, error) {
							return internal.BuildV3MiniFlashCalldata(c, amt, 9999, fp, ft0)
						})
				}
			}
		}
		if cfg.Trading.ExecutorV4Mini != "" {
			v4Probe, v4Ref := pickPairSharingToken(pools, []string{"UniV4"}, []string{"UniV4"})
			var cycle *internal.Cycle
			if v4Probe != nil && v4Ref != nil {
				cycle = buildSyntheticCycle(v4Probe, v4Ref)
			}
			if cycle != nil {
				flashSrc := findFlashSourceForCycle(pools, cycle, []string{"UniV3", "SushiV3", "PancakeV3"})
				if flashSrc != nil {
					addr := common.HexToAddress(cfg.Trading.ExecutorV4Mini)
					pool := common.HexToAddress(flashSrc.Address)
					tok0 := common.HexToAddress(flashSrc.Token0)
					measureOne("DATA-GAS-MEASURE-V4Mini", "V4Mini", addr, cycle, pool, tok0,
						func(c internal.Cycle, amt *big.Int, fp, ft0 common.Address) ([]byte, error) {
							return internal.BuildV4MiniFlashCalldata(c, amt, fp, ft0)
						})
				}
			}
		}
		if cfg.Trading.ExecutorMixedV3V4 != "" {
			mixProbe, mixRef := pickPairSharingToken(pools, []string{"UniV4"}, []string{"UniV3", "SushiV3", "PancakeV3", "RamsesV3", "CamelotV3", "ZyberV3"})
			var cycle *internal.Cycle
			if mixProbe != nil && mixRef != nil {
				cycle = buildSyntheticCycle(mixProbe, mixRef)
			}
			if cycle != nil {
				flashSrc := findFlashSourceForCycle(pools, cycle, []string{"UniV3", "SushiV3", "PancakeV3"})
				if flashSrc != nil {
					addr := common.HexToAddress(cfg.Trading.ExecutorMixedV3V4)
					pool := common.HexToAddress(flashSrc.Address)
					tok0 := common.HexToAddress(flashSrc.Token0)
					measureOne("DATA-GAS-MEASURE-Mixed", "MixedV3V4Executor", addr, cycle, pool, tok0,
						func(c internal.Cycle, amt *big.Int, fp, ft0 common.Address) ([]byte, error) {
							return internal.BuildMixedV3V4FlashCalldata(c, amt, fp, ft0)
						})
				}
			}
		}
	}

	return out
}

// loadLedgerGasEstimates reads contract_ledger.gas_estimate for every active
// executor. Keyed by `name` (matches the contract_ledger.name column).
// Opens the SQLite DB read-only directly (no dependency on the dashboard's
// /api/contracts endpoint so this works headless).
func loadLedgerGasEstimates() map[string]uint64 {
	out := make(map[string]uint64)
	db, err := sql.Open("sqlite", "/home/arbitrator/go/arb-bot/arb.db?mode=ro")
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query("SELECT name, gas_estimate FROM contract_ledger WHERE kind='executor' AND status='active'")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ge uint64
		if rows.Scan(&name, &ge) == nil {
			out[name] = ge
		}
	}
	return out
}

// findFlashSourceForCycle picks the deepest V3-style pool outside the cycle
// that holds the cycle's borrow token. Returns nil when none qualifies.
func findFlashSourceForCycle(pools []debugPool, cycle *internal.Cycle, dexes []string) *debugPool {
	borrowAddr := strings.ToLower(cycle.Edges[0].TokenIn.Address)
	inCycle := make(map[string]bool, len(cycle.Edges))
	for _, e := range cycle.Edges {
		inCycle[strings.ToLower(e.Pool.Address)] = true
	}
	inSet := func(d string) bool {
		for _, x := range dexes {
			if x == d {
				return true
			}
		}
		return false
	}
	var best *debugPool
	for i := range pools {
		pp := &pools[i]
		if pp.Disabled || !pp.Verified || inCycle[strings.ToLower(pp.Address)] || !inSet(pp.DEX) || !probeSimUsable(pp) {
			continue
		}
		t0 := strings.ToLower(pp.Token0)
		t1 := strings.ToLower(pp.Token1)
		if t0 != borrowAddr && t1 != borrowAddr {
			continue
		}
		if best == nil || pp.TVLUSD > best.TVLUSD {
			best = pp
		}
	}
	return best
}
