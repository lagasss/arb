---
name: Major session 2026-04-12 — quality, speed, math, flash sources
description: Consolidated list of all fixes and features deployed on 2026-04-12: pool quality filter, speed optimizations, simulator fixes, multi-source flash loans, test infrastructure
type: project
originSessionId: 2d6b593d-b638-4f91-a725-06bb07a3c542
---
# Session 2026-04-12 — all changes

## 1. Pool quality filter (scam/honeypot defense)

- `absolute_min_tvl_usd: 5000` — hard floor in ForceAdd, Add, cyclecache rebuild, prune
- `min_tick_count: 30` — V3/V4 pools below 30 initialized ticks excluded
- `high_fee_tier_min_tvl_usd: 50000` — 1% fee tier pools need ≥$50k TVL
- `min_volume_tvl_ratio: 0.001` — dead pools (volume < 0.1% of TVL) excluded
- Auto-disable: pools with ≥5 sim-rejects/hour get `Disabled=true` persisted to DB
- `poolQualityReason()` in graph.go — single function called from all 3 enforcement points
- Motivated by Unishop.ai ($580 TVL pool producing "hop 0 simulation returned 0" rejects)

## 2. Data freshness optimizations (from previous session, carried over)

- Block-driven multicall via WSS new-head subscription (was 5s ticker)
- Eager tick re-fetch on tick-crossing swap events (`TickRefetchCh`)
- Algebra dynamic fee refresh on each CamelotV3/ZyberV3 swap (`AlgebraFeeRefreshCh`)
- Config: `tick_eager_refetch_spacings: 8`, `tick_eager_refetch_cooldown_ms: 750`

## 3. Pool snapshot fix

- `Pool.Snapshot()` deep-copies mutable state under `p.mu.RLock()`
- `Cycle.WithSnapshots()` builds frozen Cycle for evalOneCandidate
- Eliminates "build hops: cycle unprofitable" divergence between optimalAmountIn and buildHops
- Validated: 11 → 0 cycle-unprofitable rejects in controlled A/B test

## 4. Speed optimizations

- `skip_sim_above_bps: 100` / `skip_sim_above_usd: 1.0` — skips 39ms eth_call for confident cycles
- Vault balance cache TTL 30s (was per-block invalidation) — eliminates 28ms cold-miss
- Vault balance pre-warming for all borrowable tokens at startup (`WarmVaultBalances`)
- `tokenAmountFromUSDWithPrice` — captures USD price once in optimalAmountIn for consistency
- Net hot-path: ~20ms for confident cycles (was 67ms+ before)

## 5. Simulator fixes

- **Camelot V2 directional fee 10× scale** — calibration used ×10000 but sim uses ÷100000. Fixed to ×100000.
- **Camelot V2 FeeBps contamination** — changed max-propagation to REPLACE semantics
- **Camelot stable FeeBps reset** — stable pools get FeeBps=4 during calibration (was carrying 994)
- **Solidly StableSwap simulator** — new `solidlyStableAmountOut()` implementing x³y + y³x = k for Camelot/Ramses stable pairs (was using wrong Curve invariant)
- **TraderJoe disabled** — uses Liquidity Book (not x*y=k); disabled in config + discovery until LB sim is implemented

## 6. Dead factory addresses fixed

- RamsesV3: `0xaaa16c…` (0 bytecode) → `0xd0019e86…` (3423 bytes) from docs.ramses.exchange
- CamelotV3: `0x6dd3fb…` (0 bytecode, was AlgebraPoolDeployer) → `0x1a3c9B1d…` (14031 bytes, AlgebraFactory) from docs.camelot.exchange

## 7. Quoter addresses verified and wired

- PancakeV3 QuoterV2: `0xB048Bbc1Ee6b733FFfCFb9e9CeF7375518e25997`
- SushiV3 QuoterV2: `0x0524E833cCD057e4d7A296e3aaAb9f7675964Ce1`
- CamelotV3: direct `globalState()` verification (factory mismatch prevents quoter use)
- RamsesV3: direct `slot0()` verification (same reason)
- UniV4 StateView: `0x76fd297e…` selector `0xc815641c` for `getSlot0(bytes32)`
- Curve: direct `get_dy(int128,int128,uint256)` on pool
- Balancer: spot-rate sanity check from reserves+weights

## 8. Multicall decoder locking

- `p.mu.Lock()` around decoder calls in BatchUpdatePoolsAtBlock parse loop (main + fallback)
- Prevents torn reads where simulator sees e.g. fresh SqrtPriceX96 paired with stale Liquidity

## 9. Multi-source flash loans

- See [project_flash_sources.md](project_flash_sources.md) for full details
- Contract `0x73aF65fe487AC4D8b2fb7DC420c15913E652e9Aa` with 3 entry points
- FlashSourceSelector with Balancer > V3 > Aave priority
- Fee-adjusted profit math in evalOneCandidate
- Aave Pool address: `0x794a61358D6845594F94dc1DB02A252b5b4814aD`

## 10. Test infrastructure

- **T11 `invariants` category** — 20 cross-field SQL checks on arb_observations/our_trades
- **POOL-14/15/16/17/18** — pool quality filter verification tests
- **SIM-09/10/11/12** — snapshot correctness, optimalAmountIn consistency, boundary, metamorphic
- **ROUTER-12** — quoter coverage gap detector (fails when DEX in registry has no quoter)
- All 12 active DEX types now have smoketest probes (sim + contract categories)
- Smoketest score: 32 PASS sim, 7 PASS contract

## Current contract history

1. `0x0F2dACc1…` — original, PancakeV3 via broken UniV3 layout
2. `0x56483EEE…` — fixed PancakeV3 _swapPancakeV3 handler, no deadline
3. `0x6D808C46…` — fixed CamelotV2 swapExactTokensForTokensSupportingFeeOnTransferTokens
4. `0x73aF65fe…` — **current** — added executeV3Flash + executeAaveFlash + setAavePool
