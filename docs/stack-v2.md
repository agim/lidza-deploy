# Stack manifest version 2

Minimum agent and control-panel version: **v0.2.102**. Upgrade both before
using this contract; older agents reject version2. Version1 remains supported
with its unchanged policies and resource profiles. See the
[isolated platform acceptance report](reviews/stack-v2-acceptance.md).

The path remains deploy/lidza.stack.json. Set version:2. Existing fields retain
their meaning. New capabilities are:

* Up to8 services: exactly one app, at most one mail/office, and repeated
  auxiliary roles with unique names. Auxiliary policies may be hardened or the
  existing bounded init policy. There are no arbitrary users, capabilities,
  mounts, host ports, GPU access or additional shared network namespaces.
* profile_resources maps supported profile IDs to services and database budgets.
  services maps every declared name (including conditional services) to
  {memory_mib,cpus}; database is a combined allowance for attached local managed
  databases. Budgets require >=16MiB/service, >=64MiB database allowance and CPU
  decimals >=0.01 with at most3 fractional digits. No unknown/missing budget keys.
* enabled_when:{name,equals} selects an optional non-app service by exact public
  selector value. Conditional services must use immutable pinned images, not
  build. This lets reload enable a previously absent service without fetching
  private repository source. Unconditional auxiliary services may use build.
  Disabled services are not built, pulled, created or started. Their model/data
  volumes remain retained; declaring a service does not initialize a local model.
* database_bindings lists existing application attachment keys for a service.
  The agent connects only their corresponding managed-database networks. External
  database bindings need no network attachment. A service sharing the app network
  cannot specify extra networks. Credentials use explicit environment references;
  no database/user/schema is implicitly created by a sidecar declaration.
* runtime_selection declares a purpose-specific authenticated private exchange.

Example selection declaration (same JSON keys are implemented):

```json
"generated_secrets": ["DEPLOY_RUNTIME_TOKEN"],
"runtime_selection": {
  "selectors": {"AI_RUNTIME": ["remote", "local"]},
  "initial": {"AI_RUNTIME": "remote"},
  "path": "/.well-known/deploy-runtime",
  "token_key": "DEPLOY_RUNTIME_TOKEN"
}
```

The path must be a safe /.well-known/ app path, distinct from the agent's domain
proof endpoint. The token key must be a generated credential key, outside the
platform-reserved namespace. Selectors are nonsecret names with allowed public
string values (max16 selectors and16 values each). Initial values are mandatory.
The token is generated once and persisted in encrypted agent settings. Never
return it in the response, print it or expose it to a browser.

For a current installation the agent requests the current app's recorded private
HTTP port, with Authorization: Bearer <token>, accepting HTTP200 only. It sends
Accept: application/json and Cache-Control:no-store, follows no redirects and
uses a5-second deadline and16KiB response limit. Response fields are exactly:

```json
{"version":1,"revision":"opaque-config-revision","values":{"AI_RUNTIME":"local"}}
```

Revision is a nonblank, control-free string up to128 characters. All declared
selector values, and no other values, must be present. The app authenticates the
exchange even though the agent blocks this path (including normalized subpaths)
on its public host proxy. Endpoint access cannot require published setup or ready
backends: otherwise first-run local provisioning would be circular.

The app returns **required topology over active plus explicitly saved desired
configuration**. An unsaved form edit must not change topology. A saved local-AI
draft may request local provisioning before model probes succeed. When switching
to remote, keep returning local while the active configuration still requires it;
return remote only after publication no longer needs local. The app increments
revision for relevant saved/active state changes. It owns provider/model/team/
storage/mail/policy persistence, registrations and idempotent initialization.
Deploy never rewrites those application database settings.

At first install (no current release), initial values apply, with any explicit
operator selector value taking precedence if allowed. With a running release,
the app's response is authoritative: an explicit operator environment value must
match or deployment fails with guidance. Do not persist automatic selectors as
operator overrides. Missing/unavailable/malformed endpoints or undeclared values
fail closed; existing installations never revert silently to initial defaults.

Planning persists the snapshot in encrypted state. The agent reads it again
immediately before stopping current writers. Changed revision or values aborts
before activation. It also validates the candidate's response after service/app
readiness: topology must match; subsequent releases must match the planned
revision. First install may adopt the candidate's actual revision when values
match. Configuration changes after these optimistic checks require another
coordinated reconciliation; this is not an app-database transaction or a
cross-process configuration-write lock.

Redeploy and reload reconcile selected services automatically. A newly enabled
pinned service is pulled only when selected. Explicit Start can use the recorded
release selection when the app was stopped. Rollback requires compatibility with
the app's current active/saved desired topology and refuses an unverified downgrade
without a selection contract. Named volumes and stable generated credentials are
preserved; old/new release writers never run concurrently. Removing a release
removes only its selected containers; normal app removal retains data under the
existing resource-retention rules.

To migrate an existing version1 application, first ship its private authenticated
endpoint and stable generated token using a version1 manifest. Then switch to
version2. A new token invented during a version2 update is not known to the old
process, so its failed authentication must not be bypassed with initial defaults.
Changing the token identity in-place is unsupported; retain it across releases.

Resource limits are manifest budget ceilings, not measured capacity guarantees.
The enabled plan plus declared database allowance and existing host allowance
must fit its selected profile's4/8/16GiB budget and2/4/8CPU ceilings. Disabled
service budgets do not count. This does not restore admission control based on
unrelated containers' ceiling sums. Existing managed database sizing is retained;
if attached database ceilings exceed the declared allowance, increase the
allowance/profile or explicitly change attachments. No shared/preexisting DB is
silently resized. Legacy database attachments without a recorded CPU limit are
unbounded and cannot establish a bounded v2 database allowance; explicitly
configure a bounded attachment rather than assuming a CPU limit. A4GiB Matrix/local-model claim needs new whole-app acceptance;
the prior four-container measurements do not establish it.

Settings advertises stack_manifest_versions:[1,2] and returns the committed
runtime_selection snapshot as public read-only metadata. Container inventory,
health, logs, stop/start, rollback, recovery, retention and backups use the same
owned application lifecycle. SMIP TLS1.3 transport is outside this extension and
remains gated on upstream verified transport support; no proxy-header bypass.

Protected TLS protocols may opt into `gateway_transport` on v0.2.103 or newer.
See the [protected gateway contract](gateway-transport.md); ordinary HTTP
forwarding headers never establish original-client transport evidence.
