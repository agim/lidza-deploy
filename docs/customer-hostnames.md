# Customer hostnames and automatic HTTPS

Requires Deploy v0.2.35 or newer on the control panel and each hosting agent.
The hosted app must be built with Līdza v0.1.107 or newer and implement
`App.TLSHosts(ctx, host)` to approve only hostnames belonging to its customers.
Updating Deploy does not rebuild or upgrade hosted applications automatically.

1. Deploy the app successfully under its primary hostname.
2. In **Applications → Settings**, enable **Serves customer hostnames** and save.
   The same opt-in is available when creating an app; it defaults off.
3. Customers point their hostname (for example `mta-sts.customer.com`) to this
   hosting server, using a CNAME to the app's hostname or appropriate A/AAAA
   records. Every published address must reach the host. Ports 80/443 must be
   reachable; restrictive CAA records can prevent certificate issuance.
4. On the first HTTPS connection, Caddy asks the agent. The agent asks the
   running app's loopback `/tls-allow?domain=...` endpoint. An empty HTTP 200
   approves the name; redirects, errors and HTML fallbacks do not.
5. Caddy obtains and automatically renews a certificate, then routes requests
   to the approving app with the customer's Host header intact. No manual
   hostname list or certificate installation is required.

Registered primary hostnames always win, even for stopped or not-yet-deployed
apps. PR previews cannot approve customer names. If two apps approve one name,
issuance and routing are refused rather than picking an arbitrary app.

Approval checks use a total five-second deadline, at most eight concurrent
probes and a maximum 10,000-entry cache. Agent decisions expire after 30 seconds
when approved, 15 seconds when refused. Līdza additionally caches the app's hook
verdict for five minutes/one minute; customer revocations can therefore take up
to about six minutes to become visible. Release, port, assignment, stop, removal
and opt-in changes invalidate agent decisions immediately. Disabling the setting
immediately stops extra-name routing, but does not revoke already issued
certificates or delete Caddy's persisted certificate files.

Stopped/retiring apps cannot approve or serve customer hostnames. Maintenance
mode also covers approved customer names. Database/cache services and primary
hostname behavior are unchanged.

`/tls-allow`, `/healthz`, `/readyz` and `/metrics` are blocked at app ingress,
including normalized paths. Approval requests are direct loopback HTTP requests
without visitor cookies, credentials or forwarding headers. The endpoint is not
exposed as a public API. Never expose the agent's `/tls/allow` management gate.

For failures, check the app's approval policy, its successful release and opt-in,
DNS/CAA and Caddy service logs. An app without a hook, or one that still runs an
older framework, cannot approve customer names. A local-mode agent without a
public Caddy listener cannot provide public automatic HTTPS.
