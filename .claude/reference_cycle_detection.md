---
name: Cycle detection triggers and frequency
description: Every code path that causes a cycle to be scored, with cadence and why it fires
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
The bot has FOUR trigger paths that cause cycles to be scored. If any of these drifts from spec, sim results become phantom or missed. Accuracy target: every sim run must reflect CURRENT on-chain state.

### 1. Swap-event triggered (`SwapListener.handleSwap` → `FastEval(pool)`)
- WSS `SubscribeFilterLogs` on Swap events (V2/V3/V4 topics)
- On fire: `applyV3Swap` / `applyV2Swap` / `applyV4Swap` updates pool state FROM the event (sqrtPriceX96, tick, liquidity for V3/V4; reserve deltas for V2)
- Then calls `FastEval(pool)` → scores cycles containing this pool
- **Latency**: ~250ms from block mined (WSS delivery + RPC processing)

### 2. Cross-pool propagation (`FastEvalPeer`)
- Same hook point as #1; after scoring the swapped pool, for each same-pair peer
- `RefreshPools(peers)` does a targeted `BatchUpdatePools` (slot0 + liquidity) + `FetchV4PoolStates` + conditional `FetchTickMaps` for peers whose tick moved ≥1 spacing
- Then `FastEvalPeer(peer)` runs unthrottled (bypasses `MinPriceDelta` check)
- Why: the peer didn't swap but its price is now mispriced vs the one that did

### 3. Block-level scoring (`blockScoringLoop`)
- Ticker: 250ms × `block_scoring_interval` (default 1 = every 250ms ≈ every Arbitrum block)
- Hot-cycle filter: only scores pools whose `SpotPrice` differs from `blockScorePrice[addr]` snapshot. Pools that didn't move are skipped
- Calls `doFastEvalCycles(pool, skipThrottle=true)` per changed pool
- Catches drift-based arbs where no swap event triggered (price accumulates between swaps across pools)

### 4. Targeted refresh inside `doFastEvalCycles`
- Before ANY worker scores candidates: collect every unique pool across the candidate cycles, batch-refresh slot0/liquidity/ticks
- `BatchUpdatePools` → `FetchV4PoolStates` → conditional `FetchTickMaps` (only pools where `|curTick − TickAtFetch| ≥ TickSpacing`) → `UpdateEdgeWeights`
- Budget: ~800ms-2s timeout. Fails silently on RPC error (scoring proceeds with stale data; eth_call catches phantom trades)

### Cycle cache rebuild (not a trigger, but sets the search space)
- `cycleCache.Run` every `cycle_rebuild_secs` (default 60s)
- Parallel DFS from borrowable start tokens through graph edges
- Edge pruning: `max_dex_per_dest: 7` (top-7 DEXes per dest, fee-tier siblings preserved), `max_edges_per_node: 120` (top-120 outgoing edges per token), `max_hops: 5`
- Whitelist: `cycleCache.whitelist` (test_mode → `major.tokens` only; else all registry tokens)
- Only pools with `Verified=true` (or never-verified default state) participate

### Accuracy failure modes
- **State-from-event mismatch**: V2 applies deltas (`UpdateFromV2Swap`) — accumulates rounding drift over many swaps; reconciled only at next multicall
- **Multi-hop compound error**: per-hop sim drift × N hops; a 0.5%/hop error compounds to 2.5% over 5 hops (exceeds 200 bps slippage)
- **Tick bitmap staleness**: `FetchTickMaps` runs every 5s, eager refetch on tick crossings; between fetches, added/removed ticks produce wrong liquidityNet
- **Pool not refreshed**: if `BatchUpdatePools` times out for a pool, it scores on last-known state (possibly 5s+ old)
