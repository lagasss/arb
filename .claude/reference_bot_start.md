---
name: Bot start command
description: How to start the arb bot on the server
type: reference
---

Always start the bot with:

```
ARB_BOT_PRIVKEY=9e040b096238466c47ede2bbd314bda5732e28be9c40c022910eb4f61f78c52c nohup /home/arbitrator/go/arb-bot/bot >> /home/arbitrator/go/arb-bot/bot.log 2>&1 & disown; echo "PID=$!"
```

Always start arbscan with:

```
nohup /home/arbitrator/go/arb-bot/arbscan >> /home/arbitrator/go/arb-bot/arbscan.log 2>&1 & disown
```

**Always use absolute paths** for both binaries and log files — never use `./bot` or `./arbscan`. SSH heredocs and `nohup` calls don't reliably preserve `cd` state, and using relative paths leads to "command not found" or starting from `/home/seb` instead of the project directory.

**Why:** The private key is passed via env var (not stored in config). `nohup` + `disown` keeps it running after SSH session ends. Logs go to `bot.log`/`arbscan.log` at the absolute path so they're consistent regardless of cwd.
