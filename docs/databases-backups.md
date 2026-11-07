# Application databases and backups

Requires matching current agent and control-panel builds. Līdza v0.1.71 resolves the mail reload/send race reported in [#27](https://github.com/agim/lidza/issues/27); its concurrency and queued-delivery tests pass. See [implementation status](implementation-status.md) for validation and remaining external acceptance checks.

## Add an application

The New application form requires a database choice:

- **Create local PostgreSQL:** the agent pulls PostgreSQL 17 Alpine and creates a private container, network, named data volume, database, and a non-superuser application role. Each app gets separate credentials. No PostgreSQL port is published on the server. The database container restarts with Docker and currently has a 1 GiB memory limit.
- **Use existing managed PostgreSQL:** enter its PostgreSQL connection URL. The database must already exist and its user must be able to connect, run the app's migrations, and read the objects that should be backed up. The agent checks the connection from the hosting server; it does not create a database at the external provider. Use a hostname reachable from containers, not `localhost`. TLS is required (`sslmode=require`, `verify-ca`, or `verify-full`). The bundled backup client supports PostgreSQL servers up to version 17; providers with private CAs or client-certificate requirements are not supported by this first UI.
- **No database:** useful for static apps or apps with their own data services. A manually entered DATABASE_URL does not enroll an app in managed backups.

For either database option, choose manual, hourly, daily, or weekly backups, keep the latest usable local copy, and optionally enable S3 copying. Daily backups with one local copy are the form defaults. Older client retention values are accepted for compatibility but normalized to one. After provisioning succeeds, the agent supplies `DATABASE_URL` automatically and protects it from edits through the generic environment editor. Credentials remain encrypted in agent state and never return to the browser. If the app is already deployed, successful database setup queues an automatic reload with the new connection. An undeployed app still needs its first deployment. Database provisioning does not run application migrations; migrations remain the app's responsibility.

Open **Applications → Settings → Manage database & backups** to create, attach, or switch a database on an existing app. Apps without a connection show the setup form immediately. The same dialog is available under **More actions → Database & backups**. Use it to check progress, retry failed provisioning, change backup frequency, or choose **Back up now**. Create another local or external database with a unique ID and a variable such as `ANALYTICS_DATABASE_URL`, or select an existing database on the same agent and attach it by name. To switch the main connection, select `DATABASE_URL`. To change an external database path or rotate connection credentials, create a new connection ID using the replacement URL and attach it; the old resource and backups remain available. Switching between local and external resources is supported this way. A data migration remains a separate operation; attachment does not copy data. Apps sharing a database share its credentials and data.

The installer installs Docker. App PostgreSQL containers are created on demand by the agent when selected in the GUI, independently of the control panel's own PostgreSQL database. It does not reuse, reconfigure, or take ownership of a pre-existing host PostgreSQL service.

## Runtime variables and primary switching

Each app has independent, named attachments. `DATABASE_URL` is used by the standard Līdza DB pack; additional variables ending in `_DATABASE_URL` are available to the app as normal environment variables. The app can open another pool with the existing framework API, for example `db.Open(ctx, db.Config{URL: os.Getenv("ANALYTICS_DATABASE_URL")})`, and must close that pool during shutdown. The deploy agent does not automatically teach application queries to use a second database.

Local database networks are attached before the app container starts. Connection secrets stay encrypted in state and are not returned by the GUI. Database variables appear as managed entries in the environment editor; change them through Database & backups. Older agent state is migrated once to the primary-attachment model without changing database credentials or deleting files.

A primary switch reloads the current image with the new URL. The previous healthy release remains available if readiness fails; Deployments reports the failure. Saved settings represent the requested configuration, so use Reload to retry after fixing the issue. Old rollback containers retain their original variables and connections; rollback does not migrate data or undo schema changes.

## Before deployments and reloads

**Back up attached databases before deployments and reloads** is enabled by default in app Settings. Manual and webhook deployments, automatic settings reloads, and manual Reload use the same backup gate. Each distinct attached database is dumped once, including the currently running release's old primary connection when switching to another resource. Each database keeps its newest usable local dump across all backup kinds. A completed pre-deployment dump replaces the previous local copy; a failed dump preserves it. These backups do not postpone scheduled backups. Each database's pre-deployment deadline covers operation-slot wait, dumping and required off-site upload, and follows the database: the largest of the configured minimum (agent configuration key `predeployment_backup_minutes`, at most 120 minutes; 0 or omitted means 10), twice the duration of its last successful backup plus five minutes, and its current size at a conservative 4 MiB/s plus five minutes, capped at four hours. The deploy log states the deadline and how it was derived. Live build output names each backup stage and reports elapsed time and processed bytes every 15 seconds. PostgreSQL exclusive-lock acquisition is limited to 30 seconds, so a blocking schema change fails the backup with an actionable error instead of silently waiting. The current release remains running on failure. When off-site copies are enabled, the same `databases/<database-id>/predeployment.dump` object is overwritten on each upload. An upload failure blocks activation and keeps the new local dump; the previous remote object remains if the upload failed before replacement. Existing off-site destination and size limits apply. Older backups made before this change remain scheduled/manual history because their purpose was not recorded.

A failed dump, required off-site upload or metadata save stops activation and leaves the current release running. Fix the problem and retry, or explicitly disable the app's pre-deployment backup option in Settings. Apps without managed attachments have nothing to back up. Manually supplied database URLs are not enrolled automatically. Each dump is consistent for its own database; multiple databases are not a single atomic snapshot, and application writes are not paused. App migrations remain the application's responsibility.

### Before deploys, per database

Each database has its own **Before deploys** setting, in **Applications →
Settings → Data & storage** (one control per attached database) and on the
database's own page under **Databases & backups**. It applies to container
deployments and reloads, to native releases and to ClickHouse databases
alike, and it is the operator's choice: the platform keeps a safe default
and only prevents the one choice that would leave a primary database
unprotected.

- **Always take a fresh backup** (the default, and what every existing
  database has): a new verified backup before every deployment, as described
  above.
- **Reuse a backup newer than N hours** (1–720): a verified successful
  backup of this database — scheduled, manual or an earlier pre-deployment
  one, with its off-site copy when the database requires off-site
  protection, and still present on the server otherwise — that is newer than
  N hours counts as the pre-deployment backup. Otherwise a fresh backup is
  taken. The deploy log says which: `Reusing backup <id> from 2h ago for
  <database> (policy: reuse within 24h).` or `Taking a fresh backup of
  <database>: the newest verified backup is 3d old (policy: reuse within
  24h).` A database whose last backup failed is always backed up fresh.
- **Scheduled backups only**: the deployment takes no backup of this
  database; its schedule continues. Only a secondary database can choose
  it. A primary database — a native primary, analytics or standalone
  database, or the `DATABASE_URL` of a container application — shows the
  option disabled with the reason, and the agent refuses it.

The setting is saved on the hosting agent per database (`PATCH
/api/control/servers/<server>/databases/<database>/backups/before-deploy`
with `{"mode": "always" | "reuse" | "never", "max_age_hours": N}`; changing it
requires the administrator role and is audited). Each database's entry in
`GET /api/control/databases` reports the effective policy (`before_deploy`)
and whether the database is primary (`primary`). A native database policy in
the agent's `config.json` may set `"before_deploy": {"mode": "reuse",
"max_age_hours": 24}` as the initial value for that database; the legacy
`"skip_release_backup": true` is still read and means scheduled backups only.
Either applies only until an operator saves a choice in the panel. Moving an
application carries each database's setting to the destination; new
databases start at always.

## Local and off-site copies

### Tables whose data backups skip

On a PostgreSQL database's page in **Data & Storage**, the card **Backups
skip data of these tables** keeps the rows of disposable tables (logs,
caches, raw event history) out of that database's backups. **Choose tables**
opens the database's table list, largest first, with each table's estimated
rows (`pg_class.reltuples`, or the live tuple statistics of a table never
analyzed) and total size (`pg_total_relation_size`); search it, tick the
tables to skip and save. The agent refuses a table that does not exist and a
list that would skip every table. An empty list makes backups complete again.
Changing the list requires the `infrastructure.manage` role and is audited.

Table definitions remain in every archive; only the selected tables' rows are
left out (`pg_dump --exclude-table-data`). **A restore recreates those tables
empty**, so applications that refer to that history must handle it missing.
The setting applies to manual, scheduled, deployment and reload backups of
that database only. The deploy log and each backup record list the skipped
tables, and the backup history notes "data of N tables skipped". The backup
gate, deadline, hashes, upload verification and failure retention are
unchanged. Native, SQLite and ClickHouse resources always back up every
table and do not offer the setting.

Agent API: `GET /v1/databases/{id}/tables` lists the tables;
`PATCH /v1/databases/{id}/backups/excluded-tables` with
`{"tables": ["schema.table", …]}` saves the list (exact lowercase names, at
most 100). The control panel proxies both under
`/api/control/servers/{server}/databases/{id}/`.

**Legacy configuration.** Earlier releases read the list only from
`backup_exclude_table_data` in the agent configuration, a map from database
resource identity to exact `schema.table` names:

```json
{
  "backup_exclude_table_data": {
    "example-db": ["public.request_logs"]
  }
}
```

The key is still read as a database's initial list until the panel saves one;
from then on the saved list applies, even when it is empty. Prefer the panel.

### Archive storage

Backups use `pg_dump --format=custom --compress=zstd:1 --no-owner --no-acl` and capture one database, not cluster roles or the control panel. They are written privately under `/var/lib/lidza-agent/backups/DATABASE_ID/` (or the agent's configured data directory), finalized before being listed, and include a SHA-256 checksum. Dumps are limited to 5 GiB, with two database operations per agent at once. Scheduled/manual backups retain their two-hour operation deadline; deployment backups use the shorter deadline described above. Large installations needing physical backups or point-in-time recovery should use their PostgreSQL provider's backup facilities.

In **Integrations → S3-compatible storage**, enter an HTTPS endpoint, bucket, region, optional prefix, and access/secret keys. Saving writes and deletes a small connection-check object. Enable off-site copies per app after configuring the destination. The bucket must exist and grant object write/read-metadata/delete access within the prefix. Keep it private and enable provider-side encryption. Off-site history follows the workspace retention policy (see [Off-site retention](#off-site-retention)); no bucket lifecycle rule is needed. Pre-deployment backups overwrite one current object; if bucket versioning is enabled, configure lifecycle expiration for noncurrent versions to avoid retaining previous copies. This version does not provide client-side encryption or automatic S3 restores.

A local dump is saved first. Off-site copying streams it through Līdza's signed PUT API and verifies the uploaded size. If S3 fails, the local copy is retained and the operation reports the failure. Scheduled failures retry after 15 minutes. Manual-only backups require another manual attempt. Changing the storage profile affects subsequent backups; old objects remain at the old destination. Blank credential fields preserve saved keys.

Removed or retiring applications do not produce new backups. Their retained databases report no active backup schedule, and the agent rejects stale backup requests even after restart or application ID reuse. A shared database continues backing up while another active application uses it. Existing local and S3 backups remain subject to their retention rules.

The control panel schedules checks once a minute through Līdza's persistent jobs pack; the agent performs the actual dump. The first scheduled backup becomes due when provisioning completes. Subsequent successful backups advance the next due time by the selected interval; changing a schedule starts its new interval from the save time. After downtime, a due backup runs once instead of producing a backlog for every missed interval. **The control panel and its job workers must be running** for scheduling and alerts. Manual agent requests still work while the control panel is offline.

Deleting an application archives it for 90 days: its databases and files are
kept with every login disabled and can be restored from **Deleted apps**, while its
cache and every local and off-site backup of its databases are deleted at once (see
[deleting and restoring applications](deleting-applications.md)). Its application ID
is released. Recreating the same ID starts fresh with a new
resource identity and authentication secret. New local storage, cache and database
resources do not inherit the removed application's data or credentials. You can
explicitly attach an existing database through the database settings if you want
to reuse it.

Scheduled backups stop for resources with no active attachments or current releases;
a database still shared with another app keeps its schedule. Retained databases and
backups remain listed in **Databases & backups**. **Retained resources** on that
page, on each server's page and in the app's **Data & storage** tab lists what a
removed application or a finished move left on a server: databases, caches,
ClickHouse and file volumes, with their sizes, since when they are retained, why
(application deleted, or moved and now running on another server) and the last
local and off-site backup of each database. File/cache recovery currently requires
server access.

**Remove** frees that disk space. Type the resource ID to confirm. The agent refuses
while any part is attached to a live application on the server, or while a move of
the same application involves the server. Every database needs a verified backup
taken after its application stopped writing (for a moved application: after the
final sync put it in maintenance); otherwise choose **Take a final backup first**,
which backs it up (and uploads the backup off-site when storage is configured)
before anything is removed. The agent then stops and removes the containers and
networks, and deletes the volumes, each only when it carries the agent's ownership
label. Backup records stay with the database (marked removed, restorable into a new
database) and off-site copies follow the retention policy. Each removal is recorded
in the panel's audit log and as a host job on the server.

A resource that the destination of a move left before the move switched is
marked *Copied by a move that never switched*: its data is a partial copy of
the source's, so no backup is required to remove it, and the next attempt of a
move of that application removes it on its own (see moving applications).

Native applications that the agent created keep their data the same way. Removing
one removes its services, account and route; its PostgreSQL database and role,
its ClickHouse database and its Redis data stay on the server as one retained
resource (its files stay in the application directory). The Redis instance is
stopped and disabled because it ran as the removed account. **Remove** on that
retained resource, after the same backup checks, drops the database and role,
removes the ClickHouse database with its user and profile, and deletes the Redis
instance's configuration and data directory. Backup records and off-site copies
stay. Until then the application ID cannot be created again on that server,
because the database, role and Redis names derive from it.
After the hosting agent is upgraded, recreating an ID archives resources retained
by older removals automatically. Legacy retained resources also appear in the inventory.
Keep the hosting server registered to retain GUI access. Database destruction and
reclamation remain manual; do not run global Docker pruning. Removing a server from
the panel does not remove its data; reconnect the same agent to regain access.

## Verify recovery

Download a backup from the authenticated GUI. Treat the file as sensitive: the custom dump is not encrypted. Compare its SHA-256 with the record before restoring. The S3 object key is shown when uploaded; prepend your configured object prefix when locating it in the bucket.

Restore into a **separate disposable database first**, not over the production database. For a local app database, these commands create a new database owned by the existing app role:

```sh
# Substitute your database ID and the downloaded file; use a fresh test database name.
docker exec lidza-db-your-database createdb -U postgres -O app restore_check
docker exec -i lidza-db-your-database pg_restore -U postgres --role=app \
  --no-owner --no-acl --exit-on-error -d restore_check < downloaded-backup.dump
docker exec lidza-db-your-database psql -U postgres -d restore_check -c '\dt'
```

Validate application-specific records and queries. Stop writes and plan a controlled connection change before a production restore; rollback of an application release does not roll back the database. For an external provider, follow its restore procedure and use its credential mechanism without putting passwords in shell history. The automated integration test creates real rows, dumps them, restores into another database, and verifies the restored values.

Back up the **control panel's own PostgreSQL**, `/var/lib/lidza-control`, `/var/lib/lidza-agent` including their master keys, and Caddy certificate storage separately. Per-app backups do not cover the control panel, arbitrary app files, or external object storage. Protect master keys separately from untrusted readers; losing them prevents recovery of encrypted configuration.

## Email alerts

The **Integrations → Email** form accepts SMTP host, port, TLS/STARTTLS, sender, and authentication. Alerts go to the operator account. Credentials are stored with Līdza credentials; service-environment overrides must be removed before editing those keys in the GUI. Blank username/password fields preserve the saved login. **Send test email** queues a message; Recent mail shows its delivery status and attempts. Saving SMTP settings validates configuration but does not claim successful delivery.

Deployment and database/backup errors generate alerts; unavailable hosting agents and failed readiness probes require three failed checks. Active incidents are deduplicated across control-panel restarts, followed by recovery notices when resolved. Līdza's durable mail outbox retries delivery. Queueing plus incident persistence is at-least-once: a crash in the narrow interval between them may duplicate a notice. Unknown readiness results do not count as failures. Use an independent uptime monitor for control-panel outages; this panel cannot send alerts when it is down itself.

Email delivery uses the released Līdza mail pack and durable jobs outbox. Send a test email after configuring your actual SMTP provider and confirm delivery before relying on alerts.

## Latest-only local retention

The hosting agent keeps **one newest usable local dump per database**, across
manual, scheduled and pre-deployment backups. This includes retained databases
left by removed applications. Cleanup runs after a new backup and at least hourly
while the agent is running; it also becomes due at agent startup. A missing or
empty newest inventory entry never causes the last usable local dump to be
removed. Busy backup/upload/restore operations defer cleanup. Local cleanup
never deletes S3 objects; off-site history follows the retention policy below,
and the rolling pre-deployment object key is unchanged.

## Off-site retention

Each database's scheduled and manual backups are stored as
`databases/<database-id>/<backup-id>.dump` under the bucket prefix. Retention
keeps, grandfather-father-son style, the newest backup of each of the last
**daily** days, **weekly** ISO weeks and **monthly** months that have backups
(UTC calendar periods; days without backups do not use up the count). New
installations start with 7 daily, 4 weekly and 0 monthly backups. Change the
workspace default in **Remote backups → Off-site retention**, and override it for
one database in its **Off-site retention** card; both accept 0–365 daily, 0–260
weekly and 0–120 monthly.

These rules always apply, whatever the counts:

- the `predeployment.dump` copy is kept and never counted;
- the newest nonempty backup of every database is kept;
- a backup younger than 24 hours is kept unless a newer backup of the same day
  supersedes it;
- databases with a running backup, upload or restore, and databases of an
  application move in progress, are left untouched;
- objects that are not `databases/<id>/<name>.dump` are never touched.

After each verified upload the agent lists that database's folder and deletes
what the policy no longer keeps, logging each deleted key and size. Every six
hours the control panel plans the whole bucket with the same rules and applies
the plan, but only when every server answered. A folder whose database no server
has any more (an application or resource that was removed) is **orphaned**: it is
kept for the removal grace period (30 days by default, 1–365) after the control
panel first notices it, then deleted entirely, including its pre-deployment copy.

Automatic cleanup starts only after retention has been saved. On an installation
that configured its bucket before this feature, nothing is deleted until an
operator reviews and saves the settings. **Review cleanup** shows, per database,
what is kept and what would be deleted, with sizes and the total reclaimed.
**Apply cleanup** deletes exactly the reviewed plan and refuses if anything
changed since the review. The same plan is available from the API:

```sh
# Workspace-wide dry run (bucket, every server, orphaned folders):
curl -sS -b cookies.txt https://deploy.example.com/api/control/storage/retention/plan
# Apply exactly that plan (the Origin header must match the panel):
curl -sS -b cookies.txt -H 'Origin: https://deploy.example.com' -X POST \
  -d '{"fingerprint":"<fingerprint from the plan>"}' \
  https://deploy.example.com/api/control/storage/retention/apply
# One server's databases only:
curl -sS -b cookies.txt https://deploy.example.com/api/control/servers/<server>/backups/retention/plan
```

Planning and applying require the administrator role and are recorded in the
audit log.

**Servers → More actions → Disk cleanup & retention** provides immediate cleanup,
a configurable cache/log timer, and the last run's report. Defaults are daily
cleanup of unused Docker build cache and archived system journals older than
seven days. Choose hourly, six-hourly, daily or weekly and an age from 1–365 days.
Disabling cache/log automation does not disable hourly local backup retention.
The agent runs these jobs independently of control-panel availability. Jobs
coalesce missed intervals after restart and defer while deployments, database
operations or upgrades are active.

Cleanup uses `docker builder prune` with an age filter and journald's supported
rotation/vacuum commands. It does not prune images, containers or volumes, flush
Valkey/application caches, truncate active Docker logs, delete arbitrary files
under `/var/log`, or change S3 history. Managed app/database/worker container logs
already rotate at 10 MiB per file and three files per container. Database dumps,
persistent volumes, image layers and application files remain separate disk-space
consumers. Cleanup controls require the current hosting agent; mutations require
an administrator and are audited by the control panel.
