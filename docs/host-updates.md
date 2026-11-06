# Operating-system updates

Each server's **Connection, updates & firewall** page shows pending package
updates and runs them through the server's agent
(`/v1/host/updates`, proxied as `/api/control/servers/{server}/host/updates`).
The agent never refreshes package lists while observing; apt's daily timer
does.

## What counts as a security update

A package is a security update only when a repository offering its new
(candidate) version is a security pocket: a suite, archive or label
containing `security`, such as Ubuntu `noble-security` or Debian
`stable-security`. The agent reads this from `apt-get -s dist-upgrade` and
confirms it per package with `apt-cache policy`, so a mirror that serves both
`-updates` and `-security` is told apart by suite, and a repository cannot
qualify by its origin name.

Docker's, Caddy's and the PostgreSQL project's (PGDG) repositories have no
security pocket, so their updates never count as security updates, however
they are labelled elsewhere. Unattended upgrades keep their configured
origins (see *Automatic updates*).

## Disruptive packages

Some upgrades interrupt every hosted application or need a reboot:

| Packages | Effect |
| --- | --- |
| `docker-ce`, `docker-ce-cli`, `containerd.io`, `docker.io` | restarts the container runtime: all containers restart unless Docker live-restore is on |
| `postgresql`, `postgresql-*` (not clients or docs) | restarts PostgreSQL: every database connection drops |
| `redis-server`, `valkey` | restarts Redis: caches and queues reconnect |
| `caddy` | restarts Caddy: HTTPS traffic pauses |
| `linux-image-*`, `linux-modules*`, kernel meta packages | the server must reboot to run the new kernel |
| `systemd`, `systemd-sysv` | re-executes systemd; a reboot is recommended |

The status lists them separately (`disruptive`, `disruptive_packages[]` with
`effect`). For every manual run — *Install security updates now* and *Install
all updates* — they are excluded unless the request carries
`"include_disruptive": true`. The panel sends that only from *Include
disruptive updates*, after a confirmation that lists each package and what it
restarts.

Exclusion works by pinning the candidate version of every package outside the
selection to priority -1 for that one `apt-get upgrade` run, through a
temporary `Dir::Etc::PreferencesParts` directory that also carries the host's
own `preferences.d` files. Nothing under `/etc/apt` changes. A selected
package that needs a held one is kept back by apt and named in the job
summary.

## Maintenance window

The reboot window is the server's maintenance window. With
`disruptive_in_window` (default on):

- the agent's unattended-upgrades drop-in
  (`/etc/apt/apt.conf.d/52lidza-unattended-upgrades`) blacklists the
  disruptive packages, so a daily run never restarts the container runtime or
  a database;
- inside the window the agent installs the pending disruptive upgrades once
  (job `disruptive-updates`, trigger `schedule`, also written to the agent
  log); a reboot they require follows in the same window when automatic
  reboots are on.

Turning the option off returns the blacklist to the distribution's defaults;
disruptive packages then install only on request.

## Docker live-restore

With live-restore, containers keep running while the Docker daemon restarts
or is upgraded. The agent reconciles it where it manages Docker: it installed
Docker Engine, or it runs container applications on the host. Hosts that
merely have Docker are reported, not changed.

Hourly, when the running daemon reports live-restore off, the agent

1. reads `/etc/docker/daemon.json` (a malformed file is reported and left
   untouched), saves the previous file as `daemon.json.bak-<UTC time>`,
2. merges `"live-restore": true`, keeping every other key, and validates it
   with `dockerd --validate` where available,
3. reloads with `systemctl reload docker.service` (SIGHUP; `live-restore` is
   one of the options dockerd reconfigures on reload, so no container
   restarts), and
4. confirms with `docker info` (`LiveRestoreEnabled`).

If the daemon does not apply it on reload, a restart is the only way; the
agent records it as pending and restarts Docker in the next maintenance
window (job `docker-restart`), never outside it. The status reports
`docker.live_restore`, `docker.live_restore_configured`, `docker.managed`
and a `docker.warning` while it is off.

## Control-panel database port

The installer-managed PostgreSQL container for the control panel publishes a
fixed loopback port, `127.0.0.1:5433` or the next free port up to 5499. The
choice is sealed as `SETUP_DB_PORT` with the other credentials, so the
saved `DATABASE_URL` stays valid when Docker restarts the container.

Existing installations whose container was created with a random port are
recreated once on a fixed port at the next panel start, before the panel
serves: the container is stopped and removed, the named data volume is kept
and remounted. While the panel runs, it probes its database every 15
seconds; after two failures it asks Docker for the labelled container's
current port (`io.lidza.control.database=<id>`, `docker port <name>
5432/tcp`). When it moved, the panel saves the corrected `DATABASE_URL`, logs
the change and exits cleanly so systemd restarts it onto the new address.
External databases are never probed or changed.

## API

- `GET /v1/host/updates` adds `disruptive`, `disruptive_packages`,
  `docker` and `next_disruptive_window`; each package carries `disruptive`
  and `effect`.
- `PATCH /v1/host/updates` accepts `disruptive_in_window` (omitted = on).
- `POST /v1/host/updates/run {"scope": "security" | "all",
  "include_disruptive": false}`.
- Job kinds: `security-updates`, `all-updates`, `disruptive-updates`,
  `configure`, `reboot`, `docker-live-restore`, `docker-restart`, `firewall`.
