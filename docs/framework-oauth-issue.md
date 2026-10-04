# Add reusable OAuth connections for external API integrations

## Problem

Līdza's `packs/auth` supports signing in with external providers. Applications also need to connect an external account to call its API after the authorization callback, independently of signing in.

At v0.1.60 (`18f14217e0554673a39180ea4794a918b7a7df12`):

- `packs/auth/providers.go`: `GitHubProvider.AuthURL` fixes scopes to `read:user user:email`.
- `GitHubProvider.Exchange` uses the provider access token to fetch identity, then returns `Identity` without the token, granted scopes, expiry, or refresh credentials.
- The `Provider` interface models sign-in, not retained external API authorization.
- `pkg/credentials` already exposes encryption primitives, and `packs/db` supports encrypted runtime credentials; reuse these facilities rather than adding another encryption implementation.

A deployment application needs GitHub repository access, including private repositories, and permission to manage repository webhooks. Calendar, storage, and other integrations need the same connect/disconnect lifecycle. These applications should not each rebuild OAuth state handling, PKCE, token storage, refresh, and revocation.

## Requested framework capability

Provide an opt-in external-account connection API/pack, distinct from identity-only sign-in, with:

1. Provider-specific configurable scopes and validated callback URLs. Keep current sign-in defaults and behavior unchanged.
2. Initiation by an authenticated application user, binding state to that user and browser, expiration and one-time consumption; PKCE where supported.
3. A connection result containing stable provider identity, granted scopes, token type, access-token expiry, and optional refresh token. Do not expose secrets through normal JSON serialization, logs, browser state, or generated client responses.
4. A documented encrypted persistence interface using existing framework credentials/db facilities, scoped to the authenticated owner. Allow an app to implement additional workspace authorization without coupling the framework to a deployment product.
5. Server-side retrieval/use, refresh with bounded concurrent refresh and timeouts, reconnect on revoked/expired grants, disconnect and provider revocation where supported.
6. A GitHub OAuth adapter that supports repository and webhook authorization, with clear documentation of GitHub App installation tokens as a separate, narrower-access option. GitHub repository listing and deployment orchestration remain application code.
7. Tests using local fake providers: denied consent, invalid/replayed/expired/cross-user state, missing scopes, token exchange failure, refresh/rotation, revoked grants, encrypted storage, and absence of token values from logs/responses.
8. A neutral example/recipe, generated API/agent documentation as appropriate, changelog entry, and a published version tag so downstream applications can pin and verify the feature.

## Acceptance

An authenticated app user connects GitHub with explicit repository/webhook permissions; the server subsequently calls GitHub with the retained credential without placing it in the browser. A different user cannot access that connection. Existing GitHub sign-in tests still pass.

Please implement and publish a release containing this capability. The dependent application will pause its OAuth integration, then upgrade to and verify the released API before continuing. Closing this issue alone is not the release gate: record the release tag and supported usage.
