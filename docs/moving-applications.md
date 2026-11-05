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
