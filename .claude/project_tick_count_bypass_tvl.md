---
name: Tick-count filter TVL bypass
description: Configurable TVL threshold above which the min_tick_count quality filter is skipped; lowered from hardcoded $1M to $10k to admit niche major-token pairs
type: project
originSessionId: 03e1d65c-9b34-4b1b-b0f7-bea1a18cc9b9
---
Fix landed 2026-04-19. The pool-quality filter in `poolQualityReason` ([internal/graph.go:79](arb-bot/internal/graph.go#L79)) rejected V3/V4 pools with `len(p.Ticks) < min_tick_count AND p.TVLUSD < 1_000_000`. The $1M cutoff was a hardcoded "big pools bypass" but excluded legitimate niche pairs that competitors still trade — e.g. tBTC/USDC UniV3 0.3% at $27k TVL, 5 ticks, used by competitor_arbs id 15982.

**Fix:** new config param `min_tick_count_bypass_tvl_usd` (default $1M for backward compatibility, set to $10k in config.yaml). Pools with TVL ≥ this value skip the tick-count gate entirely.

**Why:** TVL proxies trade capacity. The tick-count filter was meant to catch single-LP trap pools (1-2 ticks, often exotic tokens paired with WETH) — not niche stable/peg pairs. Above the bypass threshold, the pool has enough depth for small arb notionals regardless of tick distribution.

**How to apply:** If `comparison_result` in `competitor_arbs` shows `cycle_not_cached` but all pools are `known/verified`, check for tick-count rejections on any pool in the path. Look in `bot.log` for `[quality-reject] ... tick_count=X < floor Y (tvl=$Z < bypass $W)`. If the pool's TVL is slightly below bypass, consider lowering further — but stay above `absolute_min_tvl_usd: 3000` to preserve the trap-pool defense. After the fix, cycles through tBTC/USDC jumped from ~0 to 618 and the exact 15982 cycle now lives in the cache.

The related `min_volume_tvl_ratio_exempt_tvl_usd` ($50M) handles the dead-pool volume/TVL filter for a different class (multi-$100M stable pools with low ratio but high absolute volume). Don't conflate the two — this one is about tick-count diversity, that one is about volume/TVL divergence.
