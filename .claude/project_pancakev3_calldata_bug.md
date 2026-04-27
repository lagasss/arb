---
name: PancakeV3 calldata bug — fixed by redeploying ArbitrageExecutor
description: Original ArbitrageExecutor 0x0F2dACc1... routed PancakeV3 hops via UniV3 calldata (with `deadline`) which Pancake doesn't accept; fixed in 0x56483EEE… by adding _swapPancakeV3 with the no-deadline layout
type: project
---

PancakeV3 forked Uniswap V3 but **removed the `deadline` field** from `ExactInputSingleParams`. The function selector therefore differs (`0x04e45aaf` vs UniV3's `0x414bf389`), so the original `ArbitrageExecutor` calling Pancake's router with the UniV3 layout failed every Pancake swap silently in the contract's `try`/`catch`. This was the dominant cause of the 0-trade situation on 2026-04-11: 124 of 139 (89%) hop-0 sim-rejects were on PancakeV3 hops, and PancakeV3 covers ~30% of usable on-Arbitrum cycle paths.

**Resolution:** A patched contract was deployed on 2026-04-11 17:39 UTC at `0x56483EEEe23A8C09f78Ae44DCdeaaCa8ffD995f6` (deploy tx `0xec63c51e6f9ac6ccada771d5405b38f6325aaa6b2354202c41071cfb1d5b9e04`, block 451445210). It adds `_swapPancakeV3` using `IPancakeV3Router` (no-deadline struct) wired into `_executeHops` via the new `DEX_PANCAKE_V3 = 8` constant. `config.yaml` was updated with the new address and `trading.executor_supports_pancake_v3: true`, and the bot was restarted to pick up the change.

**Why:** The flag exists because the dispatch must change in lock-step with the on-chain bytecode. The old contract has no DEX_PANCAKE_V3 case so type 8 falls into its `_swapV2` `else` branch, which would silently misroute every Pancake hop into the UniV2 router and break the cycles that currently work. Always pair flag flips with contract address changes.

**How to apply:** If anything in `_swapPancakeV3` ever needs adjusting, the redeploy sequence is: forge create from `/home/arbitrator/go/arb-bot/contracts`, update `trading.executor_contract`, keep `executor_supports_pancake_v3: true`, restart bot capturing `ARB_BOT_PRIVKEY` from `/proc/<pid>/environ`. Don't deploy and forget the flag — and don't flip the flag without redeploying.
