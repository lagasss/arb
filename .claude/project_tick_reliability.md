---
name: Tick management reliability fixes + ticktest suite
description: Tick bitmap/liquidity subsystem hardened and covered by cmd/ticktest; every formerly-silent failure now produces an observable counter or fails a /debug/tick-health invariant
type: project
originSessionId: acc4de0c-d22a-4388-8eff-b7652df4e608
---
Landed 2026-04-18 after an audit identified 11 reliability issues in the tick fetch path. The fixes and the test binary MUST stay paired — the binary is the invariant-enforcement layer that guarantees the fixes don't silently regress.

## Fixes

1. **Verified-empty vs fetch-failure** (`multicall.go`): `FetchTickMaps` now stamps `Pool.TicksFetchOK` + `TicksFetchReason` so the eval gate can tell "bitmap genuinely empty in range" (OK=true reason=empty-bitmap — do NOT reject purely on `len(Ticks)==0`) from "multicall RPC failed" (OK=false reason=bitmap-rpc-failed/ticks-rpc-failed). Before this, both produced identical state and the sim silently dropped opportunities during RPC 429 storms.

2. **Per-pool outcome tracking + TickFetchStats return** (`multicall.go`): `FetchTickMaps` now returns a `TickFetchStats{Pools,PoolsSucceeded,PoolsEmpty,PoolsFailed,PoolsSkipped,AlgebraRTMismatches,...}` struct. Callers in `bot.go:watchTickMapsLoop` store these in `Health.TickFetchPass*` atomics surfaced on `/debug/tick-health`.

3. **Algebra round-trip validation** (`multicall.go`): after recovering a `realTick` from `(wordPos, bit)`, the code now computes the inverse (tick → wordPos + bit) and compares. Mismatches increment `stats.AlgebraRTMismatches` and the pool is stamped `TicksFetchOK=false reason=algebra-roundtrip`. Guards against silent off-by-one corruption across Algebra and UniV3/V4 paths.

4. **Eager refetch drop observability** (`swaplisten.go` + `bot.go`): `SwapListener.TickDropCounter` + `TickEnqueueCounter` wire to `Health.TickFetchEagerDropped/Enqueued`. Both `fireTickRefetchImmediate` and `maybeFireTickRefetch` bump them on every send attempt. Drops are printed every stats flush (`[tickmap] eager refetch: enqueued=N dropped=M drop_rate=X%`).

5. **Refetch channel capacity config**: `trading.tick_refetch_chan_cap` is a mandatory config param (0 = log.Fatal). Raised from hard-coded 256 to 2048. Also sizes the Algebra fee-refresh channel.

6. **RPC skew gate** (`bot.go:IsReady`): `TickRPCBlock` is stored on every call to `currentTickBlock`. The health gate fails submissions when tick_data_rpc LAGS arbitrum_rpc by more than `tick_rpc_max_skew_blocks` (default 10). One-directional: tick RPC leading is harmless (bitmap fresher than state). Config name: `trading.tick_rpc_max_skew_blocks`.

7. **Split candidate reject counters** (`bot.go:evalOneCandidate`): the former `candRejectTickStale` is now the SUM of four sub-counters:
   - `candRejectTickNeverFetched` — pool hasn't had a FetchTickMaps pass yet
   - `candRejectTickFetchFailed` — last attempt stamped OK=false (RPC error or algebra round-trip failure)
   - `candRejectTickEmptyVerified` — OK=true but Ticks=nil (bitmap genuinely empty in range)
   - `candRejectTickCoverageDrift` — current tick outside fetched ±radius words
   Surfaced on `/debug/tick-health` as `reject_tick_*`.

8. **/debug/tick-health endpoint** (`debug_http.go`): single authoritative snapshot of pass stats, skew, channel state, per-reason breakdown, out-of-range pools, failure-loop pools, stale-by-age pools. Everything the test binary needs.

9. **Per-pool debug fields on /debug/pools**: `ticks_fetch_ok`, `ticks_fetch_reason`, `ticks_fetch_attempted_at`, `ticks_fetch_bitmap_words`, `ticks_fetch_non_empty_words`, `ticks_fetch_failure_count`. Visible in dashboard too.

## The test binary — cmd/ticktest

Invocation:
```
/home/arbitrator/go/arb-bot/ticktest -cat all           # every check
/home/arbitrator/go/arb-bot/ticktest -cat pass          # last-pass outcome
/home/arbitrator/go/arb-bot/ticktest -cat eager         # drop rate
/home/arbitrator/go/arb-bot/ticktest -cat skew          # RPC lag
/home/arbitrator/go/arb-bot/ticktest -cat coverage      # tick out of range
/home/arbitrator/go/arb-bot/ticktest -cat staleness     # age
/home/arbitrator/go/arb-bot/ticktest -cat failure       # fetch-failure loops
/home/arbitrator/go/arb-bot/ticktest -cat gate          # per-reason rejections
/home/arbitrator/go/arb-bot/ticktest -cat chain         # cross-check bot bitmap vs on-chain
/home/arbitrator/go/arb-bot/ticktest -cat invariants    # structural checks
```

Thresholds are flags (`-max-eager-drop-rate`, `-max-fail-pct`, `-max-coverage-drift-pct`, `-max-stale-age-pct`, `-max-fail-loop-pct`). Exit code 1 on any FAIL.

Baseline (2026-04-18 21:02 post-restart, bot warmed up):
```
pass_total=95 pass_succeeded=95 pass_empty=1 pass_failed=0 pass_rt_mismatch=0 pass_dur_ms=14450
eager drop_rate=0%
rpc lag=0 (signed=-9 → tick RPC leads by 9 blocks) max=10
coverage 0/108 out of range
staleness 0/108 stale
failure 2/754 in failure loop (below 1% threshold)
gate fetch_failed=0 never_fetched=0 coverage_drift=0 empty_verified=0
chain cross-check: 5 pools, all bot-ticks ⊆ chain-bitmap
all 108 fetched pools have positive TickSpacing + TicksWordRadius
```

Result: **19 PASS, 0 FAIL, 0 SKIP**.

## Rebuild after tick-subsystem changes

```
cd /home/arbitrator/go/arb-bot && /usr/local/go/bin/go build -o bot.new ./cmd/bot && /usr/local/go/bin/go build -o ticktest ./cmd/ticktest
```

## Don'ts

- Don't re-introduce hard-coded `256` for TickRefetchCh — it'll silently drop on bursty markets. The config-validator fatals at startup if `tick_refetch_chan_cap` is 0.
- Don't treat `len(Ticks)==0` alone as "pool is stale". Check `TicksFetchOK` first — empty verified is a legitimate state.
- Don't make the RPC skew gate bi-directional. Tick RPC leading the main RPC is harmless.
