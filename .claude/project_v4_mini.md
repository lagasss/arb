---
name: V4Mini executor — deployed 0x615ba27d
description: V4-only executor deployed 2026-04-19 at 0x615ba27dad033bc8d39db8a5548b77863d674060, ~300k gas 2-5 hops, native-ETH-aware, hook-whitelist gated; routing live via qualifyForV4Mini
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Closes the V4_HANDLER reject class on V4-ETH and active-hook pools via a dedicated V4-only executor.

## Status: LIVE
- Address: `0x615ba27dad033bc8d39db8a5548b77863d674060`
- contract_ledger status: `active`, gas_estimate=300000
- Deployed 2026-04-19

## Contract — `contracts/V4Mini.sol`
- Single `PoolManager.unlock` for the whole 2-5 hop V4 cycle (vs one unlock per hop in the generic executor) — saves ~70k gas per extra V4 hop.
- Native-ETH-aware: `_settleCurrency` branches on `currency == 0x0` and uses `pm.settle{value:}` for ETH-in legs. Eliminates the `IERC20(0x0).transfer` revert that affected every V4-ETH cycle through the generic path.
- Hook gate: `allowedHooks` mapping (owner-toggleable). Address(0) is implicit-allowed; everything else must be explicitly whitelisted via `setHookAllowed(addr, true)`. Shares `HookRegistry` 0x1249139d with MixedV3V4Executor.
- Flash via V3 pool's `flash()` (V4 has no native flash). Same callback shape as V3FlashMini.
- Owner-only entry, transient storage for hop blob, packed-bytes calldata (67 bytes/hop: c0+c1+flags+fee+tickSpacing+hooks).
- Measured/target gas: 220k (2-hop), 280k (3-hop), 340k (4-hop), 395k (5-hop) vs 700k-1.2M generic.

## Routing wiring
- `internal/executor.go`: `cycleIsV4MiniShape`, `qualifyForV4Mini`, `packV4MiniHops`, `executeV4MiniABI`, `SetV4MiniAddress`/`V4MiniAddress`.
- `internal/bot.go`: `evalOneCandidate` sets `useV4Mini = !useV3Mini && qualifyForV4Mini(...)`; gas estimate 300k for V4Mini route.
- `cmd/smoketest/main.go`: `CONTRACT-V4Mini-UniV4` probe runs when `ExecutorV4Mini` non-empty.

## Don'ts
- Don't enable a V4 cycle through the generic executor — still reverts on V4-ETH and active-hook pools. V4Mini qualifier auto-picks when shape matches; mixed-DEX cycles go to MixedV3V4Executor instead.
- Don't whitelist hooks blindly. Hooks can rewrite swap delta accounting; an unverified hook could drain the executor on output. HookRegistry classifier auto-whitelists safe ones; `/hook approve` for manual review.
