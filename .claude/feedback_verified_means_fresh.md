---
name: Verified ≠ once — means fresh RIGHT NOW
description: Extends the "verified only" rule — `verified=true` alone is insufficient, every simulator-dependent field must also be currently fresh at scoring time
type: feedback
originSessionId: 3815f914-2e9b-4525-9ac4-3f915b6c5a87
---
**Rule**: a pool being `Verified=true` is NECESSARY but NOT SUFFICIENT. Every simulator-dependent field (fee, tickSpacing, slot0, liquidity, tick bitmap, liquidityNet per tick, reserves, decimals) must also be **currently fresh** at the moment a cycle enters scoring — not stale from an earlier verification pass.

**Why**: sets the invariant that prevents the recurring "sim accurate / eth_call reverts at 99% slippage" regression (seen 2026-04-17: 138/138 arb_observations failed with "hop N: swap reverted"). Verification timestamps decay; a pool verified at T0 with liquidity=X may have liquidity=Y at T1 with different tick-ranges active. If scoring uses the T0 snapshot, sim says "+$0.50 profit" but the on-chain state requires min_out that the cached data can't produce. Root cause is gate-leakage: `verified` flag is persistent, freshness is not re-checked at scoring time for every required field.

**How to apply**:
- When Seb says "new opportunities keep appearing that shouldn't", DON'T investigate specific handler bugs first — audit the gate pipeline. The question is always: "which simulator-dependent field for which pool/token wasn't fresh at the moment this cycle got scored?"
- Every cycle-entry gate (swap-event, cross-pool, block-scoring, inline refresh — the 4 triggers per reference_cycle_detection.md) must independently check freshness of every required field, not just `Verified=true`.
- Acceptable staleness is per-field: fee (hours — rarely changes), tickSpacing (never), slot0 (seconds — changes every block), tick bitmap (seconds — same), liquidityNet (seconds), reserves (seconds), decimals (never). The tight ones (slot0, bitmap, liquidity, reserves) dominate.
- If ANY required field can't be confirmed fresh for a cycle, skip the cycle — never fall back to stale data "to keep things moving". That's exactly how phantom cycles appear.
- When a 100% failure rate appears in arb_observations, default diagnosis path: (1) confirm every failing pool has current-block data cached, (2) if not, find which trigger path let it through without a freshness check. Contract bugs are a secondary hypothesis; gate leakage is primary.
