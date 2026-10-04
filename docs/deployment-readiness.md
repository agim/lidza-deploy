# Deployment readiness review — 2026-10-04

## Decision

The current source is suitable for a controlled deployment on a dedicated supported Linux server. It is **not yet verified for unattended production use**. Local checks pass, but the Go vulnerability-database check and real provider/fresh-host acceptance below remain incomplete. This is a tested deployment candidate, not a guarantee of production operation.

The source pins Līdza v0.1.75. The existing public v0.2.1 binary release predates this dependency update and the security fixes below. Use the current source installer for this candidate; do not assume the old release bundle includes these fixes. No new binary release was published as part of this review.

## Findings fixed

- Updating an app's settings or disabling its webhook could return its saved preview environment values. Both responses now omit those values; regression checks verify that the encrypted stored configuration still retains them.
- The app log endpoint returned runtime output without the known-value masking already used for task/build output. It now masks currently configured environment values and passwords extracted from database URLs. An authenticated log-route regression verifies that useful diagnostics survive while the fixture credentials disappear. This does not identify arbitrary, encoded or formerly configured secrets: apps must still avoid logging credentials.
- The setup wizard's GitHub registration button could overlap the callback address. Its layout now gives both their own space, stacks account fields on mobile, and keeps the claim page's background across the viewport. A browser assertion checks the button/callback geometry.
- Removed unreachable duplicate Caddy assertions in the installer test. The reachable Caddy validation/adaptation assertions run with TEST_CADDY=1.
- Resolved static-analysis style findings concerning HTTP status constants and error messages. No checks were disabled.

## Validation

| Area | Evidence | Result |
| --- | --- | --- |
| Go tests | Uncached `go test -race -count=1` with TEST_DOCKER=1, TEST_CADDY=1 and dedicated TEST_DATABASE_URL | Passed |
| Go analysis | `go vet ./...`, Staticcheck v0.8.1 | Passed |
| Deployment | Real local Docker apps; readiness gating, healthy-release preservation, reload, rollback and removal | Passed |
| Domains | DNS route/proof rejection, domain edits/retries, trusted local Caddy certificate issuance | Passed locally; public ACME unverified |
| Data | Local PostgreSQL creation, external connection fixtures, named attachments/primary switch, rolling backup retention, backup/download/restore and source preservation | Passed locally |
| Operations | Workers/schedules, maintenance, preview isolation/cleanup, metrics and alert/queue fixtures | Passed locally |
| Access | Framework authentication, same-origin mutations, role restrictions, active-session revocation, audit persistence/failure handling and secret omission | Passed |
| Installation | Source/curl-pipe/bundle staging, required FQDN, repeatability, private file modes, checksums and Caddy adaptation/validation | Passed in staging; live package/systemd installation unverified |
| Upgrade | Isolated healthy upgrade, failed-health rollback, checksum and unsafe-version rejection | Passed in fixtures; real systemd upgrade unverified |
| Builds | Linux amd64 and arm64 bundles | Built; arm64 cross-compiled, not executed |
| Browser | Console, databases, features, teams and first-run setup suites in Chromium | Passed |
| First-run durability | Real managed PostgreSQL, operator login, restart, setup endpoint closure | Passed |
| Setup layout | Public installer-origin/callback checks, desktop/mobile screenshots, no horizontal mobile overflow | Passed; public address uses a local browser network fixture, not a public TLS test |
| Browser dependencies | `npm audit` for tests/browser | Zero reported vulnerabilities |
| Go vulnerabilities | `govulncheck` | Incomplete: environment denied vuln.go.dev |

Statement coverage is 56.4% for the Go module (agent 64.3%, control 49.7%, onboarding 57.2%, GitHub client 81.8%). Browser/installer subprocess coverage is not included. This is not exhaustive path coverage or an independent penetration test.

## Production acceptance still required

1. Install on a fresh dedicated Debian 12/13 or Ubuntu 22.04/24.04 systemd VM using a real FQDN. Verify signed package installation, service users/permissions, Docker access, startup after reboot and public firewall/ports 80/443. Stage tests do not exercise apt or systemd.
2. Complete setup at the public HTTPS address. Deploy two apps with different FQDNs, then change one app's FQDN. Verify waiting-DNS status, issuance after DNS reaches the host, trusted public HTTPS and continued routing of the other app. Local CA tests do not prove public ACME or cloud NAT/firewall behavior.
3. Authorize a real GitHub OAuth application, clone a private repository, create its webhook and push an update. Verify recorded commit, automatic deployment, invalid signature rejection, delivery retry/redelivery and PR preview creation/cleanup. GitHub organization approval and repository hook permissions can affect this flow.
4. Configure the intended S3-compatible destination and SMTP provider. Verify an off-site backup object and download/integrity, a delivered failure alert and a recovery email. Local signed-request/outbox fixtures do not prove provider delivery.
5. Complete `govulncheck` after allowing vuln.go.dev in the cloud environment settings. The destination was added to the saved draft while preserving existing explicit hosts; saving the draft does not apply the change to this machine. Run: `source scripts/env.sh; go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.
6. On the dedicated VM, verify a real managed upgrade and reboot. Save and test recovery of the control-panel PostgreSQL data, its encrypted configuration/master key, and each agent's encrypted state/master key. GUI app-database backups do not automatically back up the control panel itself.

## Operating limits to account for

- One shared fleet workspace with admin/deployer/viewer access; no separate tenant isolation. Docker administrators can read container environment values.
- Apps must have root project metadata (`lidza.json`) and a normal Līdza entrypoint. The agent generates missing Docker deployment files; app migrations remain the app's responsibility. Runtime variables are not build secrets; private build-time module/package dependencies are not supported by the clone-token workflow.
- App containers have a read-only filesystem, UID/GID 65532, 512 MiB memory and one CPU. Persistent file uploads need external storage or another explicitly supported design.
- Current and rollback web containers both keep running. Avoid in-process singleton/background jobs in the web entrypoint; use managed worker tasks and account for migration compatibility. Rollback does not restore database contents.
- External PostgreSQL backup clients support server versions up to 17. Backup objects are capped at 5 GiB; automatic restore currently reads local copies, not S3. Normal off-site retention needs bucket lifecycle rules.
- SMTP/queued alerts and scheduled dispatch require the control panel and its database. Use an independent external monitor for the control panel itself.
- Review build-cache and crash-orphan resource cleanup periodically; the platform does not supply high availability or an automatic disaster recovery process.

## Setup interview

The first page asks for the server's one-time setup key. After unlocking, the current GUI shows a single numbered form: operator account, managed/external control-panel PostgreSQL, the installer-selected address, optional GitHub OAuth configuration, and optional remote-agent connection. The installer-selected FQDN is read-only. A colocated agent is paired automatically. GitHub and extra servers can be configured later. Finishing validates dependencies, saves encrypted configuration, starts the application and redirects to login; setup stays closed after restart.

S3 storage, SMTP alerts and per-app database/backup choices are configured after login in Integrations and the app/database forms, not in this initial interview.

To reproduce screenshots without capturing credentials:

```sh
./scripts/dev-prepare.sh
SETUP_SCREENSHOT_DIR="$PWD/.local/screenshots/setup-readiness" node tests/browser/setup.cjs
```

The test uses isolated temporary installations, creates/removes its own PostgreSQL container, and captures blank fields before credentials are entered. Files 05–09 show the public-origin layout; the public browser route is mapped to an isolated local backend. This verifies UI behavior without claiming public DNS/TLS verification.
