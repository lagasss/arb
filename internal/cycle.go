package internal

import (
	"fmt"
	"strings"
)

// Cycle represents a closed-loop arbitrage path through a sequence of pool
// edges, ending at the same token it started with. Cycles are produced by the
// CycleCache (DFS over the token graph) and re-scored on every swap event by
// fastEvalCycles.
type Cycle struct {
	Edges     []Edge
	LogProfit float64 // sum of log weights (positive = profitable at current spot)
	ProfitPct float64 // approximate % profit
}

// WithSnapshots returns a new Cycle whose Edges all reference frozen pool
// snapshots (see Pool.Snapshot). The returned Cycle is safe to pass through
// the simulator and buildHops without observing background pool-state churn.
//
// Pools that appear in multiple edges of the same cycle (e.g. an A→B and B→A
// pair) share a single snapshot — both edges read consistent fields, and we
// only pay one Snapshot() cost per distinct pool.
func (c Cycle) WithSnapshots() Cycle {
	if len(c.Edges) == 0 {
		return c
	}
	snapByAddr := make(map[string]*Pool, len(c.Edges))
	frozen := make([]Edge, len(c.Edges))
	for i, e := range c.Edges {
		key := strings.ToLower(e.Pool.Address)
		clone, ok := snapByAddr[key]
		if !ok {
			clone = e.Pool.Snapshot()
			snapByAddr[key] = clone
		}
		frozen[i] = Edge{
			Pool:      clone,
			TokenIn:   e.TokenIn,
			TokenOut:  e.TokenOut,
			LogWeight: e.LogWeight,
		}
	}
	return Cycle{
		Edges:     frozen,
		LogProfit: c.LogProfit,
		ProfitPct: c.ProfitPct,
	}
}

// Path renders the cycle as a human-readable arrow chain like
// "WBTC →[UniV3] ARB →[UniV3] USDC →[UniV3] WBTC". Used in logs and DB rows.
func (c Cycle) Path() string {
	if len(c.Edges) == 0 {
		return ""
	}
	s := c.Edges[0].TokenIn.Symbol
	for _, e := range c.Edges {
		s += fmt.Sprintf(" →[%s] %s", e.Pool.DEX, e.TokenOut.Symbol)
	}
	return s
}

// logProfit sums the per-edge log weights of a candidate path. A positive
// result means the cycle is profitable at the current edge weights (which are
// log(effective_rate_after_fee) — see Edge.LogWeight).
func logProfit(edges []Edge) float64 {
	sum := 0.0
	for _, e := range edges {
		sum += e.LogWeight
	}
	return sum
}

// cycleKey returns a canonical, direction-independent key for a cycle, built
// from the sorted lowercase pool addresses of its edges. Two cycles that visit
// the same pool set (in any order) collide on this key — used by the per-cycle
// submission cooldown and by arb_observations dedup.
func cycleKey(edges []Edge) string {
	addrs := make([]string, len(edges))
	for i, e := range edges {
		addrs[i] = strings.ToLower(e.Pool.Address)
	}
	sorted := make([]string, len(addrs))
	copy(sorted, addrs)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	key := ""
	for _, a := range sorted {
		key += a
	}
	return key
}
