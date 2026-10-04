# Application databases and backups

Requires matching current agent and control-panel builds. Līdza v0.1.71 resolves the mail reload/send race reported in [#27](https://github.com/agim/lidza/issues/27); its concurrency and queued-delivery tests pass. See [implementation status](implementation-status.md) for validation and remaining external acceptance checks.

## Add an application

The New application form requires a database choice:

- **Create local PostgreSQL:** the agent pulls PostgreSQL 17 Alpine and creates a private container, network, named data volume, database, and a non-superuser application role. Each app gets separate credentials. No PostgreSQL port is published on the server. The database container restarts with Docker and currently has a 1 GiB memory limit.
- **Use existing managed PostgreSQL:** enter its PostgreSQL connection URL. The database must already exist and its user must be able to connect, run the app's migrations, and read the objects that should be backed up. The agent checks the connection from the hosting server; it does not create a database at the external provider. Use a hostname reachable from containers, not `localhost`. TLS is required (`sslmode=require`, `verify-ca`, or `verify-full`). The bundled backup client supports PostgreSQL servers up to version 17; providers with private CAs or client-certificate requirements are not supported by this first UI.
- **No database:** useful for static apps or apps with their own data services. A manually entered DATABASE_URL does not enroll an app in managed backups.

For either database option, choose manual, hourly, daily, or weekly backups, retain 1–100 local copies, and optionally enable S3 copying. Local daily backups with seven retained copies are the form defaults. After provisioning succeeds, the agent supplies `DATABASE_URL` automatically and protects it from edits through the generic environment editor. Credentials remain encrypted in agent state and never return to the browser. If the app is already deployed, successful database setup queues an automatic reload with the new connection. An undeployed app still needs its first deployment. Database provisioning does not run application migrations; migrations remain the app's responsibility.

Use an app's **Database & backups** button to check progress, retry failed provisioning, change retention/frequency, or choose **Back up now**. Create another local or external database with a unique ID and a variable such as `ANALYTICS_DATABASE_URL`, or select an existing database on the same agent and attach it by name. To switch the main connection, select `DATABASE_URL`. To change an external database path or rotate connection credentials, create a new connection ID using the replacement URL and attach it; the old resource and backups remain available. Switching between local and external resources is supported this way. A data migration remains a separate operation; attachment does not copy data. Apps sharing a database share its credentials and data.

The installer installs Docker. App PostgreSQL containers are created on demand by the agent when selected in the GUI, independently of the control panel's own PostgreSQL database. It does not reuse, reconfigure, or take ownership of a pre-existing host PostgreSQL service.

## Runtime variables and primary switching

Each app has independent, named attachments. `DATABASE_URL` is used by the standard Līdza DB pack; additional variables ending in `_DATABASE_URL` are available to the app as normal environment variables. The app can open another pool with the existing framework API, for example `db.Open(ctx, db.Config{URL: os.Getenv("ANALYTICS_DATABASE_URL")})`, and must close that pool during shutdown. The deploy agent does not automatically teach application queries to use a second database.

Local database networks are attached before the app container starts. Connection secrets stay encrypted in state and are not returned by the GUI. Database variables appear as managed entries in the environment editor; change them through Database & backups. Older agent state is migrated once to the primary-attachment model without changing database credentials or deleting files.

A primary switch reloads the current image with the new URL. The previous healthy release remains available if readiness fails; Deployments reports the failure. Saved settings represent the requested configuration, so use Reload to retry after fixing the issue. Old rollback containers retain their original variables and connections; rollback does not migrate data or undo schema changes.

## Before deployments and reloads

**Back up attached databases before deployments and reloads** is enabled by default in app Settings. Manual and webhook deployments, automatic settings reloads, and manual Reload use the same backup gate. Each distinct attached database is dumped once, including the currently running release's old primary connection when switching to another resource. Existing retention, off-site destination and size limits apply.

A failed dump, required off-site upload or metadata save stops activation and leaves the current release running. Fix the problem and retry, or explicitly disable the app's pre-deployment backup option in Settings. Apps without managed attachments have nothing to back up. Manually supplied database URLs are not enrolled automatically. Each dump is consistent for its own database; multiple databases are not a single atomic snapshot, and application writes are not paused. App migrations remain the application's responsibility.

## Local and off-site copies

Backups use `pg_dump --format=custom --no-owner --no-acl` and capture one database, not cluster roles or the control panel. They are written privately under `/var/lib/lidza-agent/backups/DATABASE_ID/` (or the agent's configured data directory), finalized before being listed, and include a SHA-256 checksum. Dumps are limited to 5 GiB, with two database operations per agent at once and a two-hour operation deadline. Large installations needing physical backups or point-in-time recovery should use their PostgreSQL provider's backup facilities.

In **Integrations → S3-compatible storage**, enter an HTTPS endpoint, bucket, region, optional prefix, and access/secret keys. Saving writes and deletes a small connection-check object. Enable off-site copies per app after configuring the destination. The bucket must exist and grant object write/read-metadata/delete access within the prefix. Keep it private, enable provider-side encryption, and configure a bucket lifecycle rule for off-site retention. Local retention does **not** delete S3 objects. This version does not provide client-side encryption or automatic S3 restores.

A local dump is saved first. Off-site copying streams it through Līdza's signed PUT API and verifies the uploaded size. If S3 fails, the local copy is retained and the operation reports the failure. Scheduled failures retry after 15 minutes. Manual-only backups require another manual attempt. Changing the storage profile affects subsequent backups; old objects remain at the old destination. Blank credential fields preserve saved keys.

The control panel schedules checks once a minute through Līdza's persistent jobs pack; the agent performs the actual dump. The first scheduled backup becomes due when provisioning completes. Subsequent successful backups advance the next due time by the selected interval; changing a schedule starts its new interval from the save time. After downtime, a due backup runs once instead of producing a backlog for every missed interval. **The control panel and its job workers must be running** for scheduling and alerts. Manual agent requests still work while the control panel is offline.

Removing an application preserves its databases, volumes, and backups. Scheduled backups stop for resources with no active attachments or current releases; a database still shared with another app keeps its schedule. Retained copies remain listed in **Databases & backups**, and the application ID stays reserved on that agent to prevent accidental reuse. Keep the hosting server registered to retain GUI access. Database destruction and reclamation are intentionally manual; do not run global Docker pruning. Removing a server from the panel does not remove its database data; reconnect the same agent to regain access.

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
