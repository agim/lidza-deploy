# Implementation status

The application pins Līdza v0.1.81. Authentication, encrypted credentials, private GitHub connections, durable job dispatch, interval/daily scheduling, mail alerts, S3 signing, scoped memberships and durable audit storage use released framework APIs.

## Available

- Public/private app deployment and redeployment, readiness-gated activation, previous-release rollback, runtime environment edits, named databases and automatic reloads.
- Multiple FQDNs per server, DNS verification/retries, proactive Caddy certificates, automatic renewal and status in the GUI.
- Local/external PostgreSQL connections, one rolling pre-deployment backup per database, separate scheduled/manual retention, local backup download and streaming S3 copies.
- Restore into a new database with integrity checks, source preservation and attachment/reload after success.
- CPU/load, memory, agent/Docker disk space, PostgreSQL sizes, low-resource/overdue-backup alerts and recovery notices.
- Deployment commit subjects, durations, failure details, changed setting names and bounded captured build logs.
- Hosted-application Errors dashboard with automatic agent console collection, encrypted retry outbox, authenticated central storage/deduplication, grouping/search and retained reports during agent outages; optional app analytics. See [Application errors](application-errors.md).
- App-specific maintenance pages.
- Signed pull-request preview deployment and cleanup, separate environment and optional private database.
- Background worker start/restart/disable/logs, current-image/environment reconciliation, durable interval/daily command dispatch, overlap prevention and failure alerts.
- Official agent release checks, checksum/manifest validation, atomic binary upgrades, co-located control-panel upgrade and failed-health rollback.
- Required-FQDN installer, first-run setup wizard, guided GitHub App connection/webhooks using installation tokens, storage and email configuration, and the approved Signal design using released Līdza colors.

## Team access and audit

[Līdza #28](https://github.com/agim/lidza/issues/28) shipped in v0.1.72. The control panel uses its released scoped role and audit APIs for one fleet workspace, with administrator/deployer/viewer membership, immediate revocation for existing sessions, protected installation-owner access, a paginated team GUI, and a durable audit view. Mutation intent must persist before an agent call; result failures are surfaced. No passwords, environment values or request bodies are recorded.

## Validation and limits

Race-enabled Go tests and vet cover the product and released framework integrations. Real local Docker/PostgreSQL/Caddy tests cover deployment/reload/rollback, DNS verification routes, proactive trusted local certificate issuance, backups/restore, preserved source data, worker execution/replacement and preview resource cleanup. Isolated upgrade fixtures verify successful upgrade, failed-health rollback, checksum rejection and version validation. Installer staging verifies root/source-build and bundled paths without modifying the shared host. Browser checks cover console layouts, CRUD, backup/download/restore, named connections, maintenance, tasks, preview configuration with secret omission, server metrics and upgrade availability, team account creation, role changes, audit secrecy and revocation of live sessions.

Live public ACME, GitHub consent/private clone and actual PR webhooks, real S3/SMTP providers and a systemd upgrade on a dedicated production VM remain external acceptance checks. Local test CAs, fake GitHub endpoints and isolated service mocks do not establish those provider/host results. See [Operations](operations.md), [Domains and HTTPS](domains-ssl.md), and [Databases and backups](databases-backups.md) for requirements and recovery steps.

## Guided GitHub App setup

New installs use **Connect GitHub → approve app → Choose repositories** after local login. The product uses framework one-use state, roles, sealed credentials, audit and HMAC webhook primitives; GitHub-specific manifest/JWT/installation/token handling lives here as directed by [Līdza #29](https://github.com/agim/lidza/issues/29#issuecomment-5987110069). Agent checkout tickets obtain fresh scoped credentials after queue delays. GitHub Apps are the primary GUI connection flow; configured OAuth connections remain supported. See [Guided GitHub connection](github-app-plan.md) for permissions, organization ownership, repository attachment and validation limits. Live GitHub acceptance remains unverified; this is not included in the existing v0.2.1 binaries.
