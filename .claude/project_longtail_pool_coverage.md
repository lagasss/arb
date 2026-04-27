---
name: Long-tail pool coverage — post-go-live
description: Post-live TODO to extend pool discovery below current TVL/volume thresholds so we can participate in niche-token dislocations
type: project
originSessionId: 03e1d65c-9b34-4b1b-b0f7-bea1a18cc9b9
---
TODO to pick up post-go-live. Identified 2026-04-19 from competitor_arbs id 16106 (fUSDC dislocation, $4.68 net to the winner; 4 independent bots captured a combined ~$7.50 in the same block while we saw nothing).

**Gap:** our pool registry is seeded from top-N-by-TVL subgraph queries and Initialize-log backfills that are all gated on a minimum TVL/volume floor. Niche pairs like UniV3 USDC/fUSDC (`0xe47294075d211c3a4f61b5107959d523b51cdb8b`) and Camelot fUSDC/WETH (`0x85df70e1636d28ab29bb81df93b68834f4308750`) never land in `v4_pools`/`pools` — so even when a legitimate 20% dislocation opens on a real token (Fluid USDC, totalSupply ~41k), our DFS literally doesn't know the pool exists.

**Why:** lowering the seeding TVL floor globally would add thousands of junk pools. Opportunistic discovery (ingest pools we observe via `Swap` events in competitor txs or arbscan captures, then backfill on-the-fly) is the cheaper path but needs careful auth/verification so we don't onboard scam pools.

**How to apply:** when we're ready to expand post-live capital deployment, pick one of:
1. Lower the seeding TVL floor to something like $5k and rely entirely on the existing quality filters (tick-count, volume/TVL, absolute_min_tvl, `project_tick_count_bypass_tvl.md`) to sort out junk. Cheapest to implement, largest blast-radius — needs a verification pass before trusting it.
2. Listen to cross-pool Swap events in the swap listener, and when a swap happens on an unknown pool whose tokens are both in our token registry AND whose volume suggests real activity, queue an opportunistic pool-add via the normal registration path (`registry.ForceAdd` + tick-fetch + verify). This is more targeted but needs swap-listener re-architecture to accept unknown pools.
3. Subscribe to `arbscan`'s `competitor_arbs` insert stream: whenever we see a missed arb with `comparison_result=missing_pool` where the unseen pool's tokens are recognizable, auto-onboard it. Same path as (2) but driven off our own classifier output. Simplest integration point if #2 is too invasive.

Not a pre-live hard blocker. Real value is in post-live scale-up — we'd chase $0.50–$5 dislocations that our sub-$0.05 layer currently ignores. Before implementing, confirm the specialised-contract fleet (`project_lean_contract_fleet.md`) is in place so the gas math works on these larger single-hit trades.
