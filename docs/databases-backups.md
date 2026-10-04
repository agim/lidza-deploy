# Application databases and backups

**This feature is in review. Do not deploy this branch yet:** live email configuration depends on the released fix for [Līdza #27](https://github.com/agim/lidza/issues/27), which reproduces a mail reload/send data race in v0.1.70. The independent database and backup flows have passed real Docker and browser tests. See [implementation status](implementation-status.md).

## Add an application

The New application form requires a database choice:

- **Create local PostgreSQL:** the agent pulls PostgreSQL 17 Alpine and creates a private container, network, named data volume, database, and a non-superuser application role. Each app gets separate credentials. No PostgreSQL port is published on the server. The database container restarts with Docker and currently has a 1 GiB memory limit.
- **Use existing managed PostgreSQL:** enter its PostgreSQL connection URL. The database must already exist and its user must be able to connect, run the app's migrations, and read the objects that should be backed up. The agent checks the connection from the hosting server; it does not create a database at the external provider. Use a hostname reachable from containers, not `localhost`. TLS is required (`sslmode=require`, `verify-ca`, or `verify-full`). The bundled backup client supports PostgreSQL servers up to version 17; providers with private CAs or client-certificate requirements are not supported by this first UI.
- **No database:** useful for static apps or apps with their own data services. A manually entered DATABASE_URL does not enroll an app in managed backups.

For either database option, choose manual, hourly, daily, or weekly backups, retain 1–100 local copies, and optionally enable S3 copying. Local daily backups with seven retained copies are the form defaults. After provisioning succeeds, the agent supplies `DATABASE_URL` automatically and protects it from edits through the generic environment editor. Credentials remain encrypted in agent state and never return to the browser. Deploy or redeploy once the database is ready to apply the connection. Database provisioning does not run application migrations; migrations remain the app's responsibility.

Use an app's **Database & backups** button to check progress, retry failed provisioning, change retention/frequency, or choose **Back up now**. External connection URLs can be replaced here to correct a typo or rotate credentials, then checked again before redeploying. Local and external database modes cannot be switched in place; a data migration is a separate operation.

The installer installs Docker. App PostgreSQL containers are created on demand by the agent when selected in the GUI, independently of the control panel's own PostgreSQL database. It does not reuse, reconfigure, or take ownership of a pre-existing host PostgreSQL service.

## Local and off-site copies

Backups use `pg_dump --format=custom --no-owner --no-acl` and capture one database, not cluster roles or the control panel. They are written privately under `/var/lib/lidza-agent/backups/APP_ID/` (or the agent's configured data directory), finalized before being listed, and include a SHA-256 checksum. Dumps are limited to 5 GiB, with two database operations per agent at once and a two-hour operation deadline. Large installations needing physical backups or point-in-time recovery should use their PostgreSQL provider's backup facilities.

In **Integrations → S3-compatible storage**, enter an HTTPS endpoint, bucket, region, optional prefix, and access/secret keys. Saving writes and deletes a small connection-check object. Enable off-site copies per app after configuring the destination. The bucket must exist and grant object write/read-metadata/delete access within the prefix. Keep it private, enable provider-side encryption, and configure a bucket lifecycle rule for off-site retention. Local retention does **not** delete S3 objects. This version does not provide client-side encryption or automatic S3 restores.

A local dump is saved first. Off-site copying streams it through Līdza's signed PUT API and verifies the uploaded size. If S3 fails, the local copy is retained and the operation reports the failure. Scheduled failures retry after 15 minutes. Manual-only backups require another manual attempt. Changing the storage profile affects subsequent backups; old objects remain at the old destination. Blank credential fields preserve saved keys.

The control panel schedules checks once a minute through Līdza's persistent jobs pack; the agent performs the actual dump. The first scheduled backup becomes due when provisioning completes. Subsequent successful backups advance the next due time by the selected interval; changing a schedule starts its new interval from the save time. After downtime, a due backup runs once instead of producing a backlog for every missed interval. **The control panel and its job workers must be running** for scheduling and alerts. Manual agent requests still work while the control panel is offline.

Removing an application preserves its database, volume, and backups and stops its scheduled backups. Retained copies remain listed in **Databases & backups**, and the application ID stays reserved on that agent to prevent accidental reuse. Keep the hosting server registered to retain GUI access. Database destruction and reclamation are intentionally manual; do not run global Docker pruning. Removing a server from the panel does not remove its database data; reconnect the same agent to regain access.

## Verify recovery

Download a backup from the authenticated GUI. Treat the file as sensitive: the custom dump is not encrypted. Compare its SHA-256 with the record before restoring. The S3 object key is shown when uploaded; prepend your configured object prefix when locating it in the bucket.

Restore into a **separate disposable database first**, not over the production database. For a local app database, these commands create a new database owned by the existing app role:

```sh
# Substitute your app ID and the downloaded file; use a fresh test database name.
docker exec lidza-db-your-app createdb -U postgres -O app restore_check
docker exec -i lidza-db-your-app pg_restore -U postgres --role=app \
  --no-owner --no-acl --exit-on-error -d restore_check < downloaded-backup.dump
docker exec lidza-db-your-app psql -U postgres -d restore_check -c '\dt'
```

Validate application-specific records and queries. Stop writes and plan a controlled connection change before a production restore; rollback of an application release does not roll back the database. For an external provider, follow its restore procedure and use its credential mechanism without putting passwords in shell history. The automated integration test creates real rows, dumps them, restores into another database, and verifies the restored values.

Back up the **control panel's own PostgreSQL**, `/var/lib/lidza-control`, `/var/lib/lidza-agent` including their master keys, and Caddy certificate storage separately. Per-app backups do not cover the control panel, arbitrary app files, or external object storage. Protect master keys separately from untrusted readers; losing them prevents recovery of encrypted configuration.

## Email alerts (blocked on framework release)

The prepared **Integrations → Email** form accepts SMTP host, port, TLS/STARTTLS, sender, and authentication. Alerts go to the operator account. Credentials are stored with Līdza credentials; service-environment overrides must be removed before editing those keys in the GUI. Blank username/password fields preserve the saved login. **Send test email** queues a message; Recent mail shows its delivery status and attempts. Saving SMTP settings validates configuration but does not claim successful delivery.

Deployment and database/backup errors generate alerts; unavailable hosting agents and failed readiness probes require three failed checks. Active incidents are deduplicated across control-panel restarts, followed by recovery notices when resolved. Līdza's durable mail outbox retries delivery. Queueing plus incident persistence is at-least-once: a crash in the narrow interval between them may duplicate a notice. Unknown readiness results do not count as failures. Use an independent uptime monitor for control-panel outages; this panel cannot send alerts when it is down itself.

The mail integration must remain in draft until #27 is fixed in a tagged Līdza release and the concurrency regression passes. No application-local mail implementation is substituted.
