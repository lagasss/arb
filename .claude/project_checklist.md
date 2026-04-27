---
name: Arb Bot — Pending Setup Steps
description: Things that must be done before the bot can go live — checklist for future sessions
type: project
originSessionId: ec5763e2-a748-48c4-94e0-3e6a24cf0749
---
Steps remaining before the bot can trade. Check off as completed.

**Why:** These are one-time config tasks that require external accounts, keys, or deployed contracts. Cannot be done until those prerequisites exist.

## Competitive-edge work (pre-live)
- [ ] **OwnCapitalMini** executor — 2-3 hop, no flash, ~340k gas target. Highest-leverage item: closes the sub-$0.05 / 257k-gas layer competitor 15191 dominates. Fund with ~$500 USDC + $500 WETH, wire `ownCapitalQualify` into `evalOneCandidate`, add `trading.executor_own_capital_mini` config key, flip ledger row `not_deployed:own_capital_mini` → live.
- [ ] **Sequencer feed ingestion** (queue behind OwnCapitalMini) — subscribe to `wss://arb1.arbitrum.io/feed` for ~200-400ms pre-block view of pending txs. Requires Nitro `broadcastclient` decoder, pending-swap → affected-cycle index, pending-state sim overlay, fast-path scorer bypassing block-boundary pipeline. ~1 week of work. Wasted effort until gas gate is competitive.

## Server / Bot Config
- [ ] Add wallet address to `/home/arbitrator/go/arb-bot/.env` → `ARB_BOT_WALLET=0x...`
- [ ] Add wallet private key to `/home/arbitrator/go/arb-bot/.env` → `ARB_BOT_PRIVKEY=0x...`
- [ ] Add Alchemy API key to `config.yaml` → `l1_rpc: https://eth-mainnet.alchemyapi.io/v2/YOUR_KEY`
- [ ] Set `executor_contract` in `config.yaml` after deploying ArbitrageExecutor.sol

## Smart Contract
- [ ] Install Foundry on server or local machine
- [ ] Deploy `ArbitrageExecutor.sol` to Arbitrum mainnet
- [ ] Paste deployed contract address into `config.yaml` → `executor_contract: 0x...`
- [ ] Fund the wallet with ETH (for gas) — a few hundred dollars worth minimum
- [ ] Test contract on Arbitrum Sepolia testnet before mainnet deploy

## Arbitrum Node
- [ ] Wait for Arbitrum Nitro node to fully sync (check: `docker logs arbitrum-nitro -f`)
- [ ] Confirm bot can connect: `ws://localhost:8548` responds to eth_blockNumber
- [ ] Update `config.yaml` → `arbitrum_rpc: ws://localhost:8548` (already set, just verify)

## Monitoring
- [ ] Install InfluxDB on server (for P&L logging)
- [ ] Install Grafana on server (for dashboards)
- [ ] Add InfluxDB token to `config.yaml`
- [ ] Set up Telegram bot token + chat ID in `config.yaml` for trade alerts

## Development Roadmap (10 phases)
- [ ] Phase 1: Pool reader — multicall reads 50 pools/block cleanly
- [ ] Phase 2: Graph builder — weighted graph from pool data
- [ ] Phase 3: Cycle detector — Bellman-Ford on live data
- [ ] Phase 4: AMM simulator — unit tests match on-chain quotes
- [ ] Phase 5: Contract skeleton — testnet deploy, single swap works
- [ ] Phase 6: Integration — Go bot triggers testnet contract end-to-end
- [ ] Phase 7: Multi-hop — 3-hop triangular cycle on testnet
- [ ] Phase 8: Paper trading — 1 week mainnet logging, measure spread duration
- [ ] Phase 9: Mainnet live — small capital, monitored closely
- [ ] Phase 10: Scale — expand pool list, add chains

## Server Info
- Host: `arb1` (sessions run directly here; previously accessed remotely as `seb@209.172.45.63`)
- User: seb
- Project path: /home/arbitrator/go/arb-bot
- Bot binary: built to /tmp/arb-bot-enum, logs to /tmp/arb-bot.log
- Bot service: arb-bot.service file is empty (bot runs via nohup manually)
- Competitor scanner output: /tmp/arbscan.jsonl
- Dashboard: running on port 8080 (http://arb1:8080 from LAN, or http://209.172.45.63:8080 externally), user systemd service `arb-dashboard`
- Dashboard binary: /home/arbitrator/go/arb-bot/arb-dashboard
- Arbitrum node: /usr/local/bin/nitro process (not Docker), listening on localhost:8547/8548
- Go: /usr/local/go/bin/go (v1.22.4)

**How to apply:** At the start of any session, read this file to know what's left to do and remind Seb of pending items if relevant.
