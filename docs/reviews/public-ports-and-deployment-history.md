# Host public ports, grouped containers and deployment visibility

Contract: [public ports](../public-ports.md). The manifest's existing public_ports
declarations remain unchanged. The agent uses only its marked IPv4 DOCKER-USER
chain and exact actual DNAT publications, preserving unrelated rules and INPUT.
Unsupported backends and ownership collisions fail explicitly. Public cloud
firewalls/SMTP policy remain external and reachability stays unverified.

Actual host acceptance used a dedicated rootful Docker28.4 Linux daemon inside a
privileged isolated container, a separate external-client network namespace and
an operator ingress DROP in DOCKER-USER. The daemon's network/firewall namespace
was separate from the session host and all production servers. Its cgroup parent
was dedicated to acceptance. No customer application or console was used.

The real stack lifecycle test passed:

* Managed PostgreSQL, declared publications and actual profile limits.
* TCP25 HTTP fixture and UDP7882 echo from the external namespace through DNAT.
* An unrelated published Valkey service reachable locally but blocked from that
  client, proving the allow did not broaden to unrelated publications.
* Inventory: exact host bind, target namespace/container, Docker publication,
  verified owned allow-rule presence, external reachability still unverified.
* Redeploy, agent/Docker port refresh, missing jump repair, rollback, failed
  candidate recovery, stop/start, existing database sizing preservation, retained
  data isolation and app removal. Removal leaves no owned ACCEPT rules and
  preserves the operator DROP.

These network fixtures are not SMTP delivery, public cloud reachability, provider
SMTP allowance or a new Thura application acceptance claim. The reproducible
isolated-host entry point is tests/public-ports-host.sh.

Python ownership/lifecycle regressions cover idempotence, shared chain owners,
conflicting publications, mismatched bindings, stopped containers, foreign rules
and unsupported forwarding order. Agent/control tests cover authentication,
app/release filtering and read-only permissions.

The deployment visibility regression reproduces history over4MiB, which exceeded
the control-panel decoder's existing bound and made the host's history disappear.
Agent summaries (`GET /v1/deployments?logs=0`) omit logs and bound list fields;
individual detail (`GET /v1/deployments/{id}`) preserves complete retained output.
The control-panel log page polls individual detail. A size failure now reports an
explicit reason instead of silently losing history. Accepted Deploy responses
remain visible while history refresh is unavailable. Existing legacy full-history
agent API remains available, but control and agent should be upgraded together.

Validation includes full Go race tests/vet, Python regressions, UI component tests,
all27 routes at320/390/768/1440px, wizard/MFA/console/restore/restart workflows,
live log scrolling/pause/offline recovery, copy controls, actual Deploy
request/queue/log-navigation and rejection, application container grouping and
centered diagnostic-filter geometry. Missing deployed commits now read "No release
deployed" and failed requests/results are visible directly on the app card.
