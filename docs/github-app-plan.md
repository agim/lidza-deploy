# Guided GitHub connection

Status: planned, blocked on a tagged framework release for [Līdza #29](https://github.com/agim/lidza/issues/29). The current implementation still uses manually configured OAuth credentials. Do not present the flow below as implemented.

## User experience

Local operator login remains independent of GitHub. After first-run setup the installation owner clicks **Connect GitHub** in Integrations. Līdza Deploy posts a prefilled GitHub App manifest to GitHub, where the owner confirms creation. Returning to this control panel exchanges the manifest code and saves the returned credentials securely; the user never copies a client secret or private key.

Next, **Choose repositories** opens the new app's GitHub installation page. The owner chooses a personal account or authorized organization and selects repositories. A verified installation appears in Integrations. New applications choose a repository from those installations. GitHub organizations can require administrator approval; the GUI must distinguish pending approval from a failed connection.

This creates a GitHub App for each self-hosted control panel. It requires GitHub confirmation of app creation and installation, but no central authorization broker and no shared credentials embedded in distributed binaries. A public HTTPS control-panel hostname must already be established by the installer.

Initial setup collects the local account and database and uses the chosen FQDN; connecting GitHub happens after login so the registration is bound to the authenticated installation owner. Public repositories can still be deployed without connecting GitHub.

## Permissions and webhook behavior

The product requests contents read, metadata read (implicit) and pull requests read, and subscribes to push, pull_request, installation and installation_repositories events. The GitHub App's manifest configures the control-panel webhook and secret. Per-app Auto-deploy filters signed events by verified installation, repository and branch; it does not create a separate OAuth-backed repository webhook. Preserve existing durable delivery deduplication, retries and preview isolation/cleanup.

The framework supplies the registration state/credential exchange, GitHub App authentication, installation verification and scoped-token lifecycle. The product supplies the manifest, owner-only UI, app-to-repository mapping, signed-event routing and deployment/preview orchestration.

## Automatic credentials

GitHub App installation tokens normally expire after one hour. The control panel signs a short-lived GitHub App JWT with its encrypted private key and requests a new installation token automatically. This is token minting, not a user OAuth refresh-token exchange. A background deployment must obtain valid repository-scoped credentials without interactive approval.

Minting only at the time a webhook is accepted is insufficient: a token may expire while a deployment waits in the agent's queue. Product dispatch must arrange fresh credentials immediately before checkout (including queued/retried deployments) through an authenticated control-panel/agent exchange. Validate the agent, assigned app, repository and installation before issuing credentials; never send the GitHub App private key to a hosting agent. The agent uses its existing askpass mechanism and clears checkout tokens without persisting them or injecting them into the Docker build/runtime.

Revocation, installation suspension/deletion and repository removal must fail closed and show that access is no longer available. They must not silently fall back to broad OAuth grants. Reauthorization is necessary only when access actually needs restoration or a replacement app connection is chosen.

## Migration and verification

Keep existing OAuth deployments functional during the migration, but make guided GitHub App registration the new-installation default once it is fully implemented. Confirm GitHub App access to each mapped private repository before replacing that app's credential source. Signed deliveries arriving from existing OAuth hooks must not cause duplicate deployments alongside the new app webhook. Retiring legacy hooks requires explicit migration state and an available grant/permission; do not remove access before the replacement has been verified.

Required tests cover no-secret registration, owner/state/callback checks, cancellation/restart/replay, installation/account verification, selected-repository listing, delayed deployment token expiry, concurrent renewal, revocation/suspension, signature/delivery matching, secret omission, browser registration/install status and legacy migration. Complete actual GitHub creation/install/private-clone/push acceptance after fixture tests; mock GitHub tests do not establish live consent or organization behavior.

Do not implement a local replacement for the reusable framework capability while #29 is pending. Verify the released API and tag, upgrade the dependency and run integration tests before implementing dependent product code.
