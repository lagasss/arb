---
name: V4 pool stranded-in-tokens bug
description: v4_pool_tokens rows that never got promoted to v4_pools were invisible to the bot; fix unions both sources in V4PoolIDsMissingTickSpacing
type: project
originSessionId: 03e1d65c-9b34-4b1b-b0f7-bea1a18cc9b9
---
Bug fixed 2026-04-19. arbscan's V4 discovery has two DB tables:
- `v4_pool_tokens` (poolId→tok0,tok1) — seeded by Uniswap GraphQL API (`topV4Pools first:100`)
- `v4_pools` (full metadata: fee_ppm, tick_spacing, hooks, state) — seeded by Initialize event logs

The API-only path wrote to `v4_pool_tokens` but the Initialize-log backfill only looked at `V4PoolIDsMissingTickSpacing`, which queried `v4_pools WHERE tick_spacing=0`. Pools that never landed in `v4_pools` at all were invisible forever. The bot only reads `v4_pools`, so those pools never loaded → cycles through them were impossible.

Competitor trade id 15870 surfaced it: pool `0xe39e8b…3853c44` (USDC↔USDC.e, 3.5bp, no hooks) was in the Uniswap top-100 at rank 76 with $15k TVL but stranded in `v4_pool_tokens`.

Hard cap discovered: `topV4Pools(first: ...)` rejects anything >100.

**Why:** API writes tokens, Initialize-logs write metadata — they diverge when an Initialize log doesn't hit the backfill's block window. `V4PoolIDsMissingTickSpacing` only looked at one side of the join.

**How to apply:** If you ever see `comparison_result: missing_pool` on a UniV4 hop, check `v4_pool_tokens` first — if the pool is there but not in `v4_pools`, the backfill UNION path is still working (a regression would mean the fix was reverted). The fix is in [internal/db.go:1371](arb-bot/internal/db.go#L1371) — do not narrow that query back to `v4_pools` only.
