# Moving applications between servers

## Move from the panel

Choose **Move** on an application to recreate it on another connected server:
natively when its repository builds natively, otherwise with the same container
runtime. The move copies the environment and databases, deploys the same commit
and verifies it on the destination while the source keeps serving. Switch DNS
next; the move shows the exact records, waits until every hostname resolves to
the destination and HTTPS works there, then makes the destination the live
application and retires the source. An optional final sync freezes writes on the
source and copies the data once more. Every step is resumable, and a move can be
aborted until it finishes.

Environment values and database dumps travel only between agents through the
control plane. Values are never stored or logged, and dumps are verified end to
end by length and SHA-256. The agent's part is served under `/v1/move/...` with
the agent API key, which only the control panel holds.

## Read-only migration plan

`GET /api/control/apps/{id}/migration-plan` provides an authenticated, read-only
inventory before moving a Go application. The agent endpoint is
`GET /v1/apps/{id}/migration-plan`. Plans contain environment **names**, database
attachments and sharing, backup timestamps, persistent-file requirements, task
IDs and blockers. They contain no environment values, database passwords, task
commands or backup object names. Planning never starts/stops containers, writes
state, copies data or changes DNS.

The plan explicitly reports `execution_supported: false`. It is a preparation
and review tool; the move above performs the transfer. A fresh plan and a verified destination
are needed before execution. Database backup timestamps are inventory evidence,
not proof that backups restore successfully. Test a restore before cutover.

Database review includes both explicit application bindings and database IDs
retained by the current live release. Older inventories may have live dependencies
without explicit bindings; an empty environment-key list records that distinction.
Sharing checks include those live dependencies as well. Manually configured
connections still require review; planning never invents or rewrites bindings.

The returned steps require a single-writer cutover: prepare the destination,
quiesce the source and jobs, take final verified backups, restore data, check
readiness, move traffic and certificates, then enable destination jobs. Preserve
encryption keys and retain the stopped source for rollback. Local shared
databases, queue contents, ClickHouse and stack volumes need explicit coordination.
Never remove source data as part of planning or run source and destination
workers concurrently.

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
query_id = …`). The final sync is a fresh full directory backup of the
frozen source (incremental backups grow back to full size once merges
run). Destinations on an older release series get the per-table Native
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
