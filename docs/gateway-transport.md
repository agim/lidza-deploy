# Protected application gateway

Implementation contract on feat/gateway-transport; not released in Deploy yet.
Do not upgrade or redeploy production for this checkpoint. Framework API is
released in github.com/agim/lidza v0.1.125; all builds require Go1.27.3 or newer.

## App opt-in

Add this to deploy/lidza.stack.json, version2:

```json
"gateway_transport": {
 "version": 1,
 "paths": ["/your-protected-protocol", "/your-transport-challenge"]
}
```

Paths are exact prefixes (the prefix itself and slash-delimited children), max8,
no root, operator health paths, domain-proof/runtime-selection paths or overlap
with ordinary service HTTP routes. Port3001 is reserved for private app TLS and
cannot be declared as a public port or HTTP service route. Protected paths cannot
be removed, or the transport disabled, by an in-place update. This prevents a
rollback/update from silently restoring plaintext routing.

The agent supplies only the app container with these generated runtime values:

| Key | Value |
| --- | --- |
| DEPLOY_GATEWAY_ADDR | 0.0.0.0:3001 |
| DEPLOY_GATEWAY_CERT | Standard-base64 encoded PEM server certificate |
| DEPLOY_GATEWAY_KEY | Standard-base64 encoded PEM private key |
| DEPLOY_GATEWAY_CLIENT_CA | Standard-base64 encoded PEM trusted client CA |
| DEPLOY_GATEWAY_PEER_PINS | Comma-separated authorized agent-client SPKI pins |
| DEPLOY_GATEWAY_DELEGATE_PINS | Comma-separated authorized earlier Caddy SPKI pins |
| DEPLOY_GATEWAY_AUTHORITIES | Comma-separated configured public app hostnames |

Decode the three PEM values with base64.StdEncoding.DecodeString. Load the leaf
with tls.X509KeyPair and the CA with x509.CertPool.AppendCertsFromPEM. Pass them
to gateway.ServerTLS; set App.PrivateTLS to that configuration and
App.PrivateTLSAddr to DEPLOY_GATEWAY_ADDR. Ordinary LIDZA_ADDR remains
0.0.0.0:3000 for existing internal HTTP integrations. The agent publishes3001
only on an ephemeral host-loopback port. No additional container is introduced.

For every declared protected protocol/challenge route, enforce gateway.Verify
on both listeners, with Policy.Peers from DEPLOY_GATEWAY_PEER_PINS, Delegates
from DEPLOY_GATEWAY_DELEGATE_PINS and NativeEdges empty. Use a shared atomic
cache claim store with a stable application-specific prefix. Authorize the exact
request authority using the app's configured hostname policy; the supplied
hostname list is not permission to trust arbitrary aliases or forwarding headers.
Only gateway.RequireTLS13 or gateway.From supplies original-client evidence.
Backend r.TLS is the private hop and must never be treated as original-client TLS.

The private listener's GET /healthz must return200 to an authenticated mTLS client
without requiring original-client evidence. It verifies private endpoint/cert
readiness, not public protocol health. Ordinary private API health and existing
HTTP dependencies remain on3000. Public protocol probes must traverse the real
TLS edge; an internal mTLS-only probe cannot replace them.

## Host opt-in

The default HTTP ingress remains compatible with existing deployments. Protected
stacks require a separately configured mTLS host ingress. Prepare reviewable files
in a new private directory:

```sh
sudo lidza-agent -gateway-transport prepare -gateway-output /etc/lidza-agent/gateway-new
```

This creates credentials, a gateway-config.json fragment, a Caddyfile.gateway
snippet and permissions instructions. It never modifies live configuration,
services, DNS or firewall, and refuses to overwrite a directory. Merge the JSON
fragment into the existing agent configuration. Install the reviewed proxy snippet
only in the existing HTTPS site; keep HTTP domain proof, ACME and unrelated routes.
Grant Caddy read access only to ca.pem, edge.pem and edge-key.pem. Keep CA/agent
private keys root-only. Validate both configurations before coordinated reloads.

The host ingress is loopback-only, requires verified TLS1.3 client certificates
and explicit authorized edge pins, and uses the released native-edge protocol.
Caddy overwrites every recognized field from actual TLS/request properties.
Plaintext ingress and original TLS1.2 requests cannot invoke protected app paths.
Origin evidence is never forwarded to ordinary plaintext backend routes.

Host replay protection is a separate encrypted, atomically fsynced journal with
cross-process flock, retained through agent restarts. It is capped at8192 live
claims with30-second expiry; corruption, capacity, locking/storage errors fail
closed. Ordinary assets/API requests bypass origin attestation and journal I/O.
This is a single-host agent store, not a multi-host shared framework cache.
Applications still need their own shared Claims store across app replicas.

## Persistence and rotation

Per-app CA and leaf keys persist in encrypted agent state by app incarnation.
Only the app server leaf key is provided to the app. The app never receives its
CA key or the agent client key. Managed runtime values are hidden from ordinary
settings and cannot be edited through the environment patch API. Private key
variants are included in runtime log redaction.

Reload/deploy reuses keys and renews leaf certificates within30 days of expiry,
using the same keys/CA so pins and previous-release trust remain compatible.
Missing/corrupt retained identities fail rather than silently regenerating them.
App removal drops its transport identity; reusing an app ID gets a new identity.
CA rotation requires an explicit coordinated migration; no TOFU or automatic
CA/key replacement is performed. Back up encrypted state and its master key.

For host key/CA rotation, prepare a new directory; first install overlapping CA
trust and old/new edge pins on receivers and Caddy. Reload apps to include both
edge delegate pins while the old sender still works. Switch the Caddy credentials,
verify real protected traffic, drain old connections, then remove old trust/pins
and reload receivers/apps. Retain the nonce journal. Do not downgrade the agent
below the protected-gateway release while protected releases exist.

This contract protects transport provenance; application identity, signatures,
protocol nonces, authorization, publication gates and idempotency remain required.
An explicitly trusted compromised gateway can lie about its incoming transport.
No production deployment or vendor-protocol acceptance is claimed by this document.
