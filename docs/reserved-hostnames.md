# Reserved hostnames

Set `reserved_domains` in the agent configuration to hostnames that other web
server routes on the host already serve. A `*.tenant.example.com` entry reserves
all subdomains beneath that suffix; include `tenant.example.com` separately to
reserve the root too. At most 500 unique lowercase names or leading wildcard
subtrees are accepted.

Reservations block primary-domain and alias assignments, settings changes,
customer-host routing and certificate permission checks. Startup rejects an
owned container inventory that overlaps them. They also apply to independent
ingress snapshots, so stale state cannot route or authorize a reserved name.
Reserving a name changes no routes or DNS.

## Retired configuration keys

- `host_mode` is ignored. Every full agent manages its host: host updates and
  reboots, the managed firewall, on-demand Docker installation, swap, cleanup
  and data services (PostgreSQL, Redis, ClickHouse) for created applications.
  An agent that still has the key logs one notice at startup; remove it.
- `native_only` is no longer supported and is refused at startup. Run a full
  agent, or an `ingress_only` reader for routing only.
