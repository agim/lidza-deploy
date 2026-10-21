# Stack architecture, readiness and diagnostic visibility

Stack images are checked against the Docker daemon's native Linux CPU before
activation stops any current writers. Builds and pinned pulls select that native
platform explicitly. BuildKit's InvalidBaseImagePlatform warning also rejects a
foreign pinned FROM, including a derivative labelled with the native target.
Use immutable multi-architecture indexes or matching architecture-specific pins.
This does not verify arbitrary binaries copied into otherwise native images.

Readiness checks every declared service even when the application is behind its
first-install setup gate. Diagnostics identify each failing service and container,
include bounded exit/OOM/restart state and the latest 50 log lines on the existing
secret-redacted path. A stopped/dead/OOM candidate fails promptly; three consecutive
observations of restarting fail early and invoke existing previous-stack recovery.
Ordinary cold-start health failures retain the existing eight-minute budget.
No app or sidecar health checks are disabled for bootstrap.

Live diagnostic output checkpoints encrypted agent state at most once per five
seconds while output arrives, in addition to final status saves. Up to five
seconds of recent output can still be lost on abrupt termination. Build detail
responses and browser API requests use no-store to avoid stale operational data.
The original report's delayed output was not independently reproduced; it must
not be attributed conclusively to browser caching or checkpoint timing.

The reported server menu failure also did not reproduce in desktop/mobile
browser tests. Server destinations now use native anchors, with a regression
covering actual menu activation, routing and the locked console. MFA remains
required. Primary-domain DNS/TLS error details are now visible in app Settings.

The production TLS failure was caused by DNS pointing at another server, not
by a missing Caddy route. The initial public HTTP308 observation came from the
wrong destination; attributing it to the deployment host's Caddy configuration
was incorrect. The application session subsequently verified that both the
on-disk and live production Caddy configurations already had the HTTP proof
exception, and local HTTP proof returned200. The agent correctly refused
certificate authorization while DNS still pointed elsewhere.

The operator corrected the authoritative A record. Public resolvers then agreed,
while the production provider resolver temporarily retained the old answer until
its TTL expired. Resolver convergence and successful public TLS verification are
separate observations; the DNS correction alone does not establish TLS recovery.
No production Caddy change, app restart or redeploy was needed for this diagnosis.

The standalone deploy/Caddyfile sample did lack the HTTP proof exception and was
corrected to match the existing installer. That sample improvement is separate
from the production incident. Binary upgrades do not rewrite live Caddy config;
do not change a verified correct configuration to compensate for wrong DNS.

Thura's application session reports its corrected multi-architecture source
bc01d2e reached setup_required with all declared service health checks passing.
That is separate application evidence, not a new platform acceptance claim here.

Validation passed: full Go race suite and vet; 39 UI tests/typecheck/build; all
27 routes at320/390/768/1440px; desktop/mobile wizard/MFA/console workflows;
server-menu native-link navigation and primary-host error display; live-log
scroll/pause/offline/recovery. Actual isolated Docker checked native acceptance,
foreign image rejection and a restarting process's redacted runtime evidence.
In that Docker backend, explicit native-platform build refused the locally
imported foreign-only FROM during source resolution; the BuildKit warning branch
is covered separately by regression. Real local Caddy certificate issuance and
validation of the corrected sample Caddyfile passed. No production restart,
redeployment, Caddy edit or TLS recovery is claimed by these tests.
