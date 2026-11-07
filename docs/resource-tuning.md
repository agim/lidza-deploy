# Resource tuning

Each server sizes its data services in proportion to its CPUs, memory, disk and
number of applications. The agent recomputes at start (so after every reboot or
resize) and hourly; every change is a resource-tuning host job that records the
old and the new values.

- **PostgreSQL**: host cluster settings in a managed `conf.d` include. Values
  that apply on reload are applied at once; values that need a restart wait for
  the server's maintenance window. Every application gets its own connection cap
  from its observed peak and configured pool, never below the sessions in use;
  computed caps wait for a day of samples, and caps change only when the target
  moves by more than a tenth.
- **Redis**: memory limits and eviction by use. Caches evict old keys; queues
  and persistent data never evict (they keep 1.5 times their used memory) and
  warn above 80% of their limit.
- **ClickHouse**: server memory by the number of users.
- **Container databases**: their PostgreSQL settings follow the same sizing.

The server page's **Resources** section shows the targets. **Review changes** is
a dry run and **Apply now** applies it. Any value can be overridden (**Save
overrides**, then review and apply); **Pause automatic tuning** keeps the targets
visible without applying them. **Move to its own Redis** gives an application on
the shared Redis a private instance; the old database is cleared only once the
application runs healthily on the new one. An archived application restored
later gets its Redis instance back with its tuned policy.

API:

    GET  /api/control/servers/{server}/host/resources
    POST /api/control/servers/{server}/host/resources/apply
    PUT  /api/control/servers/{server}/host/resources/overrides
    POST /api/control/servers/{server}/host/resources/redis/{app}/migrate
