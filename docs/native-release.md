# Native releases through the release engine

The release engine (`internal/release`) deploys a native application into the
conventional `releases/<timestamp>`, `current` and `shared/` layout and
restarts the owner's existing service units. The agent wires it to the host
through one reviewed object per application: `NativeReleasePolicy` under
`native.releases` in the agent's `config.json`.

## Policy

```json
"releases": {
  "rails-example": {
    "enabled": true,
    "root": "/home/deploy/example",
    "repository": "owner/example",
    "branch": "main",
    "runtime": "rails",
    "domain": "example.com",
    "health_path": "/up",
    "units": ["example-server.service", "example-solid_queue.service"],
    "preserve": ["example-legacy-import.service"],
    "env_files": ["/home/deploy/example/.env.production"],
    "databases": ["example-primary"],
    "keep": 5
  }
}
```

The policy must describe the registered native application exactly: `root`
is `<native root>/<app name>`, `repository` and `branch` match the
registration (a settings override of the branch disables the policy until it
is reviewed), and `domain`, when set, matches too. `units` are restarted in
order, web servers first; only units that are running at deploy time are
restarted, so stopped or socket-idle units stay as they are. `preserve` lists
units the operator keeps stopped or disabled: a release never restarts,
starts or enables them. `runtime` is `rails`, `node`, `static` or `auto`
(detected from the current release). `databases` names the protected native
database resources backed up, with off-site verification, before any release
step such as `db:migrate` runs; without them an application with release
steps is refused rather than migrated unprotected.

## What a deploy does

1. Fetches the branch or exact commit into `<root>/repo` (a bare mirror)
   with a short-lived GitHub App token passed only through git's environment.
2. Exports the commit into `releases/<timestamp>`, links `shared/` paths,
   runs the profile's build steps, and renders the application's schedule.
3. Runs the pre-release backup gate, then the release steps.
4. Atomically repoints `current`, restarts the running units through the
   owner's `systemctl --user`, and probes `https://<domain><health_path>`
   through the local web server (SNI and Host set, no DNS). A failed restart
   or probe republishes and restarts the previous release.
5. Replaces the application's block in the owner's crontab and prunes old
   releases, keeping `keep` (default 5) for rollback.

Rollback republishes an earlier release the same way and restores that
release's schedule. Migrations are not reversed.

## Confinement

Every build, release and schedule command runs as the application owner in
a transient systemd unit: `ProtectSystem=strict`, `ProtectHome=read-only`,
write access to the application root only, private `/tmp`, 1 GiB memory,
half a CPU, a 30 minute limit, no new privileges and no capabilities after
`setpriv`, inside private mount and PID namespaces. Build steps without
network run under `PrivateNetwork` with database sockets hidden; package
installs reach the internet but not loopback services (`IPAddressDeny`);
only database steps can reach the application's databases. The owner's
`systemctl` runs unsandboxed but unprivileged, without opening a session,
so a stopped user manager is never started. The crontab is replaced as root
and a write that would drop any line outside the application's own block is
refused.

## API

- `POST /v1/native/apps/{id}/release` `{"ref": "<branch or commit>", ...}`
  queues a deploy; the body also accepts the usual checkout credentials.
- `POST /v1/native/apps/{id}/deploy` behaves the same for release-managed
  applications, which is what GitHub pushes and the control panel use.
- `POST /v1/native/apps/{id}/rollback` `{"release": "<id>"}` queues a
  rollback to that release, or to the previous one.
- `GET /v1/native/apps/{id}/releases` lists releases with their commits.
- `GET /v1/native/apps/{id}` reports `release_managed: true` and
  `deployment_supported: true` when the policy applies.

Jobs appear in `/v1/deployments` with kind `deploy` or `rollback`, live
logs, the commit, and `live` or `failed`.

## GitHub pushes

A push to the deployed branch of a native application with auto-deploy
enabled is dispatched to the agent only while the agent reports
`release_managed` for it and the registration still matches. Applications
without a policy keep today's behaviour: pushes are acknowledged and dropped.

## Agent unit write access

Release management writes `repo/`, `releases/<id>`, `shared/`, `current` and
`.release.lock` under each managed application root, and installs the owner's
crontab. When the agent unit restricts writes (for example with
`ProtectSystem=strict`, `ProtectHome=read-only` or `ReadOnlyPaths=`), add a
drop-in with one `ReadWritePaths=` line per managed root plus
`ReadWritePaths=/var/spool/cron/crontabs`, then run `systemctl daemon-reload`
and restart the agent; the restart interrupts the agent only. Listing a root
grants nothing until that application has an enabled policy. Applications the
agent creates itself live under roots it already manages.

## Not covered yet

- Memory above 1 GiB for unusually large asset builds (policy knob to add).
- A control-panel view of the release list; the agent API exists.
- Automatic runtime detection needs a published current release.
