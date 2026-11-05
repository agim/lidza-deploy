# Moving a native application between servers

`internal/migrate` moves an application deployed in the
`releases/current/shared` layout (see `internal/release`) from one host to
another with a single-writer cutover. Every operation is a separate call, so
the operator reviews and sequences them; the same calls run against local
directories in the package tests. Nothing in the package changes DNS or
removes source data.

## Model

| Phase | Where | Function |
|---|---|---|
| Preflight | both | `PreflightSource`, `PreflightTarget` with `Requirements` (free space, toolchain paths such as the pinned Ruby/Node installs, free ports, unused unit names). Findings record where the domain resolves today. |
| Export | source | `Export(app, dir, ExportOptions)` writes a 0700 bundle: `shared/` (via rsync, minus `DefaultExcludes`: gem and node caches, tmp caches, logs, the maintenance directory), environment files (0600), unit files, the application's crontab block, and one dump per database through a `Dumper`. `manifest.json` carries no secret values. |
| Transfer | operator | `Transport.Push(bundle, targetPath)`. `LocalTransport` for tests; `SSHTransport{Command, Host}` runs rsync over exactly the ssh argv given (for example an existing `ControlPath` with `ControlMaster=no`, `BatchMode=yes`, `ConnectionAttempts=1`). It never retries or reconnects. |
| Import | target | `Import(app, dir, ImportOptions)`: `ReadManifest` verifies dump digests; `ImportData` copies shared files and environment files and restores databases into caller-supplied target URLs; units are written into the owner's unit directory with the source root rewritten to the target root, `daemon-reload`ed and **enabled but not started**; then `Deploy(commit)` publishes the exact source commit with the release engine. The crontab block is deliberately not installed here. |
| Verify | target | `StartServices` (sockets first), then `Verify(Probe{Domain, Address, TLS, InsecureTLS})` sends a request with the production Host name to the target address, bypassing DNS. Before DNS moves the target cannot hold a public certificate for the name, so `InsecureTLS` checks the application rather than the certificate. |
| Cutover | both | `FreezeSource` (maintenance page on, workers and timers stopped; web units keep serving the page), a second `Export` + `Push` + `ImportData` for the final data, `StartServices` + `InstallSchedule` on the target, DNS switch by the operator, `Verify` again with a real certificate. `ThawSource` abandons a cutover before the target accepted writes. `Steps()` returns this runbook. |

## Maintenance mode

`Maintenance{Dir}` is a per-application switch evaluated by the web server
on every request: while `<Dir>/enabled` exists, requests get HTTP 503 with
`<Dir>/index.html`. `Enable`/`Disable` touch only that marker, so no reload
or restart is involved. `CaddySite(domain, upstream, healthPath)` renders a
Caddyfile site block that implements this with a `file` matcher and
`file_server { status 503 }`, exempting the health path so monitoring keeps
seeing the application. The block validates with `caddy validate`.

## Databases

`Dumper` is the seam for the agent's backup pipeline, so migration dumps can
be retained and verified off-site like scheduled backups. `Postgres{Bin}`
uses `pg_dump --format=custom` (verified with `pg_restore --list` before the
file is renamed into place) and `pg_restore --clean --if-exists
--single-transaction --exit-on-error`, which replaces stale objects on a
rehearsal target and can be repeated. The connection URL is passed as
`--dbname`, never through the environment or a shell.

There is no incremental logical sync: the write window at cutover lasts as
long as the final dump, its transfer and its restore. Measure it on the
rehearsal import (same data size, same hosts) before choosing the window.
Redis or other caches are not moved; queues in the database travel with it.

## Production sequence for one Rails application

Rehearsal (source keeps serving; no window):

1. On the target, provision the owner account, the pinned Ruby and Node
   installs, PostgreSQL with an empty database and role, and the web server.
   Run `PreflightTarget` with `Requirements{FreeBytes, Paths: toolchain,
   Ports: {app port}, Units: {all unit names}}`; fix every blocker.
2. On the source, `PreflightSource`, then `Export` with the real `Dumper` and
   the owner's crontab. Push with `SSHTransport` over the existing control
   connection.
3. On the target, `Import` with the target database URL and a
   `release.Plan` for the application (Ref is the bundle commit). Then
   `StartServices`, `Verify` by Host header, and exercise the application
   (sign-in, a background job run by hand). Stop the services again, leaving
   the units enabled and the schedule uninstalled.

Cutover (window = final dump + transfer + restore):

4. Lower the DNS TTL in advance. Announce the window.
5. `FreezeSource`: maintenance page on, Solid Queue (or Sidekiq) and timers
   stopped. Confirm no worker process remains (`Workers(units)`).
6. `Export` again (shared files and dumps only), `Push`, `ImportData` on the
   target. Compare row counts of the largest tables on both sides.
7. `StartServices` and `InstallSchedule` on the target; `Verify` with the
   Host header. The target is now the only writer.
8. Switch DNS to the target. The source keeps answering with the maintenance
   page until clients converge; once the name resolves to the target, its
   web server obtains the public certificate. `Verify` over TLS normally.
9. Keep the frozen source and the final bundle for the retention period.
   Rolling back after the target accepted writes needs a fresh dump from
   the target restored into the source, then `ThawSource` — never just
   `ThawSource`, which would resurrect stale data.

## What the agent still needs

- Routes and jobs: `POST /v1/native/apps/{id}/export`, `.../import`,
  `.../cutover/{step}` with the same reservation and log plumbing as deploys,
  and a dashboard runbook view driven by `Steps()`.
- A `Dumper` backed by the existing verified-backup flow, so every migration
  dump is also an off-site backup with a receipt.
- Caddy integration: render `CaddySite` into the host's site configuration
  for every managed application, so maintenance mode is available before a
  move is ever needed.
- Per-application `migrate.App` derived from the native release policy
  (root, units, environment files, domain, port, database resources).
