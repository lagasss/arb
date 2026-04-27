package internal

import (
	"fmt"
	"math"
	"math/big"
	"testing"
)

func TestUniswapV2AmountOut(t *testing.T) {
	token0 := NewToken("0x0000000000000000000000000000000000000001", "USDC", 6)
	token1 := NewToken("0x0000000000000000000000000000000000000002", "WETH", 18)

	pool := &Pool{
		Address:  "0xtest",
		DEX:      DEXUniswapV2,
		FeeBps:   30, // 0.30%
		Token0:   token0,
		Token1:   token1,
		Reserve0: new(big.Int).Mul(big.NewInt(3_000_000), big.NewInt(1e6)),  // 3M USDC
		Reserve1: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 WETH
	}

	sim := UniswapV2Sim{}
	// Swap 3000 USDC for WETH -- expect ~0.999 WETH
	amountIn := new(big.Int).Mul(big.NewInt(3000), big.NewInt(1e6))
	out := sim.AmountOut(pool, token0, amountIn)
	if out == nil || out.Sign() <= 0 {
		t.Fatal("expected positive output")
	}

	// Output should be close to 1e18 (1 WETH) minus fee and price impact
	oneEth := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	ratio := new(big.Float).Quo(new(big.Float).SetInt(out), new(big.Float).SetInt(oneEth))
	f, _ := ratio.Float64()
	if f < 0.99 || f > 1.001 {
		t.Errorf("unexpected output ratio: %f (expected ~0.999)", f)
	}
}

func TestCurveGetD(t *testing.T) {
	sim := CurveSim{}
	// Equal reserves of 1M each (18 decimals)
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(24), nil) // 1M in 18-decimal units
	D := sim.getD(one, one, 100)
	expected := new(big.Int).Mul(big.NewInt(2), one)
	// D should be close to 2*reserves = 2e24
	diff := new(big.Int).Abs(new(big.Int).Sub(D, expected))
	threshold := new(big.Int).Div(expected, big.NewInt(1000)) // 0.1%
	if diff.Cmp(threshold) > 0 {
		t.Errorf("getD: expected ~%s got %s diff %s", expected, D, diff)
	}
}

func TestSimulateCycleViable(t *testing.T) {
	token0 := NewToken("0x0000000000000000000000000000000000000001", "USDC", 6)
	token1 := NewToken("0x0000000000000000000000000000000000000002", "WETH", 18)
	token2 := NewToken("0x0000000000000000000000000000000000000003", "ARB", 18)

	// Pool A: USDC -> WETH (underpriced WETH: 2900 USDC per WETH)
	poolA := &Pool{
		DEX: DEXUniswapV2, FeeBps: 30,
		Token0: token0, Token1: token1,
		Reserve0: new(big.Int).Mul(big.NewInt(2_900_000), big.NewInt(1e6)),
		Reserve1: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
	}
	// Pool B: WETH -> ARB (1 WETH = 2000 ARB)
	poolB := &Pool{
		DEX: DEXUniswapV2, FeeBps: 30,
		Token0: token1, Token1: token2,
		Reserve0: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		Reserve1: new(big.Int).Mul(big.NewInt(2_000_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
	}
	// Pool C: ARB -> USDC (1 ARB = 1.6 USDC -> so 2000 ARB = 3200 USDC)
	poolC := &Pool{
		DEX: DEXUniswapV2, FeeBps: 30,
		Token0: token2, Token1: token0,
		Reserve0: new(big.Int).Mul(big.NewInt(2_000_000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		Reserve1: new(big.Int).Mul(big.NewInt(3_200_000), big.NewInt(1e6)),
	}

	cycle := Cycle{
		Edges: []Edge{
			{Pool: poolA, TokenIn: token0, TokenOut: token1},
			{Pool: poolB, TokenIn: token1, TokenOut: token2},
			{Pool: poolC, TokenIn: token2, TokenOut: token0},
		},
	}

	amountIn := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e6)) // 1000 USDC
	result := SimulateCycle(cycle, amountIn)
	if !result.Viable {
		t.Errorf("expected viable cycle, profit=%s path=%s", result.Profit, result.Path)
	}
	t.Logf("Cycle result: profit=%s bps=%f path=%s", result.Profit, result.ProfitBps, result.Path)
}

// TestCamelotUniV3TwoHopDebug replicates the WETH→Camelot(USDT)→UniV3(WETH)
// cycle that was producing astronomical profit values in live runs.
//
// Realistic Arbitrum mainnet values (approximate block ~300M):
//   Camelot WETH/USDT: Reserve0≈407 ETH, Reserve1≈730k USDT
//   UniV3 WETH/USDT 0.05%: sqrtPriceX96 ≈ 3.36e24, liquidity ≈ 5.9e17
func TestCamelotUniV3TwoHopDebug(t *testing.T) {
	weth := NewToken("0x82af49447d8a07e3bd95bd0d56f35241523fbab1", "WETH", 18)
	usdt := NewToken("0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", "USDT", 6)

	// Camelot WETH/USDT pool (V2-style)
	// Reserve0 = 407 WETH in wei = 407 * 1e18
	// Reserve1 = 730000 USDT in 6-dec = 730000 * 1e6
	camelotPool := &Pool{
		Address:      "0xa6c5c7d189fa4eb5af8ba34e63dcdd3a635d433f",
		DEX:          DEXCamelot,
		FeeBps:       30,
		Token0:       weth,
		Token1:       usdt,
		Reserve0:     new(big.Int).Mul(big.NewInt(407), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		Reserve1:     new(big.Int).Mul(big.NewInt(730_000), big.NewInt(1_000_000)),
		Token0FeeBps: 0, // not fetched from chain — defaults to 0
		Token1FeeBps: 0,
	}

	// UniV3 WETH/USDT 0.05% pool
	// sqrtPriceX96 for WETH≈$1794, token0=WETH(18dec), token1=USDT(6dec):
	//   raw_price = 1794 * 10^6 / 10^18 = 1.794e-9
	//   sqrtPriceX96 = sqrt(1.794e-9) * 2^96 ≈ 4.235e-5 * 7.922e28 ≈ 3.354e24
	sqrtPriceX96, _ := new(big.Int).SetString("3354000000000000000000000", 10) // ≈3.354e24
	liquidity, _ := new(big.Int).SetString("590000000000000000", 10)           // 5.9e17
	univ3Pool := &Pool{
		Address:      "0x641c00a822e8b671738d32a431a4fb6074e5c79d",
		DEX:          DEXUniswapV3,
		FeeBps:       5, // 0.05%
		Token0:       weth,
		Token1:       usdt,
		SqrtPriceX96: sqrtPriceX96,
		Liquidity:    liquidity,
	}

	// amountIn: ~55.74 WETH (100k USD / 1794 per ETH)
	amountIn := new(big.Int).Mul(big.NewInt(5574), new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil)) // 55.74e18

	t.Logf("=== Step 1: WETH → Camelot → USDT ===")
	t.Logf("amountIn (WETH wei): %s  (%.4f ETH)", amountIn, wei2float(amountIn, 18))
	t.Logf("Camelot Reserve0 (WETH wei): %s", camelotPool.Reserve0)
	t.Logf("Camelot Reserve1 (USDT): %s", camelotPool.Reserve1)
	t.Logf("Camelot Token0FeeBps: %d, Token1FeeBps: %d, FeeBps: %d", camelotPool.Token0FeeBps, camelotPool.Token1FeeBps, camelotPool.FeeBps)

	camelotSim := CamelotSim{}
	step1Out := camelotSim.AmountOut(camelotPool, weth, amountIn)
	t.Logf("step1Out (USDT 6-dec): %s  (%.2f USDT)", step1Out, wei2float(step1Out, 6))

	t.Logf("")
	t.Logf("=== Step 2: USDT → UniV3 → WETH ===")
	t.Logf("amountIn (USDT 6-dec): %s", step1Out)
	t.Logf("UniV3 sqrtPriceX96: %s", univ3Pool.SqrtPriceX96)
	t.Logf("UniV3 liquidity: %s", univ3Pool.Liquidity)
	t.Logf("UniV3 FeeBps: %d", univ3Pool.FeeBps)

	v3Sim := UniswapV3Sim{}
	step2Out := v3Sim.AmountOut(univ3Pool, usdt, step1Out)
	t.Logf("step2Out (WETH wei): %s  (%.4f ETH)", step2Out, wei2float(step2Out, 18))

	t.Logf("")
	profit := new(big.Float).Sub(
		new(big.Float).SetInt(step2Out),
		new(big.Float).SetInt(amountIn),
	)
	t.Logf("=== Cycle Summary ===")
	t.Logf("amountIn:  %.4f ETH", wei2float(amountIn, 18))
	t.Logf("amountOut: %.4f ETH", wei2float(step2Out, 18))
	profitF, _ := profit.Float64()
	t.Logf("profit:    %.6f ETH = $%.2f", profitF/1e18, profitF/1e18*1794)

	// Sanity checks
	if step1Out == nil || step1Out.Sign() <= 0 {
		t.Fatal("step1 (Camelot WETH→USDT) returned zero/nil")
	}
	step1USDT, _ := new(big.Float).SetInt(step1Out).Float64()
	step1USDT /= 1e6
	if step1USDT < 70_000 || step1USDT > 110_000 {
		t.Errorf("step1 output %.2f USDT is out of expected range [70k, 110k]", step1USDT)
	}

	if step2Out == nil || step2Out.Sign() <= 0 {
		t.Fatal("step2 (UniV3 USDT→WETH) returned zero/nil")
	}
	step2ETH := wei2float(step2Out, 18)
	if step2ETH > 100 || step2ETH <= 0 {
		t.Errorf("step2 output %.4f ETH is out of expected range (0, 100]", step2ETH)
	}
}

// TestCamelotUniV3FullCycle runs SimulateCycle for the WETH→Camelot(USDT)→UniV3(WETH) path.
func TestCamelotUniV3FullCycle(t *testing.T) {
	weth := NewToken("0x82af49447d8a07e3bd95bd0d56f35241523fbab1", "WETH", 18)
	usdt := NewToken("0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", "USDT", 6)

	camelotPool := &Pool{
		Address: "0xa6c5c7d189fa4eb5af8ba34e63dcdd3a635d433f",
		DEX:     DEXCamelot, FeeBps: 30,
		Token0:   weth, Token1: usdt,
		Reserve0: new(big.Int).Mul(big.NewInt(407), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
		Reserve1: new(big.Int).Mul(big.NewInt(730_000), big.NewInt(1_000_000)),
	}
	sqrtPriceX96, _ := new(big.Int).SetString("3354000000000000000000000", 10)
	liquidity, _ := new(big.Int).SetString("590000000000000000", 10)
	univ3Pool := &Pool{
		Address: "0x641c00a822e8b671738d32a431a4fb6074e5c79d",
		DEX:     DEXUniswapV3, FeeBps: 5,
		Token0: weth, Token1: usdt,
		SqrtPriceX96: sqrtPriceX96, Liquidity: liquidity,
	}

	cycle := Cycle{
		Edges: []Edge{
			{Pool: camelotPool, TokenIn: weth, TokenOut: usdt},
			{Pool: univ3Pool, TokenIn: usdt, TokenOut: weth},
		},
	}

	amountIn := new(big.Int).Mul(big.NewInt(5574), new(big.Int).Exp(big.NewInt(10), big.NewInt(16), nil))
	result := SimulateCycle(cycle, amountIn)
	t.Logf("Cycle: path=%s profitable=%v amountIn=%s amountOut=%s profit=%s",
		result.Path, result.Viable, result.AmountIn, result.AmountOut, result.Profit)

	if result.Viable {
		inETH := wei2float(result.AmountIn, 18)
		outETH := wei2float(result.AmountOut, 18)
		t.Logf("amountIn=%.4f ETH amountOut=%.4f ETH profit=%.4f ETH", inETH, outETH, outETH-inETH)
		// If profitable, profit should be < 5% (500 bps) to be realistic
		if outETH > inETH*1.05 {
			t.Errorf("profit > 5%% seems unrealistic: in=%.4f out=%.4f", inETH, outETH)
		}
	}
}

// TestUniV3AmountOutDirections verifies zeroForOne and oneForZero give reciprocal results.
func TestUniV3AmountOutDirections(t *testing.T) {
	weth := NewToken("0x82af49447d8a07e3bd95bd0d56f35241523fbab1", "WETH", 18)
	usdt := NewToken("0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", "USDT", 6)

	// sqrtPriceX96 for 1800 USDT/WETH (token0=WETH, token1=USDT):
	// raw_price = 1800 * 1e6/1e18 = 1.8e-9; sqrtPriceX96 = sqrt(1.8e-9)*2^96
	sqrtPriceX96, _ := new(big.Int).SetString("3354000000000000000000000", 10)
	liquidity, _ := new(big.Int).SetString("590000000000000000", 10)
	pool := &Pool{
		DEX: DEXUniswapV3, FeeBps: 5,
		Token0: weth, Token1: usdt,
		SqrtPriceX96: sqrtPriceX96, Liquidity: liquidity,
	}

	sim := UniswapV3Sim{}

	// 1 WETH in, expect ~1800 USDT out
	oneWETH := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	usdtOut := sim.AmountOut(pool, weth, oneWETH)
	t.Logf("1 WETH → %.2f USDT", wei2float(usdtOut, 6))
	usdtF := wei2float(usdtOut, 6)
	if usdtF < 1700 || usdtF > 2000 {
		t.Errorf("expected ~1800 USDT for 1 WETH, got %.2f", usdtF)
	}

	// 1800 USDT in, expect ~1 WETH out
	usdtIn := new(big.Int).Mul(big.NewInt(1800), big.NewInt(1_000_000))
	wethOut := sim.AmountOut(pool, usdt, usdtIn)
	t.Logf("1800 USDT → %.6f WETH", wei2float(wethOut, 18))
	wethF := wei2float(wethOut, 18)
	if wethF < 0.9 || wethF > 1.1 {
		t.Errorf("expected ~1 WETH for 1800 USDT, got %.6f", wethF)
	}

	t.Logf("spot check: 1 WETH → %.2f USDT, 1800 USDT → %.6f WETH", usdtF, wethF)
}

func wei2float(v *big.Int, decimals int) float64 {
	if v == nil {
		return 0
	}
	f, _ := new(big.Float).SetInt(v).Float64()
	return f / math.Pow10(decimals)
}

var _ = fmt.Sprintf
