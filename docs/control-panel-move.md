# Moving the control panel

The control panel can move to another connected server with a guided, resumable
move. Open **Settings → Control panel → Move**, choose the destination server,
choose what happens to this server afterwards, and review the move: destination checks, the server list the new panel gets, sizes,
the certificate, routing and the DNS addresses. Then start it.

1. **Prepare.** The destination installs the panel binary from its own verified
   bundle as a standby unit, the panel's data directory (rewritten for the new
   host), a `pg_dump`/`pg_restore` copy of the panel database, a copy of the
   panel's certificate in Caddy storage and the panel hostname on its ingress
   (admin-API route with a TLS subject, the agent proxy, or a validated
   Caddyfile site). This panel keeps working meanwhile. Privileged steps are
   fixed operations of the root upgrade helper (`lidza-control-move-v1`); the
   agent only stages their inputs. When this server stays, a **Link this
   server** step connects its agent to the new panel (see below) and verifies
   a call through the link at the standby panel.
2. **Cut over.** When the move is standing by, **Cut over** makes this panel
   read-only (every mutating route, webhook and agent report answers 503 "the
   control panel is moving"), lets running jobs drain, copies the latest data
   and activates the destination. From here the move can no longer be aborted.
3. **DNS.** Change the records it lists for the panel's hostname. The check
   asks the zone's authoritative nameservers, as application moves do. Once DNS
   and HTTPS answer on the destination, the old panel waits for cached answers
   to expire: it reads the records' authoritative TTL, asks public resolvers
   (1.1.1.1, 8.8.8.8, 9.9.9.9) for their cached answer, and goes on once every
   answering resolver returns the destination or the old TTL plus five minutes
   has passed; the retire step shows the time left. The old panel then writes
   its moved marker, serves only a notice, stops its unit, and its agent
   forwards the panel hostname to the destination (HTTPS, Host header kept)
   for 7 days, so visitors with stale DNS still reach the new panel. The
   panel's address does not change.

Abort before the cutover removes everything from the destination, and the
agent link. Standby and moved panels never run the application (no ticks,
jobs, updates or agent calls). The move appears in the Activity drawer.

## This server after the move

**Keep** (the default) leaves the old server in the server list for its
applications and for future ones. When the destination cannot reach its agent
(a loopback agent, or a firewall that admits only 80 and 443), the agent
connects out to the new panel's public HTTPS instead: an agent link
(`GET /api/agent/link`, authenticated with the agent's own token) that the
panel uses for every call to that server, so no port is opened. An HTTPS
agent URL the destination reaches can be given instead. **Retire** removes
the server from the list and is refused while it holds applications; shut it
down afterwards. Retiring only stops the old panel; its agent keeps running.

Application moves wait for cached DNS the same way after the switch, before
the source stops serving.

API: `GET /api/control/panel-move`, `POST /api/control/panel-move` (plan or
start), `POST /api/control/panel-move/{action}`.
