package internal

// CycleCache pre-computes all viable arbitrage cycles in the graph and indexes
// them by pool address. On every swap event, only cycles that contain the
// swapped pool are re-scored — O(cycles_per_pool) per swap.
//
// Discovery runs in the background every rebuildInterval. Evaluation runs
// inline inside handleSwap, so latency from swap event → profitable decision is
// pool.UpdateFromSwap + score_cached_cycles ≈ microseconds.
//
// Multi-token paths (2-hop, 3-hop, 4-hop, etc.) are all discovered — DFS up to
// maxHops finds every circular path that returns to the start token.

import (
	"context"
	"fmt"
	"log"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var rebuildInterval = 15 * time.Second

// cachedCycle is a pre-discovered cycle with pool-index for fast lookup.
type cachedCycle struct {
	Cycle       Cycle
	PoolAddrs   []string // lower-case pool addresses in this cycle
	logProfitAt float64  // log profit when last discovered (used for sorting)
}

// cycleIndex is the immutable snapshot swapped atomically on each rebuild.
type cycleIndex struct {
	// byPool maps lower-case pool address → slice of cycle indices into `all`
	byPool map[string][]int
	all    []cachedCycle
}

// CycleCache holds the current cycle index and rebuilds it periodically.
type CycleCache struct {
	graph           *Graph
	whitelist       map[string]bool // static set of known-good token addresses
	maxHops         int
	maxDexPerDest   int
	maxEdgesPerNode int

	// borrowable is an optional filter on cycle start tokens. When set, the
	// DFS only picks start tokens that the Balancer Vault can flash-loan
	// (vault balance > 0). This eliminates the entire BAL#528 sim-reject
	// failure class — see borrowable.go. The pointer is swapped atomically
	// by the borrowable refresh loop; nil means "no filter, use whitelist".
	borrowable    *borrowableTracker
	flashSelector *FlashSourceSelector

	// atomic pointer to *cycleIndex — readers never block writers
	ptr unsafe.Pointer

	OnRebuild func() // optional: called after each successful rebuild (for health tracking)

	// stats
	lastBuild    time.Time
	lastCount    int
}

// SetBorrowable wires a borrowable token tracker into the cycle cache. After
// this is called the next rebuild will only DFS-from tokens that the tracker
// reports as flash-loanable. Pass nil to disable the filter.
func (cc *CycleCache) SetBorrowable(b *borrowableTracker) {
	cc.borrowable = b
}

// SetFlashSelector wires a flash source selector into the cycle cache. When
// set, the DFS start-candidate filter checks ALL flash sources (Balancer +
// V3 pools + Aave) rather than just the Balancer borrowable tracker. This
// expands the set of startable tokens from ~70 (Balancer-only) to potentially
// hundreds (any token with a V3 pool or Aave listing).
func (cc *CycleCache) SetFlashSelector(s *FlashSourceSelector) {
	cc.flashSelector = s
}

func (cc *CycleCache) SetWhitelistOverride(addrs []string) {
	wl := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		wl[strings.ToLower(a)] = true
	}
	cc.whitelist = wl
}

func NewCycleCache(graph *Graph, tokens *TokenRegistry, maxHops, maxDexPerDest, maxEdgesPerNode int) *CycleCache {
	if maxHops <= 0 {
		maxHops = 4
	}
	if maxDexPerDest <= 0 {
		maxDexPerDest = 1
	}
	if maxEdgesPerNode <= 0 {
		maxEdgesPerNode = 8
	}
	// Snapshot the token whitelist at construction time.
	// We use the registry state at startup — only config-seeded tokens, not tokens
	// discovered dynamically via pool events (which may include junk).
	wl := make(map[string]bool, len(tokens.All()))
	for _, t := range tokens.All() {
		wl[strings.ToLower(t.Address)] = true
	}
	cc := &CycleCache{
		graph:           graph,
		whitelist:       wl,
		maxHops:         maxHops,
		maxDexPerDest:   maxDexPerDest,
		maxEdgesPerNode: maxEdgesPerNode,
	}
	// Start with empty index
	empty := &cycleIndex{byPool: make(map[string][]int)}
	atomic.StorePointer(&cc.ptr, unsafe.Pointer(empty))
	return cc
}

// Run rebuilds the cycle index every rebuildInterval.
func (cc *CycleCache) Run(ctx context.Context) {
	// Initial build immediately
	cc.rebuild()

	ticker := time.NewTicker(rebuildInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cc.rebuild()
		}
	}
}

// CyclesForPool returns all pre-discovered cycles that involve the given pool address.
// This is safe to call from any goroutine — it reads from an atomic pointer.
func (cc *CycleCache) CyclesForPool(poolAddr string) []cachedCycle {
	idx := (*cycleIndex)(atomic.LoadPointer(&cc.ptr))
	if idx == nil {
		return nil
	}
	addr := strings.ToLower(poolAddr)
	positions, ok := idx.byPool[addr]
	if !ok {
		return nil
	}
	out := make([]cachedCycle, len(positions))
	for i, pos := range positions {
		out[i] = idx.all[pos]
	}
	return out
}

// All returns a snapshot of all currently cached cycles.
func (cc *CycleCache) All() []cachedCycle {
	idx := (*cycleIndex)(atomic.LoadPointer(&cc.ptr))
	if idx == nil {
		return nil
	}
	out := make([]cachedCycle, len(idx.all))
	copy(out, idx.all)
	return out
}

// PoolAddrs returns all pool addresses that have at least one cached cycle.
func (cc *CycleCache) PoolAddrs() []string {
	idx := (*cycleIndex)(atomic.LoadPointer(&cc.ptr))
	if idx == nil {
		return nil
	}
	out := make([]string, 0, len(idx.byPool))
	for addr := range idx.byPool {
		out = append(out, addr)
	}
	return out
}

// Len returns the number of currently cached cycles.
func (cc *CycleCache) Len() int {
	idx := (*cycleIndex)(atomic.LoadPointer(&cc.ptr))
	if idx == nil {
		return 0
	}
	return len(idx.all)
}

// WhyNotCachedResult explains why a specific pool sequence is NOT in the
// cache. One of Reason/Detail is always populated; Reasons may list multiple
// concurrent issues when several filters rejected the cycle.
type WhyNotCachedResult struct {
	Cached         bool     `json:"cached"`
	Reason         string   `json:"reason"`
	Detail         string   `json:"detail"`
	HopCount       int      `json:"hop_count"`
	MaxHops        int      `json:"max_hops"`
	MaxEdgesPerNode int     `json:"max_edges_per_node"`
	MaxDexPerDest  int      `json:"max_dex_per_dest"`
	TokensInCycle  []string `json:"tokens_in_cycle,omitempty"`
	TokensOutsideWhitelist []string `json:"tokens_outside_whitelist,omitempty"`
	UnknownPools   []string `json:"unknown_pools,omitempty"`
	DisabledPools  []string `json:"disabled_pools,omitempty"`
	UnverifiedPools []map[string]string `json:"unverified_pools,omitempty"`
	ZeroLiquidityPools []string `json:"zero_liquidity_pools,omitempty"`
	StartTokenNotBorrowable bool `json:"start_token_not_borrowable,omitempty"`
	StartTokenAddress       string `json:"start_token_address,omitempty"`
}

// WhyNotCached returns a structured explanation of why the cycle built from
// the given ordered pool addresses is NOT in the CycleCache. Used by the
// competitor-comparison classifier to surface specific exclusion reasons
// ("max_hops=5 but competitor cycle is 6 hops", "token 0xabc not in whitelist",
// "pool 0xdef is disabled") rather than a generic "cycle_not_cached" label.
//
// Resolution is best-effort — we look up each pool in the registry, check its
// flags, and cross-reference against the cache's filters. If the cycle IS in
// the cache, Cached=true and Reason/Detail are empty.
func (cc *CycleCache) WhyNotCached(poolAddrs []string, registry *PoolRegistry) *WhyNotCachedResult {
	r := &WhyNotCachedResult{
		HopCount:        len(poolAddrs),
		MaxHops:         cc.maxHops,
		MaxEdgesPerNode: cc.maxEdgesPerNode,
		MaxDexPerDest:   cc.maxDexPerDest,
	}

	idx := (*cycleIndex)(atomic.LoadPointer(&cc.ptr))
	if idx != nil && len(poolAddrs) > 0 {
		first := strings.ToLower(poolAddrs[0])
		for _, pos := range idx.byPool[first] {
			cc := idx.all[pos]
			if len(cc.PoolAddrs) != len(poolAddrs) {
				continue
			}
			match := true
			for i, a := range poolAddrs {
				if strings.ToLower(cc.PoolAddrs[i]) != strings.ToLower(a) {
					match = false
					break
				}
			}
			if match {
				r.Cached = true
				return r
			}
		}
	}

	if cc.maxHops > 0 && len(poolAddrs) > cc.maxHops {
		r.Reason = "exceeds_max_hops"
		r.Detail = fmt.Sprintf("cycle has %d hops but max_hops=%d", len(poolAddrs), cc.maxHops)
		return r
	}

	if registry != nil {
		var unknown, disabled, zeroLiq []string
		var unverified []map[string]string
		tokenSet := make(map[string]bool)
		var firstTokenIn string
		for _, addr := range poolAddrs {
			p, ok := registry.Get(addr)
			if !ok {
				unknown = append(unknown, strings.ToLower(addr))
				continue
			}
			if p.Token0 != nil {
				tokenSet[strings.ToLower(p.Token0.Address)] = true
			}
			if p.Token1 != nil {
				tokenSet[strings.ToLower(p.Token1.Address)] = true
			}
			if firstTokenIn == "" && p.Token0 != nil {
				firstTokenIn = strings.ToLower(p.Token0.Address)
			}
			if p.Disabled {
				disabled = append(disabled, strings.ToLower(addr))
			}
			if !p.Verified {
				reason := p.VerifyReason
				if reason == "" {
					reason = "not yet verified"
				}
				unverified = append(unverified, map[string]string{"pool": strings.ToLower(addr), "reason": reason})
			}
			switch p.DEX {
			case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
				if p.Liquidity == nil || p.Liquidity.Sign() == 0 {
					zeroLiq = append(zeroLiq, strings.ToLower(addr))
				}
			default:
				if p.Reserve0 == nil || p.Reserve0.Sign() == 0 || p.Reserve1 == nil || p.Reserve1.Sign() == 0 {
					zeroLiq = append(zeroLiq, strings.ToLower(addr))
				}
			}
		}
		r.UnknownPools = unknown
		r.DisabledPools = disabled
		r.UnverifiedPools = unverified
		r.ZeroLiquidityPools = zeroLiq
		if len(tokenSet) > 0 {
			toks := make([]string, 0, len(tokenSet))
			for t := range tokenSet {
				toks = append(toks, t)
			}
			sort.Strings(toks)
			r.TokensInCycle = toks
			var outside []string
			for _, t := range toks {
				if !cc.whitelist[t] {
					outside = append(outside, t)
				}
			}
			r.TokensOutsideWhitelist = outside
		}
		if firstTokenIn != "" {
			r.StartTokenAddress = firstTokenIn
			if cc.flashSelector != nil {
				sel := cc.flashSelector.Select(firstTokenIn)
				r.StartTokenNotBorrowable = !sel.Available
			} else if cc.borrowable != nil && !cc.borrowable.Has(firstTokenIn) {
				r.StartTokenNotBorrowable = true
			}
		}

		switch {
		case len(unknown) > 0:
			r.Reason = "missing_pool"
			r.Detail = fmt.Sprintf("%d pool(s) not in registry", len(unknown))
		case len(disabled) > 0:
			r.Reason = "pool_disabled"
			r.Detail = fmt.Sprintf("%d pool(s) flagged Disabled=true (auto-disabled or manual SQLite update)", len(disabled))
		case len(unverified) > 0:
			r.Reason = "pool_unverified"
			r.Detail = fmt.Sprintf("%d pool(s) failed verification — excluded from cycle building", len(unverified))
		case len(zeroLiq) > 0:
			r.Reason = "pool_zero_liquidity"
			r.Detail = fmt.Sprintf("%d pool(s) have zero liquidity/reserves in active range", len(zeroLiq))
		case len(r.TokensOutsideWhitelist) > 0:
			r.Reason = "token_not_whitelisted"
			r.Detail = fmt.Sprintf("%d token(s) not in whitelist: %s", len(r.TokensOutsideWhitelist), strings.Join(r.TokensOutsideWhitelist, ", "))
		case r.StartTokenNotBorrowable:
			r.Reason = "start_token_not_borrowable"
			r.Detail = fmt.Sprintf("start token %s not flash-loanable by any source (Balancer + V3 + Aave)", firstTokenIn)
		}
	}

	if r.Reason == "" {
		r.Reason = "filtered_by_ranking"
		r.Detail = fmt.Sprintf("all pools verified and whitelisted but cycle dropped by DFS ranking (max_dex_per_dest=%d, max_edges_per_node=%d) — the cycle lost to higher-ranked alternatives for at least one hop",
			cc.maxDexPerDest, cc.maxEdgesPerNode)
	}
	return r
}

// rebuild discovers all cycles up to maxHops via DFS and atomically publishes the index.
func (cc *CycleCache) rebuild() {
	allEdges := cc.graph.AllEdges()
	if len(allEdges) == 0 {
		log.Printf("[cyclecache] rebuild skipped: empty graph")
		return
	}
	t0 := time.Now()

	// Build adjacency: tokenAddr → []Edge
	// Only include edges with a valid (non-dead) weight.
	// Strategy: keep the best edge per (tokenOut, DEX) pair, then cap to top-3 DEXes
	// per tokenOut. This guarantees cross-DEX diversity (UniV3 + PancakeV3 + Camelot on
	// the same pair all survive). Then cap to top-10 destinations by best rate to bound
	// the DFS branching factor: 10 dests × 3 DEXes = 30 edges max per node.
	// Worst case: 30^5 = 24M paths, but DFS prunes non-simple paths aggressively so
	// in practice it's ~1-2s. With maxHops=4 it's 30^4 = 810k → well under 1s.
	maxDexPerDest := cc.maxDexPerDest

	// Build whitelist of known token addresses for fast lookup.
	// Only edges where both tokenIn and tokenOut are in the whitelist are considered.
	// This eliminates junk/honeypot tokens that inflate TVL metrics and pollute the DFS.
	whitelist := cc.whitelist

	// Step 1: collect ALL passing edges per (tokenIn, tokenOut, DEX, feeTier) group —
	// whitelisted tokens only. Edges are bucketed into groups but NOT deduped within
	// a group, so multiple pools that share (DEX, feeTier) for the same pair (e.g.
	// the 3 UniV2 USDT/WETH pools) all enter the cache. The marginal LogWeight is a
	// poor proxy for which pool is actually best for a sized trade — slippage curves
	// and TVL matter more — so leave the per-pool sim to discriminate.
	type destDexKey struct {
		tokenOut string
		dex      DEXType
		feeTier  uint32 // FeePPM or FeeBps*100 — distinguishes same-DEX/same-pair pools
	}
	type groupInfo struct {
		edges []Edge
		bestW float64
	}
	groupsByTokenIn := make(map[string]map[destDexKey]*groupInfo, len(allEdges)/4)
	for _, e := range allEdges {
		if e.LogWeight == math.Inf(-1) || e.LogWeight == 0 {
			continue
		}
		if e.Pool != nil && !e.Pool.Verified {
			continue
		}
		// Skip pools the auto-disable mechanism (or manual SQLite UPDATE)
		// has flagged. Disabled pools stay in the registry — we just exclude
		// them from cycle building until either the operator re-enables
		// them or the bot restarts. Since the cycle cache rebuilds every
		// ~15s, this is the fastest guard between detecting a scam pool
		// and stopping the bot from routing through it.
		if e.Pool != nil && e.Pool.Disabled {
			continue
		}
		// Composite pool-quality filter — absolute TVL floor, tick count,
		// high-fee/small-tvl combo, volume/tvl dead-pool floor. Fast-path
		// guard that catches pools within ~15s of them entering any
		// reject condition, rather than waiting for the hourly prune()
		// pass. Each check is input-gated so freshly-loaded pools without
		// populated state pass through until the next rebuild.
		if e.Pool != nil && poolQualityReason(e.Pool) != "" {
			continue
		}
		if !whitelist[strings.ToLower(e.TokenIn.Address)] || !whitelist[strings.ToLower(e.TokenOut.Address)] {
			continue
		}
		tokenInKey := strings.ToLower(e.TokenIn.Address)
		if groupsByTokenIn[tokenInKey] == nil {
			groupsByTokenIn[tokenInKey] = make(map[destDexKey]*groupInfo)
		}
		fee := e.Pool.FeePPM
		if fee == 0 {
			fee = e.Pool.FeeBps * 100
		}
		k := destDexKey{strings.ToLower(e.TokenOut.Address), e.Pool.DEX, fee}
		g, ok := groupsByTokenIn[tokenInKey][k]
		if !ok {
			g = &groupInfo{}
			groupsByTokenIn[tokenInKey][k] = g
		}
		g.edges = append(g.edges, e)
		if len(g.edges) == 1 || e.LogWeight > g.bestW {
			g.bestW = e.LogWeight
		}
	}

	// Step 2: per tokenOut, keep top-maxDexPerDest (DEX, feeTier) GROUPS by best
	// in-group LogWeight; preserve ALL edges within each surviving group.
	adj := make(map[string][]Edge, len(groupsByTokenIn))
	type rankedGroup struct {
		key  destDexKey
		info *groupInfo
	}
	for tokenInKey, destDexMap := range groupsByTokenIn {
		// Group by tokenOut.
		byDest := make(map[string][]rankedGroup)
		for k, g := range destDexMap {
			byDest[k.tokenOut] = append(byDest[k.tokenOut], rankedGroup{k, g})
		}
		var edges []Edge
		for _, groups := range byDest {
			// Sort groups descending by best in-group LogWeight.
			for i := 0; i < len(groups); i++ {
				for j := i + 1; j < len(groups); j++ {
					if groups[j].info.bestW > groups[i].info.bestW {
						groups[i], groups[j] = groups[j], groups[i]
					}
				}
			}
			// Keep top-maxDexPerDest **with ties at the cutoff also kept**.
			//
			// Without the tied-at-cutoff rule, a hot pair like USDC/USDT where
			// 4 different DEXes (UniV3 + PancakeV3 + SushiV3 + UniV4) all sit
			// at the same 100ppm fee tier — and therefore tie on log weight —
			// would have only 1 of the 4 survive a max_dex_per_dest=3 cap (the
			// 8ppm + 25ppm pools take the first 2 slots, then ONE 100ppm pool
			// wins the third slot via map iteration order). The other 3
			// (including the deepest $25B+ UniV3 pool) get silently dropped.
			//
			// Keeping ties at the cutoff means: if the Nth-ranked entry has
			// log weight W, all entries with log weight == W are kept. For
			// pairs without ties, this is identical to the plain top-N. For
			// hot pairs with shared fee tiers, all the tied entries survive.
			if len(groups) > maxDexPerDest {
				cutoff := groups[maxDexPerDest-1].info.bestW
				keep := maxDexPerDest
				for keep < len(groups) && groups[keep].info.bestW == cutoff {
					keep++
				}
				groups = groups[:keep]
			}
			for _, g := range groups {
				edges = append(edges, g.info.edges...)
			}
		}
		// Sort destinations by number of DEXes (descending) — destinations with more
		// DEXes have more cross-DEX arb potential. Within same destination, sort by
		// best LogWeight. This prevents high-value pairs like WETH/USDC (low rate but
		// many DEXes) from being pruned in favor of obscure high-rate tokens.
		destDexCount := make(map[string]int)
		for _, e := range edges {
			destDexCount[strings.ToLower(e.TokenOut.Address)]++
		}
		for i := 0; i < len(edges); i++ {
			for j := i + 1; j < len(edges); j++ {
				di := destDexCount[strings.ToLower(edges[i].TokenOut.Address)]
				dj := destDexCount[strings.ToLower(edges[j].TokenOut.Address)]
				if dj > di || (dj == di && edges[j].LogWeight > edges[i].LogWeight) {
					edges[i], edges[j] = edges[j], edges[i]
				}
			}
		}
		// Always preserve stablecoin bridge edges AND fee-tier siblings.
		if len(edges) > cc.maxEdgesPerNode {
			// CAREFUL: do NOT use `edges[:0]` for both preserved and rest —
			// they would share the same backing array and append-after-append
			// would silently overwrite each other's elements as the loop reads
			// from `edges`. (We hit this in 2026-04: the $25.8B UniV3 USDC/USDT
			// pool was being silently dropped every rebuild because it sat in
			// USDC's outgoing edge list at exactly the wrong index.)

			// Pass 1: identify which (tokenOut, DEX) pairs survived the top-N cut.
			// Any fee-tier sibling of a surviving pair is also preserved. This
			// catches fee-tier arbs (e.g., UniV3 WETH/USDT 5bps vs 1bps) which
			// would otherwise be pruned because the lower-TVL tier ranks too low
			// among 1400+ WETH edges.
			type pairDex struct {
				tokenOut string
				dex      DEXType
			}
			survivedPairs := make(map[pairDex]bool)
			cutIdx := cc.maxEdgesPerNode
			if cutIdx > len(edges) {
				cutIdx = len(edges)
			}
			for _, e := range edges[:cutIdx] {
				survivedPairs[pairDex{strings.ToLower(e.TokenOut.Address), e.Pool.DEX}] = true
			}

			preserved := make([]Edge, 0, len(edges))
			rest := make([]Edge, 0, len(edges))
			for _, e := range edges {
				if isStableBridgeEdge(e) {
					preserved = append(preserved, e)
				} else if survivedPairs[pairDex{strings.ToLower(e.TokenOut.Address), e.Pool.DEX}] {
					preserved = append(preserved, e)
				} else {
					rest = append(rest, e)
				}
			}
			cap := cc.maxEdgesPerNode - len(preserved)
			if cap < 0 {
				cap = 0
			}
			if len(rest) > cap {
				rest = rest[:cap]
			}
			edges = append(preserved, rest...)
		}
		adj[tokenInKey] = edges
	}

	// Debug: count adjacency stats
	totalAdj := 0
	for _, e := range adj {
		totalAdj += len(e)
	}
	// Count how many raw edges have both tokens whitelisted
	wlEdges := 0
	liveEdges := 0
	edgeTokens := make(map[string]bool)
	for _, e := range allEdges {
		if e.LogWeight == math.Inf(-1) || e.LogWeight == 0 {
			continue
		}
		liveEdges++
		ti := strings.ToLower(e.TokenIn.Address)
		to := strings.ToLower(e.TokenOut.Address)
		edgeTokens[ti] = true
		edgeTokens[to] = true
		if whitelist[ti] && whitelist[to] {
			wlEdges++
		}
	}
	_ = fmt.Sprintf // keep import used by other code paths

	// Start DFS only from tokens that:
	//   1. Are in the whitelist (excludes junk/honeypot tokens)
	//   2. Have ≥2 outgoing edges (otherwise no cycles possible)
	//   3. Are flash-loanable from the Balancer Vault (cc.borrowable, when set)
	//
	// The borrowable filter eliminates the BAL#528 sim-reject failure class:
	// without it, the DFS produces cycles starting at exotic tokens like EVA,
	// ZRO, #BB that Balancer doesn't hold, the bot detects them as profitable,
	// runs the eth_call, and gets BAL#528 every time. See borrowable.go.
	type nodeScore struct {
		addr   string
		degree int
	}
	var startCandidates []nodeScore
	skippedNotBorrowable := 0
	for addr, edges := range adj {
		if !whitelist[addr] {
			continue
		}
		if len(edges) < 2 {
			continue
		}
		// Check if this token can be flash-borrowed from ANY source.
		// Priority: flash selector (Balancer+V3+Aave) > borrowable (Balancer only).
		if cc.flashSelector != nil {
			if sel := cc.flashSelector.Select(addr); !sel.Available {
				skippedNotBorrowable++
				continue
			}
		} else if cc.borrowable != nil && !cc.borrowable.Has(addr) {
			skippedNotBorrowable++
			continue
		}
		startCandidates = append(startCandidates, nodeScore{addr, len(edges)})
	}

	// Parallelize per-start-token DFS. Each start token's DFS is independent —
	// the only shared state is the dedup set, which is content-addressed
	// (sorted pool addresses) so concurrent inserts via sync.Map are safe.
	// Each worker writes into its own []cachedCycle and the merge step at the
	// end concatenates them. Used to be sequential at ~43s for 98k cycles;
	// parallel cuts that to ~8-12s on an 8-core box.
	workers := runtime.NumCPU()
	if workers > len(startCandidates) {
		workers = len(startCandidates)
	}
	if workers < 1 {
		workers = 1
	}

	var seenSync sync.Map
	workCh := make(chan string, len(startCandidates))
	for _, cand := range startCandidates {
		workCh <- cand.addr
	}
	close(workCh)

	workerOuts := make([][]cachedCycle, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []cachedCycle
			for startAddr := range workCh {
				dfsConcurrent(startAddr, startAddr, nil, 0, cc.maxHops, adj, &seenSync, &local)
			}
			workerOuts[w] = local
		}()
	}
	wg.Wait()

	var all []cachedCycle
	totalCount := 0
	for _, out := range workerOuts {
		totalCount += len(out)
	}
	all = make([]cachedCycle, 0, totalCount)
	for _, out := range workerOuts {
		all = append(all, out...)
	}

	// Build pool → cycle index
	byPool := make(map[string][]int, len(all)*2)
	for i, cc := range all {
		for _, addr := range cc.PoolAddrs {
			byPool[addr] = append(byPool[addr], i)
		}
	}

	newIdx := &cycleIndex{byPool: byPool, all: all}
	atomic.StorePointer(&cc.ptr, unsafe.Pointer(newIdx))

	ms := float64(time.Since(t0).Microseconds()) / 1000.0
	if len(all) != cc.lastCount || cc.lastBuild.IsZero() {
		log.Printf("[cyclecache] rebuilt: %d cycles across %d pools (%.1fms)", len(all), len(byPool), ms)
		cc.lastCount = len(all)
	}
	cc.lastBuild = time.Now()
	if cc.OnRebuild != nil && len(all) > 0 {
		cc.OnRebuild()
	}
}

// dfs discovers cycles via depth-first search.
// path accumulates edges from startAddr back to startAddr.
// stablecoins is the set of USD-pegged token addresses on Arbitrum.
// Edges between any two of these are stablecoin bridge hops — always preserved.
// pegGroups defines sets of near-parity tokens. Edges between any two tokens in
// the same group are "bridge edges" and are always preserved regardless of the
// maxEdgesPerNode cap, because near-zero log weight causes them to rank last
// yet they are essential intermediaries in multi-leg cycles.
var pegGroups = []map[string]bool{
	{
		"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true, // USDC
		"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true, // USDC.e
		"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true, // USDT
		"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true, // DAI
		"0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34": true, // USDe
		"0x93b346b6bc2548da6a1e7d98e9a421b42541425b": true, // LUSD
		"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true, // FRAX
	},
	{
		"0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f": true, // WBTC
		"0x6c84a8f1c29108f47a79964b5fe888d4f4d0de40": true, // tBTC
	},
}

// stablecoins is kept for backward compatibility with the rebuild log.
var stablecoins = pegGroups[0]

func isStableBridgeEdge(e Edge) bool {
	inAddr  := strings.ToLower(e.TokenIn.Address)
	outAddr := strings.ToLower(e.TokenOut.Address)
	for _, group := range pegGroups {
		if group[inAddr] && group[outAddr] {
			return true
		}
	}
	return false
}

func dfs(
	startAddr string,
	curAddr string,
	path []Edge,
	depth int,
	maxDepth int,
	adj map[string][]Edge,
	seen map[string]bool,
	out *[]cachedCycle,
) {
	if depth >= maxDepth {
		return
	}

	edges, ok := adj[curAddr]
	if !ok {
		return
	}

	for _, e := range edges {
		nextAddr := strings.ToLower(e.TokenOut.Address)

		// Cycle complete: back to start
		if nextAddr == startAddr && depth >= 1 {
			newPath := make([]Edge, len(path)+1)
			copy(newPath, path)
			newPath[len(path)] = e

			lp := logProfit(newPath)

			// Dedup: canonical key is sorted pool addresses
			poolAddrs := make([]string, len(newPath))
			for i, edge := range newPath {
				poolAddrs[i] = strings.ToLower(edge.Pool.Address)
			}
			key := cycleKey(newPath)
			if seen[key] {
				continue
			}
			seen[key] = true

			*out = append(*out, cachedCycle{
				Cycle: Cycle{
					Edges:     newPath,
					LogProfit: lp,
					ProfitPct: (math.Exp(lp) - 1) * 100,
				},
				PoolAddrs:   poolAddrs,
				logProfitAt: lp,
			})
			continue
		}

		// Avoid re-visiting the same token mid-path (prevents non-simple cycles)
		// except we allow re-entering startAddr only at the final hop.
		alreadyVisited := false
		for _, pe := range path {
			if strings.ToLower(pe.TokenIn.Address) == nextAddr {
				alreadyVisited = true
				break
			}
		}
		if alreadyVisited {
			continue
		}

		// Avoid using the same pool twice in a path
		for _, pe := range path {
			if strings.ToLower(pe.Pool.Address) == strings.ToLower(e.Pool.Address) {
				alreadyVisited = true
				break
			}
		}
		if alreadyVisited {
			continue
		}

		dfs(startAddr, nextAddr, append(path, e), depth+1, maxDepth, adj, seen, out)
	}
}

// dfsConcurrent is a thread-safe version of dfs used by the parallelized
// rebuild path. It uses sync.Map for the shared dedup set so multiple workers
// can run start-token DFS in parallel without serializing on a mutex. The
// dedup is content-addressed (sorted pool addresses), so concurrent
// LoadOrStore is correct: the first goroutine to claim a key wins, the others
// see `loaded=true` and skip.
func dfsConcurrent(
	startAddr string,
	curAddr string,
	path []Edge,
	depth int,
	maxDepth int,
	adj map[string][]Edge,
	seen *sync.Map,
	out *[]cachedCycle,
) {
	if depth >= maxDepth {
		return
	}

	edges, ok := adj[curAddr]
	if !ok {
		return
	}

	for _, e := range edges {
		nextAddr := strings.ToLower(e.TokenOut.Address)

		// Cycle complete: back to start
		if nextAddr == startAddr && depth >= 1 {
			newPath := make([]Edge, len(path)+1)
			copy(newPath, path)
			newPath[len(path)] = e

			lp := logProfit(newPath)

			poolAddrs := make([]string, len(newPath))
			for i, edge := range newPath {
				poolAddrs[i] = strings.ToLower(edge.Pool.Address)
			}
			key := cycleKey(newPath)
			if _, loaded := seen.LoadOrStore(key, true); loaded {
				continue
			}

			*out = append(*out, cachedCycle{
				Cycle: Cycle{
					Edges:     newPath,
					LogProfit: lp,
					ProfitPct: (math.Exp(lp) - 1) * 100,
				},
				PoolAddrs:   poolAddrs,
				logProfitAt: lp,
			})
			continue
		}

		// Avoid re-visiting the same token mid-path (prevents non-simple cycles)
		alreadyVisited := false
		for _, pe := range path {
			if strings.ToLower(pe.TokenIn.Address) == nextAddr {
				alreadyVisited = true
				break
			}
		}
		if alreadyVisited {
			continue
		}

		// Avoid using the same pool twice in a path
		for _, pe := range path {
			if strings.ToLower(pe.Pool.Address) == strings.ToLower(e.Pool.Address) {
				alreadyVisited = true
				break
			}
		}
		if alreadyVisited {
			continue
		}

		dfsConcurrent(startAddr, nextAddr, append(path, e), depth+1, maxDepth, adj, seen, out)
	}
}

// ScoreCycle re-evaluates a cached cycle's current log profit from live edge weights.
// Uses e.effectiveRate() via the live Pool pointer so a swap that just moved prices
// is reflected immediately — not the stale LogWeight snapshot from the last rebuild.
// Returns (logProfit, isPositive).
func ScoreCycle(c cachedCycle) (float64, bool) {
	lp := 0.0
	for _, e := range c.Cycle.Edges {
		w := logWeightFor(e.effectiveRate())
		if w == math.Inf(-1) || w == 0 {
			return 0, false
		}
		lp += w
	}
	return lp, lp > 0
}
