# Implementation status

The framework-first gate is satisfied: agim/lidza#23 was closed after v0.1.61 was published. The application now pins v0.1.71 and uses `auth.Mount`, `auth.Require`, `auth.Connection`, framework encrypted credentials, and the durable jobs pack. The temporary local OAuth flow and duplicate encryption implementation were removed.

## Implemented

The source agent is adapted inside this repository. Public/private GitHub clone support, deploy/redeploy, readiness-gated switching, previous-release rollback, runtime logs, multiple hostnames per server, Caddy domain authorization and automatic HTTPS configuration are implemented. A separate Līdza control panel supports server inventory, operator sessions, GitHub connections, webhook creation/update, signed push filtering, persistent dispatch/retries, and the selected Signal GUI with Līdza brand colors.

## Validated in this environment

- All 16 Go tests passed with the race detector, including agent, control/auth/PostgreSQL, GitHub APIs, retirement retries, and partial fleet history; go vet passed.
- Real Docker: two real Līdza runtime apps on one host, FQDN isolation, redeploy, rollback, container/image retirement, runtime isolation settings.
- Caddy production configuration validation; actual local-CA certificate issuance for two registered hostnames with verified chains/hostnames, rejecting an unknown hostname.
- Private-token askpass handling, omission from Git URLs/helper files/persisted deployment history, command-error redaction, and retrieval through Līdza's encrypted connection store.
- Actual Līdza sign-in and operator authorization; another account is denied fleet access.
- Signed webhook filtering and persistent dispatch; private repository listing/webhook API requests against local test servers.
- Browser: all three previews; demo create/deploy/logs/settings/removal; selected Signal mobile layout; actual sign-in; partial fleet history; real app creation, environment patching, and removal through the control panel and local agent; approved theme and root navigation.

## External acceptance and current scope

Real GitHub consent and private-repository deployment need the user's OAuth App configuration. Public ACME needs deployment hosts and DNS; local-CA testing is not a claim of public issuance or renewal. No production deployment or upstream code push was performed.

The product currently has one operator and one active control process, GUI-managed encrypted server inventory, root-Dockerfile app builds, and fixed container resource limits. Detailed constraints and operational recovery are in deployment.md. Team roles, automated cloud-server provisioning, persistent app volumes, build secrets for private dependencies, and scaling are not implemented. The user selected Signal with the exact Līdza dark brand palette; it is the live default. Alternative designs are retained as previews only.


Server inventory is now editable in the GUI, with credential checks and encrypted persistence. App settings support branch/FQDN changes and write-only environment patches. Auto-deploy can be disabled independently of GitHub connectivity. Regression coverage includes these management operations, secret omission, restart persistence, active-deployment rejection, session/origin guards, and all three browser design variants.


Application retirement is available end to end: typed ID confirmation, persisted auto-deploy disablement, retryable agent cleanup, FQDN/TLS revocation, and retained container/image removal. Active deployments block removal until they finish. Old queued events cannot deploy a recreated app with the same ID. Fleet history returns releases from healthy agents alongside explicit unavailable-server IDs.


## Requested installation automation (in progress)

- Agent bundle/installer provisions Docker, Caddy, Git, service account, credentials and services on dedicated Debian/Ubuntu hosts. Staging is tested; a fresh supported VM installation remains an external acceptance check.
- Automatic missing Dockerfile generation reuses released framework scaffold APIs in a temporary clone and pins the target app versions. Custom Dockerfiles are preserved. Framework `/readyz` is used directly.
- Upstream #24 is closed: first-run onboarding belongs in this product and should use `lidza.Boot` and framework credential/auth APIs. The local wizard now exists; managed PostgreSQL, browser setup, login, and restart pass.
- Upstream #25 is closed: v0.1.70 provides app-version pinning and Rust toolchains, now used directly by the agent. Manifest inference was declined; `lidza.json` remains required project metadata.
- Current framework dependency: v0.1.71. Upstream #26 and #27 are closed and released. GitHub configuration after boot and concurrent mail reload/send checks now pass.

## Database, backup and alert validation — 2026-10-04

The branch adds per-app PostgreSQL provisioning or external attachment, encrypted automatic DATABASE_URL delivery, selectable backup frequency and local retention, S3-compatible copies through framework signing, backup downloads, retained database data after app removal, and GUI storage/mail settings. Database creation, backup, restore of actual rows, retention and secret protection pass real Docker tests. S3 upload streaming passes a local protocol fixture. Browser tests pass local database creation, masked external connection entry, manual backup/download, policy changes, retained-data visibility and integration controls.

Scheduled dispatch, persistent alert deduplication, recovery notifications and mail outbox queueing passed PostgreSQL integration checks with sending disabled. Concurrent reload/send testing found **Līdza #27**, which was fixed upstream in v0.1.71. After upgrading, the original reproduction and the framework tests for concurrent readers, failed reload preservation and reconfiguration during queued delivery all pass with the race detector. **#26** is also fixed and the full `scripts/test.sh` suite (including onboarding, real Docker and Caddy, and vet) now passes. No real-provider SMTP or S3 acceptance test is claimed.

Both framework release gates are satisfied. Real-provider credentials, public ACME and fresh-VM installation still require acceptance checks in the deployment environment; passing local integration tests does not establish those external results.

## Runtime edits and database attachments

Deployed-app settings now queue an image-only reload with readiness checking and retained healthy releases on failure. FQDN edits update routing, TLS authorization and the default APP_URL. Named same-agent database attachments support extra connections and primary switching while preserving old data. Existing state migrates once. Deployments and reloads back up distinct attached/current-release databases by default and fail closed on backup errors; operators can explicitly disable the policy per app.

Validation covers real Docker connections across two database networks, primary switching, settings reload without another checkout, FQDN routing/TLS authorization, runtime APP_URL, secret delivery, failed reload/backup activation protection, legacy migration and shared-database retention. GUI tests cover additional database creation, named bindings and primary switching.
