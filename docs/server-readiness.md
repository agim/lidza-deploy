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

## API

- `GET /api/control/servers/{server}/host/readiness`: `ok` and `checks`
  (`id`, `title`, `ok`, `detail`, `repairable`).
- `POST /api/control/servers/{server}/host/readiness/repair`: repairs and
  answers the report with `repaired` and `failed`.
