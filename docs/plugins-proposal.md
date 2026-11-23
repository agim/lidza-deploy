# Proposal: plugins for application-specific software

Status: stage 1 is implemented (the contract, signature check, installation
and the read-only hooks; see [Stage 1 as built](#stage-1-as-built)). The
ClickHouse plugin exists (it installs the server, gives applications their
databases, backs them up and restores them) and takes over the server checks
where it is installed. The agent's built-in ClickHouse support is still what
an application gets by default, and what moves a database between servers.

## Why

The agent manages servers and deploys applications. It should not also be the
administrator of third-party software one application happens to need. Today
it is, for ClickHouse: about 8,500 lines in 27 files install the server,
create databases, back them up, tune them, move them between servers and
police their system logs, and 55 other files of the agent and control panel
call into that code. Each new fact about ClickHouse (a release that needs a
newer processor, a log table that grows) becomes a change to the agent.

A plugin is a separate program that knows one piece of software. The agent
knows a small, fixed contract and nothing about what is behind it. Support for
a known application (a telemetry hub that needs ClickHouse, a popular Rails,
Node or Go application with its own needs) ships as a plugin with that
application or beside it, not in the agent.

PostgreSQL and Redis stay in the agent: every application runtime the agent
supports needs them, and they are part of running a server.

## What a plugin is

One executable, installed on a server under the agent's data directory
(`<data_dir>/plugins/<name>/<version>/plugin`), described by a signed manifest,
`plugin.json`. The agent runs it as `plugin <hook>`, writes one JSON
request to its standard input, reads one JSON answer from its standard output
and keeps its standard error as the hook's log. Large data never goes through
the JSON: hooks that produce or consume data are given a directory.

A plugin has no network API, keeps no process running for the agent, and
stores its state in a directory the agent gives it
(`<data_dir>/plugin-state/<name>`). Secrets it needs to keep (an administrator
password) are sealed by the agent: the plugin returns them, the agent stores
them encrypted and hands them back on later hooks.

### Hooks

Derived from what the agent calls in the ClickHouse code today. A plugin
implements the ones it supports and declares them in its manifest.

| Hook | Called when | Answers |
| --- | --- | --- |
| `describe` | the plugin is installed, and on agent start | name, version, the service it provides, environment keys it sets, ports it uses, supported hooks |
| `check` | a move is reviewed, an application is about to be created, the server's readiness is shown | checks: ok, or what is wrong, and whether `repair` fixes it. Given the application's request and the size of the data that will arrive |
| `provision` | an application that uses the service is created | environment values for the application, and the record the agent keeps (database name, port) |
| `release` | the application is removed | what was removed, what was kept |
| `status` | the server's health and the application's page are shown | version, running or not, size on disk, warnings |
| `backup` | the application's backup schedule fires, before a deploy when asked | a backup in the given directory, its digest and size |
| `restore` | an operator restores, or a move loads data | restored objects and row counts for verification |
| `export` / `import` | a move copies the service's data | the same directory contract as backup and restore, plus `since` for an incremental final sync |
| `tune` | the server's resource tuning runs | the settings it applied for the memory it was given |
| `maintain` | hourly | what it cleaned up |
| `repair` | readiness repair, and before `provision` when `check` says so | what it repaired, what it could not |
| `remove` | the last application using the service is gone and the operator confirms | what was uninstalled |

Every request carries the contract version, the server's facts the agent
already knows (architecture, processor flags, memory, the free space of the
directory the plugin stores data in) and the agent's limits for the hook
(time, disk). Every answer carries `ok`, `messages` for people and the hook's
own fields. An unknown field is ignored on both sides, so the contract can
grow.

The agent enforces what must not be left to a plugin: the time limit of each
hook, one hook at a time per plugin, that an application only gets environment
keys the manifest declared, that ports do not collide with the agent's own
allocations, and that a failed `provision` is followed by `release`.

## Trust

A plugin runs as root, like the agent. So:

- a plugin is installed only from a signed archive, verified with the same
  pinned keys as the agent's own signed updates, or with a key the server's
  operator added for that plugin in the control panel;
- installing or updating a plugin on a server is an explicit, audited action
  of an operator with the permission to manage infrastructure, never a side
  effect of deploying an application;
- an application's repository may **name** the plugin it needs
  (`deploy/lidza.stack.json`: `"plugins": [{"name": "clickhouse", "version": "1"}]`).
  A deploy on a server without it stops with what to install; it does not
  install it.

## What moves out of the agent

Everything in the files named `*clickhouse*` of the agent and control panel,
and the ClickHouse branches in the files that call them, become:

- in the agent and panel: calls to the hooks above for "a service of a
  plugin", with no knowledge of ClickHouse (its versions, its packages, its
  system logs, its backup formats, its processor requirements);
- in the plugin: the existing code, largely as it is, behind the hooks.

The control panel's pages keep working from what hooks return: a move's plan
shows the plugin's `check` and the sizes from `status`; the server's tuning
page shows what `tune` reports.

## Stages

Each stage ships on its own and leaves production working.

1. **Contract and runner.** The agent can install a signed plugin, run hooks
   with limits, seal its secrets and show its `describe`, `status` and `check`.
   No behaviour changes; nothing uses a plugin yet.
2. **ClickHouse plugin beside the built-in code.** The existing ClickHouse code
   is built as a plugin from the same source. A server can be switched to it;
   the built-in path stays the default. The two are compared on a test server
   through provision, backup, restore and a move.
3. **Adoption.** A server whose ClickHouse the agent installed hands it to the
   plugin without touching the data: the plugin's `provision` recognises the
   existing server and databases from the record the agent passes.
4. **Removal from the core.** Once every server is adopted, the built-in code
   and its branches are deleted from the agent and the control panel.
5. **Other plugins.** The same contract for application-specific support of
   known Rails, Node and Go applications.

## Stage 1 as built

A plugin release is three files: the archive (a gzip-compressed tar of
regular files with the `plugin` program at its top), `plugin.json` and
`plugin.sig`. `cmd/plugin-sign` makes them:

```sh
go run ./cmd/plugin-sign keygen -out plugin-signing.key
go run ./cmd/plugin-sign manifest -name example -version 1.0 -provides example \
    -hooks describe,check,status -env EXAMPLE_URL -archive amd64=plugin-amd64.tar.gz
go run ./cmd/plugin-sign sign -key plugin-signing.key
```

On a server, through the control panel's API (each call is audited and needs
the permission to manage infrastructure, except the list):

| Call | Does |
| --- | --- |
| `GET /api/control/servers/{server}/plugins` | installed plugins, the ids of the trusted signing keys, the server's architecture |
| `PUT .../plugins/keys` `{"keys": ["<hex public key>"]}` | replaces the keys an operator trusts on that server (at most 8), beside the keys pinned in the agent |
| `POST .../plugins/manifest` `{"manifest": "<plugin.json text>", "signature": <plugin.sig>}` | verifies the signature and the manifest; nothing is installed yet |
| `PUT .../plugins/{name}/archive` (`application/octet-stream`) | unpacks the archive if it matches the signed manifest, runs `describe`, records the plugin. A plugin that cannot describe itself is not kept |
| `POST .../plugins/{name}/hooks/{describe,status,check}` | runs a read-only hook and answers its answer, its log and how long it took |
| `DELETE .../plugins/{name}` | removes the program; the plugin's state directory stays |

What the agent enforces today: the signature (a domain of its own, so an
update bundle's signature is never a plugin's), the archive's digest and
size, no links or paths outside the plugin's directory, a minimal
environment without the agent's secrets, the plugin's state directory as
working directory, one hook at a time per plugin, a time limit per hook
after which the plugin and everything it started are killed, and bounded
answers and logs. Hooks that change a server cannot be run on request.

### Services for applications

A native application asks for a plugin's service by its `provides` name in
its create request (`data.plugins: [{"service": "search", "options": {…}}]`).
The agent then:

- refuses the request before anything is created when no installed plugin
  provides the service;
- includes the plugin's `check` answer in the rehearsal of the application
  (a move's review), with the sizes of the data that will arrive;
- runs `provision` with the application's id, account and options. The answer
  carries `env` (only variables the signed manifest declares reach the
  application, and none another service already set), `record` (the plugin's
  own description, kept by the agent and handed back on later hooks) and
  `secrets` (sealed in the agent's state like its own credentials, never
  stored in the clear);
- runs `release` with that record and those secrets when the application is
  removed, or when its creation fails part way.

### In the console, and installing from a release

A server's **Plugins** tab lists its plugins, runs a plugin's own check,
edits the signing keys trusted on that server and installs a plugin from its
published release: release `v<version>` of a public GitHub repository that
publishes `plugin.json`, `plugin.sig` and `plugin-<architecture>.tar.gz`
(`POST .../plugins/install` `{"repository": "owner/name", "version": "1.0"}`).
The control panel fetches the three files and relays them; the server's agent
still verifies the signature with the keys that server trusts and the archive
against the signed manifest.

### As part of the server

A plugin's last `status` is in server health. Its `check`, with no
application in mind, is in the server's readiness report; what it marks
repairable is fixed by its `repair` hook when the server is repaired; its
`maintain` hook runs hourly.

### Taking over from the built-in ClickHouse code

Where a plugin that provides `clickhouse` is installed and implements
`check`, the agent asks it instead of using its own ClickHouse rules: an
application's rehearsal shows the plugin's checks (given the series the
application comes from and the size of its data), the server's readiness
report leaves ClickHouse to the plugin, and a repair is the plugin's
`repair`. Servers without the plugin behave as before.

### Backups and restores

A plugin that implements `backup` gets a protected resource for each
application it serves (it appears among the server's databases, named by what
the plugin provides). Its backups are then in the same pipeline as a
database's: the schedule, one local copy, the off-site copy, the backup
before a release and a final one when the application is removed. For a
backup the agent gives the hook an empty directory (`work_dir`), with the
record and secrets `provision` answered, and archives the regular files the
hook leaves there. A restore verifies the archive's digest, unpacks it into a
fresh directory and runs `restore`, which replaces the service's data for the
application in place.

### Writing a plugin

The `pluginapi` package is the kit: the request and answer types the agent
itself uses, and `pluginapi.Main`, which reads the request, calls the handler
of the hook and writes the answer. `examples/plugin-example` is a complete
plugin (a notes directory per application) in about ninety lines; a test
builds it from source and takes it through the agent: signed, installed,
checked, provisioned for an application and released.

Not built yet: moves and tuning through hooks; naming a plugin in
`deploy/lidza.stack.json`; handing a ClickHouse the agent installed to the
plugin; and using the plugin instead of the built-in support when an
application asks for ClickHouse. Until those exist the built-in ClickHouse
code stays in the agent.

## Open questions

- Decided: the ClickHouse plugin lives in its own public repository; an
  application that needs it names it.
- Whether container applications' managed ClickHouse (a container per
  application) moves to the plugin in the same stages or after.
- How much of a move's incremental final sync the generic `export`/`import`
  hooks can express, or whether moves need a richer pair of hooks.
