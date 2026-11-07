# Moving the control panel

The control panel can move to another connected server with a guided, resumable
move. Open **Workspace → Control panel**, choose the destination server and
review the move: destination checks, the server list the new panel gets, sizes,
the certificate, routing and the DNS addresses. Then start it.

1. **Prepare.** The destination installs the panel binary from its own verified
   bundle as a standby unit, the panel's data directory (rewritten for the new
   host), a `pg_dump`/`pg_restore` copy of the panel database, a copy of the
   panel's certificate in Caddy storage and the panel hostname on its ingress
   (admin-API route with a TLS subject, the agent proxy, or a validated
   Caddyfile site). This panel keeps working meanwhile. Privileged steps are
   fixed operations of the root upgrade helper (`lidza-control-move-v1`); the
   agent only stages their inputs.
2. **Cut over.** When the move is standing by, **Cut over** makes this panel
   read-only (every mutating route, webhook and agent report answers 503 "the
   control panel is moving"), lets running jobs drain, copies the latest data
   and activates the destination. From here the move can no longer be aborted.
3. **DNS.** Change the records it lists for the panel's hostname. The check
   asks the zone's authoritative nameservers, as application moves do. Once DNS
   and HTTPS answer on the destination, the old panel writes its moved marker,
   serves only a notice, and asks its agent to replace its Caddy site and stop
   its unit. The panel's address does not change.

Abort before the cutover removes everything from the destination. Standby and
moved panels never run the application (no ticks, jobs, updates or agent
calls). The move appears in the Activity drawer.

API: `GET /api/control/panel-move`, `POST /api/control/panel-move` (plan or
start), `POST /api/control/panel-move/{action}`.
