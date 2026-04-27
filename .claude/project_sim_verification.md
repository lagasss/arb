---
name: Simulator verification status
description: Per-DEX sim accuracy status as of 2026-04-12, covering all active DEX types with smoketest results and known gaps
type: project
originSessionId: 2d6b593d-b638-4f91-a725-06bb07a3c542
---
# Simulator verification — 2026-04-12

## Verified (PASS at 0 bps or state-match)

| DEX | Method | Result |
|-----|--------|--------|
| UniswapV2 | QuoterV2 vs getAmountsOut | 0 bps |
| SushiSwap V2 | QuoterV2 vs getAmountsOut | 0 bps |
| SushiSwap V3 | QuoterV2 (0x0524E833…) | 0 bps |
| PancakeSwap V3 | QuoterV2 (0xB048Bbc1…) | 0 bps |
| UniswapV3 | QuoterV2 (0x61fFE014…) | 0 bps |
| CamelotV3 (Algebra) | Direct globalState() fee+sqrtP match | 0 delta |
| RamsesV3 | Direct slot0() state match | 0 delta |
| UniswapV4 | StateView.getSlot0 (sel 0xc815641c) | 0 delta |
| Camelot V2 volatile | getAmountsOut via Camelot router | 0 bps (after 10× fee-scale fix) |
| Curve StableSwap | Direct pool.get_dy() | 0 bps |
| Balancer Weighted | Spot-rate sanity (0.25–0.50% from fee) | PASS |
| DeltaSwap | getAmountsOut | 0 bps |
| Swapr | getAmountsOut | 0 bps |
| ArbSwap | getAmountsOut | 0 bps |

## Known issues

| DEX | Issue | Severity |
|-----|-------|----------|
| Camelot V2 stable (USDT/USDC.e) | Solidly math implemented but reserves stale for this pool — sim returns ~1:1 while chain returns 1.12:1 | Low (1 pool, 50 cycles) |
| PancakeV3 WBTC/cbBTC | Stale tick data — sim over-estimates by 40% | Low (1 specific pool) |
| TraderJoe | Uses Liquidity Book, NOT x*y=k — sim uses wrong math entirely | Disabled in config (0 TVL pools, 0 cycles) |

## Bugs fixed during verification

1. **Camelot V2 directional fee 10× scale** — calibration wrote 10,000-scale values but CamelotSim uses 100,000-denominator. Fixed in calibrateCamelotDirectionalFee (multicall.go).
2. **Camelot V2 FeeBps contamination** — max-propagation never lowered FeeBps once contaminated. Changed to REPLACE semantics.
3. **Camelot stable FeeBps reset** — stable pools detected during calibration now get FeeBps reset to 4 bps (was carrying 994 from contamination).
4. **Missing Solidly StableSwap sim** — CamelotSim.IsStable routed to CurveSim (wrong invariant). Added `solidlyStableAmountOut` implementing `x³y + y³x = k`.
5. **Dead factory addresses** — RamsesV3 (0xaaa16c… = 0 bytecode), CamelotV3 (0x6dd3fb… = 0 bytecode). Updated to live addresses.
6. **PancakeV3/SushiV3/RamsesV3 quoter addresses** — were missing from smoketest, silently producing meaningless PASS via wrong Uniswap quoter. Now wired with verified addresses.
7. **UniV4 StateView selector** — was using wrong selector 0xd13f1424, correct is 0xc815641c.
