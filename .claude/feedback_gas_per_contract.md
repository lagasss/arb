---
name: Gas cost must be per-contract
description: Profit calculations must use per-contract gas estimates, not a single global value — different executors have very different gas profiles
type: feedback
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Profit/min-profit calculations must use gas cost specific to the executor contract that will handle the trade, not a global estimate.

**Why:** V3FlashMini uses ~350-400K gas, ArbitrageExecutor uses ~900K, OwnCapitalMini (planned) targets ~340K. Using a single gas estimate means either (a) thin-margin trades routed to the cheap contract get rejected because the gas floor assumes the expensive contract, or (b) trades routed to the expensive contract pass because the floor assumes the cheap one.

**How to apply:** The contract_ledger table should include a `gas_estimate` column per contract. When evalOneCandidate computes minProfit (gas_cost × gas_safety_mult), it must know WHICH contract the cycle will route to — and use that contract's gas estimate. This means the routing decision (which contract?) must happen BEFORE the profitability check, not after.
