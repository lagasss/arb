---
name: RPC endpoints
description: Three RPC endpoints used by the arb bot — one per role (tx submission, live tracking, historical)
type: reference
---

Three RPCs, each with a dedicated role:

- **Transaction submission** — used to broadcast arb transactions
- **Live blockchain tracking** — Chainlink node, used for real-time swap events and mempool monitoring
- **Historical data** — Alchemy, used for backfill, simulation, and off-chain data queries

**How to apply:** When touching RPC config, simulation, backfill, or latency-sensitive code, keep these roles in mind. Don't mix concerns (e.g. don't use the historical RPC for tx submission). The `detect_rpc` and `submit_rpc` columns in `our_trades` track which RPC detected the opportunity vs sent the tx.
