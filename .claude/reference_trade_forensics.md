---
name: Trade forensics in SQLite
description: Where to find per-trade forensics data including hop-level revert details
type: reference
---

Trade forensics are stored in `/home/arbitrator/go/arb-bot/arb.db`, table `our_trades`.

Key columns for debugging reverts:
- `status` — 'reverted', 'confirmed', 'pending'
- `revert_reason` — e.g. "execution reverted: hop 0: swap reverted"
- `hop_forensics_json` — JSON with per-hop go_sim output vs min required, and `re_sim_error` from eth_call re-simulation
- `pool_states_json` — snapshot of each pool's sqrtPriceX96/reserves/spot_rate at decision time
- `sim_profit_bps` — estimated profit in basis points at decision time
- `profit_usd_est` — estimated USD profit

Query last N reverted trades:
```python
python3 -c "
import sqlite3, json
conn = sqlite3.connect('/home/arbitrator/go/arb-bot/arb.db')
conn.row_factory = sqlite3.Row
cur = conn.cursor()
cur.execute(\"SELECT tx_hash, submitted_at, hops, dexes, profit_usd_est, sim_profit_bps, revert_reason, hop_forensics_json, pool_states_json FROM our_trades WHERE status='reverted' ORDER BY submitted_at DESC LIMIT 3\")
for r in cur.fetchall():
    print(dict(r))
"
```

Both `sqlite3` CLI (`/usr/bin/sqlite3`, v3.37.2) and `python3` work. Use python3 when you need JSON parsing or scripted output; use the `sqlite3` CLI for ad-hoc queries.
