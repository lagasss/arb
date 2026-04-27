package internal

// Splitter finds optimal split-route arbitrage opportunities:
//
//   Buy leg:  borrow stablecoin → buy tradeToken on cheapest single pool
//   Sell leg: split-sell tradeToken across N pools to maximize proceeds
//
// The optimal split equalizes marginal output rates across all sell pools
// (Lagrangian condition). We find it by binary-searching on λ (the equimarginal
// rate) for V2 pools, and falling back to equal-weight allocation for V3.
//
// CEX price (from CexFeed) is used to:
//   a) Identify which token is likely overpriced on DEX (directional filter)
//   b) Provide a sanity check — never trade if DEX/CEX spread > maxSpreadBps

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// SellHopPlan is one pool in the split-sell leg.
type SellHopPlan struct {
	Pool       *Pool
	ZeroForOne bool    // true if tradeToken is token0 of Pool
	AmountIn   float64 // human-readable tradeToken amount allocated to this hop
	AmountOut  float64 // expected borrowToken output (human)
}

// SplitArbOpp is a fully-costed split arb opportunity ready for submission.
type SplitArbOpp struct {
	BorrowToken  *Token
	TradeToken   *Token
	BorrowAmount float64 // in borrowToken human units

	BuyPool       *Pool
	BuyZeroForOne bool
	BuyAmountOut  float64 // tradeToken received from buy leg

	SellHops []SellHopPlan

	GrossOut    float64 // total borrowToken from sell legs
	NetProfitUSD float64
	SpreadBps   float64 // DEX effective price vs CEX price
}

// String for logging.
func (o *SplitArbOpp) String() string {
	pools := make([]string, len(o.SellHops))
	for i, h := range o.SellHops {
		pools[i] = fmt.Sprintf("%s(%.0f%%)", h.Pool.DEX,
			100*h.AmountIn/o.BuyAmountOut)
	}
	return fmt.Sprintf("buy %s on %s → split-sell across [%s] profit=$%.2f spread=%.1fbps",
		o.TradeToken.Symbol, o.BuyPool.DEX,
		strings.Join(pools, ","), o.NetProfitUSD, o.SpreadBps)
}

// Splitter scans the pool registry for split-arb opportunities.
type Splitter struct {
	registry    *PoolRegistry
	tokens      *TokenRegistry
	cex         *CexFeed
	maxSellHops int
	minSpreadBps float64
	maxTradeUSD  float64
	gasCostUSD   float64 // estimated gas cost for a split-arb tx
}

func NewSplitter(registry *PoolRegistry, tokens *TokenRegistry, cex *CexFeed,
	maxSellHops int, minSpreadBps, maxTradeUSD, gasCostUSD float64) *Splitter {
	if maxSellHops <= 0 {
		maxSellHops = 4
	}
	return &Splitter{
		registry:     registry,
		tokens:       tokens,
		cex:          cex,
		maxSellHops:  maxSellHops,
		minSpreadBps: minSpreadBps,
		maxTradeUSD:  maxTradeUSD,
		gasCostUSD:   gasCostUSD,
	}
}

// Scan checks all liquid token pairs and returns the best opportunity found, or nil.
func (s *Splitter) Scan() *SplitArbOpp {
	if !s.cex.IsReady() {
		return nil
	}

	pools := s.registry.All()

	// Group pools by token pair (stablecoin vs tradeToken).
	// We only consider WETH, WBTC, ARB etc. paired with stablecoins.
	type pairKey struct{ stable, trade string } // lower-case addresses
	byPair := make(map[pairKey][]*Pool)

	stables := stableAddresses()

	for _, p := range pools {
		if p.Token0 == nil || p.Token1 == nil {
			continue
		}
		if p.SpotRate() == 0 {
			continue
		}
		a0 := strings.ToLower(p.Token0.Address)
		a1 := strings.ToLower(p.Token1.Address)
		_, s0 := stables[a0]
		_, s1 := stables[a1]
		if s0 && !s1 {
			byPair[pairKey{a0, a1}] = append(byPair[pairKey{a0, a1}], p)
		} else if s1 && !s0 {
			byPair[pairKey{a1, a0}] = append(byPair[pairKey{a1, a0}], p)
		}
	}

	var best *SplitArbOpp
	for key, pairPools := range byPair {
		if len(pairPools) < 2 {
			continue // need at least buy + sell
		}
		stableTok, ok := s.tokens.Get(key.stable)
		if !ok {
			continue
		}
		tradeTok, ok := s.tokens.Get(key.trade)
		if !ok {
			continue
		}
		cexPrice := s.cex.PriceUSD(tradeTok.Symbol) // USD per tradeToken
		if cexPrice <= 0 {
			continue
		}

		opp := s.evalPair(stableTok, tradeTok, pairPools, cexPrice)
		if opp == nil {
			continue
		}
		if best == nil || opp.NetProfitUSD > best.NetProfitUSD {
			best = opp
		}
	}
	return best
}

// evalPair evaluates a single (stable, trade) token pair across all its pools.
func (s *Splitter) evalPair(stable, trade *Token, pools []*Pool, cexPriceUSD float64) *SplitArbOpp {
	// Separate pools into buy candidates (lowest tradeToken price) and
	// sell candidates (highest tradeToken price).
	// We pick the single best buy pool, then optimize the sell split.

	var candidates []scoredPool
	for _, p := range pools {
		p.mu.RLock()
		spotRate := p.spotRateLocked() // token1 per token0
		p.mu.RUnlock()
		if spotRate <= 0 {
			continue
		}
		var spotUSD float64
		var zfi bool
		if strings.EqualFold(p.Token0.Address, stable.Address) {
			// token0=stable, token1=trade → price = 1/spotRate stable per trade
			spotUSD = 1.0 / spotRate
			zfi = true // sell stable (token0) to get trade (token1)
		} else {
			// token0=trade, token1=stable → price = spotRate stable per trade
			spotUSD = spotRate
			zfi = false // sell trade (token0) to get stable (token1)... wait
			// Actually to BUY trade we sell stable (token1→token0), so zeroForOne=false means token1 in.
			// Convention: zeroForOne=true means token0 goes in. To buy trade (token0), stable (token1) goes in → zeroForOne=false.
			// Re-derive: if token0=trade, token1=stable, buying trade means stable in → NOT zeroForOne.
			zfi = false
		}
		if spotUSD <= 0 {
			continue
		}
		candidates = append(candidates, scoredPool{p, zfi, spotUSD})
	}
	if len(candidates) < 2 {
		return nil
	}

	// Find best buy pool (lowest cost per tradeToken = buy cheap)
	bestBuyIdx := 0
	for i, c := range candidates {
		if c.spotUSD < candidates[bestBuyIdx].spotUSD {
			bestBuyIdx = i
		}
	}
	buyPool := candidates[bestBuyIdx]

	// Build sell candidates: all OTHER pools (will sell tradeToken there)
	var sellCandidates []scoredPool
	for i, c := range candidates {
		if i == bestBuyIdx {
			continue
		}
		// For sell: we want high spotUSD (sell expensive)
		// The zeroForOne for selling is the opposite of buying:
		// if token0=stable → sell trade means token1 in → zeroForOne=false
		sellZfi := !c.zeroForOne
		sellCandidates = append(sellCandidates, scoredPool{c.pool, sellZfi, c.spotUSD})
	}
	if len(sellCandidates) == 0 {
		return nil
	}

	// Sort sell candidates by spotUSD descending (best sell price first)
	sortBySpotDesc(sellCandidates)
	if len(sellCandidates) > s.maxSellHops {
		sellCandidates = sellCandidates[:s.maxSellHops]
	}

	// Compute max borrow amount: bounded by maxTradeUSD and buy pool liquidity
	maxBorrowUSD := s.maxTradeUSD
	buyPoolLiqUSD := poolLiqUSD(buyPool.pool, stable, trade)
	if buyPoolLiqUSD*0.05 < maxBorrowUSD { // cap at 5% of buy pool liquidity
		maxBorrowUSD = buyPoolLiqUSD * 0.05
	}
	if maxBorrowUSD <= 0 {
		return nil
	}

	// Borrow amount in stable tokens
	borrowDecimals := math.Pow10(int(stable.Decimals))
	borrowAmountHuman := maxBorrowUSD // 1 stable ≈ $1

	// Simulate buy leg: how much tradeToken do we get?
	buyAmountRaw := SimulatorFor(buyPool.pool.DEX).AmountOut(
		buyPool.pool, stable,
		floatToWei(borrowAmountHuman, stable.Decimals),
	)
	if buyAmountRaw == nil || buyAmountRaw.Sign() == 0 {
		return nil
	}
	buyAmountHuman := weiToFloat(buyAmountRaw, trade.Decimals)

	// Effective buy price (stable per trade)
	effectiveBuyPrice := borrowAmountHuman / buyAmountHuman

	// Check spread: if buy price ≥ cex price, there's no profit potential
	if effectiveBuyPrice >= cexPriceUSD*(1+s.minSpreadBps/10_000) {
		return nil
	}

	// Optimize sell split across sell candidates
	sellHops, grossOutHuman := s.optimizeSellSplit(trade, stable, buyAmountHuman, sellCandidates)
	if len(sellHops) == 0 {
		return nil
	}

	// Effective sell price
	effectiveSellPrice := grossOutHuman / buyAmountHuman

	// Spread vs CEX
	spreadBps := SpreadBps(effectiveSellPrice, cexPriceUSD)

	// Net profit
	netProfit := (grossOutHuman - borrowAmountHuman) * (1.0 / borrowDecimals * borrowDecimals) // already in human
	netProfitUSD := netProfit // stable ≈ $1

	// Subtract gas
	netProfitUSD -= s.gasCostUSD
	if netProfitUSD <= 0 {
		return nil
	}

	// Require minimum spread
	if spreadBps < s.minSpreadBps {
		return nil
	}

	return &SplitArbOpp{
		BorrowToken:  stable,
		TradeToken:   trade,
		BorrowAmount: borrowAmountHuman,

		BuyPool:       buyPool.pool,
		BuyZeroForOne: buyPool.zeroForOne,
		BuyAmountOut:  buyAmountHuman,

		SellHops:     sellHops,
		GrossOut:     grossOutHuman,
		NetProfitUSD: netProfitUSD,
		SpreadBps:    spreadBps,
	}
}

// optimizeSellSplit allocates buyAmount of tradeToken across sell pools to maximize
// total stable proceeds. Uses binary search on the equimarginal shadow price λ for
// V2 pools; approximates V3 as V2 using virtual reserves.
func (s *Splitter) optimizeSellSplit(
	trade, stable *Token,
	totalAmountHuman float64,
	candidates []scoredPool,
) ([]SellHopPlan, float64) {

	type poolModel struct {
		sp    scoredPool
		rIn   float64 // virtual reserve of tradeToken (human)
		rOut  float64 // virtual reserve of stable (human)
		feeMul float64 // e.g. 0.997 for 0.3% fee
	}

	models := make([]poolModel, 0, len(candidates))
	for _, c := range candidates {
		rIn, rOut := virtualReserves(c.pool, trade, stable)
		if rIn <= 0 || rOut <= 0 {
			continue
		}
		fee := 1.0 - float64(c.pool.FeeBps)/10_000.0
		if fee <= 0 {
			fee = 0.997
		}
		models = append(models, poolModel{c, rIn, rOut, fee})
	}
	if len(models) == 0 {
		return nil, 0
	}

	// Binary search on λ (shadow price): find λ such that sum of optimal
	// allocations equals totalAmountHuman.
	//
	// For V2-style pool: optimal a_i(λ) = max(0, (sqrt(k*Ri_in*Ri_out/λ) - Ri_in) / k)
	// where k = feeMul.
	//
	// Search range: λ ∈ (0, maxSpotRate]
	maxSpot := 0.0
	for _, m := range models {
		spot := m.rOut / m.rIn * m.feeMul
		if spot > maxSpot {
			maxSpot = spot
		}
	}

	allocForLambda := func(lam float64) ([]float64, float64) {
		allocs := make([]float64, len(models))
		total := 0.0
		for i, m := range models {
			// a_i = (sqrt(k*Ri_in*Ri_out/λ) - Ri_in) / k
			inner := m.feeMul * m.rIn * m.rOut / lam
			if inner <= 0 {
				continue
			}
			ai := (math.Sqrt(inner) - m.rIn) / m.feeMul
			if ai < 0 {
				ai = 0
			}
			allocs[i] = ai
			total += ai
		}
		return allocs, total
	}

	lo, hi := 1e-12, maxSpot
	for iter := 0; iter < 64; iter++ {
		mid := (lo + hi) / 2
		_, total := allocForLambda(mid)
		if total > totalAmountHuman {
			hi = mid
		} else {
			lo = mid
		}
	}

	lam := (lo + hi) / 2
	allocs, totalAlloc := allocForLambda(lam)

	// Scale down if sum > available (rounding)
	if totalAlloc > totalAmountHuman*1.001 {
		scale := totalAmountHuman / totalAlloc
		for i := range allocs {
			allocs[i] *= scale
		}
		totalAlloc = totalAmountHuman
	}

	// Build hop plans, simulate actual output
	var hops []SellHopPlan
	grossOut := 0.0
	for i, m := range models {
		if allocs[i] < 1e-9 {
			continue
		}
		amtInRaw := floatToWei(allocs[i], trade.Decimals)
		amtOut := SimulatorFor(m.sp.pool.DEX).AmountOut(m.sp.pool, trade, amtInRaw)
		if amtOut == nil || amtOut.Sign() == 0 {
			continue
		}
		outHuman := weiToFloat(amtOut, stable.Decimals)
		grossOut += outHuman
		hops = append(hops, SellHopPlan{
			Pool:       m.sp.pool,
			ZeroForOne: m.sp.zeroForOne,
			AmountIn:   allocs[i],
			AmountOut:  outHuman,
		})
	}
	return hops, grossOut
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// virtualReserves approximates V3 pools as virtual V2 reserves from liquidity + sqrtPrice.
// For V2 pools, returns actual reserves in human-readable units.
func virtualReserves(p *Pool, tokenIn, tokenOut *Token) (rIn, rOut float64) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	switch p.DEX {
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
		if p.SqrtPriceX96 == nil || p.Liquidity == nil || p.Liquidity.Sign() == 0 {
			return 0, 0
		}
		// sqrtP = sqrtPriceX96 / 2^96
		q96 := math.Pow(2, 96)
		sqrtPInt, _ := new(big.Float).SetInt(p.SqrtPriceX96).Float64()
		sqrtP := sqrtPInt / q96
		if sqrtP <= 0 {
			return 0, 0
		}
		liq, _ := new(big.Float).SetInt(p.Liquidity).Float64()
		// Virtual reserves in token units:
		// x (token0) = L / sqrtP  ;  y (token1) = L * sqrtP
		r0raw := liq / sqrtP
		r1raw := liq * sqrtP
		// Adjust for decimals
		r0 := r0raw / math.Pow10(int(p.Token0.Decimals))
		r1 := r1raw / math.Pow10(int(p.Token1.Decimals))
		if strings.EqualFold(tokenIn.Address, p.Token0.Address) {
			return r0, r1
		}
		return r1, r0

	default: // V2 style
		if p.Reserve0 == nil || p.Reserve1 == nil {
			return 0, 0
		}
		r0, _ := new(big.Float).SetInt(p.Reserve0).Float64()
		r1, _ := new(big.Float).SetInt(p.Reserve1).Float64()
		r0 /= math.Pow10(int(p.Token0.Decimals))
		r1 /= math.Pow10(int(p.Token1.Decimals))
		if strings.EqualFold(tokenIn.Address, p.Token0.Address) {
			return r0, r1
		}
		return r1, r0
	}
}

// poolLiqUSD estimates the pool's one-sided liquidity in USD for trade sizing.
func poolLiqUSD(p *Pool, stable, trade *Token) float64 {
	rIn, _ := virtualReserves(p, stable, trade)
	return rIn // stable ≈ $1
}

// stableAddresses returns the set of known stablecoin addresses on Arbitrum (lower-case).
func stableAddresses() map[string]struct{} {
	return map[string]struct{}{
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831": {}, // USDC
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": {}, // USDC.e
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": {}, // USDT
		"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": {}, // DAI
	}
}

// floatToWei converts a human-readable amount to raw token units.
func floatToWei(amount float64, decimals uint8) *big.Int {
	f := new(big.Float).SetFloat64(amount)
	f.Mul(f, new(big.Float).SetFloat64(math.Pow10(int(decimals))))
	i, _ := f.Int(nil)
	return i
}

// weiToFloat converts raw token units to human-readable.
func weiToFloat(amount *big.Int, decimals uint8) float64 {
	f, _ := new(big.Float).SetInt(amount).Float64()
	return f / math.Pow10(int(decimals))
}

func sortBySpotDesc(pools []scoredPool) {
	for i := 1; i < len(pools); i++ {
		for j := i; j > 0 && pools[j].spotUSD > pools[j-1].spotUSD; j-- {
			pools[j], pools[j-1] = pools[j-1], pools[j]
		}
	}
}

// ToOnChainParams converts a SplitArbOpp into the ABI-encodable params for SplitArb.sol.
func (o *SplitArbOpp) ToOnChainParams(slippageBps int64, minProfitWei *big.Int) SplitArbParams {
	borrowRaw := floatToWei(o.BorrowAmount, o.BorrowToken.Decimals)
	buyMinOut := floatToWei(
		o.BuyAmountOut*(1-float64(slippageBps)/10_000),
		o.TradeToken.Decimals,
	)

	hops := make([]SplitHopOnChain, len(o.SellHops))
	for i, h := range o.SellHops {
		isV3 := isV3DEX(h.Pool.DEX)
		amtRaw := floatToWei(h.AmountIn, o.TradeToken.Decimals)
		hops[i] = SplitHopOnChain{
			Pool:       common.HexToAddress(h.Pool.Address),
			IsV3:       isV3,
			ZeroForOne: h.ZeroForOne,
			AmountIn:   amtRaw,
		}
	}

	return SplitArbParams{
		BorrowToken:   common.HexToAddress(o.BorrowToken.Address),
		BorrowAmount:  borrowRaw,
		TradeToken:    common.HexToAddress(o.TradeToken.Address),
		BuyPool:       common.HexToAddress(o.BuyPool.Address),
		BuyIsV3:       isV3DEX(o.BuyPool.DEX),
		BuyZeroForOne: o.BuyZeroForOne,
		BuyMinOut:     buyMinOut,
		SellHops:      hops,
		MinProfitWei:  minProfitWei,
	}
}

// SplitArbParams mirrors SplitArb.sol's TradeParams for ABI encoding.
type SplitArbParams struct {
	BorrowToken   common.Address
	BorrowAmount  *big.Int
	TradeToken    common.Address
	BuyPool       common.Address
	BuyIsV3       bool
	BuyZeroForOne bool
	BuyMinOut     *big.Int
	SellHops      []SplitHopOnChain
	MinProfitWei  *big.Int
}

// SplitHopOnChain mirrors SplitArb.sol's SellHop struct.
type SplitHopOnChain struct {
	Pool       common.Address
	IsV3       bool
	ZeroForOne bool
	AmountIn   *big.Int
}

func isV3DEX(d DEXType) bool {
	switch d {
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
		return true
	}
	return false
}

type scoredPool struct {
	pool       *Pool
	zeroForOne bool
	spotUSD    float64
}
