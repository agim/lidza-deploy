# Implementation status

The application pins Līdza v0.1.71. Authentication, encrypted credentials, private GitHub connections, durable job dispatch, interval/daily scheduling, mail alerts and S3 signing use released framework APIs.

## Available

- Public/private app deployment and redeployment, readiness-gated activation, previous-release rollback, runtime environment edits, named databases and automatic reloads.
- Multiple FQDNs per server, DNS verification/retries, proactive Caddy certificates, automatic renewal and status in the GUI.
- Local/external PostgreSQL connections, one rolling pre-deployment backup per database, separate scheduled/manual retention, local backup download and streaming S3 copies.
- Restore into a new database with integrity checks, source preservation and attachment/reload after success.
- CPU/load, memory, agent/Docker disk space, PostgreSQL sizes, low-resource/overdue-backup alerts and recovery notices.
- Deployment commit subjects, durations, failure details, changed setting names and bounded captured build logs.
- App-specific maintenance pages.
- Signed pull-request preview deployment and cleanup, separate environment and optional private database.
- Background worker start/restart/disable/logs, current-image/environment reconciliation, durable interval/daily command dispatch, overlap prevention and failure alerts.
- Official agent release checks, checksum/manifest validation, atomic binary upgrades, co-located control-panel upgrade and failed-health rollback.
- Required-FQDN installer, first-run setup wizard, GitHub OAuth/webhooks, storage and email configuration, and the approved Signal design using released Līdza colors.

## Pending framework release

Administrator/deployer/viewer team access and durable audit logging depend on [Līdza #28](https://github.com/agim/lidza/issues/28). No application substitute bypasses the framework-first rule. The remaining code currently continues to enforce the configured operator only.

## Validation and limits

Race-enabled Go tests and vet cover the product and released framework integrations. Real local Docker/PostgreSQL/Caddy tests cover deployment/reload/rollback, DNS verification routes, proactive trusted local certificate issuance, backups/restore, preserved source data, worker execution/replacement and preview resource cleanup. Isolated upgrade fixtures verify successful upgrade, failed-health rollback, checksum rejection and version validation. Installer staging verifies root/source-build and bundled paths without modifying the shared host. Browser checks cover console layouts, CRUD, backup/download/restore, named connections, maintenance, tasks, preview configuration with secret omission, server metrics and upgrade availability.

Live public ACME, GitHub consent/private clone and actual PR webhooks, real S3/SMTP providers and a systemd upgrade on a dedicated production VM remain external acceptance checks. Local test CAs, fake GitHub endpoints and isolated service mocks do not establish those provider/host results. See [Operations](operations.md), [Domains and HTTPS](domains-ssl.md), and [Databases and backups](databases-backups.md) for requirements and recovery steps.
