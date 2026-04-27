---
name: V4 native-ETH executor blocker
description: Phase B/C/D of the V4 native-ETH refactor must land before pause_trading=false; current state would silently revert every V4-ETH swap on chain
type: project
originSessionId: 3815f914-2e9b-4525-9ac4-3f915b6c5a87
---
74 V4 pools on Arbitrum use native ETH (currency0 = `0x0000000000000000000000000000000000000000`) on chain. Phase A landed 2026-04-18: TokenRegistry.Get aliases 0x0000 → WETH metadata, arbscan stopped substituting WETH for ETH at ingest, 21 wrong-order DB rows backfilled. Sim now correct for these pools. **Executor is not.**

The unfinished phases that MUST ship before going live:

- **Phase B** (~2–3 h): cycle graph wrap/unwrap. Verify nothing builds nonsensical paths through V4-ETH pools and that gas estimates account for the wrap step.
- **Phase C** (~1–2 d, contract redeploy): add `V4NativeETHSlot int8` field on `Pool`; populate at LoadV4PoolsFull from the raw r.Token0/r.Token1 BEFORE the registry alias normalises them; executor reads this flag when building the V4 `PoolKey` and uses `0x0000…` not WETH; inject WETH↔ETH wrap/unwrap at cycle boundaries (other DEXes only know WETH ERC20). Likely needs new ArbitrageExecutor.sol with native-ETH receive/wrap helpers + new deployment + contract_ledger update.
  - **Gas-adjustment requirement (DO NOT SHIP C WITHOUT)**: extend the per-route gas estimate in evalOneCandidate. Each V4-ETH hop in the cycle adds ~60K gas (WETH.deposit ~25-30K + WETH.withdraw ~30-35K + native settle in unlockCallback). Compute `effectiveGasEst = baseRouteGas + v4EthHopCount × 60_000` before the profit gate. Without this, breakeven cycles (gas_safety_mult=1) gated against the static 380K/900K will be ~$0.003 short of true breakeven on Arbitrum at current prices and bleed money on every marginal V4-ETH trade. The 60K/hop number is an estimate — measure it from real on-chain executions once Phase C runs and feed back via the rolling-average gas-tracking idea (see critical-issues #5 in MEMORY).
- **Phase D** (~30 min, cosmetic): dashboard alias ETH/WETH for display.

**Why:** without Phase C the executor builds `PoolKey{ currency0=WETH, currency1=X, ... }` and computes a keccak256 different from the chain's actual `pool_id` (computed with currency0=ETH=0x0000). Every V4-ETH swap reverts silently. While `pause_trading: true` this is harmless; the moment trading goes live, every cycle through one of the 74 pools loses gas to a guaranteed revert.

**How to apply:** treat as a hard blocker on flipping `pause_trading: false` in config.yaml. Before going live, complete Phase B + C + D, then validate end-to-end with smoketest dry-runs of V4-ETH cycles against a fork. The Phase A registry alias makes the bug invisible in observation logs (sim is honest) — only on-chain execution would reveal it, and by then real funds are at stake.
