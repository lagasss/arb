---
name: Executor contracts — dispatch, gas, supported DEXes
description: Per-contract technical detail: entry points, per-DEX handler, Hop encoding, max hops, gas estimates. Must stay accurate for correct routing.
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Source of truth is the `contract_ledger` table in arb.db. This file documents the technical details that aren't in the table: dispatch type numbers, calldata formats, handler names.

### ArbitrageExecutor (`0x73aF65fe487AC4D8b2fb7DC420c15913E652e9Aa`)
- **File**: `contracts/ArbitrageExecutor.sol`
- **Gas**: ~900K (multi-hop mixed)
- **Max hops**: unlimited
- **Flash sources**: Balancer (0 bps), V3 pool (5-100 bps), Aave V3 (5 bps)
- **Entry points**:
  - `execute(tokens, amounts, hopData, minProfitNative)` → triggers `balancerVault.flashLoan`, receiver is `receiveFlashLoan`
  - `executeV3Flash(poolAddr, borrowToken, amountIn, hopData, minProfitNative)` → triggers `pool.flash`, receiver is `uniswapV3FlashCallback`
  - `executeAaveFlash(borrowToken, amountIn, hopData, minProfitNative)` → triggers `aavePool.flashLoanSimple`, receiver is `executeOperation`
- **Hop struct** (9 fields, 288 bytes ABI-encoded per hop):
  ```
  uint8   dexType         (see dispatch table below)
  address pool            (router address for V2/V3, pool for Curve/Balancer, PoolManager for V4)
  address tokenIn
  address tokenOut
  uint24  fee             (V3 fee tier in ppm)
  uint256 amountOutMin    (slippage guard)
  int128  curveIndexIn    (Curve coin index; reused for V4 tickSpacing)
  int128  curveIndexOut   (Curve coin index)
  bytes32 balancerPoolId  (Balancer poolId; reused for V4 hooks address, right-aligned)
  ```
- **Dispatch table** (from `dexTypeOnChain` in Go → matches `DEX_*` constants in Solidity):
  | Value | DEX | Handler | Router / target |
  |---|---|---|---|
  | 0 | UniV2 / Sushi / UniV2 forks / RamsesV2 / Chronos | `_swapV2` | fork-specific router |
  | 1 | UniV3 / SushiV3 / RamsesV3* / PancakeV3 (fallback) | `_swapV3` | UniV3 SwapRouter `0xE59242…` |
  | 2 | Curve | `_swapCurve` | pool direct |
  | 3 | CamelotV3 / ZyberV3 | `_swapCamelotV3` | Algebra router |
  | 4 | Balancer | `_swapBalancer` | Vault |
  | 5 | Camelot V2 | `_swapCamelotV2` | Camelot router (with referrer) |
  | 6 | RamsesV3 | `_swapRamsesV3` | Ramses V3 router (exactInput path-encoded) |
  | 7 | UniV4 | `_swapUniV4` | PoolManager unlock |
  | 8 | PancakeV3 | `_swapPancakeV3` | Pancake router (no deadline in params) |
- **`executor_supports_pancake_v3` flag**: MUST be paired with the address. Flipping true against a contract that lacks `_swapPancakeV3` sends Pancake hops to `_swapV2` → wrong math
- **Profit check**: at end of callback, sweeps final borrowToken balance, requires `>= borrowAmount + flashFee + minProfitNative`, transfers excess to `owner`

### V3FlashMini (`0x33347Af466CE0aDC4ABE27fF84388FacF64D43cE`)
- **File**: `contracts/V3FlashMini.sol`
- **Gas**: ~375K (2 hops) → ~385K (5 hops)
- **Max hops**: 2-5 (enforced by Go via `cycleIsV3MiniShape`; contract only requires `hops % 61 == 0 && hops >= 61`)
- **Flash sources**: V3 pool only (uses `pool.flash` → `uniswapV3FlashCallback`)
- **Supported DEXes**: UniV3, SushiV3, RamsesV3, PancakeV3, CamelotV3, ZyberV3
- **Entry point**: `flash(flashPool, borrowToken, amount, isToken0, packedHops, minProfitNative)`
- **Packed hops format** (61 bytes per hop, no ABI encoding):
  ```
  [ 0:20] pool address (V3-compatible)
  [20:40] tokenOut address
  [40:41] flags byte (bit 0 = zeroForOne)
  [41:61] amountOutMin (uint160)
  ```
- **Transient storage** (EIP-1153): flash state lives in transient slots (pool, borrowToken, borrowAmount, hopsPtr, hopsLen) — zero gas refund, auto-clears
- **Direct pool.swap() calls**: no router indirection. Callback names `uniswapV3SwapCallback`, `pancakeV3SwapCallback`, `algebraSwapCallback` all alias to same handler (semantics identical)
- **Routing decision** (`qualifyForV3Mini` in executor.go): cycle must pass `cycleIsV3MiniShape` (2-5 hops, all V3-family DEXes) AND flash source must be `FlashV3Pool`

### Old ArbitrageExecutor (`0x6D808C4670a50f7dE224791e1B2A98C590157AeA`)
- **Status**: DEPRECATED. Replaced 2026-04-12. Has no V3 flash, no Aave flash, no UniV4 dispatch
- **Do not route to this address**

### SplitArb (not deployed)
- **File**: `contracts/SplitArb.sol`
- **Planned gas**: ~600K
- **Trade types**: CEX-DEX split routes (buy one leg, sell across N legs)
- **Flash source**: Balancer
- **Status**: awaiting `cex_dex` bot activation

### OwnCapitalMini (not deployed)
- **Planned gas**: ~340K (matching fastest competitor gas budget)
- **Trade types**: own-capital 2-3 hop arbs (no flash loan overhead)
- **Supported DEXes**: V3 family
- **Why**: 55% of observed competitor arbs use own capital; flash loan fee + ~160K extra gas eliminated

### Infrastructure contracts (on-chain reads only)
- Balancer Vault `0xBA12222222228d8Ba445958a75a0704d566BF2C8` — flash source
- Aave V3 Pool `0x794a61358D6845594F94dc1DB02A252b5b4814aD` — flash source
- Multicall3 `0xcA11bde05977b3631167028862bE2a173976CA11` — batch state reads
- UniV3 QuoterV2 `0x61fFE014bA17989E743c5F6cB21bF9697530B21e` — sim verification
- UniV4 StateView `0x76fd297e2d437cd7f76d50f01afe6160f86e9990` — V4 state + tick reads
- UniV4 PoolManager `0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32` — V4 swap execution

### Routing decision (in evalOneCandidate)
1. `miniShapeOK` = cycle length 2-5 AND all V3-family AND `executor_v3_mini` configured
2. Use V3FlashMini's 380K gas estimate for minProfit threshold if `miniShapeOK`, else ArbitrageExecutor's 900K
3. At submit time, `qualifyForV3Mini` re-checks shape + FlashV3Pool source; if it passes, calldata is packed for mini; otherwise full executor

### Accuracy hazards
- **Contract ↔ `dexTypeOnChain` mismatch**: adding a new DEX to Go without a matching `_swap*` handler in Solidity → hops revert
- **Wrong dispatch type**: e.g., mapping PancakeV3 to dispatch=1 (UniV3) ships broken calldata to the router (has `deadline` field that PancakeV3 router doesn't accept)
- **Gas estimate drift**: if ArbitrageExecutor gets more hops or changes dispatch overhead, the 900K estimate in `contract_ledger` is stale. minProfit threshold then uses wrong gas → accept trades that won't clear actual gas cost. Must re-measure after any contract change.
