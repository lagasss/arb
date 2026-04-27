---
name: Incremental tick-liquidity delta from Mint/Burn/ModifyLiquidity
description: 2026-04-19 SIM_PHANTOM class fix — apply V3 Mint/Burn and V4 ModifyLiquidity liquidityDelta incrementally to Pool.Ticks + Pool.Liquidity without waiting for the next FetchTickMaps refetch
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Landed 2026-04-19. Eliminates the SIM_PHANTOM class on tight-margin / ts=1 pools where `sim_bps == fresh_bps` but `eth_call` reverts. Root cause: our cached per-tick `LiquidityNet` was up to ~1 block stale after a Mint/Burn event because the reconciliation path was a full `FetchTickMaps` RPC (100-250 ms).

## What changed

1. **`Pool.ApplyTickLiquidityDelta(tickLower, tickUpper, liquidityDelta, blockNumber)`** — new method at `pool.go` that:
   - Adds `+delta` to `liquidityNet[tickLower]` and `-delta` to `liquidityNet[tickUpper]`.
   - Removes entries whose net becomes exactly zero (matches UniV3's bitmap-clear semantics).
   - Inserts tickLower / tickUpper into the sorted `Ticks` slice if not present (binary-search + single `copy`).
   - If `p.Tick ∈ [tickLower, tickUpper)`, adjusts active `p.Liquidity` by the same delta.
   - Preserves the "slice is always replaced, never mutated" invariant by building a fresh slice.
   - Increments `p.TickDeltaApplied` counter for observability.

2. **Mint/Burn decoders in `swaplisten.go`** — two helpers decode the indexed topics (tickLower, tickUpper as int24 in the last 3 bytes of topic slots) and the non-indexed `amount`:
   - `applyV3LiquidityEvent(p, topics, data, isMint, blockNumber)` — Mint → +amount; Burn → -amount.
   - `applyV4ModifyLiquidity(p, data, blockNumber)` — signed int256 liquidityDelta from non-indexed payload.
   - `sign24FromHash` / `sign24FromBytes32` helpers extract int24 from 32-byte slots with sign extension from bit 23.

3. **Listener wiring** — both Mint/Burn and ModifyLiquidity event branches now apply the delta BEFORE firing the safety-net `fireTickRefetchImmediate`. If the decoder misses an event (e.g. bot restart), the next `FetchTickMaps` run reconciles from chain truth.

4. **Counter on `/debug/pools`** — `tick_delta_applied: uint32` per pool in the JSON response. Dashboard + ticktest can watch for mint-burn churn.

## Observability

- `GET /debug/pools?limit=5000` → filter by `tick_delta_applied > 0`. In steady state, active 1bp pools (WETH/USDC 0.01%, WETH/USDT 0.01%) should show 1-20 deltas/min.
- Zero growth across all pools with the rest of the system healthy = decoder or subscription regression.
- A pool with `tick_delta_applied > 0` and `ticks_count = 0` is normal when an LP burned-then-remminted on the same range (net delta = 0 at each tick → our code correctly removes the entries).

## Don'ts

- Don't replace the `fireTickRefetchImmediate` safety net — the periodic refetch is the self-healing mechanism for any decoder drift, missed event, or bot-restart gap.
- Don't mutate `p.Ticks` in place. `ApplyTickLiquidityDelta` builds a fresh slice; Snapshot() depends on the "always-replaced" invariant.
- Don't apply deltas without the slash — Mint uses `+amount` at the lower edge and `-amount` at the upper edge (UniV3's liquidityNet is a signed delta that activates at lower, deactivates at upper). The same math with a negative delta handles Burn.

## Test plan follow-up

- New ticktest category `delta-consistency` that samples a pool with `tick_delta_applied > 0`, reads its Ticks slice, compares against an on-chain `ticks(tickLower)` + `ticks(tickUpper)` call for those specific ticks. Should match within 1 block lag.
- SIM_PHANTOM class should drop to near-zero for ts=1 pools after this lands. Monitor `[validate] ... SIM_PHANTOM` lines in bot.log.
