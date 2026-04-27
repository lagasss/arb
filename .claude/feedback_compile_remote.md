---
name: Compile locally on the server
description: Go is installed on this machine — build directly, no scp/ssh wrapper
type: feedback
---

Compile directly with `go build` from `/home/arbitrator/go/arb-bot`. No scp, no ssh.

**Why:** Sessions now run on the server itself (host `arb1`, user `seb`) instead of a remote workstation. Go is installed at `/usr/local/go/bin`. The previous "scp + ssh go build" workflow was for when the dev machine had no Go toolchain — that's no longer the case.

**How to apply:** From the repo root, just `go build -o <bin> ./cmd/<name>/`. PATH may need `export PATH=$PATH:/usr/local/go/bin` if not already set. The CLAUDE.md "No local compile / Deploy to server" rules are stale wording from the remote-workstation era — confirm with the user before re-applying them.
