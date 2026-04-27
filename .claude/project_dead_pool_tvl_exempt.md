---
name: Dead-pool filter TVL exemption
description: Pool quality filter bug where huge stable pools were rejected as "dead pool", fixed 2026-04-14
type: project
originSessionId: 2d6b593d-b638-4f91-a725-06bb07a3c542
---
The `volume/TVL` dead-pool floor in [graph.go](arb-bot/internal/graph.go) rejects pools whose 24h volume / TVL falls below `min_volume_tvl_ratio` (0.001). This broke for deep stable/stable pools: the $1.36B UniV4 USDC/USD+ pool (0xdbe8fb92…) had $353k/day volume → ratio 0.00026 → rejected → competitor 2-hop xUSD↔USDC trades worth $200-500 all went undetected for days.

**Why:** The ratio check is right for scam pools (small + no volume), wrong for huge liquidity reservoirs whose turnover is slow by construction.

**How to apply:** Added `min_volume_tvl_ratio_exempt_tvl_usd` (default $50M) in config.yaml. Above that TVL, the ratio check is skipped. After the fix the cycle cache went from 461k cycles / 186 pools → 1.29M cycles / 224 pools in one rebuild.

Diagnostic that caught it: temporary `[adj-diag]` log in `cyclecache.rebuild` dumping adj[USDC], adj[WETH], adj[xUSD] destinations (removed after fix). If a symmetric fee-tier 2-hop pattern is failing again, re-introduce that log.
