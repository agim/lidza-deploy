# Console structure

How the console is organised: which resources exist, how each is listed and
shown, the URL of every view, and how breadcrumbs are built. The visual system
(tokens, spacing, components) is in `console-design.md`; this document is the
information architecture on top of it.

## Principles

1. **An index is an index.** It lists resources in one table and nothing else:
   no per-item cards, no actions besides creating one, no logs or settings. A
   row's name links to the resource.
2. **A detail page is about one resource.** Its header names the resource and
   shows its status, key facts and actions; the properties of the resource are
   tabs below it, one URL per tab.
3. **Two templates, every resource.** `ResourceIndexPage` and
   `ResourceDetailPage` (`ui/src/components/resource/`) render every index and
   every detail page. A panel-wide settings page (integrations, defaults,
   updates, …) is a detail page of a singleton resource: same header, same tabs.
4. **Every location is a link.** Breadcrumbs are generated from the route, and
   each segment but the current one is a real anchor.
5. **Old links keep working.** A previous URL redirects (history replace) to
   the view that now shows its content.

## URL scheme

All views live under `/console`.

| Shape | View | Example |
|---|---|---|
| `/<collection>` | index | `/console/servers` |
| `/<collection>/new` | create form | `/console/applications/new?server=local` |
| `/<collection>/<id>` | detail, Overview tab | `/console/servers/local` |
| `/<collection>/<id>/<tab>` | detail, another tab | `/console/servers/local/firewall` |
| `/<singleton>` | singleton, first tab | `/console/integrations` |
| `/<singleton>/<tab>` | singleton, another tab | `/console/integrations/storage` |

Tabs are a fixed list per collection in `ui/src/lib/resources.ts`; a path with
an unknown tab or extra segments is *Not found*. `new` is reserved as an id in
collections that have a create form. Error and security groups belong to an
application, so their detail carries it as a query: `/console/errors/<group>?app=<app>`.

## Resources

### Collections (index → detail)

| Collection | Index columns | Detail tabs (first is the bare path) | Header actions |
|---|---|---|---|
| Applications `applications` | Name + hostname, Status, Runtime, Server, Release, Updated | Overview · Deployments · Logs · Environment · Domains · Database & cache · Backups · Processes · Move · Settings | Deploy / Start; menu: Restart, Stop/Start, Rollback, auto-deploy, Move to another server, Errors, Security activity, Delete |
| Servers `servers` | Name + agent URL, Status, Applications, CPU, Memory, Disk | Overview · Applications · Processes · Web ingress · Updates · Tuning · Firewall · Disk cleanup · Retained data · Console · SSH access · Agent | Add application; menu: Console, SSH access, Disk cleanup, Upgrade agent |
| Databases `databases` | Name, Status, Engine, Server, Schedule, Backups, Size | Overview · Backups · Schedule · Excluded tables (PostgreSQL) · Settings | Back up now |
| Deployments `deployments` | Deployment + application, Status, Kind, Commit, Duration, Started | Overview (facts + timeline) · Log | Refresh |
| Deleted apps `deleted-apps` | Application, Status (days left), Server, Kept, Deleted | Overview · Settings (restore, Danger zone: delete now) | Restore |
| Team `team` | Member, Role, Email | Overview · Settings (role, Danger zone: remove) | — (create: `/team/new`) |
| Errors `errors` | Error, Occurrences, Request, Source, Last seen (filter: application) | Overview · Occurrences | Copy details |
| Security activity `security` | Event, Occurrences, Request, Source, Last seen (filter: application) | Overview · Occurrences | Copy details |
| Audit log `audit` | Event, Actor, Target, Time | — (an entry has nothing more to show) | Copy audit log |

Tabs whose subject a resource lacks are hidden for it (an observed application
shows Overview only; a native application has no Move; a ClickHouse database
has no Excluded tables). Opening such a tab by URL shows a notice with a link
to the Overview, not a blank panel. ClickHouse has no settings of its own in
the control API: its one difference, restoring in place with verified row
counts, is part of the Backups tab, so it gets no separate tab.

### Singletons (one resource, tabs)

| Singleton | Tabs |
|---|---|
| Integrations `integrations` | GitHub · Backup storage `storage` · Email `email` |
| Application defaults `defaults` | Līdza (Go) · Rails `rails` · Astro `astro` · Node.js `node` |
| Remote backups `remote-backups` | Objects · Retention `retention` |
| Updates `updates` | Overview · Signed channel `channel` (private builds) |
| Control panel `control-panel` | Overview · Move `move` |
| Help `help` | Guides · Components `components` |
| Account `account` | Two-factor authentication |

### Create forms

`/applications/new` (wizard), `/servers/new` (agent connection) and
`/team/new` (member). They keep their own form layout and the breadcrumb
`Home › <Collection> › New …`.

## What moves where

| Was | Now |
|---|---|
| Application card actions (Deploy, Build logs, Settings, More menu) | Application header (Deploy + actions menu); Deployments tab |
| Application card "Move to another server" | Application **Move** tab; header menu links to it |
| App settings › General: source, branch, build, runtime profile | Application **Settings** tab |
| App settings › General: hostname, additional hostnames, customer hosts, redirect domains | Application **Domains** tab |
| App settings › General: forwarded ports | Application **Settings** tab (Network ports) |
| App settings › General: deployment safety | Application **Backups** tab |
| App settings › Environment | Application **Environment** tab |
| App settings › Data & storage; `/applications/{id}/cache` | Application **Database & cache** tab |
| App settings › Processes | Application **Processes** tab |
| App settings Danger zone | Application **Settings** tab, last section |
| `/applications/{id}/logs` page | Application **Logs** tab (same URL) |
| Server cards (resources, tools, menu) | Servers index rows; server header and tabs |
| `/servers/{id}/containers`, `cleanup`, `console`, `ssh` pages | Server tabs `processes`, `cleanup`, `console`, `ssh` |
| `/servers/{id}/upgrade` page, server connection card, Danger zone | Server **Agent** tab |
| Database cards on the index | Databases index rows |
| Database page sections | Database tabs: history → Backups; schedule + off-site retention → Schedule; skipped tables → Excluded tables; before-deploy policy → Settings |
| Deployment log page | Deployment **Overview** (facts, timeline) and **Log** tab |
| Deleted apps row buttons (Restore, Delete now) | Deleted app header (Restore) and Settings tab Danger zone |
| Team list Remove button, Add member card | Member Settings tab; `/team/new` |
| Error/security group cards with expanding details | Index rows; group detail page |
| Two-factor page | Account singleton |

Removed from indexes as detail content: the applications stats cards and
"next application" tile, grid/list toggle, the guidance and "self-hosted"
notes, the server guidance card, and the panel-wide retained-resources
list under databases (retained resources stay on each server and application).

## Redirects

| Old | New |
|---|---|
| `/console`, `/console.html`, `/` | `/console/applications` |
| `#…` legacy hash routes | their path equivalents below |
| `/new-application?server=x` | `/applications/new?server=x` |
| `/applications?move=x` | `/applications/x/move` |
| `/applications/x/settings/general` | `/applications/x/settings` |
| `/applications/x/settings/environment` | `/applications/x/environment` |
| `/applications/x/settings/resources`, `/applications/x/cache` | `/applications/x/data` |
| `/applications/x/settings/processes` | `/applications/x/processes` |
| `/servers/x/containers` | `/servers/x/processes` |
| `/servers/x/upgrade` | `/servers/x/agent` |
| `/servers/x/overview` (any `/<c>/<id>/overview`) | `/servers/x` |
| `/account/two-factor` | `/account` |
| `/errors/x`, `/security/x` (application) | `/errors?app=x`, `/security?app=x` |

`/applications/x/settings`, `/applications/x/logs`, `/databases/x` and
`/deployments/x` are tab URLs themselves and need no redirect.

## Breadcrumbs

Generated by `breadcrumbs(route, name)` in `ui/src/lib/resources.ts`:

```
index      Home › Servers
detail     Home › Servers › Production                (overview)
tab        Home › Servers › Production › Firewall
singleton  Home › Integrations › Backup storage       (first tab: Home › Integrations)
create     Home › Applications › New application
not found  Home › Not found
```

- `Home` links to the home view (`/console/applications`).
- Every segment but the last is an `<a href>`; the last is the current page
  (`aria-current="page"`), not a link.
- A resource is named by its display name (server name, application id,
  member name); until loaded, by its id.
- On phones (< 640px) the middle segments collapse into a `…` menu whose
  items are the same links; the first and last segments stay visible.

## Templates

### Index (`ResourceIndexPage`, root `[data-template=index]`)

```
┌──────────────────────────────────────────────────────────────┐
│ Home › Servers                                  topbar  │
├──────────────────────────────────────────────────────────────┤
│ Servers. [3]                              [+ Connect server] │
│ Hosts running your applications.                            │
│ (notices: errors, unavailable servers)                       │
│ [Search…        ] [Status ▾] [Server ▾]  Clear filters       │
│ ┌──────────────────────────────────────────────────────────┐ │
│ │ Name            Status    Apps   CPU    Memory   Disk     │ │
│ │ Production ›    ● online  2      12%    3.0 GiB  70%      │ │
│ │ Edge ›          ● online  0      4%     1.1 GiB  22%      │ │
│ └──────────────────────────────────────────────────────────┘ │
│ (empty: icon, sentence, [+ Connect server])                  │
└──────────────────────────────────────────────────────────────┘
phone: each row is a stacked block of label/value pairs.
```

### Detail (`ResourceDetailPage`, root `[data-template=detail]`)

```
┌──────────────────────────────────────────────────────────────┐
│ Home › Servers › Production › Firewall          topbar  │
├──────────────────────────────────────────────────────────────┤
│ Production. ● online                [Add application] [⋯ ▾]  │
│ http://agent.example.com · 2 applications · v0.2.84          │
│ ─────────────────────────────────────────────────────────────│
│ Overview  Applications  Processes  …  Firewall  …  Agent  →  │  tab list (scrolls, no scrollbar; edge fades)
│ ┌──────────────────────────────────────────────────────────┐ │
│ │ <tab panel in a SectionBoundary; loads its own data>     │ │
│ └──────────────────────────────────────────────────────────┘ │
│ Settings tab ends with:                                      │
│ ┌ Danger zone ─────────────────────────────────────────────┐ │
│ │ Stop / Delete / Disconnect …                [Danger btn] │ │
│ └──────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────┘
```

Tabs are links in behaviour: selecting one pushes its URL (Back returns to the
previous tab), and the tab list marks the tab the URL names as selected.

## Review against the request

- *Index should be an index*: every index is the table template; cards with
  actions, logs and settings moved to detail tabs (table above).
- *Detail page about the resource, tabs for its properties*: every resource
  has the detail template with path tabs; panel settings are singletons
  with the same header and tabs, so there is one pattern.
- *Breadcrumbs linkable everywhere*: generated for every route including not
  found, all segments but the last are anchors, phones keep them in a menu.
- *Consistency in indexing and presentation*: one column order (name, status,
  facts, time), one header shape, one tab model, Danger zone always last on
  the settings tab (a server's settings tab is *Agent*: its settings are its
  agent connection, service control of adopted applications and system log
  access for the deployment account).
- *Proper structure*: the route tree, tab lists and breadcrumb labels live in
  one registry; templates enforce the layout; tests visit every index and tab.
