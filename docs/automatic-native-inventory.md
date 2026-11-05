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
