# Automatic native inventory

Connecting a server is enough to include the native applications its agent has
registered. The control panel queues a synchronization at startup and repeats it every minute using
the released jobs pack. The Applications page shows the last sync status and
opens directly on the application list; it has no discovery or JSON upload forms.

Synchronization only sends authenticated GET requests to the connected agent:
`/v1/native/capabilities`, `/v1/native/discovery`, and `/v1/apps`. The capability
gate requires metadata-only native status with HTTP activation probes disabled.
Agents without native discovery are skipped. Sync never opens an SSH connection,
registers a service on a host, probes an app port, or sends a service action.

Already registered native apps are added to the dashboard automatically with
auto-deploy off, no webhook, and a fresh dashboard identity. Existing app records,
deployment preferences, server names, credentials and bindings are preserved.
Ownership, domain or service-binding conflicts retain the previous inventory and
produce a visible sync error. Failed saves restore the previous in-memory state.

Discovered services without a complete agent registration appear automatically
as observations. Unknown hostnames and repositories stay unknown. When the agent
provides a complete registered binding, the next sync promotes the observation
without a duplicate card. Missing observations are retained and marked missing;
offline agents never cause deletions. Manual imported observations remain intact.

This automates dashboard inventory only. Existing service enablement, stopped
states, releases, environment, databases and routing are unchanged. Native
deployments are configured separately through release policies; see
[native releases](native-release.md).

## What discovery reports

A release policy is the source of truth for an application's services. The
agent's `/v1/native/discovery` lists, in this order:

1. Every registered application with the services it runs today. For an
   application with system units (one the agent created under `apps_root`,
   or a configured policy with `unit_scope: "system"`) those are the
   policy's `units` and `preserve` entries that exist as unit files in
   `/etc/systemd/system`, in policy order. User units that an earlier move
   to system units left on the deployment user's manager are not the
   application's services and never appear. Applications without such a
   policy keep their user units bound to `<root>/<name>/current`.
2. Configured system-unit policies nobody has registered, as observations
   without an ID: metadata registration binds user units, so the panel
   shows them and leaves registration to the operator.
3. The directory scan of the native root. It exists to adopt existing
   applications that were laid out there before the agent managed them, and
   only proposes names that are not registered; it decides nothing about a
   registered application's services.

The panel compares each registration with the discovery entry of the same
name as sets of services, so process order on one side and alphabetical order
on the other are not a difference. Applications created through the panel are
already registered on the agent and recorded in the panel; discovery only has
to agree with that registration for them to stay synced, and an application
created on a server the panel did not record is adopted on the next sync.

A disagreement still retains the previous inventory, and the sync status names
the application and what differs, for example
`rails-shop: registered services [a,b] vs discovered [a]; previous inventory retained.`
A unit the policy names without a unit file (removed, or masked) shows up this
way; so does a panel record whose binding differs from the agent's.
