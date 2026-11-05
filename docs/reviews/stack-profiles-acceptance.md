# Stack profiles: platform acceptance

Branch: `feat/stack-profiles`. Contract: [stack-profiles.md](../stack-profiles.md).
The repository manifest is **deploy/lidza.stack.json**, version 1. The root-file
framework check remains enabled; no framework allowlist exception is needed.

## Scope and evidence

Platform tests deploy three real fixture services plus a real managed PostgreSQL
container on the isolated local Docker daemon. The checkout is a local test seam;
builds, volume/network attachment, resource enforcement, health, HTTP proxying,
state persistence and lifecycle operations use actual Docker. This is not an
application benchmark or a vendor-image editing test. The integrating application
separately owns its production-mode setup, SMTP/AV, office editing and RTC acceptance on an
actual 2 CPU / 4 GiB VM. No production console is used by these tests.

Verified platform behavior:

- New dedicated PostgreSQL gets 256 MiB / .25 CPU for the minimum profile,
  non-superuser application credentials and the existing database attachment path.
- App/mail/office container limits match the profile, with memory+swap equal so
  the containers cannot consume host swap. App storage uses the existing platform
  attachment; mail state uses an app-incarnation-scoped named volume.
- First install publishes a distinctly labeled setup UI only after the explicit
  setup marker, bootstrap HTTP health and all service checks pass. The real
  `/readyz` and protected fixture API remain HTTP 503 until publication. A reload after
  publication passes genuine readiness and clears the bootstrap state.
- Application readiness includes a real PostgreSQL connection and a service in
  the app's shared network namespace. Service health is shown in inventory.
- HTTPS-proxy upstream routing sends `/rtc/...` to the private SFU HTTP binding
  and removes the routing prefix. The remaining app paths keep the app target.
  Browser WSS and actual SFU media are application-side acceptance requirements.
- Reload, rollback, failed candidates and stop/start preserve files. Previous
  sidecar writers are stopped before replacement services start.
- A host-owned RTC port causes candidate failure and restores the old stack;
  no other process is evicted. Committed app port reservations are checked too.
- Encrypted state survives a simulated agent process restart during a rollback.
  Recovery stops the uncommitted writer, restores the committed release, refreshes
  its loopback routes and checks service readiness before recovery completes.
- Removal retains database/storage/mail data. Reusing an app ID creates a new
  resource identity instead of adopting the retained volumes.
- Attaching an existing database and changing a deployed stack profile do not
  resize, retune or replace existing/shared databases.
- Cleanup checks ownership labels and leaves unrelated colliding network and
  container names untouched; candidate cleanup does not delete reused image tags.
- A real root supervisor can health-check and terminate a UID65532 child with
  the bounded init KILL capability. A negative control without KILL is denied.
- The existing hardened two-application Docker deploy/redeploy/rollback smoke
  test passes independently of stack support.

## Vendor runtime findings

A real application integration identified two requirements that the fixture cannot prove:

1. The root app supervisor needs KILL to signal workers after dropping their UID.
   The fixed init policy now includes it while retaining no-new-privileges.
2. The official non-root Collabora helper uses file capabilities. The office
   policy preserves the vendor USER, requires it to be non-root, and has an
   explicit office-only exception to no-new-privileges with exactly CHOWN, FOWNER
   and SYS_CHROOT in its capability bounding set. The other policies retain NNP.
   Actual document editing under this policy remains an application-side acceptance
   requirement; a healthy discovery endpoint alone does not prove editing.

Distroless office images need an application-owned health executable. The application's
probe must query its configured `/api/v1/collabora/hosting/discovery` service root.
The platform executes the manifest's health argv, with a bounded deadline.

## Application integration

An integrating multi-service application separately reported guarded first setup,
mail/storage wizard probes, publication, coordinated reload to `/readyz` HTTP 200,
document editing, mail and scanning checks, and rollback with preserved file and
database state on a 2 CPU / 4 GiB ARM64 VM. That is application-side evidence,
separate from the platform fixture results above. It does not establish public
DNS/TLS/MX, TURN or peer-media behavior, or throughput capacity.

## Platform verification

The completed checks include the full Go race suite and vet, 29 React tests,
TypeScript checking and a production console build, Linux amd64/arm64 builds,
real Docker stack lifecycle and legacy single-container acceptance, and browser
profile workflows at 390/1440 px. The broader layout sweep covers 27 routes at
320/390/768/1440 px. Placement tests reject unbounded other containers and count
existing database limits without retuning them.

## Reproduce

From the prepared checkout, source `scripts/env.sh`, then run:

```sh
go test -race ./...
go vet ./...
TEST_DOCKER=1 go test ./internal/agent -run '^TestDockerStack' -v -count=1
TEST_DOCKER=1 go test ./internal/agent -run '^TestDockerTwoAppsRedeployRollback$' -v -count=1
npm run typecheck
npm run test:ui
npm run build
```

Serve `web/static` locally and set TEST_WEB_URL for browser checks:

```sh
node tests/browser/stack-profiles.cjs
node tests/browser/template-redesign.cjs
node tests/browser/template-workflows.cjs
```

The profile browser test checks creation with managed PostgreSQL and retention of
profile edits across settings tabs at 390 and 1440 px. The broad browser sweep
checks 27 routes at 320, 390, 768 and 1440 px, including spacing and overflow.

A nominal 4 GiB VM can expose less RAM through Docker's MemTotal. Kernel-reserved
RAM is charged to the existing host allowance within the documented bound;
container limits do not shrink. Other running container limits remain included.
These limits describe placement and isolation, not validated throughput or a claim
that a colocated control panel and arbitrary other services fit in 4 GiB.
