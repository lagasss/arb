---
description: Manage V4 hook whitelist (list, classify, approve delta hooks, inspect)
argument-hint: [list | approve <addr> <reviewer-note> | reject <addr> | inspect <addr>]
---

You are operating the V4 hook whitelist for the arb-bot's lean fleet (V4Mini, MixedV3V4Executor). The whitelist is stored in:

- **SQLite** `hook_registry` table on the server (`/home/arbitrator/go/arb-bot/arb.db`) — classifier output, classification status, bytecode hash
- **On-chain** `HookRegistry` contract at `trading.hook_registry` in config.yaml — the contract V4Mini + MixedV3V4Executor actually read at swap time


## Commands

### `list`
Show every row in `hook_registry`: address, classification, on_chain_status, permission_bits (hex), has_delta_flag, pushed_at age. Flag rows where `classification='safe' AND on_chain_status='pending'` — those are candidates the sync loop hasn't pushed yet, probably because hook_sync_interval_sec=0 or the wallet hasn't been funded for gas.

Query:
```sql
SELECT address, classification, on_chain_status,
       printf('0x%04x', permission_bits) AS perms,
       has_delta_flag,
       CASE WHEN pushed_at=0 THEN 'never' ELSE datetime(pushed_at,'unixepoch') END AS last_push,
       substr(reviewer_note,1,60) AS note
FROM hook_registry ORDER BY classification, address;
```

### `approve <0xHOOK> <note>`
Promote a delta-rewriting hook to allowed after a manual source review. Two-step process:

1. **Audit first**: `cast code <addr> --rpc-url $ARBITRUM_RPC` — read the bytecode, cross-reference with Arbiscan source if verified. Confirm the hook does NOT re-route tokens out of PoolManager during swap (watch for afterSwap custom `CurrencyDelta.settle`/`take` calls that don't pair).
2. **Only then**, broadcast `HookRegistry.approveDeltaHook(hook, permissions, bytecodeHash, note)` from the owner wallet. Use the cached permission_bits + bytecode_hash from `hook_registry` to prevent mismatch. Use `cast send`:
   ```
   cast send <hook_registry> 'approveDeltaHook(address,uint16,bytes32,string)' <hook> <perms> <hash> "$note" \
     --rpc-url $ARBITRUM_RPC --private-key $ARB_BOT_PRIVKEY
   ```
3. After the tx confirms, run: `UPDATE hook_registry SET on_chain_status='manual', reviewer_note=?, pushed_at=strftime('%s','now'), updated_at=strftime('%s','now') WHERE address=?`.

Refuse to broadcast if:
- The hook's `on_chain_status` is already `allowed` or `manual`
- `has_delta_flag=0` (then use `setHook` via the auto-sync loop, not this override)
- The note is empty

### `reject <0xHOOK>`
Explicit reject: push `setHook(hook, false, ...)` on-chain and set `on_chain_status='rejected'`. Use when a previously-allowed hook turned out to be unsafe (e.g. upgraded implementation, exploit discovered).

### `inspect <0xHOOK>`
Verbose per-hook dump:
- Classifier verdict + permission flag decode (e.g. `beforeSwap | afterSwap | beforeSwapReturnDelta`)
- Bytecode size + hash
- On-chain `HookRegistry.statusOf(hook)` result (live)
- Count of V4 pools in the live `/debug/pools` registry referencing this hook
- Any `arb_observations` rows whose `pools` column contains a V4 pool with this hook (indicates we saw trading activity through it)

## Hard rules

- **Read-only by default**: `list` and `inspect` never write. `approve` and `reject` broadcast txs and require explicit user confirmation before `cast send`.
- **Never bypass the audit step in `approve`**: dumping bytecode is not enough; the user has to say "go" after seeing the classifier + bytecode summary.
- **Only the deployer wallet** can call `HookRegistry.setHook` / `approveDeltaHook`. If `$ARB_BOT_PRIVKEY` isn't set on the server session, abort with an error — don't try alternative signers.
- **Never run against the bot's main wallet without explicit user ack** if the hook wallet is shared with the trading wallet (in practice it is — we only have one deployer key).
