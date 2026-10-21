# Proposed stack version 2: additional services and runtime selection

Status: proposal, not an accepted manifest or a released runtime capability.
Do not put these fields in a production manifest yet. v0.2.101 accepts only
version1, one app and at most one each mail/office, three services total.
Unknown fields and roles fail closed. No production deployment is authorized by
this document. The application repository owns its integration and measurements.

## Existing supported contract

The repository path is deploy/lidza.stack.json. Version1 provides managed
PostgreSQL attachment, stable generated_secrets in encrypted agent state,
existing application STORAGE_DIR, per-service named persistent volumes, private
network aliases, explicit environment references including mixed APP_DOMAIN
literals, declared health commands, coordinated lifecycle/recovery and guarded
first-install bootstrap. Existing app settings override app manifest environment
defaults; sidecar environment is explicitly declared. Neither regenerated keys
nor default provider values may replace a saved installation choice.

configuration stage deploy/setup/optional describes validation ownership.
required_when only validates an optional setting against effective production
environment/credentials. It does not start/stop services or read the app database.
The existing proxy uses HTTP internally; its forwarding headers are not proof
that the original connection used TLS1.3.

## Minimal additional-service schema

Use version2 so old agents reject the manifest instead of ignoring orchestration
requirements. Preserve version1 behavior. Proposed new fields are:

* services[].role: add auxiliary. Keep exactly one app and at most one each
  mail/office. Auxiliary roles may repeat; names remain unique, max8 total
  services. Do not add vendor-specific matrix/ollama roles.
* services[].enabled_when: optional {name, equals}, using one declared nonsecret
  selector with an exact public-literal comparison. No shell, expression, secret
  comparison or automatic inference from a model name. App cannot be conditional.
  Missing/unknown selector values fail planning; never silently choose a provider.
* services[].database_bindings: optional bounded list of existing application
  binding keys. Join only the corresponding owned managed-database networks;
  never accept raw network names or Docker IDs. Credentials still use ordinary
  encrypted references. Version1 attaches those networks only to the app, so a
  new Matrix container cannot simply assume PostgreSQL is reachable. New logical
  database/schema/user creation and migrations need an explicit supported app
  provisioning flow; do not assume superuser credentials or silently retune an
  existing/shared database. Additional database containers need separate budgets.
* profile_resources: map of existing profile IDs to {services, database}.
  services maps every declared service name to {memory_mib, cpus}; database has
  the same budget shape. Explicit budgets for all declared services are required
  in version2. No auxiliary service may fall through to the database budget.
  Disabled services are not pulled, built, created or started. They consume no
  effective container budget. Existing host allowances remain512/1024/2048MiB.
* runtime_selection: {selectors, path, token_key, initial}. selectors maps each
  declared public selector name to its allowed public string values (a bounded
  nonempty enum). initial supplies validated first-install
  values. path is a validated private HTTP app path. token_key references an
  existing generated_secrets entry used only to authenticate this exchange.

A conditional service fragment is structurally:

```json
{
  "name": "local-ai",
  "role": "auxiliary",
  "policy": "hardened",
  "enabled_when": {"name": "AI_RUNTIME", "equals": "local"},
  "aliases": ["local-ai"],
  "environment": {},
  "volumes": [{"name": "model-state", "target": "/var/lib/models"}],
  "health_command": ["/service-health"]
}
```

It still needs a pinned architecture-correct image or build, and complete profile
budgets. No numeric budget is endorsed by this fragment. Matrix would be an
unconditional auxiliary service, with its own persisted registration/database
settings. Hardened keeps nonroot UID65532, read-only rootfs, bounded tmpfs,
cap-drop ALL and no-new-privileges. New volume ownership must be initialized
explicitly: a root-owned empty volume is not writable by that UID. If required,
version2 may explicitly allow the existing bounded init policy for auxiliary
services after policy tests; do not silently relax hardened or accept arbitrary
users/capabilities. Images must support the selected fixed policy.
No auxiliary host publications or Docker/host mounts are introduced.

Network aliases are service names/declared aliases. The app can use ordinary
manifest environment defaults such as http://matrix:8008 and http://local-ai:11434
only when those services/ports actually exist. Disabled local service URLs must
not override a saved remote provider. A model-specific health command verifies
that the selected model can load, not merely that a model server answers HTTP.
No GPU, model download, throughput or4GiB capacity claim is implicit.

## Ownership and automatic reconciliation

Deploy owns infrastructure identities, networks, stable bootstrap secrets and
private service locations. The app owns teams, registrations, domains selected
for its own features, storage/provider choices, policies and model selection.
The agent never edits the app database, invents a provider or rotates a retained
key to repair setup. App-side provisioning uses idempotent migrations and stable
registration IDs, with explicit failures instead of placeholder success.

For database-backed runtime choices, environment-only conditions are insufficient.
The proposed private runtime_selection endpoint authenticates with the declared
purpose-specific token and returns exactly:

```json
{"version":1,"revision":"opaque-app-config-revision","values":{"AI_RUNTIME":"remote"}}
```

Only declared selectors with manifest-allowed values are accepted. No images,
commands, ports, secrets, arbitrary URLs or resource increases can be supplied by
this response. The agent calls only the current app's recorded private published
address. It cannot follow redirects. Bound response size and deadlines. Expose
this path only on the private control exchange, never the public host proxy.

Before redeploy/reload, read and validate that snapshot while the current release
still runs, persist its revision/values in encrypted agent state and plan the
candidate from that immutable snapshot. Refresh before activation; if the revision
changed, stop and replan, retaining the running release. New installations use
explicit initial values. If the current app is unavailable, never silently reset
to those defaults: fail with guidance, or use an explicitly selected recorded
snapshot for recovery. The app-published
snapshot is authoritative for selectors named in runtime_selection. An explicit
operator environment value for one of those selectors must match the snapshot
or planning fails with guidance; it cannot silently override a published choice.
Secrets for this exchange must be carried across releases without rotation.

A wizard choice stored in the app database takes effect for container layout on
the next coordinated reload/deploy, not by magic from ordinary required_when.
Publication must keep the feature gated until its selected backend is ready.
Disabling local AI stops/removes its process at activation and retains its model
volume; selecting remote AI must not download/load a local model. Rollback uses
its recorded selection and release topology, and must fail before stopping
writers when incompatible with the currently published app configuration.
Named state volumes stay isolated and never get competing upgrade writers.

## Honest resource profiles

The current minimum already allocates3584MiB container limits and512MiB host
allowance. There is no remaining advertised allowance for Matrix/local inference.
Version2 must reject a selected service plan that exceeds its named profile's
budget; this is manifest-budget validation, not restoration of the removed
host-wide sum-of-unrelated-container-limits admission gate.

A4GiB claim needs new app-owned budgets and an actual whole-stack acceptance run
including Matrix and the selected model, cold starts, simultaneous document/mail
work, queues, model loading, native-architecture images, no-swap behavior and
agent overhead. Existing four-container evidence is insufficient. If local AI
cannot fit, its selection must require a larger validated profile or an external
provider; do not disguise unloaded-model memory as working local inference.
Existing shared/preexisting PostgreSQL is never silently shrunk or retuned.

## SMIP transport boundary

Current Caddy -> HTTP agent -> HTTP app routing cannot provide direct TLS1.3 to
SMIP, and forwarded proto is not authenticated TLS-version evidence. Do not set
request.TLS, trust arbitrary headers or disable SMIP's transport checks.

Two secure designs require separate implementation and acceptance:

1. End-to-end TLS passthrough to an app-owned TLS1.3 listener, with certificate
   lifecycle/SNI routing and explicit port ownership. Stock deployed Caddy HTTP
   routing has no such L4 contract; no extra proxy is hidden from resource claims.
2. A deliberately supported authenticated TLS gateway contract. The public edge
   enforces TLS1.3, its transport evidence reaches the app over authenticated
   protected hops, and a released framework/protocol API verifies the peer,
   authority, request binding, freshness and anti-replay as appropriate before
   exposing a distinct trusted-transport context. Backend TLS alone proves only
   that hop, not the original client's TLS version. Plain loopback/XFP cannot
   establish this contract.

The reusable transport-verification API belongs upstream, not in an app-local
bypass. Until a released supported design is available, direct-TLS-required SMIP
behind the current HTTP proxy remains an explicit blocker. Existing REST/WS/TLS
routing must not be weakened for it.

## Acceptance and release boundary

Before implementation is advertised, cover parsing/backward compatibility,
selected-service inventory/health/logs, disabled build/model absence, stable
settings/secrets/registrations, interrupted redeploy and failed-candidate recovery,
rollback compatibility, retained volume isolation, database sizing and backups,
private endpoint authentication/replay/stale-revision behavior, truthful budgets,
spoofed TLS evidence rejection and genuine production-mode app acceptance.
Publish an actual minimum agent/control version only after those checks pass.
