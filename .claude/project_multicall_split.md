---
name: Multicall + tick-fetcher loop split
description: watchNewBlocks used to bundle BatchUpdatePools, FetchV4PoolStates, and FetchTickMaps into one ~16-19s pass; now BatchUpdatePools/V4 run on a fast loop and FetchTickMaps runs on its own slower loop, eliminating the multicall stale health-reject class
type: project
---

**Symptom (pre-fix):** 15% of all observations in a busy hour were rejected with `health: multicall stale (16-19s ago, max 15s)`. This was a different stale-data class than the earlier "no swap events" issue (which the dedup + cap fix resolved); the failure mode shifted to multicall lag.

**Root cause:** `Bot.watchNewBlocks` ran three things sequentially every 5 seconds:

1. `BatchUpdatePools` — fast (~1-2s for 1700 pools), this is what `Health.MulticallAt` tracks
2. `FetchV4PoolStates` — fast (~1s)
3. **`FetchTickMaps`** — slow (~5-10s for 96 V3-cycle pools × 25 bitmap words each = ~2400 calls in batches of 500)

Plus TVL recomputation and graph weight updates after that.

The pass took 16-19s end-to-end. Since `Health.MulticallAt` was updated at the start of the pass and then the next pass couldn't begin until the previous one finished, the gap between `MulticallAt` updates was ≈ pass duration ≈ 17s. Setting `max_multicall_age_sec = 15` made the health gate fire on every other observation.

**Fix:** Split into two independent goroutines started from `Bot.Run()`:

1. **`watchNewBlocks`** keeps:
   - `BatchUpdatePools` → updates `MulticallAt`
   - `FetchV4PoolStates`
   - V3 pool state count log
   - TVL recomputation (V2 + V3)
   - Graph weight refresh
   - Total wall-clock per pass: ~2-3s

2. **`watchTickMapsLoop`** (new) runs:
   - V3-cycle-pool tick fetching via `FetchTickMaps` → updates `TickDataAt`
   - Tick coverage stats (`TickedPoolsHave/Total`)
   - Per-pool tick freshness JSON (for dashboard)
   - Total wall-clock per pass: ~5-10s, independent of multicall

Both run on their own 5-second tickers. Tick fetcher's slowness no longer affects multicall freshness.

**Validation 2026-04-11:**

| Metric | Before | After |
|---|---|---|
| `health: multicall stale` rejects (last hour) | 22 | **0** |
| `multicall_age_secs` (typical) | 16-19s | **2-3s** |
| `tick_data_age_secs` (typical) | 16-19s | **2-3s** |
| `health-reject` ratio | 15.1% | **0.0%** |

**Notes for future work:**
- The cycle cache rebuild itself is intrinsically slow at this scale (~43s for 98k cycles). Both before and after this fix, `cycle_rebuild_age_secs` can fluctuate up to ~45s. The plan's `cycle_rebuild_age_secs ≤ 30s` check is too tight given the current cycle cache size — bump it to ~60-90s, OR optimize the rebuild (next-step opportunity).
- The current 5s ticker means `FetchTickMaps` will be called every 5s even though each call takes 5-10s. The Go time.Ticker drops missed ticks (channel buffer of 1), so successive calls back-to-back are fine, but we're not getting the 5s cadence we wanted. Practical effect: tick map data refreshes about every 5-10s, which is well under the 30s `tick_pool_max_age_sec` threshold.
- If the tick fetcher ever becomes too slow, increase its ticker interval (currently hardcoded to 5s in `watchTickMapsLoop`) instead of speeding up `FetchTickMaps` itself.
