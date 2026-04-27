---
name: Fee extraction by DEX type
description: Exactly where each DEX's fee comes from, when it can drift, and how we detect errors
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Fees are the #1 silent source of sim drift. Wrong fee by 5 bps → wrong output → failed eth_call. For every trade, we need the EXACT fee the pool uses at the current block.

### Fee storage on `Pool` struct
- `FeeBps uint32` — basis points (30 = 0.30%)
- `FeePPM uint32` — parts-per-million (100 = 0.01%, 3000 = 0.30%, 10000 = 1.00%)
- **Precedence**: `FeePPM` overrides `FeeBps` for sub-1bps pools (e.g., RamsesV3 `fee=1` = 0.0001%)
- `Token0FeeBps` / `Token1FeeBps` — directional fees (Camelot V2 only; derived from `getAmountOut` calibration per direction)
- `IsStable bool` — Camelot V2 stable pool flag from `stableSwap()` call

### Fee source per DEX

| DEX | Source | RPC call | Stable? | Drift risk |
|---|---|---|---|---|
| **UniV3** | `pool.fee()` | static uint24 | ✅ yes — fixed at deploy | None |
| **SushiV3** | `pool.fee()` | static uint24 | ✅ | None |
| **PancakeV3** | `pool.fee()` | static uint24 | ✅ | None (but slot0 ABI differs — uses `pancakeV3Slot0ABI`) |
| **RamsesV3** | `pool.fee()` | static uint24 | ✅ (CL fee tier) | None |
| **CamelotV3 / ZyberV3** | `globalState()` returns dynamic `fee` field | uint16 | ❌ **DYNAMIC** — changes per block based on volatility | High — must re-read on every swap |
| **UniV4** | `StateView.getSlot0` returns `lpFee` + `protocolFee` | uint24 + uint24 | Usually static but hooks can modify | Medium — hooks |
| **UniV2 / SushiV2 / UniV2 forks** | Calibrated via `getAmountOut(amt, t0)` in `calibrateV2FeeOnChain` | derived | ✅ | Low — drift only if router charges differently than getAmountOut reports |
| **Camelot V2** | `getAmountOut` calibration PER DIRECTION (stores in `Token0FeeBps`, `Token1FeeBps`) | derived | ✅ but directional | Medium — must match swap direction |
| **Curve** | `pool.fee()` in 1e10 scale (`curve_fee_1e10`) | static | ✅ | None |
| **Balancer V2** | `pool.getSwapFeePercentage()` in 1e18 scale | static per pool | ✅ | None |

### Algebra dynamic fee flow (CamelotV3 / ZyberV3)
1. Swap event fires → `SwapListener.applyV3Swap` updates sqrtPriceX96/tick/liquidity
2. Listener also sends pool to `AlgebraFeeRefreshCh` (if cooldown elapsed)
3. `algebraFeeRefreshLoop` drains the channel, batches pools, calls `globalState()` to re-read fee
4. Updates `Pool.FeeBps` in place
5. Next sim uses the fresh fee

**Accuracy hazard**: between swap event and the fee refresh (bounded by `tick_eager_refetch_cooldown_ms`, default 750ms), the sim uses the stale pre-swap fee. If the new swap pushed the pool into a higher-volatility regime, the fee can double. Sim overestimates output until refresh lands.

### V2 fee calibration detail
`calibrateV2FeeOnChain(ctx, client, poolAddr, t0addr)`:
1. `getReserves()` → (r0, r1)
2. `getAmountOut(testAmt=r0/1000, t0) → actualOut`
3. Compute `rawOut = (testAmt * r1) / (r0 + testAmt)` (feeless constant product)
4. `feeBps = (rawOut - actualOut) * 10000 / rawOut`
5. Clamp to [1, 1000] bps — anything outside means pool is non-standard

Stable pools return `rawOut ≤ actualOut` (impossible in xy=k) → returns 1 bps (stable-swap-like signal). Recent fix: Camelot stable pools use `spotRateCurve` not V2 formula (line 252 of pool.go).

### Fee in sim (`Edge.effectiveRate`)
```
fee = feePPM / 1_000_000       (sub-bps aware)
   or feeBps / 10000            (normal path)
   or Token0/1FeeBps / 10000    (Camelot V2 directional)
   or camelot_v3_default_fee_bps / 10000  (Algebra fallback if FeeBps=0)
effectiveRate = rawSpotRate * (1 - fee)
```

### Known fee drift signatures
- **CamelotV3 fee stale**: sim says profitable, eth_call reverts at `hop N: slippage`. Usually within 200-500ms after a swap on that pool (before algebra fee refresh catches up)
- **V2 fork fee wrong**: if router uses different fee than `getAmountOut`, sim drifts. Solidly stable pools were this until we added `IsStable` path
- **Ramses sub-bps tier ignored**: forgetting to read `FeePPM` and falling back to `FeeBps=0` → sim treats as zero-fee → massive overestimate. `effectiveRate` now checks `FeePPM > 0` first
- **PancakeV3 slot0 ABI mismatch**: reading `pancakeV3Slot0ABI` pool with `uniV3Slot0ABI` decoder puts feeProtocol in the tick field → silently wrong state. Use the dedicated `pancakeV3Slot0ABI` always

### Debug: how to find a fee error
1. Run validated observation's hop: `hop.Pool.FeeBps` / `FeePPM` at eval time
2. Compare to on-chain `pool.fee()` (static DEXes) or `globalState()` (Algebra)
3. If they differ → stale state; find why the refresh path didn't fire
