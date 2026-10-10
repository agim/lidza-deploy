# Application caches

The hosting agent reads `lidza.json` before building. If the app enables
`lidza/cache` and has no `CACHE_URL`, it creates an authenticated Valkey service
on a private Docker network and attaches `CACHE_URL` and an app-specific
`CACHE_PREFIX`. This works on the next deployment of an existing or previously
failed application, as well as the first deployment of a new app. No root-level
cache port is published. Docker must be able to download the Valkey image.

Credentials are generated once and stored in the agent's encrypted state.
Redeployments, reloads and agent restarts reuse them. Existing `CACHE_URL`
environment values and production sealed credentials are preserved. Sealed
credentials still require the application's original `LIDZA_MASTER_KEY`; Deploy
does not generate or replace that key.

Open **Applications → Settings → Cache connection → Manage cache** to create
a local service explicitly, or attach an existing authenticated Redis/Valkey
service. Connection URLs are write-only. Use `rediss://` or `valkeys://` for TLS
over public networks, and a hostname reachable from application containers.
`localhost` in an app container refers to that container, not its hosting server.
An external connection is checked from the hosting agent before attachment; the
app's readiness check verifies connectivity from its container on reload.

A successful attachment saves the environment and reloads a running app with
readiness checks. A failed connection change preserves the previous connection.
For an app with no successful release, attachment saves the connection for its
next deployment. A failed reload retains the previous running release; check
deployment logs, correct the connection and reload again.

Local caches run as UID 65532, with a read-only root filesystem, a persistent
Docker volume, a 128 MiB eviction budget, and a 256 MiB container memory limit.
Append-only persistence syncs every second. This is a cache, not a replacement
for database backups or durable application storage. Cache data is not included
in the PostgreSQL/S3 backup schedule. Switching services does not migrate data.
Removing an app or attaching an external service retains its local cache volume
and container; stop unused services and retain or remove their volumes according
to your recovery needs. Back up the agent's encryption keys together with its
state so connections remain recoverable.

The preflight also checks the released Līdza production requirements for enabled
official packs, reporting all missing or development-only settings together
before compilation. Additional mail, provider and app-specific configuration
still belongs in application Settings. Deploy does not invent provider accounts
or silently substitute an in-memory cache.
