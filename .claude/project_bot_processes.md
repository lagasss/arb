---
name: Bot process architecture
description: All goroutines running inside cmd/bot, their cadence, and data flow
type: project
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Bot (`cmd/bot`) runs these concurrent loops, all sharing state via the `Bot` struct and the `PoolRegistry`/`Graph`/`CycleCache`:

**Data freshness pipeline**
- `watchNewBlocks` — subscribes to WSS new-head events. On each block, triggers `BatchUpdatePools` (all registered pools: slot0/reserves/liquidity/tickSpacing). Falls back to 5s safety ticker if WSS drops.
- `watchTickMapsLoop` — every **5s**, fetches tick bitmap + liquidityNet for V3/V4 cycle pools via `FetchTickMaps`. V3 pools query the pool contract's `tickBitmap()` + `ticks()`; V4 pools query `StateView.getTickBitmap()` + `StateView.getTickLiquidity()` — SAME FetchTickMaps function handles both. Also consumes `TickRefetchCh` for eager single-pool refetches (triggered by swap events that cross tick boundaries).
- `algebraFeeRefreshLoop` — drains `swapListener.AlgebraFeeRefreshCh` (event-driven, not periodic); re-reads CamelotV3/ZyberV3 dynamic fee via `globalState()` after each swap event.
- `swapListener.Run` — WSS `SubscribeFilterLogs` on Swap events (V2/V3/V4). Updates pool state in memory from event data BEFORE the next multicall lands (sub-block latency).

**Discovery & maintenance**
- `watchPoolCreation` — subscribes to PoolCreated/PairCreated events, adds new pools to the registry.
- `subgraphRefreshLoop` — every `cfg.Pools.Subgraph.RefreshHours` (default **4h**), fetches TVL/volume from subgraphs. Disabled unless `Subgraph.Enabled=true`.
- `pruneLoop` — every **1 hour**, removes dead pools (TVL/volume floor) from the registry.
- `gasMonitorLoop` — every **30s**, polls `eth_gasPrice`, updates `minProfitUSD` and `gasCostPerUnit` atomics.
- `cycleCache.Run` — every `cfg.Strategy.CycleRebuildSecs` (default **60s**, was 15s), rebuilds cycle index via parallel DFS from borrowable tokens.
- `competitorCycleMatchLoop` — every **3s**, calls `annotateCompetitorCycleMatches` to write `cycle_in_memory` flag to competitor_arbs (used by arbscan's classifier).

**Scoring pipeline**
- `SwapListener.handleSwap` (triggered per swap event):
  1. `applyV3Swap` / `applyV2Swap` / `applyV4Swap` → update pool state from event
  2. `FastEval(pool)` → score cycles through this pool (`doFastEvalCycles`)
  3. For each peer pool (same token pair, different DEX): `RefreshPools(peers)` then `FastEvalPeer(peer)` — the cross-pool propagation path
- `blockScoringLoop` — every `BlockScoringInterval` × 250ms, scans pools whose SpotPrice changed since last pass, scores their cycles.

**Candidate flow (`doFastEvalCycles` → `evalOneCandidate` → `trySubmitCandidate`)**
1. Targeted refresh: `BatchUpdatePools` + `FetchV4PoolStates` + conditional `FetchTickMaps` for every unique pool across candidate cycles
2. `UpdateEdgeWeights` to recalc graph spot rates
3. Parallel worker scoring: `ScoreCycle` → LP floor → tick stale gate → `optimalAmountIn` → MinSimProfitBps → flash source selection → V3Mini routing decision
4. If accepted: `trySubmitCandidate` → cooldown → readiness → health → `PauseTrading` gate → eth_call validation → broadcast (or log as observation when paused)

**Execution**
- `Executor.Submit` builds Hop array (`buildHops`), encodes calldata for selected contract (ArbitrageExecutor or V3FlashMini), runs `eth_call` dry-run via `simClient`, broadcasts via `client`.
- `Executor.DryRun` runs buildHops + encode + eth_call WITHOUT broadcasting — used in paused-trading mode for sim validation.

**Logging & forensics**
- `arbLogger.Record` → `arb_observations` table. Logs every accepted candidate with pool_states_json snapshot (sqrtPrice/liquidity/ticks/tickCount/tickSpacing), amount_in_wei, sim_profit_wei, reject_reason. Has a 30s dedup window per cycle key.
- `trackTrade` → `our_trades` table for submitted txs with per-hop forensics.

**Debug HTTP**
- 127.0.0.1:6060 serves `/debug/pools`, `/debug/cycles`, `/debug/health`, `/debug/executor`, `/debug/swaps` — live in-memory state for smoketests and diagnostics.
