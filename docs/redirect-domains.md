# Redirect domains

Any application, whatever its runtime, can keep redirect domains: hostnames
that answer only with a redirect. Open the application's settings, **General**,
and choose **Add redirect domain** under **Redirect domains**. Each entry has:

- **Hostname**, for example `old.example.com`;
- **Redirect to**: the primary hostname, another hostname of the application or
  a custom hostname;
- **Status**: 301 or 308 for a permanent move, 302 or 307 for a temporary one;
- **Keep path and query** (on by default): `old.example.com/a?b` goes to the same
  path on the target; off, every request goes to the target's home page.

Point the hostname's DNS to the server with the A/AAAA records shown under it.
The certificate is issued once DNS arrives, and the line under the hostname says
when HTTPS is ready.

Redirects are answered by the web server, not the application, so they keep
working while the application is stopped or in maintenance. On hosts whose
existing Caddy owns the web ports through its admin API, each application gets
one managed route (`@id lidza-redirect-<id>`) and its redirect hostnames join
the TLS automation subjects; a reconcile loop republishes the routes and
withdraws those of removed applications. Otherwise the agent's own proxy answers
them before any application routing, and on-demand certificates accept them.

A hostname belongs to one application only: redirect domains never overlap any
application's hostnames or redirects, reserved hostnames or the control panel
hostname, anywhere in the fleet, and a redirect cannot point at another redirect
domain. The panel warns about external targets and hostnames that a Caddyfile
already serves.

API:

    GET /api/control/apps/{id}/redirects
    PUT /api/control/apps/{id}/redirects   {"redirects":[{"host","target","status","keep_path"}]}

Each change is audited as `app.redirects.update` with the added, removed and
changed hosts. A [move](moving-applications.md) recreates the redirect domains
on the destination, lists their DNS records with the cutover and waits for
their DNS too. Deleting an application keeps its redirect domains in the
[archive](deleting-applications.md) and restore brings them back.
