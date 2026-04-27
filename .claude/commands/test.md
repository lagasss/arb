---
description: Run the arb-bot end-to-end test plan (full or scoped to a category)
argument-hint: [category | all]
---

You are running the arb-bot end-to-end test plan defined in project memory at `project_test_plan.md`.

`$ARGUMENTS` selects the scope. Valid values:

- `all` — run every category in order
- One of: `rpc`, `pools`, `state`, `routers`, `cycles`, `sim`, `contract`, `submit`, `health`, `db`, `forensics`, `e2e`, `tick`
- Empty — list the categories with their test counts and ask which to run; do **not** start running anything until the user picks one or says `all`

## How to run

1. **Read the test plan** from `/root/.claude/projects/-home-arbitrator-go/memory/project_test_plan.md`. It is the source of truth for test IDs, what each verifies, the concrete query/command, and the pass criteria.

2. **Honor the tier markers:**
   - **T1** — execute now via SSH to `seb@209.172.45.63` (password file `/home/arbitrator/.ssh/seb_pass`). Working dir on the server is `/home/arbitrator/go/arb-bot`. Use `curl` for JSON-RPC, `python3` for SQLite (the project uses `python3`, not the `sqlite3` CLI), and `grep`/`awk` for log inspection.
   - **T2** — needs a dashboard endpoint. If the required endpoint doesn't exist yet, mark `SKIPPED` with reason `needs dashboard endpoint <name>`. Don't try to fake it.
   - **T3** — needs a Go test binary. If it doesn't exist, mark `SKIPPED` with reason `needs cmd/smoketest binary`. On the first invocation that hits a T3 test, also surface a one-paragraph spec for the missing binary so the user knows what to build.

3. **Batch independent T1 checks** into single SSH sessions where possible — don't open one SSH connection per test. RPC checks for several endpoints can share a curl loop; DB queries can share a single python3 heredoc.

4. **Never run destructive operations**: no real `eth_sendRawTransaction`, no contract deploys, no DB writes, no `kill` of the running bot, no log rotation. Tests must be read-only.

5. **Report format** — at the end of each category, emit a markdown table:

   ```
   ## <category> — N tests
   | ID | Status | Detail |
   |---|---|---|
   | RPC-01 | ✅ PASS | block 451450123, 87ms |
   | RPC-07 | ❌ FAIL | 3 × 429 from chainstack in last 5 min |
   | STATE-01 | ⏭ SKIP | needs dashboard endpoint /api/pools-state |
   ```

   After all selected categories, emit a **summary table** with totals: `category | passed | failed | skipped`.

6. **For every FAIL**, after the table:
   - Quote the failing detail
   - Hypothesize the root cause in one sentence
   - Propose the smallest concrete fix (file:line if relevant)
   - Do NOT auto-apply any fix unless the user explicitly says so

7. **For every SKIP that is blocked on missing infrastructure** (T2 dashboard endpoint or T3 test binary), accumulate them and at the end of the run produce a "Missing infrastructure" section listing what would need to be built to unblock those tests.

## Important constraints

- The bot is **live on the server** (`pgrep -f '^./bot$'`). Read state, never modify it. If a test would require restarting or reconfiguring the bot, mark it SKIPPED with reason `requires bot restart — out of scope for /test`.
- The project's auto-memory in `MEMORY.md` lists hard rules (server-only, python3-not-sqlite3, never compile locally, etc.). Honor them.
- If the test plan has been updated since you last read it, re-read it before running — it is the source of truth.
- If `$ARGUMENTS` doesn't match a valid category, list the valid categories and ask the user to pick one. Don't guess.
