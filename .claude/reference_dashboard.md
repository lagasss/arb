---
name: Web dashboard (arb-dashboard)
description: Full architecture, endpoints, tabs, and operational details for the arb-bot web dashboard
type: reference
originSessionId: 4183ad67-41e7-4f97-965b-49b59d2beebb
---
# What it is

Lightweight Go HTTP server that renders a single-page dashboard over the bot's data.
- **Source:** `/home/arbitrator/go/arb-bot/cmd/dashboard/`
- **Binary:** `/home/arbitrator/go/arb-bot/arb-dashboard` (rebuilt with `go build -o arb-dashboard ./cmd/dashboard/`)
- **Frontend:** single `static/index.html` embedded via `//go:embed`. No React / no framework. Vanilla JS + Tippy.js for tooltips.
- **Port:** `:8080` (public, via `DASHBOARD_PORT` env var). URL: `http://209.172.45.63:8080`
- **Runs as:** systemd user service `arb-dashboard.service` at `/home/seb/.config/systemd/user/arb-dashboard.service`. Controlled with `systemctl --user restart arb-dashboard`.
- **Linger must be enabled** (`sudo loginctl enable-linger seb`) or the service dies when the SSH session ends.

# Critical rebuild rule

HTML changes only take effect after `go build` because the HTML is embedded at compile time. Every HTML edit requires a full rebuild + service restart. Run directly on the server (host `arb1`):

```bash
cd /home/arbitrator/go/arb-bot && export PATH=$PATH:/usr/local/go/bin
go build -o arb-dashboard ./cmd/dashboard/
systemctl --user restart arb-dashboard
```

(Older notes that wrap this in `scp` / `ssh seb@209.172.45.63` are stale — sessions now run on the server itself, no SSH layer.)

# Data sources

1. **SQLite** at `/home/arbitrator/go/arb-bot/arb.db` (same file as the bot and arbscan — the canonical DB path is set in `config.yaml` via `db_path`). The dashboard opens read-write so it can run defensive `ALTER TABLE` migrations mirroring the bot's. Connection uses WAL mode with `SetMaxOpenConns(1)` — any handler that runs two concurrent queries will deadlock.
2. **Bot debug HTTP server** at `http://127.0.0.1:6060/debug/*` (loopback only). The dashboard proxies to it for live in-memory state (cycle cache). See `reference_debug_http.md`.
3. **`bot.log`** tailed for the Live Logs tab via SSE.
4. **`ps ax`** for the Processes tab — matches by `comm` field, not args.

# Environment variables

- `ARB_DB_PATH` (default `/home/arbitrator/go/arb-bot/arb.db`)
- `ARB_LOG_FILE` (default `/tmp/arb-bot.log` — the systemd unit overrides this to `/home/arbitrator/go/arb-bot/bot.log`)
- `ARB_CONFIG_PATH` (default `/home/arbitrator/go/arb-bot/config.yaml`)
- `DASHBOARD_PORT` (default `8080`)
- `BOT_DEBUG_URL` (default `http://127.0.0.1:6060`) — where to proxy `/api/cycles` and `/api/cycle-stats`

# API endpoints (dashboard backend)

Read-only unless noted.

| Endpoint | Purpose |
|---|---|
| `/api/stats` | Trade counts, success rate, est. profit, 24h observations |
| `/api/trades` | `our_trades` rows, with server-side resolved `hop_prices_json` (addr→symbol via `tokens` table) |
| `/api/trade?id=N` | Full forensics for a single trade (pool states, sim profit, revert reason) |
| `/api/observations` | `arb_observations` — also resolves `hop_prices_json` server-side |
| `/api/competitors` | `competitor_arbs` — supports `?sender=`, `?arb_type=`, `?profitable=0/1` filters. Returns `comparison_result` + `cycle_in_memory`. |
| `/api/competitor-comparison?id=N` | Full classification detail for one competitor row |
| `/api/pools` | UNION of `pools` + `v4_pools`. Filters: `?dex=`, `?search=`, `?sort=`, `?addresses=` (comma-sep for the "click Ticks → filter" navigation) |
| `/api/config` | Parses `config.yaml`, returns as JSON |
| `/api/processes` | Runs `ps` and matches known bot-related processes (`bot`, `arb-dashboard`, `arbscan`, `nitro`). Resolves absolute paths via `/proc/<pid>/exe`. |
| `/api/logs` | Last N lines of `bot.log` |
| `/api/logs/stream` | SSE live tail via `tail -f bot.log` |
| `/api/health` | Reads `bot_health` table: subsystem freshness, per-pool tick status, per-RPC probe state |
| `/api/cycles` | **Proxy** to bot's `/debug/cycles` — pass-through for `limit`, `token`, `pool`, `hops`, `token1..token5` |
| `/api/cycle-stats` | **Proxy** to bot's `/debug/cycle-cache/stats` |
| `/api/query` | **POST**, SQL editor for the DB tab. Blocks `DROP`, `ALTER`, `CREATE`, `ATTACH`, `PRAGMA`, `TRUNCATE` (schema-destructive) |

# Dashboard tabs

In nav order:
1. **Overview** — stat cards + recent trades
2. **Our Trades** — `our_trades` with status badges clickable to open the forensics modal
3. **Opportunities** — `arb_observations` with filters (all/executed/not-executed/rejected)
4. **Competitors** — `competitor_arbs` with filters; columns include `Result` (comparison_result badge), `In Cache` (cycle_in_memory badge)
5. **Live Logs** — SSE-streamed `bot.log` with auto-scroll + level color coding
6. **Config** — `config.yaml` parsed into sections
7. **Processes** — PID / CPU / memory / uptime / absolute binary path for all known processes
8. **Pools** — UNION of `pools` + `v4_pools` with search, DEX filter, sort, tracked-ticks filter
9. **Cycles** — live proxy to bot's in-memory cycle cache. Filters: 5 token-position boxes, pool address, hops, limit. Total/matched counts from `/debug/cycle-cache/stats`.
10. **DB** — free-form SQL editor with quick-select buttons for each table. Per-row delete button when the result has an `id` column.

# Header elements (always visible)

- **ARB BOT · running** dot — driven by `s.last_24h_observations > 0` heuristic
- **Auto-refresh** checkbox — 15s interval for stats + active-tab data. Clock updates every 1s.
- **Ticks X / Y** indicator — clickable, jumps to Pools tab filtered by the tracked V3/V4 pool addresses. X = pools with non-empty tick data, Y = total tracked.
- **Multicall / Swaps / Cycles** freshness dots (green/yellow/red from `bot_health` timestamps)
- **RPC dots**: Live Tracking RPC / L1 RPC / Tx Sequencer RPC / Sim RPC / Ticks RPC. Green = probe ok, yellow = ok + recent 429 (5-min window), red = probe failed, grey = not configured. Hover for latency + last block + rate-limit count.

# Key JS conventions

- **`bindTippy()`** called after every `innerHTML` replacement that contains `.tok` elements — wires up price tooltips.
- **`tokens(str, hopPricesJSON, hopsJSON)`** and **`compPath(pathStr, hopPricesJSON, hopsJSON)`** render token names with per-hop USD price tooltips.
- **`buildSymbolPrices(hopPricesJSON, hopsJSON)`** normalizes address-keyed prices to symbol-keyed (required because `competitor_arbs.hop_prices_json` uses addresses but `arb_observations.hop_prices_json` uses symbols; `our_trades.hop_prices_json` is resolved server-side).
- **Status badges are clickable** — `statusBadge(status, id)` opens the forensics modal via `openForensics(id)`.

# Things that commonly go wrong

1. **HTML changes not visible** → rebuild required. Confirm with `curl http://localhost:8080/ | grep <new-element-id>`.
2. **SCP into wrong dir** — always use absolute `/home/arbitrator/go/arb-bot/cmd/dashboard/static/index.html` as the SCP destination, NOT `/home/arbitrator/go/arb-bot/cmd/dashboard/` (which flattens both files into the same directory).
3. **Dashboard crashes silently** when the bot opens the DB at the same time — `SetMaxOpenConns(1)` means any handler that holds an open `rows.Next()` cursor AND tries another query deadlocks. The `addrToSym` map is now pre-loaded once at startup + refreshed every 5 min in a background goroutine to avoid this.
4. **Browser cache** — after a rebuild, hard refresh (`Ctrl+Shift+R`) or incognito. `F5` is not enough because the embedded files serve with ETags.
5. **Bot not restarted** — any dashboard feature that relies on new bot columns or `/debug/*` query params silently falls back to whatever the old running binary supports. Always check `stat /proc/$(pgrep -x bot)/exe` after rebuilding the bot.

# Building new features — checklist

- Does the feature need bot data? Add to `bot_health` table (for small state) or expose via `/debug/*` (for large / live state).
- Does it need a new DB column? Add the `ALTER TABLE` migration in **both** `internal/db.go` (bot side) and `cmd/dashboard/main.go` (dashboard side) so either process can start first.
- Does it need a new JS helper? Always call `bindTippy()` after any `innerHTML` replacement that may contain `.tok` elements.
- If it touches the HTML, rebuild the dashboard binary. SCP the single-file `index.html` with full absolute path.
- If it touches the bot, it doesn't affect the dashboard binary — but the bot itself needs to be restarted (`pkill -x bot` then the start command from `reference_bot_start.md`) and linger must be on.
