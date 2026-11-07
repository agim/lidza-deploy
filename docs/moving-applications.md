# Moving applications between servers

A move copies an application to another server and switches it there, for
every runtime. In the console it starts from **Move to another server** on the
application's card; the API is:

    GET  /api/control/apps/{id}/move-plan     review before starting
    POST /api/control/apps/{id}/move          start {target_server, runtime_override?, ...}
    GET  /api/control/apps/{id}/move          the application's latest move
    GET  /api/control/moves/{id}              one move, with live progress
    POST /api/control/moves/{id}/{action}     continue, retry-now, recheck-dns, auto-cutover,
                                              final-sync, finish, abort, remove-retained,
                                              excluded-tables

The plan lists databases (with their copy mode), file sets, published ports,
mail records, build memory, estimates, warnings and blockers. Planning never
starts or stops anything, writes state or changes DNS. A started move runs
these steps: Plan, Prepare destination, Copy environment, Copy data, Copy
files, Deploy on destination, Switch DNS, Final sync (application locked) and
Finish. The source keeps serving until the move finishes. PostgreSQL databases
replicate continuously when they can (through the control panel by default,
so no port opens between the servers) and fall back to a dump and restore.
DNS is switched by the operator: the move waits at Switch DNS, checking the
zone's own nameservers, until every hostname resolves to the destination. The
final sync puts the source and the destination in maintenance, copies the
latest changes and redeploys the destination; with the automatic cutover it
runs, and the move finishes, as soon as DNS and HTTPS work on the destination.
Every step is checkpointed and idempotent, so a failed or interrupted step
(including a control panel restart) is retried or resumed; an abort before the
switch removes the destination copy. The sections below describe each part.

### Read-only migration inventory

`GET /api/control/apps/{id}/migration-plan` (agent: `GET /v1/apps/{id}/migration-plan`)
is an older, read-only inventory of a Go application: environment **names**,
database attachments and sharing, backup timestamps, persistent-file
requirements, task IDs and blockers, with no environment values, database
passwords, task commands or backup object names. Its `execution_supported:
false` means this inventory itself executes nothing; moves run through the
move API above.

## Build memory

The move plan's `build_memory` compares what the application's build needs
with the destination's memory: the largest peak measured on the source when
known, otherwise an estimate for the detected stack (Go ~1 GiB, Līdza ~1.5
GiB or ~2.5 GiB with a front end, +1 GiB for Rust packs, Rails assets ~1.5
GiB, JavaScript builds ~2 GiB). An estimate above the destination's free
memory minus the reserve for running applications is a warning; above its
total memory minus the reserve it blocks the move. Repository detection
(`build_memory` in `GET /api/control/repos/detect`) reports the same
estimate, and the new-application wizard checks it in its Server step.

## Live progress

While a move runs, `GET /api/control/moves/{id}` reports the running step's
`progress`: a `phase` ("streaming the dump of shop to beta"), `done_bytes`,
`total_bytes` when the size is known, and `started_at`/`updated_at`. Byte counts
come from the control panel while a dump or file archive streams through it, and
from the agents while they create or restore one: database views
(`GET /v1/databases`) carry the running operation's `progress`, and
`GET /v1/move/files/{move}/progress` lists the move's file sets an agent is
archiving, receiving or extracting. Agents without these fields or the endpoint
are still moved; their steps show elapsed time only.

Live progress is kept in memory. The encrypted control state is written on step
transitions, which keep the step's last progress, and by heartbeats: when a step
logs nothing for 30 seconds, it gets a log line such as "Still dumping shop on
alpha: 1.20 GiB written (4m0s elapsed)." that the next heartbeat replaces.
Phases and heartbeats name databases, file sets and servers only.
Progress also carries `engine`, `done_objects`/`total_objects` and `counted`
(the phase has live counters).

## Background copy and the final-sync lock

The application stays fully usable while a move copies in the background
(plan, prepare destination, environment, data and files, destination deploy,
waiting for DNS): deploys, settings, environment, domains and data actions
work, and webhooks deploy as usual. Every such change is recorded with the
move (`source_changes`); a source deploy marks the destination release stale
(`destination_stale`). The panel shows **Preparing move (app stays live)**.

The **final sync** locks the application (**Final sync — app locked**): every
mutating route answers 409 and pushes are skipped until the move finishes or
is aborted, also while a failed final sync waits to be resumed. It freezes
the source, exports the environment again (the log lists changed, added and
removed keys; values are never recorded), carries over hostname and branch
changes, copies every database and file set again, and redeploys the
source's current commit on the destination. Finishing without a final sync
reconciles the environment and redeploys a stale destination first. The
databases the move created on the destination stay locked for the whole
move. The move view and the application list report `locked` and `phase`.

The destination is frozen during the final sync as well: it serves the bulk
copy from the DNS switch on, so the final sync puts it in maintenance
(containers) or stops it (native) next to the source, and the redeploy at
the end starts it again; a failed attempt releases both. Writes the
destination accepted before the final sync are not kept, and the cutover
log says so. A dump restore waits until the destination runs no deployment
of the application and no operation on the database, and logs what it waits
for.

## Continuous PostgreSQL replication

The plan chooses per database between continuous logical replication and
dump and restore, from the source agent's readiness report (server version,
`wal_level`, free replication slots and WAL senders, tables without a
replica identity), the destination's abilities and the operator's
**Database copy** option (automatic, or dump and restore), and names the
reason for every fallback. It warns when Prepare restarts the source once to
set `wal_level=logical` (new databases start with it) and that schema
changes on the source during the move break the replication.

The bulk copy restores a schema-only dump, publishes the database on the
source (a replication role with a fresh password and a publication of every
table, or every table but the excluded ones) and subscribes the destination
with an initial copy, followed as live progress. Tables without a replica
identity are copied with a data dump. The replication connection travels:

- over a private network both hosts share, when the destination agent
  reaches the source's relay port within 5 seconds;
- otherwise **through the control panel**, which already reaches every
  agent: the panel keeps idle upgraded HTTP streams to the destination agent
  and, when the destination hands one a PostgreSQL connection, opens a
  stream to the source agent and copies bytes both ways. No extra port is
  opened on either host, so a cloud firewall that admits only HTTPS and SSH
  does not force a dump. Tunnels resume after an agent or panel restart;
- over a public TLS relay on the source (with an owned host firewall rule
  admitting only the destination's addresses) only when the move asks for
  `replication_network=public`.

While the move waits at the cutover, each status read renews the
publication's lifetime, so only a move nobody follows loses its slot after
seven days. The locked final sync waits until the subscriber has applied the
publisher's LSN and sets the sequences; Cleanup drops the subscription,
publication, slot, role, relay, tunnel and firewall rule. A subscription
that breaks falls back to a fresh dump (a locked final sync fails with what
to do). Estimates show seconds (replication lag) for replicated databases.

## Automatic cutover and DNS checks

At **Switch DNS** the check asks the zone's own authoritative nameservers,
falling back to public resolvers and then the host's resolver, so a changed
record is seen without waiting for cached TTLs; the cutover view names the
server that answered.

The `auto_cutover` option (plan, start request, and the `auto-cutover
{"enabled": …}` action while the move waits at the cutover) is on by default
when every copied database replicates continuously. Once every hostname
resolves to the destination and HTTPS works there, the DNS check starts the
final sync itself and finishes the move after it succeeds; both steps are
logged. A failed automatic final sync switches the option off. The manual
**Final sync** and **Finish** buttons stay available.

## Ports and mail

The source reports the host ports its release publishes; the destination
reports the ports already bound on its public addresses (with their owning
process) and its managed firewall state. A port the destination already
uses blocks the plan, except one held by a host Postfix that served nobody
outside the host and relayed nothing in the last 7 days: that is a warning,
and Prepare backs up its configuration, binds it to loopback and switches
off its SMTP listeners for the published ports (local system mail still
works through the pickup queue), then verifies the ports are free on every
address. A Postfix that serves mail, unreadable mail logs and other holders
stay blockers that name the process and the evidence.

Applications that publish a mail port (25, 465, 587) get their reverse DNS,
SPF and MX looked up: a warning per destination address whose PTR is not
one of the application's hostnames, a warning per MX pointing elsewhere, and
the cutover records list the rewritten SPF record and a PTR line per address
next to the A/AAAA records. The cutover re-checks reverse DNS on every DNS
check; reverse DNS is optional and never holds the move. When the environment
shows that outgoing mail goes through a provider (provider API settings such as
`MAILGUN_*`, a `MAIL_PROVIDER` value, or an SMTP host of a known provider),
reverse DNS does not affect it and is not checked or listed at all. A release
that publishes a mail port while no service declares `public_tls` gets the same
warning in the plan as on its card (see [deployment](deployment.md#mail-ports-and-starttls)).

## Redirect domains

The plan includes the application's [redirect domains](redirect-domains.md).
Prepare recreates them on the destination (and again when they change before
the final sync), the cutover lists their DNS records next to the application's,
and the DNS check waits for them too.

## Protected applications

A container release with `gateway_transport` moved to a host whose Caddy
answers on its admin API gets the host gateway set up automatically at its
first deployment (see [gateway transport](gateway-transport.md)); the plan
notes it, or blocks when the destination cannot serve one.

Before the final sync, `lock_estimate` gives the expected lock duration (from
this move's measured bulk copy and deploy times). Above 15 minutes the
`final-sync` action needs `{"confirm_lock": true}`.

## Retained data on the destination

Until the move switches, the destination holds a copy of the source's data.
An abort before the switch removes the destination application together with
that copy; nothing is retained there. Removals made by a move record their
origin (move, source server, switched or not) on any retained resource they
leave, so a later attempt can tell a copy from an application's own data.

When **Prepare destination** finds retained data of the same application ID
on the destination, it removes a copy left by an earlier attempt that never
switched on its own, without a backup (log: *Removed the data copied by the
earlier attempt … on …*), and creates the application. Retained data of an
application that really ran there (a switched move, or an unknown origin)
stops the step with an operator failure (`retained_data`, `failure.retained`
describes the resource). The panel offers **Remove retained data of … on …
(last backup …)**, the `remove-retained` action with `{"confirm": "<retained
id>", "final_backup": true|false}` under the same rules as the
retained-resources page; the move then waits for the removal and continues.

## Retries, checkpoints and failures

Each step runs through attempts. A transient failure (connection reset or
timeout, an agent restarting or upgrading, 5xx, busy resources, database or
ClickHouse connection errors, a stalled transfer) is retried after 30s, 1m,
2m, 5m and 10m, up to `max_attempts` (six by default, 1–10 at the start of the
move); the log says "attempt 2/6 in 1m: …" and the `retry-now` action skips
the wait. Operator-actionable failures (disk space, permissions,
authentication, incompatible versions, capacity, a checksum that mismatched
twice) stop at once. Every failure is recorded as `failure` (step, engine,
server, class, code, bounded detail, attempt, next retry, remediation),
written to the audit log and the application's Errors page; a move that
stopped raises an operational alert that clears when the move recovers or
ends.

Engines (`internal/control/move_engine.go`: PostgreSQL, ClickHouse, file sets,
Redis) keep `checkpoints` per phase and object: a finished copy is skipped
on retry, and an export, backup or archive of an earlier attempt is reused
while the source still has it. Transfers through the control panel resume by
byte offset when both agents support it (`resume` in `GET /v1/move/host`):
downloads honour `Range: bytes=N-`, restore routes accept
`X-Lidza-Upload`/`X-Lidza-Size`/`X-Lidza-Offset` and append to a staged
upload with a persisted rolling SHA-256 (`GET`/`DELETE
/v1/move/uploads/{upload}`), and the whole artifact's digest is verified
before anything is restored.

Within a database's copy the checkpoint also records what the destination
already holds: the digest of the PostgreSQL dump it restored and, after
it, the verified load of the excluded tables' data (export, digest, the
chosen tables and the row counts), and the digest of the ClickHouse archive
it restored. An attempt resumed after a restart of the control panel skips
a restore or load recorded for the same dump and the same table choices
(the log says *… before this attempt; not restoring it again*); a
ClickHouse destination is first asked for its current row counts
(`GET /v1/move/apps/{id}/clickhouse?counts=1&cutoff=…`) and restored again
when they no longer match the export. A fresh backup forgets the record,
so a different dump is always restored in full.

Release updates wait for moves. The panel's update dispatcher
(`internal/control/updates.go`) keeps a target **waiting** with *Waiting
for the move of <app> to <server> to reach a safe point*, shown in the
Updates page's per-host row, while a move runs, syncs, finishes or aborts
with a step running: the control panel's own host for any such move (its
restart would interrupt every relayed stream), a remote agent for a move it
is the source or destination of. Moves waiting for the operator (planned,
failed, at the cutover, ready to finish) are idle; the update proceeds on
the next tick. Agents additionally refuse an upgrade while they transfer
move data.

A step whose counters stand still for 10 minutes gets a warning (`stall`);
after 30 minutes the attempt is cancelled, the agents' diagnostics
(`GET /v1/move/apps/{id}/diagnostics`: running operations, ClickHouse
`system.backups` and queries) are logged, and the step is retried.

## Estimates

The plan's `estimate` lists each engine's measured size and objects with the
estimated bulk-copy and final-sync duration, from earlier moves' transfer
rates, the source's backup durations and a short disk probe on both agents
(`POST /v1/move/probe`), else conservative defaults. Every figure is an
estimate and names its basis.

## Tables the backups leave out

A database's **Backups skip data of these tables** setting keeps the rows of
listed tables out of backups (their schema stays). Because a PostgreSQL move
copies through a fresh backup, the plan lists those tables with their
estimated rows and sizes (measured from the source's table list; a table
without a row estimate shows its size only), and before starting the
operator chooses per table to copy its data (the default) or leave it behind.
The choice travels in the start request (`excluded_tables`,
`{database: {table: "copy"|"leave"}}`). Jobs created before plans offered the
choice, or a setting changed during the move, stop with the undecided tables
listed and ask for the choice (`excluded-tables` action with
`{"database": …, "tables": {name: "copy"|"leave"}}`) before continuing. Chosen tables move as a separate data-only dump of just
those tables (`/v1/move/databases/{id}/table-data`), at the bulk copy and
again at the final sync; the backup policy itself is unchanged.
Verification compares exact row counts (`/v1/move/databases/{id}/row-counts`)
and requires left-behind tables to be empty; the log and the finish summary
name every table left behind. A backup that leaves out a table without a
choice stops the move with the tables listed.

**Incremental final sync.** A large excluded table is usually an append-only
log, and copying it in full again with the source frozen would be most of
the lock. At the bulk dump the source agent looks for an incremental key: a
single-column integer primary key, or else the first indexed (btree, not
null) `timestamptz`/`timestamp` column named `created_at`, `received_at`,
`inserted_at`, `occurred_at`, `ts` or `timestamp` (the agent's
`move_key_columns` setting changes the list). When the table saw no updates
or deletes while the dump ran (`pg_stat_user_tables` counters), the dump
records the key, its maximum before the dump (the watermark) and the
counters; the job keeps them with the table (`exported.key`). At the final
sync such a table is not dumped again: the destination keeps it across the
base restore (`/restore?replace=1&keep=schema.table,…`, which restores the
dump in place through a `pg_restore -L` list that leaves out the kept
tables, their indexes, constraints, owned sequences and schemas), the source
exports the rows at or after the watermark as a binary `COPY`
(`/v1/move/databases/{id}/table-delta`; strictly after an integer key), and
the destination deletes its rows from the watermark on and appends the
export in one transaction (`/table-delta/restore`), so rows sharing the
watermark instant are never duplicated and a retry is exact. Both ends then
report `count(*)` and `max(key)`, which must match. A table that was updated
or deleted from since the bulk copy, whose key the source no longer
recognises, or whose statistics were reset falls back to the full copy of
that table alone, and the log says why. The plan's final-sync estimate
counts such a table by the source's average insert rate (rows per hour from
`n_tup_ins` since the statistics reset, labelled as such) over the bulk
copy's duration plus an hour, and the other tables in full; the lock
estimate shown before the final sync takes the same share off the measured
bulk copy.
A bulk copy made without a recorded key gets one derived at the final sync:
the destination reports the highest key it loaded and its row count, and the
source confirms it holds exactly that many rows up to it. Loads of excluded
tables run with `session_replication_role=replica`, so foreign-key and user
triggers do not fire for each verified row (host clusters grant the
application role that right on PostgreSQL 15 and later).

## Copy engines

**ClickHouse.** The source runs `BACKUP DATABASE … TO Disk('backups',
'agent/lidza-<id>/')` (a managed container: `File('lidza-<id>/')`, read out
with `docker exec tar`) with `ASYNC`, and streams the directory as one tar
after a manifest (format `directory`), compressed with `zstd -1 -T0` when
both agents report `zstd` in `GET /v1/move/host`. The destination verifies
the digest, extracts into its backups directory (owned by the server's
user, 0750/0640) and runs `RESTORE DATABASE … AS <target> … ASYNC`; files
and bytes read come from `system.backups`. A directory restores tens of
thousands of parts in seconds, where `.zip` and tar archive backups read
each file with a scan of the archive and take hours, so they are never
written for these copies. The restore never leaves the target half
replaced: the current database is renamed aside, the backup is restored
under the target's name (views are rewritten to it) and its row counts
verified, and only then is the aside copy dropped; any failure drops the
partial database and renames the previous one back. A cancelled or
aborted copy kills a running BACKUP or RESTORE (`KILL QUERY WHERE
query_id = …`). When both agents advertise the `incremental` ClickHouse
format, the bulk copy keeps its directory backup on both servers as a base,
and the final sync exports `BACKUP … SETTINGS base_backup` against it,
streaming only the parts added or merged since; the destination restores it
against its copy of the base with the same count check and swap. A missing
base, a failed BACKUP or RESTORE, or differing counts fall back to a full
copy with the reason logged; the estimate measures the parts added since the
base (`system.parts`), and the bases are removed when the move finishes or
aborts. Destinations on an older release series get the per-table Native
copy; destination agents without `clickhouse_formats` get the `.zip`
BACKUP they understand. Scheduled ClickHouse backups use the same
directory format as one local file and one off-site object; earlier
`.zip` backups keep restoring (the format is read from the manifest).

**PostgreSQL.** Dumps stay `pg_dump -Fc -Z zstd:1`. A host database is
restored into a fresh `<name>_lidza_staging` database (owned by the
application's role, with its privileges) in one transaction with
`PGOPTIONS='-c maintenance_work_mem=1GB -c synchronous_commit=off'`, then
both renames run in one transaction after new connections are refused, so
a failure leaves the application's database unchanged. Parallel
`pg_restore -j` is not used: measured restores are disk-bound and it did
not win reliably. Progress counts `pg_restore --verbose` lines against the
dump's table of contents. Container databases restore in place with the
same tuning.

**Files.** Archives are Zstandard (`zstd -1 -T0`) when both agents have
the tool, otherwise gzip; the destination detects the compression.
