---
description: Investigate a competitor_arbs row — classify why we missed it, judge legitimacy, surface actionable gaps
argument-hint: <competitor_arbs id>
---

You are analyzing competitor_arbs row `$ARGUMENTS` to answer two questions the user always asks in this flow:

1. **Why didn't we pick up this cycle?** — root-cause the `comparison_result` concretely (which pool/token/cycle-gate rejected it, not a generic answer).
2. **Is the trade legitimate, or a honeypot / one-shot?** — especially when `profit_usd` is unusually high.

If `$ARGUMENTS` is empty, ask for an id before doing anything.

## How to run

Server: SSH as `seb@209.172.45.63` (password `Agadou11!`, remote CWD `/home/arbitrator/go/arb-bot`). DB is `arb.db` — use `python3` (not `sqlite3` CLI).

Run independent queries in parallel tool calls when possible. Never modify state (no DB writes, no bot restarts, no destructive actions).

### Step 1 — Fetch the row

Pull every column from `competitor_arbs WHERE id=$ARGUMENTS`. Key fields to extract:
- `path_str`, `hops_json` (the cycle and per-hop pools/tokens/amounts)
- `profit_usd`, `net_usd`, `gas_used`, `tx_hash`
- `sender`, `bot_contract`
- `comparison_result`, `comparison_detail` (our own classification — trust it as the starting point, don't recompute)

### Step 2 — Branch on `comparison_result`

- **`missing_pool`** — read `comparison_detail.pool_status`; find every entry with `known=false` or `reason != "ok"`. For each unseen pool, check:
  - Is it in `v4_pools` or `v4_pool_tokens` (V4 discovery gap — see `project_v4_pool_stranded_bug.md`)?
  - Does the pool's token pair involve a long-tail token we don't index? Probe the token via `eth_call` with `name()/symbol()/decimals()/totalSupply()` (selectors `0x06fdde03 / 0x95d89b41 / 0x313ce567 / 0x18160ddd`).
  - Is the TVL below our seeding threshold?
- **`cycle_not_cached`** — all pools known, but DFS didn't emit the cycle. Check each hop pool in `/debug/pools` for:
  - Active in-memory presence, `verified`, `disabled` flags.
  - Recent `[quality-reject]` entries in `bot.log` (grep `-a` for binary-safe). Common rejectors: `tick_count < floor` (see `project_tick_count_bypass_tvl.md`), `volume/tvl < floor` (see `project_dead_pool_tvl_exempt.md`), `absolute_min_tvl_usd`.
  - If all pools pass, the cycle may be pruned by `max_edges_per_node` or `max_hops` — note this as a hypothesis, don't claim without evidence.
- **`accepted` / other** — we did see it; explain what happened in our pipeline (sim reject, gas reject, not-profitable, submission failure). Check `arb_observations` or recent `bot.log` for the cycle.

### Step 3 — Legitimacy check (ALWAYS for profit > $1, optional otherwise)

The user specifically asks about this whenever `profit_usd` is above a typical arb ($0.05–$0.20). Run these checks:

1. **On-chain status**: `eth_getTransactionReceipt` on `tx_hash`. Status 0x1 confirms the competitor actually banked the profit (no revert).
2. **Same-block co-ocurrence**: query `competitor_arbs WHERE block_number = <same> AND id != <target>`. If other independent bots captured profit on the same dislocation without reverting, it's legitimate (honeypots trap late arrivals).
3. **Token probe** for any non-major token in the path: fetch `name/symbol/decimals/totalSupply` via RPC. Flag as suspicious if:
   - `totalSupply` is very small (< 100k tokens) yet pool TVL is high (spoofing setup).
   - Symbol matches a well-known token but contract address isn't the canonical one (spoofing).
   - Token isn't in our `tokens` table AND isn't in `v4_pool_tokens` AND has no activity in other `competitor_arbs` rows.
4. **Sender / bot_contract history**: query `competitor_arbs WHERE sender=<s>` and `WHERE bot_contract=<b>`. A bot with only 1–3 trades total is either brand-new or a one-shot; an established bot with dozens of consistent extractions is more likely legitimate.

### Step 4 — Report

Emit a concise report in this structure (no headers/sections for small cases):

- **Verdict**: `legitimate / honeypot-suspect / ambiguous` in one line with the strongest supporting evidence.
- **Why we missed it**: one paragraph, concrete. Name the specific pool/filter/config param that's blocking.
- **Competitors on the same block** (if any): one-liner list of ids + net_usd.
- **Token flags** (if any non-major tokens): name, symbol, totalSupply, our registry status.
- **Recommended action** (if any): the smallest concrete fix — config change, filter threshold, new pool seed, new token entry. Do NOT implement; just propose. Cite the file:line of the code that would change. If the fix is meaningful enough to warrant a pre-live blocker, say so explicitly so we can flag it in memory.

Keep the final response ≤ 250 words unless the user follows up.

## Important constraints

- **Use `grep -a`** on `bot.log` — it's often detected as binary and plain `grep` returns "binary file matches" without lines.
- **Don't recompute our classification.** `comparison_result` and `comparison_detail` are our own code's verdict from `competitor_compare.go`; trust them. Your job is to *explain* that verdict, not replace it.
- **Never restart the bot or modify config.** If the investigation reveals a fix, propose it; the user decides when to apply.
- **Absolute dates only.** When writing memory or citing events, convert relative dates to `YYYY-MM-DD`.
- **Check memory first** — `project_v4_pool_stranded_bug.md`, `project_tick_count_bypass_tvl.md`, `project_dead_pool_tvl_exempt.md`, `project_token_rehydrate_bug.md` and the `reference_*.md` files already contain the patterns this skill diagnoses. Reuse the diagnosis vocabulary so findings are consistent across runs.
