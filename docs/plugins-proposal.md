# Proposal: plugins for application-specific software

Status: proposal, not implemented. Nothing here changes the agent yet.

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

One executable, installed on a server under
`/usr/local/lib/lidza/plugins/<name>/<version>/plugin`, with a manifest
`plugin.json` beside it. The agent runs it as `plugin <hook>`, writes one JSON
request to its standard input, reads one JSON answer from its standard output
and keeps its standard error as the hook's log. Large data never goes through
the JSON: hooks that produce or consume data are given a directory.

A plugin has no network API, keeps no process running for the agent, and
stores its state in a directory the agent gives it
(`/var/lib/lidza/plugins/<name>`). Secrets it needs to keep (an administrator
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

## Open questions

- Where the ClickHouse plugin lives: its own repository, or the repository of
  the application that needs it.
- Whether container applications' managed ClickHouse (a container per
  application) moves to the plugin in the same stages or after.
- How much of a move's incremental final sync the generic `export`/`import`
  hooks can express, or whether moves need a richer pair of hooks.
