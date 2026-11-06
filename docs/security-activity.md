# Security activity

Update the control panel first, then each hosting agent. The agent uses Deploy's existing authenticated console collector, persistent replay cursor and bounded outbox, for container applications (Docker console) and native applications (the journal of each registered service unit) alike. No application analytics pack is required.

Probes are classified from the application's own request logs:

- Līdza (Go) applications: structured JSON request logs with `msg=request`, path, method and HTTP status (the framework's request logger emits them).
- Rails applications: `ActionController::RoutingError (No route matches [GET] "/.env")` lines, and `Started METHOD "path"` followed by the request's `Completed 4xx/2xx/3xx` line (tagged request ids are kept for correlation).
- Node and other text logs: common and combined access-log lines (`"GET /.env HTTP/1.1" 404`) and `METHOD /path status` lines such as morgan's short formats.

Not covered: requests rejected before they reach the application (by Caddy, the agent's ingress or the firewall) are not in the application's logs and are not reported here; Deploy does not read proxy access logs. A native application that logs only to a file instead of the journal, or an application without request logging, reports nothing. Static sites have no process and no request log.

Open Security activity in the navigation, choose an application, or use the application's More actions → Security activity button. Refresh runs without clearing current results; failed refreshes retain previous results. Console application failures continue to appear in Errors, not this view.

Categories are suspected secret-file access, path traversal (including encoded traversal), execution probes and a small set of discovery endpoints. Ordinary 404s and GraphQL requests alone are not treated as attacks. A rejected response, redirect or 2xx result is reported without claiming an exploit. In particular, SPA fallbacks can return 200 to malicious-looking paths. HTTP 5xx entries remain application errors for diagnosis.

Stored details include sanitized path, method, response status, request ID and release/container correlation. Queries are dropped, configured secrets are redacted and fields are bounded. The collector ignores client-IP headers; trustworthy client attribution and per-client protection await the framework API requested in agim/lidza#46. There is no automatic IP blocking or path-based blocking in this release, and no claim that this replaces a vulnerability scan.

For each application, the existing operations job creates an incident when at least 50 captured probes occur in five minutes, or a suspected probe receives a 2xx response in the past hour. Alerts use Deploy's configured mail service and operator recipient, with its existing incident deduplication and recovery notifications. Incidents appear in operational infrastructure settings even without configured email. Incident identity includes server and app; reporting and queries also require the app's current incarnation. App removal clears its security incidents.

Activity uses the existing 30-day console report retention and a 500-record view limit. It is sampled: at most 25 probes per container or service unit per collection pass, bounded console and journal reads, transport backpressure and offline periods limit coverage. This is not a total traffic counter. Alert thresholds refer to captured records, not inferred unseen requests. Sampling keeps noisy probes from consuming every collection slot; application errors retain their capture path.

Agent authentication, fleet permissions, replay deduplication and incarnation checks apply to these reports. OAuth is unchanged. Upgrade older agents before interpreting an empty view as absence of probes.

## Trusted proxies after the v0.1.91 upgrade

Deploy's control-panel service trusts only loopback by default (`LIDZA_TRUSTED_PROXIES=loopback`), matching the installer’s Caddy → 127.0.0.1:3000 route. The framework now supplies resolved client identity to its existing authentication throttles and request logs. An explicit value in `/etc/lidza-control/control.env` overrides this service default; change it only to your actual trusted proxy addresses. A service restart is required.

Hosted applications have their own framework dependency and configuration. Updating Deploy does not upgrade their framework or configure their trust automatically. For a Docker-hosted app, Caddy’s peer address inside the container may be the Docker bridge gateway rather than loopback; configure the actual gateway/proxy address, not a guessed address or a blanket trust of private networks. Their request rate limits remain explicit app policies using the released framework API. Deploy's Security activity view still does not automatically block IPs.
