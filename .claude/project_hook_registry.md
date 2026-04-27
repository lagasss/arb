---
name: V4 Hook Registry + classifier
description: 2026-04-19 shipped — on-chain HookRegistry (0x1249139d...) shared by V4Mini + MixedV3V4Executor; off-chain classifier auto-whitelists safe hooks (no *ReturnDelta bits); shape filter pre-rejects unclassified hooks at scoring time
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Long-term fix for the V4 hook-whitelist drift problem (ticktest `/hooks` was flagging 39 non-zero-hook V4 pools with empty whitelist pre-2026-04-19).

## Architecture

Three layers, each an independent circuit breaker:

1. **Off-chain classifier** — `internal/hookclass.go` reads the bottom 14 bits of each hook address (V4 encodes permissions there, no bytecode inspection needed) + fetches runtime bytecode for a hash. Classifications:
   - `safe` — no `*ReturnDelta` bits → auto-whitelist on-chain
   - `fee_only` — `beforeSwapReturnDelta` only, no `afterSwapReturnDelta` → likely dynamic-fee hook, manual review required
   - `delta_rewriting` — any `afterSwapReturnDelta` bit → explicit `approveDeltaHook` only
   - `unknown` — RPC/bytecode failure; retried every pass

2. **Off-chain sync loop** — `internal/hooksync.go`. Runs every `hook_sync_interval_sec` (config, default 300s = 5 min). Walks V4 pools in registry, classifies every new non-zero hook, persists to `hook_registry` SQLite table, and calls `HookRegistry.setHook(hook, true, perms, codeHash, label)` on-chain for safe hooks. 15s warmup ticker triggers first pass as soon as pool registry is populated.

3. **Pre-scoring shape filter** — `cycleIsV4MiniShape` + `cycleIsMixedV3V4Shape` in `internal/executor.go` now consult `globalHookGate.IsAllowed(hook)`. Unclassified / fee_only / delta_rewriting hooks fail the shape check → cycle never even hits the simulator. Catches drift before RPC is burned.

## On-chain contracts (deployed 2026-04-19)

- `HookRegistry`: `0x1249139d6aecae6147c159d540202412c4798b71` — single source of truth. `isAllowed(hook) view returns(bool)` is the gate function. Owner-only `setHook`/`approveDeltaHook` setters.
- `V4Mini` (v2): `0x15b8a1360c3cb764e7b7dfc0d00db171659dd7e0` — supersedes `0x615ba27D…`. Adds `setHookRegistry`; `allowedHooks` mapping kept as fallback only.
- `MixedV3V4Executor` (v2): `0x8fe29c50f72c580643342122b43697e6747887a6` — supersedes `0x448494C9…`.

Old addresses are `status='deprecated'` in `contract_ledger`. Both new executors have been wired to the HookRegistry via `setHookRegistry(0x1249139d…)`.

## Permission bit encoding (V4 spec)

Bottom 14 bits of the hook address:
```
bit 13: beforeInitialize
bit 12: afterInitialize
bit 11: beforeAddLiquidity
bit 10: afterAddLiquidity
bit  9: beforeRemoveLiquidity
bit  8: afterRemoveLiquidity
bit  7: beforeSwap
bit  6: afterSwap
bit  5: beforeDonate
bit  4: afterDonate
bit  3: beforeSwapReturnDelta         ← delta_rewriting
bit  2: afterSwapReturnDelta          ← delta_rewriting (most dangerous)
bit  1: afterAddLiquidityReturnDelta  ← delta_rewriting
bit  0: afterRemoveLiquidityReturnDelta
```

`HasDeltaFlag = bit 3 OR bit 2 OR bit 1 OR bit 0`. Hooks with no delta bits cannot modify swap accounting, only observe — safe to auto-whitelist.

## Initial classification results (2026-04-19)

39 non-zero hook addresses scanned from live UniV4 pool registry:
- 8 safe (auto-whitelisted on-chain)
- 24 fee_only (pending manual review)
- 4 delta_rewriting (pending explicit approveDeltaHook)
- 3 unknown (retry next pass)

Impact: 8 hook → ~12 V4 pool addresses unblocked for V4Mini + MixedV3V4Executor routing.

## Manual approval flow

Use `/hook approve <hook_addr> <reviewer_note>` slash command (`/.claude/commands/hook.md`):
1. Mandatory: `cast code <hook>` + read source (Arbiscan if verified) before approving. The slash command will NOT broadcast without the user's explicit "go".
2. Two-step: read bytecode_hash + permission_bits from `hook_registry` SQLite, then `cast send HookRegistry approveDeltaHook(…)`.
3. After tx confirms, `UPDATE hook_registry SET on_chain_status='manual'` so the sync loop doesn't overwrite.

## Config

`config.yaml`:
```yaml
hook_registry: "0x1249139d6aecae6147c159d540202412c4798b71"
hook_sync_interval_sec: 300
```

Setting `hook_registry: ""` falls back to per-executor `allowedHooks` maps (local mode). `hook_sync_interval_sec: 0` disables the classifier entirely.

## Debug + tests

- `/debug/hook-registry` — JSON snapshot of classifier cache with permission_bits, classification, bytecode_hash, reason.
- `./ticktest -cat hooks` — cross-checks live V4 pools against `HookRegistry.isAllowed`. Updated to query the registry (not per-executor `allowedHooks`) when `trading.hook_registry` is set.
- `hook_registry` SQLite table — canonical classification record; survives bot restarts.

## Known follow-up work

- None of the fee_only hooks have been audited yet. Safe default: the shape filter rejects them at scoring time, so no risk to the bot beyond "miss some cycles."
- If we ever want to manually approve a delta_rewriting hook, always read the hook's source first — delta-rewriting is the vector that can actually route flash-borrowed funds out of our unlock callback.
