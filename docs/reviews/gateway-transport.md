# Protected gateway acceptance

Framework dependency: github.com/agim/lidza v0.1.125, release commit 16f45f5.
Deploy consumes the released API; it does not fabricate Request.TLS or implement
an app-local attestation substitute. Source/release builds use Go 1.27.3 to fix
GO-2026-6809 certificate verification. Production was not changed or redeployed.

## Actual protected flow

An isolated stock Caddy 2.10.2 TLS edge, actual agent Proxy on verified mTLS, and
actual Docker app on its separate mTLS listener passed protected POST traffic.
Client-forged origin headers were overwritten. Body, method, target and public
authority remained bound to the protected flow. The app received distinct
original-client context while Request.TLS remained the backend connection.
Original TLS 1.2 and ordinary HTTP ingress with forged evidence were rejected.
Ordinary internal HTTP remained available. Private app port 3001 was published
only to an ephemeral loopback host port; no public firewall port was added.

`TEST_DOCKER=1 LIDZA_TEST_CADDY=/path/to/caddy go test -v ./internal/agent -run
'^TestDockerProtectedStackLifecycle$'` passed in 78.672 seconds. It exercised real
edge -> agent -> Docker app requests through initial guarded setup, publication,
reload/redeploy, saved/active runtime-selection changes, rollback, interrupted
rollback recovery across an agent restart, stop/start, profile change and removal.
The managed PostgreSQL attachment and its limits were preserved. A candidate
whose private TLS listener was disabled failed readiness and the previous release
was restored; public protected traffic still succeeded after recovery.

These are platform fixtures, not vendor SMIP, Matrix, local-model throughput,
Office/media or expanded 4 GiB application acceptance. The application repository
owns those protocol and whole-stack tests. No production readiness claim follows
from a fixture's protected response.

## Regression and release checks

The full `go test -race ./...` and `go vet ./...` passed. Focused cases cover:

* Stable encrypted per-incarnation keys, saved provider settings, managed-setting
  privacy/immutability and decoded/encoded private-key log redaction.
* Exactly one of 100 concurrent claims across independent host verifier instances,
  replay rejection after restart, cancellation, journal corruption, bounded
  capacity and expiry reclamation. The journal is separate from deployment history.
* Certificate renewal under retained keys/CA with stable pins, mismatched trust
  rejection, forbidden listener bindings, route conflicts and transport downgrade
  refusal. Rollback cannot remove protected paths or fall back to ordinary HTTP.
* Capability metadata forwarded through the control panel.

AMD64 and ARM64 bundles built locally. Root/source installer checks passed,
including repeated installation, no implicit hostname migration, archive/checksum
rejection and official Go-download fallback failures. The installer now pins
verified Go 1.27.3 bootstrap checksums. `govulncheck` found no affected code; one
required-module advisory was reported as unreachable by this application.

Framework acceptance additionally used a real shared Valkey claim backend,
100 concurrent claims, verifier client restart, forged/changed/stale/replayed
metadata, untrusted certificates, store failure and gateway-key rotation. Its
native Caddy -> mTLS relay -> mTLS app test rejected original TLS 1.2 even with
TLS 1.3 on both private hops.

## Operational boundary

The host helper prepares a new private directory and reviewable configuration;
it does not install it or touch live services. Host CA/key rotation requires
coordinated overlapping trust and sender changes, followed by draining old
connections. Per-app leaves renew on deploy/reload before expiry under stable
keys; CA replacement is an explicit migration. Nonce storage must be retained.
An authorized compromised gateway remains an explicit trust boundary. Application
identity, signatures, nonce checks, authorization and publication gates remain
required. No trust is granted by X-Forwarded-Proto, loopback or backend TLS alone.
