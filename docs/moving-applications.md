# Moving applications between servers

`GET /api/control/apps/{id}/migration-plan` provides an authenticated, read-only
inventory before moving a Go application. The agent endpoint is
`GET /v1/apps/{id}/migration-plan`. Plans contain environment **names**, database
attachments and sharing, backup timestamps, persistent-file requirements, task
IDs and blockers. They contain no environment values, database passwords, task
commands or backup object names. Planning never starts/stops containers, writes
state, copies data or changes DNS.

For a source agent that predates this endpoint, build `./cmd/migration-review`
and run it with `-config /etc/lidza-agent/config.json` (optionally `-app ID`).
It reads the retained encrypted inventory and gateway configuration through the
independent reader. It starts no listeners or management jobs and needs no API
token. The running source agent does not need an upgrade or restart. Run it as
an operator able to read the retained keys; only the redacted plans are printed.

The plan explicitly reports `execution_supported: false`. It is a preparation
and review tool, not an automated move. A fresh plan and a verified destination
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
