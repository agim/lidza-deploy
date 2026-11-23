# Server readiness

A server is ready when everything an application needs on it works. The agent
checks that itself, repairs what has one safe answer, and names the rest so
nobody discovers it in the middle of a deployment or a move.

## What is checked

| Check | Ready when | Repaired by the agent |
| --- | --- | --- |
| Native applications | the deployment account exists and is named in the agent's configuration | yes, when the agent starts (see [application runtimes](application-runtimes.md)) |
| PostgreSQL | the cluster applications use is online, a primary and owned by `postgres` | a cluster whose service **failed**: the agent starts it, first letting the `postgres` account enter a directory on the way to its data directory when that is what stops it |
| Docker | the daemon runs (where Docker is installed) | yes, it is started |

| ClickHouse, Redis | the service runs (where installed) | a service that **failed** is started; one stopped without failing is left alone and not reported |
| Web server (Caddy) | it runs, on servers where the installer set it up as the web entrance | yes, it is started |
| Disk space | the root file system and each PostgreSQL data disk have more than 2 GiB and 5% free | no: named with the path and what is free |

A PostgreSQL cluster that is stopped without having failed was stopped on
purpose; a standby and a cluster with another owner are somebody's decision
too. Those are reported with the cluster's name and port and left unchanged.

Letting `postgres` into a directory adds an access-control entry for that
account alone (`setfacl -m u:postgres:x`), or the search bit for others where
the tool is missing. Neither lets anyone list or read the directory.

## When it runs

- **Installer:** its last step asks the agent to repair, and prints what was
  repaired and what still needs a person.
- **Creating or moving an application:** before the agent provisions a
  database it repairs a failed cluster instead of stopping. If it cannot, the
  error says which cluster, what is wrong and what to do.
- **A move's review:** the destination's problems are listed before the move
  starts: as a blocker when a person must decide, as a note when the
  destination will repair it itself.
- **The server's Overview** shows the report under *Ready for applications*,
  with **Repair now**.

## Rehearsing an application before a move

A move's review does more than read that report. The destination agent
**rehearses the creation of that application** with the sizes of its data
(`POST /v1/native/preflight`) and changes nothing:

- the create request itself (names, runtime, processes, conflicts), the
  application's account and a free port;
- PostgreSQL, Redis and ClickHouse as that application needs them: usable,
  repairable, or installed on creation;
- the free space **on the file system each service stores its data on**
  against the size of the data arriving there. A database on its own disk is
  measured on that disk, not on the root file system;
- the web route: the Caddy that owns the server's web ports answers.

The review shows the steps under *Destination rehearsal*. A step only a
person can resolve blocks the move; a step the destination repairs itself
when the move prepares it is marked *Repaired at start*.

## API

- `GET /api/control/servers/{server}/host/readiness`: `ok` and `checks`
  (`id`, `title`, `ok`, `detail`, `repairable`).
- `POST /api/control/servers/{server}/host/readiness/repair`: repairs and
  answers the report with `repaired` and `failed`.
