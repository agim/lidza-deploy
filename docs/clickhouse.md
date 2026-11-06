# Managed ClickHouse

Lidza Deploy provisions a private ClickHouse service on the app's hosting agent
when its `lidza.json` declares the existing non-secret deployment environment
setting below and no explicit `CLICKHOUSE_URL` is configured:

```json
{"deploy":{"env":{"CLICKHOUSE_MANAGED":"true"}}}
```

This uses the released framework's `config.Deploy.Env` contract. Docker service
orchestration belongs to the deployment agent; schema and migration execution
remain application responsibilities. No custom framework pack is required.

The first code deployment reserves a random password in encrypted agent state,
creates the `lidza-clickhouse-APP_ID` network/container and persistent `-data`
and `-logs` volumes, and waits for an authenticated query before building the
application. ClickHouse 26.8.15.10 is pinned. Allow an additional 2 GiB of host
memory for the service. No ClickHouse ports are published to the host.

The agent supplies `CLICKHOUSE_URL`, `CLICKHOUSE_DATABASE=telemetry`,
`CLICKHOUSE_USER=lidza`, and `CLICKHOUSE_PASSWORD` to app and task containers.
Connection values remain write-only in Settings. Managed connection keys cannot
be deleted or changed through the environment editor. The application receives
the private service network on first deploy, subsequent deploys and reloads.

Retries and agent restarts reuse the reserved password and existing volumes.
Stopped services restart on the next code deployment. A name occupied by an
unmanaged Docker resource causes refusal; the agent never replaces that resource
or erases data to fix a failed deployment. Service/authentication failure prevents
candidate activation and retains the previous release. App rollback does not
roll back ClickHouse schema or data.

Deployment diagnostics identify the failed stage and include bounded, redacted
Docker output. A first-time image download is reported separately from container
creation. Readiness failures include the last authenticated query error and
container status, exit code and OOM flag. The agent's retained password never
enters the deployment log.

After a failure, keep existing containers and volumes intact. Inspect the
deployment's specific failure before changing Docker connectivity. Retry after
correcting that condition; the reserved password and
volumes are reused. Download failures require checking Docker's registry access;
an application redeploy cannot repair a host networking or registry outage.

An explicit external `CLICKHOUSE_URL`, including sealed application credentials,
takes precedence before any managed service is created. Partial connection
settings without a URL are rejected; remove them to request automatic setup.
`CLICKHOUSE_MANAGED=false` opts out of first-time provisioning. Existing managed
attachments remain retained; this flag does not delete a database or remove its
connection. External-service configuration after managed provisioning currently
requires deliberate host/state administration rather than silently replacing
the managed connection.

App removal retains the service, volumes and encrypted password. Recreating the
same app ID receives a fresh resource identity, including for previews; it never
inherits retired telemetry. `/v1/retained-resources` reports the data volume
without credentials. The container service's data moves with the
application (see "Moving applications with ClickHouse" below); scheduled
backups of the container service are not part of the backup pipeline.

For an existing application that starts requesting ClickHouse, update the hosting
agent to this release, then deploy the application. When its manifest requests
ClickHouse, an existing `DB_MIGRATE=true` startup applies all packaged ClickHouse
migrations before readiness. Existing app keys and PostgreSQL jobs remain intact.
Failed jobs may still need the application's own replay command, and explicitly
paused workers must be resumed.

Acceptance covers encrypted credential persistence, failed setup and retry,
external connection preservation, removal/recreation isolation, and a real
Docker app deployment, reload and stopped-service redeployment with the original
row and password retained. Run the Docker test with `TEST_DOCKER=1`.

## Detection and the new-application wizard

Repository detection reports `needs_clickhouse` with its reason when the
application uses a ClickHouse client: a direct go.mod requirement of
`github.com/ClickHouse/clickhouse-go` (v2) or `github.com/ClickHouse/ch-go`,
the `clickhouse-activerecord`, `click_house` or `clickhouse-ruby` gem, or the
`@clickhouse/client`, `@clickhouse/client-web` or `clickhouse` package. A
compose service with a `clickhouse/clickhouse-server` (or `clickhouse`) image is
data the host provides, like PostgreSQL and Redis services. The wizard's
Database step has a "ClickHouse on this server" switch prefilled from
detection: native applications request `data.clickhouse`, container
applications get `CLICKHOUSE_MANAGED=true`. The connection variables are set
automatically and cannot be entered by hand alongside the switch.

## Host ClickHouse for native applications

A native application created with `"data": {"clickhouse": true}` (moves request
it automatically) gets a database on the host's ClickHouse server, installed on
first use from the official ClickHouse apt repository. The signing key is
verified against its published fingerprint
(`3A9EA1193A97B548BE1457D48919F6BD2B48D754`) and the packages are pinned to one
release series in `/etc/apt/preferences.d/lidza-clickhouse`: the series of the
server an application moves from, `native.host_data.clickhouse_version`, or the
newest LTS series. A ClickHouse the agent did not install is never taken over.

The server listens on 127.0.0.1 and ::1 only (HTTP `clickhouse_http_port`,
default 8123; native TCP `clickhouse_tcp_port`, default 9000; no MySQL,
PostgreSQL or interserver ports). It caps its memory at
`clickhouse_memory_ratio` of the host's RAM (default 0.25), its systemd unit has
a hard memory limit above that and no outbound network beyond loopback, logs
rotate at 100 MiB × 5 and system logs expire after 14 days (`trace_log` after 3 days;
`text_log` records warnings and errors only). Managed ClickHouse containers get the same
policy as `config.d/lidza-logs.xml`, and the agent applies the TTL to existing system log
tables (including renamed `<name>_N` copies) with an idempotent `ALTER TABLE … MODIFY TTL`
on provisioning and hourly, without restarting ClickHouse. Server health reports each
server's system log size. The `default` user
keeps loopback access only, with a random password sealed in agent state.

Each application gets a database, user and settings profile named `app_<id>`:
the user signs in from loopback only, is granted privileges on its own database
only (including `CREATE DATABASE IF NOT EXISTS` for that name) and runs queries
with `max_memory_usage` (half the server cap), `max_execution_time` 300 s and at
most 32 concurrent queries. The application receives the same variables as the
container service: `CLICKHOUSE_URL=http://127.0.0.1:8123`,
`CLICKHOUSE_DATABASE`, `CLICKHOUSE_USER` and `CLICKHOUSE_PASSWORD`.
Applications that read `CLICKHOUSE_DATABASE` need no change; a database name
hard-coded to `telemetry` does not work on a native host.

The database is a protected database resource (engine ClickHouse in Data &
Storage, size from `system.parts`). Scheduled and pre-release backups, local
retention, verified off-site copies and the final backup before removal use
`BACKUP DATABASE … TO Disk('backups', …)` on a dedicated disk under
`/var/lib/clickhouse/lidza-backups`; the agent stores the result as an archive
(`manifest.json` with every table's row counts, plus `backup.zip`). Restoring a
backup from Data & Storage replaces the database's contents and verifies the
row counts. Its **Before deploys** setting works as for any other database
(see [databases and backups](databases-backups.md#before-deploys-per-database)):
a fresh backup before every release by default, or a verified backup newer
than N hours; as an analytics database it cannot choose scheduled backups
only. Removing the application drops its user, profile and database; the
server stops when no application uses it.

## Moving applications with ClickHouse

A move copies the managed ClickHouse database of a container application (the
`telemetry` database of its service) or of a native application. The source
agent exports an archive: a ClickHouse BACKUP when the destination runs the same
or a newer release series, otherwise one Native-format file per table with its
CREATE statement. The control panel streams it to the destination like a
PostgreSQL dump, checking its length and SHA-256 on the way; the destination
checks the digest again before touching anything, restores the archive under
its own database name (`RESTORE DATABASE telemetry AS app_<id>`, which renames
references inside views), and verifies the row count of every table. The
control panel compares those counts with the source's export a second time. A
destination that cannot read the BACKUP receives the tables one by one instead.
The `CLICKHOUSE_*` variables always come from the destination's own service.

Row counts are taken just before and after the source's snapshot. A live copy
accepts any count between them; the final sync (source in maintenance) checks
exact counts. Tables of the Replacing/Collapsing/Summing/Aggregating families
may merge rows away and are checked against the upper bound. The per-table
format cannot preserve materialized views that store rows in an inner table
(views without `TO`); such databases move as a BACKUP between servers of the
same or a newer release.

Plan disk space for about three times the database on the destination (the
received archive, the staged backup and the restored data) and twice on the
source. Downtime during the final sync is the time to export, transfer and
restore the whole database.
