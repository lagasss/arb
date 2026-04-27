---
name: gas_safety_mult = 1.0 (breakeven target, not 3× safety buffer)
description: Bot targets profit ≥ 1× gas cost, not 3×. Competitors run at ~1.2× and capture the sub-$0.05-net arbs that dominate Arbitrum volume
type: feedback
originSessionId: 3815f914-2e9b-4525-9ac4-3f915b6c5a87
---
**Rule**: `strategy.gas_safety_mult: 1.0`. Profit must only clear `txCost`, not `3 × txCost`.

**Why**: On 2026-04-17 we discovered the bot was missing 93% of competitor arbs (214/231 in 24h netted <$0.05). Investigating competitor_arbs id 13545 specifically: competitor netted $0.007 on a $0.025-gas trade = 1.2× gas. Our 3.0× multiplier meant we rejected anything below ~$0.09 profit, which excludes the entire low-margin layer that dominates Arbitrum MEV volume. Seb wants to compete at the breakeven layer — any positive net is worth capturing.

**How to apply**:
- When computing a cycle's minimum profit threshold, use `contractMin = cpuPrice × gasEst × 1.0` (set via this config value).
- DON'T re-raise this above 1.0 without a compelling reason. If revert rate on marginal trades becomes a problem, tighten sim accuracy or the LP-floor gate — not this multiplier. Those address the root cause (phantom profit) rather than just widening the buffer.
- Absolute floor still governed by `trading.min_profit_usd` (currently $0.001). The gas multiplier is the DYNAMIC floor (scales with gas price); `min_profit_usd` is the absolute floor.
- Matching change may be warranted: ensure Balancer flash is always preferred when the borrow token is balancer-borrowable (0-fee beats V3-flash's 5 bps) — the fee drag is often the difference between breakeven and loss at this margin.

**Companion tunings (aggressive-margin config, 2026-04-17)**: applied alongside gas_safety_mult=1.0 to unblock sub-25-bps arbs that dominate Arbitrum volume:
- `strategy.dynamic_lp_floor.base_bps_v3: 12.0 → 6.0` — halve the per-hop V3 buffer
- `strategy.dynamic_lp_floor.latency_overhead_bps: 5.0 → 2.0`
- `strategy.dynamic_lp_floor.tvl_scale_min: 0.5 → 0.3` — deep pools get a tighter floor
- Context: competitor_arbs id 13643 was a 4-hop V3/V4 cycle at 24 bps gross vs our ~25 bps floor. The old defaults were tuned for a higher-margin regime. At the new tunings a 4-hop V3 cycle with one stable/stable leg floors at ~14 bps instead of ~25, opening up the competitor's layer.
- These are correlated: if revert rate spikes badly after deploy, the floor was the load-bearing safety. Raise `base_bps_v3` back toward 8-10 before touching `gas_safety_mult`.
