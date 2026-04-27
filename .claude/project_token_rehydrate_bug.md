---
name: Token registry rehydrate fix — silent pool drop on restart
description: The bot's startup token registry was seeded only from cfg.Tokens (~17 tokens), so LoadPools silently dropped every persisted pool whose token0/token1 had been discovered live in a previous session. Fixed 2026-04-11 by adding DB.LoadTokens and rehydrating at startup.
type: project
---

**Symptom:** After every bot restart, the in-memory pool count was ~50–100 lower than the count of `disabled=0 AND is_dead=0` rows in the `pools` table. The bot was dropping 50+ persisted pools per restart, including some `verified=1` pools — visible only via the new `/debug/pools` endpoint vs SQLite cross-check.

**Root cause:** [`db.go LoadPools`](arb-bot/internal/db.go#L631-L635) silently `continue`s any row whose `token0` or `token1` isn't in the in-memory `TokenRegistry`. The registry was seeded only from `cfg.Tokens` (17 tokens) — but the DB had **1269 tokens** that the bot had discovered live in previous sessions (via `discovery.go`, `compwatcher.go`, `subgraph.go`, `resolvepool.go`). On restart, those 1252 discovered tokens were forgotten, so any pool whose `token0` or `token1` was among them got dropped.

**Why:** `UpsertToken` persists tokens to the DB, but no corresponding `LoadTokens` ever read them back. The lifecycle was *write-only*: discoveries piled up in the DB across restarts but never seeded the next session's registry.

**How to apply:** The fix is in two parts:

1. [`db.go`](arb-bot/internal/db.go) — added `func (d *DB) LoadTokens() ([]*Token, error)` that returns every row in `tokens WHERE is_junk=0`.

2. [`bot.go`](arb-bot/internal/bot.go) — after `cfg.Tokens` are seeded into the registry, call `db.LoadTokens()` and add any token that isn't already in the registry (config takes precedence on conflict, junk addresses are skipped). Also logs `[bot] token registry: N from config + M rehydrated from DB = K total`.

**Validation 2026-04-11:**

| Metric | Before fix | After fix | Δ |
|---|---|---|---|
| Token registry size | 17 | **1267** | +1250 |
| Pools loaded at warm-start | 213 | **1615** | +1402 |
| Pools in memory total | 1567 | **1673** | +106 |
| STATE-08 truly missing | 48 | **5** | −43 |
| Cycle cache `pools_covered` | 113 | **251** | +138 (more than doubled) |

**Remaining 5 missing pools** (resolved 2026-04-11 in follow-up):
- 2 UniV3 pools `0xb0f6ca40...a5d6e3526c95572b` and `0xfa7191d2...0e633200` — typos in `pinned_pools` config, no bytecode on chain. The real ARB/USDC 0.05% address is `0xb0f6ca40...c5f179b8403cdcf8` and the real WBTC/USDT 0.05% is `0x5969efdde3cf5c0d9a88ae51e47d721096a97203`. Both fixed in [config.yaml](arb-bot/config.yaml).
- 3 BalancerW pools `0x64abeae...`, `0x49b2de7d...`, `0xcc65a812...` — referenced 2 token addresses (`0x040d1edc...` and `0x1509706a...`) that were in `pools.token0/token1` but never persisted to `tokens` table. Subgraph fetch path's `resolveSubgraphToken` adds tokens to memory but only `PoolStore.sync()` persists them, and there's a window where pools can land in DB without their tokens. Resolved by:
  - Making `db.UpsertPool` atomically upsert both tokens before the pool itself (eliminates the gap going forward)
  - Adding a `resolveMissingToken` callback to `db.LoadPools` that fetches missing token metadata via `FetchTokenMeta` on the bot's RPC client at warm-start time (self-heals existing orphans)

After both fixes, the next restart logged `[db] LoadPools: resolved 10 previously-orphaned token(s) on the fly` and the pool count rose from 1615 → 1706 → 1721.

**LoadPools deadlock landmine** discovered while implementing the resolver: the bot's SQLite is configured with `MaxOpenConns=1`. Calling `UpsertToken` (an Exec) from inside the `for rows.Next()` loop of LoadPools deadlocks forever because the Rows cursor holds the only connection. **Always drain rows into a slice first, then close the cursor, then run any Exec calls.** Fixed in `db.go LoadPools` by collecting raw rows into a `[]rawPoolRow` slice and processing afterward.

**Latent bug surfaced (separate, deferred):** **`fee_ppm=0` for sub-1bps V3 fee tiers.** Several deep V3 pools (e.g., the $1B PancakeV3 USDC/USDT 0.01% pool) have `fee_bps=1, fee_ppm=0` in the bot's memory. The bot's `v3Fee()` falls back to `fee_bps * 100 = 100 ppm`, which happens to be the right tier — but the on-chain UniV3 QuoterV2 probe in the smoketest sees the dispatched fee=100 mismatch the expected fee for some pools and reverts. Worth a focused look at the multicall fee-loading path that's storing `fee_ppm=0` in the first place.

**RamsesV3 state issue** turned out to be a startup-warmup transient, not a bug. After 90+ seconds of multicall passes all 13 RamsesV3 pools have populated `sqrt_price_x96` and `liquidity`. The earlier observation was sampled too soon after restart. Test plan STATE-02 was updated to allow a warmup window.
