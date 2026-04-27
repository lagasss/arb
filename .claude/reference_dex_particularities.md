---
name: DEX particularities — math, fees, routers, callbacks
description: Per-DEX quirks that affect sim accuracy and contract dispatch. Any drift here = wrong sim.
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
### UniV2 / SushiV2 / UniV2-forks (ArbSwap, Swapr, SolidLizard, MMFinance, Horiza, etc.)
- **Math**: constant product `x*y = k` with fee applied to input (`amountIn * (10000-feeBps) / 10000`)
- **Fee extraction**: calibrated via `getAmountOut(testAmt, tokenIn)` in `calibrateV2FeeOnChain` — we don't trust factory/pool stored values
- **Router**: `IUniswapV2Router.swapExactTokensForTokens(amountIn, amountOutMin, path, to, deadline)`. Forks without a dedicated router entry in `dexRouter` can't be executed — only observed
- **State**: `Reserve0` / `Reserve1` from `getReserves()`. Event deltas (`UpdateFromV2Swap`) are additive — can accumulate rounding drift
- **Sim**: `UniswapV2Sim.AmountOut`

### Camelot V2
- **Math**: either `x*y=k` (volatile) or StableSwap (if `stableSwap()` returns true, `Pool.IsStable`)
- **Directional fees**: two fees (`token0Fee`, `token1Fee`) depending on swap direction. Token-specific fee BPS stored in `Pool.Token0FeeBps` / `Pool.Token1FeeBps`; falls back to `FeeBps` if unset
- **Router**: deprecated `swapExactTokensForTokens` — must use `swapExactTokensForTokensSupportingFeeOnTransferTokens` (returns void, compute amountOut via balance diff). Contract handles this in `_swapCamelotV2`
- **Referrer param**: Camelot router has extra referrer arg → dedicated `_swapCamelotV2` contract handler (dexType=5)

### UniV3 / SushiV3
- **Math**: concentrated liquidity; sqrtPriceX96 tracks price, ticks define liquidity ranges
- **Fee**: static `uint24` from `fee()` (in ppm: 100/500/3000/10000)
- **Router**: `IUniswapV3Router.exactInputSingle(ExactInputSingleParams{fee, sqrtPriceLimitX96:0, deadline:block.timestamp+60, ...})`
- **Callback**: `uniswapV3SwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata data)` — used by V3FlashMini to transfer tokens to pool during swap

### PancakeV3
- **Math**: identical to UniV3
- **Fee**: `fee()` returns uint24 ppm — but slot0 has different ABI (uint32 `feeProtocol` instead of uint16). Uses dedicated `pancakeV3Slot0ABI` in multicall
- **Router**: no `deadline` field in `ExactInputSingleParams`. MUST use `_swapPancakeV3` handler in contract (dexType=8). Wrong dispatch = broken calldata
- **Flag pairing**: `executor_supports_pancake_v3` config bool must match the deployed executor contract. Flipping true against a contract without `_swapPancakeV3` sends Pancake hops through `_swapV2` and breaks everything
- **Callback**: `pancakeV3SwapCallback` (different name from uniswapV3SwapCallback) — V3FlashMini defines both as aliases to the same handler

### CamelotV3 / ZyberV3 (Algebra V1)
- **Math**: same as UniV3 (sqrtPriceX96 + ticks)
- **Fee**: DYNAMIC — read from `globalState()` after each swap. `AlgebraFeeRefreshCh` + `algebraFeeRefreshLoop` handles post-swap re-read
- **Router**: `IAlgebraRouter.exactInputSingle(ExactInputSingleParams{...})` — NO `fee` field in struct (pool determines fee), has `limitSqrtPrice` instead of `sqrtPriceLimitX96`. Dedicated `_swapCamelotV3` handler (dexType=3)
- **Fallback**: if `FeeBps=0` and DEX is CamelotV3/ZyberV3, sim uses `camelot_v3_default_fee_bps` (currently 10 bps) — overestimates for high-fee periods, matches typical low-fee
- **PoolCreated event**: Algebra emits a DIFFERENT PoolCreated signature than UniV3 (no fee field). Handled by dedicated `handleAlgebraLog` in discovery.go

### RamsesV3 CL
- **Math**: UniV3-compatible but supports sub-bps fee tiers (`FeePPM=1` = 0.0001%). `FeePPM` takes precedence over `FeeBps` for sub-1bps tiers
- **Router**: no `exactInputSingle` — must use `exactInput` with `path = abi.encodePacked(tokenIn, fee, tokenOut)`. Dedicated `_swapRamsesV3` handler (dexType=6)
- **Three factories**: main, alt, and CL (custom fee tiers) — all in `internal.Factories` mapped to DEXRamsesV3

### Curve (StableSwap)
- **Math**: StableSwap invariant with amplification factor `A`. Fee stored as `fee()` in 1e10 units (`curve_fee_1e10`)
- **Coin indexing**: tokens referenced by int128 index, not address. `curveIndexIn` / `curveIndexOut` on Hop. Derived from token0/token1 assumption: token0=index 0, token1=index 1 (correct for 2-coin pools)
- **Contract call**: direct `exchange(i, j, dx, min_dy)` on pool (not via router)
- **Sim**: `CurveSim.AmountOut` implements StableSwap getAmountOut via D-invariant iteration

### Balancer V2 Weighted
- **Math**: Balancer weighted-pool invariant `Π balance_i^weight_i = const`. Fee from `getSwapFeePercentage()` (in 1e18 scale)
- **Swap**: through singleton Vault `0xBA122222…`. Pool identified by `bytes32 poolId` (not address)
- **Contract call**: `Vault.swap(SingleSwap, FundManagement, limit, deadline)` via `_swapBalancer` (dexType=4)
- **Hop encoding**: `hop.pool = Vault address`, `hop.balancerPoolId` carries the actual poolId

### UniV4
- **Math**: concentrated liquidity, identical math to V3
- **Singleton**: all V4 pools live inside `PoolManager 0x360E68fa…`. Pool identified by `bytes32 poolId` computed from PoolKey(currency0, currency1, fee, tickSpacing, hooks)
- **State read**: via StateView `0x76fd297e…` (getSlot0, getTickBitmap, getTickLiquidity)
- **Swap**: `PoolManager.unlock(bytes data)` → contract's `unlockCallback` executes `poolManager.swap(key, params, hookData)` then `settle/take`. Dedicated `_swapUniV4` handler (dexType=7). Hop encodes `tickSpacing` in `curveIndexIn` field (reused), `V4Hooks` in `balancerPoolId` (reused)
- **Hooks**: pools with non-trivial hook addresses (bits set for BEFORE_SWAP / AFTER_SWAP / RETURNS_DELTA) may modify swap behavior — sim does NOT model. Filter at verification time

### TraderJoe V2 (DISABLED)
- **Math**: Liquidity Book (discrete bins), NOT xy=k
- **Status**: currently disabled in `v2FactoryAddresses` and `internal.Factories` — our UniswapV2Sim would produce wrong output
- **To enable**: requires dedicated `LiquidityBookSim` in simulator.go

### KyberSwap Elastic / SolidlyCom CL / ZyberSwap V3 (observation-only)
- Arbscan tracks these via `arbscanOnlyLabels`, but bot can't trade them (different math/interface). Any cycle through them is filtered.
