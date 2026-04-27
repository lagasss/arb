---
name: MixedV3V4Executor — V3+V4 mixed-cycle executor
description: 2026-04-19 deployed at 0x448494C9F384CCBF56F39c5BcA963c513c3204e6 — closes the V4_HANDLER reject class on mixed V3+V4 cycles like UniV4-UniV4-UniV3 that V4Mini's all-V4 gate cannot take
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Landed 2026-04-19. Third executor in the lean-fleet strategy (after V3FlashMini and V4Mini). Closes the V4_HANDLER reject class on **mixed** V3+V4 cycles.

## Naming convention

`MixedXyzExecutor` where `Xyz` is the alphabetised set of DEX families it dispatches across. Future planned variants:
- `MixedV2V3Executor` — V2 forks + V3-family in one tx (no V4)
- `MixedV3V4CurveExecutor` — adds Curve to the V3+V4 set
- etc.

Each variant is its own narrow contract; the routing layer picks the cheapest qualifying executor at scoring time.

## What MixedV3V4Executor handles

Cycles where **every hop** is one of:
- UniV4
- V3-family: UniV3, SushiV3, RamsesV3, PancakeV3, CamelotV3 (Algebra), ZyberV3 (Algebra)

AND has at least one V4 hop AND at least one V3 hop (mixed). Pure-V3 cycles still go to V3FlashMini, pure-V4 still go to V4Mini.

Hop count: 2-5. Flash source: V3 pool flash only (matches V3FlashMini and V4Mini).

## How it closes V4_HANDLER

Wraps the entire cycle in **ONE** `PoolManager.unlock()` callback:
- V4 hops use `pm.swap` + per-hop `_settleCurrency` + `pm.take` (same V4 path as V4Mini, including native-ETH branch and hooks gate)
- V3 hops use direct `pool.swap()` calls — the V3 swap callback (`uniswapV3SwapCallback` / `pancakeV3SwapCallback` / `algebraSwapCallback`) runs INSIDE the V4 unlock and transfers the input token; control returns to the unlock loop and the next hop runs

V4 deltas net to zero per V4 hop, so the unlock's net-zero invariant holds at exit. V3 hops touch tokens that aren't in V4's accounting.

## Calldata layout

67 bytes per hop. Flag byte at offset 40:
- bit 0: zeroForOne
- bit 7: isV4 (1 = V4 hop with PoolKey payload at [41:67]; 0 = V3 hop, [41:67] is zero-padding; pool address goes in [0:20], tokenOut in [20:40])

V4 hop payload at [41:67]: `fee uint24 BE | tickSpacing int24 BE | hooks address`.

## Routing precedence (in evalOneCandidate)

1. `useV3Mini` — pure V3-family, 2-5 hops
2. `useV4Mini` — pure V4, 2-5 hops
3. `useMixedV3V4` — mixed V3+V4, 2-5 hops
4. fallthrough — generic ArbitrageExecutor

All three minis require `FlashV3Pool` flash source. Cycles with Balancer/Aave flash always go to the generic executor.

## Gas envelope

- 2-hop mixed: target 350k
- 3-hop mixed: target 430k
- 4-hop mixed: target 510k
- 5-hop mixed: target 590k

Compared to ~880k-1.2M for the generic executor on the same shape.

## Deploy facts

- Contract: `contracts/MixedV3V4Executor.sol`
- Address: `0x448494C9F384CCBF56F39c5BcA963c513c3204e6`
- Deployer/owner: `0x612fB8Be…` (matches wallet)
- PoolManager: `0x360E68fa…`
- `allowedHooks` whitelist starts EMPTY (zero-hooks pools auto-allowed via the `hooks == address(0)` shortcut)
- Deploy script: `contracts/script/DeployMixedV3V4Executor.s.sol`
- Foundry config: needs `via_ir = true` (set in `contracts/foundry.toml` since the V4Mini deploy)

## Don'ts

- Don't whitelist hooks blindly. Hooks can rewrite swap delta accounting.
- Don't widen the qualification gate to include V2/Curve — those need their own dispatch dispatchers and would require the contract to also handle V2/Curve callbacks. Build a separate `MixedV2V3Executor` or `MixedV3V4CurveExecutor` instead.
- Don't merge MixedV3V4Executor's logic into the generic executor as a "fix." The whole point of the lean-fleet strategy is per-cycle-shape gas efficiency. The generic executor stays as the catch-all for cycles outside the mini fleet's scope.

## Live status

Bot PID 702499 running with all three minis enabled. Routing automatically picks the best contract per cycle shape; no manual intervention needed.

When `pause_trading: false`, the next mixed V3+V4 candidate (cycles like #33591 shape: UniV4→UniV4→UniV3) will route to MixedV3V4Executor instead of the generic — and the V4_HANDLER reverts on that class should disappear.
