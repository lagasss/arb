---
name: V3/V4 tick bitmap + liquidity data
description: How tick data is fetched, stored, and used by the simulator, with known accuracy hazards
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
For V3/V4 pools, the multi-tick simulator needs three things: current sqrtPriceX96/tick/liquidity (from slot0/getSlot0), the tick bitmap (which ticks are initialized), and the liquidityNet at each initialized tick. If ANY of these is stale or wrong, the sim drifts from on-chain execution.

### V3 tick fetch (pool contract directly)
- `tickBitmap(int16 wordPos) → uint256` — one bit per tick in a word of 256 ticks
- `ticks(int24 tick) → (liquidityGross, liquidityNet, ...)` — per-tick liquidity delta
- `FetchTickMaps` scans ±`bitmapRadius` words around the current tick (default 25 → ±6400 ticks for spacing=1)
- Compressed tick space: bit position N in word W corresponds to tick `(W*256 + N) * tickSpacing`

### V4 tick fetch (via StateView `0x76fd297e…`)
- `StateView.getTickBitmap(bytes32 poolId, int16 wordPos) → uint256`
- `StateView.getTickLiquidity(bytes32 poolId, int24 tick) → (uint128 liquidityGross, int128 liquidityNet)`
- V4 pools are keyed by poolId (bytes32), not contract address
- Same `FetchTickMaps` function handles both V3 and V4, routing per pool type

### Storage
- In-memory: `Pool.Ticks []TickData{Tick, LiquidityNet}`, `Pool.TicksUpdatedAt time.Time`, `Pool.TickAtFetch int32`
- SQLite: `pool_ticks` (V3) and `v4_pool_ticks` (V4) — for warm-restart persistence only. Live bot uses in-memory exclusively.

### Fetch cadence
- **Periodic sweep**: `watchTickMapsLoop` every 5s for every V3/V4 cycle pool
- **Eager refetch** (`TickRefetchCh`): fires when a swap event moves the tick ≥ `tick_eager_refetch_spacings` (default 8) from `TickAtFetch`. Per-pool cooldown `tick_eager_refetch_cooldown_ms` (default 750ms) prevents thrashing
- **Inline refresh** (`doFastEvalCycles`): targeted `FetchTickMaps` for pools in candidate cycles whose `|curTick − TickAtFetch| ≥ spacing`

### Sim integration
- `UniswapV3Sim.AmountOut` calls `multiTickSwap(sqrtP, liq, curTick, ticks, tickSpacing, ...)`
- If `len(ticks) == 0` OR `tickSpacing == 0`, the sim returns `big.NewInt(0)` (the single-tick fallback was removed — it produced massive overestimates)
- The tick-staleness gate in `evalOneCandidate` rejects cycles where any V3/V4 edge has `len(Ticks) == 0` OR `TicksUpdatedAt` older than `tick_pool_max_age_sec` (default 30s)

### Accuracy hazards (known and must watch)
1. **Bitmap radius too narrow**: if current tick moves outside the ±25 word range, we miss ticks beyond. Eager refetch partially mitigates; large swaps can still blow past
2. **Empty bitmap false positive**: `FetchTickMaps` sets `TicksUpdatedAt=now` even when scan returns 0 ticks (pools with no initialized ticks in range). The zero-tick gate catches this at candidate evaluation, but cycle cache builder may still include the pool if it's in the graph
3. **Liquidity drift during fetch window**: a tick that was crossed between multicall and FetchTickMaps has stale liquidityNet. Sim's multiTickSwap follows the old liquidity path → wrong output
4. **V4 hooks**: V4 pools with non-zero hooks may modify swap behavior (BEFORE_SWAP / AFTER_SWAP / RETURNS_DELTA). The sim does NOT model hooks. V4 pools with trivial hooks (0x0) are safe; exotic-hook pools must be filtered at verify time
5. **V4 pool manager settlement**: V4 swaps inside `unlock()` callbacks settle net positions, not per-swap transfers. This doesn't affect sim math but affects profit measurement in arbscan
6. **Algebra dynamic fees**: `globalState()` returns the dynamic fee. The Algebra-fee-refresh goroutine reads it on every swap event (cooldown-gated), but between refreshes the simulator uses the stale fee

### Verified DEXes (from simulator verification sweeps)
All 12 active V3/V4 DEXes verified at 0 bps drift vs on-chain quoters as of the last verify pass. Verification is per-DEX one-shot at startup in `VerifyPool`; state drift AFTER that is where accuracy fails.
