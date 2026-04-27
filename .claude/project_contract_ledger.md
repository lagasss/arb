---
name: Contract ledger in SQLite
description: contract_ledger table in arb.db tracks all deployed/planned contracts with trade type routing, status, and supported DEXes
type: project
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
`contract_ledger` table in arb.db tracks every contract:
- **ArbitrageExecutor** `0x73aF65fe…` — active, main multi-source executor, all DEXes + flash sources
- **V3FlashMini** `0x33347Af4…` — active, gas-optimized V3-only (2-3 hops), V3 flash borrow
- **Old Executor** `0x6D808C46…` — deprecated (replaced 2026-04-12)
- **SplitArb** — not yet deployed, code in contracts/SplitArb.sol
- **OwnCapitalMini** — not yet deployed, planned for thin-margin 2-hop arbs without flash loan

**Why:** Different trade types need different contracts for optimal gas. The bot's executor routing should select contract based on cycle shape + flash source.

**How to apply:** When adding new contracts, insert into contract_ledger. When routing trades, query by trade_type + supported_dexes to pick the cheapest executor.
