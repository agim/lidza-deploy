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

A separate confirmed production routing problem returned HTTP308 from
/.well-known/lidza-deploy-host even with a valid nonce. The agent requires a
fresh HTTP200 proof before authorizing on-demand TLS, so that redirect blocks
certificate issuance. The installer already contains the correct exception;
the standalone deploy/Caddyfile sample now matches it. Binary upgrades do not
rewrite an operator's live Caddy configuration. Add the HTTP proof route before
the general redirect in the existing configuration, validate and reload Caddy;
preserve management hostnames and other routes. No production configuration or
deployment was changed by this session.

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
