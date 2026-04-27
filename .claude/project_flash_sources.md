---
name: Multi-source flash loans (Balancer + V3 + Aave)
description: Bot uses 3 flash loan sources with priority ordering and fee-adjusted profit math; deployed 2026-04-12
type: project
originSessionId: 2d6b593d-b638-4f91-a725-06bb07a3c542
---
# Multi-source flash loans — deployed 2026-04-12

## Architecture

Three flash loan sources, selected per-cycle by `FlashSourceSelector.Select()`:

| Priority | Source | Fee | Entry point | Callback | Token count |
|----------|--------|-----|-------------|----------|-------------|
| 1 | Balancer Vault | 0 | `execute()` | `receiveFlashLoan()` | ~70 |
| 2 | Uniswap V3 pool | Pool's fee tier (5-100 bps) | `executeV3Flash()` | `uniswapV3FlashCallback()` | ~165 |
| 3 | Aave V3 Pool | 5 bps (0.05%) fixed | `executeAaveFlash()` | `executeOperation()` | ~20 |

## Contract

- Address: `0x73aF65fe487AC4D8b2fb7DC420c15913E652e9Aa`
- Deployed: 2026-04-12 tx `0x5dffd99ea782db779ebf586b9a6d42a54856d1b039efc6eb048ecac53424428b`
- Balancer Vault: `0xBA12222222228d8Ba445958a75a0704d566BF2C8`
- Aave Pool: `0x794a61358D6845594F94dc1DB02A252b5b4814aD` (set via `setAavePool()`)
- Previous contract: `0x6D808C4670a50f7dE224791e1B2A98C590157AeA` (Balancer-only)

## Go code

- `internal/flashsource.go` — `FlashSourceSelector`, `FlashSelection`, `RefreshV3FlashPools()`, `RefreshAaveReserves()`
- `internal/executor.go` — `executeV3FlashABI`, `executeAaveFlashABI`, `Submit()` routes based on `FlashSelection.Source`
- `internal/bot.go` — `evalOneCandidate` deducts flash fee from profit before submission gate
- `internal/cyclecache.go` — DFS start-candidate filter uses flash selector (all 3 sources) instead of Balancer-only borrowable tracker

## Fee math

In `evalOneCandidate`, after `optimalAmountIn` returns the simulator profit:
1. `FlashSourceSelector.Select(startToken)` picks the cheapest available source
2. If fee > 0: `feeCost = amountIn × feePPM / 1,000,000`
3. `profitUSD -= feeCostUSD`
4. `result.Profit -= feeCost` (native units)
5. `result.ProfitBps` recomputed
6. If profit < minProfit or < minSimProfitBps after fee: reject

## Refresh cadence

All three sources refresh every hour in the borrowable callback:
1. Balancer: `RefreshBorrowableTokens()` (existing, unchanged)
2. V3 flash: `RefreshV3FlashPools(registry)` — scans registry for cheapest V3 pool per token
3. Aave: `RefreshAaveReserves(client, aavePoolAddr)` — calls `getReservesList()` on Aave V3 Pool

## Config

```yaml
trading:
  balancer_vault: "0xBA12222222228d8Ba445958a75a0704d566BF2C8"
  aave_pool_address: "0x794a61358D6845594F94dc1DB02A252b5b4814aD"
  executor_contract: "0x73aF65fe487AC4D8b2fb7DC420c15913E652e9Aa"
```
