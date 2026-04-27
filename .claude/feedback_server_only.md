---
name: Work directly on the server (no SSH wrapper)
description: Sessions run on host arb1 itself; bot/arbscan/dashboard all live and run here at /home/arbitrator/go/arb-bot
type: feedback
---

This machine (`arb1`, user `seb`) IS the production server. Build, run, and test directly — no `sshpass`, no `rsync`, no `scp` wrappers needed.

**Why:** Earlier sessions ran from a remote workstation with no Go toolchain, so everything went through `sshpass -p ... ssh seb@209.172.45.63`. That layer is gone now — the harness runs on the server itself.

**How to apply:**
- Always `cd /home/arbitrator/go/arb-bot` (or use absolute paths) — bot binary, config.yaml, and arb.db all live there.
- Bot must still always be started from `/home/arbitrator/go/arb-bot`, never from `/tmp` or `/home/seb`.
- Skip any `sshpass` / remote-host commands you see in older memories or CLAUDE.md — they were for the old setup.
- The bot/arbscan/dashboard processes are the same systemd-user / nohup setup as before; only the access path changed.
