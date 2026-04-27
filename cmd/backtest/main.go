// backtest: replays historical arb opportunities from arbscan.jsonl.
// For each profitable tx, fetches pool states at block-1, runs Bellman-Ford
// and the simulator, and reports whether our bot would have detected it.
//
// Usage: backtest [arbscan.jsonl] [min_profit_usd]
// Output: /tmp/backtest_results.txt
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"arb-bot/internal"

	"github.com/ethereum/go-ethereum/ethclient"
)

// ArbTx mirrors the JSON output from arbscan.
type ArbTx struct {
	TxHash      string  `json:"tx_hash"`
	BlockNumber uint64  `json:"block"`
	BotContract string  `json:"bot_contract"`
	ProfitUSD   float64 `json:"profit_usd"`
	NetUSD      float64 `json:"net_profit_usd"`
	HopCount    int     `json:"hop_count"`
	PathStr     string  `json:"path_str"`
	Hops        []struct {
		Pool      string `json:"pool"`
		DEX       string `json:"dex"`
		FeeBps    uint32 `json:"fee_bps"`
		TokenIn   string `json:"token_in"`
		SymbolIn  string `json:"symbol_in"`
		DecIn     uint8  `json:"decimals_in"`
		TokenOut  string `json:"token_out"`
		SymbolOut string `json:"symbol_out"`
		DecOut    uint8  `json:"decimals_out"`
	} `json:"hops"`
}

type BacktestResult struct {
	Tx           ArbTx
	BFFound      bool    // did BF find any profitable cycle?
	SimViable    bool    // did sim say profitable?
	SimProfitBps float64 // sim's profit estimate in bps (sub-bp resolution)
	SimProfitUSD float64
	WouldSubmit  bool   // BF found + sim viable + above min profit
	FailReason   string
}

// SeedPool holds static config for a pool seed (no state).
type SeedPool struct {
	Address  string
	DEX      internal.DEXType
	FeeBps   uint32
	Token0   *internal.Token
	Token1   *internal.Token
	IsStable bool
	PoolID   string
	Weight0  float64
	Weight1  float64
}

const (
	// Use Alchemy for archive access (historical block queries) — Chainstack free plan blocks these.
	rpcURL    = "wss://arb-mainnet.g.alchemy.com/v2/E0ASSpGbenMwsLFIfcr1-"
	minProfit = 1.0 // only backtest txs with >$1 actual profit
	configPath = "/home/arbitrator/go/arb-bot/config.yaml"
)

var debugMode bool

func main() {
	inFile := "/tmp/arbscan.jsonl"
	if len(os.Args) > 1 {
		inFile = os.Args[1]
	}
	minProfitUSD := minProfit
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%f", &minProfitUSD)
	}
	noSeeds := false
	for _, a := range os.Args[3:] {
		if a == "--no-seeds" {
			noSeeds = true
		}
		if a == "--debug" {
			debugMode = true
		}
	}

	// Load seed pools from config to use as "backbone" graph for each tx.
	// These provide the closing hops for incomplete arb paths (e.g. WETH→USDC
	// to close a USDC→AAVE→WETH 2-hop path that arbscan captured incompletely).
	var seeds []SeedPool
	if !noSeeds {
		var err error
		seeds, err = loadSeedPools(configPath)
		if err != nil {
			log.Printf("[backtest] warning: could not load config seeds: %v", err)
		} else {
			log.Printf("[backtest] loaded %d seed pools from config", len(seeds))
		}
	} else {
		log.Printf("[backtest] running WITHOUT backbone seeds (--no-seeds)")
	}

	// Load arb transactions
	txs, err := loadTxs(inFile, minProfitUSD)
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	log.Printf("[backtest] loaded %d txs with profit > $%.0f", len(txs), minProfitUSD)

	client, err := ethclient.Dial(rpcURL)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	// Process concurrently, 4 at a time (RPC rate limit)
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var results []BacktestResult

	for i, tx := range txs {
		tx := tx
		i := i
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			r := backtest(ctx, client, tx, seeds)
			mu.Lock()
			results = append(results, r)
			if (i+1)%10 == 0 {
				log.Printf("[backtest] progress: %d/%d", i+1, len(txs))
			}
			mu.Unlock()
		}()
	}
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}

	writeResults(results, minProfitUSD)
}

func loadTxs(path string, minProfitUSD float64) ([]ArbTx, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var txs []ArbTx
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var tx ArbTx
		if err := json.Unmarshal(sc.Bytes(), &tx); err != nil {
			continue
		}
		if tx.ProfitUSD < minProfitUSD {
			continue
		}
		if len(tx.Hops) < 2 {
			continue
		}
		txs = append(txs, tx)
	}
	// Sort by profit descending
	sort.Slice(txs, func(i, j int) bool {
		return txs[i].ProfitUSD > txs[j].ProfitUSD
	})
	return txs, sc.Err()
}

// loadSeedPools is a stub: the legacy `seeds:` config section was removed in
// favour of the SQLite-backed `pinned_pools:` mechanism. Backtest no longer
// has a curated seed list at hand. Tools that need backbone pools should be
// updated to load from the SQLite pools table directly via internal.LoadPools.
// Returning an empty slice is safe — backtest will fall back to whatever pools
// the test transactions reference.
func loadSeedPools(cfgPath string) ([]SeedPool, error) {
	_ = cfgPath
	return nil, nil
}

// cloneSeedPool creates a fresh *Pool from a SeedPool descriptor.
// Each backtest tx needs its own pool objects since BatchUpdatePoolsAtBlock writes state in-place.
func cloneSeedPool(s SeedPool) *internal.Pool {
	return &internal.Pool{
		Address:  s.Address,
		DEX:      s.DEX,
		FeeBps:   s.FeeBps,
		Token0:   s.Token0,
		Token1:   s.Token1,
		TVLUSD:   1_000_000,
		IsStable: s.IsStable,
		PoolID:   s.PoolID,
		Weight0:  s.Weight0,
		Weight1:  s.Weight1,
	}
}

func backtest(ctx context.Context, client *ethclient.Client, tx ArbTx, seeds []SeedPool) BacktestResult {
	r := BacktestResult{Tx: tx}

	// Build pool map from hop data (provides the specific arb pools)
	poolMap := make(map[string]*internal.Pool)

	for _, hop := range tx.Hops {
		if _, exists := poolMap[strings.ToLower(hop.Pool)]; exists {
			continue
		}
		tin := internal.NewToken(hop.TokenIn, hop.SymbolIn, hop.DecIn)
		tout := internal.NewToken(hop.TokenOut, hop.SymbolOut, hop.DecOut)

		// V3/V2 pools store sqrtPriceX96 relative to token0 = lower address.
		// If hop goes high→low address, swap so token0 is always the lower address.
		token0, token1 := tin, tout
		if strings.ToLower(hop.TokenIn) > strings.ToLower(hop.TokenOut) {
			token0, token1 = tout, tin
		}
		pool := &internal.Pool{
			Address: hop.Pool,
			DEX:     parseDEX(hop.DEX),
			FeeBps:  hop.FeeBps,
			Token0:  token0,
			Token1:  token1,
			TVLUSD:  1_000_000,
		}
		poolMap[strings.ToLower(hop.Pool)] = pool
	}

	// Add config seed pools as token-aware backbone: only add a seed if BOTH of its
	// tokens appear in the hop data AND the pair isn't already covered by an existing
	// hop pool. This prevents phantom cycles, e.g. adding Camelot WETH/USDT when
	// PancakeV3 WETH/USDT is already in poolMap — both pass the token filter but
	// together create a spurious BF cycle that sim cannot validate.
	hopTokens := make(map[string]bool)
	for _, hop := range tx.Hops {
		hopTokens[strings.ToLower(hop.TokenIn)] = true
		hopTokens[strings.ToLower(hop.TokenOut)] = true
	}
	// Build set of (token0,token1) pairs already covered by hop pools.
	coveredPairs := make(map[string]bool)
	for _, p := range poolMap {
		a := strings.ToLower(p.Token0.Address)
		b := strings.ToLower(p.Token1.Address)
		if a > b {
			a, b = b, a
		}
		coveredPairs[a+":"+b] = true
	}
	for _, s := range seeds {
		addr := strings.ToLower(s.Address)
		if _, exists := poolMap[addr]; exists {
			continue
		}
		// Both tokens must appear in the hop graph.
		t0 := strings.ToLower(s.Token0.Address)
		t1 := strings.ToLower(s.Token1.Address)
		if !hopTokens[t0] || !hopTokens[t1] {
			continue
		}
		// Don't add if this token pair is already covered — it would create phantom cycles.
		// We update coveredPairs incrementally so that the first seed for a pair wins and
		// subsequent seeds for the same pair are skipped (prevents e.g. Camelot+PancakeV3
		// WETH/USDT both being added, which creates a spurious 2-pool BF cycle).
		a, b := t0, t1
		if a > b {
			a, b = b, a
		}
		pairKey := a + ":" + b
		if coveredPairs[pairKey] {
			continue
		}
		poolMap[addr] = cloneSeedPool(s)
		coveredPairs[pairKey] = true // mark covered so next seed for same pair is skipped
	}

	pools := make([]*internal.Pool, 0, len(poolMap))
	for _, p := range poolMap {
		pools = append(pools, p)
	}

	// Fetch pool states at block-1 (before arb executed)
	blockNum := tx.BlockNumber
	if blockNum > 0 {
		blockNum--
	}
	blockBig := new(big.Int).SetUint64(blockNum)

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := internal.BatchUpdatePoolsAtBlock(tctx, client, pools, blockBig); err != nil {
		r.FailReason = fmt.Sprintf("multicall@block-%d: %v", blockNum, err)
		return r
	}

	// Build graph
	graph := internal.NewGraph()
	for _, p := range pools {
		graph.AddPool(p)
	}

	// Run BF — any profitable cycle counts as detected.
	// The actual arb may use a superset of our cycle's pools (e.g. with a flash loan hop
	// that arbscan records oddly), or arbscan may have missed the closing hop.
	// Either way, if BF finds ANY profitable opportunity, our live bot would have acted.
	cycles := internal.Detect(graph, nil)
	if len(cycles) == 0 {
		r.FailReason = "BF found 0 cycles"
		return r
	}

	r.BFFound = true

	// Find the cycle with the highest overlap with actual arb pools (for FailReason reporting).
	actualPools := make(map[string]bool)
	for _, hop := range tx.Hops {
		actualPools[strings.ToLower(hop.Pool)] = true
	}
	bestOverlapCycle := cycles[0]
	bestOverlap := -1
	for _, cycle := range cycles {
		overlap := 0
		for _, e := range cycle.Edges {
			if actualPools[strings.ToLower(e.Pool.Address)] {
				overlap++
			}
		}
		if overlap > bestOverlap {
			bestOverlap = overlap
			bestOverlapCycle = cycle
		}
	}

	// Simulate ALL BF cycles and take the best viable result.
	// If BF finds a phantom cycle (backbone pools at stale prices) but sim rejects it,
	// another cycle in the list might still pass. Our live bot tries all cycles.
	var bestResult internal.SimulationResult
	var bestResultCycle internal.Cycle
	tradeSizes := []float64{10, 50, 100, 500, 1_000, 5_000, 10_000, 50_000}
	if debugMode {
		log.Printf("[debug] tx=%s profit=$%.0f BFcycles=%d", tx.TxHash[:12], tx.ProfitUSD, len(cycles))
		for ci, cycle := range cycles {
			log.Printf("[debug]   cycle[%d] logP=%.5f path=%s", ci, cycle.LogProfit, cycle.Path())
			for _, e := range cycle.Edges {
				p := e.Pool
				if p.SqrtPriceX96 != nil {
					log.Printf("[debug]     pool=%s DEX=%s sqrtP=%s liq=%s feeBps=%d", p.Address[:10], p.DEX, p.SqrtPriceX96, p.Liquidity, p.FeeBps)
				} else {
					log.Printf("[debug]     pool=%s DEX=%s R0=%s R1=%s feeBps=%d", p.Address[:10], p.DEX, p.Reserve0, p.Reserve1, p.FeeBps)
				}
			}
		}
	}
	for _, cycle := range cycles {
		startToken := cycle.Edges[0].TokenIn
		pool0 := cycle.Edges[0].Pool
		for _, usd := range tradeSizes {
			amt := tokenAmountFromUSD(usd, startToken, pool0)
			res := internal.SimulateCycle(cycle, amt)
			if debugMode && usd == 10 {
				log.Printf("[debug]   sim cycle=%s $%.0f → profit=%v bps=%.2f viable=%v", cycle.Path(), usd, res.Profit, res.ProfitBps, res.Viable)
			}
			if res.Viable && res.Profit != nil &&
				(bestResult.Profit == nil || res.Profit.Cmp(bestResult.Profit) > 0) {
				bestResult = res
				bestResultCycle = cycle
			}
		}
	}

	// If no cycle passed sim, use the best-overlap cycle's last result for FailReason.
	if !bestResult.Viable {
		startToken := bestOverlapCycle.Edges[0].TokenIn
		pool0 := bestOverlapCycle.Edges[0].Pool
		bestResult = internal.SimulateCycle(bestOverlapCycle, tokenAmountFromUSD(1_000, startToken, pool0))
		bestResultCycle = bestOverlapCycle
	}
	_ = bestResultCycle

	r.SimViable = bestResult.Viable
	r.SimProfitBps = bestResult.ProfitBps
	if bestResult.Viable && bestResult.Profit != nil {
		startToken := bestResultCycle.Edges[0].TokenIn
		pool0 := bestResultCycle.Edges[0].Pool
		r.SimProfitUSD = profitToUSD(bestResult.Profit, startToken, pool0)
	}
	r.WouldSubmit = bestResult.Viable && r.SimProfitUSD > 0.001
	if !bestResult.Viable {
		r.FailReason = fmt.Sprintf("sim not viable (best cycle: %s)", bestOverlapCycle.Path())
	}
	return r
}

func parseDEX(s string) internal.DEXType {
	switch {
	case strings.Contains(s, "PancakeV3"), strings.Contains(s, "Pancake"):
		return internal.DEXPancakeV3
	case strings.Contains(s, "SushiV3"), strings.Contains(s, "0x1af415"):
		return internal.DEXSushiSwapV3
	case strings.Contains(s, "SushiV2"), strings.Contains(s, "Sushi"):
		return internal.DEXSushiSwap
	case strings.Contains(s, "UniV3"), strings.Contains(s, "Uni"):
		return internal.DEXUniswapV3
	case strings.Contains(s, "CamelotV3"), strings.Contains(s, "0x1a3c9b"):
		return internal.DEXCamelotV3
	case strings.Contains(s, "Camelot"):
		return internal.DEXCamelot
	case strings.Contains(s, "RamsesV3"):
		return internal.DEXRamsesV3
	case strings.Contains(s, "RamsesV2"):
		return internal.DEXRamsesV2
	case strings.Contains(s, "Curve"):
		return internal.DEXCurve
	case strings.Contains(s, "Balancer"):
		return internal.DEXBalancerWeighted
	case strings.Contains(s, "V3"):
		return internal.DEXUniswapV3
	default:
		return internal.DEXUniswapV2
	}
}

func tokenAmountFromUSD(usd float64, token *internal.Token, pool *internal.Pool) *big.Int {
	priceUSD := 1.0
	switch strings.ToLower(token.Address) {
	case "0x82af49447d8a07e3bd95bd0d56f35241523fbab1": // WETH
		priceUSD = 3400.0
	case "0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f": // WBTC
		priceUSD = 85000.0
	}
	_ = pool
	nativeFloat := usd / priceUSD / token.Scalar
	if nativeFloat <= 0 {
		return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(token.Decimals)+4), nil)
	}
	result, _ := new(big.Float).SetFloat64(nativeFloat).Int(nil)
	return result
}

func profitToUSD(profit *big.Int, token *internal.Token, pool *internal.Pool) float64 {
	_ = pool
	f, _ := new(big.Float).SetInt(profit).Float64()
	humanProfit := f * token.Scalar
	switch strings.ToLower(token.Address) {
	case "0x82af49447d8a07e3bd95bd0d56f35241523fbab1": // WETH
		return humanProfit * 3400.0
	case "0xff970a61a04b1ca14834a43f5de4533ebddb5cc8",
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831",
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": // USDC/USDT variants
		return humanProfit
	case "0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f": // WBTC
		return humanProfit * 85000.0
	}
	return humanProfit * 3400.0
}

func writeResults(results []BacktestResult, minProfit float64) {
	f, _ := os.Create("/tmp/backtest_results.txt")
	if f == nil {
		return
	}
	defer f.Close()
	p := func(s string, a ...interface{}) { fmt.Fprintf(f, s, a...) }

	sort.Slice(results, func(i, j int) bool {
		return results[i].Tx.ProfitUSD > results[j].Tx.ProfitUSD
	})

	total := len(results)
	bfFound, simViable, wouldSubmit := 0, 0, 0
	for _, r := range results {
		if r.BFFound {
			bfFound++
		}
		if r.SimViable {
			simViable++
		}
		if r.WouldSubmit {
			wouldSubmit++
		}
	}

	p("=== Backtest Results (txs with profit > $%.0f) ===\n\n", minProfit)
	p("Total txs tested:    %d\n", total)
	p("BF detected:         %d (%.1f%%)\n", bfFound, pct(bfFound, total))
	p("Sim says viable:     %d (%.1f%%)\n", simViable, pct(simViable, total))
	p("Would have submitted:%d (%.1f%%)\n\n", wouldSubmit, pct(wouldSubmit, total))

	// Failure breakdown
	failReasons := make(map[string]int)
	for _, r := range results {
		if !r.WouldSubmit {
			failReasons[r.FailReason]++
		}
	}
	p("--- Miss Reasons ---\n")
	type kv struct {
		k string
		v int
	}
	var reasons []kv
	for k, v := range failReasons {
		reasons = append(reasons, kv{k, v})
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i].v > reasons[j].v })
	for _, r := range reasons {
		p("  %4d  %s\n", r.v, r.k)
	}
	p("\n")

	// Per-tx detail
	p("--- Per-Transaction Results ---\n")
	p("%-10s %-8s %-8s %-6s %-6s %-8s  %s\n",
		"Profit$", "BF?", "Sim?", "SimBps", "SimUSD", "WouldGo", "Path")
	for _, r := range results {
		bf := "NO"
		if r.BFFound {
			bf = "YES"
		}
		sim := "NO"
		if r.SimViable {
			sim = "YES"
		}
		go_ := "NO"
		if r.WouldSubmit {
			go_ = "YES"
		}
		p("$%-9.2f %-8s %-8s %-6.2f $%-7.2f %-8s  %s\n",
			r.Tx.ProfitUSD, bf, sim, r.SimProfitBps, r.SimProfitUSD, go_, r.Tx.PathStr)
		if r.FailReason != "" {
			p("           └─ %s\n", r.FailReason)
		}
	}

	log.Printf("[backtest] results → /tmp/backtest_results.txt  (BF=%d/%d sim=%d/%d submit=%d/%d)",
		bfFound, total, simViable, total, wouldSubmit, total)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100.0 / float64(total)
}
