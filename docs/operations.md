# Operations, previews and recovery

## Restore a database

In **Databases & backups**, choose **Restore** beside a local backup. Choose an app on the same server, a new database ID and the environment variable to attach (default `DATABASE_URL`). The agent pins the source backup, verifies its size/SHA-256, provisions a new private PostgreSQL database and restores it in a transaction. It verifies connectivity before attaching it. A running app reloads through the existing backup/readiness gate; follow its deployment result.

The source database is never overwritten. If restoration fails, its connection stays attached and the target appears with an error for inspection. If restoration succeeds but the app is busy or the attachment/reload fails, the new database remains available and the old running release remains available; use the attachment UI or Reload once the error is resolved. Agent restarts mark interrupted operations failed rather than silently attaching incomplete data. Restores currently use local backups; S3 objects remain off-site recovery copies and are not restored automatically.

## Server health and alerts

**Servers → Server health** shows CPU utilization between samples, load/cores, available memory, disk capacity/free space for agent data and Docker storage, and readable PostgreSQL database sizes. Unavailable measurements are identified explicitly. Linux host metrics come from procfs; disk statistics come from the actual filesystems. Database measurements use app credentials over the existing private network or external connection and have bounded timeouts.

The existing durable email alert/recovery system checks each minute. It alerts after two checks for disk usage at least 85% or less than 1 GiB free, three checks below 10% available memory, five checks at load above twice the core count, and two checks for overdue scheduled backups. Backup age considers scheduled/manual copies independently of rolling pre-deployment copies. Keep SMTP configured in Integrations; checks and mail dispatch require a running control panel. An independent monitor should still watch the control panel itself.

## Deployment history

**Deployments → Details & build logs** shows the commit subject, branch, domain, elapsed time, failure, changed setting/environment key names and bounded build output. Environment values and Git credentials are not recorded as change metadata; known environment values/tokens are masked in captured output. Repository-controlled output may contain independently supplied secrets: avoid committing credentials or printing them in build scripts. Output is capped at 64 KiB per retained deployment; old deployments without captured output say so. Deployment history is retained for the latest 500 records per agent in encrypted state.

## Maintenance mode

Choose **Maintenance** on an app, enable the page and set its message. It affects only that app's domain, returns HTTP 503 with Retry-After, escapes the message and uses the released Līdza colors. Runtime health probes and deployments continue. Disable the page to return traffic to the currently healthy release. Domain-verification requests remain available so maintenance does not interrupt automatic certificate issuance.

## Pull-request previews

Choose **PR previews** on the parent app. Set a base domain such as `preview.example.com`, optionally enable one private local PostgreSQL database per PR, and supply a separate preview environment. Production environment variables, database credentials, integrations and worker definitions are never inherited. Leaving the preview environment field blank preserves its saved values; `{}` clears it. Preview configuration applies to newly created previews; existing previews can be edited through their own app settings.

Point `*.preview.example.com` to the hosting server and enable/refresh **Auto-deploy** to register both push and pull-request events. Opened, reopened and synchronized PRs targeting the app's configured branch use GitHub's `refs/pull/<number>/head` from the parent repository, including fork PRs. Signed deliveries use the framework's durable job queue, retries and deduplication. PR state is rechecked before dispatch so old queued events cannot resurrect closed previews. Private repository access uses the operator's saved framework GitHub connection; its token is only used for checkout.

Closing the PR removes its preview app/containers, task resources, private database volume/network and local backups. Manually removing a preview uses the same cleanup. Removing a parent cleans its previews first. Explicitly shared or ordinary restored databases are preserved, and cleanup refuses databases still used by another app/release. Cleanup is retryable if a deployment or database operation is active. Off-site copies, if separately enabled, follow bucket lifecycle retention. GitHub webhook removal stops future events; remove leftover previews manually when disabling webhooks.

## Workers and scheduled commands

Choose **Workers & jobs** on an app. Commands are JSON argument arrays, for example `["/app/app", "worker"]`; no shell expansion occurs unless a shell is explicitly selected. Continuous workers use the current deployed image and app environment, private database networks, a non-root user, resource limits, bounded Docker logs and Docker restart policy. They move to a new release/environment on the next control-panel check. Start/restart, enable/disable and logs controls are available. Restarting workers repeatedly raises an alert.

Scheduled commands support intervals from one minute to one week or a daily time with an IANA timezone. Scheduling reuses Līdza's `jobs.Every` and `jobs.Daily`, including its daylight-saving behavior. Changed definitions invalidate queued commands, and recent run keys prevent retries from replaying a prior command. Commands should still be idempotent because a process can partially complete before failing. The control panel persists dispatch in the framework queue; the agent persists run keys, prevents overlap and caps runtime at 30 minutes. After downtime, overdue schedules run once rather than replaying every missed interval. Queue retries cover an unavailable agent; a failed command is reported instead of automatically repeating an operation that may have partially completed. Logs mask known app environment values. Put credentials in environment variables rather than command arguments.

Workers keep running with Docker if the control panel is down. Scheduled dispatch requires the control panel and its PostgreSQL queue. App removal stops workers, disables schedules and waits for an active scheduled command before cleanup. Control-panel and agent upgrades wait for active commands, backups, restores and deployments to finish.

## Managed agent upgrades

The installer now includes a root systemd path/service pair and a release helper. Existing hosts must run the updated installer with their original required FQDN to install this capability. **Servers → Agent upgrade** shows the current version, latest stable official release and upgrade result. Development/custom-data-directory hosts report that the managed helper must be installed.

The agent queues a validated stable version. The helper accepts only `vMAJOR.MINOR.PATCH` releases from `agim/lidza-deploy`, selects the matching amd64/arm64 asset, verifies the archive checksum and inner manifest, and checks the binary version. It atomically replaces the agent and any co-located control panel, restarts their services and checks loopback health/readiness. Failed startup restores the previous binaries and records a failure; existing hosted containers keep running. New deployments/database operations are blocked while an upgrade is queued or running. Upgrade service logs are available through `journalctl -u lidza-agent-upgrade.service`.

Upgrades replace binaries; they do not change Docker/Caddy packages or migrate hand-maintained Caddy configuration. Use the installer when a release requires host/configuration changes. Binary rollback is not a database-schema rollback; release migrations must remain backward compatible. Test production upgrades on a spare host before upgrading a fleet.

## Team access and audit logging

Līdza v0.1.72 supplies `auth.Roles` and `packs/audit` ([framework #28](https://github.com/agim/lidza/issues/28)). Tables are bootstrapped from the pack's exported DDL at startup; existing installations retain their configured owner and gain its administrator membership automatically. No manual schema migration is required.

This control panel has one fleet workspace. Its members share its apps and hosting servers; it does not offer multiple isolated tenants.

| Role | Access |
| --- | --- |
| Viewer | Read app/deployment status, host health, logs and connection metadata. Cannot mutate or download database backups. |
| Deployer | Viewer access, plus deploy/reload/rollback, existing app settings/environment, maintenance, workers/schedules, and auto-deploy webhooks. Cannot provision/remove apps, manage previews or databases, download backups, manage servers/integrations/upgrades, or administer members/audit. |
| Administrator | Manage all fleet resources, integrations, team access and audit. |

Open **Team** as an administrator to add a member by email and select their role. A new account requires a password of at least 16 characters; share it privately. Existing accounts can be granted or changed without supplying a password. Role changes never reset a password. **Remove access** revokes fleet membership, not the framework account. Changes take effect at the next request even with an existing session. The configured installation owner cannot be demoted or removed and remains administrator after restart. Only that owner authorizes the shared GitHub connection; other administrators can configure the connector but cannot connect a replacement identity.

**Audit log** provides 50-record pages of persisted authenticated mutation requests and immediate results, with member/role changes and signed webhook or scheduled-task dispatch attributed to named system actors. Metadata includes only fixed route names, safe identifiers and HTTP status; request bodies, environment values, passwords and provider/agent credentials are excluded. An `ok` request/202 response means accepted dispatch, not successful app deployment; use **Deployments** for build/readiness results. `AUDIT_RETENTION` uses the framework default of one year (`8760h`); `0` retains all events.

If the initial audit write fails, the mutation does not run. A remote agent operation cannot share a PostgreSQL transaction with its audit result: if the result write fails after a side effect, the GUI receives an explicit error saying the action may have applied. Inspect app state/history before retrying. Requested events remain durable. Framework role writes are idempotent; a reported incomplete role change requires inspection of membership before retrying.

## Publishing an agent release

Push a new stable `vMAJOR.MINOR.PATCH` tag to run `.github/workflows/release.yml`. The workflow runs race-enabled Go checks and vet, builds both Linux architectures with the tag as their version, verifies archive/inner checksums, and uploads four files to a draft GitHub release. It compares GitHub's asset size and SHA-256 digest with the built files before publishing the release as latest. An existing draft's release notes are preserved. It refuses to replace an already published release. Actions permissions are read-only during build; only the publish job receives repository contents write access.

Docker/PostgreSQL/Caddy and browser integration checks remain required before tagging; the hosted workflow's Go checks skip the optional service integrations when their test environment variables are absent. If the workflow fails, the release stays a draft. Fix the cause and rerun the failed workflow; never publish until all four assets have been verified.
