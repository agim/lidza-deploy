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
steps is refused rather than migrated unprotected. Every database bound to
the application is backed up, listed or not.

What that backup is, is each database's own **Before deploys** setting in
the panel (**Applications → Settings → Data & storage**, or the database's
page; see [databases and backups](databases-backups.md#before-deploys-per-database)):
a fresh verified backup every time (the default), a verified backup newer
than N hours when one exists (scheduled, manual or pre-deployment, with its
off-site copy) and a fresh one otherwise, or — for a secondary database the
application's migrations do not change, for example one that outside
collectors write to — scheduled backups only. The deploy log states the
decision for every database (`Reusing backup <id> from 2h ago …`, `Taking a
fresh backup of …`, `Skipping the pre-release backup of …`), and the release's
backup proofs record the reused backup's id and age. Primary, analytics and
standalone databases cannot choose scheduled backups only. A native database
policy may carry `"before_deploy": {"mode": …, "max_age_hours": …}` as the
database's initial value; the earlier `"skip_release_backup": true` is still
read as scheduled backups only. Both yield to the panel's saved choice.

Each database's backup deadline follows the database: the largest of the
configured minimum (`predeployment_backup_minutes`, default 10 minutes),
twice its last successful backup's duration plus five minutes, and its
current size at a conservative 4 MiB/s plus five minutes, at most four
hours. The deploy log states the deadline and how it was derived, and the
release's own one-hour limit is extended by these deadlines.

## What a deploy does

1. Fetches the branch or exact commit into `<root>/repo` (a bare mirror)
   with a short-lived GitHub App token passed only through git's environment.
2. Exports the commit into `releases/<timestamp>`, links `shared/` paths,
   runs the profile's build steps, and renders the application's schedule.
3. Runs the pre-release backup gate, then the release steps (for Rails,
   `db:prepare`).
4. Atomically repoints `current`, restarts the running units through the
   owner's `systemctl --user`, and probes `https://<domain><health_path>`
   through the local web server (SNI and Host set, no DNS). A failed restart
   or probe republishes and restarts the previous release.
5. Replaces the application's block in the owner's crontab and prunes old
   releases, keeping `keep` (default 5) for rollback.

Rollback republishes an earlier release the same way and restores that
release's schedule. Migrations are not reversed.

### When a health check fails

The deploy log explains the failure before the previous release is
republished (and again if that release fails its own check). It shows the
last HTTP answer the health check received, when there was one (status and
the first 300 bytes of the body), and for every unit the restart reached its
state from `systemctl show` (`ActiveState`, `SubState`, `Result`,
`NRestarts`, exit status and code of the main process) followed by the last
40 lines that unit wrote to the journal since the restart began. System units
are read as root; the units of an adopted application through the owner's
`systemctl --user` and `journalctl --user`. Values from the application's
environment files are masked in this output as in the application's log view.

```
==> checking health
==> last health response: HTTP 503
upstream unavailable
==> example-web.service since 14:02:11: ActiveState=activating SubState=auto-restart Result=exit-code NRestarts=6 ExecMainStatus=1 ExecMainCode=exited
AUTH_SECRET is not set; refusing to start
==> new release failed (release: health check failed: ...); republishing 20261106140000
```

### Gem housekeeping

For Rails applications `bundle clean` runs after each `bundle install`. It
removes gem versions the lockfile no longer needs from the application's own
bundle path (`shared/bundle`, installed in deployment mode) and never touches
system gems. The log carries one line when something was removed, with the
count and the space freed, and nothing otherwise; a failure of `bundle clean`
is logged as a warning and never fails the deploy. `"bundle_clean": false` in
the policy keeps every installed gem version.

## Confinement

Every build, release and schedule command runs as the application owner in
a transient systemd unit: `ProtectSystem=strict`, `ProtectHome=read-only`,
write access to the application root only, private `/tmp`, a memory cap
(see below), a 30 minute limit, no new privileges and no capabilities after
`setpriv`, inside private mount and PID namespaces. Build steps without
network run under `PrivateNetwork` with database sockets hidden; package
installs reach the internet but not loopback services (`IPAddressDeny`);
only database steps can reach the application's databases. The owner's
`systemctl` runs unsandboxed but unprivileged, without opening a session,
so a stopped user manager is never started. The crontab is replaced as root
and a write that would drop any line outside the application's own block is
refused.

## Build resources

Builds on a host run one at a time: native releases, static sites and
container image builds share one first-come, first-served build slot.
Release steps (migrations), publication, restarts and health checks run
outside it. A deployment that waits shows "Waiting for another build on this
server (deploying <app>)" in its log and in its live `progress`.

Each build step's memory cap is computed when the step starts:

- `build_memory_mib` in the policy, when set, is the ceiling;
- otherwise 1.5 × the step's learned peak (the largest of its last five
  measured peaks), or a quarter of the host's memory (2–8 GiB) before a peak
  is known;
- bounded by the free memory: `MemAvailable` minus a reserve for running
  applications (10% of the host's memory, at least 512 MiB), and never
  below 512 MiB.

When the free memory is below what a step is known to need, the step waits
up to 10 minutes, with a line every 30 seconds, and then fails with "this
server does not have enough free memory to build <app> (needs ~X,
available Y …)". JavaScript steps get three quarters of the cap as their
Node.js heap. Steps run without swap, so running out of memory fails fast.

After each step the log shows its peak (`npm ci: peak 1.4 GiB of 6.0 GiB`).
Peaks come from systemd's end-of-unit accounting ("memory peak" in the
journal) and from the unit's cgroup (`memory.peak`, or the largest
`memory.current` sample) for systemd versions that do not log it. The agent
keeps the last 48 peaks per application in its state (`build_peaks`).

A build step that runs out of memory is retried once from a clean scratch
directory with twice the cap (bounded by free memory and by
`build_memory_mib`) and with swap allowed when the host has swap. Build
steps are idempotent within their release directory (`npm ci`, `bundle
install`, `assets:precompile`, `lidza build`, `go build` …); release steps
are never retried. A second failure, or no room for more memory, fails the
deployment with the learned peak and the server's memory.

Build steps get every CPU core but one (at least one whole core) with low
CPU and I/O weights and `Nice=10`, so running applications win under
contention.

Container image builds take the same slot and wait for free memory, but
run inside the Docker daemon's builder, which per-build memory caps do not
reach.

## Ruby and Bundler versions

Each RubyGems release ships a default Bundler: RubyGems 3.x.y ships Bundler
2.x.y (same minor and patch) and RubyGems 4.x.y ships Bundler 4.x.y. When
`Gemfile.lock` records an older Bundler under `BUNDLED WITH`, RubyGems loads
that version if it is installed (Bundler installs it otherwise), and an old
Bundler on a newer RubyGems prints `already initialized constant
Gem::Platform::JAVA` on every command.

Before the build, a Rails release whose lockfile records its Bundler reads the
selected Ruby's version, `gem --version` and `gem list -e bundler`. When the
pinned Bundler's major.minor is older than the host's default Bundler, the
release uses the default one: `BUNDLER_VERSION` is set for build, release and
schedule commands, and written to `<release>/.lidza-toolchain/env`. The deploy
log says so in one line, for example `Bundler 2.6.9 is older than this host's
RubyGems 4.0.23; using Bundler 4.0.23`. A matching or newer pin, a lockfile
without `BUNDLED WITH`, or a `BUNDLER_VERSION` in the application's own
environment leaves the release unchanged. Gems are never installed or removed
on the host.

Units the agent renders for applications with their own account read
`current/.lidza-toolchain/env` (optional, before the application's `.env`), so
processes follow the release on every rollback. Units the agent did not render
keep their own environment. Existing created applications pick the line up the
next time their units are written.

**Servers → Server resources** reports Ruby toolchains from the latest release
of each application. Older pins need nothing from the operator and appear as a
collapsed note, for example `Rails releases on Ruby 3.4.4 use the host's
Bundler 4.0.23; 3 applications pin older versions in their Gemfile.lock`.
Warnings are the actionable states: older Bundler versions still installed
beside the default one (remove them with the server's **Remove unused Bundler
versions** action), and a pinned Bundler newer than the host's RubyGems ships,
which a release cannot activate (update the host RubyGems or pin the host's
Bundler with `bundle update --bundler`).

## Managed Ruby

With managed toolchains (`toolchains: managed`, the default for applications
created in the panel), a Rails release gets the CRuby version it pins:
`.ruby-version` (`3.4.4` or `ruby-3.4.4`; a minor pin such as `3.4` selects
the newest patch release), else the `ruby` line of `.tool-versions`, else a
`ruby "3.4.4"` line in the Gemfile. The agent installs it once per server
under `/usr/local/lib/lidza/ruby/<version>` and links it into
`<release>/.lidza-toolchain/ruby`; build, release and schedule commands and
the agent's system units put its `bin` on `PATH`. Sources, in order:

1. A prebuilt binary from [ruby/ruby-builder](https://github.com/ruby/ruby-builder)
   (the builds `setup-ruby` uses) for the server's Ubuntu release and
   architecture (22.04, 24.04, 26.04; x64 and arm64), verified against the
   SHA-256 digest GitHub publishes for the release asset. Assets without a
   digest are not used. These builds are compiled for the prefix
   `/opt/hostedtoolcache/Ruby/<version>/<arch>` and are not relocatable, so
   that path becomes a link to the installation; the link is only created
   when nothing else occupies the path, the build's `rbconfig.rb` must name
   it, and the interpreter must load openssl, psych and zlib before it is
   kept. A checksum mismatch fails the deploy.
2. Otherwise the official source tarball from cache.ruby-lang.org, verified
   against the SHA-256 in `cache.ruby-lang.org/pub/ruby/index.txt`, built
   with `./configure --prefix=… --disable-install-doc --enable-shared`,
   `make -j N` (N follows the build CPU quota) and `make install` into a
   staging directory, as root in a transient unit under the same memory and
   CPU limits as build steps. On servers whose packages the agent manages
   (root agent, Ubuntu or Debian) the missing build packages are installed
   first: build-essential libssl-dev libyaml-dev zlib1g-dev libffi-dev
   libgmp-dev libreadline-dev (gems with native extensions need them too).
   Ruby enables YJIT by itself when `rustc` is installed.

Installs share the managed toolchain lock and appear in one rename. The
deploy log shows `Ruby 3.4.4: installed (prebuilt, 38 s)` or
`Ruby 3.4.4: already installed`. Gems stay per application: Bundler installs
into `shared/bundle` as before, and `GEM_HOME`/`GEM_PATH` point at
`<root>/shared/gems` (with RubyGems' own default directory appended), so no
gem is shared between applications. The Bundler pairing above applies to the
managed Ruby as well.

Policies with `toolchains: account` keep using the account's version manager
(asdf), unchanged. To move such an application to a managed Ruby, set
Toolchains to managed in its settings (or `toolchains: managed` in its
policy). Nothing changes until the next deploy, which installs the pinned
Ruby and builds the release with it; rollbacks to earlier releases keep the
Ruby they were built with. Service units that the operator maintains keep
their own `PATH`: point them at `<root>/current/.lidza-toolchain/ruby/bin`
when the processes should run the managed Ruby too. No application is
switched automatically.

## Toolchain cleanup

Server cleanup also removes unused toolchain versions: Go, Node.js and Ruby
in the managed root, rustup toolchains there, and asdf (ruby, nodejs, golang,
rust), rbenv and rustup versions in application accounts. A version stays
when any kept release of any application on the server names it (toolchain
links or pin files: go.mod, .node-version, .nvmrc, package.json engines,
.ruby-version, .tool-versions, Gemfile, rust-toolchain), when a running
process executes from it, when it changed within 7 days, when no application
of that account uses the language, or when it is the account's global default
and either an application relies on the default or no other installed version
is in use (the preview notes these). System packages are never touched. The
step is skipped while a deploy or build runs, while a toolchain is being
installed, and for the deployment account when a registered application is
not deployed by the release engine. The preview (`GET /v1/maintenance?preview=1`)
lists each version with its size and reason; a run logs lines such as
`Removed Ruby 3.3.4 (asdf, deploy): 210 MiB freed` and a total.

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

- A control-panel view of the release list; the agent API exists.
- Automatic runtime detection needs a published current release.
