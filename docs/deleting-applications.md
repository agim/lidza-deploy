# Deleting and restoring applications

Deleting an application moves it into a restorable archive on its server for
90 days. Choose **Delete** in the card's **More** menu, or **Delete application**
in the **Danger zone** at the end of the General settings, read the two lists in
the dialog and type the application id; a reason is optional. Deletion is
refused while the application deploys or moves.

**Kept for 90 days:** its settings (environment, hostnames, redirect domains,
scheduled tasks), its files and its databases. Databases stay in place with
every login disabled: containers stopped, host roles `NOLOGIN`, ClickHouse users
`HOST NONE`.

**Deleted at once:** the cache, every local and off-site backup, Caddy routes,
TLS subjects, redirect routes and certificates nothing else uses, service TLS
copies, gateway identity, published ports, tasks, releases, native units and
account, move copies held by other servers, stored console errors and the
repository webhook. The application stops serving immediately and its hostnames
are free for other applications. Off-site backup folders are deleted whole only
when no other server has a database with that id.

Applications adopted from a server's existing setup are archived with what
belongs to them alone: their units are stopped and disabled and their unit
files removed (including user units bound to the application directory and
system units named `<app>-<role>`), their crontab block, routes, error-page and
access-log hostnames, exclusive Redis database and backups are removed, an
exclusive database role gets `NOLOGIN` while a shared role only loses `CONNECT`
on the application's database, and the directory moves into the archive. The
shared deployment account, its home, other applications and shared instances are
never changed. A sweep at start and hourly re-applies this to archives made by
earlier agents.

## Restore

**Workspace → Deleted apps** lists every server's archives with the days left.
**Restore** recreates the application on the same server from its settings, with
its files and databases, an empty cache, the hostnames no other application took
meanwhile, its webhook and a new deployment of its branch. If another
application took its id, the dialog asks for a new id; a new primary domain can
be given if its domain was taken. Backups start again with the next scheduled
backup. An adopted application keeps its directory name, is restored with all
its hostnames or not at all, and is not deployed.

## Purge

After 90 days the server deletes the archive permanently, databases included,
even while the control panel is offline; **Delete now** does the same at once.
A purge never runs during a restore and takes no final dump. Purges done by the
agent on its own are recorded in the panel's audit log.

## API

    DELETE /api/control/apps/{id}                 {"confirm": "<id>", "reason": "..."}
    GET    /api/control/archives
    POST   /api/control/archives/{id}/restore
    DELETE /api/control/archives/{id}

Agent: `POST /v1/apps/{id}/archive`, `GET /v1/archives`,
`POST /v1/archives/{id}/restore`, `DELETE /v1/archives/{id}`. Every operation is
a resumable tombstone with its own log.
