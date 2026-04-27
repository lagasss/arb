---
name: Config.yaml structure and key interactions
description: Map of config.yaml blocks, what each one drives, and how they interact (especially test_mode, major, pinned_pools, tokens)
type: reference
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
`config.yaml` layout at `/home/arbitrator/go/arb-bot/config.yaml`:

### Top-level
- `arbitrum_rpc`, `arbscan_rpc`, `l1_rpc`, `uniswap_api_key` — RPC endpoints
- `pause_tracking` / `pause_trading` — two kill switches; tracking stops scoring entirely, trading stops only submission (candidates still log to arb_observations with `eth_call` validation)
- `test_mode` — single bool, used with `major:` block below
- `db_path` — SQLite WAL DB shared by bot + arbscan + dashboard

### `major:` block (the known-safe arbitrage universe)
- `major.tokens` — array of `{address, symbol, decimals}`. Major tokens get loaded FIRST into the registry as bootstrap
- `major.pools` — pool addresses pinned on-chain (bypass TVL filters)
- **When `test_mode: true`**: ONLY `major.tokens` + `major.pools` are active. `tokens:` and `pinned_pools:` are filtered out. Cycle cache whitelist = `major.tokens`. Used to conserve RPC quota while Nitro node is syncing.
- **When `test_mode: false`**: `major.*` is MERGED with `tokens:` + `pools.pinned_pools:`.

### Merge logic (in `mergeMajorIntoConfig`, bot.go)
Runs after `applyStrategyDefaults`, before `NewBot` wires everything:
1. Dedup-merge `major.tokens` into `cfg.Tokens`
2. Dedup-merge `major.pools` into `cfg.Pools.PinnedPools`
3. If `test_mode: true`, filter both lists DOWN to major-only

### `cex_dex:` block
CEX-DEX split-route bot (cmd/cexdex). **Planned for later** — SplitArb.sol not yet deployed. `enabled: false` keeps it dormant.

### `wallet:`
Bot wallet address. Private key from `ARB_BOT_PRIVKEY` env var.

### `pools:` block (non-major)
- `absolute_min_tvl_usd`, `min_tick_count`, `high_fee_tier_min_tvl_usd`, `min_volume_tvl_ratio` — composite quality filter applied at registry add time, cycle cache rebuild, and hourly prune
- `max_hops` — cycle length cap
- `sanity_max_tvl_usd` — subgraph TVL cap
- `subgraph.*` — subgraph seeding config (enabled, API key, per-DEX subgraph IDs, `min_tvl_usd`, `limit_per_dex`, `refresh_hours`)
- `pinned_pools:` — non-major pinned pools (exotic tokens like PENDLE/MAGIC/GNS/RDNT/CAPX/GMX). Loaded only when `test_mode: false`.

### `strategy:` block
All cycle-cache + scoring + LP-floor tunables:
- `min_price_delta`, `lp_sanity_cap`, `max_profit_usd_cap`
- `tvl_trade_cap_fraction`, `shallow_pool_tvl_usd`, `shallow_pool_trade_fraction`
- `submit_cooldown_secs`, `cycle_rebuild_secs`, `max_dex_per_dest`, `max_edges_per_node`, `pool_staleness_secs`
- `arb_log_retention_hours`, `arb_log_max_rows`
- `skip_sim_above_usd`, `skip_sim_above_bps` — fast-path bypass
- `min_sim_profit_bps` — minimum margin to submit (currently 2)
- `slippage_bps` — per-hop amountOutMin tolerance (currently 200)
- `fast_eval_max_submits_per_swap`, `block_scoring_interval` (1 = every 250ms)
- `gas_safety_mult`, `min_cycle_lp_bps`
- `dynamic_lp_floor.*` — per-hop base bps by DEX category + TVL scaling + volatility class + gas multiplier + latency overhead
- `camelot_v3_default_fee_bps` — fallback for Algebra dynamic fee

### `trading:` block
- `sequencer_rpc`, `simulation_rpc`, `tick_data_rpc` — separate endpoints per workload
- `min_profit_usd`, `flash_loan_amount_usd`
- `balancer_vault`, `aave_pool_address` — flash sources
- `executor_contract` (main ArbitrageExecutor `0x73aF65fe…`), `executor_v3_mini` (V3FlashMini `0x33347Af4…`)
- `executor_supports_pancake_v3` — must stay paired with the address above
- `debug_http_port` — 6060 loopback for `/debug/*`
- `tick_pool_max_age_sec`, `pool_state_max_age_sec` — per-pool freshness gates
- `tick_eager_refetch_spacings`, `tick_eager_refetch_cooldown_ms` — eager V3 tick re-fetch on big swaps

### `factories:`
DEX factory addresses. Single source of truth is now `internal.Factories` map — this config block is legacy/reference only.

### `tokens:` block (non-major)
Non-major token metadata (GMX, LINK, AAVE, USDe, sUSDe, aArb*). Merged with `major.tokens` at load. aTokens are here for `yield_monitoring`'s `wrapper_address`, not for direct trading.

### `yield_monitoring:`
Chainlink oracle + Aave aToken rate divergence strategy. **Planned for later**.

## Key interactions I must remember

1. **`test_mode` filters AT LOAD TIME, not at runtime.** Requires bot restart to flip.
2. **Major is always included.** `major:` is the floor — exotic tokens/pools ADD to it when test_mode is off.
3. **Adding a token**: if it's trading-grade high-TVL, put it in `major.tokens`. If it's yield-tracking-only (aTokens) or minor-cap, put it in `tokens:`.
4. **Adding a pool**: same rule — major (high-TVL, major pair) → `major.pools`. Exotic → `pinned_pools:`.
5. **`yield_monitoring.pairs` references tokens** — dropping an aToken from the config breaks that pair.
6. **Removed blocks**: `monitoring:` (dead influxdb/telegram), `test_mode_tokens:` (replaced by `major.tokens`).
