# Console design system

The production console uses the user-supplied Līdza Deploy shadcn/ui Nova
reference and exact Matrix-green theme. The reference was a local-state preview;
production pages continue using the control API, paired hosting agents, and the
existing OAuth, role, credential step-up and server-console access controls.

## Reusable layers

- `ui/src/theme.css`: the supplied semantic tokens, without color substitutions.
- `ui/src/shadcn.css`: Tailwind theme mappings and the bundled Inter variable font.
- `ui/src/components/primitives/`: the supplied shadcn/Radix building blocks.
  Data-state selectors are adapted to the pinned Radix release. Use these
  primitives instead of introducing a second UI library.
- `ui/src/components/ui.tsx`: production adapters for forms, actions, loading,
  notices, menus and confirmation. Keep asynchronous error handling and the
  1.95-second minimum action feedback here, rather than in individual pages.
- `AppShell`: shared navigation, accessible mobile sheet, account actions and
  workspace loading state. Connection labels use observed workspace state.
- `ResourceList`: a semantic shadcn table with a labeled stacked mobile layout.
- `template.css`: layout rules supplied with the reference.
- `console.css`: the integration rules for production routes, mobile layouts,
  resources, logs and credential fields. Keep additions in these shared layers.

Applications support search, status filtering, and grid/list views. Creating an
application uses Source & identity, Hosting & data, and Environment steps. Inputs
stay mounted while hidden so moving between steps preserves values. Each step
validates its own controls before advancing, and creation revalidates all steps.
The final request retains repository installation, branch, server, domain,
auto-deploy, customer-host, database, backup, and environment settings.

Existing operational routes remain available: deployments and live logs;
application settings and credential verification; servers, health, containers,
root console, SSH and agent upgrades; databases, restores, remote backups and
cache; integrations, defaults, team, audit, security, errors and account 2FA.

The Help page includes a component gallery using the same production primitives.
Its examples are explicitly local previews and make no server changes.

## Verification

Run component and route tests with `npm run test:ui`, TypeScript with
`npm run typecheck`, and build the embedded console with `npm run build`.
Browser regressions under `tests/browser/` cover interaction, responsiveness,
form payloads, credentials, logs, health and action feedback. Keep fixture tests
separate from live infrastructure mutations. Production CSS/assets are embedded
in the Go control-panel binary, so installing a release is required to deploy a
new console; refreshing a browser alone cannot upgrade the installed binary.

The template regression suite can be reproduced against the built static files:

```sh
npm ci
npm run typecheck
npm run test:ui
npm run build
npm ci --prefix tests/browser
# Start a separate local static server on a free port, then:
TEST_WEB_URL=http://127.0.0.1:8880 npm run test:console
```

The browser suite intercepts API calls with fixtures. It covers all 26 routes at
320, 390, 768 and 1440 pixels; grid/list views, search and status filters; mobile
navigation and dialogs; complete creation payloads; credential field alignment,
MFA reveal and environment saves; JIT root console/clipboard; confirmed container
restart and safe restore; QR enrollment; empty/offline states and save retries.
Additional existing regressions cover live log polling, server resources,
long repository branches and the 1.95-second action-feedback duration. Browser
fixture success does not establish the health of a live hosting agent.
