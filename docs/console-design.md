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
  the loading state. One control panel is one installation (one team, its
  servers and applications), so the console has no workspace concept: the
  sidebar's top names this panel (its public hostname and release, linking to
  the Control panel page), its status line says whether every server's agent
  answered the last refresh (linking to Servers, unreachable names in the
  tooltip), and its bottom block is the signed-in person and their role,
  opening the shared `ActionMenu` with Account and Sign out. The panel-wide
  pages sit in the sidebar's Settings group.
- `ResourceList`: a semantic shadcn table with a labeled stacked mobile layout.
- `components/resource/`: the index and detail templates, breadcrumbs and the
  Danger zone (see Structure below).
- `template.css`: layout rules supplied with the reference.
- `console.css`: the integration rules for production routes, mobile layouts,
  resources, logs and credential fields. Keep additions in these shared layers.

## Structure: two templates for every resource

The console's information architecture is in `console-structure.md`: the
resources, their index columns and tabs, the URL of every view, redirects
from earlier URLs and the breadcrumb rules. The route tree lives in one
registry, `ui/src/lib/resources.ts`.

**Rule: every resource uses the templates in `ui/src/components/resource/`.**

- `ResourceIndexPage` renders an index: header (title, one line, count,
  primary action), a filter row, one `ResourceList` table whose first column
  links to each resource, an empty state that offers creating one. An index
  shows no detail content: no per-item cards with actions, logs or settings.
- `ResourceDetailPage` renders a resource: header (name, status, primary
  facts, actions menu), then its property tabs, one path each
  (`/console/<collection>/<id>/<tab>`, the first tab at the bare path). Each
  tab panel sits in a `SectionBoundary` and loads its own data; the tab list
  scrolls sideways on phones. Destructive actions live in a `DangerZone` at
  the end of the resource's settings tab.
- A panel-wide settings page is a singleton resource with the same
  detail template (`/console/<singleton>/<tab>`).
- Breadcrumbs come from the route (`breadcrumbs()`); every segment but the
  current one is a link, and phones fold the middle segments into a menu.

A new resource adds its collection and tabs to `resources.ts`, renders
through the two templates, and adds its views to `tests/browser/fixtures/views.cjs`
(the route table of `layout-spacing.cjs` and `console-errors.cjs`), which fails
when a view skips its template, its breadcrumbs or its selected tab.

Creating an application uses Source & identity, Hosting & data, and
Environment steps (`/console/applications/new`). Inputs stay mounted while
hidden so moving between steps preserves values. Each step validates its own
controls before advancing, and creation revalidates all steps.

The Help page includes a component gallery using the same production primitives.
Its examples are explicitly local previews and make no server changes.

## Spacing

Spacing lengths are written once, as tokens in `ui/src/tokens.css`: the scale
`--space-1` … `--space-7` (4, 8, 12, 16, 24, 32, 48px) and semantic tokens
(`--page-gap` 24px between page sections and sibling cards, `--section-gap` 12px
from a heading block to its content, `--card-padding` 24px/16px on phones,
`--stack-gap-sm/md`, `--form-gap`, `--control-gap` 8px between buttons,
`--page-gutter`). Layout comes from the primitives in
`ui/src/components/layout` (`Page`, `Section`, `Stack`, `Cluster`) and the
shared rules in `ui/src/layout.css`: spacing between siblings is the parent's
`gap`; components carry no outer margins. Every route renders inside a `Page`;
cards, forms (`ActionForm`) and fieldsets are vertical flows too.

`npm run lint` (also part of `npm run typecheck` and the UI test suite) fails on
a raw pixel value in any margin, padding or gap outside `tokens.css`.
`tests/browser/layout-spacing.cjs` (a suite of `npm run test:browser`) measures every route at
1440 and 390px: every index and every detail tab uses its template, with
linked breadcrumbs and the selected tab matching the URL; no overflow, headings at least 16px below the block before them,
sibling cards 24px apart, no touching blocks in a flow, buttons at least 8px apart.

## Verification

Run component and route tests with `npm run test:ui`, TypeScript with
`npm run typecheck`, and build the embedded console with `npm run build`.
Production CSS/assets are embedded in the Go control-panel binary, so
installing a release is required to deploy a new console; refreshing a browser
alone cannot upgrade the installed binary.

### Browser suites

`npm run test:browser` (`tests/browser/run.cjs`) serves the built console
(`web/static`) once and runs every browser suite against it, three at a time,
with a summary; it exits non-zero when any check fails. CI runs it after the
build in both release workflows.

```sh
npm ci && npm run build
npm ci --prefix tests/browser && npx --prefix tests/browser playwright install chromium
npm run test:browser                      # every suite
npm run test:browser -- servers logs      # some suites
TEST_ONLY='console:' npm run test:browser -- servers   # checks whose name contains the text
```

| Suite | Covers |
|---|---|
| `layout-spacing` | every index, detail tab, create form and Not found: template, breadcrumbs, selected tab, earlier URLs, spacing tokens, Actions and account menus, sidebar identity and status links (1440 and 390px) |
| `console-errors` | every view and earlier URL renders with no console error or warning, failed or 4xx/5xx request, form hint, input without autocomplete, or unmocked API read |
| `applications` | index search and filters, an empty or offline control panel, Deploy and lifecycle actions, the wizard payload, Environment (step-up reveal, saves, master key), Domains, Database & cache, Settings |
| `servers` | index resources, connecting a server, Agent, Processes, Disk cleanup, Console (unlock, run, cancel, restored and reconnected jobs) |
| `databases` | databases index, backups and restore, schedule, remote backup objects, storage integration |
| `logs` | live build log (streaming, reading position, pause, outage, completion), runtime log, copy, failed deployment |
| `settings` | phone navigation and touch targets, sidebar server status and sign-out, GitHub connection, defaults, updates, team, errors and audit, two-factor setup, help gallery |
| `setup-auth` | `setup.html` first-run interview, `login.html` errors, one-time code and safe return, signed-out redirect |

Every check runs at a phone and a desktop width on a fresh page, fails on a
page error, an unexpected native dialog or a page that scrolls sideways
(`tests/browser/fixtures/harness.cjs`), and talks only to the fixture API
(`fixtures/console.cjs`, `fixtures/server-host.cjs`), whose data uses
example.com hosts and generic names. Fixture success does not establish the
health of a live hosting agent.

**Every new feature's browser check is a case in an existing suite run by
`npm run test:browser`** — the suite of the area it belongs to — never a
standalone script. A new view is added to `fixtures/views.cjs`; a new endpoint
the console reads is answered in `fixtures/console.cjs` (the `console-errors`
suite fails on an unmocked read). A check that no longer matches the console
is fixed or removed in the same change that alters the behaviour; a script
nothing runs is not kept.
