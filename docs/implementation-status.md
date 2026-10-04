# Implementation status

The framework-first gate is satisfied: agim/lidza#23 was closed after v0.1.61 was published. The application now pins v0.1.70 and uses `auth.Mount`, `auth.Require`, `auth.Connection`, framework encrypted credentials, and the durable jobs pack. The temporary local OAuth flow and duplicate encryption implementation were removed.

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
- Current framework dependency: v0.1.70. One new integration check fails: enabling GitHub after boot without connectors does not register connection routes. Upstream #26 is open; wait for its released fix before completing that path.

## Database and backup work in review — 2026-10-04

The branch adds per-app PostgreSQL provisioning or external attachment, encrypted automatic DATABASE_URL delivery, selectable backup frequency and local retention, S3-compatible copies through framework signing, backup downloads, retained database data after app removal, and GUI storage/mail settings. Database creation, backup, restore of actual rows, retention and secret protection pass real Docker tests. S3 upload streaming passes a local protocol fixture. Browser tests pass local database creation, masked external connection entry, manual backup/download, policy changes, retained-data visibility and integration controls.

Scheduled dispatch, persistent alert deduplication, recovery notifications and mail outbox queueing passed PostgreSQL integration checks with sending disabled. Subsequent concurrent reload/send testing found **Līdza #27**, a framework data race in mail.Reconfigure. The email feature and publication to main are blocked until its tagged fix is released and verified. No real-provider SMTP or S3 acceptance test is claimed. Existing **#26** remains open and its onboarding regression still fails.

The feature will be kept in a draft PR for review while waiting for those framework releases. A passing database restore test does not constitute full production acceptance.
