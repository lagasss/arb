package internal

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TickData holds the liquidityNet for a single initialized V3 tick.
// A sorted slice of these enables the multi-tick swap simulator to cross
// tick boundaries with the correct liquidity at each level.
type TickData struct {
	Tick         int32
	LiquidityNet *big.Int // signed: positive = liquidity added when crossing left→right
}

type DEXType uint8

const (
	DEXUniswapV2 DEXType = iota
	DEXUniswapV3
	DEXCamelot
	DEXCamelotV3
	DEXTraderJoe
	DEXCurve
	DEXSushiSwap
	DEXSushiSwapV3
	DEXBalancerWeighted
	DEXRamsesV2
	DEXRamsesV3
	DEXPancakeV3
	DEXChronos
	DEXZyberV3
	DEXDeltaSwap
	DEXSwapr
	DEXArbSwap
	DEXUniswapV4
)

func (d DEXType) String() string {
	switch d {
	case DEXUniswapV2:
		return "UniV2"
	case DEXUniswapV3:
		return "UniV3"
	case DEXCamelot:
		return "Camelot"
	case DEXCamelotV3:
		return "CamelotV3"
	case DEXTraderJoe:
		return "TJoe"
	case DEXCurve:
		return "Curve"
	case DEXSushiSwap:
		return "Sushi"
	case DEXSushiSwapV3:
		return "SushiV3"
	case DEXBalancerWeighted:
		return "BalancerW"
	case DEXRamsesV2:
		return "RamsesV2"
	case DEXRamsesV3:
		return "RamsesV3"
	case DEXPancakeV3:
		return "PancakeV3"
	case DEXChronos:
		return "Chronos"
	case DEXZyberV3:
		return "ZyberV3"
	case DEXDeltaSwap:
		return "DeltaSwap"
	case DEXSwapr:
		return "Swapr"
	case DEXArbSwap:
		return "ArbSwap"
	case DEXUniswapV4:
		return "UniV4"
	default:
		return "Unknown"
	}
}

// ParseDEXType converts a String() representation back to DEXType.
func ParseDEXType(s string) DEXType {
	switch s {
	case "UniV2":
		return DEXUniswapV2
	case "UniV3":
		return DEXUniswapV3
	case "Camelot":
		return DEXCamelot
	case "CamelotV3":
		return DEXCamelotV3
	case "TJoe":
		return DEXTraderJoe
	case "Curve":
		return DEXCurve
	case "Sushi":
		return DEXSushiSwap
	case "SushiV3":
		return DEXSushiSwapV3
	case "BalancerW":
		return DEXBalancerWeighted
	case "RamsesV2":
		return DEXRamsesV2
	case "RamsesV3":
		return DEXRamsesV3
	case "PancakeV3":
		return DEXPancakeV3
	case "Chronos":
		return DEXChronos
	case "ZyberV3":
		return DEXZyberV3
	case "DeltaSwap":
		return DEXDeltaSwap
	case "Swapr":
		return DEXSwapr
	case "ArbSwap":
		return DEXArbSwap
	case "UniV4":
		return DEXUniswapV4
	default:
		return DEXUniswapV2
	}
}

type Pool struct {
	mu sync.RWMutex

	Address string
	DEX     DEXType
	FeeBps  uint32 // fee in basis points: 30 = 0.30%
	FeePPM  uint32 // fee in Uniswap V3 native units (parts-per-million): 100=0.01%, 500=0.05%, 3000=0.30%
	          // Non-zero overrides FeeBps for executor fee encoding. Use for sub-1bps tiers (e.g. fee=1 = 0.0001%).

	Token0 *Token
	Token1 *Token

	// V2-style reserves (UniV2, Camelot, TraderJoe, Sushi)
	Reserve0 *big.Int
	Reserve1 *big.Int

	// V3-specific state (from slot0 / liquidity())
	SqrtPriceX96 *big.Int
	Tick         int32
	TickSpacing  int32  // constant per pool; 1/10/60/200 depending on fee tier
	Liquidity    *big.Int

	// V3 tick map: sorted slice of initialized ticks with their liquidityNet.
	// Used by the multi-tick simulator to cross tick boundaries accurately.
	// Populated by FetchTickMap(); nil means single-tick approximation is used.
	Ticks []TickData
	// TicksUpdatedAt records the last time FetchTickMaps successfully populated
	// (or explicitly cleared) Ticks for this pool. Used by the per-pool tick
	// freshness gate in fastEvalCycles and the dashboard's stale-pool badge.
	TicksUpdatedAt time.Time
	// TicksBlock is the chain block number at which the current bitmap was
	// fetched. Checked by the bitmap-coverage gate in evalOneCandidate alongside
	// tick drift: the bitmap is valid iff the current tick is still inside the
	// fetched word range (center ± TicksWordRadius).
	TicksBlock uint64
	// TicksWordRadius is the bitmap word radius used at the last FetchTickMaps
	// pass (the fetch covered [centerWord-radius, centerWord+radius]). Stored
	// per-pool because the radius is a config value that could change across
	// bot restarts; gating uses the value that was actually fetched.
	TicksWordRadius int16
	// TickAtFetch is the pool's `tick` value at the moment FetchTickMaps last
	// populated `Ticks`. The swap listener compares this against the post-swap
	// tick from each Swap event; when |new - TickAtFetch| exceeds a configured
	// threshold (in tick-spacing units), the listener fires an eager re-fetch
	// via TickRefetchCh because the cached bitmap likely no longer covers the
	// active range. Reset every successful single-pool or batch tick fetch.
	TickAtFetch int32
	// TicksFetchOK records whether the last FetchTickMaps attempt for this
	// pool completed both multicall phases without any per-call failure.
	// The critical distinction is "verified empty" (Ticks=nil, OK=true —
	// bitmap genuinely has no initialized ticks in range, do NOT treat as
	// stale) vs "fetch failed" (Ticks=nil, OK=false — multicall RPC error
	// or tryAggregate entry returned Success=false, MUST NOT be simulated).
	// Before this field existed, both cases produced identical state and
	// the sim silently dropped real opportunities during transient RPC
	// failures. Set on every FetchTickMaps return (including the
	// tickCalls==0 fast path).
	TicksFetchOK bool
	// TicksFetchReason is a short machine-readable tag set when Ticks is
	// empty or the fetch could not complete. Values: "empty-bitmap" (OK=true),
	// "bitmap-rpc-failed" (OK=false), "ticks-rpc-failed", "decode-err",
	// "algebra-roundtrip", "no-tick-spacing", "no-sqrt-price", "skipped".
	// Empty string on normal success with a populated Ticks slice.
	TicksFetchReason string
	// TicksFetchAttemptedAt records the wall-clock time of the most recent
	// FetchTickMaps attempt for this pool regardless of whether it yielded
	// any ticks. TicksUpdatedAt only advances on success (OK=true); this
	// field advances on every attempt so the tick-test suite can detect
	// pools that are being hammered by failed fetches.
	TicksFetchAttemptedAt time.Time
	// TicksFetchBitmapWords is the count of bitmap words the last attempt
	// actually fetched (not the count that returned non-zero). Equal to
	// (2*radius+1) on a successful fetch, 0 when the fetch was skipped.
	TicksFetchBitmapWords int16
	// TicksFetchNonEmptyWords is the count of bitmap words that contained
	// at least one initialized-tick bit on the last successful attempt.
	TicksFetchNonEmptyWords int16
	// TicksFetchFailureCount is a cumulative counter of consecutive fetch
	// failures for this pool. Reset to 0 on success. Used by the tick-test
	// suite to flag pools stuck in a fetch-failure loop.
	TicksFetchFailureCount uint32
	// TickDeltaApplied counts V3 Mint/Burn + V4 ModifyLiquidity events
	// whose liquidityDelta has been applied incrementally via
	// ApplyTickLiquidityDelta. Sanity gauge for the fast-path tick
	// reconciliation: on an active 1bp/ts=1 pool this should grow by
	// ~1-10 per minute; zero growth plus ongoing SIM_PHANTOM means the
	// event subscription or the delta math regressed.
	TickDeltaApplied uint32

	// V4-specific: hooks contract address (hex string, e.g. "0x000...")
	V4Hooks string

	// Camelot directional fees
	Token0FeeBps uint32
	Token1FeeBps uint32
	IsStable     bool

	// Curve amplification factor
	AmpFactor  uint64
	CurveFee1e10 uint64 // Curve fee in native 1e10 units (e.g. 100000 = 0.001%); more precise than FeeBps for sub-bps fees

	// Balancer weighted pool
	PoolID  string  // bytes32 pool ID for Vault.getPoolTokens()
	Weight0 float64 // normalized weight of token0 (e.g. 0.8)
	Weight1 float64 // normalized weight of token1 (e.g. 0.2)

	// Metrics (periodically updated)
	TVLUSD       float64
	Volume24hUSD float64

	// Cached spot price (token1 per token0, human-readable)
	SpotPrice float64

	// LastUpdated is stamped whenever pool state changes (swap event or multicall).
	// Edges for pools that haven't been updated recently are zeroed in the graph.
	LastUpdated time.Time

	// StateBlock is the chain block number at which the current pool state
	// (slot0 / liquidity / reserves / dynamic fee) was fetched or updated.
	// Checked by the block-lag freshness gate in evalOneCandidate against the
	// latest header block. Stamped by: BatchUpdatePoolsAtBlock, FetchV4PoolStates,
	// swap-event handlers, and pool-resolution paths on initial fetch.
	StateBlock uint64

	// Disabled pools are loaded from DB but excluded from the graph and cycle cache.
	// Set via DisablePool() in SQLite; survives restarts.
	Disabled bool

	// Pinned pools are manually-curated and protected from TVL/volume pruning.
	// Loaded from the `pinned_pools:` config list at startup. Replaces the legacy
	// `seeds:` config section.
	Pinned bool

	// Verified is true if VerifyPool has run all DEX-specific sanity checks
	// (correct fee, sim accuracy within 1% of on-chain quoter, non-zero state,
	// trivial hooks for V4, etc.) and the pool is safe to include in cycles.
	// Pools with Verified=false are loaded but excluded from cycle building.
	Verified bool
	// VerifyReason is set when Verified=false to explain why (e.g.
	// "fee mismatch: chain=300 stored=500", "v4 hook has BEFORE_SWAP permission").
	VerifyReason string

	// simRejectsRecent is a sliding-window counter of sim-rejects attributed
	// to this pool (i.e. the failing hop's pool). Incremented by RecordSimReject;
	// reset when the window expires (simRejectsWindowStart). Used by the
	// auto-disable mechanism in trySubmitCandidate to flag scam/honeypot pools
	// that repeatedly cause "hop simulation returned 0" or "execution reverted:
	// hop X" failures. Atomic int32 so the worker pool can update concurrently.
	simRejectsRecent      atomic.Int32
	simRejectsWindowStart atomic.Int64 // unix seconds when the current window began
}

func (p *Pool) PairKey() string {
	a, b := strings.ToLower(p.Token0.Address), strings.ToLower(p.Token1.Address)
	if a > b {
		a, b = b, a
	}
	return a + ":" + b
}

// SpotRate returns token1 per token0 at current reserves (human-readable).
func (p *Pool) SpotRate() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spotRateLocked()
}

func (p *Pool) spotRateLocked() float64 {
	switch p.DEX {
	case DEXUniswapV3, DEXCamelotV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXZyberV3, DEXUniswapV4:
		return p.spotRateV3()
	case DEXBalancerWeighted:
		return p.spotRateBalancer()
	case DEXCurve:
		return p.spotRateCurve()
	default:
		// Solidly/Camelot stable pools: use the same StableSwap probe formula as the simulator
		// so edge weights are consistent with simulation. V2 formula (reserve ratio) gives the
		// wrong (inflated) rate for stable pools where reserves can diverge without large price movement.
		if p.IsStable {
			return p.spotRateCurve()
		}
		return p.spotRateV2()
	}
}

// spotRateCurve computes the Curve StableSwap marginal price by simulating a tiny swap.
// This is necessary because the V2 reserve ratio (R1/R0) does not reflect the true
// exchange rate for Curve pools — the high-A invariant keeps prices near parity
// even when reserves are imbalanced.
func (p *Pool) spotRateCurve() float64 {
	if p.Reserve0 == nil || p.Reserve1 == nil {
		return 0
	}
	// AmpFactor==0 is OK — CurveSim defaults to 100, same as spotRateCurve probing via CurveSim.
	// Probe with 0.01% of reserve0 (small enough to be marginal, large enough to avoid rounding)
	probe := new(big.Int).Div(p.Reserve0, big.NewInt(10_000))
	if probe.Sign() == 0 {
		probe = big.NewInt(1)
	}
	sim := CurveSim{}
	out := sim.AmountOut(p, p.Token0, probe)
	if out == nil || out.Sign() == 0 {
		return 0
	}
	probeF, _ := new(big.Float).SetInt(probe).Float64()
	outF, _ := new(big.Float).SetInt(out).Float64()
	// Both probe and out are in native decimal space; adjust for decimal difference
	rawRate := outF / probeF
	if p.Token0.Decimals != p.Token1.Decimals {
		rawRate *= math.Pow(10, float64(p.Token0.Decimals)-float64(p.Token1.Decimals))
	}
	return rawRate
}

func (p *Pool) spotRateV2() float64 {
	if p.Reserve0 == nil || p.Reserve1 == nil {
		return 0
	}
	r0 := new(big.Float).SetInt(p.Reserve0)
	r1 := new(big.Float).SetInt(p.Reserve1)
	if r0.Sign() == 0 {
		return 0
	}
	ratio := new(big.Float).Quo(r1, r0)
	// Adjust for decimals: multiply by 10^dec0 / 10^dec1
	adj := math.Pow(10, float64(p.Token0.Decimals)) / math.Pow(10, float64(p.Token1.Decimals))
	f, _ := ratio.Float64()
	return f * adj
}

func (p *Pool) spotRateV3() float64 {
	if p.SqrtPriceX96 == nil || p.SqrtPriceX96.Sign() == 0 {
		return 0
	}
	// price = (sqrtPriceX96 / 2^96)^2 * 10^dec0 / 10^dec1
	q96 := new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))
	sqrtF := new(big.Float).SetInt(p.SqrtPriceX96)
	sqrtRatio := new(big.Float).Quo(sqrtF, q96)
	price := new(big.Float).Mul(sqrtRatio, sqrtRatio)
	adj := math.Pow(10, float64(p.Token0.Decimals)) / math.Pow(10, float64(p.Token1.Decimals))
	f, _ := price.Float64()
	return f * adj
}

// spotRateBalancer returns token1 per token0 spot price for a Balancer weighted pool.
// Spot price = (balance0 / weight0) / (balance1 / weight1)
func (p *Pool) spotRateBalancer() float64 {
	if p.Reserve0 == nil || p.Reserve1 == nil || p.Weight0 == 0 || p.Weight1 == 0 {
		return 0
	}
	b0 := new(big.Float).SetInt(p.Reserve0)
	b1 := new(big.Float).SetInt(p.Reserve1)
	// adjust for decimals
	adj0 := math.Pow(10, float64(p.Token0.Decimals))
	adj1 := math.Pow(10, float64(p.Token1.Decimals))
	// effective balance in human units
	hb0 := new(big.Float).Quo(b0, new(big.Float).SetFloat64(adj0))
	hb1 := new(big.Float).Quo(b1, new(big.Float).SetFloat64(adj1))
	// spot = (hb0/w0) / (hb1/w1) inverted to get token1 per token0
	// price of token0 in token1 = (hb1/w1) / (hb0/w0)
	num := new(big.Float).Quo(hb1, new(big.Float).SetFloat64(p.Weight1))
	den := new(big.Float).Quo(hb0, new(big.Float).SetFloat64(p.Weight0))
	if den.Sign() == 0 {
		return 0
	}
	result, _ := new(big.Float).Quo(num, den).Float64()
	return result
}

// UpdateSpotPrice recomputes and caches SpotPrice. Call after reserve update.
func (p *Pool) UpdateSpotPrice() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.SpotPrice = p.spotRateLocked()
	if p.SpotPrice > 0 {
		p.LastUpdated = time.Now()
	}
}

// ApplyTickLiquidityDelta incrementally updates the cached tick-liquidity
// state from a V3 Mint/Burn or V4 ModifyLiquidity event payload, WITHOUT
// waiting for a full FetchTickMaps re-read. This is the sim-accuracy fix
// for the SIM_PHANTOM class on tight-margin / ts=1 pools: a full refetch
// takes 100-250 ms over RPC, but in that window the stale per-tick
// liquidityNet values cause our sim to disagree with on-chain execution
// by a few bps — enough to flip sub-20bp arbs from "executes" to "reverts".
//
// Mutation model:
//   - For Mint (liquidityDelta > 0): liquidityNet[tickLower] += delta;
//     liquidityNet[tickUpper] -= delta.
//   - For Burn (liquidityDelta < 0): the same formula with negative delta
//     (add a negative number → subtract).
//   - If the pool's current tick ∈ [tickLower, tickUpper), the active
//     liquidity is adjusted by the same delta.
//
// The Ticks slice is immutability-preserved: we allocate a fresh slice and
// swap the pointer atomically under p.mu.Lock, keeping the invariant
// "Ticks is only ever replaced, never mutated in place" (see Snapshot
// doc). A tick entry whose LiquidityNet becomes exactly zero is removed
// from the slice (matches UniV3's bitmap semantics where a tick-gross=0
// clears the bit).
//
// Ticks outside the currently fetched word range are still inserted —
// the periodic FetchTickMaps will re-intersect with the bitmap on its
// next pass; in the meantime having a pending delta is safer than
// dropping it.
//
// This is strictly a best-effort fast path; the caller should still fire
// `fireTickRefetchImmediate` so a full bitmap re-read eventually
// reconciles any missed events (e.g. the bot was restarting during a
// Mint). The counter TickDeltaApplied makes the hot-path observable.
func (p *Pool) ApplyTickLiquidityDelta(tickLower, tickUpper int32, liquidityDelta *big.Int, blockNumber uint64) {
	if liquidityDelta == nil || liquidityDelta.Sign() == 0 {
		return
	}
	if tickLower >= tickUpper {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	newTicks := make([]TickData, 0, len(p.Ticks)+2)
	foundLower := false
	foundUpper := false
	for _, td := range p.Ticks {
		switch td.Tick {
		case tickLower:
			foundLower = true
			newNet := new(big.Int).Add(td.LiquidityNet, liquidityDelta)
			if newNet.Sign() != 0 {
				newTicks = append(newTicks, TickData{Tick: td.Tick, LiquidityNet: newNet})
			}
		case tickUpper:
			foundUpper = true
			newNet := new(big.Int).Sub(td.LiquidityNet, liquidityDelta)
			if newNet.Sign() != 0 {
				newTicks = append(newTicks, TickData{Tick: td.Tick, LiquidityNet: newNet})
			}
		default:
			newTicks = append(newTicks, td)
		}
	}
	if !foundLower {
		newTicks = insertTickSorted(newTicks, TickData{Tick: tickLower, LiquidityNet: new(big.Int).Set(liquidityDelta)})
	}
	if !foundUpper {
		negDelta := new(big.Int).Neg(liquidityDelta)
		newTicks = insertTickSorted(newTicks, TickData{Tick: tickUpper, LiquidityNet: negDelta})
	}
	p.Ticks = newTicks

	if p.Tick >= tickLower && p.Tick < tickUpper {
		if p.Liquidity == nil {
			p.Liquidity = new(big.Int).Set(liquidityDelta)
		} else {
			newLiq := new(big.Int).Add(p.Liquidity, liquidityDelta)
			if newLiq.Sign() < 0 {
				newLiq = new(big.Int)
			}
			p.Liquidity = newLiq
		}
	}

	if blockNumber > p.StateBlock {
		p.StateBlock = blockNumber
	}
	p.LastUpdated = time.Now()
	p.TickDeltaApplied++
}

// insertTickSorted inserts td into a sorted-by-Tick slice, preserving order.
// Binary-searches for the position. Allocates a new slice one longer; the
// caller already holds the write lock.
func insertTickSorted(dst []TickData, td TickData) []TickData {
	lo, hi := 0, len(dst)
	for lo < hi {
		m := (lo + hi) / 2
		if dst[m].Tick < td.Tick {
			lo = m + 1
		} else {
			hi = m
		}
	}
	dst = append(dst, TickData{})
	copy(dst[lo+1:], dst[lo:len(dst)-1])
	dst[lo] = td
	return dst
}

// UpdateFromV3Swap applies a Uniswap V3 (or Algebra/SushiV3) Swap event directly.
// sqrtPriceX96/liquidity/tick come from the non-indexed event data. blockNumber
// is the chain block that emitted the event; stamped on StateBlock only when
// it advances the pool's view (log-ordering guarantees against regression).
func (p *Pool) UpdateFromV3Swap(sqrtPriceX96, liquidity *big.Int, tick int32, blockNumber uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sqrtPriceX96 != nil && sqrtPriceX96.Sign() > 0 {
		p.SqrtPriceX96 = new(big.Int).Set(sqrtPriceX96)
	}
	if liquidity != nil && liquidity.Sign() > 0 {
		p.Liquidity = new(big.Int).Set(liquidity)
	}
	p.Tick = tick
	p.SpotPrice = p.spotRateLocked()
	p.LastUpdated = time.Now()
	if blockNumber > p.StateBlock {
		p.StateBlock = blockNumber
	}
}

// UpdateFromV2Swap increments reserves by the swap deltas.
// Only applies if a baseline exists from a prior multicall.
func (p *Pool) UpdateFromV2Swap(amount0In, amount1In, amount0Out, amount1Out *big.Int, blockNumber uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Reserve0 == nil || p.Reserve1 == nil {
		return // no baseline yet; next multicall will set it
	}
	r0 := new(big.Int).Add(p.Reserve0, new(big.Int).Sub(amount0In, amount0Out))
	r1 := new(big.Int).Add(p.Reserve1, new(big.Int).Sub(amount1In, amount1Out))
	if r0.Sign() > 0 {
		p.Reserve0 = r0
	}
	if r1.Sign() > 0 {
		p.Reserve1 = r1
	}
	p.SpotPrice = p.spotRateLocked()
	if blockNumber > p.StateBlock {
		p.StateBlock = blockNumber
	}
	p.LastUpdated = time.Now()
}

func (p *Pool) String() string {
	return fmt.Sprintf("%s[%s/%s fee=%dbps]", p.DEX, p.Token0.Symbol, p.Token1.Symbol, p.FeeBps)
}

// RecordSimReject increments the sliding-window sim-reject counter for this
// pool and returns the new count. If the current window (windowSecs) has
// expired, the counter resets to 1 and the window restarts at now. Callers
// use the returned count to decide whether the pool has crossed the
// auto-disable threshold.
//
// The window is a simple "reset on expiry" scheme rather than a true rolling
// window: cheap, lock-free, and good enough for a "N failures in the last
// hour" heuristic. A scam pool that produces rejects every minute will
// always trip the threshold long before the window expires; a legit pool
// that produces one reject per hour will never accumulate enough count to
// trip it.
func (p *Pool) RecordSimReject(windowSecs int64) int32 {
	now := time.Now().Unix()
	start := p.simRejectsWindowStart.Load()
	if start == 0 || now-start > windowSecs {
		// Window expired (or never started). Reset: set window start to
		// now, counter to 1. We use CompareAndSwap on the window start to
		// avoid two concurrent writers both resetting back to 1 — whoever
		// loses the CAS just falls through to the normal Add below.
		if p.simRejectsWindowStart.CompareAndSwap(start, now) {
			p.simRejectsRecent.Store(1)
			return 1
		}
	}
	return p.simRejectsRecent.Add(1)
}

// SimRejectsInWindow returns the current sim-reject count without mutating
// the window. Used by the debug HTTP endpoint and telemetry.
func (p *Pool) SimRejectsInWindow() int32 {
	return p.simRejectsRecent.Load()
}

// ResetSimRejects clears the sim-reject counter. Called when the operator
// manually re-enables a previously auto-disabled pool so it gets a fresh
// budget before tripping the threshold again.
func (p *Pool) ResetSimRejects() {
	p.simRejectsRecent.Store(0)
	p.simRejectsWindowStart.Store(0)
}

// Snapshot returns a deep copy of the mutable state on this pool, sharing the
// immutable fields (Token0, Token1, Address, DEX, TickSpacing, V4Hooks, PoolID,
// Weights, AmpFactor, IsStable, Verified, etc.) and the Ticks slice (which is
// only ever replaced atomically by FetchTickMaps — never mutated in place).
//
// Why this exists: the simulator (UniswapV3Sim.AmountOut, V2 reserves math,
// Algebra dynamic-fee fields, etc.) is called twice on the same cycle —
// once from optimalAmountIn during fastEvalCycles, and again from buildHops
// during executor.Submit milliseconds later. With the block-driven multicall,
// eager tick re-fetch, and Algebra fee refresh loops all writing pool state in
// the background, the second call can read different state than the first,
// causing the cycle to re-score with a ~50–100 bps delta on shallow pools.
// That gap was the dominant source of "build hops: cycle unprofitable" rejects
// in arb_observations.
//
// Snapshot freezes the relevant fields at one instant under p.mu.RLock(), so
// downstream callers (the simulator, buildHops, the JSON loggers) all see the
// same state regardless of how many background writers fire in between.
//
// The returned *Pool has a fresh sync.RWMutex (zero value) and is safe to use
// from a single goroutine. It MUST NOT be inserted back into the registry —
// it's an evaluation artifact, not a live pool.
func (p *Pool) Snapshot() *Pool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	clone := &Pool{
		// Immutable identity / config — share or copy by value.
		Address:      p.Address,
		DEX:          p.DEX,
		FeeBps:       p.FeeBps,
		FeePPM:       p.FeePPM,
		Token0:       p.Token0,
		Token1:       p.Token0, // overwritten just below — keeps the field order tidy
		TickSpacing:  p.TickSpacing,
		V4Hooks:      p.V4Hooks,
		Token0FeeBps: p.Token0FeeBps,
		Token1FeeBps: p.Token1FeeBps,
		IsStable:     p.IsStable,
		AmpFactor:    p.AmpFactor,
		CurveFee1e10: p.CurveFee1e10,
		PoolID:       p.PoolID,
		Weight0:      p.Weight0,
		Weight1:      p.Weight1,
		TVLUSD:       p.TVLUSD,
		Volume24hUSD: p.Volume24hUSD,
		SpotPrice:    p.SpotPrice,
		LastUpdated:  p.LastUpdated,
		StateBlock:   p.StateBlock,
		Disabled:     p.Disabled,
		Pinned:       p.Pinned,
		Verified:     p.Verified,
		VerifyReason: p.VerifyReason,
		Tick:                    p.Tick,
		TickAtFetch:             p.TickAtFetch,
		TicksUpdatedAt:          p.TicksUpdatedAt,
		TicksBlock:              p.TicksBlock,
		TicksWordRadius:         p.TicksWordRadius,
		TicksFetchOK:            p.TicksFetchOK,
		TicksFetchReason:        p.TicksFetchReason,
		TicksFetchAttemptedAt:   p.TicksFetchAttemptedAt,
		TicksFetchBitmapWords:   p.TicksFetchBitmapWords,
		TicksFetchNonEmptyWords: p.TicksFetchNonEmptyWords,
		TicksFetchFailureCount:  p.TicksFetchFailureCount,
		// Ticks is only ever replaced atomically by FetchTickMaps with a fresh
		// slice — never mutated in place — so sharing the slice header is safe.
		Ticks: p.Ticks,
	}
	clone.Token1 = p.Token1
	// Mutable big.Int fields must be deep-copied so subsequent writes to the
	// live pool don't ripple into the clone.
	if p.SqrtPriceX96 != nil {
		clone.SqrtPriceX96 = new(big.Int).Set(p.SqrtPriceX96)
	}
	if p.Liquidity != nil {
		clone.Liquidity = new(big.Int).Set(p.Liquidity)
	}
	if p.Reserve0 != nil {
		clone.Reserve0 = new(big.Int).Set(p.Reserve0)
	}
	if p.Reserve1 != nil {
		clone.Reserve1 = new(big.Int).Set(p.Reserve1)
	}
	return clone
}

// mergeFrom updates the receiver's fields with values from `src` IN PLACE,
// preserving the receiver's identity (pointer, mutex) so that all consumers
// holding the receiver (cycle cache, graph edges, swap listener callbacks)
// keep seeing the same Pool instance. Called by PoolRegistry.ForceAdd/Add
// when an existing entry matches the new pool's address.
//
// Why in-place merge instead of replacing the registry entry: the previous
// implementation aliased *big.Int pointers between the old and new Pool
// structs, then replaced the registry entry with the new pool. Subsequent
// UpdateFromV3Swap calls would replace the NEW pool's SqrtPriceX96 pointer
// (via new(big.Int).Set(...)), but the cycle cache still held the OLD pool
// pointer — whose SqrtPriceX96 field kept pointing at the originally-shared
// big.Int. Result: cycle cache read frozen-in-time sqrtPrice values while
// the registry had fresh state, producing phantom cycles (see trade 132
// forensics: cached sqrt was 1.32% off from on-chain, 266 bps phantom profit).
//
// Fields handled:
//   - Identity (DEX, token0/1, fee): replaced from src if non-zero
//   - State big.Ints (sqrtPrice, liquidity, reserves): DEEP-COPIED from src
//     only if src has a meaningful value (non-nil AND non-zero); otherwise
//     the receiver's existing state is preserved
//   - Metadata (TVL, Volume24h, LastUpdated): copied from src if newer/non-zero
//   - Flags (Disabled, Pinned, Verified): replaced from src
//
// Locks the receiver's mutex for the entire merge.
func (p *Pool) mergeFrom(src *Pool) {
	if src == nil || p == src {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Identity / config. Update from src unconditionally for fields that
	// represent pool configuration; these should already match, but a
	// subgraph refresh or pinned reload may provide fresher metadata.
	if src.DEX != 0 {
		p.DEX = src.DEX
	}
	if src.Token0 != nil {
		p.Token0 = src.Token0
	}
	if src.Token1 != nil {
		p.Token1 = src.Token1
	}
	if src.FeeBps != 0 {
		p.FeeBps = src.FeeBps
	}
	if src.FeePPM != 0 {
		p.FeePPM = src.FeePPM
	}
	if src.TickSpacing != 0 {
		p.TickSpacing = src.TickSpacing
	}
	if src.V4Hooks != "" {
		p.V4Hooks = src.V4Hooks
	}
	if src.PoolID != "" {
		p.PoolID = src.PoolID
	}
	if src.Weight0 != 0 {
		p.Weight0 = src.Weight0
	}
	if src.Weight1 != 0 {
		p.Weight1 = src.Weight1
	}
	if src.Token0FeeBps != 0 {
		p.Token0FeeBps = src.Token0FeeBps
	}
	if src.Token1FeeBps != 0 {
		p.Token1FeeBps = src.Token1FeeBps
	}
	if src.AmpFactor != 0 {
		p.AmpFactor = src.AmpFactor
	}
	if src.CurveFee1e10 != 0 {
		p.CurveFee1e10 = src.CurveFee1e10
	}
	// IsStable is a bool — apply src unconditionally only if src has been
	// configured (any explicit True wins; src False doesn't override an
	// existing True, as that would drop stable semantics on a re-seed).
	if src.IsStable {
		p.IsStable = true
	}

	// Mutable state: DEEP COPY from src only if src has a meaningful value.
	// This is the critical change — previous code aliased pointers, allowing
	// subsequent writes to one pool to detach from the other.
	if src.SqrtPriceX96 != nil && src.SqrtPriceX96.Sign() > 0 {
		p.SqrtPriceX96 = new(big.Int).Set(src.SqrtPriceX96)
	}
	if src.Liquidity != nil && src.Liquidity.Sign() > 0 {
		p.Liquidity = new(big.Int).Set(src.Liquidity)
	}
	if src.Reserve0 != nil && src.Reserve0.Sign() > 0 {
		p.Reserve0 = new(big.Int).Set(src.Reserve0)
	}
	if src.Reserve1 != nil && src.Reserve1.Sign() > 0 {
		p.Reserve1 = new(big.Int).Set(src.Reserve1)
	}
	if src.Tick != 0 {
		p.Tick = src.Tick
	}
	if src.SpotPrice != 0 {
		p.SpotPrice = src.SpotPrice
	}
	// Tick map: only replace if src provides a populated slice. Callers that
	// don't load ticks leave this nil — preserve the existing slice in that
	// case (the periodic tick fetcher will refresh it on its own schedule).
	if len(src.Ticks) > 0 {
		p.Ticks = src.Ticks
	}
	if !src.TicksUpdatedAt.IsZero() {
		p.TicksUpdatedAt = src.TicksUpdatedAt
	}
	if src.TicksBlock > p.TicksBlock {
		p.TicksBlock = src.TicksBlock
	}
	if src.TicksWordRadius != 0 {
		p.TicksWordRadius = src.TicksWordRadius
	}
	if src.TickAtFetch != 0 {
		p.TickAtFetch = src.TickAtFetch
	}

	// Metadata: take the most recent / non-zero.
	if src.TVLUSD > 0 {
		p.TVLUSD = src.TVLUSD
	}
	if src.Volume24hUSD > 0 {
		p.Volume24hUSD = src.Volume24hUSD
	}
	if src.LastUpdated.After(p.LastUpdated) {
		p.LastUpdated = src.LastUpdated
	}
	if src.StateBlock > p.StateBlock {
		p.StateBlock = src.StateBlock
	}

	// Flags: src wins. Subgraph refreshes can legitimately flip Disabled
	// back to false, and pinned/verified should track whichever re-seed
	// path last touched the pool.
	p.Disabled = src.Disabled
	if src.Pinned {
		p.Pinned = true
	}
	if src.Verified {
		p.Verified = true
	}
	if src.VerifyReason != "" {
		p.VerifyReason = src.VerifyReason
	}
}
