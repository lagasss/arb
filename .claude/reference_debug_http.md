---
name: Bot debug HTTP introspection server
description: Read-only loopback HTTP server inside the bot process exposing live in-memory state (pool reserves, V3 ticks, cycles, executor caches, health) for tests and inspection
type: reference
---

The bot exposes a read-only HTTP introspection server on `127.0.0.1:6060` (loopback only — never bind 0.0.0.0). Implementation in [`internal/debug_http.go`](arb-bot/internal/debug_http.go), started from `Bot.Run()` when `trading.debug_http_port != 0` in config.yaml.

**Why it exists:** Volatile state (`Pool.Reserve0/1`, `SqrtPriceX96`, `Liquidity`, `TicksUpdatedAt`, `Health.*` atomics, `Executor` caches) lives in the running bot process's RAM and is intentionally not persisted to SQLite (`db.go:430-432`). The dashboard is a separate process that only sees SQLite, so it can't observe this state. The debug server is the only way to ask "what does the bot believe right now".

**Endpoints** (curl from the server only):

| Endpoint | Returns |
|---|---|
| `GET /debug/` | Root index listing every endpoint |
| `GET /debug/health` | Health atomic timestamps + ages, RPCRateLimit map, MinProfitUSD, LastGasPriceGwei, configured thresholds |
| `GET /debug/pools?dex=&limit=&offset=&token=` | Full Pool registry snapshot. Supports filters by `dex` (string), `token` (matches token0 or token1), pagination via `limit`/`offset` |
| `GET /debug/pools/{address}` | Single pool snapshot or 404 |
| `GET /debug/cycles?limit=&token=&pool=` | Cycle cache sample with full edge detail (default limit=50) |
| `GET /debug/cycle-cache/stats` | total_cycles, pools_covered, last_rebuild_at, top-20 pools by cycle count |
| `GET /debug/executor` | Contract addr, wallet, chain id, vault, cached nonce, cached baseFee, cached vault balances per token |
| `GET /debug/swap-listener` | swaps_total, max_logs_depth, pools_subscribed |
| `GET /debug/config` | Effective strategy + trading config (no RPC URLs / private keys) |

**How to use from tests:** the `/test` slash command's T2-debug tests rely on these endpoints. Always filter for `verified=true` when checking pool state — the bot loads unverified pools metadata-only without populating reserves. For tick-freshness checks, walk `/debug/cycles` to find which V3 pools are *actually* in the cycle cache (the bot only fetches ticks for cycle pools to bound RPC budget — checking all verified V3 pools is wrong because ~75% are intentionally not tick-tracked).

**Safety:** all handlers are GET-only, never mutate state, take read locks (`Pool.mu`, `Health.RPCRateLimitMu`, `Executor.vaultBalMu`/`baseFeeMu`) when reading shared fields. Never bind to anything other than `127.0.0.1` — these endpoints reveal pool inventory and wallet caches.
