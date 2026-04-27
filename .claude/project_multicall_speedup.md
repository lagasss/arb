---
name: Multicall speedup + stamp-at-latest (sb_lag fix)
description: 2026-04-19 refactor of BatchUpdatePoolsAtBlock + doMulticall; parallel batches, immutable skipping, stamp-at-latest. Eliminated pre-dryrun regate rejects from the `sb_lag>1` class
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Landed 2026-04-19. Three changes together, one patch to `internal/multicall.go` + one call-site in `internal/bot.go`.

## What changed

**1. Parallel batches.** `doMulticallParallel` replaces the sequential loop in the per-block state refresh path. Each Multicall3 batch (500 sub-calls) dispatches as its own goroutine; wall-clock time collapses from `sum(batches_RTT)` to `max(batches_RTT)`. ~10× speedup on Chainstack.

**2. Skip immutable sub-calls.** Previously every per-block pass re-read `tickSpacing` for all 758 V3 pools, `getNormalizedWeights` + `getSwapFeePercentage` for every Balancer pool, `A()` + `fee()` for every Curve pool, and the Camelot V2 directional-fee calibration (`getAmountOut` ×2). All of these are either immutable or admin-controlled slow-changing state.

New approach: per-pool build step conditionally skips these sub-calls when the corresponding `Pool` field is already populated (e.g. `p.TickSpacing > 0`, `p.Weight0 > 0`, `p.AmpFactor > 0`, `p.Token0FeeBps > 0`). The fields are primed at startup via `VerifyPool` (already running) and on pool resolution. Per-block call count drops ~40%.

**3. Stamp at latest, not at trigger block.** The old code called `BatchUpdatePoolsAtBlock(ctx, pools, bn)` with `bn` = the block number that triggered the dispatch. By the time the multicall returned 500-1000ms later, `Health.LatestBlock` had advanced 2-4 blocks. Pools got stamped with the stale `bn` → pre-dryrun regate saw `sb_lag=3b > max=1b` and rejected.

Fix: `watchNewBlocks` now passes `nil` for `blockNum`. `BatchUpdatePoolsAtBlock` reads `latest` from the RPC's POV, then calls `client.BlockNumber()` AFTER the multicall completes to stamp pools with the block the RPC actually observed at read time.

## Architecture: tagged-call refactor

To make (2) implementable without fragile fixed-offset decoding, introduced `callKind` enum + `taggedCall` struct. Each sub-call is tagged with its purpose (`kindV3Slot0`, `kindTickSpacing`, `kindBalancerTokens`, etc.). The decoder groups results per-pool and applies each Kind individually; skipped sub-calls simply don't appear in the results. Fixes a whole class of ordering bugs.

## Observability invariants

- `sb_lag` distribution in `[validate]` and `[pre-dryrun regate]` log lines should be dominated by 0 and 1 (maybe occasional 2 during RPC hiccups).
- Any appearance of `sb_lag=3b abs > max=1b` in a fresh run means either (a) Chainstack RPC is in trouble, or (b) this refactor regressed — check `doMulticallParallel` isn't serializing.
- `first pass complete` log line should be < 1s on a clean boot.

## Don'ts

- Don't re-introduce `tickSpacing` to the per-block pass. It's immutable; reading it every block wastes ~3,000 sub-calls/pass.
- Don't make `BatchUpdatePoolsAtBlock` serial again. Parallel batches are the main reason `sb_lag` stays at 0-1.
- Don't pass `bn` (trigger block) from watchNewBlocks. The post-multicall `BlockNumber()` stamp is the correct semantic.
- Don't revert the tagged-call architecture. Fixed-offset decoding + skip-immutables would require per-pool offset bookkeeping that's easy to get wrong.

## Rebuild after touching multicall

```
cd /home/arbitrator/go/arb-bot && /usr/local/go/bin/go build -o bot.new ./cmd/bot
```

Deploy via /tmp/restart_bot.sh (bot.new → bot, kill old, nohup new). Bot starts in its own cwd so `config.yaml` resolves via relative path.

## HTTP/2 dialer (landed same day)

`internal/rpc_http2.go` defines `DialHTTP2(url)` using the stdlib http.Transport with `ForceAttemptHTTP2=true`. No external dep on `golang.org/x/net/http2` — stdlib has built-in HTTP/2 since Go 1.6 and auto-upgrades via ALPN.

Used by:
- `tick_data_rpc` client (`bot.go` — `b.tickClient`)
- `simulation_rpc` client (`executor.go` — `e.simClient`)

NOT used for:
- `arbitrum_rpc` (WSS — WebSockets tunnel over HTTP/1.1 by design; different go-ethereum code path)
- `sequencer_rpc` (plain POST via net/http, not go-ethereum RPC)

Chainstack HTTPS endpoint confirmed via `curl --http2 -w '%{http_version}'` → returns `2`. Log lines to confirm activation: `[bot] tick_data_rpc dialed with HTTP/2` and `[executor] simulation client connected to ... (HTTP/2)`.

Benefits: parallel multicall batches (5 concurrent via doMulticallParallel) share a single TLS connection via HTTP/2 streams instead of N TCP/TLS handshakes. Measurable win primarily on bursty ticks + eth_call dry-runs.
