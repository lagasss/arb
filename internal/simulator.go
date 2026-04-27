package internal

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// AMMSimulator computes the actual output amount for a swap including fee and price impact.
type AMMSimulator interface {
	AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int
}

// DiagnoseZeroOutput returns a human-readable reason why a swap through
// (pool, tokenIn, amountIn) would produce zero output. It inspects the
// specific preconditions each AmountOut path checks for zero-output early
// exits. Returns "" when the sim SHOULD produce a non-zero output — in that
// case a zero from AmountOut is a genuine math-level result (e.g. rounding
// at wei granularity or a swap so large it zeroes out in integer division).
//
// Exists so cmd/smoketest and other diagnostic tools can surface the
// concrete missing field (e.g. "V3: Ticks slice empty; bitmap never
// fetched for this pool") rather than the generic "simulation returned 0".
// That's the difference between a 10-minute debug rabbit hole and a
// 30-second fix.
func DiagnoseZeroOutput(pool *Pool, tokenIn *Token, amountIn *big.Int) string {
	if pool == nil {
		return "pool=nil"
	}
	if tokenIn == nil {
		return "tokenIn=nil"
	}
	if amountIn == nil || amountIn.Sign() == 0 {
		return "amountIn is zero"
	}
	switch pool.DEX {
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXUniswapV4,
		DEXCamelotV3, DEXZyberV3:
		if pool.SqrtPriceX96 == nil || pool.SqrtPriceX96.Sign() == 0 {
			return "V3: SqrtPriceX96 missing or zero"
		}
		if pool.Liquidity == nil {
			return "V3: Liquidity nil"
		}
		pool.mu.RLock()
		ticks := pool.Ticks
		spacing := pool.TickSpacing
		pool.mu.RUnlock()
		if spacing == 0 {
			return "V3: TickSpacing=0 (metadata not fetched)"
		}
		if len(ticks) == 0 {
			return "V3: Ticks slice empty — bitmap never fetched for this pool (falls to single-tick approximation which returns 0)"
		}
	case DEXCurve:
		if pool.Reserve0 == nil || pool.Reserve1 == nil {
			return "Curve: reserves nil"
		}
		if pool.Reserve0.Sign() == 0 || pool.Reserve1.Sign() == 0 {
			return "Curve: one or both reserves are zero"
		}
	case DEXBalancerWeighted:
		if pool.Reserve0 == nil || pool.Reserve1 == nil {
			return "Balancer: reserves nil"
		}
		if pool.Weight0 == 0 || pool.Weight1 == 0 {
			return "Balancer: weights not fetched (both zero)"
		}
	default:
		// V2 family: UniV2, Sushi, Camelot, TraderJoe
		var reserveIn, reserveOut *big.Int
		if strings.EqualFold(tokenIn.Address, pool.Token0.Address) {
			reserveIn, reserveOut = pool.Reserve0, pool.Reserve1
		} else {
			reserveIn, reserveOut = pool.Reserve1, pool.Reserve0
		}
		if reserveIn == nil || reserveOut == nil {
			return "V2: reserves nil"
		}
		if reserveIn.Sign() == 0 {
			return "V2: reserveIn is zero"
		}
		if reserveOut.Sign() == 0 {
			return "V2: reserveOut is zero"
		}
		// Check whether rounded integer division would yield zero —
		// typically when amountIn is microscopic relative to reserves.
		feeBps := pool.FeeBps
		if feeBps == 0 {
			feeBps = 30 // common default for diagnostic purposes
		}
		feeNumer := int64(10_000 - feeBps)
		num := new(big.Int).Mul(amountIn, big.NewInt(feeNumer))
		num.Mul(num, reserveOut)
		denom := new(big.Int).Mul(reserveIn, big.NewInt(10_000))
		denom.Add(denom, new(big.Int).Mul(amountIn, big.NewInt(feeNumer)))
		if new(big.Int).Div(num, denom).Sign() == 0 {
			return "V2: amountIn too small relative to reserves — integer division rounds to 0"
		}
		if pool.IsStable {
			return "V2 stable/solidly: no obvious zero-preconditions — may be a StableSwap-specific edge case"
		}
	}
	return ""
}

// SimulatorFor returns the correct simulator for a DEX type.
func SimulatorFor(dex DEXType) AMMSimulator {
	switch dex {
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXUniswapV4:
		return UniswapV3Sim{} // V4 uses same sqrtPrice/liquidity/tick math as V3
	case DEXCamelotV3, DEXZyberV3:
		return UniswapV3Sim{} // Algebra pools use same tick-math formula
	case DEXCamelot:
		return CamelotSim{}
	case DEXCurve:
		return CurveSim{}
	case DEXBalancerWeighted:
		return BalancerWeightedSim{}
	default:
		return UniswapV2Sim{}
	}
}

// ─── Uniswap V2 / SushiSwap / Trader Joe ───────────────────────────────────

type UniswapV2Sim struct{}

func (s UniswapV2Sim) AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	var reserveIn, reserveOut *big.Int
	if tokenIn.Address == pool.Token0.Address {
		reserveIn, reserveOut = pool.Reserve0, pool.Reserve1
	} else {
		reserveIn, reserveOut = pool.Reserve1, pool.Reserve0
	}
	if reserveIn == nil || reserveOut == nil || reserveIn.Sign() == 0 || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}
	feeDenom := big.NewInt(10_000)
	feeNumer := new(big.Int).Sub(feeDenom, big.NewInt(int64(pool.FeeBps)))
	amountInWithFee := new(big.Int).Mul(amountIn, feeNumer)
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Add(
		new(big.Int).Mul(reserveIn, feeDenom),
		amountInWithFee,
	)
	return new(big.Int).Div(numerator, denominator)
}

// ─── Camelot ───────────────────────────────────────────────────────────────

type CamelotSim struct{}

func (s CamelotSim) AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	if pool.IsStable {
		return solidlyStableAmountOut(pool, tokenIn, amountIn)
	}
	var reserveIn, reserveOut *big.Int
	var feeBps int64
	if strings.EqualFold(tokenIn.Address, pool.Token0.Address) {
		reserveIn, reserveOut = pool.Reserve0, pool.Reserve1
		feeBps = int64(pool.Token0FeeBps)
	} else {
		reserveIn, reserveOut = pool.Reserve1, pool.Reserve0
		feeBps = int64(pool.Token1FeeBps)
	}
	if reserveIn == nil || reserveOut == nil || reserveIn.Sign() == 0 || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}

	// Camelot directional fees use 100_000 denominator.
	// If directional fees were not fetched from chain (both are 0), fall back to
	// the pool-level FeeBps which uses 10_000 denominator.
	var feeDenom, feeNumer *big.Int
	if feeBps == 0 && pool.FeeBps > 0 {
		feeDenom = big.NewInt(10_000)
		feeNumer = new(big.Int).Sub(feeDenom, big.NewInt(int64(pool.FeeBps)))
	} else {
		feeDenom = big.NewInt(100_000)
		feeNumer = new(big.Int).Sub(feeDenom, big.NewInt(feeBps))
	}

	amountInFee := new(big.Int).Mul(amountIn, feeNumer)
	numerator := new(big.Int).Mul(amountInFee, reserveOut)
	denominator := new(big.Int).Add(
		new(big.Int).Mul(reserveIn, feeDenom),
		amountInFee,
	)
	return new(big.Int).Div(numerator, denominator)
}

// ─── Solidly StableSwap (x³y + y³x = k) ────────────────────────────────────
// Used by Camelot V2 stable pairs, Ramses V2 stable pairs, and other Solidly
// forks. The invariant is: x³·y + y³·x = k (where x,y are normalized to 18
// decimals). Given a swap of dx, we solve for the new y via Newton's method.

// solidlyStableAmountOut computes the output for a Solidly-style stable swap.
func solidlyStableAmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	if pool.Reserve0 == nil || pool.Reserve1 == nil || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}
	var reserveIn, reserveOut *big.Int
	var decIn, decOut uint8
	if strings.EqualFold(tokenIn.Address, pool.Token0.Address) {
		reserveIn, reserveOut = pool.Reserve0, pool.Reserve1
		decIn, decOut = pool.Token0.Decimals, pool.Token1.Decimals
	} else {
		reserveIn, reserveOut = pool.Reserve1, pool.Reserve0
		decIn, decOut = pool.Token1.Decimals, pool.Token0.Decimals
	}
	if reserveIn.Sign() == 0 || reserveOut.Sign() == 0 {
		return big.NewInt(0)
	}

	// Apply fee: Camelot stable pairs typically charge 0.04% (4 bps) but
	// some may have different fees. Use FeeBps if populated, else default 4.
	feeBps := int64(pool.FeeBps)
	if feeBps <= 0 {
		feeBps = 4
	}
	amountInAfterFee := new(big.Int).Mul(amountIn, big.NewInt(10000-feeBps))
	amountInAfterFee.Div(amountInAfterFee, big.NewInt(10000))

	// Normalize to 1e18
	x := normalize(reserveIn, decIn)
	y := normalize(reserveOut, decOut)
	dx := normalize(amountInAfterFee, decIn)

	// k = x³y + y³x
	k := solidlyK(x, y)
	// new_x = x + dx
	xNew := new(big.Int).Add(x, dx)
	// Solve for new_y such that solidlyK(xNew, yNew) = k via Newton's method
	yNew := solidlyGetY(xNew, k, y)
	if yNew == nil || yNew.Cmp(y) >= 0 {
		return big.NewInt(0)
	}
	dy := new(big.Int).Sub(y, yNew)
	return denormalize(dy, decOut)
}

// solidlyK computes x³·y + y³·x (the Solidly stable invariant).
func solidlyK(x, y *big.Int) *big.Int {
	// x³y = x*x*x * y / 1e18 / 1e18 / 1e18
	// We work in 1e18 fixed point, so we need to be careful with scaling.
	// k = x * x / 1e18 * x / 1e18 * y / 1e18 + y * y / 1e18 * y / 1e18 * x / 1e18
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	x2 := new(big.Int).Mul(x, x)
	x2.Div(x2, e18)
	x3 := new(big.Int).Mul(x2, x)
	x3.Div(x3, e18)
	x3y := new(big.Int).Mul(x3, y)
	x3y.Div(x3y, e18)

	y2 := new(big.Int).Mul(y, y)
	y2.Div(y2, e18)
	y3 := new(big.Int).Mul(y2, y)
	y3.Div(y3, e18)
	y3x := new(big.Int).Mul(y3, x)
	y3x.Div(y3x, e18)

	return new(big.Int).Add(x3y, y3x)
}

// solidlyGetY solves x³·y + y³·x = k for y via Newton's method.
// Initial guess is the current y (close to the solution for small swaps).
func solidlyGetY(x, k, yGuess *big.Int) *big.Int {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	y := new(big.Int).Set(yGuess)
	for i := 0; i < 255; i++ {
		// f(y) = solidlyK(x, y) - k
		ky := solidlyK(x, y)
		f := new(big.Int).Sub(ky, k)
		if f.Sign() == 0 {
			return y
		}
		// f'(y) = d/dy [x³y + y³x] = x³ + 3·y²·x (all in 1e18 fixed point)
		x2 := new(big.Int).Mul(x, x)
		x2.Div(x2, e18)
		x3 := new(big.Int).Mul(x2, x)
		x3.Div(x3, e18) // x³ in 1e18

		y2 := new(big.Int).Mul(y, y)
		y2.Div(y2, e18)
		threeY2X := new(big.Int).Mul(y2, x)
		threeY2X.Div(threeY2X, e18)
		threeY2X.Mul(threeY2X, big.NewInt(3))

		fprime := new(big.Int).Add(x3, threeY2X)
		if fprime.Sign() == 0 {
			return nil
		}
		// Newton step: y_new = y - f(y)/f'(y)
		step := new(big.Int).Mul(f, e18)
		step.Div(step, fprime)
		yNew := new(big.Int).Sub(y, step)
		if yNew.Sign() <= 0 {
			yNew = big.NewInt(1)
		}
		// Convergence check: |y - yNew| <= 1
		diff := new(big.Int).Sub(y, yNew)
		diff.Abs(diff)
		y = yNew
		if diff.Cmp(big.NewInt(1)) <= 0 {
			return y
		}
	}
	return y
}

// ─── Uniswap V3 ────────────────────────────────────────────────────────────

type UniswapV3Sim struct{}

var Q96 = new(big.Int).Lsh(big.NewInt(1), 96)
var Q192 = new(big.Int).Mul(Q96, Q96)

// camelotV3DefaultFeeBps is applied when an Algebra (CamelotV3/ZyberV3) pool
// reports fee_bps=0, which happens when the pool uses dynamic fees and the
// current fee is not captured in globalState(). Set via camelot_v3_default_fee_bps
// in config.yaml.
var camelotV3DefaultFeeBps uint32 = 25

// AmountOut computes the exact on-chain output for a UniV3-style swap using
// the single-active-tick formula from the Uniswap V3 whitepaper.
//
// For each direction:
//
//	zeroForOne (token0 in → token1 out):
//	  newSqrtP = L·sqrtP·Q96 / (L·Q96 + amountInAfterFee·sqrtP)
//	  amountOut = L·(sqrtP - newSqrtP) / Q96
//
//	oneForZero (token1 in → token0 out):
//	  newSqrtP = sqrtP + amountInAfterFee·Q96 / L
//	  amountOut = L·(1/newSqrtP - 1/sqrtP)·Q96
//	           = L·Q96·(newSqrtP - sqrtP) / (sqrtP·newSqrtP)
//
// When TickSpacing is known we detect whether the trade would exhaust the current
// tick and clamp amountIn to the tick boundary. This prevents over-estimating
// output on trades that cross into a range with different liquidity — which is the
// primary source of go_sim false positives for arb cycles that actually revert.
//
// Without TickSpacing (first block after startup before multicall runs) we still
// use exact math for the current tick; we just won't clamp at the boundary.
func (s UniswapV3Sim) AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	if pool.SqrtPriceX96 == nil || pool.Liquidity == nil || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}

	var feePips uint64
	if pool.FeePPM > 0 {
		feePips = uint64(pool.FeePPM)
	} else {
		feeBps := pool.FeeBps
		if feeBps == 0 && (pool.DEX == DEXCamelotV3 || pool.DEX == DEXZyberV3) {
			feeBps = camelotV3DefaultFeeBps
		}
		feePips = uint64(feeBps) * 100
	}

	zeroForOne := strings.EqualFold(tokenIn.Address, pool.Token0.Address)

	// If tick data is available, use multi-tick simulation that matches the
	// on-chain swap loop exactly. Otherwise fall back to single-tick approximation.
	pool.mu.RLock()
	ticks := pool.Ticks
	sqrtP := pool.SqrtPriceX96
	liq := pool.Liquidity
	curTick := pool.Tick
	pool.mu.RUnlock()

	if len(ticks) > 0 && pool.TickSpacing > 0 {
		return s.multiTickSwap(sqrtP, liq, curTick, ticks, pool.TickSpacing, amountIn, feePips, zeroForOne)
	}
	return big.NewInt(0)
}

// multiTickSwap simulates the Uniswap V3 swap loop, crossing tick boundaries
// with the correct liquidity at each level. This matches the on-chain behaviour
// exactly (modulo rounding) and eliminates the single-tick approximation error
// that caused false-positive/false-negative arb cycles.
func (s UniswapV3Sim) multiTickSwap(
	sqrtP, liq *big.Int,
	curTick int32,
	ticks []TickData,
	tickSpacing int32,
	amountIn *big.Int,
	feePips uint64,
	zeroForOne bool,
) *big.Int {
	// Work with copies so we don't mutate pool state
	sqrtPCur := new(big.Int).Set(sqrtP)
	liqCur := new(big.Int).Set(liq)
	remaining := new(big.Int).Set(amountIn)
	totalOut := new(big.Int)

	feeMul := new(big.Int).SetUint64(1_000_000 - feePips)
	feeDenom := big.NewInt(1_000_000)

	// Max iterations to prevent infinite loops on bad data
	for i := 0; i < 200 && remaining.Sign() > 0 && liqCur.Sign() > 0; i++ {
		// Find the next initialized tick in the swap direction
		nextTick, found := s.nextInitializedTick(ticks, curTick, zeroForOne)
		if !found {
			// No initialized tick in swap direction within the loaded bitmap.
			// We do NOT know where the next liquidity change is, so consuming
			// `remaining` at current liquidity would assume liquidity persists
			// to infinity past the bitmap edge — an over-estimate that
			// fabricates phantom +bps cycles. CLAUDE.md hard rule says
			// return 0 in this case.
			//
			// EXCEPTION: if `remaining` fits within the current tick range
			// at this liquidity level (i.e., would not need to cross any
			// boundary), the answer is reliable. We can't test exact fit
			// without a boundary, but we can use a synthetic boundary at the
			// nearest tick-spacing-aligned tick on the swap-direction side
			// to be conservative. If maxIn against that synthetic boundary
			// is >= remaining, we're staying inside the known range and the
			// single-tick formula is exact.
			boundSqrt := s.tickBoundarySqrt(curTick, tickSpacing, zeroForOne)
			maxIn := s.maxAmountInToSqrtPrice(sqrtPCur, boundSqrt, liqCur, feePips, zeroForOne)
			if maxIn != nil && maxIn.Sign() > 0 && remaining.Cmp(maxIn) <= 0 {
				out := s.exactOutput(sqrtPCur, liqCur, remaining, feePips, zeroForOne)
				totalOut.Add(totalOut, out)
				break
			}
			return big.NewInt(0)
		}

		// sqrtPrice at the next tick boundary
		nextSqrtP := s.sqrtPriceAtTick(nextTick)
		if nextSqrtP == nil || nextSqrtP.Sign() == 0 {
			break
		}

		// How much amountIn can we consume before reaching nextSqrtP?
		maxIn := s.maxAmountInToSqrtPrice(sqrtPCur, nextSqrtP, liqCur, feePips, zeroForOne)
		if maxIn == nil || maxIn.Sign() <= 0 {
			// Can't reach the boundary, consume all remaining
			out := s.exactOutput(sqrtPCur, liqCur, remaining, feePips, zeroForOne)
			totalOut.Add(totalOut, out)
			break
		}

		if remaining.Cmp(maxIn) <= 0 {
			// Remaining fits within this tick — final step
			out := s.exactOutput(sqrtPCur, liqCur, remaining, feePips, zeroForOne)
			totalOut.Add(totalOut, out)
			break
		}

		// Consume maxIn, compute output for this tick range
		out := s.exactOutput(sqrtPCur, liqCur, maxIn, feePips, zeroForOne)
		totalOut.Add(totalOut, out)
		remaining.Sub(remaining, maxIn)

		// Cross the tick: update sqrtP and liquidity
		sqrtPCur.Set(nextSqrtP)

		// Apply liquidityNet: when crossing left→right (zeroForOne=false), add;
		// when crossing right→left (zeroForOne=true), subtract.
		liqNet := s.getLiquidityNet(ticks, nextTick)
		if liqNet != nil {
			if zeroForOne {
				liqCur.Sub(liqCur, liqNet)
			} else {
				liqCur.Add(liqCur, liqNet)
			}
		}
		if liqCur.Sign() <= 0 {
			break // no liquidity beyond this tick
		}

		// Deduct fee on the amount consumed to reach boundary.
		// The fee is already accounted for in exactOutput's amountInAfterFee,
		// but maxAmountInToSqrtPrice returns the pre-fee amount. The fee portion
		// doesn't generate output — it's collected by LPs.
		_ = feeMul
		_ = feeDenom

		// Advance current tick past the boundary
		if zeroForOne {
			curTick = nextTick - 1
		} else {
			curTick = nextTick
		}
	}

	return totalOut
}

// nextInitializedTick finds the next initialized tick in the swap direction.
// For zeroForOne (price decreasing): find the highest tick <= curTick.
// For oneForZero (price increasing): find the lowest tick > curTick.
func (s UniswapV3Sim) nextInitializedTick(ticks []TickData, curTick int32, zeroForOne bool) (int32, bool) {
	if zeroForOne {
		// Search backwards for highest tick <= curTick
		for i := len(ticks) - 1; i >= 0; i-- {
			if ticks[i].Tick <= curTick {
				return ticks[i].Tick, true
			}
		}
	} else {
		// Search forwards for lowest tick > curTick
		for i := 0; i < len(ticks); i++ {
			if ticks[i].Tick > curTick {
				return ticks[i].Tick, true
			}
		}
	}
	return 0, false
}

// getLiquidityNet returns the liquidityNet for a specific tick index.
func (s UniswapV3Sim) getLiquidityNet(ticks []TickData, tick int32) *big.Int {
	// Binary search since ticks are sorted
	lo, hi := 0, len(ticks)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if ticks[mid].Tick == tick {
			return ticks[mid].LiquidityNet
		} else if ticks[mid].Tick < tick {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return nil
}

// exactOutput computes the output amount for a swap within a single tick using
// the exact UniV3 sqrt-price formulas.
func (s UniswapV3Sim) exactOutput(sqrtP, liq, amountIn *big.Int, feePips uint64, zeroForOne bool) *big.Int {
	amountInAfterFee := new(big.Int).Mul(amountIn, new(big.Int).SetUint64(1_000_000-feePips))
	amountInAfterFee.Div(amountInAfterFee, big.NewInt(1_000_000))
	if amountInAfterFee.Sign() == 0 {
		return big.NewInt(0)
	}

	if zeroForOne {
		// newSqrtP = L * sqrtP * Q96 / (L * Q96 + amountInAfterFee * sqrtP)
		num := new(big.Int).Mul(liq, sqrtP)
		num.Mul(num, Q96)
		denom := new(big.Int).Mul(liq, Q96)
		denom.Add(denom, new(big.Int).Mul(amountInAfterFee, sqrtP))
		if denom.Sign() == 0 {
			return big.NewInt(0)
		}
		newSqrtP := new(big.Int).Div(num, denom)
		if newSqrtP.Cmp(sqrtP) >= 0 {
			return big.NewInt(0)
		}
		// amountOut = L * (sqrtP - newSqrtP) / Q96
		diff := new(big.Int).Sub(sqrtP, newSqrtP)
		out := new(big.Int).Mul(liq, diff)
		out.Div(out, Q96)
		return out
	}

	// oneForZero: newSqrtP = sqrtP + amountInAfterFee * Q96 / L
	add := new(big.Int).Mul(amountInAfterFee, Q96)
	add.Div(add, liq)
	newSqrtP := new(big.Int).Add(sqrtP, add)
	// amountOut = L * Q96 * (newSqrtP - sqrtP) / (sqrtP * newSqrtP)
	diff := new(big.Int).Sub(newSqrtP, sqrtP)
	num := new(big.Int).Mul(liq, Q96)
	num.Mul(num, diff)
	denom := new(big.Int).Mul(sqrtP, newSqrtP)
	if denom.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Div(num, denom)
}

// tickBoundarySqrt returns the sqrtPrice at the nearest tick boundary in the
// swap direction. zeroForOne → we need the lower bound of the current tick;
// oneForZero → we need the upper bound.
func (s UniswapV3Sim) tickBoundarySqrt(tick int32, spacing int32, zeroForOne bool) *big.Int {
	var boundTick int32
	if zeroForOne {
		// moving left: boundary is the floor tick of the current tick range
		boundTick = (tick / spacing) * spacing
		if tick < 0 && tick%spacing != 0 {
			boundTick -= spacing
		}
	} else {
		// moving right: boundary is the ceiling tick + 1
		boundTick = (tick/spacing)*spacing + spacing
	}
	return s.sqrtPriceAtTick(boundTick)
}

// sqrtPriceAtTick computes sqrtPriceX96 = sqrt(1.0001^tick) * 2^96.
// Uses the same bit-manipulation approach as the Uniswap V3 TickMath library.
func (s UniswapV3Sim) sqrtPriceAtTick(tick int32) *big.Int {
	// Clamp to valid tick range
	if tick > 887272 {
		tick = 887272
	}
	if tick < -887272 {
		tick = -887272
	}

	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}

	// Magic ratios from the Uniswap V3 TickMath contract (in Q128 fixed point).
	// ratio = 2^128 / sqrt(1.0001)^|tick| for each bit of absTick.
	ratio := new(big.Int)
	if absTick&0x1 != 0 {
		ratio, _ = new(big.Int).SetString("fffcb933bd6fad37aa2d162d1a594001", 16)
	} else {
		ratio, _ = new(big.Int).SetString("100000000000000000000000000000000", 16)
	}
	if absTick&0x2 != 0 {
		ratio.Mul(ratio, mustHex("fff97272373d413259a46990580e213a"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x4 != 0 {
		ratio.Mul(ratio, mustHex("fff2e50f5f656932ef12357cf3c7fdcc"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x8 != 0 {
		ratio.Mul(ratio, mustHex("ffe5caca7e10e4e61c3624eaa0941cd0"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x10 != 0 {
		ratio.Mul(ratio, mustHex("ffcb9843d60f6159c9db58835c926644"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x20 != 0 {
		ratio.Mul(ratio, mustHex("ff973b41fa98c081472e6896dfb254c0"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x40 != 0 {
		ratio.Mul(ratio, mustHex("ff2ea16466c96a3843ec78b326b52861"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x80 != 0 {
		ratio.Mul(ratio, mustHex("fe5dee046a99a2a811c461f1969c3053"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x100 != 0 {
		ratio.Mul(ratio, mustHex("fcbe86c7900a88aedcffc83b479aa3a4"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x200 != 0 {
		ratio.Mul(ratio, mustHex("f987a7253ac413176f2b074cf7815e54"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x400 != 0 {
		ratio.Mul(ratio, mustHex("f3392b0822b70005940c7a398e4b70f3"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x800 != 0 {
		ratio.Mul(ratio, mustHex("e7159475a2c29b7443b29c7fa6e889d9"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x1000 != 0 {
		ratio.Mul(ratio, mustHex("d097f3bdfd2022b8845ad8f792aa5825"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x2000 != 0 {
		ratio.Mul(ratio, mustHex("a9f746462d870fdf8a65dc1f90e061e5"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x4000 != 0 {
		ratio.Mul(ratio, mustHex("70d869a156d2a1b890bb3df62baf32f7"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x8000 != 0 {
		ratio.Mul(ratio, mustHex("31be135f97d08fd981231505542fcfa6"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x10000 != 0 {
		ratio.Mul(ratio, mustHex("9aa508b5b7a84e1c677de54f3e99bc9"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x20000 != 0 {
		ratio.Mul(ratio, mustHex("5d6af8dedb81196699c329225ee604"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x40000 != 0 {
		ratio.Mul(ratio, mustHex("2216e584f5fa1ea926041bedfe98"))
		ratio.Rsh(ratio, 128)
	}
	if absTick&0x80000 != 0 {
		ratio.Mul(ratio, mustHex("48a170391f7dc42444e8fa2"))
		ratio.Rsh(ratio, 128)
	}

	// For positive ticks, invert: ratio = MaxUint256 / ratio
	if tick > 0 {
		maxU256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		ratio = new(big.Int).Div(maxU256, ratio)
	}

	// Convert from Q128 to Q96: shift right by 32, round up if remainder > 0
	q32 := new(big.Int).Lsh(big.NewInt(1), 32)
	rem := new(big.Int).Mod(ratio, q32)
	sqrtQ96 := new(big.Int).Rsh(ratio, 32)
	if rem.Sign() > 0 {
		sqrtQ96.Add(sqrtQ96, big.NewInt(1))
	}
	return sqrtQ96
}

// maxAmountInToSqrtPrice computes the maximum amountIn (before fee) that moves
// the price to targetSqrt without crossing it. Returns nil if the target is
// already past the current price (wrong direction).
func (s UniswapV3Sim) maxAmountInToSqrtPrice(sqrtP, targetSqrt, liq *big.Int, feePips uint64, zeroForOne bool) *big.Int {
	if zeroForOne {
		if targetSqrt.Cmp(sqrtP) >= 0 {
			return nil // boundary in wrong direction
		}
		// amountInAfterFee needed to move sqrtP → targetSqrt:
		//   token0 in = L * Q96 * (sqrtP - target) / (target * sqrtP)
		diff := new(big.Int).Sub(sqrtP, targetSqrt)
		num := new(big.Int).Mul(liq, Q96)
		num.Mul(num, diff)
		denom := new(big.Int).Mul(targetSqrt, sqrtP)
		if denom.Sign() == 0 {
			return nil
		}
		amountInAfterFee := new(big.Int).Div(num, denom)
		// gross amountIn = amountInAfterFee * 1e6 / (1e6 - feePips)
		remaining := uint64(1_000_000) - feePips
		if remaining == 0 {
			return nil
		}
		gross := new(big.Int).Mul(amountInAfterFee, big.NewInt(1_000_000))
		gross.Div(gross, new(big.Int).SetUint64(remaining))
		return gross
	}
	// oneForZero
	if targetSqrt.Cmp(sqrtP) <= 0 {
		return nil
	}
	// amountInAfterFee needed to move sqrtP → targetSqrt:
	//   token1 in = L * (targetSqrt - sqrtP) / Q96
	diff := new(big.Int).Sub(targetSqrt, sqrtP)
	amountInAfterFee := new(big.Int).Mul(liq, diff)
	amountInAfterFee.Div(amountInAfterFee, Q96)
	remaining := uint64(1_000_000) - feePips
	if remaining == 0 {
		return nil
	}
	gross := new(big.Int).Mul(amountInAfterFee, big.NewInt(1_000_000))
	gross.Div(gross, new(big.Int).SetUint64(remaining))
	return gross
}

func mustHex(h string) *big.Int {
	n, ok := new(big.Int).SetString(h, 16)
	if !ok {
		panic("mustHex: invalid hex: " + h)
	}
	return n
}

// ─── Curve StableSwap ──────────────────────────────────────────────────────

type CurveSim struct{}

func (s CurveSim) AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	if pool.Reserve0 == nil || pool.Reserve1 == nil || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}
	amp := pool.AmpFactor
	if amp == 0 {
		amp = 100 // default
	}

	// Normalize to 18 decimals
	x := normalize(pool.Reserve0, pool.Token0.Decimals)
	y := normalize(pool.Reserve1, pool.Token1.Decimals)

	D := s.getD(x, y, amp)

	var xNew, oldReserve *big.Int
	var outDecimals uint8
	if tokenIn.Address == pool.Token0.Address {
		xNew = new(big.Int).Add(normalize(pool.Reserve0, pool.Token0.Decimals),
			normalize(amountIn, pool.Token0.Decimals))
		oldReserve = new(big.Int).Set(y)
		outDecimals = pool.Token1.Decimals
	} else {
		xNew = new(big.Int).Add(normalize(pool.Reserve1, pool.Token1.Decimals),
			normalize(amountIn, pool.Token1.Decimals))
		oldReserve = new(big.Int).Set(x)
		outDecimals = pool.Token0.Decimals
	}

	yNew := s.getY(xNew, D, amp)
	if yNew == nil || yNew.Cmp(oldReserve) >= 0 {
		return big.NewInt(0)
	}

	raw := new(big.Int).Sub(oldReserve, yNew)
	// Apply fee using Curve's native 1e10 precision when available.
	// CurveFee1e10=100000 means 0.001% — FeeBps would round this to 0.
	if pool.CurveFee1e10 > 0 {
		feeDenom := big.NewInt(10_000_000_000) // 1e10
		feeNumer := new(big.Int).Sub(feeDenom, new(big.Int).SetUint64(pool.CurveFee1e10))
		raw.Mul(raw, feeNumer)
		raw.Div(raw, feeDenom)
	} else if pool.FeeBps > 0 {
		feeDenom := big.NewInt(10_000)
		feeNumer := new(big.Int).Sub(feeDenom, big.NewInt(int64(pool.FeeBps)))
		raw.Mul(raw, feeNumer)
		raw.Div(raw, feeDenom)
	}

	return denormalize(raw, outDecimals)
}

// getD computes the StableSwap invariant D via Newton's method.
// Ann = A * N_COINS (Curve convention; for 2-pool: amp * 2).
func (s CurveSim) getD(x, y *big.Int, amp uint64) *big.Int {
	S := new(big.Int).Add(x, y)
	if S.Sign() == 0 {
		return big.NewInt(0)
	}
	Ann := new(big.Int).SetUint64(amp * 2) // A * N (Curve vyper: Ann = amp * N_COINS)
	D := new(big.Int).Set(S)

	for i := 0; i < 255; i++ {
		Dprev := new(big.Int).Set(D)
		D2 := new(big.Int).Mul(D, D)
		D3 := new(big.Int).Mul(D2, D)
		denom4xy := new(big.Int).Mul(big.NewInt(4), new(big.Int).Mul(x, y))
		if denom4xy.Sign() == 0 {
			break
		}
		DP := new(big.Int).Div(D3, denom4xy)

		annS := new(big.Int).Mul(Ann, S)
		dp2 := new(big.Int).Mul(DP, big.NewInt(2))
		num := new(big.Int).Add(annS, dp2)
		num.Mul(num, D)

		ann1 := new(big.Int).Sub(Ann, big.NewInt(1))
		den := new(big.Int).Mul(ann1, D)
		dp3 := new(big.Int).Mul(DP, big.NewInt(3))
		den.Add(den, dp3)

		if den.Sign() == 0 {
			break
		}
		D.Div(num, den)

		diff := new(big.Int).Abs(new(big.Int).Sub(D, Dprev))
		if diff.Cmp(big.NewInt(1)) <= 0 {
			break
		}
	}
	return D
}

// getY solves the StableSwap invariant for y given x and D.
func (s CurveSim) getY(xNew, D *big.Int, amp uint64) *big.Int {
	Ann := new(big.Int).SetUint64(amp * 2) // A * N_COINS

	D2 := new(big.Int).Mul(D, D)
	D3 := new(big.Int).Mul(D2, D)
	c := new(big.Int).Mul(D3, big.NewInt(1))
	denom := new(big.Int).Mul(big.NewInt(4), new(big.Int).Mul(xNew, Ann))
	if denom.Sign() == 0 {
		return nil
	}
	c.Div(c, denom)

	b := new(big.Int).Add(xNew, new(big.Int).Div(D, Ann))
	b.Sub(b, D)

	y := new(big.Int).Set(D)
	for i := 0; i < 255; i++ {
		yPrev := new(big.Int).Set(y)
		y2 := new(big.Int).Mul(y, y)
		num := new(big.Int).Add(y2, c)
		den := new(big.Int).Add(new(big.Int).Mul(big.NewInt(2), y), b)
		if den.Sign() == 0 {
			break
		}
		y.Div(num, den)
		diff := new(big.Int).Abs(new(big.Int).Sub(y, yPrev))
		if diff.Cmp(big.NewInt(1)) <= 0 {
			break
		}
	}
	return y
}

// normalize scales a token amount to 18 decimals.
func normalize(amount *big.Int, decimals uint8) *big.Int {
	if decimals == 18 {
		return new(big.Int).Set(amount)
	}
	if decimals < 18 {
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(18-decimals)), nil)
		return new(big.Int).Mul(amount, factor)
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-18)), nil)
	return new(big.Int).Div(amount, factor)
}

// denormalize scales from 18 decimals back to native decimals.
func denormalize(amount *big.Int, decimals uint8) *big.Int {
	if decimals == 18 {
		return new(big.Int).Set(amount)
	}
	if decimals < 18 {
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(18-decimals)), nil)
		return new(big.Int).Div(amount, factor)
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-18)), nil)
	return new(big.Int).Mul(amount, factor)
}

// ─── Balancer V2 Weighted Pool ─────────────────────────────────────────────

type BalancerWeightedSim struct{}

// AmountOut implements the Balancer weighted product formula:
//
//	amountOut = balanceOut * (1 - (balanceIn / (balanceIn + amountInAfterFee)) ^ (wIn/wOut))
func (s BalancerWeightedSim) AmountOut(pool *Pool, tokenIn *Token, amountIn *big.Int) *big.Int {
	if pool.Reserve0 == nil || pool.Reserve1 == nil || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}
	if pool.Weight0 == 0 || pool.Weight1 == 0 {
		return big.NewInt(0)
	}

	var balanceIn, balanceOut *big.Int
	var wIn, wOut float64
	if tokenIn.Address == pool.Token0.Address {
		balanceIn, balanceOut = pool.Reserve0, pool.Reserve1
		wIn, wOut = pool.Weight0, pool.Weight1
	} else {
		balanceIn, balanceOut = pool.Reserve1, pool.Reserve0
		wIn, wOut = pool.Weight1, pool.Weight0
	}

	// Apply swap fee: amountInAfterFee = amountIn * (1 - feeBps/10000)
	feeDenom := big.NewInt(10_000)
	feeNumer := new(big.Int).Sub(feeDenom, big.NewInt(int64(pool.FeeBps)))
	amountInAfterFee := new(big.Int).Mul(amountIn, feeNumer)
	amountInAfterFee.Div(amountInAfterFee, feeDenom)

	// Convert to float64 for power computation (precision is sufficient for simulation)
	bInF, _ := new(big.Float).SetInt(balanceIn).Float64()
	bOutF, _ := new(big.Float).SetInt(balanceOut).Float64()
	aInF, _ := new(big.Float).SetInt(amountInAfterFee).Float64()

	if bInF == 0 || bOutF == 0 {
		return big.NewInt(0)
	}

	// ratio = balanceIn / (balanceIn + amountInAfterFee)
	ratio := bInF / (bInF + aInF)
	// power = ratio ^ (wIn/wOut)
	power := math.Pow(ratio, wIn/wOut)
	// amountOut = balanceOut * (1 - power)
	amountOutF := bOutF * (1.0 - power)
	if amountOutF <= 0 {
		return big.NewInt(0)
	}

	result, _ := new(big.Float).SetFloat64(amountOutF).Int(nil)
	return result
}

// ─── Cycle simulation ──────────────────────────────────────────────────────

// SimHop holds the per-hop amounts from a simulation run, in human-readable
// (decimal-adjusted) form. Used to propagate USD prices the same way arbscan does.
type SimHop struct {
	TokenIn   string  // lower-case address
	AmountIn  float64 // human units (divided by 10^decimals)
	TokenOut  string  // lower-case address
	AmountOut float64 // human units
}

type SimulationResult struct {
	AmountIn   *big.Int
	AmountOut  *big.Int
	Profit     *big.Int
	// ProfitBps is the simulated profit margin expressed in basis points,
	// with sub-bp resolution. Previously int64 — the integer truncation made
	// a 2.2-bps arb indistinguishable from a 2.0-bps arb, which matters when
	// the gate (min_sim_profit_bps) is set below 1.0. DB columns remain
	// INTEGER; writes round to int64 at the boundary.
	ProfitBps  float64
	Path       string
	Viable     bool
	HopAmounts []SimHop
}

func SimulateCycle(cycle Cycle, amountIn *big.Int) SimulationResult {
	current := new(big.Int).Set(amountIn)
	path := ""
	if len(cycle.Edges) > 0 {
		path = cycle.Edges[0].TokenIn.Symbol
	}

	var hops []SimHop
	for _, edge := range cycle.Edges {
		sim := SimulatorFor(edge.Pool.DEX)
		out := sim.AmountOut(edge.Pool, edge.TokenIn, current)
		if out == nil || out.Sign() <= 0 {
			return SimulationResult{Viable: false, Path: path}
		}
		inF, _ := new(big.Float).Mul(new(big.Float).SetInt(current), new(big.Float).SetFloat64(edge.TokenIn.Scalar)).Float64()
		outF, _ := new(big.Float).Mul(new(big.Float).SetInt(out), new(big.Float).SetFloat64(edge.TokenOut.Scalar)).Float64()
		hops = append(hops, SimHop{
			TokenIn:   strings.ToLower(edge.TokenIn.Address),
			AmountIn:  inF,
			TokenOut:  strings.ToLower(edge.TokenOut.Address),
			AmountOut: outF,
		})
		path += fmt.Sprintf(" ->[%s] %s", edge.Pool.DEX, edge.TokenOut.Symbol)
		current = out
	}

	profit := new(big.Int).Sub(current, amountIn)
	profitBps := float64(0)
	if amountIn.Sign() > 0 && profit.Sign() > 0 {
		pf, _ := new(big.Float).SetInt(profit).Float64()
		af, _ := new(big.Float).SetInt(amountIn).Float64()
		if af > 0 {
			profitBps = (pf / af) * 10_000
		}
	}

	// Sanity check: output > 3× input (300% profit) indicates bad pool state
	// (e.g. wrong sqrtPriceX96, mismatched decimals, stale reserves).
	// Real arb on Arbitrum never exceeds a few percent; 3× is extremely generous.
	viable := profit.Sign() > 0
	if viable && current.Cmp(new(big.Int).Mul(amountIn, big.NewInt(3))) > 0 {
		viable = false
	}

	return SimulationResult{
		AmountIn:   amountIn,
		AmountOut:  current,
		Profit:     profit,
		ProfitBps:  profitBps,
		Path:       path,
		Viable:     viable,
		HopAmounts: hops,
	}
}
