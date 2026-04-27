---
name: Borrowable token filter — BAL#528 elimination
description: Cycle cache now filters start tokens by Balancer Vault flash-loan eligibility, eliminating the BAL#528 sim-reject failure class (was 51% of all sim-rejects)
type: project
---

**Symptom (pre-fix):** 51% of all sim-rejects in a busy hour returned `BAL#528` (`INSUFFICIENT_FLASH_LOAN_BALANCE`). Every single one was for a borrow token that **Balancer Vault held zero of** — typically exotic tokens like EVA, ZRO, #BB that the bot had picked up via swap-event discovery and treated as valid cycle start tokens.

**Root cause:** The cycle cache's DFS picked start tokens from the in-memory token registry (`cc.whitelist`) without checking whether Balancer can actually flash-loan that token. The 95% vault-balance cap in `executor.Submit` was supposed to handle this:

```go
if vaultBal := e.vaultBalance(ctx, borrowToken); vaultBal != nil && vaultBal.Sign() > 0 {
    cap95 := vaultBal * 95 / 100
    if amountIn > cap95 { amountIn = cap95 }
}
```

But the cap **only applied when `vaultBal > 0`** — when Balancer held *zero* of the token, the cap was skipped and the original `amountIn` flowed through unchanged, producing BAL#528 every time.

**Fix:** Two-layer.

1. **`internal/borrowable.go`** (new): a `borrowableTracker` that maintains the set of tokens with non-zero Balancer Vault balance. Refreshed via a single multicall round-trip every hour by `RunBorrowableRefreshLoop` started from `Bot.Run()`. Validated 2026-04-11: 70 of 1289 registry tokens (~5%) are actually flash-loanable.
2. **`internal/cyclecache.go`** start-candidate selection in `rebuild()` now checks `cc.borrowable.Has(addr)` and skips non-borrowable tokens. Falls open (treats everything as borrowable) when the tracker is empty so the bot doesn't go dark before the first refresh.
3. **`internal/executor.go` `Submit` defensive backstop**: when `vaultBal == 0` for the borrow token, return an explicit error (`borrow token not held by Balancer Vault (would BAL#528)`) instead of skipping the cap. Also returns an error when the vault balance lookup itself fails, instead of silently trusting the optimizer.

**Validation 2026-04-11:**

| Metric | Before | After (15 min sample) |
|---|---|---|
| BAL#528 sim-rejects | 74 in last hour | **0** |
| Unique borrow tokens used | EVA, #BB, ZRO, … (junk) | USDT, WBTC (major) |
| Borrowable token count | n/a (no filter) | 70 / 1289 |
| Sim-reject ratio | 84% | small sample, real reverts only |

**How to apply / extend:** the borrowable refresh interval is currently 1 hour (`Bot.Run()` calls `RunBorrowableRefreshLoop(..., 1*time.Hour, nil)`). Vault balances change as Balancer pools get traded — if a token's vault balance changes by orders of magnitude in a single hour, the bot will either (a) try to borrow more than the new lower balance and hit BAL#528 once before the refresh corrects it, or (b) skip a token whose balance just appeared. Tighten to 15-30 min if false positives or negatives become noticeable. The min-balance threshold passed to `RunBorrowableRefreshLoop` is currently `nil` (any non-zero balance qualifies); raise to e.g. `1e18 * $10k worth` if exotic-token cycles still produce thin/wasteful trades.

**Latent followup:** the borrowable filter doesn't reduce cycle count much (88k → 98k — actually slightly more cycles because pool count grew between samples), but it eliminates the wasted 50ms eth_call per BAL#528 attempt. Combined with the multicall-split fix (see project_multicall_split.md), the bot's RPC budget is now spent on cycles that can actually execute.
