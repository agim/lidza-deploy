# Public certificates for stack services

Implementation contract for agent/control v0.2.105 (not available in v0.2.104).
Do not add this field to deployed manifests until that version is installed.
This is host deployment orchestration; the framework's private gateway identities
are not public-domain certificates and are never reused here.

An opt-in service in a version 2 deploy/lidza.stack.json declares:

```json
"public_tls": {"hostname": "primary", "uid": 0}
```

Only the app's configured primary hostname is supported; a repository cannot
request another app's domain, a wildcard, a host path, the Caddy store, or a
Docker socket. uid is the numeric reader of the private key (0..65535, default
0). For the hardened policy it must be 65532. No runtime capability is added.
The service receives a private read-only bind volume at
/var/lib/lidza-public-tls. It must not overlap another service volume.

The agent injects these service-only settings; manifests cannot override them:

* DEPLOY_PUBLIC_TLS_HOSTNAME: the primary hostname frozen for this release.
* DEPLOY_PUBLIC_TLS_CERT_FILE: /var/lib/lidza-public-tls/current/fullchain.pem
* DEPLOY_PUBLIC_TLS_KEY_FILE: /var/lib/lidza-public-tls/current/privkey.pem
* DEPLOY_PUBLIC_TLS_BUNDLE_FILE: /var/lib/lidza-public-tls/current/bundle.pem

The bundle contains the full certificate chain and private key in one PEM file.
Prefer loading the bundle once, or resolve current once and load both files from
that immutable generation, so a renewal cannot mix certificate/key generations.
The service owns its TLS configuration and detects file changes/reloads its TLS
listener. Deploy does not run repository-provided host commands or reload other
services. Without a usable certificate, disable STARTTLS; do not advertise it
and respond 454. This contract does not require or publish the application's
wizard/settings, or change mail/AI/storage/operator choices.

First-install HTTPS issuance needs the app's route to exist. Accordingly a
missing certificate permits the service to start with these paths absent and
status pending. Caddy's existing on-demand ACME flow remains responsible for
issuance/renewal and DNS proof. The agent reconciles once at deploy/start and
every minute thereafter; the service must notice the first valid material as
well as renewals. It should independently validate hostname, key and validity
before enabling TLS. Invalid sources do not replace a last valid generation;
expired retained material is reported expired and must not enable TLS.

The host default source is Caddy's standard systemd storage directory:
/var/lib/caddy/.local/share/caddy/certificates. Hosts with custom storage can set
agent service_tls.certificate_directory to an absolute directory, and optionally
service_tls.trust_roots to a host-admin-owned PEM CA file for private PKI. The
manifest cannot set either. No recursive search, wildcard acquisition, remote
Caddy API, credentials, or filesystem path is exposed to the container. The agent
validates the full chain, server purpose, matching private key, hostname and
validity before atomic publication. The private key/bundle are mode0400 owned by
the declared uid; source/key bytes never appear in diagnostics or inventory.

Material is isolated by app incarnation, primary hostname and service. Rollback
keeps the previous release's hostname binding, and sync covers retained releases.
Stopped/removed apps do not initiate issuance. Removal deletes copied key material
after containers are removed; Caddy's original objects and unrelated apps remain.
