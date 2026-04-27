---
name: Server Security TODOs
description: Security hardening tasks to do before going live with real money
type: project
---

Security tasks deferred until later — do these before putting real capital on the bot.

**Why:** Server is in early setup phase. These are important but not blocking development. Must be done before mainnet live trading.

**How to apply:** Remind Seb to address these before Phase 9 (mainnet live).

## Critical — Do Before Live Trading

- [ ] **Disable SSH password auth** — brute-force risk. Run:
  ```bash
  sudo sed -i 's/PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
  sudo systemctl restart sshd
  ```
  (Confirm SSH key login works first — already confirmed via WSL key)

- [ ] **Change seb's password** — `Agadou11!` has been used in shell commands and may be in server bash history. Run: `passwd seb`

- [ ] **Enable firewall (UFW)**:
  ```bash
  sudo ufw allow OpenSSH
  sudo ufw enable
  ```

- [ ] **Use a dedicated hot wallet** — never use main wallet for the bot. Hot wallet holds gas ETH only. Profits withdraw to cold wallet periodically.

- [ ] **Wallet separation — 3 wallets:**
  - Cold wallet: holds profits, never touches bot
  - Hot wallet: gas ETH only, signs bot transactions
  - Contract owner: deployer address, keep offline after deploy

- [ ] **Audit ArbitrageExecutor.sol** before putting real money through it — onlyOwner and Balancer vault check are correct but get a professional audit before significant capital.

## Important — Before Live Trading

- [ ] **Verify .env and sftp.json never get committed** — both in .gitignore already but double-check before creating git repo

## Lower Priority

- [ ] **Install fail2ban** — auto-blocks IPs after failed SSH attempts:
  ```bash
  sudo apt install fail2ban
  ```

- [ ] **Enable automatic security updates**:
  ```bash
  sudo apt install unattended-upgrades
  ```

- [ ] **Docker socket risk** — seb in docker group = effectively root. Acceptable for single-user server, but be aware.
