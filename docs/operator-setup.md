# Hosted app operator setup

Hosted apps using Līdza v0.1.90 or newer can protect their first operator with the framework's one-time owner claim. This is opt-in; existing apps and OAuth grants are preserved.

1. In the hosted app, enable the auth pack, generate its schema with `lidza gen`, and apply the migration with `lidza db migrate` using the app's supported migration workflow. The owner-claim table must exist before startup.
2. In Deploy's app Settings, add `AUTH_OWNER_CLAIM=true` to its environment. Deploy or reload with the updated hosting agent. The app must allow the intended operator to sign in through its configured provider or registration policy. Deploy does not enable public registration for it.
3. Expand More actions → Operator setup token → Reveal setup token. Administrators and deployers can reveal; viewers cannot. Copy it and sign in to the hosted app. Its `/admin` claim form, or its custom owner-claim page, consumes the token.
4. Once claimed, Deploy displays the claimed state and does not return the token. Closing the dialog clears its displayed value.

The hosting agent mounts a private persistent directory at `/run/lidza-owner-claim` and sets `AUTH_OWNER_CLAIM_DIR` to that path. This managed path overrides a custom environment value for apps that opt in. Its files persist across container replacement. Token retrieval is an authenticated POST, audited without recording the token, with non-cacheable responses. Tokens are not collected from console logs and are not part of routine polling.

An optional platform-provided `AUTH_OWNER_CLAIM_TOKEN` must be at least 32 characters and shared across app nodes. Deploy reveals it only while the framework status says unclaimed. Changing platform tokens or the claim settings requires an app reload/redeploy; claiming applies immediately. Removing a GUI value doesn't clear a user's clipboard.

For apps predating this agent support, reload after updating the agent so the private directory is mounted. Unsupported framework versions, missing migrations or disabled auth produce startup/status errors; do not enable the setting before the hosted app supports it.

Deploy's own first-run token remains separate at `/var/lib/lidza-control/setup-token`. Retrieve that initial token through the installer/server or cloud-provider console. An unclaimed public Deploy GUI cannot securely reveal its own claim token. Once Deploy is claimed, its authenticated managers can retrieve hosted apps' tokens through the GUI without SSH.

Provider settings have their own runtime behavior: Deploy's configured GitHub connector API reconfigures the connector live; hosted app environment edits replace/reload the app process. Framework owner-claim configuration is read at startup, while claims take effect immediately. OAuth is not disabled by any of these operations.
