# Stack v2 platform acceptance

Implemented auxiliary services, conditional pinned service selection, explicit
profile resources, existing database network attachments and private revisioned
runtime snapshots. No production application, DNS, Caddy, TLS or provider setting
was changed by this work. Framework issue73 remains open and no SMIP trusted
transport bypass is included.

An isolated real Docker stack on the session daemon passed:

* Four initial service containers plus managed PostgreSQL. The auxiliary fixture
  had its own network identity and reached only an explicitly attached managed
  PostgreSQL network; its health command performed a real database ping.
* Actual profile memory/no-swap limits, native images, private SFU routing,
  persistence, application grouping/inventory, service health and runtime logs.
* A saved local selection enabled a fifth service via coordinated reload before
  publication; an active-local/saved-remote union retained that service. After
  remote publication the next reload stopped local execution.
* The private snapshot endpoint was blocked on the public app proxy. The agent
  used its retained generated token on the recorded private HTTP address.
* Subsequent deploy, interrupted rollback recovery across an agent restart,
  compatible rollback, failed-candidate restoration, stop/start, profile change
  without database retuning and removal/retained-resource isolation passed.

This is a platform fixture, not a real Matrix homeserver or model benchmark.
The model-side fixture uses pinned native Valkey to exercise conditional process
lifecycle and private connectivity. It performs no model inference and makes no
new4GiB capacity, model quality, delivery or vendor-application acceptance claim.
The application session owns the real services and whole-stack measurements.

Race-enabled regressions cover version1 compatibility, strict version2 schema,
conditional app/unsupported policy rejection, selected-only budgets, retained
DB allowances, missing bindings, stable credentials and encrypted snapshots,
initial explicit choices, active/saved-desired selection, preactivation revision
fencing, candidate revision/topology fencing, incompatible rollback refusal,
recorded start, response bounds/unknown fields/enums/redirect rejection, public
path normalization and credential masking. Disabled services are checked to
ensure their images/containers are not pulled or created. Conditional services
require immutable images; an unconditional auxiliary service may be built.

The endpoint protocol is an optimistic snapshot fence, not an app-database
transaction or a distributed configuration lock. Choices changing after final
verification need another reconciliation. The app must authenticate its endpoint,
report the union of active and explicitly saved desired topology, and retain
feature publication/probes until the selected backend is actually ready.

Validation completed locally: `go test -race ./...`, `go vet ./...`, and
`TEST_DOCKER=1 go test -v ./internal/agent -run '^TestDockerStackV2Lifecycle$'`
all passed. The final real Docker run completed in 60.488 seconds. Release CI
is additional packaging verification, not the source of this acceptance claim.
