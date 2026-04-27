---
name: Cycle paths not Bellman-Ford
description: The bot uses pre-cached cycle paths for arb detection, not Bellman-Ford graph traversal
type: feedback
---

The bot does NOT use Bellman-Ford for real-time arb detection. It uses **pre-cached cycle paths** (CycleCache) that are rebuilt periodically. When a swap event arrives, the FastEval function checks only the pre-cached cycles involving the affected pool — not a full graph search.

**Why:** Bellman-Ford is too slow for real-time arb detection on every swap event. The cycle cache pre-computes all viable cycles and evaluates them in microseconds when a pool's price moves.

**How to apply:** When discussing arb detection, refer to "cycle cache" or "cached cycles" not "Bellman-Ford". When a pool is missing from the cycle cache, the bot won't find arbs through it even if the pool is in the registry. Cycle cache is rebuilt every `cycle_rebuild_secs` (config param).
