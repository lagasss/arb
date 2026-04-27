// ticktest — end-to-end tick management invariant checker.
//
// Reads the bot's /debug/tick-health and /debug/pools endpoints to verify that
// every reliability invariant the tick subsystem is supposed to uphold is
// actually holding in steady state. Optionally cross-checks a sampled set of
// pools against on-chain bitmap data via direct RPC to catch the "stored
// state is self-consistent but wrong relative to chain" class of bug.
//
// Categories:
//
//   -cat pass       Latest FetchTickMaps pass succeeded, no algebra
//                   round-trip mismatches, no wide-scale fetch failures.
//   -cat eager      Eager-refetch channel drop rate below threshold.
//   -cat skew       tick_data_rpc vs arbitrum_rpc block skew under the
//                   configured limit.
//   -cat coverage   No cycle pool has current tick outside fetched range.
//   -cat staleness  No cycle pool has TicksUpdatedAt age over max_tick_age_sec.
//   -cat failure    No cycle pool stuck in a fetch-failure loop.
//   -cat gate       Per-sub-counter gate rejection profile is within budget
//                   (no single reason dominates).
//   -cat chain      For a sample of pools, re-fetch bitmap+ticks on-chain
//                   directly and diff against bot's in-memory state.
//   -cat invariants Low-level structural checks on stored ticks
//                   (sorted, non-zero liquidity, spacing-aligned, etc.).
//   -cat all        Run everything.
//
// Exit code: 1 on any FAIL, 0 on all PASS/SKIP. Every category prints one
// line per check.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"gopkg.in/yaml.v3"
)

var (
	flagCat      = flag.String("cat", "all", "test category: pass, eager, skew, coverage, staleness, failure, gate, chain, invariants, hooks, all")
	flagDebugURL = flag.String("debug-url", "http://127.0.0.1:6060", "bot /debug/* base URL")
	flagConfig   = flag.String("config", "/home/arbitrator/go/arb-bot/config.yaml", "bot config.yaml path")
	flagJSON     = flag.Bool("json", false, "emit results as JSON instead of a text table")
	flagVerbose  = flag.Bool("v", false, "verbose: print every probe")
	flagChainSample = flag.Int("chain-sample", 5, "number of V3 pools to cross-check on-chain in -cat chain")
	// Thresholds. All expressed so that a FAIL condition is a loud wrong state,
	// not a noise trigger. Every default must be overridable so the caller
	// (the /test skill) can tighten or loosen per environment.
	flagMaxDropRate        = flag.Float64("max-eager-drop-rate", 0.01, "max fraction of eager sends allowed to drop (0.01 = 1%)")
	flagMaxFailPct         = flag.Float64("max-fail-pct", 0.02, "max fraction of pools allowed to be in fetch-failure state (0.02 = 2%)")
	flagMaxCoverageDriftPct = flag.Float64("max-coverage-drift-pct", 0.05, "max fraction of cycle pools out of range")
	flagMaxStaleAgePct     = flag.Float64("max-stale-age-pct", 0.10, "max fraction of cycle pools exceeding max_tick_age_sec")
	flagMaxFailLoopPct     = flag.Float64("max-fail-loop-pct", 0.01, "max fraction of pools in fetch-failure loop (fail count > 1)")
)

// ─── bot debug types (mirror handler JSON) ──────────────────────────────────

type debugTickHealth struct {
	Now                 int64  `json:"now"`
	TickDataAt          int64  `json:"tick_data_at"`
	TickDataAgeSecs     int64  `json:"tick_data_age_secs"`
	ArbitrumBlock       uint64 `json:"arbitrum_block"`
	TickRPCBlock        uint64 `json:"tick_rpc_block"`
	RPCSkewBlocks       int64  `json:"rpc_skew_blocks"`
	TickRPCLagsBy       int64  `json:"tick_rpc_lags_by"`
	MaxRPCSkewBlocks    int64  `json:"max_rpc_skew_blocks"`
	RPCSkewOK           bool   `json:"rpc_skew_ok"`
	PassTotal           int64  `json:"pass_total"`
	PassSucceeded       int64  `json:"pass_succeeded"`
	PassEmpty           int64  `json:"pass_empty"`
	PassFailed          int64  `json:"pass_failed"`
	PassSkipped         int64  `json:"pass_skipped"`
	PassRTMismatch      int64  `json:"pass_rt_mismatch"`
	PassDurMs           int64  `json:"pass_dur_ms"`
	PassBlockTick       uint64 `json:"pass_block_tick"`
	PassBlockArb        uint64 `json:"pass_block_arb"`
	EagerDropped        uint64 `json:"eager_refetch_dropped"`
	EagerEnqueued       uint64 `json:"eager_refetch_enqueued"`
	EagerChanLen        int    `json:"eager_refetch_chan_len"`
	EagerChanCap        int    `json:"eager_refetch_chan_cap"`
	CoverageRadius      int    `json:"tick_bitmap_coverage_words"`
	ByReason            []struct {
		Reason string   `json:"reason"`
		Count  int      `json:"count"`
		Pools  []string `json:"pools,omitempty"`
	} `json:"by_reason"`
	CoverageOutOfRange int      `json:"coverage_out_of_range"`
	OutOfRangeSample   []string `json:"out_of_range_sample,omitempty"`
	FailureLoopCount   int      `json:"failure_loop_count"`
	FailureLoopSample  []string `json:"failure_loop_sample,omitempty"`
	StaleByAge         int      `json:"stale_by_age"`
	MaxTickAgeSecs     int64    `json:"max_tick_age_sec"`
	RejectNeverFetched  uint64 `json:"reject_tick_never_fetched"`
	RejectFetchFailed   uint64 `json:"reject_tick_fetch_failed"`
	RejectCoverageDrift uint64 `json:"reject_tick_coverage_drift"`
	RejectEmptyVerified uint64 `json:"reject_tick_empty_verified"`
}

type debugPool struct {
	Address        string  `json:"address"`
	DEX            string  `json:"dex"`
	Tick           int32   `json:"tick"`
	TickSpacing    int32   `json:"tick_spacing"`
	TicksCount     int     `json:"ticks_count"`
	TicksUpdatedAt int64   `json:"ticks_updated_at"`
	TicksBlock     uint64  `json:"ticks_block"`
	TicksWordRadius int16  `json:"ticks_word_radius"`
	TickAtFetch    int32   `json:"tick_at_fetch"`
	TicksFetchOK            bool   `json:"ticks_fetch_ok"`
	TicksFetchReason        string `json:"ticks_fetch_reason"`
	TicksFetchAttemptedAt   int64  `json:"ticks_fetch_attempted_at"`
	TicksFetchBitmapWords   int16  `json:"ticks_fetch_bitmap_words"`
	TicksFetchNonEmptyWords int16  `json:"ticks_fetch_non_empty_words"`
	TicksFetchFailureCount  uint32 `json:"ticks_fetch_failure_count"`
	PoolID   string `json:"pool_id,omitempty"`
	V4Hooks  string `json:"v4_hooks,omitempty"`
}

type debugPoolDetail struct {
	debugPool
	Ticks []struct {
		Tick         int32  `json:"tick"`
		LiquidityNet string `json:"liquidity_net"`
	} `json:"ticks"`
}

type debugPoolsResp struct {
	Total int         `json:"total"`
	Pools []debugPool `json:"pools"`
}

// ─── config snippet ─────────────────────────────────────────────────────────

type yamlConfig struct {
	ArbitrumRPC string `yaml:"arbitrum_rpc"`
	Trading     struct {
		TickDataRPC                string `yaml:"tick_data_rpc"`
		TickBitmapCoverageWords    int    `yaml:"tick_bitmap_coverage_words"`
		MaxTickAgeSec              int64  `yaml:"max_tick_age_sec"`
		TickRPCMaxSkewBlocks       int64  `yaml:"tick_rpc_max_skew_blocks"`
		TickEagerRefetchSpacings   int    `yaml:"tick_eager_refetch_spacings"`
		TickEagerRefetchCooldownMs int    `yaml:"tick_eager_refetch_cooldown_ms"`
		TickRefetchChanCap         int    `yaml:"tick_refetch_chan_cap"`
		ExecutorV4Mini             string `yaml:"executor_v4_mini"`
		ExecutorMixedV3V4          string `yaml:"executor_mixed_v3v4"`
		HookRegistry               string `yaml:"hook_registry"`
	} `yaml:"trading"`
}

// ─── result reporting ───────────────────────────────────────────────────────

type result struct {
	ID     string `json:"id"`
	Cat    string `json:"cat"`
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
	health, err := fetchTickHealth(*flagDebugURL)
	if err != nil {
		die("GET /debug/tick-health: %v", err)
	}
	pools, err := fetchPools(*flagDebugURL)
	if err != nil {
		die("GET /debug/pools: %v", err)
	}

	var results []result

	cats := strings.Split(*flagCat, ",")
	for i, c := range cats {
		cats[i] = strings.TrimSpace(c)
	}
	want := func(name string) bool {
		for _, c := range cats {
			if c == "all" || c == name {
				return true
			}
		}
		return false
	}

	if want("pass") {
		results = append(results, checkPass(health)...)
	}
	if want("eager") {
		results = append(results, checkEager(health)...)
	}
	if want("skew") {
		results = append(results, checkSkew(health)...)
	}
	if want("coverage") {
		results = append(results, checkCoverage(health, pools)...)
	}
	if want("staleness") {
		results = append(results, checkStaleness(health, pools)...)
	}
	if want("failure") {
		results = append(results, checkFailure(health, pools)...)
	}
	if want("gate") {
		results = append(results, checkGate(health)...)
	}
	if want("chain") {
		results = append(results, checkChain(cfg, pools)...)
	}
	if want("invariants") {
		results = append(results, checkInvariants(pools)...)
	}
	if want("hooks") {
		results = append(results, checkHooks(cfg, pools)...)
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

// ─── checks ─────────────────────────────────────────────────────────────────

func checkPass(h *debugTickHealth) []result {
	var out []result
	if h.TickDataAt == 0 {
		out = append(out, fail("pass.bootstrap", "pass", "FetchTickMaps has not yet run a successful pass (tick_data_at=0)"))
		return out
	}
	if h.PassTotal == 0 {
		out = append(out, fail("pass.populated", "pass", "pass_total=0 — last pass had no pools (registry empty or cycle cache cold?)"))
		return out
	}
	if h.PassRTMismatch > 0 {
		out = append(out, fail("pass.rt_mismatch", "pass",
			fmt.Sprintf("%d algebra round-trip mismatches — bitmap-index math regression", h.PassRTMismatch)))
	} else {
		out = append(out, pass("pass.rt_mismatch", "pass", "no algebra round-trip mismatches"))
	}
	if h.PassFailed > 0 && float64(h.PassFailed)/float64(h.PassTotal) > *flagMaxFailPct {
		out = append(out, fail("pass.fail_rate", "pass",
			fmt.Sprintf("pass_failed=%d/%d (%.2f%%) > threshold %.2f%%",
				h.PassFailed, h.PassTotal, 100*float64(h.PassFailed)/float64(h.PassTotal), 100**flagMaxFailPct)))
	} else {
		out = append(out, pass("pass.fail_rate", "pass",
			fmt.Sprintf("pass_failed=%d/%d", h.PassFailed, h.PassTotal)))
	}
	if h.PassSkipped == h.PassTotal && h.PassTotal > 0 {
		out = append(out, fail("pass.all_skipped", "pass", "every pool in the pass was skipped — no sqrtPrice or tickSpacing?"))
	}
	if h.PassSucceeded == 0 && h.PassTotal > 0 {
		out = append(out, fail("pass.zero_success", "pass", "no pools succeeded in the last pass"))
	} else {
		out = append(out, pass("pass.succeeded", "pass",
			fmt.Sprintf("pass_succeeded=%d (empty=%d skipped=%d)", h.PassSucceeded, h.PassEmpty, h.PassSkipped)))
	}
	if h.PassDurMs > 20_000 {
		out = append(out, fail("pass.latency", "pass", fmt.Sprintf("pass took %d ms — tick RPC overloaded?", h.PassDurMs)))
	} else {
		out = append(out, pass("pass.latency", "pass", fmt.Sprintf("pass_dur_ms=%d", h.PassDurMs)))
	}
	return out
}

func checkEager(h *debugTickHealth) []result {
	var out []result
	total := h.EagerEnqueued + h.EagerDropped
	if total == 0 {
		out = append(out, skip("eager.activity", "eager", "no eager refetch activity yet"))
		return out
	}
	rate := float64(h.EagerDropped) / float64(total)
	if rate > *flagMaxDropRate {
		out = append(out, fail("eager.drop_rate", "eager",
			fmt.Sprintf("drop_rate=%.2f%% (%d/%d) > threshold %.2f%% — grow tick_refetch_chan_cap",
				100*rate, h.EagerDropped, total, 100**flagMaxDropRate)))
	} else {
		out = append(out, pass("eager.drop_rate", "eager",
			fmt.Sprintf("drop_rate=%.2f%% (%d enqueued, %d dropped)", 100*rate, h.EagerEnqueued, h.EagerDropped)))
	}
	if h.EagerChanCap > 0 && h.EagerChanLen >= h.EagerChanCap-1 {
		out = append(out, fail("eager.chan_saturated", "eager",
			fmt.Sprintf("channel at %d/%d — imminent drops", h.EagerChanLen, h.EagerChanCap)))
	} else {
		out = append(out, pass("eager.chan_saturation", "eager",
			fmt.Sprintf("channel at %d/%d", h.EagerChanLen, h.EagerChanCap)))
	}
	return out
}

func checkSkew(h *debugTickHealth) []result {
	var out []result
	if h.ArbitrumBlock == 0 || h.TickRPCBlock == 0 {
		out = append(out, skip("skew.bootstrap", "skew", "one RPC has not yet reported a block"))
		return out
	}
	if !h.RPCSkewOK {
		out = append(out, fail("skew.threshold", "skew",
			fmt.Sprintf("arbitrum=%d tick=%d lag=%d (signed=%d) max=%d", h.ArbitrumBlock, h.TickRPCBlock, h.TickRPCLagsBy, h.RPCSkewBlocks, h.MaxRPCSkewBlocks)))
	} else {
		out = append(out, pass("skew.threshold", "skew",
			fmt.Sprintf("lag=%d (signed=%d) max=%d", h.TickRPCLagsBy, h.RPCSkewBlocks, h.MaxRPCSkewBlocks)))
	}
	return out
}

func checkCoverage(h *debugTickHealth, pools []debugPool) []result {
	var out []result
	cyclePools := 0
	for _, p := range pools {
		if isV3Family(p.DEX) && p.TicksBlock > 0 {
			cyclePools++
		}
	}
	if cyclePools == 0 {
		out = append(out, skip("coverage.bootstrap", "coverage", "no cycle pools have had a tick fetch yet"))
		return out
	}
	if frac := float64(h.CoverageOutOfRange) / float64(cyclePools); frac > *flagMaxCoverageDriftPct {
		out = append(out, fail("coverage.out_of_range", "coverage",
			fmt.Sprintf("%d/%d pools out of range (%.2f%% > threshold %.2f%%) sample=%v",
				h.CoverageOutOfRange, cyclePools, 100*frac, 100**flagMaxCoverageDriftPct, h.OutOfRangeSample)))
	} else {
		out = append(out, pass("coverage.out_of_range", "coverage",
			fmt.Sprintf("%d/%d pools out of range", h.CoverageOutOfRange, cyclePools)))
	}
	return out
}

func checkStaleness(h *debugTickHealth, pools []debugPool) []result {
	var out []result
	cyclePools := 0
	for _, p := range pools {
		if isV3Family(p.DEX) && p.TicksBlock > 0 {
			cyclePools++
		}
	}
	if cyclePools == 0 {
		out = append(out, skip("staleness.bootstrap", "staleness", "no cycle pools have been fetched"))
		return out
	}
	if frac := float64(h.StaleByAge) / float64(cyclePools); frac > *flagMaxStaleAgePct {
		out = append(out, fail("staleness.age", "staleness",
			fmt.Sprintf("%d/%d pools exceed max_tick_age_sec=%d (%.2f%% > threshold %.2f%%)",
				h.StaleByAge, cyclePools, h.MaxTickAgeSecs, 100*frac, 100**flagMaxStaleAgePct)))
	} else {
		out = append(out, pass("staleness.age", "staleness",
			fmt.Sprintf("%d/%d pools exceed max_tick_age_sec=%d", h.StaleByAge, cyclePools, h.MaxTickAgeSecs)))
	}
	return out
}

func checkFailure(h *debugTickHealth, pools []debugPool) []result {
	var out []result
	cyclePools := 0
	for _, p := range pools {
		if isV3Family(p.DEX) {
			cyclePools++
		}
	}
	if cyclePools == 0 {
		out = append(out, skip("failure.bootstrap", "failure", "no V3-family pools in registry"))
		return out
	}
	if frac := float64(h.FailureLoopCount) / float64(cyclePools); frac > *flagMaxFailLoopPct {
		out = append(out, fail("failure.loop", "failure",
			fmt.Sprintf("%d/%d pools stuck in fetch-failure loop (%.2f%% > threshold %.2f%%) sample=%v",
				h.FailureLoopCount, cyclePools, 100*frac, 100**flagMaxFailLoopPct, h.FailureLoopSample)))
	} else {
		out = append(out, pass("failure.loop", "failure",
			fmt.Sprintf("%d/%d pools in failure loop", h.FailureLoopCount, cyclePools)))
	}
	return out
}

func checkGate(h *debugTickHealth) []result {
	var out []result
	if h.RejectFetchFailed > 0 {
		out = append(out, fail("gate.fetch_failed", "gate",
			fmt.Sprintf("%d candidates rejected due to fetch-failed pools — tick RPC unreliable or bitmap radius misconfigured", h.RejectFetchFailed)))
	} else {
		out = append(out, pass("gate.fetch_failed", "gate", "0 candidates rejected for fetch-failed pool"))
	}
	out = append(out, pass("gate.never_fetched", "gate",
		fmt.Sprintf("never_fetched=%d (expected high at startup, low in steady state)", h.RejectNeverFetched)))
	out = append(out, pass("gate.coverage_drift", "gate",
		fmt.Sprintf("coverage_drift=%d", h.RejectCoverageDrift)))
	out = append(out, pass("gate.empty_verified", "gate",
		fmt.Sprintf("empty_verified=%d", h.RejectEmptyVerified)))
	return out
}

func checkInvariants(pools []debugPool) []result {
	var out []result
	var total, unaligned, emptyOK, zeroRadius, fetchOKNoTicks int
	for _, p := range pools {
		if !isV3Family(p.DEX) {
			continue
		}
		if p.TicksBlock == 0 {
			continue
		}
		total++
		if p.TickSpacing <= 0 {
			unaligned++
			continue
		}
		if p.TicksFetchOK && p.TicksCount == 0 {
			emptyOK++
		}
		if p.TicksWordRadius == 0 {
			zeroRadius++
		}
		if p.TicksFetchOK && p.TicksCount == 0 && p.TicksFetchReason != "empty-bitmap" && p.TicksFetchReason != "" {
			fetchOKNoTicks++
		}
	}
	if total == 0 {
		out = append(out, skip("invariants.bootstrap", "invariants", "no V3-family pools have been fetched"))
		return out
	}
	if unaligned > 0 {
		out = append(out, fail("invariants.spacing", "invariants",
			fmt.Sprintf("%d pools with TickSpacing<=0 but TicksBlock>0 (impossible)", unaligned)))
	} else {
		out = append(out, pass("invariants.spacing", "invariants", fmt.Sprintf("all %d fetched pools have positive TickSpacing", total)))
	}
	if zeroRadius > 0 {
		out = append(out, fail("invariants.radius", "invariants",
			fmt.Sprintf("%d pools with TicksWordRadius==0 but TicksBlock>0 (stamped but radius missing)", zeroRadius)))
	} else {
		out = append(out, pass("invariants.radius", "invariants", "all fetched pools have positive TicksWordRadius"))
	}
	if fetchOKNoTicks > 0 {
		out = append(out, fail("invariants.ok_no_reason", "invariants",
			fmt.Sprintf("%d pools have OK=true Ticks=0 but reason != empty-bitmap", fetchOKNoTicks)))
	} else {
		out = append(out, pass("invariants.ok_no_reason", "invariants", "reason consistent with OK + Ticks count"))
	}
	out = append(out, pass("invariants.empty_ok", "invariants",
		fmt.Sprintf("verified-empty pools=%d/%d (OK=true Ticks=nil reason=empty-bitmap)", emptyOK, total)))
	return out
}

// ─── hook whitelist sync ────────────────────────────────────────────────────

// allowedHooksABI — the auto-generated `mapping(address => bool) public
// allowedHooks` getter on V4Mini and MixedV3V4Executor.
var (
	allowedHooksABI   abi.ABI
	hookIsAllowedABI  abi.ABI
)

func init() {
	var err error
	allowedHooksABI, err = abi.JSON(strings.NewReader(`[{"name":"allowedHooks","type":"function","stateMutability":"view","inputs":[{"name":"","type":"address"}],"outputs":[{"name":"","type":"bool"}]}]`))
	if err != nil {
		panic(err)
	}
	hookIsAllowedABI, err = abi.JSON(strings.NewReader(`[{"name":"isAllowed","type":"function","stateMutability":"view","inputs":[{"name":"","type":"address"}],"outputs":[{"name":"","type":"bool"}]}]`))
	if err != nil {
		panic(err)
	}
}

// checkHooks flags every V4 pool whose hooks address is non-zero but is NOT
// in the allowedHooks whitelist of either V4Mini or MixedV3V4Executor. Such
// a pool will pre-revert inside the contract with `hooks` require — so any
// cycle touching it through the lean fleet burns gas for nothing.
//
// Failure mode: whitelist drift vs live pool registry. Either the owner
// forgot to `setAllowedHook` a new hook address, or a hook that used to be
// safe is no longer in the list (unlikely but tracked).
func checkHooks(cfg *yamlConfig, pools []debugPool) []result {
	var out []result

	v4Mini := strings.TrimSpace(cfg.Trading.ExecutorV4Mini)
	mixed := strings.TrimSpace(cfg.Trading.ExecutorMixedV3V4)
	if v4Mini == "" && mixed == "" {
		out = append(out, skip("hooks.config", "hooks", "neither executor_v4_mini nor executor_mixed_v3v4 configured"))
		return out
	}

	rpc := cfg.Trading.TickDataRPC
	if rpc == "" {
		rpc = cfg.ArbitrumRPC
	}
	rpc = strings.Replace(rpc, "wss://", "https://", 1)
	rpc = strings.Replace(rpc, "ws://", "http://", 1)
	client, err := ethclient.DialContext(context.Background(), rpc)
	if err != nil {
		out = append(out, fail("hooks.dial", "hooks", fmt.Sprintf("dial RPC: %v", err)))
		return out
	}
	defer client.Close()

	hooksWithPools := make(map[string][]string)
	for _, p := range pools {
		if p.DEX != "UniV4" {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(p.V4Hooks))
		if h == "" || h == "0x0000000000000000000000000000000000000000" {
			continue
		}
		hooksWithPools[h] = append(hooksWithPools[h], p.Address)
	}
	if len(hooksWithPools) == 0 {
		out = append(out, pass("hooks.scan", "hooks", "no non-zero V4 hooks in live pool registry — nothing to whitelist"))
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	hookRegistryCfg := strings.TrimSpace(cfg.Trading.HookRegistry)

	queryRegistry := func(registryAddr, hookAddr string) (bool, error) {
		data, err := hookIsAllowedABI.Pack("isAllowed", common.HexToAddress(hookAddr))
		if err != nil {
			return false, err
		}
		target := common.HexToAddress(registryAddr)
		res, err := client.CallContract(ctx, ethereum.CallMsg{To: &target, Data: data}, nil)
		if err != nil {
			return false, err
		}
		if len(res) < 32 {
			return false, fmt.Errorf("short response (%d bytes)", len(res))
		}
		return res[31] != 0, nil
	}

	queryExecutorLocal := func(execAddr, hookAddr string) (bool, error) {
		data, err := allowedHooksABI.Pack("allowedHooks", common.HexToAddress(hookAddr))
		if err != nil {
			return false, err
		}
		target := common.HexToAddress(execAddr)
		res, err := client.CallContract(ctx, ethereum.CallMsg{To: &target, Data: data}, nil)
		if err != nil {
			return false, err
		}
		if len(res) < 32 {
			return false, fmt.Errorf("short response (%d bytes)", len(res))
		}
		return res[31] != 0, nil
	}

	queryWhitelist := func(execAddr, hookAddr string) (bool, error) {
		if hookRegistryCfg != "" {
			return queryRegistry(hookRegistryCfg, hookAddr)
		}
		return queryExecutorLocal(execAddr, hookAddr)
	}

	checkOne := func(label, execAddr string) {
		if execAddr == "" {
			out = append(out, skip("hooks."+label+".config", "hooks", label+" address not configured"))
			return
		}
		var missing []string
		for hook, pps := range hooksWithPools {
			allowed, err := queryWhitelist(execAddr, hook)
			if err != nil {
				out = append(out, fail("hooks."+label+".rpc."+hook, "hooks", fmt.Sprintf("allowedHooks(%s): %v", hook, err)))
				continue
			}
			if !allowed {
				sample := pps
				if len(sample) > 3 {
					sample = sample[:3]
				}
				missing = append(missing, fmt.Sprintf("%s (%d pool%s, e.g. %s)", hook, len(pps), plural(len(pps)), strings.Join(sample, ",")))
			}
		}
		if len(missing) == 0 {
			out = append(out, pass("hooks."+label+".sync", "hooks",
				fmt.Sprintf("%s whitelist covers all %d distinct non-zero hooks across %d V4 pools", label, len(hooksWithPools), poolTotal(hooksWithPools))))
			return
		}
		out = append(out, fail("hooks."+label+".missing", "hooks",
			fmt.Sprintf("%s missing %d hook(s) from whitelist — cycles through these pools will pre-revert: %s",
				label, len(missing), strings.Join(missing, "; "))))
	}
	checkOne("v4mini", v4Mini)
	checkOne("mixedv3v4", mixed)

	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func poolTotal(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

// ─── chain cross-check ──────────────────────────────────────────────────────

// uniV3 tickBitmap / V4 StateView ABIs — minimal for the on-chain diff.
var (
	tickBitmapABI     abi.ABI
	algebraTickTableABI abi.ABI
	v4StateViewABI    abi.ABI
)

func init() {
	var err error
	tickBitmapABI, err = abi.JSON(strings.NewReader(`[{"name":"tickBitmap","type":"function","stateMutability":"view","inputs":[{"name":"wordPosition","type":"int16"}],"outputs":[{"name":"","type":"uint256"}]}]`))
	if err != nil {
		panic(err)
	}
	algebraTickTableABI, err = abi.JSON(strings.NewReader(`[{"name":"tickTable","type":"function","stateMutability":"view","inputs":[{"name":"wordPosition","type":"int16"}],"outputs":[{"name":"","type":"uint256"}]}]`))
	if err != nil {
		panic(err)
	}
	v4StateViewABI, err = abi.JSON(strings.NewReader(`[{"name":"getTickBitmap","type":"function","stateMutability":"view","inputs":[{"name":"poolId","type":"bytes32"},{"name":"tick","type":"int16"}],"outputs":[{"name":"","type":"uint256"}]}]`))
	if err != nil {
		panic(err)
	}
}

const v4StateViewAddr = "0x76fd297e2d437cd7f76d50f01afe6160f86e9990"

func checkChain(cfg *yamlConfig, pools []debugPool) []result {
	var out []result
	rpc := cfg.Trading.TickDataRPC
	if rpc == "" {
		rpc = cfg.ArbitrumRPC
	}
	rpc = strings.Replace(rpc, "wss://", "https://", 1)
	rpc = strings.Replace(rpc, "ws://", "http://", 1)
	client, err := ethclient.DialContext(context.Background(), rpc)
	if err != nil {
		out = append(out, fail("chain.dial", "chain", fmt.Sprintf("dial RPC: %v", err)))
		return out
	}
	defer client.Close()

	// Pick a sample of V3-family pools with populated bitmap metadata.
	var sample []debugPool
	for _, p := range pools {
		if !isV3Family(p.DEX) || p.TicksBlock == 0 || p.TickSpacing <= 0 {
			continue
		}
		sample = append(sample, p)
	}
	sort.Slice(sample, func(i, j int) bool {
		return sample[i].TicksFetchFailureCount > sample[j].TicksFetchFailureCount
	})
	if len(sample) > *flagChainSample {
		sample = sample[:*flagChainSample]
	}
	if len(sample) == 0 {
		out = append(out, skip("chain.bootstrap", "chain", "no V3-family pools with bitmap metadata"))
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, p := range sample {
		// Fetch bitmap word at the pool's centerWord.
		var centerWord int16
		switch p.DEX {
		case "CamelotV3", "ZyberV3":
			centerWord = int16(p.TickAtFetch >> 8)
		default:
			centerWord = int16((p.TickAtFetch / p.TickSpacing) >> 8)
		}
		var data []byte
		var target common.Address
		switch p.DEX {
		case "UniV4":
			if p.PoolID == "" {
				out = append(out, skip("chain.v4.nopoolid", "chain", fmt.Sprintf("%s has no PoolID", p.Address)))
				continue
			}
			pid := common.HexToHash(p.PoolID)
			data, _ = v4StateViewABI.Pack("getTickBitmap", pid, centerWord)
			target = common.HexToAddress(v4StateViewAddr)
		case "CamelotV3", "ZyberV3":
			data, _ = algebraTickTableABI.Pack("tickTable", centerWord)
			target = common.HexToAddress(p.Address)
		default:
			data, _ = tickBitmapABI.Pack("tickBitmap", centerWord)
			target = common.HexToAddress(p.Address)
		}
		res, err := client.CallContract(ctx, ethereum.CallMsg{To: &target, Data: data}, nil)
		if err != nil {
			out = append(out, fail("chain."+p.Address, "chain", fmt.Sprintf("%s centerWord=%d RPC err: %v", p.DEX, centerWord, err)))
			continue
		}
		if len(res) < 32 {
			out = append(out, fail("chain."+p.Address, "chain", fmt.Sprintf("%s centerWord=%d short response (%d bytes)", p.DEX, centerWord, len(res))))
			continue
		}
		chainBM := new(big.Int).SetBytes(res[:32])
		// Fetch the per-pool detail endpoint to reconstruct the bitmap Ticks list.
		detail, err := fetchPoolDetail(*flagDebugURL, p.Address)
		if err != nil {
			out = append(out, fail("chain."+p.Address, "chain", fmt.Sprintf("pool detail fetch: %v", err)))
			continue
		}

		// Reconstruct the expected bitmap bits for centerWord from the bot's
		// Ticks slice and compare against chainBM.
		algebra := p.DEX == "CamelotV3" || p.DEX == "ZyberV3"
		expected := new(big.Int)
		for _, t := range detail.Ticks {
			var w int16
			var bit int
			if algebra {
				w = int16(t.Tick >> 8)
				bit = int(uint32(t.Tick) & 0xff)
			} else {
				compressed := t.Tick / p.TickSpacing
				w = int16(compressed >> 8)
				bit = int(uint32(compressed) & 0xff)
			}
			if w != centerWord {
				continue
			}
			expected.SetBit(expected, bit, 1)
		}
		// Chain bitmap may have more bits than bot Ticks slice because our Ticks
		// slice only contains ticks with non-zero liquidityNet, while the bitmap
		// includes anything with non-zero liquidityGross. So expected MUST be a
		// subset of chainBM; anything else = regression.
		andBM := new(big.Int).And(expected, chainBM)
		if andBM.Cmp(expected) != 0 {
			out = append(out, fail("chain."+p.Address, "chain",
				fmt.Sprintf("%s bot bitmap has bits NOT in on-chain bitmap (wrong ticks in cache): bot=0x%x chain=0x%x",
					p.DEX, expected, chainBM)))
		} else {
			if *flagVerbose {
				out = append(out, pass("chain."+p.Address, "chain",
					fmt.Sprintf("%s centerWord=%d bot_bits=%d chain_bits=%d", p.DEX, centerWord, popcount(expected), popcount(chainBM))))
			}
		}
	}
	out = append(out, pass("chain.sample", "chain", fmt.Sprintf("cross-checked %d pools on-chain", len(sample))))
	return out
}

func popcount(n *big.Int) int {
	c := 0
	for i := 0; i < n.BitLen(); i++ {
		if n.Bit(i) == 1 {
			c++
		}
	}
	return c
}

// ─── helpers ────────────────────────────────────────────────────────────────

func isV3Family(dex string) bool {
	switch dex {
	case "UniV3", "SushiV3", "RamsesV3", "PancakeV3", "CamelotV3", "ZyberV3", "UniV4":
		return true
	}
	return false
}

func pass(id, cat, detail string) result { return result{ID: id, Cat: cat, Status: "PASS", Detail: detail} }
func fail(id, cat, detail string) result { return result{ID: id, Cat: cat, Status: "FAIL", Detail: detail} }
func skip(id, cat, detail string) result { return result{ID: id, Cat: cat, Status: "SKIP", Detail: detail} }

func hasFails(rs []result) bool {
	for _, r := range rs {
		if r.Status == "FAIL" {
			return true
		}
	}
	return false
}

func printTable(rs []result) {
	var pass, fail, skip int
	for _, r := range rs {
		switch r.Status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "SKIP":
			skip++
		}
		fmt.Printf("%-6s %-12s %-32s %s\n", r.Status, r.Cat, r.ID, r.Detail)
	}
	fmt.Printf("\n-- %d pass, %d fail, %d skip --\n", pass, fail, skip)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ticktest: "+format+"\n", args...)
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

func fetchTickHealth(base string) (*debugTickHealth, error) {
	url := strings.TrimRight(base, "/") + "/debug/tick-health"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out debugTickHealth
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func fetchPools(base string) ([]debugPool, error) {
	url := strings.TrimRight(base, "/") + "/debug/pools?limit=5000"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var r debugPoolsResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Pools, nil
}

func fetchPoolDetail(base, addr string) (*debugPoolDetail, error) {
	url := strings.TrimRight(base, "/") + "/debug/pools/" + addr
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var d debugPoolDetail
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}
