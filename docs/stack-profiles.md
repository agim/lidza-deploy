# Stack profile contract (version 1)

Status: implementation branch `feat/stack-profiles`. This document defines the
repository/platform boundary; do not assume support on an agent until its settings
response advertises `stack_profiles_supported: true`.

## Selection and budgets

Creation accepts `runtime_profile`: omitted/empty means the existing hardened
single-container runtime. Explicit choices are `minimum`, `standard`, and
`performance`. Settings PATCH accepts the same field; changing it on a deployed
stack queues a reload. Switching between single-container and stack mode after
first deployment requires a new application; existing data stays retained for explicit reattachment. Stack profiles are unavailable to pull-request previews.

| Profile | App + SFU | Mail + filters + Valkey | Office | New dedicated PostgreSQL | Host allowance |
| --- | --- | --- | --- | --- | --- |
| minimum | 512 MiB / 0.50 CPU | 1792 MiB / 0.75 CPU | 1024 MiB / 0.50 CPU | 256 MiB / 0.25 CPU | 512 MiB |
| standard | 1024 MiB / 1 CPU | 2560 MiB / 1 CPU | 3072 MiB / 1.5 CPU | 512 MiB / 0.5 CPU | 1024 MiB |
| performance | 2048 MiB / 2 CPU | 4096 MiB / 2 CPU | 7168 MiB / 3 CPU | 1024 MiB / 1 CPU | 2048 MiB |

These are resource limits, not throughput guarantees. The compact standalone total
is 3584 MiB of containers plus 512 MiB host allowance. A colocated Deploy control
panel, its database, other applications, and existing databases consume additional
capacity. Other running containers must have explicit memory limits; unbounded
containers cause a placement error rather than being counted as zero. Linux
kernel-reserved RAM is charged against the host allowance, up to
256 MiB and never more than half that allowance; service limits stay unchanged.
This allows a nominal 4 GiB VM whose usable MemTotal is slightly smaller while
rejecting hosts that cannot fit the actual container limits. Existing/shared databases are reused at their existing limits and are
never silently resized or tuned. No swap is allowed for stack services/new compact
DBs. Migrations remain the application's responsibility.

## Repository manifest

Commit `deploy/lidza.stack.json` inside the existing `deploy/` directory. It is strict
JSON, max 64 KiB, version 1. It is never passed to Compose. Unknown fields, unsafe
paths, unpinned vendor images, unsupported ports and policies are rejected before
building. No host binds, Docker socket, host networking, privileged containers,
user-chosen capabilities or arbitrary resource limits are accepted.

```json
{
  "version": 1,
  "required_environment": ["LIDZA_MASTER_KEY"],
  "generated_secrets": ["THURA_SETUP_TOKEN", "THURA_POSTFIX_TOKEN", "MAIL_RSPAMD_PASSWORD", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET", "COLLABORA_TOKEN_SECRET"],
  "services": [
    {
      "name": "app", "role": "app", "policy": "init",
      "build": {"dockerfile": "Dockerfile", "target": "compact"},
      "aliases": ["thura"],
      "environment": {
        "DB_MIGRATE": "true", "DB_MAX_CONNS": "8",
        "CACHE_URL": "redis://127.0.0.1:6379/1",
        "REALTIME_BUS_URL": "redis://127.0.0.1:6379/2",
        "MAIL_CLAMD_ADDRESS": "127.0.0.1:3310",
        "MAIL_RSPAMD_URL": "http://127.0.0.1:11333",
        "MAIL_RSPAMD_CONTROLLER_URL": "http://127.0.0.1:11334",
        "LIVEKIT_SERVER_URL": "http://127.0.0.1:7880",
        "LIVEKIT_PUBLIC_URL": "wss://${APP_DOMAIN}/rtc",
        "COLLABORA_SERVER_URL": "http://office:9980",
        "COLLABORA_APP_URL": "http://thura:3000",
        "THURA_SERVICE_HTTP_ORIGINS": "http://127.0.0.1:7880,http://office:9980,http://thura:3000"
      },
      "public_ports": [{"port": 25, "protocol": "tcp"}, {"port": 7881, "protocol": "tcp"}, {"port": 7882, "protocol": "udp"}],
      "http_routes": [{"prefix": "/rtc", "port": 7880, "strip_prefix": true}],
      "health_command": ["python3", "/opt/thura/app.py", "--health"]
    },
    {
      "name": "mail", "role": "mail", "policy": "mail",
      "build": {"dockerfile": "deploy/compact/mail.Dockerfile"},
      "share_app_network": true,
      "environment": {"THURA_POSTFIX_MANAGED": "true", "THURA_POSTFIX_TOKEN": "${THURA_POSTFIX_TOKEN}", "MAIL_RSPAMD_PASSWORD": "${MAIL_RSPAMD_PASSWORD}"},
      "volumes": [{"name": "postfix-queue", "target": "/var/spool/postfix"}, {"name": "rspamd-state", "target": "/var/lib/rspamd"}, {"name": "clamav-signatures", "target": "/var/lib/clamav"}, {"name": "mail-filter-learning", "target": "/var/lib/thura-valkey"}],
      "health_command": ["python3", "/opt/thura/compact.py", "--health"]
    },
    {
      "name": "office", "role": "office", "policy": "office",
      "image": "collabora/code@sha256:REPLACE_WITH_TESTED_ARCHITECTURE_CORRECT_DIGEST",
      "command": ["--o:ssl.enable=false", "--o:ssl.termination=true", "--o:net.service_root=/api/v1/collabora", "--o:storage.wopi.alias_groups[@mode]=groups", "--o:storage.wopi.alias_groups.group[0].host=http://thura:3000", "--o:storage.wopi.alias_groups.group[0].host[@allow]=true", "--o:admin_console.enable=false", "--o:logging.level=warning", "--o:num_prespawn_children=1", "--o:per_document.max_concurrency=1"],
      "health_command": ["/usr/local/bin/office-health"]
    }
  ]
}
```

The example's digest is deliberately a placeholder. The application team must pin
its tested image/index; Docker selects the host architecture or fails explicitly.
The office example assumes the image includes `/usr/local/bin/office-health`;
distroless vendor images commonly lack curl and require an application-owned
derivative with a static discovery probe. Build context is always repository root, without `.git`; only the Dockerfile and
optional target are selectable. Build arguments/secrets from the manifest are not
supported. Health commands are argv, not a host shell; each has a bounded timeout.

App service gets its configured/generated environment plus manifest settings.
Sidecars get only their declared environment, with `${ENV_KEY}` references resolved
from app settings. `${APP_DOMAIN}` is the primary HTTPS hostname. Credential-like
keys must be references, never literal secrets. Secrets are generated only when
missing, persisted in encrypted agent state, and masked in normal API responses.
The original `LIDZA_MASTER_KEY` must be provided; Deploy never replaces it.
Existing sealed credentials still use the framework's released decryption API.

## Runtime and routing

Exactly one `app` role and at most one each `mail` and `office` are accepted.
The app is named/aliased per release on a private, app-incarnation-specific network;
mail may share its network namespace. All containers are created before starting. The app namespace owner starts first
and waits for Valkey; mail/office then start immediately, without waiting for app
HTTP readiness. This avoids a circular startup dependency.
The app HTTP port 3000 and route ports publish on random loopback ports only.
The existing host HTTPS proxy forwards `/rtc` and WebSocket upgrades to port 7880,
stripping `/rtc`; other paths reach the app. Office 9980 stays private.

Public ports are limited to app-owned SMTP TCP25 and RTC TCP7881/UDP7882. Conflicts
with other apps or host processes fail explicitly; other containers are never
stopped to claim ports. `THURA_RTC_NODE_IP` must be set to the server's externally
reachable address when automatic SFU address discovery is unsuitable. Configure
SMTP TLS in the application/mail image; arbitrary host certificate binds are not
part of this contract.

Policies are fixed by the agent: `hardened` keeps UID65532, read-only rootfs and no
capabilities; `init` allows app root initialization with CHOWN/DAC_OVERRIDE/FOWNER/
SETUID/SETGID/KILL only; `mail` additionally allows low-port binding and process ownership;
`office` preserves the pinned vendor image's USER and supplies bounded ownership/chroot capabilities.
All retain cap-drop ALL, PID/log/resource limits, no host mounts,
no host namespaces and no Docker access. App and mail retain no-new-privileges. Office requires an explicit non-root image
USER and has an office-only exception to no-new-privileges so its vendor helper
can acquire its file capabilities. Its bounding set is exactly CHOWN, FOWNER and
SYS_CHROOT; capabilities outside this set cannot be gained. This exception does
not change the single-container, app-init, or mail policies.
Root initialization policies have writable
container layers but only declared named volumes persist.

## Persistence, lifecycle and diagnostics

App storage reuses the existing STORAGE_DIR attachment. Sidecar volume names are
namespaced by app incarnation and retained across deployment, stop/start, reload,
rollback and removal; reusing an app ID cannot adopt old data accidentally.
No sidecar shares writable volumes with a concurrently running old release.

Images build first. Activation stops the old release before starting the candidate;
this introduces a maintenance interval, rather than claiming zero-downtime upgrades.
Readiness requires app `/readyz` plus every declared service health command. Failure
removes the candidate and restarts the previous stack. Rollback checks capacity for the previous release’s recorded profile, stops current
writers, starts previous services and checks readiness; failure restores current.
Desired settings remain configured for the next reload, while rollback runs the
previous release’s recorded runtime and environment, as with single-container rollback.
Removal deletes owned containers/release networks and retains persistent volumes
and the managed database/backups, following the platform's retained-data policy.

Release metadata records service images, container names, routes and the validated
manifest, enabling reload/rollback without fetching a mutable branch. Container
inventory includes sidecars with app/role ownership and resources. Stack container restart controls queue a coordinated reload, rather than breaking
the shared network namespace with an isolated app restart. Runtime logs
include bounded per-service output, scrubbed using application credentials.

## First-install setup readiness

Apps whose production `/readyz` intentionally returns 503 until an operator
publishes setup may opt into this top-level manifest declaration:

```json
"bootstrap_readiness": {
  "path": "/api/v1/health",
  "readyz_body_contains": "Server setup is required",
  "setup_path": "/setup"
}
```

Only the first installation (or an unfinished bootstrap release) may use it.
The agent still probes `/readyz`: fallback requires HTTP **503** and the exact
case-sensitive declared marker, a successful HTTP200 from the declared private
app health path, and every service health command succeeding. Generic outages,
connection errors, unrelated503s, unhealthy sidecars and established releases
cannot use this fallback. The application keeps its real `/readyz`503 and API
setup gate; no test mode or framework bypass is used.

The release is marked `current.stack.bootstrap=true`, the deployment status is
`setup_required`, and the GUI shows **Setup required** with an **Open setup**
link. The app's existing production setup guard controls access. Publishing setup
and reloading must pass genuine `/readyz`200, which clears the bootstrap marker.
A release that already passed genuine readiness cannot regain bootstrap eligibility
just because a later candidate returns a setup marker.
