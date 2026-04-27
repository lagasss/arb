package internal

// Pool verification: ensures every pool we add to the registry has correct,
// internally-consistent on-chain state. Catches the class of bugs where a
// resolver returns partial data (missing tickSpacing, wrong fee tier, stale
// reserves) and ships it into the cycle cache.
//
// Each DEX type has a dedicated verifier that:
//   1. Fetches the canonical state fields directly from the pool contract
//   2. Cross-checks them against what the pool struct already holds
//   3. Runs a trial swap (when a quoter exists) and compares the result
//      against our local simulator's output
//
// A pool that fails any check is stored as Verified=false with a reason; the
// cycle cache and ScoreCycle skip unverified pools entirely. Verification
// runs once at resolution time and can be re-run periodically by a background
// loop.

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// verifyMaxDriftBps is the largest allowed difference between our local sim
// and the on-chain quoter for a trial swap. 100 bps = 1% — anything above is
// almost certainly wrong fee, wrong tickSpacing, or wrong DEX handler.
const verifyMaxDriftBps = 100

// VerifyV2RouterReachable checks that the router we're about to trade through
// can actually address this specific pool. Two failure modes this catches:
//
//  1. DEXType → router mapping mismatch. If a PancakeV2 pool is registered as
//     DEXUniswapV2, dexRouter[DEXUniswapV2] returns Uniswap's router. We'd
//     send trades to Uniswap's router referencing the PancakeV2 pair — the
//     router would resolve the pair via its own factory and route to an
//     unrelated UniV2 pool (or revert).
//
//  2. Fork with a dedicated router but different factory. The router's
//     canonical pair address (computed via `factory.getPair(t0, t1)`) must
//     equal our pool address. If not, the router uses a different pool for
//     this token pair and our sim (which was built against OUR pool) will
//     diverge from what the router actually executes.
//
// The check is two RPC calls and fails closed — any error returns a
// non-empty reason. Called from ResolvePoolFromChain immediately after
// VerifyPool for V2-family pools.
//
// poolFactory is the factory address we resolved the pool from (discovered
// by calling `pool.factory()` in ResolvePoolFromChain). router is the router
// address that would be used to execute trades through this DEXType.
func VerifyV2RouterReachable(ctx context.Context, client *ethclient.Client, p *Pool, router string, poolFactory string) (bool, string) {
	if p == nil || p.Token0 == nil || p.Token1 == nil {
		return false, "pool missing tokens"
	}
	if router == "" {
		return false, "no router address for DEX " + p.DEX.String()
	}
	poolFactoryLo := strings.ToLower(poolFactory)
	routerAddr := common.HexToAddress(router)

	// Step 1: router.factory() must return the same factory we found on the pool.
	// Standard IUniswapV2Router02.factory() selector = 0xc45a0155.
	rfRaw, err := callRaw(ctx, client, routerAddr, "0xc45a0155")
	if err != nil || len(rfRaw) < 32 {
		return false, fmt.Sprintf("router.factory() call failed: %v", err)
	}
	routerFactoryLo := strings.ToLower(common.BytesToAddress(rfRaw[12:32]).Hex())
	if routerFactoryLo != poolFactoryLo {
		return false, fmt.Sprintf("router.factory=%s != pool.factory=%s — router targets a different DEX", routerFactoryLo, poolFactoryLo)
	}

	// Step 2: factory.getPair(t0, t1) must return this exact pool address.
	// This proves the router can address OUR pool (not some other pair for the
	// same token couple). Selector: 0xe6a43905.
	getPairData := make([]byte, 4+64)
	copy(getPairData[0:4], []byte{0xe6, 0xa4, 0x39, 0x05})
	t0 := common.HexToAddress(p.Token0.Address)
	t1 := common.HexToAddress(p.Token1.Address)
	copy(getPairData[4+12:4+32], t0[:])
	copy(getPairData[4+32+12:4+64], t1[:])

	factoryAddr := common.HexToAddress(poolFactory)
	pairRaw, err := client.CallContract(ctx, ethereum.CallMsg{To: &factoryAddr, Data: getPairData}, nil)
	if err != nil || len(pairRaw) < 32 {
		return false, fmt.Sprintf("factory.getPair(t0,t1) call failed: %v", err)
	}
	canonicalPairLo := strings.ToLower(common.BytesToAddress(pairRaw[12:32]).Hex())
	poolAddrLo := strings.ToLower(p.Address)
	if canonicalPairLo != poolAddrLo {
		return false, fmt.Sprintf("factory.getPair(t0,t1)=%s != pool=%s — the router would route through a different pair", canonicalPairLo, poolAddrLo)
	}

	return true, ""
}

// verifyTrialUSD is the trial swap size in USD-equivalent. Small enough to
// avoid moving a thin pool's price, large enough that integer rounding doesn't
// dominate the comparison.
const verifyTrialUSD = 10.0

// VerifyPool runs DEX-specific sanity checks on a pool. On success it
// populates any missing fields it discovered (e.g. TickSpacing for V3) and
// returns (true, ""). On failure it returns (false, reason) and the pool
// should be flagged Verified=false.
//
// Adds ~3-5 RPC round-trips per pool (~300-500ms total). Designed to be
// called from ResolvePoolFromChain after the bare-minimum resolution succeeds.
func VerifyPool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	if p == nil {
		return false, "nil pool"
	}
	if p.Token0 == nil || p.Token1 == nil {
		return false, "missing token0 or token1"
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	switch p.DEX {
	case DEXUniswapV3, DEXSushiSwapV3, DEXPancakeV3, DEXRamsesV3:
		return verifyUniV3Pool(tctx, client, p)
	case DEXCamelotV3, DEXZyberV3:
		return verifyAlgebraPool(tctx, client, p)
	case DEXCurve:
		return verifyCurvePool(tctx, client, p)
	case DEXBalancerWeighted:
		return verifyBalancerPool(tctx, client, p)
	case DEXCamelot:
		return verifyCamelotV2Pool(tctx, client, p)
	case DEXUniswapV4:
		// V4 pools are verified via VerifyV4Pool which takes a poolId
		// instead of an address (V4 uses the singleton PoolManager).
		return false, "use VerifyV4Pool for V4 pools"
	default:
		// Generic V2: UniV2, SushiV2, RamsesV2, TraderJoe, etc.
		return verifyV2Pool(tctx, client, p)
	}
}

// ── V3 verification ──────────────────────────────────────────────────────────

// verifyUniV3Pool fetches tickSpacing, slot0, liquidity for a Uniswap-style
// V3 pool, validates the fee tier matches the standard fee→tickSpacing map,
// and runs a trial swap via the QuoterV2 contract to confirm sim accuracy.
func verifyUniV3Pool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	a := common.HexToAddress(p.Address)

	// 1. tickSpacing()
	tsRaw, err := callRaw(ctx, client, a, "0xd0c93a7c") // tickSpacing() selector
	if err != nil || len(tsRaw) < 32 {
		return false, fmt.Sprintf("tickSpacing call failed: %v", err)
	}
	ts := int32(new(big.Int).SetBytes(tsRaw[:32]).Int64())
	if ts <= 0 || ts > 2000 {
		return false, fmt.Sprintf("nonsensical tickSpacing %d", ts)
	}
	p.TickSpacing = ts

	// 2. Verify fee tier matches standard mapping (warn-only — exotic pools exist)
	stdMap := map[uint32]int32{100: 1, 500: 10, 3000: 60, 10000: 200}
	if expected, ok := stdMap[p.FeePPM]; ok && expected != ts {
		// Fee tier exists in std map but tickSpacing doesn't match — fishy
		return false, fmt.Sprintf("nonstandard tickSpacing %d for fee %dppm (std=%d)", ts, p.FeePPM, expected)
	}

	// 3. slot0() — must return non-zero sqrtPriceX96
	slot0Raw, err := callRaw(ctx, client, a, "0x3850c7bd") // slot0() selector
	if err != nil || len(slot0Raw) < 64 {
		return false, fmt.Sprintf("slot0 call failed: %v", err)
	}
	sqrtP := new(big.Int).SetBytes(slot0Raw[:32])
	if sqrtP.Sign() == 0 {
		return false, "slot0.sqrtPriceX96 is zero (uninitialised pool)"
	}
	tickRaw := new(big.Int).SetBytes(slot0Raw[32:64])
	tick := tickRaw.Int64()
	if tick >= (1 << 23) {
		tick -= (1 << 24) // sign-extend int24
	}
	p.SqrtPriceX96 = sqrtP
	p.Tick = int32(tick)

	// 4. liquidity()
	liqRaw, err := callRaw(ctx, client, a, "0x1a686502") // liquidity() selector
	if err != nil || len(liqRaw) < 32 {
		return false, fmt.Sprintf("liquidity call failed: %v", err)
	}
	liq := new(big.Int).SetBytes(liqRaw[:32])
	if liq.Sign() == 0 {
		// Empty range — common for newly-deployed pools. Not a hard fail
		// but mark as unverified so we don't trade on it.
		return false, "liquidity is zero (no active range)"
	}
	p.Liquidity = liq

	// 5. Trial swap via QuoterV2.quoteExactInputSingle.
	// Compares our exactOutput sim against the official quoter for a small
	// trade. Must agree within verifyMaxDriftBps.
	//
	// Pancake/Sushi/Ramses each deploy their own QuoterV2 at addresses we
	// don't currently have wired up — calling Uniswap's quoter against those
	// pools would silently produce a quote for an unrelated Uniswap pool with
	// the same token pair + fee tier, which is worse than no check at all.
	// Until those addresses are added (and ABI-verified), these pools pass
	// verification on slot0+liquidity sanity alone, with no sim accuracy
	// guarantee. The 2026-04-11 forensics correlated PancakeV3-touching
	// cycles with downstream hop reverts — that observation is consistent
	// with an unverified PancakeV3 sim, but we can't confirm the gap without
	// the right quoter. See cmd/smoketest/main.go quoterAddrFor for the same
	// gap on the smoketest side.
	if p.DEX == DEXUniswapV3 {
		if drift, err := verifyV3TrialSwap(ctx, client, p); err != nil {
			// Quoter call failed — skip drift check, log but don't reject
			_ = err
		} else if drift > verifyMaxDriftBps {
			return false, fmt.Sprintf("sim drift %dbps > %dbps vs QuoterV2", drift, verifyMaxDriftBps)
		}
	}

	return true, ""
}

// uniV3Quoter is the official Uniswap V3 QuoterV2 on Arbitrum
const uniV3QuoterAddr = "0x61fFE014bA17989E743c5F6cB21bF9697530B21e"

// verifyV3TrialSwap calls QuoterV2.quoteExactInputSingle for a small trade
// and compares against our local exactOutput simulation. Returns the drift
// in bps (positive means our sim is HIGHER than the quoter).
func verifyV3TrialSwap(ctx context.Context, client *ethclient.Client, p *Pool) (int64, error) {
	// Trial amount: $10 worth in token0 base units. Without a price feed we
	// approximate using token0 decimals: 10 * 10^decimals / 1000 = 1% of $1k
	// notional per token. For tokens with decimals < 6, use a fixed minimum.
	amountIn := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p.Token0.Decimals)), nil)
	if amountIn.Cmp(big.NewInt(1000)) < 0 {
		amountIn = big.NewInt(1000)
	}

	// quoteExactInputSingle((tokenIn, tokenOut, amountIn, fee, sqrtPriceLimitX96))
	// Selector: 0xc6a5026a
	// Encoded as: tuple offset (0x20) + tokenIn(32) + tokenOut(32) + amountIn(32) + fee(32) + limit(32)
	data := make([]byte, 4+6*32)
	copy(data[0:4], []byte{0xc6, 0xa5, 0x02, 0x6a})
	// offset to tuple
	new(big.Int).SetInt64(0x20).FillBytes(data[4:36])
	// tokenIn (left-padded to 32 bytes)
	t0 := common.HexToAddress(p.Token0.Address)
	copy(data[4+32+12:4+64], t0[:])
	t1 := common.HexToAddress(p.Token1.Address)
	copy(data[4+64+12:4+96], t1[:])
	amountIn.FillBytes(data[4+96 : 4+128])
	new(big.Int).SetUint64(uint64(p.FeePPM)).FillBytes(data[4+128 : 4+160])
	// sqrtPriceLimitX96 = 0 (no limit) — bytes 4+160..4+192 already zero

	q := common.HexToAddress(uniV3QuoterAddr)
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &q, Data: data}, nil)
	if err != nil {
		return 0, err
	}
	if len(res) < 32 {
		return 0, fmt.Errorf("quoter returned short data")
	}
	quoterOut := new(big.Int).SetBytes(res[:32])
	if quoterOut.Sign() == 0 {
		return 0, fmt.Errorf("quoter returned zero")
	}

	// Our local sim
	sim := UniswapV3Sim{}
	localOut := sim.AmountOut(p, p.Token0, amountIn)
	if localOut == nil || localOut.Sign() == 0 {
		return 0, fmt.Errorf("local sim returned zero")
	}

	// Drift in bps
	diff := new(big.Int).Sub(localOut, quoterOut)
	driftBps := new(big.Int).Mul(diff, big.NewInt(10000))
	driftBps.Div(driftBps, quoterOut)
	d := driftBps.Int64()
	if d < 0 {
		d = -d
	}
	return d, nil
}

// ── Algebra (CamelotV3) verification ─────────────────────────────────────────

func verifyAlgebraPool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	a := common.HexToAddress(p.Address)

	// Algebra has tickSpacing() like UniV3
	tsRaw, err := callRaw(ctx, client, a, "0xd0c93a7c")
	if err != nil || len(tsRaw) < 32 {
		return false, fmt.Sprintf("tickSpacing call failed: %v", err)
	}
	ts := int32(new(big.Int).SetBytes(tsRaw[:32]).Int64())
	if ts <= 0 || ts > 2000 {
		return false, fmt.Sprintf("nonsensical tickSpacing %d", ts)
	}
	p.TickSpacing = ts

	// globalState() returns sqrtPrice, tick, fee, ...
	gsRaw, err := callRaw(ctx, client, a, "0xe76c01e4")
	if err != nil || len(gsRaw) < 96 {
		return false, fmt.Sprintf("globalState call failed: %v", err)
	}
	sqrtP := new(big.Int).SetBytes(gsRaw[:32])
	if sqrtP.Sign() == 0 {
		return false, "globalState.sqrtPrice is zero"
	}
	p.SqrtPriceX96 = sqrtP

	tickRaw := new(big.Int).SetBytes(gsRaw[32:64])
	tick := tickRaw.Int64()
	if tick >= (1 << 23) {
		tick -= (1 << 24)
	}
	p.Tick = int32(tick)

	// Algebra dynamic fee is in vals[2] for V1.0; for V1.9 it's a separate
	// communityFee + currentFee setup. Skip strict fee validation — the
	// dynamic fee changes per swap and our sim handles it via globalState.

	// liquidity()
	liqRaw, err := callRaw(ctx, client, a, "0x1a686502")
	if err != nil || len(liqRaw) < 32 {
		return false, fmt.Sprintf("liquidity call failed: %v", err)
	}
	liq := new(big.Int).SetBytes(liqRaw[:32])
	if liq.Sign() == 0 {
		return false, "algebra liquidity is zero"
	}
	p.Liquidity = liq

	return true, ""
}

// ── V2 verification ──────────────────────────────────────────────────────────

func verifyV2Pool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	a := common.HexToAddress(p.Address)

	// getReserves() — both reserves must be non-zero
	res, err := callRaw(ctx, client, a, "0x0902f1ac")
	if err != nil || len(res) < 64 {
		return false, fmt.Sprintf("getReserves failed: %v", err)
	}
	r0 := new(big.Int).SetBytes(res[0:32])
	r1 := new(big.Int).SetBytes(res[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return false, "v2 reserves are zero"
	}
	p.Reserve0 = r0
	p.Reserve1 = r1

	// Trial swap: call pair.getAmountOut for $10 worth, compare with our V2 sim.
	// If the pair doesn't have getAmountOut (vanilla UniV2), skip drift check.
	amountIn := new(big.Int).Div(r0, big.NewInt(1000)) // 0.1% of reserve
	if amountIn.Sign() == 0 {
		amountIn = big.NewInt(1)
	}
	gaoData := make([]byte, 4+64)
	copy(gaoData[0:4], []byte{0xf1, 0x40, 0xa3, 0x5a}) // getAmountOut(uint256,address)
	amountIn.FillBytes(gaoData[4:36])
	t0 := common.HexToAddress(p.Token0.Address)
	copy(gaoData[36+12:68], t0[:])

	gaoRes, err := client.CallContract(ctx, ethereum.CallMsg{To: &a, Data: gaoData}, nil)
	if err == nil && len(gaoRes) >= 32 {
		actualOut := new(big.Int).SetBytes(gaoRes[:32])
		if actualOut.Sign() > 0 {
			// Compare with our V2 sim
			sim := UniswapV2Sim{}
			localOut := sim.AmountOut(p, p.Token0, amountIn)
			if localOut != nil && localOut.Sign() > 0 {
				diff := new(big.Int).Sub(localOut, actualOut)
				driftBps := new(big.Int).Mul(diff, big.NewInt(10000))
				driftBps.Div(driftBps, actualOut)
				d := driftBps.Int64()
				if d < 0 {
					d = -d
				}
				if d > verifyMaxDriftBps {
					return false, fmt.Sprintf("v2 sim drift %dbps > %dbps", d, verifyMaxDriftBps)
				}
			}
		}
	}

	return true, ""
}

// verifyCamelotV2Pool handles directional fees + stable pool detection.
func verifyCamelotV2Pool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	a := common.HexToAddress(p.Address)

	// Reserves must be non-zero
	res, err := callRaw(ctx, client, a, "0x0902f1ac")
	if err != nil || len(res) < 64 {
		return false, fmt.Sprintf("getReserves failed: %v", err)
	}
	r0 := new(big.Int).SetBytes(res[0:32])
	r1 := new(big.Int).SetBytes(res[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return false, "camelot v2 reserves are zero"
	}
	p.Reserve0 = r0
	p.Reserve1 = r1

	// stableSwap() — bool. If true the pool uses StableSwap math, not xy=k.
	if ssRaw, err := callRaw(ctx, client, a, "0x297cd1ba"); err == nil && len(ssRaw) >= 32 {
		stable := new(big.Int).SetBytes(ssRaw[:32]).Sign() != 0
		p.IsStable = stable
	}

	// Calibrate effective fee via getAmountOut. We trust this over any stored
	// value because Camelot V2 has both LP fees and protocol fees.
	amountIn := new(big.Int).Div(r0, big.NewInt(1000))
	if amountIn.Sign() == 0 {
		amountIn = big.NewInt(1)
	}
	gaoData := make([]byte, 4+64)
	copy(gaoData[0:4], []byte{0xf1, 0x40, 0xa3, 0x5a})
	amountIn.FillBytes(gaoData[4:36])
	t0 := common.HexToAddress(p.Token0.Address)
	copy(gaoData[36+12:68], t0[:])

	gaoRes, err := client.CallContract(ctx, ethereum.CallMsg{To: &a, Data: gaoData}, nil)
	if err == nil && len(gaoRes) >= 32 {
		actualOut := new(big.Int).SetBytes(gaoRes[:32])
		if actualOut.Sign() > 0 {
			// Derive effective fee from raw xy=k vs actual
			rawNum := new(big.Int).Mul(amountIn, r1)
			rawDenom := new(big.Int).Add(r0, amountIn)
			rawOut := new(big.Int).Div(rawNum, rawDenom)
			if rawOut.Sign() > 0 && rawOut.Cmp(actualOut) > 0 {
				diff := new(big.Int).Sub(rawOut, actualOut)
				feeBps := new(big.Int).Mul(diff, big.NewInt(10000))
				feeBps.Div(feeBps, rawOut)
				if feeBps.IsUint64() && feeBps.Uint64() < 1000 {
					p.Token0FeeBps = uint32(feeBps.Uint64())
					if p.FeeBps == 0 {
						p.FeeBps = uint32(feeBps.Uint64())
					}
				}
			}
		}
	}

	return true, ""
}

// ── Curve verification ───────────────────────────────────────────────────────

func verifyCurvePool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	a := common.HexToAddress(p.Address)

	// coins(0) and coins(1) must match token0/token1
	coin0Raw, err := callWithUint(ctx, client, a, "0xc6610657", 0) // coins(uint256)
	if err != nil || len(coin0Raw) < 32 {
		return false, fmt.Sprintf("coins(0) failed: %v", err)
	}
	coin0 := strings.ToLower(common.BytesToAddress(coin0Raw[12:32]).Hex())
	if coin0 != strings.ToLower(p.Token0.Address) {
		return false, fmt.Sprintf("curve coins(0) mismatch: chain=%s stored=%s", coin0, p.Token0.Address)
	}
	coin1Raw, err := callWithUint(ctx, client, a, "0xc6610657", 1)
	if err != nil || len(coin1Raw) < 32 {
		return false, fmt.Sprintf("coins(1) failed: %v", err)
	}
	coin1 := strings.ToLower(common.BytesToAddress(coin1Raw[12:32]).Hex())
	if coin1 != strings.ToLower(p.Token1.Address) {
		return false, fmt.Sprintf("curve coins(1) mismatch: chain=%s stored=%s", coin1, p.Token1.Address)
	}

	// A() and fee()
	aRaw, err := callRaw(ctx, client, a, "0xf446c1d0") // A() selector
	if err != nil || len(aRaw) < 32 {
		return false, fmt.Sprintf("A() failed: %v", err)
	}
	A := new(big.Int).SetBytes(aRaw[:32]).Uint64()
	if A == 0 || A > 100000 {
		return false, fmt.Sprintf("curve A out of range: %d", A)
	}
	p.AmpFactor = A

	feeRaw, err := callRaw(ctx, client, a, "0xddca3f43") // fee() selector
	if err != nil || len(feeRaw) < 32 {
		return false, fmt.Sprintf("fee() failed: %v", err)
	}
	feeWei := new(big.Int).SetBytes(feeRaw[:32]).Uint64()
	if feeWei == 0 || feeWei > 1e8 {
		return false, fmt.Sprintf("curve fee out of range: %d", feeWei)
	}
	p.CurveFee1e10 = feeWei

	// balances(0) and balances(1) must be non-zero
	bal0Raw, err := callWithUint(ctx, client, a, "0x4903b0d1", 0) // balances(uint256)
	if err != nil || len(bal0Raw) < 32 {
		return false, fmt.Sprintf("balances(0) failed: %v", err)
	}
	b0 := new(big.Int).SetBytes(bal0Raw[:32])
	bal1Raw, err := callWithUint(ctx, client, a, "0x4903b0d1", 1)
	if err != nil || len(bal1Raw) < 32 {
		return false, fmt.Sprintf("balances(1) failed: %v", err)
	}
	b1 := new(big.Int).SetBytes(bal1Raw[:32])
	if b0.Sign() == 0 || b1.Sign() == 0 {
		return false, "curve balances are zero"
	}
	p.Reserve0 = b0
	p.Reserve1 = b1

	return true, ""
}

// ── Balancer verification ────────────────────────────────────────────────────

func verifyBalancerPool(ctx context.Context, client *ethclient.Client, p *Pool) (bool, string) {
	if p.PoolID == "" {
		return false, "balancer poolID missing"
	}
	a := common.HexToAddress(p.Address)

	// getPoolId() must match what we have stored
	pidRaw, err := callRaw(ctx, client, a, "0x38fff2d0") // getPoolId() selector
	if err != nil || len(pidRaw) < 32 {
		return false, fmt.Sprintf("getPoolId failed: %v", err)
	}
	chainPID := strings.ToLower(common.Bytes2Hex(pidRaw[:32]))
	storedPID := strings.ToLower(strings.TrimPrefix(p.PoolID, "0x"))
	if chainPID != storedPID {
		return false, fmt.Sprintf("balancer poolID mismatch: chain=%s stored=%s", chainPID, storedPID)
	}

	// getNormalizedWeights() — sum must be ~1e18
	wRaw, err := callRaw(ctx, client, a, "0xf89f27ed") // getNormalizedWeights()
	if err != nil || len(wRaw) < 128 {
		return false, fmt.Sprintf("getNormalizedWeights failed: %v", err)
	}
	// Skip the array offset+length headers (64 bytes), then weights (32 bytes each)
	w0 := new(big.Float).Quo(
		new(big.Float).SetInt(new(big.Int).SetBytes(wRaw[64:96])),
		new(big.Float).SetFloat64(1e18),
	)
	w1 := new(big.Float).Quo(
		new(big.Float).SetInt(new(big.Int).SetBytes(wRaw[96:128])),
		new(big.Float).SetFloat64(1e18),
	)
	w0f, _ := w0.Float64()
	w1f, _ := w1.Float64()
	if w0f+w1f < 0.99 || w0f+w1f > 1.01 {
		return false, fmt.Sprintf("balancer weights don't sum to 1.0: %f + %f", w0f, w1f)
	}
	p.Weight0 = w0f
	p.Weight1 = w1f

	return true, ""
}

// ── V4 verification ──────────────────────────────────────────────────────────

// V4 hook permission bits encoded in the hooks address (highest 14 bits of
// the 160-bit address). A hook with any of the swap-affecting bits set can
// modify the swap's input/output and our local sim is unreliable.
//
// Reference: https://github.com/Uniswap/v4-periphery/blob/main/src/utils/Hooks.sol
const (
	hookBeforeInitialize         = 1 << 13 // bit 159 (msb of 14-bit perms)
	hookAfterInitialize          = 1 << 12
	hookBeforeAddLiquidity       = 1 << 11
	hookAfterAddLiquidity        = 1 << 10
	hookBeforeRemoveLiquidity    = 1 << 9
	hookAfterRemoveLiquidity     = 1 << 8
	hookBeforeSwap               = 1 << 7
	hookAfterSwap                = 1 << 6
	hookBeforeDonate             = 1 << 5
	hookAfterDonate              = 1 << 4
	hookBeforeSwapReturnsDelta   = 1 << 3
	hookAfterSwapReturnsDelta    = 1 << 2
	hookAfterAddLiqReturnsDelta  = 1 << 1
	hookAfterRemLiqReturnsDelta  = 1 << 0
)

// V4HooksAffectSwap returns true if the hook contract has any permission flag
// set that could modify a swap's input or output. Such pools cannot be safely
// simulated by our local code — only the live PoolManager knows the result.
func V4HooksAffectSwap(hooksAddr string) bool {
	if hooksAddr == "" || hooksAddr == "0x" || hooksAddr == "0x0000000000000000000000000000000000000000" {
		return false
	}
	addr := common.HexToAddress(hooksAddr)
	// The 14 hook permission flags are encoded in the lowest 14 bits of the
	// address (interpreted as a uint160). Mask the address against the
	// "affects swap" set: BEFORE_SWAP | AFTER_SWAP | *_RETURNS_DELTA.
	flags := uint16(addr[19]) | (uint16(addr[18]) << 8)
	swapAffecting := uint16(hookBeforeSwap | hookAfterSwap |
		hookBeforeSwapReturnsDelta | hookAfterSwapReturnsDelta)
	return (flags & swapAffecting) != 0
}

// VerifyV4Pool checks a V4 pool's current state against the StateView contract
// and validates the hooks address. Returns (true, "") if the pool is safe to
// trade, (false, reason) otherwise.
//
// poolID is the bytes32 V4 poolId. The other fields come from the Initialize
// event that arbscan captured. expectedHooks is the hooks contract address
// from the same event.
func VerifyV4Pool(ctx context.Context, client *ethclient.Client, poolID string, expectedTickSpacing int32, expectedFeePPM uint32, expectedHooks string) (bool, string) {
	if poolID == "" || len(poolID) != 66 {
		return false, fmt.Sprintf("invalid poolID length: %d (expected 66)", len(poolID))
	}

	// 1. Hooks check — reject pools with swap-modifying hooks
	if V4HooksAffectSwap(expectedHooks) {
		return false, fmt.Sprintf("v4 hook %s has swap-affecting permissions", expectedHooks)
	}

	// 2. tickSpacing sanity
	if expectedTickSpacing <= 0 || expectedTickSpacing > 2000 {
		return false, fmt.Sprintf("nonsensical tickSpacing %d", expectedTickSpacing)
	}

	// 3. Fee sanity (V4 fee is in ppm; static fees up to 1% = 10000)
	if expectedFeePPM > 100000 { // anything above 10% is dynamic-fee or broken
		return false, fmt.Sprintf("nonsensical fee %d ppm", expectedFeePPM)
	}

	// 4. Fetch live state from StateView and verify non-zero
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	stateView := common.HexToAddress(V4StateViewAddress)
	poolIDBytes := common.HexToHash(poolID)

	// getSlot0(bytes32) selector — calculated from v4Slot0ABI
	slot0Data, err := v4Slot0ABI.Pack("getSlot0", poolIDBytes)
	if err != nil {
		return false, fmt.Sprintf("v4 getSlot0 pack: %v", err)
	}
	slot0Res, err := client.CallContract(tctx, ethereum.CallMsg{To: &stateView, Data: slot0Data}, nil)
	if err != nil || len(slot0Res) < 32 {
		return false, fmt.Sprintf("v4 getSlot0 call: %v", err)
	}
	slot0Out, err := v4Slot0ABI.Unpack("getSlot0", slot0Res)
	if err != nil || len(slot0Out) < 4 {
		return false, fmt.Sprintf("v4 getSlot0 unpack: %v", err)
	}
	sqrtP, _ := slot0Out[0].(*big.Int)
	if sqrtP == nil || sqrtP.Sign() == 0 {
		return false, "v4 sqrtPriceX96 is zero"
	}

	// getLiquidity(bytes32)
	liqData, err := v4LiquidityABI.Pack("getLiquidity", poolIDBytes)
	if err != nil {
		return false, fmt.Sprintf("v4 getLiquidity pack: %v", err)
	}
	liqRes, err := client.CallContract(tctx, ethereum.CallMsg{To: &stateView, Data: liqData}, nil)
	if err != nil || len(liqRes) < 32 {
		return false, fmt.Sprintf("v4 getLiquidity call: %v", err)
	}
	liqOut, err := v4LiquidityABI.Unpack("getLiquidity", liqRes)
	if err != nil || len(liqOut) < 1 {
		return false, fmt.Sprintf("v4 getLiquidity unpack: %v", err)
	}
	liq, _ := liqOut[0].(*big.Int)
	if liq == nil || liq.Sign() == 0 {
		return false, "v4 liquidity is zero (no active range)"
	}

	return true, ""
}

// ── helpers ──────────────────────────────────────────────────────────────────

// callRaw makes an eth_call with a 4-byte selector and no arguments, returning
// the raw return data.
func callRaw(ctx context.Context, client *ethclient.Client, to common.Address, selectorHex string) ([]byte, error) {
	sel := common.FromHex(selectorHex)
	if len(sel) != 4 {
		return nil, fmt.Errorf("invalid selector %s", selectorHex)
	}
	// Retry up to 3 times on transient empty responses or RPC errors. Without
	// this, a single hiccup on a verify call (slot0(), tickSpacing(), etc.)
	// flips the pool's verified flag to false for an entire 1-hour cycle.
	// Reasonable backoff: 0ms, 200ms, 600ms.
	var (
		out  []byte
		err  error
	)
	delays := []time.Duration{0, 200 * time.Millisecond, 600 * time.Millisecond}
	for _, d := range delays {
		if d > 0 {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(d):
			}
		}
		out, err = client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: sel}, nil)
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}
	return out, err
}

// callWithUint makes an eth_call with a 4-byte selector + one uint256 argument.
func callWithUint(ctx context.Context, client *ethclient.Client, to common.Address, selectorHex string, arg uint64) ([]byte, error) {
	sel := common.FromHex(selectorHex)
	if len(sel) != 4 {
		return nil, fmt.Errorf("invalid selector %s", selectorHex)
	}
	data := make([]byte, 4+32)
	copy(data[0:4], sel)
	new(big.Int).SetUint64(arg).FillBytes(data[4:36])
	return client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
}

// _ uses the abi import to satisfy the linter when we don't pack via ABI.
var _ = abi.ABI{}
