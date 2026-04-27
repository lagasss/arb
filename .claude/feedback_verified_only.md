---
name: Only verified pools and tokens
description: Hard rule — never use a pool or token in scoring/sim/execution unless every required metric has been blockchain-confirmed
type: feedback
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
**Rule**: never use a non-VERIFIED pool or token.

**Verified means**: every metric the simulator or router depends on has been read from the chain and validated for this specific pool/token — not defaulted, not inferred, not guessed.

Per-pool required-verified metrics:
- V2: `fee_bps` (via `getAmountOut` calibration), `reserve0`, `reserve1`, token0/token1 addresses, `IsStable` flag (if Camelot V2)
- V3: `fee_ppm` (from `fee()`), `tick_spacing` (from `tickSpacing()`), `sqrtPriceX96` + `tick` (from `slot0()`), `liquidity` (from `liquidity()`), tick bitmap + per-tick `liquidityNet` via `tickBitmap()` / `ticks()`, token0/token1 addresses, quoter drift ≤ verifyMaxDriftBps
- V4: same as V3 but via StateView (`getSlot0`, `getTickBitmap`, `getTickLiquidity`), plus hooks address, tickSpacing, lpFee, protocolFee
- Algebra (CamelotV3/ZyberV3): dynamic fee from `globalState()`, tickSpacing, slot0-equivalent fields
- Curve: `A`, `fee` (1e10 scale), coin indices, reserves (`balances(i)`)
- Balancer: `getSwapFeePercentage` + `getNormalizedWeights` + pool tokens from Vault
- Ramses CL: `FeePPM` MUST be populated (sub-1bps tiers lose accuracy otherwise)

Per-token required-verified metrics:
- Address (lowercased)
- Symbol (from `symbol()`)
- Decimals (from `decimals()`)

**Why**: any defaulted/inferred value creates sim drift. A single wrong fee by 5 bps compounds over 4 hops into a guaranteed revert. The 4 eth_call OK trades in this session all went through pools with fully-verified state; every phantom revert involved at least one partially-verified pool (zero ticks, stale fee, wrong decimals, etc.).

**How to apply**:
- Cycle cache builder MUST skip any pool where `Verified=false` — not just pools with a non-empty `VerifyReason`. Pools in the default "never verified yet" state are also skipped.
- Scoring pipeline MUST re-check verified status at candidate evaluation time. A pool can become unverified between cycle cache rebuild and scoring (e.g., quoter drift detected by background re-verification).
- `ResolvePoolFromChain` sets `Verified=true` only when EVERY check in `VerifyPool` passed. If any check fails, `Verified=false` + `VerifyReason` is set; the pool stays in the registry (for dashboard visibility) but is excluded from trading.
- Tokens must be verified before being added to the registry. `FetchTokenMeta` reads decimals/symbol from chain; config-seeded tokens count as verified IF their decimals match chain.
- When adding a new DEX type, the verification function must cover all metrics the simulator reads — if a field is needed and we can't verify it, the DEX must not be enabled for trading.
