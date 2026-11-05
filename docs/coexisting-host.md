# Agents on an existing application host

Set `"host_mode": "coexist"` when adding a deployment agent beside existing
applications and independently managed host services. The default empty/managed
mode retains the existing dedicated-host behavior. Unknown values are rejected
before manager construction writes state.

Coexist mode disables automatic swap provisioning, legacy build-helper cleanup,
console-root/control-boot service migrations, host-wide maintenance scheduling,
manual host-wide cleanup and enabling its policy. It does not vacuum journald or
prune the host's Docker build cache. `/v1/maintenance` reports
`host_automation_blocked: true`. These checks apply at the underlying methods,
not just in the dashboard. An empty agent inventory starts without Docker calls.

This mode does not install Docker, edit Caddy or host services, adopt existing
apps, import schedules, or redirect public traffic. Use a separate state directory
and explicit private listener/port allocations. Configure existing certificate
storage explicitly when using public-TLS stack services. The fresh-host installer
is not a migration or coexistence installer and must not replace an existing
server's routing configuration.

Normal application-scoped behavior continues for explicitly registered container
resources: builds/deployments, route/port reconciliation, database backups,
workers, schedules and service TLS updates. Those definitions confer management
ownership; do not import an arbitrary Docker inventory or a foreign agent's live
release ports. Explicit administrator console/SSH and application actions remain
privileged operations; coexist mode is not an OS sandbox or a substitute for
reviewing live changes. Manual per-database backup retention remains available.

An independent `ingress_only` process remains a routing reader, regardless of
host mode, and does not become a deployment or maintenance manager.

## Preserve existing hostnames

Set `reserved_domains` to the existing applications' DNS names before allowing
new deployments. A `*.tenant.example.com` entry reserves all subdomains beneath
that suffix; include `tenant.example.com` separately to reserve the root too.
At most 500 unique lowercase names or leading wildcard subtrees are accepted.

Reservations block primary-domain and alias assignments, settings changes,
customer-host routing and certificate permission checks. Startup rejects an
owned container inventory that overlaps them. They also apply to independent
ingress snapshots, so stale state cannot route or authorize a reserved name.
Existing applications continue through their current web server routes.

Import the reviewed existing web-server hostname inventory into this list; it
does not automatically track changes made by another deployment manager. Update
the reservations through the same reviewed host configuration process when new
independently managed applications are added. No routes or DNS are changed by
reserving a name.
