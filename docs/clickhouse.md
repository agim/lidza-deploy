# Managed ClickHouse

Lidza Deploy provisions a private ClickHouse service on the app's hosting agent
when its `lidza.json` declares the existing non-secret deployment environment
setting below and no explicit `CLICKHOUSE_URL` is configured:

```json
{"deploy":{"env":{"CLICKHOUSE_MANAGED":"true"}}}
```

This uses the released framework's `config.Deploy.Env` contract. Docker service
orchestration belongs to the deployment agent; schema and migration execution
remain application responsibilities. No custom framework pack is required.

The first code deployment reserves a random password in encrypted agent state,
creates the `lidza-clickhouse-APP_ID` network/container and persistent `-data`
and `-logs` volumes, and waits for an authenticated query before building the
application. ClickHouse 26.8.15.10 is pinned. Allow an additional 2 GiB of host
memory for the service. No ClickHouse ports are published to the host.

The agent supplies `CLICKHOUSE_URL`, `CLICKHOUSE_DATABASE=telemetry`,
`CLICKHOUSE_USER=lidza`, and `CLICKHOUSE_PASSWORD` to app and task containers.
Connection values remain write-only in Settings. Managed connection keys cannot
be deleted or changed through the environment editor. The application receives
the private service network on first deploy, subsequent deploys and reloads.

Retries and agent restarts reuse the reserved password and existing volumes.
Stopped services restart on the next code deployment. A name occupied by an
unmanaged Docker resource causes refusal; the agent never replaces that resource
or erases data to fix a failed deployment. Service/authentication failure prevents
candidate activation and retains the previous release. App rollback does not
roll back ClickHouse schema or data.

Deployment diagnostics identify the failed stage and include bounded, redacted
Docker output. A first-time image download is reported separately from container
creation. Readiness failures include the last authenticated query error and
container status, exit code and OOM flag. The agent's retained password never
enters the deployment log.

After a failure, keep existing containers and volumes intact. Inspect the
deployment's specific failure before changing Docker connectivity. Retry after
correcting that condition; the reserved password and
volumes are reused. Download failures require checking Docker's registry access;
an application redeploy cannot repair a host networking or registry outage.

An explicit external `CLICKHOUSE_URL`, including sealed application credentials,
takes precedence before any managed service is created. Partial connection
settings without a URL are rejected; remove them to request automatic setup.
`CLICKHOUSE_MANAGED=false` opts out of first-time provisioning. Existing managed
attachments remain retained; this flag does not delete a database or remove its
connection. External-service configuration after managed provisioning currently
requires deliberate host/state administration rather than silently replacing
the managed connection.

App removal retains the service, volumes and encrypted password. Recreating the
same app ID receives a fresh resource identity, including for previews; it never
inherits retired telemetry. `/v1/retained-resources` reports the data volume
without credentials. Local ClickHouse is separate from managed PostgreSQL
backup schedules; configure application-specific ClickHouse backups separately.

For Kulla, update the hosting agent to this release, then deploy current `main`.
Its manifest requests ClickHouse; its existing `DB_MIGRATE=true` startup applies
all packaged ClickHouse migrations before readiness. Existing imported app keys
and PostgreSQL delivery jobs remain intact. Failed delivery jobs may still need
Kulla's targeted replay command, and explicitly paused workers must be resumed.

Acceptance covers encrypted credential persistence, failed setup and retry,
external connection preservation, removal/recreation isolation, and a real
Docker app deployment, reload and stopped-service redeployment with the original
row and password retained. Run the Docker test with `TEST_DOCKER=1`.
