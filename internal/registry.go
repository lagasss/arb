package internal

import (
	"strings"
	"sync"
)

type PoolFilter func(p *Pool) bool

type PoolRegistry struct {
	mu      sync.RWMutex
	pools   map[string]*Pool   // address → pool
	byPair  map[string][]*Pool // pairKey → pools
	byToken map[string][]*Pool // lower-case token address → pools containing that token
	filters []PoolFilter
}

func NewPoolRegistry(filters ...PoolFilter) *PoolRegistry {
	return &PoolRegistry{
		pools:   make(map[string]*Pool),
		byPair:  make(map[string][]*Pool),
		byToken: make(map[string][]*Pool),
		filters: filters,
	}
}

// MinTVLFilter rejects pools below minTVL USD.
func MinTVLFilter(minTVL float64) PoolFilter {
	return func(p *Pool) bool { return p.TVLUSD >= minTVL }
}

// WhitelistFilter accepts only pools where at least one token is whitelisted.
func WhitelistFilter(tr *TokenRegistry) PoolFilter {
	return func(p *Pool) bool {
		return tr.IsWhitelisted(p.Token0.Address) || tr.IsWhitelisted(p.Token1.Address)
	}
}

// MinVolumeFilter rejects pools below min 24h volume.
func MinVolumeFilter(minVol float64) PoolFilter {
	return func(p *Pool) bool { return p.Volume24hUSD >= minVol }
}

// MaxTVLFilter rejects pools above maxTVL USD (set 0 to disable).
func MaxTVLFilter(maxTVL float64) PoolFilter {
	return func(p *Pool) bool { return maxTVL == 0 || p.TVLUSD <= maxTVL }
}

// ForceAdd bypasses all filters — used for seed pools that are explicitly trusted.
// If the pool already exists, ForceAdd MERGES the new fields into the existing
// pool instance and returns that existing pointer. It never replaces the
// registry entry for an existing address. This preserves pointer identity for
// all consumers (cycle cache, graph edges, swap listener callbacks) so they
// continue seeing the same *Pool across re-seeds.
//
// Pre-fix, ForceAdd replaced the entry with the new Pool and aliased big.Int
// pointers between old and new. Subsequent swap-event updates would call
// UpdateFromV3Swap on the NEW pool (reassigning its SqrtPriceX96 pointer),
// leaving the OLD pool — still held by the cycle cache — pointing at a stale
// big.Int. Trade 132 (2026-04-14 00:58 UTC) was the first concrete evidence:
// cached sqrtPrice for ARB/USDC 500 was 1.32% off from chain, producing
// a +266 bps phantom profit that reverted on-chain.
//
// Returns (canonical *Pool, ok). On success, callers MUST use the returned
// pointer (not the p they passed in) for graph.AddPool and downstream use.
func (r *PoolRegistry) ForceAdd(p *Pool) (*Pool, bool) {
	if p.Disabled {
		return nil, false
	}
	// Pool-quality composite filter — applies to ForceAdd too, so subgraph
	// seeds / pinned pools / competitor-watcher adds all face the same gate
	// as discovered pools. Subsumes the absoluteMinTVL floor plus the
	// tick-count, high-fee, and volume-ratio checks. Each check is gated on
	// its input being populated, so a freshly-constructed pool with only
	// (address, dex, token0, token1) still passes.
	if reason := poolQualityReason(p); reason != "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	addr := strings.ToLower(p.Address)
	if existing, ok := r.pools[addr]; ok {
		// MERGE IN PLACE. Existing stays in the registry; p is discarded.
		existing.mergeFrom(p)
		return existing, true
	}
	r.pools[addr] = p
	key := p.PairKey()
	r.byPair[key] = append(r.byPair[key], p)
	t0 := strings.ToLower(p.Token0.Address)
	t1 := strings.ToLower(p.Token1.Address)
	r.byToken[t0] = append(r.byToken[t0], p)
	r.byToken[t1] = append(r.byToken[t1], p)
	return p, true
}

// Add inserts or merges a pool into the registry. Same merge-in-place contract
// as ForceAdd — if an entry exists for this address, `p` is merged into the
// existing pool and the existing pointer is returned; otherwise `p` is added
// directly. See ForceAdd's comment for the rationale (trade 132 phantom sqrt).
//
// Returns (canonical *Pool, ok). On success, callers MUST use the returned
// pointer for subsequent graph.AddPool and downstream operations.
func (r *PoolRegistry) Add(p *Pool) (*Pool, bool) {
	if p.Disabled {
		return nil, false
	}
	// Composite quality filter (TVL floor, tick count, high-fee combo,
	// volume/TVL ratio). Same check used by ForceAdd and the cyclecache
	// rebuild so every path has consistent rejection semantics.
	if reason := poolQualityReason(p); reason != "" {
		return nil, false
	}
	for _, f := range r.filters {
		if !f(p) {
			return nil, false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	addr := strings.ToLower(p.Address)
	if existing, ok := r.pools[addr]; ok {
		existing.mergeFrom(p)
		return existing, true
	}
	r.pools[addr] = p
	key := p.PairKey()
	r.byPair[key] = append(r.byPair[key], p)
	t0 := strings.ToLower(p.Token0.Address)
	t1 := strings.ToLower(p.Token1.Address)
	r.byToken[t0] = append(r.byToken[t0], p)
	r.byToken[t1] = append(r.byToken[t1], p)
	return p, true
}

func (r *PoolRegistry) Remove(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	addr := strings.ToLower(address)
	p, ok := r.pools[addr]
	if !ok {
		return
	}
	delete(r.pools, addr)
	key := p.PairKey()
	peers := r.byPair[key]
	updated := peers[:0]
	for _, peer := range peers {
		if strings.ToLower(peer.Address) != addr {
			updated = append(updated, peer)
		}
	}
	if len(updated) == 0 {
		delete(r.byPair, key)
	} else {
		r.byPair[key] = updated
	}
	for _, tk := range []string{strings.ToLower(p.Token0.Address), strings.ToLower(p.Token1.Address)} {
		tpeers := r.byToken[tk]
		tupdated := tpeers[:0]
		for _, peer := range tpeers {
			if strings.ToLower(peer.Address) != addr {
				tupdated = append(tupdated, peer)
			}
		}
		if len(tupdated) == 0 {
			delete(r.byToken, tk)
		} else {
			r.byToken[tk] = tupdated
		}
	}
}

// PoolsForToken returns all registered pools that contain the given token address.
func (r *PoolRegistry) PoolsForToken(tokenAddr string) []*Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byToken[strings.ToLower(tokenAddr)]
}

func (r *PoolRegistry) Get(address string) (*Pool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pools[strings.ToLower(address)]
	return p, ok
}

func (r *PoolRegistry) All() []*Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Pool, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p)
	}
	return out
}

// PeersFor returns other pools for the same token pair (cross-DEX arb targets).
func (r *PoolRegistry) PeersFor(p *Pool) []*Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := r.byPair[p.PairKey()]
	out := make([]*Pool, 0, len(all)-1)
	for _, peer := range all {
		if strings.ToLower(peer.Address) != strings.ToLower(p.Address) {
			out = append(out, peer)
		}
	}
	return out
}

func (r *PoolRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pools)
}
