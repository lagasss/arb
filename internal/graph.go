package internal

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// poolStaleness is how long a pool can go without a state update before its
// edges are zeroed in the graph. Pools that never receive swap events and fail
// multicall won't create phantom BF cycles.
var poolStaleness = 5 * time.Minute

// absoluteMinTVLUSD is the hard TVL floor enforced in the cycle cache rebuild
// filter. Set from cfg.Pools.AbsoluteMinTVLUSD in NewBot — see that field's
// comment for the rationale. 0 = disabled. The value is also enforced at
// ForceAdd/Add time in registry.go and once per hour in prune(); the cycle
// cache check is the fast-path guard that catches below-floor pools within
// ~15s (next rebuild) rather than waiting for the hourly prune cycle.
var absoluteMinTVLUSD float64

// Pool-quality composite filter knobs. All default to 0 (disabled) at zero
// value; NewBot sets them from cfg.Pools. See the comments on those config
// fields for the rationale behind each default.
var (
	minTickCount         int     // cfg.Pools.MinTickCount
	highFeeTierMinTVLUSD float64 // cfg.Pools.HighFeeTierMinTVLUSD
	minVolumeTVLRatio    float64 // cfg.Pools.MinVolumeTVLRatio
	// minVolumeTVLRatioExemptTVLUSD: pools with TVL above this value are
	// exempt from the dead-pool volume/TVL ratio check. Multi-billion-dollar
	// stable pools (USDC/USDT at $6B+ TVL) sit naturally at 0.01%-0.1%
	// daily turnover by ratio — way below the 0.1% floor — despite being
	// among the most-swapped pools on chain in absolute USD terms. Without
	// the exemption they get pruned, FetchTickMaps never populates their
	// bitmaps, the simulator returns 0, and every cycle routing through
	// them is silently dropped. Set to 0 to disable the exemption.
	minVolumeTVLRatioExemptTVLUSD float64
	// minTickCountBypassTVLUSD: TVL above which the tick-count floor is
	// skipped. TVL proxies trade capacity — niche legitimate pairs like
	// tBTC/USDC ($27k, 5 ticks — competitor_arbs id 15982) still absorb
	// small arb notionals. Above this value the tick-count gate is skipped.
	minTickCountBypassTVLUSD float64
)

// poolQualityReason returns a non-empty rejection reason if the pool fails
// any of the quality checks configured via package vars, or "" if the pool
// passes (including the case where a check's input hasn't been populated
// yet — we never reject on "not yet measured" data).
//
// Design principles:
//  1. Each check is independent — a pool only has to fail ONE to be rejected.
//  2. Each check is gated on its input being populated (e.g. we only apply
//     the tick-count floor when the pool has actually been through
//     FetchTickMaps and its Ticks slice is non-nil, so fresh pools aren't
//     rejected prematurely at ForceAdd time).
//  3. The returned string is used for logging only — it's not persisted to
//     the DB. Operators grep bot.log for "[quality-reject]" to see which
//     pools got filtered and why.
//
// Called from three places:
//   - registry.ForceAdd / registry.Add: initial insertion gate
//   - cyclecache.rebuild: per-rebuild fast guard (~15s cadence)
//   - bot.prune: hourly full eviction
func poolQualityReason(p *Pool) string {
	if p == nil {
		return ""
	}
	// TVL hard floor — kept in sync with the absoluteMinTVLUSD var applied
	// elsewhere so "quality" can be a single-function check. Only rejects
	// on TVL > 0 (never on "TVL not yet measured").
	if absoluteMinTVLUSD > 0 && p.TVLUSD > 0 && p.TVLUSD < absoluteMinTVLUSD {
		return fmt.Sprintf("tvl=$%.0f < floor $%.0f", p.TVLUSD, absoluteMinTVLUSD)
	}
	// Tick-count floor for V3/V4 pools. Only applies once tick data has
	// been fetched (TicksUpdatedAt non-zero AND Ticks slice has been
	// populated at least once). Pools freshly loaded from DB have
	// Ticks=nil and TicksUpdatedAt=zero — those get a free pass until
	// FetchTickMaps has run at least once on them.
	if minTickCount > 0 && !p.TicksUpdatedAt.IsZero() {
		switch p.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
			bypass := minTickCountBypassTVLUSD
			if bypass <= 0 {
				bypass = 1_000_000
			}
			if len(p.Ticks) > 0 && len(p.Ticks) < minTickCount && p.TVLUSD < bypass {
				return fmt.Sprintf("tick_count=%d < floor %d (tvl=$%.0f < bypass $%.0f)", len(p.Ticks), minTickCount, p.TVLUSD, bypass)
			}
		}
	}
	// 1% fee tier + small TVL combo — pump-and-dump signature. Only
	// applies when fee_ppm is populated AND matches exactly 10000 (1%).
	// Note: we check fee_ppm rather than fee_bps because sub-bps fees
	// would slip through a `fee_bps * 100 == 10000` comparison.
	if highFeeTierMinTVLUSD > 0 && p.FeePPM == 10000 && p.TVLUSD > 0 && p.TVLUSD < highFeeTierMinTVLUSD {
		return fmt.Sprintf("1%% fee tier with tvl=$%.0f < $%.0f", p.TVLUSD, highFeeTierMinTVLUSD)
	}
	// Dead-pool volume floor — volume / TVL ratio. Legitimate dormant
	// pools can legitimately sit at near-zero volume for short periods,
	// so we only apply this check when BOTH tvl AND volume are
	// populated (Volume24hUSD > 0 implies the subgraph or metrics loop
	// has updated it at least once). A pool that never had its volume
	// measured at all passes.
	if minVolumeTVLRatio > 0 && p.TVLUSD > 0 && p.Volume24hUSD > 0 {
		// Exempt deep pools — see minVolumeTVLRatioExemptTVLUSD comment.
		if minVolumeTVLRatioExemptTVLUSD <= 0 || p.TVLUSD < minVolumeTVLRatioExemptTVLUSD {
			ratio := p.Volume24hUSD / p.TVLUSD
			if ratio < minVolumeTVLRatio {
				return fmt.Sprintf("volume/tvl=%.4f < floor %.4f (dead pool)", ratio, minVolumeTVLRatio)
			}
		}
	}
	return ""
}

// Edge represents one swap direction through a pool.
type Edge struct {
	Pool      *Pool
	TokenIn   *Token
	TokenOut  *Token
	LogWeight float64 // log(effective_rate_after_fee); updated every block
}

// effectiveRate computes the spot rate for this edge direction after fee.
// Returns 0 if the pool has no valid state (sqrtPrice/liquidity/reserves).
// Note: staleness is NOT checked here — that's handled by the health gate
// before trade submission. The graph should reflect all pools with known state
// so the cycle cache can discover all viable paths.
func (e *Edge) effectiveRate() float64 {
	var rawRate float64
	switch e.Pool.DEX {
	case DEXUniswapV3, DEXCamelotV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXZyberV3, DEXUniswapV4:
		// Simulator requires both SqrtPriceX96 and Liquidity. If either is missing,
		// mark the edge dead so BF doesn't find phantom cycles it can't simulate.
		if e.Pool.SqrtPriceX96 == nil || e.Pool.SqrtPriceX96.Sign() == 0 ||
			e.Pool.Liquidity == nil || e.Pool.Liquidity.Sign() == 0 {
			return 0
		}
		sp := e.Pool.SpotRate()
		if e.TokenIn.Address == e.Pool.Token0.Address {
			rawRate = sp
		} else {
			if sp == 0 {
				return 0
			}
			rawRate = 1.0 / sp
		}
	default:
		sp := e.Pool.SpotRate()
		if e.TokenIn.Address == e.Pool.Token0.Address {
			rawRate = sp
		} else {
			if sp == 0 {
				return 0
			}
			rawRate = 1.0 / sp
		}
	}
	// Use directional fees for Camelot V2 pools (calibrated from getAmountOut).
	// FeePPM overrides FeeBps for sub-1bps pools (e.g. RamsesV3 fee=1 = 0.0001%).
	var fee float64
	if e.Pool.FeePPM > 0 {
		fee = float64(e.Pool.FeePPM) / 1_000_000.0
	} else if e.Pool.DEX == DEXCamelot {
		// Camelot V2: use calibrated directional fee for this swap direction.
		var feeBps uint32
		if strings.EqualFold(e.TokenIn.Address, e.Pool.Token0.Address) {
			feeBps = e.Pool.Token0FeeBps
		} else {
			feeBps = e.Pool.Token1FeeBps
		}
		if feeBps == 0 {
			feeBps = e.Pool.FeeBps // fallback to pool-level
		}
		fee = float64(feeBps) / 10000.0
	} else {
		feeBps := e.Pool.FeeBps
		if feeBps == 0 && (e.Pool.DEX == DEXCamelotV3 || e.Pool.DEX == DEXZyberV3) {
			feeBps = camelotV3DefaultFeeBps
		}
		fee = float64(feeBps) / 10000.0
	}
	return rawRate * (1.0 - fee)
}

type Graph struct {
	mu    sync.RWMutex
	nodes map[string]bool   // token address → present
	edges map[string][]Edge // tokenIn address → outgoing edges
}

func NewGraph() *Graph {
	return &Graph{
		nodes: make(map[string]bool),
		edges: make(map[string][]Edge),
	}
}

// AddPool adds or updates both directed edges for a pool.
func (g *Graph) AddPool(p *Pool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[p.Token0.Address] = true
	g.nodes[p.Token1.Address] = true
	g.upsertEdge(p, p.Token0, p.Token1)
	g.upsertEdge(p, p.Token1, p.Token0)
}

func logWeightFor(r float64) float64 {
	if r > 0 {
		return math.Log(r)
	}
	return math.Inf(-1)
}

func (g *Graph) upsertEdge(pool *Pool, tokenIn, tokenOut *Token) {
	key := strings.ToLower(tokenIn.Address)
	edges := g.edges[key]
	poolAddr := strings.ToLower(pool.Address)
	for i, e := range edges {
		if strings.ToLower(e.Pool.Address) == poolAddr && e.TokenIn.Address == tokenIn.Address {
			edges[i].Pool = pool // update pool pointer (ForceAdd may have replaced it)
			edges[i].TokenIn = tokenIn
			edges[i].TokenOut = tokenOut
			r := (&Edge{Pool: pool, TokenIn: tokenIn, TokenOut: tokenOut}).effectiveRate()
			edges[i].LogWeight = logWeightFor(r)
			g.edges[key] = edges
			return
		}
	}
	r := (&Edge{Pool: pool, TokenIn: tokenIn, TokenOut: tokenOut}).effectiveRate()
	g.edges[key] = append(edges, Edge{
		Pool:      pool,
		TokenIn:   tokenIn,
		TokenOut:  tokenOut,
		LogWeight: logWeightFor(r),
	})
}

// RemovePool removes all edges referencing the given pool address.
func (g *Graph) RemovePool(address string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	addr := strings.ToLower(address)
	for key, edges := range g.edges {
		updated := edges[:0]
		for _, e := range edges {
			if strings.ToLower(e.Pool.Address) != addr {
				updated = append(updated, e)
			}
		}
		g.edges[key] = updated
	}
}

// UpdateEdgeWeights refreshes log weights for all edges of a pool after reserve update.
func (g *Graph) UpdateEdgeWeights(p *Pool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	poolAddr := strings.ToLower(p.Address)
	for key, edges := range g.edges {
		for i, e := range edges {
			if strings.ToLower(e.Pool.Address) == poolAddr {
				edges[i].LogWeight = logWeightFor(e.effectiveRate())
			}
		}
		g.edges[key] = edges
	}
}

// EdgesFrom returns a snapshot of outgoing edges for a token.
func (g *Graph) EdgesFrom(tokenAddress string) []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	src := g.edges[strings.ToLower(tokenAddress)]
	out := make([]Edge, len(src))
	copy(out, src)
	return out
}

// NodeAddresses returns all token node addresses.
func (g *Graph) NodeAddresses() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.nodes))
	for addr := range g.nodes {
		out = append(out, addr)
	}
	return out
}

// AllEdges returns a flat snapshot of all edges.
func (g *Graph) AllEdges() []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []Edge
	for _, edges := range g.edges {
		out = append(out, edges...)
	}
	return out
}
