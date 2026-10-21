# Shared-server capacity policy

Deployment, reload and rollback no longer reject stacks using host MemTotal,
summed Docker memory ceilings, or the presence of unbounded unrelated containers.
Profile memory/CPU limits, no-swap configuration, resource ownership, public-port
conflict checks, original-key checks and genuine readiness remain enforced.
Existing database limits are not changed.

Validation:

* Full `go test -race ./...` and `go vet ./...` passed.
* `TEST_DOCKER=1 go test -race ./internal/agent -run '^TestDockerStack' -v -count=1`
  passed all three isolated acceptance tests (54.818s).
* The lifecycle test keeps an unrelated unbounded idle container and an unrelated
  idle container with a 64 GiB memory ceiling running throughout deployment,
  redeployment, reload/start, rollback and failed-candidate recovery. Neither
  container allocates large buffers. Profile limits are checked with real Docker
  inspect; app/service health, managed PostgreSQL reuse, storage persistence,
  isolation and removal checks continue to pass. Fixtures are cleaned up.

The selected profile describes service ceilings and host sizing guidance; these
ceilings do not reserve RAM. Operators can deploy on shared hosts without a
platform placement gate. Actual memory pressure is still governed by Linux and
Docker. No running production console or customer application was used here.
