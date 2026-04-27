# Arb Bot — project context for Claude

Arbitrum arbitrage system. Go + Solidity. Three processes share one SQLite DB (`arb.db`):
- **bot** (`cmd/bot`) — detects + executes arbitrage
- **arbscan** (`cmd/arbscan`) — observes competitor txs, compares against our decisions
- **dashboard** (`cmd/dashboard`) — web UI over arb.db for trades, cycles, pools, health

## Goal

**Sim accuracy.** Every profit number our sim produces must match on-chain execution (`eth_call` OK) as close to 100% as possible. Profit thresholds are adjustable — accuracy is not. The bot stays in `pause_trading: true` until sim reaches near-100% eth_call OK rate. Every sim calculation must be validated against on-chain reality and tweaked until the match is near-perfect. Every data source (pool state, ticks, swaps, gas, flash-loan availability) must track the chain in real time — any lag causes failed trades.

## Hard rules

- **No default values.** Any number (threshold, interval, gas estimate, fee) is a `config.yaml` param with a comment. No hardcoded magic numbers.
- **Sim must match reality.** If a code path produces unreliable output (e.g. single-tick fallback for V3 with no tick data), it returns 0 — never an approximation. Missing an opportunity is better than a phantom trade.
- **No lag.** Every data source (pool state, ticks, swaps, gas price) must track the chain in real time. Stale data → wrong sim → failed trades. If we can't keep up, we filter out; we never guess.
- **Per-contract gas.** Profit calcs decide the routing contract first, then use that contract's gas estimate. No global gas number.
- **No code comments.** They drift and mislead. Self-documenting code.
- **Build and run on this machine.** Sessions run directly on the server (host `arb1`, user `seb`, repo at `/home/arbitrator/go/arb-bot`). Just `go build` from the repo root — no scp/ssh wrapper. Bot must always be started from `/home/arbitrator/go/arb-bot`, never `/tmp` or `/home/seb`.
- **Test against on-chain.** Sim changes must be validated with `eth_call` dry-runs before considered correct.

## Infrastructure

- **RPC quota is limited** while the Nitro node is syncing. Chainstack shared endpoint runs out of RUs quickly. Every RPC call counts — batch with Multicall3, reuse cached state, prefer events over polling.
- **Nitro node** being built locally. Once synced, it becomes the primary RPC and quota pressure disappears.
- **Montreal server, fiber uplink.** Low latency to most RPC providers.

## Contract deployment

We can deploy additional contracts freely to optimize gas per trade type. Current ledger in `contract_ledger` (sqlite). Active:
- `0x73aF65fe…` ArbitrageExecutor — all DEXes, all flash sources, ~900K gas
- `0x33347Af4…` V3FlashMini — V3-only 2-5 hop, V3 flash, ~380K gas

Planned: OwnCapitalMini, SplitArb. See `project_contract_ledger.md`, `feedback_gas_per_contract.md`.

## Directory layout

```
arb-bot/
  cmd/bot/        — main bot binary
  cmd/arbscan/    — competitor observer
  cmd/dashboard/  — web dashboard
  cmd/smoketest/  — standalone test binary
  internal/       — shared package (state, sim, executor, graph, cycle cache)
  contracts/      — Solidity (ArbitrageExecutor.sol, V3FlashMini.sol, SplitArb.sol)
  config.yaml     — all tunables
  arb.db          — SQLite WAL, shared by all three processes
```

## When in doubt

Check `/home/arbitrator/go/arb-bot/.claude/MEMORY.md` — it indexes all project/feedback/reference notes from prior sessions. If a memory conflicts with current code, trust the code.
