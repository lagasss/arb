---
name: Arb bot end-to-end test plan
description: Categorized smoke + integration tests covering every subsystem of the Arbitrum arb bot, organized around the fact that most volatile state lives in the bot process's memory (not SQLite or chain) and therefore needs an introspection server before it can be tested
type: project
originSessionId: 2d6b593d-b638-4f91-a725-06bb07a3c542
---
# Arb-bot test plan

This is the canonical list of tests that must pass before we can be confident the bot is fully working. Invoked by the `/test` slash command.

## The big realization

**Most of what we want to test lives in the running bot's RAM**, not on chain, not in SQLite. The DB intentionally stores only metadata (`db.go:430-432`: *"Volatile state — prices, reserves, tick data, calibrated fees — is NOT stored, it's re-derived from the chain via multicall + calibration"*). The dashboard is a *separate* process that only reads SQLite, so it can't see the bot's memory either.

So the source-of-truth hierarchy for tests is:

| What we're verifying | Source of truth |
|---|---|
| Pool reserves, sqrt_price, liquidity, ticks | **Bot memory** (`Pool` struct in `internal/pool.go`) |
| Cycle cache contents and structure | **Bot memory** (`CycleCache` in `internal/cyclecache.go`) |
| Health timestamps (TickDataAt, MulticallAt, SwapEventAt, CycleRebuildAt) | **Bot memory** (`Health` atomics in `internal/bot.go`) |
| Executor caches: nonce, baseFee, vault balance | **Bot memory** (`Executor` in `internal/executor.go`) |
| Swap listener depth / peak / total | **Bot memory** (`SwapListener` in `internal/swaplisten.go`) |
| Pool metadata (address, DEX, tokens, fees) | **DB** `pools` table |
| Token registry | **DB** `tokens` table |
| Historical observations | **DB** `arb_observations` |
| Trade outcomes | **DB** `our_trades` |
| Contract bytecode + state | **Chain** (eth_call) |
| RPC reachability | **Chain** (eth_blockNumber) |
| Router/factory mappings | **Chain** + bot's `dexRouter` map (source code) |

A test that reads the DB to check reserves is **wrong** — it'll always say "empty" because we don't persist them. The same test against the bot's memory would say "fresh".

## Infrastructure prerequisites

These have to be built **before** most tests can actually run. The slash command will SKIP tests that depend on missing infrastructure with a clear "needs `<thing>`" reason.

### ✅ INFRA-1: bot debug HTTP server — **IMPLEMENTED 2026-04-11**

Lives in [`internal/debug_http.go`](arb-bot/internal/debug_http.go), started from `Bot.Run()` via `startDebugHTTP(ctx, b, b.cfg.Trading.DebugHTTPPort)`. Bound to `127.0.0.1:6060` (loopback only) when `trading.debug_http_port != 0` in config.yaml. All endpoints are read-only GETs returning JSON.

Endpoints (sample with `curl http://127.0.0.1:6060/debug/<endpoint>`):

- `GET /debug/health` — `Health.*` atomic timestamps with computed ages, `RPCRateLimit` map, `MinProfitUSD`, `LastGasPriceGwei`, plus the configured thresholds
- `GET /debug/pools?dex=<DEX>&limit=<N>&offset=<M>&token=<addr>` — full snapshot of every `Pool` in the registry (under `Pool.mu` RLock); includes reserve0/1, sqrt_price_x96, liquidity, tick, ticks_count, ticks_updated_at, last_updated, V4 hooks, Curve amp, Balancer pool_id, Camelot directional fees, TVL, spot price, disabled/pinned/verified flags. Pagination via `limit`/`offset`. Filters: `dex` (string), `token` (matches token0 or token1).
- `GET /debug/pools/{address}` — single pool, same fields, returns 404 if not in registry
- `GET /debug/cycles?limit=<N>&token=<addr>&pool=<addr>` — sample of cycle-cache entries with full edge details (path string, per-edge pool/dex/tokenIn/tokenOut/feeBps, log_profit). Defaults to limit=50.
- `GET /debug/cycle-cache/stats` — total cycles, last rebuild timestamp, pools_covered, top-20 pools by cycle count
- `GET /debug/executor` — contract address, wallet address, chain ID, balancer vault, cached nonce, cached base_fee_wei, cached vault_balances per token (under `vaultBalMu`), `vault_bal_block`
- `GET /debug/swap-listener` — `swaps_total` counter, `max_logs_depth` peak, `logs_chan_cap`, `pools_subscribed`
- `GET /debug/config` — non-sensitive config dump: full `strategy` block + `trading` minus RPC URLs (which contain API keys) and wallet info beyond the public address
- `GET /debug/` — root index listing every endpoint

**Validation 2026-04-11**: all 8 endpoints respond, return live state, snapshot under appropriate locks. Spot-tested with state/health/cycles categories — see test plan body for refined PASS criteria.

### ✅ INFRA-2: smoketest test binary at `cmd/smoketest` — **IMPLEMENTED 2026-04-11**

Lives at [`cmd/smoketest/main.go`](arb-bot/cmd/smoketest/main.go). Standalone Go binary that imports `arb-bot/internal` and runs two test categories: `sim` (simulator vs on-chain quoter per DEX) and `contract` (synthetic 2-hop eth_call against the deployed executor per DEX dispatch).

**Architecture:** reads pool state from the live bot's `/debug/pools` HTTP endpoint, reconstructs minimal `*internal.Pool` objects, and runs probes against either the local `internal.SimulatorFor(d).AmountOut(...)` (for `sim`) or the deployed contract via `internal.BuildExecuteCalldata(...)` + `eth_call` (for `contract`). Read-only — never writes to the DB or broadcasts transactions.

**Public API surface added to `internal`:**
- `BuildExecuteCalldata(cycle, amountIn, slippageBps, minProfit, forTest)` — exposes the same calldata-builder the production submit path uses, with a `forTest` flag that bypasses the "round-trip unprofitable" reject in `buildHops`. Smoketest passes `forTest=true` for synthetic round-trip cycles.
- `SetExecutorSupportsPancakeV3(bool)` — public setter for the `executorSupportsPancakeV3` atomic so standalone tools can mirror the bot's dispatch policy from config. **Critical**: without calling this, the smoketest defaults to the legacy `DEX_V3=1` path for PancakeV3 swaps and gets the same calldata-layout failure the contract redeploy was supposed to fix. Always wire this from `cfg.Trading.ExecutorSupportsPancakeV3` in any new tool that imports `internal`.
- `buildHopsOpt(cycle, amountIn, slippageBps, bypassProfitCheck)` — internal generalization of `buildHops`. Production path still calls `buildHops` (which delegates with `bypassProfitCheck=false`).

**Usage:**
```
./smoketest -cat sim       # simulator vs on-chain quoter
./smoketest -cat contract  # contract dispatch via eth_call
./smoketest -cat all       # both
./smoketest -json          # machine-readable output
./smoketest -v             # print every probe to stderr
./smoketest -per-dex N     # number of pools to sample per DEX (default 3)
```

**Output:** markdown table with `ID | DEX | Pair | Status | Detail` per probe, plus a summary line. Status codes: PASS, WARN (small delta), FAIL (large delta or revert), SKIP (no probe implementation or no usable pool).

**Validated 2026-04-11:** sim category covers UniV2, Sushi, Camelot V2, ArbSwap, DeltaSwap, Swapr (V2 forks via UniV2 router's `getAmountsOut`); UniV3, SushiV3, PancakeV3 (UniV3 QuoterV2). Contract category covers UniV2, Sushi, Camelot V2, UniV3, SushiV3, PancakeV3, CamelotV3, RamsesV3 — **6 of 8 PASS** (incl. PancakeV3 regression check), 2 known smoketest edge cases (Camelot V2 router quirk, UniV3 back-leg sim returning 0).

**Known TODO (separate follow-ups):**
- SIM: implement on-chain quoter probes for Curve (`get_dy`), Balancer (`queryBatchSwap`), Algebra/CamelotV3 (CamelotV3 quoter), UniV4 (`StateView.quoteExactInputSingle`).
- SIM: skip pools with `fee_ppm=0` (sub-1bps tier storage bug — see project_test_plan.md latent issues).
- CONTRACT: investigate Camelot V2 dispatch quirk causing `hop 0: swap reverted` even though direct `getAmountsOut` works.
- CONTRACT: handle V3 back-leg pools with thin liquidity (smoketest sim returns 0 for hop 1).

### INFRA-3: extend `bot_health` table *(blocks some health tests if INFRA-1 isn't done first)*

If `/debug/health` isn't available, the next-best fallback is to make the dashboard helper that already writes `bot_health` rows include every Health field, not just the dashboard subset. But INFRA-1 supersedes this entirely.

---

## Test categories

**Categories:** `infra`, `rpc`, `db`, `pools`, `state`, `routers`, `cycles`, `sim`, `contract`, `flash`, `submit`, `health`, `forensics`, `invariants`, `e2e`

**Test tiers:**
- **T1** (immediate) — runnable now via curl/python3/grep directly on this machine
- **T2-debug** — needs INFRA-1 (bot `/debug/*` server). Will be runnable as soon as that endpoint exists.
- **T3-smoketest** — needs INFRA-2 (`cmd/smoketest` binary).

For every test report PASS / FAIL (with the offending detail) / SKIP (with the missing prerequisite). End each category with a markdown table summary.

---

## infra — Verify the test infrastructure itself exists

Run this category first; if its tests fail, skip to "Missing infrastructure" reporting.

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| INFRA-01 | T1 | Bot debug HTTP server is reachable on configured port | `curl -fs http://localhost:<port>/debug/health` from the server | 200 + JSON |
| INFRA-02 | T1 | `cmd/smoketest` binary exists and `--help` works | `ls /home/arbitrator/go/arb-bot/smoketest && ./smoketest -h` | binary present, exits 0 |
| INFRA-03 | T1 | The running bot was built from a tree containing `internal/debug_http.go` | `grep -l 'debug_http' /home/arbitrator/go/arb-bot/internal/` | file exists |

## rpc — RPC endpoint health  *(all T1)*

| ID | Verify | How | Pass |
|---|---|---|---|
| RPC-01 | `arbitrum_rpc` reachable | `eth_blockNumber` POST | block > 0, latency < 500ms |
| RPC-02 | `simulation_rpc` reachable | same | block within 2 of `arbitrum_rpc` |
| RPC-03 | `tick_data_rpc` reachable | same | block within 2 of `arbitrum_rpc` |
| RPC-04 | `sequencer_rpc` reachable | The Arbitrum sequencer rejects `eth_chainId`/`eth_blockNumber` ("method does not exist"). Probe instead with a deliberately-malformed `eth_sendRawTransaction("0xdeadbeef")` and confirm the response is HTTP 200 with a JSON-RPC error like `rlp: value size exceeds available input length` (selector recognized, decode failed). | HTTP 200 + RPC decode error |
| RPC-05 | All RPCs report Arbitrum One | `eth_chainId` to each | every response is `0xa4b1` |
| RPC-06 | RPCs within 2 blocks of each other | compare `eth_blockNumber` | max - min ≤ 2 |
| RPC-07 | No 429s in the bot's view of the world | `GET /debug/health` → `RPCRateLimit` map | every value 0 |
| RPC-08 | WSS swap subscription is healthy | grep `bot.log` for `[swap] subscribed` and `[swap] restarting` (last 30 min) | `restarting` count == 0 (note: `subscribed` count > 1 is normal — every `NotifyPoolAdded` triggers a graceful re-subscribe; only `restarting` indicates an error path) |
| RPC-09 | `[swap] logs chan` peak depth in last 5 min ≤ 50 | grep last 10 lines | peak < 50 |

## db — Database integrity  *(all T1)*

| ID | Verify | How | Pass |
|---|---|---|---|
| DB-01 | `arb.db` opens cleanly | python3 sqlite3 connect | no error |
| DB-02 | All expected tables present | check tokens, pools, arb_observations, our_trades, competitor_arbs, v4_pools, v4_pool_tokens, pool_ticks, v4_pool_ticks, bot_health | every table exists |
| DB-03 | `arb_observations` row count > 0 since redeploy | `SELECT COUNT(*) WHERE observed_at > <restart_ts>` | > 0 (or 0 in quiet windows is OK — note in detail) |
| DB-04 | Recent observations have non-empty `pool_states_json` | sample 10 most recent rows | every one has parseable JSON array |
| DB-05 | `tokens` table has at least the pinned set (USDC, USDT, WETH, WBTC, ARB) | DB query | all 5 present |
| DB-06 | `pools` table has > 1000 active pools | `SELECT COUNT(*) WHERE disabled=0 AND is_dead=0` | > 1000 |
| DB-07 | Schema migrations applied | `PRAGMA table_info(pools)` includes `pinned`, `verified` | columns present |
| DB-08 | `arb_log_max_rows` not exceeded | row count of arb_observations | ≤ configured max |
| DB-09 | `bot_health` table is fresh | `SELECT MAX(updated_at) FROM bot_health` | within 60s |

## pools — Pool registry integrity  *(metadata, mostly T1 via DB)*

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| POOL-01 | T1 | Every pool has address, dex, token0, token1, fee_bps populated | DB query | zero empty |
| POOL-02 | T1 | `LOWER(token0) < LOWER(token1)` (canonical ordering) | DB query | zero violations |
| POOL-03 | T1 | Every pool's token0/token1 is in the `tokens` table | LEFT JOIN | zero unmatched |
| POOL-04 | T1 | No duplicate pool addresses | GROUP BY HAVING COUNT > 1 | empty |
| POOL-05 | T1 | UniV2 pools verified via UniV2 factory.getPair | sample 10, eth_call factory `0xf1D7CC64...` | 10/10 match |
| POOL-06 | T1 | UniV3 pools verified via UniV3 factory.getPool | sample 10, eth_call factory `0x1F98431c...` | 10/10 match |
| POOL-07 | T1 | SushiV2 pools verified via Sushi factory | sample 5 | 5/5 match |
| POOL-08 | T1 | Camelot V2 pools verified via Camelot factory | sample 5 | 5/5 match |
| POOL-09 | T1 | All Curve pools have `amp_factor > 0` and `curve_fee_1e10 > 0` | DB query | zero zero rows |
| POOL-10 | T1 | All Balancer pools have `pool_id_hex` populated | DB query | zero empty |
| POOL-11 | T1 | All Camelot V2 pools have directional fees | DB query for `token0_fee_bps > 0 OR token1_fee_bps > 0` | every row has at least one non-zero |
| POOL-12 | T1 | At least 1000 active pools | DB query | ≥ 1000 |
| POOL-13 | T1 | All `disabled=1` pools have `verify_reason` documented | DB query | every disabled has a reason |
| POOL-14 | T2-debug | Quality filter: no live-registry pool has `tvl_usd > 0 && tvl_usd < cfg.Pools.AbsoluteMinTVLUSD`. Added 2026-04-12 after the Unishop.ai $580 pool class. Fail means either the prune loop hasn't run yet OR the cycle cache filter isn't evicting sub-floor pools. | `GET /debug/pools?limit=5000`, filter `tvl_usd > 0 && tvl_usd < floor`, compare against `/debug/config` AbsoluteMinTVLUSD | zero results (or annotate as "prune pending" if count < 5 and last prune was > 50 min ago) |
| POOL-15 | T2-debug | Quality filter: no verified V3/V4 cycle-cache pool has `ticks_count > 0 && ticks_count < cfg.Pools.MinTickCount`. Fail means the tick-count floor isn't being enforced at cycle rebuild time. | walk `/debug/cycles` V3/V4 pool addresses, check `ticks_count` from `/debug/pools/{addr}` against config | zero |
| POOL-16 | T2-debug | Quality filter: no live-registry pool has `fee_ppm == 10000 && tvl_usd > 0 && tvl_usd < cfg.Pools.HighFeeTierMinTVLUSD`. 1% fee tier + sub-$50k TVL = pump-and-dump signature. | filter `/debug/pools` | zero |
| POOL-17 | T2-debug | Quality filter: no live pool has `tvl_usd > 0 && volume_24h_usd > 0 && volume_24h_usd / tvl_usd < cfg.Pools.MinVolumeTVLRatio`. Dead-pool floor. | filter `/debug/pools` | zero |
| POOL-18 | T1 | Every pool referenced by `our_trades` or recent `arb_observations` still exists in the `pools` table. Catches orphan references that accumulate when a pool is removed from memory but observations keep its address. | cross-query — also covered by INV-16 for arb_observations; this is the `our_trades` half | zero orphans |

## state — Pool live state freshness  *(all T2-debug — bot memory is the source of truth)*

| ID | Verify | How | Pass |
|---|---|---|---|
| STATE-01 | Every **verified** V2 pool has `reserve0 > 0` in **bot memory**. Filter `verified=true` because the bot loads unverified pools metadata-only without populating reserves. | `/debug/pools` → filter `verified && dex in (UniV2, Sushi, Camelot, RamsesV2, DeltaSwap, Swapr, ArbSwap, Chronos, TJoe)` | zero with empty/zero reserve0. **Validated 2026-04-11: 720 verified V2 pools, 0 invalid → PASS** |
| STATE-02 | Every **verified** V3 pool has `sqrt_price_x96 > 0` and `liquidity > 0` | `/debug/pools` → filter `verified && dex in (UniV3, SushiV3, CamelotV3, RamsesV3, PancakeV3, ZyberV3, UniV4)` | zero invalid. **Validated 2026-04-11: 294 verified V3 pools, 0 invalid → PASS** |
| STATE-03 | Every V3 pool **in the cycle cache** has at least 1 initialized tick loaded | walk `/debug/cycles?limit=10000`, collect unique V3 pool addrs, check `ticks_count > 0` per `/debug/pools/{addr}` | every cycle V3 pool has ≥ 1 tick |
| STATE-04 | `TicksUpdatedAt < tick_pool_max_age_sec` for V3 pools **in the cycle cache only** — the bot deliberately fetches ticks only for cycle pools to bound RPC budget (`bot.go:1159` filters to `v3cyclePoolSet`); checking all verified V3 pools is wrong because ~75% are intentionally not tick-tracked | walk `/debug/cycles` → V3 pool addrs → check `now - ticks_updated_at` from `/debug/pools` | zero stale; allow 1–2 transient when a brand-new pool was just added to the cache |
| STATE-05 | `LastUpdated < pool_staleness_secs` for every live pool | `/debug/pools` → `now - last_updated` | zero stale; ≤ 5 with `last_updated == 0` (in-flight discoveries) |
| STATE-06 | Bot's V2 reserves match on-chain `getReserves()` for 10 random pools | sample via `/debug/pools`, query chain, compare | abs delta ≤ 1 swap of trade size |
| STATE-07 | Bot's V3 sqrt_price matches on-chain `slot0()` for 10 random pools | sample, RPC call, compare | within ±1 swap |
| STATE-08 | Every DB pool that **should be loaded** is in memory. Exclude DEX types the bot doesn't support (`V2(...)`, `SwapFish`, `MindGames`, `KyberSwap`, `PancakeV2`, `V2(0x...)`, `ElkFinance`) and pools with `verify_reason LIKE 'liquidity is zero%'` (correctly skipped). | DB query: `SELECT address FROM pools WHERE disabled=0 AND is_dead=0 AND dex IN <supported> AND COALESCE(verify_reason,'') NOT LIKE 'liquidity is zero%'`, set-difference against `/debug/pools` addresses | truly-missing count ≤ 10. **Ghosts (in-mem-not-in-DB) are normal** — pinned-config pools and live discoveries land in memory before being persisted. |
| STATE-09 | Every pool the cycle cache touches has fresh `last_updated` | cross-reference `/debug/cycles` pool list with `/debug/pools` ages | every cycle pool's `last_updated` within `pool_staleness_secs` |
| STATE-10 | No pool is `verified=true` AND missing required state for its DEX | filter `/debug/pools` for `verified` and check fields per DEX class | zero. (If verifier passes a pool, it must have populated state.) |

## routers — DEX router mapping verification  *(T1)*

| ID | Verify | How | Pass |
|---|---|---|---|
| ROUTER-01 | `dexRouter[UniV2]` has bytecode and `factory()` matches | `eth_getCode` + `factory()` call | code > 5kb, factory == `0xf1D7CC64...` |
| ROUTER-02 | `dexRouter[SushiV2]` has bytecode + factory | same | matches Sushi factory |
| ROUTER-03 | `dexRouter[Camelot]` (V2) has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-04 | `dexRouter[UniV3]` has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-05 | `dexRouter[SushiV3]` has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-06 | `dexRouter[PancakeV3]` has bytecode AND responds to no-deadline `0x04e45aaf` selector (regression for the calldata bug) | `eth_call` with dummy params | reverts with "STF" or insufficient-balance, NOT with selector mismatch |
| ROUTER-07 | `dexRouter[CamelotV3]` (Algebra) has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-08 | `dexRouter[RamsesV3]` has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-09 | `dexRouter[UniswapV4]` PoolManager has bytecode | `eth_getCode` | code > 5kb |
| ROUTER-10 | Balancer Vault matches `trading.balancer_vault` and has bytecode | check + `eth_getCode` | code > 5kb at `0xBA12222...` |
| ROUTER-11 | Bot's `dexRouter` map has an entry for every `DEXType` constant in `pool.go` | source-grep cross-check | no missing entries |
| ROUTER-12 | Every DEX type for which the bot has pools in the live registry ALSO has a verified QuoterV2 address in `cmd/smoketest/main.go` `quoterAddrFor`. Failure means sim accuracy tests (SIM-02 etc.) are silently SKIPing pools they should be checking. 2026-04-12: PancakeV3, SushiV3, RamsesV3 currently marked as TODO in `quoterAddrFor` — those pools' sim has no ground-truth validation and the test will FAIL until the quoter addresses are verified and added. This is intentional: the FAIL is the signal that says "you have PancakeV3 pools in the registry but no way to verify their sim; add the quoter or remove the pools." | walk `/debug/pools`, collect unique DEX strings, cross-check against `quoterAddrFor` via source grep | every DEX present in registry has a quoter OR is explicitly marked non-V3 |

## cycles — Cycle detection  *(T2-debug)*

| ID | Verify | How | Pass |
|---|---|---|---|
| CYC-01 | CycleCache has > 0 cycles in memory | `GET /debug/cycle-cache/stats` | total > 0 |
| CYC-02 | At least 30k cycles in steady state | same | ≥ 30,000 |
| CYC-03 | Last rebuild within 2× `cycle_rebuild_secs` | `now - last_rebuild_at` | within threshold |
| CYC-04 | Each cycle is closed (Edges[0].TokenIn == Edges[-1].TokenOut) | `/debug/cycles?limit=100`, walk every sample | every sample closes |
| CYC-05 | Each edge's TokenIn matches previous edge's TokenOut | sample inspection | no token mismatches |
| CYC-06 | At least 1 cycle exists for each pinned token | `/debug/cycles?token=<addr>` for each pinned | every pinned has ≥ 1 |
| CYC-07 | At least 1 cycle exists for every pool with TVL > $50k | cross-reference `/debug/pools` (TVL ≥ 50k) with `/debug/cycles/{addr}` | zero uncovered high-TVL pools |
| CYC-08 | The pool count covered by cycles is reasonable | `/debug/cycle-cache/stats` | within 50–500 |
| CYC-09 | Cycle log_profit values are within `lp_sanity_cap` | `/debug/cycles?limit=100` | every cycle log_profit ≤ cap |

## sim — Simulator correctness (per DEX)  *(all T3-smoketest)*

| ID | Verify | How | Pass |
|---|---|---|---|
| SIM-01 | `UniswapV2Sim.AmountOut` matches on-chain `getAmountsOut` for 10 random V2 pools (across UniV2, SushiV2, Camelot V2 forks) | smoketest binary | exact match (V2 math is closed-form) |
| SIM-02 | `UniswapV3Sim.AmountOut` matches `IQuoterV2.quoteExactInputSingle` (`0x61fFE014bA17989E743c5F6cB21bF9697530B21e`) for 10 random V3 pools | smoketest | within ±10 bps |
| SIM-03 | Multi-tick V3 simulation matches Quoter when input crosses ≥ 2 ticks | smoketest, use a deep V3 pool with concentrated liquidity | within ±20 bps |
| SIM-04 | `CamelotSim.AmountOut` matches Camelot V2 pair `getAmountOut` | smoketest, call pair directly | within ±2 bps (directional fees) |
| SIM-05 | `CurveSim.AmountOut` matches `pool.get_dy(i, j, dx)` | smoketest | within ±5 bps |
| SIM-06 | `BalancerWeightedSim.AmountOut` matches Vault `queryBatchSwap` | smoketest | within ±5 bps |
| SIM-07 | `SimulateCycle` for a known-historical observation reproduces ~the same `Profit` value | smoketest, replay against current pool state | within ±20% (acknowledges drift) |
| SIM-08 | Algebra (CamelotV3/ZyberV3) simulator with `camelot_v3_default_fee_bps` matches on-chain quote | smoketest | within ±20 bps |
| SIM-09 | `Pool.Snapshot()` preserves every mutable field. Construct a Pool with non-zero values in every mutable field (SqrtPriceX96, Liquidity, Tick, Reserve0, Reserve1, FeeBps, FeePPM, TVLUSD, Volume24hUSD, SpotPrice, Token0FeeBps, Token1FeeBps, IsStable, AmpFactor, CurveFee1e10, PoolID, Weight0, Weight1, TicksUpdatedAt, TickAtFetch, Ticks, Disabled, Pinned, Verified, VerifyReason, V4Hooks, LastUpdated). Call Snapshot(), assert every field on the clone equals the original. Catches drops, typos (`Token1: p.Token0 // overwritten just below` style), and new-field-forgot-to-clone bugs whenever someone adds a Pool field. | smoketest Go test that builds a Pool literal, snapshots, compares via reflection | every field matches |
| SIM-10 | `optimalAmountIn` is self-consistent: calling it twice with identical inputs returns `bestIn == bestResult.AmountIn` — i.e. the returned amountIn is exactly what the simulator ran with. This was the 2026-04-12 bug. | smoketest: construct a minimal cycle from a live pool, call optimalAmountIn twice, assert `bestIn.Cmp(bestResult.AmountIn) == 0` and that both calls return identical results | both assertions pass |
| SIM-11 | Simulator boundary: `UniswapV3Sim.AmountOut` on an amountIn equal to 100× pool.Liquidity returns a non-zero non-negative output without a panic. Catches integer overflow in the multi-tick simulator when someone upstreams a huge input. | smoketest synthetic test with an in-memory Pool | no panic, output ≥ 0 |
| SIM-12 | Metamorphic reverse-cycle test: if cycle A→B→C→A has `sim_profit_bps > 0`, then cycle A→C→B→A (same pools in reverse direction) must have `sim_profit_bps ≤ 0`. Both directions can't be simultaneously profitable at the same pool state — that would be free money. Catches simulator direction bugs. | smoketest: pick a recently-profitable cycle from `arb_observations`, reverse its edges, re-run SimulateCycle against current pool state | reverse cycle has `sim_profit_bps ≤ 0` |

## contract — Deployed contract integrity  *(mix of T1 and T3)*

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| CONTRACT-01 | T1 | `executor_contract` address has > 16 KB of bytecode | `eth_getCode` | length > 16384 |
| CONTRACT-02 | T1 | `owner()` returns `wallet.address` | `cast call owner()(address)` | matches |
| CONTRACT-03 | T1 | `balancerVault()` returns `trading.balancer_vault` | `cast call balancerVault()(address)` | matches |
| CONTRACT-04 | T1 | Source for currently deployed contract has all DEX_* constants | grep `ArbitrageExecutor.sol` for `DEX_PANCAKE_V3 = 8`, etc. | every constant present |
| CONTRACT-05 | T1 | `executor_supports_pancake_v3` flag agrees with deployed bytecode | check config + presence of `_swapPancakeV3` in source | flag true ⇔ source has handler |
| CONTRACT-06 | T3 | Synthetic 1-hop V3 cycle (USDC↔WETH on a deep pool) eth_call passes | smoketest constructs calldata, calls contract from owner | no revert |
| CONTRACT-07 | T3 | Synthetic 1-hop **PancakeV3** cycle eth_call passes (regression for the calldata bug) | smoketest | no revert |
| CONTRACT-08 | T3 | Synthetic 1-hop V2 (Sushi) cycle passes | smoketest | no revert |
| CONTRACT-09 | T3 | Synthetic 1-hop Camelot V2 cycle passes | smoketest | no revert |
| CONTRACT-10 | T3 | Synthetic 1-hop CamelotV3 / Algebra cycle passes | smoketest | no revert |
| CONTRACT-11 | T3 | Synthetic 1-hop RamsesV3 cycle passes | smoketest | no revert |
| CONTRACT-12 | T3 | Synthetic 1-hop UniswapV4 cycle passes | smoketest | no revert |
| CONTRACT-13 | T3 | Synthetic 1-hop Curve cycle passes | smoketest | no revert |
| CONTRACT-14 | T3 | Synthetic 2-hop cycle (V3 → V2 → start) passes | smoketest | no revert |
| CONTRACT-15 | T3 | Synthetic 3-hop cycle that touches three different DEX types passes | smoketest | no revert |
| CONTRACT-16 | T3 | `execute()` called from a non-owner reverts with "not owner" | smoketest, `From: 0x...0001` | reverts with the expected reason, not silently |
| CONTRACT-17 | T1 | New contract has `aavePool()` returning `trading.aave_pool_address` | `cast call aavePool()(address)` on executor_contract | matches config |
| CONTRACT-18 | T1 | New contract exposes `executeV3Flash` function selector in bytecode | scan bytecode for the selector (or try a static call with bad params and verify it doesn't hit INVALID opcode) | selector present |
| CONTRACT-19 | T1 | New contract exposes `executeAaveFlash` function selector in bytecode | same approach | selector present |
| CONTRACT-20 | T1 | New contract exposes `uniswapV3FlashCallback` function selector | same | present (required for V3 pools to call back) |
| CONTRACT-21 | T1 | New contract exposes `executeOperation` function selector | same | present (required for Aave to call back) |
| CONTRACT-V3Mini | T3 | Synthetic 2-hop pure-V3 cycle eth_calls `V3FlashMini.execute()` successfully. Uses `pickPairSharingToken(pools, v3DexSet, v3DexSet)` so the probe survives `test_mode=true` (restricted pool set). | smoketest | no revert; gas estimate within expected band |
| CONTRACT-V4Mini | T3 | Synthetic 2-hop pure-V4 cycle eth_calls `V4Mini.execute()` successfully. Uses `pickPairSharingToken(pools, [UniV4], [UniV4])` — greedy top-2-by-TVL was fragile under `test_mode`. Validates the V4Mini unlock callback (pm.swap + _settleCurrency + pm.take per hop), the hook gate (zero-hooks fast path), and the 67-byte packed hop layout. | smoketest | no revert; gas estimate ~300k |
| CONTRACT-MixedV3V4 | T3 | Synthetic 2-hop V4→V3 cycle eth_calls `MixedV3V4Executor.execute()` successfully. Confirms the unified unlock callback dispatches V4 hops via pm.swap and V3 hops via pool.swap (running the V3 swap callback INSIDE the V4 unlock) and that the bit-7 flag discriminator in the 67-byte hop payload is honoured. | smoketest (uses `pickPairSharingToken([UniV4], v3DexSet)`) | no revert; gas estimate ~450k |
| CONTRACT-HookGate | T3 | V4Mini rejects a PoolKey with a non-zero hook address that is NOT in `allowedHooks`. Catches accidental whitelist widening or bytecode/config drift. | smoketest eth_call with synthetic hook address `0x1111…`, expect revert `HookNotAllowed()` | reverts with the expected reason |
| DATA-GAS-LEDGER | T3 | Gas-estimate consistency: for every row in `contract_ledger.status='active'`, the `gas_estimate` column matches the hardcoded target the routing layer uses (V3FlashMini=380k, V4Mini=300k, MixedV3V4Executor=450k, ArbitrageExecutor=900k). Drift > 5% fails — indicates either the DB seed is stale or the routing constants were changed without updating the ledger. | smoketest `loadLedgerGasEstimates()` vs the expected map hardcoded in smoketest | every row within ±5% of target |
| DATA-GAS-MEASURE | T3 | On-chain `eth_estimateGas` for a representative cycle per contract lands within 20% of the ledger target. Over 50% drift = FAIL (contract logic drifted from the gas budget); 20-50% = WARN (worth investigating). Probe runs per contract (V3FlashMini / V4Mini / MixedV3V4Executor) against a constraint-satisfying synthetic cycle. | smoketest `eth_estimateGas` + comparison | delta ≤ 20% PASS, ≤ 50% WARN, > 50% FAIL |

## flash — Flash loan source health  *(T2-debug + T1)*

Tests added 2026-04-12 for the multi-source flash loan system.

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| FLASH-01 | T1 | Bot log shows `[flash] sources:` with all three counts > 0 | grep bot.log for latest `[flash] sources:` | balancer > 0, v3 > 0, aave > 0 |
| FLASH-02 | T1 | Aave `getReservesList()` returns ≥ 15 tokens | eth_call to `aave_pool_address` with selector `0xd1946dbc` | ≥ 15 addresses decoded |
| FLASH-03 | T1 | Aave Pool address in config matches what the contract returns | compare `cast call aavePool()` on executor_contract vs config `aave_pool_address` | match |
| FLASH-04 | T2-debug | Flash selector returns Balancer (priority 1) for USDC | query `/debug/pools` for a known Balancer-held token, verify bot would select Balancer | source=Balancer, fee=0 |
| FLASH-05 | T2-debug | Flash selector returns V3Flash for a token NOT in Balancer but in a V3 pool | find a token in V3 flash pool list but not in Balancer borrowable set; verify selection | source=V3Flash, fee=pool's fee tier |
| FLASH-06 | T1 | V3 flash pool for a given token is the cheapest fee tier available | for a token with multiple V3 pools at different fee tiers, verify the selected pool has the lowest fee | pool.fee == min of all available pools for that token |
| FLASH-07 | T1 | `sim_profit_bps` in recent `arb_observations` accounts for flash fee — i.e. for cycles using V3/Aave flash, the stored bps should be LOWER than the log-profit-derived bps by approximately the flash fee. | for rows where flash source != Balancer (once we log flash source): check `sim_profit_bps < (exp(log_profit)-1)*10000 - flash_fee_bps` | consistent |

## submit — Submission path readiness  *(mix of T1 chain checks and T2-debug for cache contents)*

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| SUB-01 | T1 | Wallet has nonzero ETH for gas | `eth_getBalance` for `wallet.address` | ≥ 0.0001 ETH |
| SUB-02 | T2-debug | Cached nonce matches `eth_getTransactionCount(latest)` | `/debug/executor` + RPC | exact match |
| SUB-03 | T2-debug | Cached nonce matches `eth_getTransactionCount(pending)` (no stuck txs) | same | match |
| SUB-04 | T2-debug | Cached baseFee within 1 block of `HeaderByNumber(latest)` | `/debug/executor` + RPC | delta ≤ 1 block |
| SUB-05 | T2-debug | Vault USDC balance cache matches Balancer Vault on-chain | `/debug/executor` + `balanceOf` | match within ±0.1% |
| SUB-06 | T1 | Sequencer RPC accepts `eth_chainId` | direct curl | returns `0xa4b1` |
| SUB-07 | T1 | `contract_min_profit_divisor` is sane | check config | == 1 (recommended) |

## health — Live health subsystem  *(T2-debug)*

| ID | Verify | How | Pass |
|---|---|---|---|
| HEALTH-01 | `TickDataAt` within `max_tick_age_sec` | `/debug/health` | age < threshold |
| HEALTH-02 | `MulticallAt` within `max_multicall_age_sec` | same | age < threshold |
| HEALTH-03 | `SwapEventAt` within `max_swap_age_sec` | same | age < threshold |
| HEALTH-04 | `CycleRebuildAt` within 2× `cycle_rebuild_secs` | same | age < threshold |
| HEALTH-05 | All `RPCRateLimit` counters at 0 | `/debug/health` → `RPCRateLimit` map | every value 0 |
| HEALTH-06 | bot.log has zero `panic` / `FATAL` / `runtime error` in last 1h | T1 grep | zero matches |
| HEALTH-07 | Swap listener channel depth peak in last 30 min ≤ 50 | `/debug/swap-listener` peak field, OR T1 grep `[swap] logs chan` | peak < 50 |
| HEALTH-08 | `[stats]` line shows non-zero `swaps=` and growing | T1 tail bot.log over 30s window | swaps > 0 and increases |
| HEALTH-09 | `MinProfitUSD` (dynamic) is sane | `/debug/health` | between $0.001 and $50 |
| HEALTH-10 | `LastGasPriceGwei` matches `eth_gasPrice` ±10% | `/debug/health` + RPC | within tolerance |

## tick — Tick management reliability  *(T3-ticktest + T2-debug)*

Added 2026-04-18 after a session-long audit revealed 11 reliability issues
in the tick bitmap / tick-liquidity fetch path. Every check in this
category reads from the live `/debug/tick-health` endpoint or calls out to
the `cmd/ticktest` binary. The binary exits non-zero on any FAIL so the
runner can map exit status → category status.

Invocation (single command runs every sub-check):
```
./ticktest -cat all -json | jq .
```

Or per sub-category:
```
./ticktest -cat pass       # last-pass outcome summary
./ticktest -cat eager      # eager refetch channel drop rate
./ticktest -cat skew       # arbitrum_rpc vs tick_data_rpc block skew
./ticktest -cat coverage   # cycle pools with tick out of fetched range
./ticktest -cat staleness  # pools exceeding max_tick_age_sec
./ticktest -cat failure    # pools stuck in fetch-failure loops
./ticktest -cat gate       # per-reason gate-reject counters
./ticktest -cat chain      # cross-check sampled pools against on-chain bitmap
./ticktest -cat invariants # structural checks on stored Pool tick fields
```

| ID | Verify | How | Pass |
|---|---|---|---|
| TICK-01 | Last FetchTickMaps pass had at least one successful pool | `ticktest -cat pass` → `pass.succeeded` | `pass_succeeded > 0` |
| TICK-02 | No algebra round-trip bitmap reconstruction mismatches | `ticktest -cat pass` → `pass.rt_mismatch` | `pass_rt_mismatch == 0` |
| TICK-03 | Pass failure rate under threshold | `ticktest -cat pass` → `pass.fail_rate` | `pass_failed/pass_total <= max-fail-pct` (default 2%) |
| TICK-04 | Eager refetch drop rate under threshold | `ticktest -cat eager` | `dropped/(dropped+enqueued) <= max-eager-drop-rate` (default 1%) |
| TICK-05 | Eager refetch channel not saturated | `/debug/tick-health` → `eager_refetch_chan_len` | `< cap - 1` |
| TICK-06 | tick_data_rpc vs arbitrum_rpc block skew under configured limit | `ticktest -cat skew` | `rpc_skew_blocks <= tick_rpc_max_skew_blocks` |
| TICK-07 | No cycle pools with current tick outside fetched word range | `ticktest -cat coverage` | `coverage_out_of_range/cycle_pools <= max-coverage-drift-pct` (5%) |
| TICK-08 | No cycle pools exceeding max_tick_age_sec | `ticktest -cat staleness` | `stale_by_age/cycle_pools <= max-stale-age-pct` (10%) |
| TICK-09 | No pools stuck in fetch-failure loop | `ticktest -cat failure` | `failure_loop_count/cycle_pools <= max-fail-loop-pct` (1%) |
| TICK-10 | Candidate reject counter for tick-fetch-failed is low | `/debug/tick-health` → `reject_tick_fetch_failed` | `0` in steady state |
| TICK-11 | On-chain bitmap matches bot's in-memory bitmap for a sample of pools | `ticktest -cat chain` | bot Ticks are a subset of chain bitmap for every sampled pool |
| TICK-12 | All V3-family fetched pools have TickSpacing > 0 AND TicksWordRadius > 0 | `ticktest -cat invariants` | `invariants.spacing` + `invariants.radius` PASS |
| TICK-13 | OK=true + Ticks=nil always has reason="empty-bitmap" | `ticktest -cat invariants` | `invariants.ok_no_reason` PASS |
| TICK-14 | /debug/tick-health endpoint responds | `curl /debug/tick-health` | HTTP 200 + decode OK |
| TICK-15 | Hook-whitelist-sync: for every live V4 pool whose `hooks != 0x0`, the address is in the executor contract's `allowedHooks` whitelist. Mismatch means cycles through that pool will pre-revert inside V4Mini / MixedV3V4Executor with `HookNotAllowed()`. Also flags whitelist-but-no-pool entries (dead entries the owner should prune). | `ticktest -cat hooks` walks `/debug/pools?dex=UniswapV4` collecting non-zero hook addresses, then eth_calls `allowedHooks(address)` on both V4Mini and MixedV3V4Executor. | zero missing from whitelist; count of dead whitelist entries reported but not FAIL |

**Pass criteria tuning**: the `max-*` thresholds are command-line flags on
`ticktest`. Tighten when the bot has been running long enough to reach
steady-state (typically >5 minutes after startup); loosen during the
warm-up window.


## forensics — Diagnostics & observation logging  *(mix)*

| ID | Tier | Verify | How | Pass |
|---|---|---|---|---|
| FOR-01 | T1 | `arbLogger` writes match log lines | grep `[arblog]` and compare timestamps to `arb_observations` rows | counts within ±5% |
| FOR-02 | T1 | `[diagnose]` lines appear after sim-rejects (when `diagnose_sim_rejects=true`) | grep `bot.log` for `[diagnose]` after each `[fast] sim-reject` | every recent sim-reject has a paired block |
| FOR-03 | T3 | `executor.Diagnose()` returns plausible per-hop output for a known-failing cycle | smoketest | every hop has non-zero `GoSimOutRaw` |
| FOR-04 | T1 | `bot_health` table being updated | `SELECT MAX(updated_at) FROM bot_health` | within 60s |
| FOR-05 | T1 | `our_trades` schema includes per-hop go_sim, revert_reason, pool_states_json | `PRAGMA table_info(our_trades)` | columns present |
| FOR-06 | T2-debug | `/debug/cycle-cache/stats` returns the same total as the latest `[cyclecache] rebuilt: N` log line | compare | exact match |

## invariants — Cross-field & cross-row consistency checks  *(all T1, pure SQL)*

This category was added 2026-04-12 after a class of composition bugs slipped
past every existing category. The bugs were not in any individual function —
they were in how fields of a single `arb_observations` row related to each
other, or how fields of a row related to on-chain reality. Every existing
category is component-level ("does function X return the right thing?")
which cannot catch bugs in the composition of multiple correct functions.

The canonical example: `optimalAmountIn` returned `bestIn` computed from a
fresh `tokenAmountFromUSD(bestUSD, ...)` call after the simulator had
already run with a different amountIn. Both values were individually
correct relative to the live registry state they saw, but they disagreed
by up to 100x because `usdPriceOf` took different fallback branches
between the two calls. The fix was to lift `bestIn` directly from
`bestResult.AmountIn`. The test that catches it is INV-01 below — a
one-line SQL check that fails the moment the ratio is inconsistent.

Design principles:
1. Every check is one SQL query (or one sqlite3/python3 block).
2. Every check must return **zero rows** to pass — the runner reports any
   non-zero row as FAIL with the offending row quoted.
3. Checks run against live `arb_observations` / `our_trades` / `pools` /
   `tokens` — no mocks, no fixtures.
4. When a check fails, the runner also reports a small sample (1-3 rows)
   and hypothesizes whether the bug is in the write path, the schema, or
   the compute.
5. Scope each query to the last hour (or since last bot restart) unless
   the pass criterion explicitly says otherwise — history may contain
   rows from before a given fix landed.

| ID | Verify | How | Pass |
|---|---|---|---|
| INV-01 | `sim_profit_wei * 10000 / amount_in_wei` matches stored `sim_profit_bps` within 2 bps. This is the direct consistency check between the simulator's input and its output — failures indicate the two fields were computed from different invocations (the 2026-04-12 `optimalAmountIn` bug). | `SELECT id, sim_profit_bps, amount_in_wei, sim_profit_wei FROM arb_observations WHERE observed_at >= strftime("%s","now","-1 hour") AND amount_in_wei != "" AND sim_profit_wei != "" AND CAST(amount_in_wei AS INTEGER) > 0 AND ABS(sim_profit_bps - (CAST(sim_profit_wei AS INTEGER) * 10000 / CAST(amount_in_wei AS INTEGER))) > 2`. NOTE the bigint multiplication may overflow SQLite's 64-bit int for 18-decimal tokens — use python3 for arbitrary-precision arithmetic instead. | zero rows |
| INV-02 | `profit_usd` reconciles with `sim_profit_wei × token_price_usd / 10^scale`. Scale comes from the first token in `tokens`. Catches unit mismatches (decimals, price fallback branches). | python3: for each row, parse `tokens` first entry, look up its decimals from the `tokens` table, compute expected = `int(sim_profit_wei) / 10^decimals * token_price_usd`, assert `abs(profit_usd - expected) / max(profit_usd, 0.01) < 0.02` (2%). | zero rows exceed 2% drift |
| INV-03 | `profit_pct` equals `(exp(log_profit) - 1) * 100` within 1e-4. These are derived from the same source and must match mathematically. Any drift indicates the derivation ran on mismatched inputs (e.g. log_profit from one cycle, profit_pct from another due to a loop-variable capture bug). | python3 query `arb_observations`, compute `(exp(log_profit) - 1) * 100 - profit_pct` per row | zero rows with abs diff > 1e-4 |
| INV-04 | `hops` equals the number of DEXes in `dexes`. Catches any mismatch between the cycle's shape and its stored metadata. | `SELECT id FROM arb_observations WHERE observed_at >= strftime("%s","now","-1 hour") AND hops != (LENGTH(dexes) - LENGTH(REPLACE(dexes,",","")) + 1)` | zero rows |
| INV-05 | `tokens` is a closed cycle: the first token equals the last token. Every cycle in the cycle cache MUST close; if this fails the cycle cache is producing invalid cycles or the logger is truncating the tokens list. | python3: split `tokens` on comma, assert `toks[0] == toks[-1]` | zero rows |
| INV-06 | For every row in `arb_observations` with `executed=1`, `tx_hash` is non-empty and parses as 0x-prefixed 32 bytes. Catches silent execution-log corruption. | `SELECT id FROM arb_observations WHERE executed=1 AND (tx_hash IS NULL OR tx_hash = "" OR LENGTH(tx_hash) != 66 OR tx_hash NOT LIKE "0x%")` | zero rows |
| INV-07 | For every row with `executed=0`, `reject_reason` is non-empty. Every rejection must have a recorded reason. | `SELECT id FROM arb_observations WHERE observed_at >= strftime("%s","now","-1 hour") AND executed=0 AND (reject_reason IS NULL OR reject_reason = "")` | zero rows |
| INV-08 | `token_price_usd > 0` for every row. A zero token price means `usdPriceOf` fell all the way through its lookup chain and returned 0 — cycles built on zero-priced tokens produce meaningless `profit_usd` values that pass the USD gate for the wrong reason. | `SELECT id, tokens FROM arb_observations WHERE observed_at >= strftime("%s","now","-1 hour") AND token_price_usd <= 0` | zero rows |
| INV-09 | `log_profit` is strictly positive for every recorded row. `ScoreCycle` already filters negative-lp cycles before they reach `trySubmitCandidate`; any row with `log_profit <= 0` means either the gate failed or the column was written from the wrong variable. | `SELECT id, log_profit FROM arb_observations WHERE observed_at >= strftime("%s","now","-1 hour") AND log_profit <= 0` | zero rows |
| INV-10 | `sim_profit_bps ≤ (exp(log_profit) - 1) * 10000 * 1.1` — the simulator's profit (after slippage) should not exceed the edge-weight score (pre-slippage) by more than 10%. Edge-weight is the upper bound because it uses marginal spot rates; simulator output is always lower due to non-linear slippage and fee curvature. A row where sim > lp × 1.1 means the two were computed from different amountIns or different pool states. | python3: for each row, compute `lp_bps = (exp(log_profit) - 1) * 10000`, assert `sim_profit_bps <= lp_bps * 1.1` | zero rows violate |
| INV-11 | `pool_states_json` parses as a JSON array with one entry per unique pool in the cycle. Entries must contain `address`, `dex`, `fee_bps`, `spot_rate`. Catches truncation, partial serialization, and Pool.Snapshot() field drops. | python3 parse each row's JSON, assert `len(pools_in_json) == len(set(pool addresses in the cycle's path))`, assert every entry has the 4 required keys | zero rows fail parse or schema |
| INV-12 | `hop_prices_json` parses as a JSON object mapping lowercase token addresses to positive USD prices, with one entry per unique token in the cycle. | python3 parse, verify each token in `tokens` list has a positive-price entry | zero rows fail |
| INV-13 | `our_trades` rows have `revert_reason` populated when `status='reverted'`. If a revert came with no reason extracted, forensics is broken. | `SELECT tx_hash FROM our_trades WHERE status='reverted' AND observed_at >= strftime("%s","now","-24 hours") AND (revert_reason IS NULL OR revert_reason = "")` | zero rows |
| INV-14 | `our_trades` rows have `hop_forensics_json` populated when `status='reverted'`. | same filter, check hop_forensics_json | zero rows |
| INV-15 | `our_trades` and `arb_observations` cross-reference: every `our_trades` tx_hash that was submitted (status in 'submitted','confirmed','reverted') has a corresponding `arb_observations` row with `executed=1` and matching tx_hash. | `SELECT t.tx_hash FROM our_trades t WHERE t.submitted_at >= strftime("%s","now","-24 hours") AND t.status IN ('submitted','confirmed','reverted') AND NOT EXISTS (SELECT 1 FROM arb_observations o WHERE o.tx_hash = t.tx_hash AND o.executed = 1)` | zero rows |
| INV-16 | Every pool referenced in recent `arb_observations.pools` exists in the live `pools` table. Catches orphaned/stale references. | python3: for each row, parse `pools` column (comma-separated addresses), `SELECT` each from pools table, assert all present | zero rows with missing pools |
| INV-17 | No `arb_observations` row has `sim_profit_bps > 10000` (>100% return on a flash loan cycle). This is the "absurd profit" guard — a real arb cycle never exceeds a few hundred bps on Arbitrum; 100%+ is a simulator math bug (wrong decimals scaling, wrong token in the ratio, or the 100x `tokenAmountFromUSD` inconsistency bug). | `SELECT id, sim_profit_bps, tokens, dexes FROM arb_observations WHERE observed_at >= strftime("%s","now","-24 hours") AND sim_profit_bps > 10000` | zero rows |
| INV-18 | No `arb_observations` row has `profit_usd > max_profit_usd_cap`. The cap should reject absurd rows BEFORE they hit the DB; any row above the cap means the check is bypassed or mis-ordered. | `SELECT id, profit_usd FROM arb_observations WHERE observed_at >= strftime("%s","now","-24 hours") AND profit_usd > (SELECT CAST(value AS REAL) FROM <cap_source>)`. Cap source: read `cfg.Strategy.MaxProfitUSDCap` from `/debug/config`. | zero rows |
| INV-19 | `bot_health` row count grows monotonically — no backward `updated_at` jumps. Catches clock skew or double-writes. | `SELECT COUNT(*) FROM bot_health WHERE updated_at < (SELECT updated_at FROM bot_health ORDER BY id DESC LIMIT 1 OFFSET 1)` | zero rows |
| INV-20 | For every disabled pool (`pools.disabled=1`), there are **no new** `arb_observations` rows touching it created after the disable timestamp. Once a pool is disabled the cycle cache should stop feeding it to the submit path within ~15s; observations touching it 30s after the disable indicate the cycle cache filter is broken. | Requires tracking disable timestamps (not currently stored — SKIPPED until a `disabled_at` column is added to `pools`). Flag as `SKIP: needs pools.disabled_at column`. | SKIP until column added |

### How the runner should handle this category

- **Every check is a single SQL query or a short python3 block.** No RPC calls, no smoketest binary, no bot restart. Runs in milliseconds against existing DB state.
- **Batch into one shell invocation.** Open one python3 heredoc that runs all 20 checks against `arb.db`, and report results in a single markdown table.
- **For each FAIL, include the offending row's `id`, a 1-line field dump, and a 1-sentence hypothesis** about whether the bug is in the write path (`trackTrade` / `arbLogger.Record`), the compute path (`optimalAmountIn` / `SimulateCycle`), or the schema.
- **Scope window**: default to the last hour so freshly-fixed bugs don't keep showing up as failures from historical rows. For INV-15 (cross-reference), use 24h because trades accumulate slowly.

---

## e2e — End-to-end profitability indicators  *(T1)*

| ID | Verify | How | Pass |
|---|---|---|---|
| E2E-01 | At least one observation since contract redeploy | `SELECT COUNT(*) FROM arb_observations WHERE observed_at > <redeploy_ts>` | > 0 (in busy windows) — note OK if quiet |
| E2E-02 | At least one *submission attempt* since redeploy | `SELECT COUNT(*) FROM arb_observations WHERE observed_at > <redeploy_ts> AND reject_reason NOT LIKE 'health:%' AND reject_reason NOT LIKE 'cooldown%'` | > 0 |
| E2E-03 | Health-rejection ratio in **last hour only** (not all-time) < 20%. The readiness barrier blocks all submissions for ~30s at startup, producing a burst of health rejects that inflates all-time ratio. Scoping to the last hour reflects steady-state behavior. Previous threshold (5%) was unrealistic. | `SELECT COUNT(*) FROM arb_observations WHERE observed_at >= strftime('%s','now','-1 hour') AND reject_reason LIKE 'health:%'` / total in same window | < 0.20 |
| E2E-04 | Sim-reject ratio in **last hour only** < 50%, **IF** ≥ 20 observations in the window. Below 20 observations, mark as PASS with note "insufficient data (N=X)" — ratios at small N are dominated by sampling noise (e.g. 4/6 during quiet 5AM UTC periods is meaningless). Most sim-rejects are state-drift reverts (opportunity moved between detection and eth_call), not math bugs — confirmed by smoketest at 0 bps for all DEXes. | same scoping, add N≥20 guard | < 0.50 or N < 20 |
| E2E-05 | At least one `executed=1` row in `our_trades` since redeploy | DB query | count > 0 |
| E2E-06 | `[fast] tx submitted` lines appearing in bot.log | grep | ≥ 1 in last 24h |
| E2E-07 | PancakeV3 share of hop-0 sim-rejects is < 30% (regression for the calldata bug fix) | breakdown of failing dex at hop 0 | PancakeV3 share < 0.30 |
| E2E-08 | Zero `health: no swap events` rows since the dedup + cap deploy | DB query against current bot start time | zero |
| E2E-09 | Average submission latency (cycle detection → tx broadcast) ≤ 200ms | grep `[timing] total=` lines | average < 200000μs |

---

## Notes for the runner

- **T1 tests run immediately** via curl/python3/grep/sqlite3 directly on this machine (host `arb1`) at `/home/arbitrator/go/arb-bot`. Both `sqlite3` CLI and `python3` work for DB queries.
- **T2-debug tests** assume INFRA-1 is implemented. Test the endpoint exists first; SKIP the entire category with reason `needs INFRA-1 (debug HTTP server)` if it doesn't.
- **T3-smoketest tests** assume INFRA-2 is implemented. SKIP with reason `needs INFRA-2 (cmd/smoketest binary)`.
- **For the contract eth_call tests**, the smoketest binary must construct calldata via the bot's own `internal.encodeHops` and `executeABI.Pack` so the round-trip is exactly what the live bot would send. This means exporting them from `internal`.
- **Always report results as a markdown table** with columns `ID | Status | Detail`. Group by category. After all categories, surface a "Missing infrastructure" section listing INFRA-N items that blocked SKIPPED tests.
- **For every FAIL**, quote the offending detail, hypothesize the root cause in one sentence, and propose the smallest fix (file:line if relevant). Do NOT auto-apply fixes unless the user says so.
- **Never run destructive operations**: no real submissions, no contract deploys, no DB writes, no killing of the running bot.
