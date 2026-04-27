---
name: PancakeV2 executor dispatch (pre-live blocker)
description: PancakeV2 (factory 0x02a84c1b…) is mapped sim-only; trading through its pools requires a dedicated router + executor dispatch before pause_trading=false
type: project
originSessionId: 3815f914-2e9b-4525-9ac4-3f915b6c5a87
---
**Factory**: `0x02a84c1b3bbd7401a5f7fa98a384ebc70bb5749e` — PancakeSwap V2 on Arbitrum. 799 pairs, vanilla UniswapV2 shape (x·y=k, 0.3% fee, standard `getReserves`/`token0`/`token1`), no Solidly/Algebra extras. Added to `discovery.go` factory map on 2026-04-18 as `DEXUniswapV2`, label `"PancakeV2"`, `⚠ no dedicated router` — same treatment as SolidLizard/MMFinance/ZyberSwapV2/Horiza (sim-only for cycle-cache visibility; submitting would revert in UniswapV2Router). Unlocks observation of a class of cycles where ≥9/22 recent competitor-profitable cycles originated.

What Phase 2 (full enable) requires before going live:

1. **Dedicated DEXType** (`DEXPancakeV2`) — currently it shares the `DEXUniswapV2` dispatch which routes to UniswapV2Router on-chain, NOT PancakeV2 router. Any submitted cycle through these pools will revert inside Uniswap's router.
2. **Router entry** — add PancakeV2 router address to `dexRouter` map. Router is `0x8cFe327CEc66d1C090Dd72bd0FF11d690C33a2Eb` on Arbitrum (verify before deploy).
3. **Solidity dispatch** — either a new `_swapPancakeV2` case in `ArbitrageExecutor.sol`, or a verified claim that PancakeV2 router implements the same ABI as UniswapV2Router (`swapExactTokensForTokens`). Likely the latter since Pancake inherits V2 — but validate with a smoketest dry-run against a fork before assuming. If validated, no contract redeploy needed; just the router entry + DEXType wiring.
4. **VerifyPool coverage** — extend smoketest `-cat contract` to dry-run a PancakeV2 hop through the executor at realistic notional sizes.

**Why:** sub-$0.05 arbs on pairs like KIMA/USDT are routing through PancakeV2 repeatedly (observed competitor pattern). Without Phase 2, we continue to OBSERVE the opportunity but can't capture it. Every PancakeV2-touching cycle in the observation log is a margin we're donating.

**How to apply:** block flipping `pause_trading: false` until Phase 2 lands. Either do the dispatch work first, or add a config switch that excludes PancakeV2-touching cycles from submission (white-list fallback) so pause-off doesn't immediately burn gas on reverts. Same pattern applies to the other `⚠ no dedicated router` entries already in discovery.go (SolidLizard, MMFinance, ZyberSwapV2, Horiza) — they're all in the same class and should be addressed together when the dedicated-router work is done.
