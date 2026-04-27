---
name: Arbscan process architecture
description: All goroutines running inside cmd/arbscan and what they do
type: project
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Arbscan (`cmd/arbscan`) is the competitor observer. It runs alongside the bot on the same server and writes to the same arb.db.

**Main loop**
- WSS `SubscribeNewHead` → `processBlock` per block. For each block, fetches all logs matching known swap topics (V2/V3/Algebra/Curve/Balancer/DODO/V4) and FlashLoan topics (Aave/Balancer). Groups logs by tx, identifies txs with 2+ swaps = candidate arbitrage.
- Has HTTP polling fallback (250ms ticker) for when WSS silently drops — `watchdog` detects header silence and activates polling.

**Per-tx processing**
- `decodeHops` → parses each swap event into SwapHop with token/pool/amount data
- Pool resolution: `getPool` caches on-chain factory/token/fee lookups per pool. Uses `factoryLabel()` which reads from `internal.Factories` (shared with bot) + `arbscanOnlyLabels` fallback for DEXes the bot can't trade yet (KyberSwap, SolidlyCom CL, etc.)
- Profit calculation: sums net token flows (Transfer events to/from bot contract) priced in USD. For **discontinuous** arbs (hops with token gaps), only counts round-trip tokens (tokens appearing in BOTH inflow and outflow) — ignores one-directional settlement artifacts from V4 PoolManager unlock() patterns
- Arb classification: `isCircular` (start==end) → cyclic. Non-circular with 2+ swaps through bot contract → discontinuous. Split-route detection by `maxSameIn >= 2` in `ClassifyCompetitorArbType` (multiple hops with same input token)

**DB writes**
- `InsertCompetitorArb` → `competitor_arbs` table with tx_hash, block, sender, bot_contract, flash_loan_src, path_str, hop_count, dexes, profit_usd, net_usd, gas_used, hops_json
- Only inserts if `profit_usd >= 0.01` and passes junk-token filter

**Competitor comparison loop**
- `CompetitorCompareLoop` runs every 15s with a 30s settleDelay (gives bot time to log its own decision in arb_observations)
- `classifyCompetitor(txHash, blockNumber, hopsJSON, cycleInMemory)` decides `comparison_result`:
  1. If our_trades has a row within ±5 blocks with ≥50% pool overlap → `executed` or `submitted_failed`
  2. If arb_observations has a row within ±60s with ≥50% pool overlap → `detected_*` based on reject_reason
  3. If any pool isn't in our registry → `missing_pool`
  4. Fallback uses `cycle_in_memory` flag (written by bot's annotator):
     - `cycle_in_memory=1` → `cycle_known_unprofitable`
     - `cycle_in_memory=0` → `cycle_not_cached`
     - `cycle_in_memory=-1` → `not_profitable_for_us`

**V4 pool seeding**
- At startup: loads v4_pools cache from DB, fetches top pools from Uniswap API, backfills fee/tickSpacing/hooks via RPC (last ~1M blocks).
