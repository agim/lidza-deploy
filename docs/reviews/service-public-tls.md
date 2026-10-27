# Public service certificate acceptance

Release: v0.2.105. This capability belongs to Deploy's host orchestration; it
does not modify the framework, issue a replacement private gateway identity,
change operator mail/storage/AI selections, or deploy a production application.

## Real isolated Docker evidence

`TEST_DOCKER=1 go test -v ./internal/agent -run '^TestDockerPublicTLSSTARTTLSRenewal$'`
passed in 5.419 seconds. A real SMTP fixture started with no certificate and did
not advertise STARTTLS. After host-side provisioning it completed a real TLS1.3
handshake with the correct hostname and an explicitly trusted isolated CA. A
renewed certificate changed the peer fingerprint on the same running container,
with zero restarts. The service could not write to its certificate bind mount.
This tests actual transport and mount behavior, not a mock SMTP extension reply.

`TEST_DOCKER=1 go test -v ./internal/agent -run '^TestDockerPublicTLSStackLifecycle$'`
passed in 76.637 seconds. The real managed PostgreSQL/stack fixture carried the
public certificate through creation, publication, reload, saved/active selection
changes, rollback, interrupted rollback recovery after agent restart, failed
candidate recovery, stop/start, profile changes and removal. Actual Docker
mount inspection proved read-only publication; each checked release exposed only
the expected hostname certificate. Managed database attachment, persistent data
and resource sizing were preserved. Removal deleted only the app-incarnation's
copied certificate directory, not Caddy's originals or another app's resources.

These tests use an explicitly trusted local test CA and Caddy's storage layout.
They do not claim a new public ACME issuance, a production change, SMTP delivery,
cloud firewall reachability or Thura/Postfix application acceptance. The app
repository owns its certificate watcher/configuration and real Postfix behavior.
No Caddy store or Docker socket is mounted into any certificate-consuming service.
The SMTP fixture ran as uid1000 to match this executor; root-agent uid0 delivery
uses the same checked owner-only file/chown path. A chmod regression covers the
systemd UMask0077 bind-root traversal requirement without relaxing key permissions.

## Regression checks

The full Go race suite and vet pass. Tests cover matching trusted chain/key,
exact hostname authorization, wildcard/multi-domain/untrusted/expired/mismatched
rejection, bounded files, symlink rejection, cold first install, last-valid
retention, expired retained status, concurrent sync, periodic first provisioning
and renewal, restart stability, read-only mount construction, immutable reader
UID paths, revocation and removal fencing. Invalid source material never replaces
a valid published pair. Atomic generation switching and a single PEM bundle let
consumers avoid mixing key generations. Readers must reload their TLS context.

The worker only runs for Docker runtimes. The first full race run found that
starting it for fake runtimes raced with older fixtures' direct state setup; the
worker was restricted to its actual runtime and the full suite then passed.
No existing assertion was weakened or skipped to pass that check.

TypeScript typechecking, all40 UI tests in12 files and the production UI build
pass. The Containers page shows pending, ready, expiration and safe warnings.
The control panel forwards typed metadata, never certificate/private-key bytes.
Ready means delivery is valid, not independent public listener verification.

## Operational contract

See [the exact manifest and environment contract](../service-public-tls.md).
Existing manifests and single-container deployments do not opt in and retain
existing behavior. Default systemd Caddy storage works without additional config;
custom stores/private CAs require explicit host-admin configuration. First issuance
remains asynchronous to avoid requiring an HTTPS route before the route exists.
Pending/expired/invalid credentials must not cause a consumer to advertise broken
STARTTLS. Renewal sync is every minute and does not run repository-defined host
commands, reload other apps, replace Caddy credentials or open public ports.
