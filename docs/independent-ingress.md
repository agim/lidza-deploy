# Independent application ingress

An agent normally owns deployment management and the application proxy. Stopping
that process can interrupt hosted requests even while every container stays up.
`ingress_only` provides a separate process for the existing proxy and protected
gateway, using the retained encrypted deployment inventory.

It reads the existing master key, authenticated `state.json`, host gateway
certificate, client trust and per-app gateway identities. It creates none of
these. It does not start Docker reconciliation, maintenance, deployment workers,
schedules, certificate jobs or the management API. Starting the ordinary manager
with this role is rejected. The application proxy and health listeners require
explicit loopback IPs. No agent API token is needed for this role.

Use a separate pinned binary, unit and config. Copy the existing config locally
on the server and change only:

```json
{
  "ingress_only": true,
  "listen": "127.0.0.1:19091",
  "proxy_listen": "127.0.0.1:18081"
}
```

This is a config **patch**, not a complete config. Retain `data_dir`, the original
`tls_listen` policy and all existing `gateway` configuration. When a gateway is
configured, change its `listen` to a separate free loopback port, keeping its
certificate, key, CA and peer pins unchanged. Do not copy credentials through the
dashboard or into source control. Both processes need the same retained state and
gateway replay journal. The reader only writes this flock-protected encrypted
nonce journal for protected requests; it refuses deployment-inventory writes.
Its OS permissions therefore need journal writes within the existing data
directory. This role is not a separate security principal for the stored keys.

The health listener serves `GET /health` and the existing loopback-only
`GET /tls/allow` certificate permission check. No management endpoints are
available. Inventory is refreshed on atomic replacement, at 250 ms intervals.
Invalid, missing, oversized, plaintext or unauthenticated snapshots fail closed
with 503 for new requests until a valid snapshot returns. Existing upgraded
streams retain their established backend connection.

The deployment manager remains the only inventory writer. Deployments must retain
the preceding healthy release across the snapshot refresh interval, as the
existing reload/deploy flow does. Do not run simultaneous management agents.
Changing inventory format, host gateway trust or encryption keys requires a
separate compatibility and ingress rollout review.

## Live handoff requirements

Starting the reader does not redirect traffic. Review the host's exact routing
before changing it. Include ordinary requests, protected gateway traffic,
on-demand certificate authorization, active jobs and long-lived connections.

Do not assume that a Caddy reload preserves WebSockets: Caddy documents that
streams close on config reload by default. A bounded `stream_close_delay` still
does not guarantee that an indefinitely active connection stays open.
[Caddy streaming documentation](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming).

One reviewed Linux handoff can redirect only new local Caddy TCP connections to
the prepared ingress while existing connections drain on the old process. Scope
rules to the verified Caddy UID, destination loopback IP and exact ports. Require
TCP SYN **and** conntrack NEW; a broad NAT rule can otherwise redirect an old
connection that was not tracked before NAT hooks were installed. Never flush
conntrack or unrelated firewall chains. Netfilter keeps NAT decisions per
connection. [Netfilter connection/NAT documentation](https://www.iptables.org/documentation/HOWTO/netfilter-hacking-HOWTO-4.html).

The management agent must stay running until all old accepted ingress sockets
have drained naturally and deployment/backup/restore operations are idle. If they
do not drain, defer the agent upgrade. A timeout is not permission to disconnect
hosted requests. Prepare reboot ordering, monitoring and rollback before applying
any persistent redirect. Removing a redirect changes future connections; existing
translated streams must also drain before the new ingress can be stopped.

This repository adds the process role, not an automatic firewall installer or a
promise that restarting its ingress process preserves live connections. Every
live routing, service or host configuration change requires a reviewed rollout.

## Validation

`TestIngressReader*` checks manager-independent HTTP and upgraded streams,
atomic routing updates, invalid-state refusal/recovery, no state/key creation or
inventory writes, listener restrictions and management API isolation.

With a trusted stock Caddy test binary:

```sh
LIDZA_TEST_CADDY=/absolute/path/to/caddy go test -race ./internal/agent \
  -run 'TestIngress|TestRealProtected' -count=1
```

The real-edge test verifies the retained mTLS path with the deployment manager
stopped, original TLS 1.3 evidence, unchanged request bodies and rejection of
forged plaintext evidence and original TLS 1.2.
