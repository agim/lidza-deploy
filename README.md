# Līdza Deploy

A deployment agent and a separate web control panel built with Līdza v0.1.75. One agent hosts multiple Līdza apps, each on its own FQDN with automatic HTTPS through Caddy. The control panel can run on that server or on another host.

The MonolithCMS agent was copied/adapted into this repository; the original repository was not changed. See [provenance](docs/provenance.md).

The GUI now includes database restore into a new connection, host health and alerts, deployment build history, maintenance mode, PR previews, workers/scheduled jobs, and managed agent upgrades. See [Operations and recovery](docs/operations.md). Administrator/deployer/viewer team access and durable audit logging use the framework APIs released in [Līdza v0.1.72 (#28)](https://github.com/agim/lidza/issues/28).

See the [deployment readiness review](docs/deployment-readiness.md) for the latest test evidence, fixed findings and production acceptance checklist.

## Working features

- GitHub public and private repositories, selected branches, manual deploy and redeploy.
- Līdza-managed team sessions and GitHub OAuth connections. No provider or agent credentials in the browser.
- GitHub webhook creation/update, signed payload verification, repository/branch filtering, duplicate-delivery protection, and persistent dispatch retries through Līdza jobs.
- Docker release builds, non-root application containers, CPU/memory/process limits, loopback-only application ports, and `/readyz` checks before traffic switches.
- Separate host routing per app; DNS verification and proactive Caddy certificate issuance on deployment and FQDN edits, with periodic retries. See [Domains and automatic HTTPS](docs/domains-ssl.md).
- Deployment history, runtime logs, and rollback to the previous healthy release.
- The selected **Signal** interface uses Līdza’s released dark brand palette. Standalone HTML previews remain available.
- GUI server inventory, branch/domain settings, write-only environment edits, and retryable app removal.

This supports one fleet team with admin, deployer and viewer roles, with one active control-panel process and one agent process per deployment host. Server inventory is managed in the GUI and stored encrypted; a private JSON file can seed initial setup. It is not yet full Hatchbox feature parity: cloud-server provisioning, scaling, and zero-downtime database migrations are future product work.

## Installation and first startup

Use a dedicated **Debian 12/13 or Ubuntu 22.04/24.04** server with systemd, sudo/root access, and a public IP. Allow inbound TCP ports **80 and 443** for application HTTPS and your normal SSH port. The installer installs Git, Docker Engine, Caddy, and the bundled services. A Go toolchain is needed only on the machine building the bundle.

### 1. Run the installer

First point `deploy.example.com` at your server’s public IP and allow ports 80/443. From a checkout on that server:

```sh
sudo sh install.sh --fqdn deploy.example.com
```

Or install the published `main` directly:

```sh
curl -fsSL https://raw.githubusercontent.com/agim/lidza-deploy/main/install.sh | sudo sh -s -- --fqdn deploy.example.com
```

The root installer builds the agent and control panel, installs their prerequisites, pairs the local agent, and starts the services. It reuses Go 1.27+ or downloads temporary Go 1.27.1 with a pinned, verified SHA-256 checksum. You do not need to build or transfer a bundle manually. Docker, Caddy, Git and systemd services are installed by the bundled host installer. The default includes the control panel; PostgreSQL and other settings are configured in the browser. **`--fqdn` is required.** Omitting it stops before downloads, builds or host changes. Use `--fqdn localhost` explicitly for a local/tunnel installation; localhost is never selected implicitly. With `--agent-only`, the FQDN names the agent API instead.

To inspect the host or planned actions:

```sh
sh install.sh --check
sh install.sh --fqdn deploy.example.com --plan
```

To install only the agent on a separate hosting server:

```sh
sudo sh install.sh --agent-only --fqdn agent.example.com --email ops@example.com
```

The downloaded equivalent is `curl -fsSL https://raw.githubusercontent.com/agim/lidza-deploy/main/install.sh | sudo sh -s -- --agent-only --fqdn agent.example.com --email ops@example.com`.

### 2. Installer options and repeat runs

`--version REF` selects a branch, tag or commit. `--stage /absolute/path` produces an inspectable installation tree without changing packages or services. `--bundle /path/to/lidza-agent-linux-amd64.tar.gz` uses a prebuilt archive and verifies its adjacent `.sha256` file. Both amd64 and arm64 hosts are detected automatically.

Re-running preserves existing credentials and configuration. The control panel listens internally on loopback port 3000. Caddy exposes the chosen public GUI FQDN over HTTPS immediately, including the setup wizard. Reinstall with the same FQDN; changing an existing installation’s canonical hostname requires an explicit migration. An existing unmanaged Caddy configuration is rejected; use a dedicated host. The full option list is `sh install.sh --help`.

For offline transfer or building on another machine, `./scripts/package-agent.sh amd64` (or `arm64`) still creates a bundle in `dist/`; see the [deployment runbook](docs/deployment.md).

### 3. Complete the browser setup wizard

For a public FQDN, open **`https://deploy.example.com`** directly. Caddy obtains the certificate automatically when DNS and ports 80/443 are ready. No SSH tunnel is needed.

Read the one-time ownership key in an SSH session or your cloud provider's server console:

```sh
sudo cat /var/lib/lidza-control/setup-token
```

For an explicitly local installation (`sudo sh install.sh --fqdn localhost`), open `http://localhost:3000`. To access that local installation remotely, keep this tunnel open from your computer:

```sh
ssh -N -L 3000:127.0.0.1:3000 user@control-host
```

Paste it into **Unlock setup**. Keep this key private. The wizard configures:

1. Your operator email and password.
2. Managed PostgreSQL on this host, or an existing PostgreSQL database.
3. The GUI address supplied to the installer, prefilled and locked. Setup verifies public HTTPS; DNS and Caddy routing are already configured before you reach the wizard.
4. GitHub OAuth client credentials. The wizard links to GitHub registration and supplies the callback URL. Configure these now if you need private repositories or automatic deployments.
5. Agent pairing. The local agent is already paired; remote agents can be imported using their private connection file.

Choose **Validate and finish setup**, then sign in at your public control-panel URL. Setup checks the database, agent and public HTTPS before completion; failed attempts can be retried. Settings are encrypted, and completed setup stays closed after restart. No database or OAuth environment-file editing is required for this installation path.

[View the setup wizard](design-previews/setup-wizard.png).

### 4. Deploy your first application

In the control panel:

1. Open **Integrations → Authorize GitHub** for private repositories and webhook auto-deploy. Public repositories can deploy without GitHub authorization.
2. Create an application with its GitHub `owner/repository`, branch, server and FQDN.
3. Point that app's DNS A/AAAA records to its hosting server; include IPv6 only when it is reachable.
4. Enter per-app secrets in the masked **Runtime environment** fields, or later in **Settings → Environment variables**. Values are encrypted on the agent and never returned to the browser. Save, then deploy to apply changes.
5. Choose **Deploy**. The agent builds the app, waits for readiness, and switches its domain to the healthy release. Enable **Auto-deploy** to create the GitHub push webhook.

Each Līdza project must contain `lidza.json`. An absent root Dockerfile is generated using the released framework scaffolding; an existing Dockerfile is preserved. Līdza supplies `/readyz`. Each registered app receives its own hostname and automatically managed HTTPS certificate.

### Separate hosting servers

Run the root installer on each hosting server. Configure the agent hostname's DNS first, then install:

```sh
sudo sh install.sh --agent-only --fqdn agent.example.com --email ops@example.com
```

This installs the hosting agent with an authenticated HTTPS management endpoint. Import `/etc/lidza-agent/connection.json` through the control panel's wizard or **Servers → Connect server**; retrieve it privately over SSH. The file contains the agent token. Ports 9090 and 8081 remain internal. You can connect multiple servers and deploy multiple apps on each.

### Application databases, backups and alerts

The New application flow lets you create local PostgreSQL, attach an existing managed PostgreSQL database, or choose no database. It attaches `DATABASE_URL` automatically, offers backup frequency and local retention, and supports S3-compatible off-site copies. **Databases & backups** provides manual backups, downloads and retained data after app removal. **Integrations** contains storage and SMTP settings.

Apps can attach multiple databases using named environment variables and switch `DATABASE_URL` to another database on the same server. Saving FQDN/environment settings or changing an attachment automatically reloads the deployed image after readiness checks. Pre-deployment backups are enabled by default, including before reloads; failures retain the current release. The option can be changed in app Settings. See [runtime and database switching](docs/databases-backups.md).

Database provisioning, backup/restore, S3 streaming, alert queueing and the GUI database flow are tested. Līdza v0.1.71 fixes concurrent mail reconfiguration ([#27](https://github.com/agim/lidza/issues/27)); the original race reproduction and queued-delivery regression now pass. See [database and backup instructions](docs/databases-backups.md) for limits, retention, recovery, and alert behavior.

### Checks, backups and current limitation

Check service startup on the server with:

```sh
sudo systemctl status lidza-agent lidza-control caddy
sudo journalctl -u lidza-control -n 50 --no-pager
```

Back up PostgreSQL, `/var/lib/lidza-control`, `/var/lib/lidza-agent` including their encryption keys, and Caddy's certificate storage. Staged installer checks and browser/database/deployment tests pass; live installation on a fresh supported VM and real GitHub/public ACME remain external acceptance checks.

GitHub credentials can be configured during first setup or later in Integrations without restarting the control panel. Līdza v0.1.71 fixes [#26](https://github.com/agim/lidza/issues/26), and the onboarding regression now passes.

See the [deployment runbook](docs/deployment.md) for security boundaries, recovery, rollback and removal semantics. [Līdza #24](https://github.com/agim/lidza/issues/24) confirms the wizard uses existing framework APIs; [#25](https://github.com/agim/lidza/issues/25) shipped deployment generation improvements in v0.1.70.

## Development

Requires Go 1.27.1+, Git, Docker Engine, and Python 3 for the local setup helper. The web assets need no build step.

```sh
./scripts/dev-prepare.sh
# In separate terminals:
./scripts/dev-agent.sh
./scripts/dev-web.sh
```

The helper creates private local configuration in `.local/`, builds both binaries, and starts a PostgreSQL development container on loopback port 55432. It preserves existing local values. The operator email/password are in `.local/dev.env`; do not commit or share that file. The development database uses trust authentication on a loopback-only port; use authenticated PostgreSQL in production.

The web process listens on port 3000. Its root page opens the selected Signal workspace; `/designs.html` retains the design gallery and `/console.html` opens the live workspace and `/login.html` signs in through Līdza. Preview URLs use `/console.html?demo=1&design=studio`, `terminal`, or `fleet`. Preview actions never make deployment API calls. Agent API: port 9090. Internal application ingress: port 8081.

GitHub and public DNS are optional for local development. Configure them using the [production runbook](docs/deployment.md) before enabling private repository access and external push webhooks.

## Tests

```sh
source scripts/env.sh  # activates the prepared cloud toolchain, when present
./scripts/test.sh
```

This runs race-enabled Go tests, PostgreSQL auth/queue integration, real Docker deployments of two Līdza apps, local Caddy certificate issuance with full TLS verification, database backup/restore, S3 streaming, alert queue persistence, and `go vet`. The test database is separate from the development database.

With the development agent and web process running:

```sh
npm ci --prefix tests/browser
npm test --prefix tests/browser
node tests/browser/databases.cjs
node tests/browser/features.cjs
node tests/browser/teams.cjs
```

Browser checks need Chromium (`CHROMIUM_BIN`, defaults to `/usr/bin/chromium`). They cover all designs, demo create/deploy/logs, mobile overflow, real login, partial fleet history, and real app create/edit/remove through the local agent. Screenshots are saved under `.local/screenshots/`.

The Docker smoke test uses a local Git fixture, then real Docker build/run and real Līdza readiness checks. Private token transport and webhook APIs use fake Git/GitHub endpoints. Local TLS uses Caddy's test CA and verifies its chain and hostname; it does not prove public ACME/DNS configuration or future renewal. Live GitHub consent, a real private-repository deployment, and public ACME issuance require production configuration and remain external acceptance checks.

## Layout

- `cmd/agent`: host agent; `internal/agent`: deployment lifecycle and application ingress.
- `cmd/web`: Līdza app using its db/auth/jobs packs.
- `internal/control`: fleet authorization, application configuration, and dispatch.
- `internal/providers/github`: GitHub repository and webhook calls; OAuth is provided by Līdza.
- `web/static`: embedded HTML/CSS/JavaScript, including the design gallery.
- `deploy`: example configuration, Caddyfile, and systemd units.

[Framework-first rule](AGENTS.md): reuse Līdza; file reusable gaps upstream and wait for a released capability. [Issue #23](https://github.com/agim/lidza/issues/23) shipped in v0.1.61 and is integrated here.

Database GUI check (with the development agent/control running): `node tests/browser/databases.cjs`. It creates and cleans up an isolated hosting agent and database.
