---
name: Arbitrum Flash Loan Arbitrage Bot
description: Go bot + Solidity contract for DEX/DEX triangular arb on Arbitrum using Balancer flash loans
type: project
---

Full project scaffold already built at /home/arbitrator/go/arb-bot/. Structure matches the design doc exactly.

**Why:** Seb wants to capture DEX/DEX price divergences on Arbitrum One atomically via flash loans (0% fee from Balancer Vault). Montreal fiber + local Arbitrum node = latency edge.

**Strategy:** Balancer flash loan (primary, 0% fee) → triangular swap cycle (e.g. USDC→ETH→ARB→USDC) across Uniswap V3, Camelot, Trader Joe, Curve, SushiSwap → repay principal → keep profit.

**Key design decisions:**
- Go (goroutines, near-Rust speed) — chosen over Rust/Python
- Bellman-Ford on log-weighted directed graph for cycle detection
- Multicall batches 100-150 pool reads per block (<5ms)
- AMM simulators for V2, V3 (tick math), Camelot (directional fees), Curve (StableSwap Newton's method)
- ArbitrageExecutor.sol: onlyOwner execute(), receiveFlashLoan() with vault auth check, minProfit guard
- Balancer Vault: 0xBA12222222228d8Ba445958a75a0704d566BF2C8 (Arbitrum)

**Files built:**
- cmd/bot/main.go — entry point
- internal/token.go, pool.go, registry.go, graph.go, bellmanford.go
- internal/simulator.go (V2/V3/Camelot/Curve AMM math), simulator_test.go
- internal/multicall.go, discovery.go, executor.go, bot.go
- contracts/ArbitrageExecutor.sol + interfaces + Foundry test
- config.yaml (all factory addresses, token whitelist pre-populated)
- go.mod with go-ethereum v1.13.14

**Development roadmap (10 phases):**
1. Pool reader (multicall reads 50 pools/block) ← START HERE
2. Graph builder
3. Cycle detector (Bellman-Ford live)
4. AMM simulator (unit tests vs on-chain quotes)
5. Contract skeleton (testnet deploy)
6. Integration (Go bot → testnet contract)
7. Multi-hop (3-hop triangular)
8. Paper trading (1 week mainnet logging)
9. Mainnet live (small capital)
10. Scale

**How to apply:** When Seb asks what to work on next, refer to the roadmap. Phase 1 (pool reader/multicall) is the starting point.
