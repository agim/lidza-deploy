# Guided GitHub connection

Status: implemented in lidza-deploy using released Līdza v0.1.75 primitives. [Framework #29 was declined](https://github.com/agim/lidza/issues/29#issuecomment-5987110069) because manifest registration, installation authentication and repository selection are product-specific. The existing framework auth tokens, authorization, sealed credentials, audit and webhook APIs already cover the general needs. No framework release is required for this flow. Local protocol and browser fixtures pass; real GitHub account/organization acceptance remains required. This source change is not present in the old v0.2.1 release bundle.

## User experience

Local operator login remains independent of GitHub. After first-run setup the installation owner clicks **Connect GitHub** in Integrations. Līdza Deploy posts a prefilled GitHub App manifest to GitHub, where the owner confirms creation. Returning to this control panel exchanges the manifest code and saves the returned credentials securely; the user never copies a client secret or private key.

Next, **Choose repositories** opens the new app's GitHub installation page. The owner selects repositories on the account that owns the private App. To use organization repositories, create the App under that organization using the optional organization field. A verified installation appears in Integrations. New applications choose a repository from those installations. GitHub organizations can require administrator approval; the GUI must distinguish pending approval from a failed connection.

This creates a GitHub App for each self-hosted control panel. It requires GitHub confirmation of app creation and installation, but no central authorization broker and no shared credentials embedded in distributed binaries. A public HTTPS control-panel hostname must already be established by the installer.

Initial setup collects the local account and database and uses the chosen FQDN; connecting GitHub happens after login so the registration is bound to the authenticated installation owner. Public repositories can still be deployed without connecting GitHub.

## Permissions and webhook behavior

The product requests contents read, metadata read (implicit) and pull requests read, and subscribes to push, pull_request, installation and installation_repositories events. The GitHub App's manifest configures the control-panel webhook and secret. Per-app Auto-deploy filters signed events by verified installation, repository and branch; it does not create a separate OAuth-backed repository webhook. Preserve existing durable delivery deduplication, retries and preview isolation/cleanup.

Līdza supplies one-use expiring auth tokens, owner/role checks, atomic sealed credentials, credential encryption, audit recording, and HMAC webhooks with a durable delivery store. lidza-deploy supplies the GitHub-specific manifest exchange, RS256 App JWT, verified installations, narrow token requests, UI, repository mapping and deployment orchestration.

## Automatic credentials

GitHub App installation tokens normally expire after one hour. The control panel signs a short-lived GitHub App JWT with its encrypted private key and requests a new installation token automatically. This is token minting, not a user OAuth refresh-token exchange. A background deployment must obtain valid repository-scoped credentials without interactive approval.

A token may expire while a deployment waits in the agent's queue. The control panel instead issues a one-use framework auth ticket, expiring after 24 hours, bound to the app generation, server, repository and installation. Immediately before checkout, the agent submits the ticket and its server-specific bearer key to `/api/agent/checkout-token`. The control panel validates those assignments, rechecks GitHub installation/repository access and mints a fresh narrow token. Invalid, replayed or expired tickets fail the candidate; use Deploy again for a new ticket. The private key never reaches an agent. The agent uses its existing askpass mechanism and clears checkout tokens without persisting them or injecting them into the Docker build/runtime.

Revocation, installation suspension/deletion and repository removal must fail closed and show that access is no longer available. They must not silently fall back to broad OAuth grants. Reauthorization is necessary only when access actually needs restoration or a replacement app connection is chosen.

## Verification

GitHub Apps are the exclusive authorization method. OAuth credential configuration, reconnect routes, stored-account token fallback and legacy repository webhook routes are removed. Public apps deploy without authorization; connect their repositories to an installation before enabling automatic deployment or private checkout.

Automated checks cover no-secret registration, owner/state/callback checks, replay, encrypted restart persistence, installation verification, selected-repository listing, delayed checkout credentials, expired/revoked tokens, signatures/delivery deduplication, secret omission, browser registration/install navigation and repository attachment. Complete actual GitHub creation/install/private-clone/push acceptance after fixture tests; mock GitHub tests do not establish live consent or organization behavior.

For organization repositories, enter the organization name before Connect GitHub so the private App is created under that organization. A private App can be installed only on its owning account. Choose repositories again after approval or permission changes. Only the local installation owner may register or install the App.

Public apps use **Connect repository** after the repository is selected in GitHub. This verifies access and updates existing preview assignments. Disconnect is blocked while apps still depend on the connection. Uninstall/delete the App in GitHub to revoke its GitHub access.

Upgrade both the control panel and hosting agents together: older agents reject the new checkout-ticket fields. No centralized authorization broker is needed. The owner can register one private App per control panel; additional account ownership requires a separate control panel in this first implementation.

Run `node tests/browser/github-app.cjs` in the prepared development environment for the manifest form/CSP/organization/repository-selection UI fixture. Protocol tests cover one-use state, wrong installation, encrypted persistence, selected repository listing, expired/revoked tokens, signed/deduplicated events and agent checkout tickets.
