# Persistent application storage

Apps enabling `lidza/storage` receive a private persistent Docker volume when
`STORAGE_PROVIDER` is `local` or is omitted. The agent attaches it at
`/var/lib/lidza-storage` and supplies `STORAGE_DIR` automatically. It does not
set `STORAGE_PROVIDER`: the framework defaults to local, and a provider saved
through the app's **Admin → Storage** page can take effect without another
Deploy setting. This runs during deployment, including for apps that never
successfully deployed. Update the hosting agent, then deploy the app again.

The framework's local provider can run in production on one node with a lasting
disk. Deploy permits it only after creating and verifying that attachment; it
continues rejecting unattached local storage in production preflight. The rest
of the container filesystem remains read-only and the app runs as UID 65532.
Each app has a separate volume. Existing volumes without the expected ownership
label are rejected rather than adopted.

Deployments, reloads, rollbacks, workers, and scheduled jobs use the same volume.
App settings show `STORAGE_DIR` as managed. Changing it through environment
settings is blocked, so uploads cannot silently move onto an ephemeral path.
Supplied S3-compatible providers and credentials are preserved. Changing providers
does not migrate existing files. Files from older containers are not copied into
the new volume automatically.

Older agents added `STORAGE_PROVIDER=local` without recording whether the operator
had already set it deliberately. These ambiguous overrides are preserved during
upgrade. After updating the hosting agent, open **Application settings → Application
storage → Let the app manage storage** to remove the deployment override explicitly.
Running apps reload through the readiness gate; stopped or undeployed apps use the
change when next started or deployed. The existing volume and local files remain
attached, including while an external provider is selected.

Explicit provider choices in Deploy settings, the project's `deploy.env`, or the
Dockerfile still override saved app settings. Remove those choices if you want the
app's Admin page to control the provider. Deploy's backup destination is separate
from the hosted application's file storage.

This is single-server storage: it does not replicate across agents, and PostgreSQL
backup schedules do not include it. Back up the app's Docker volume separately,
or configure the framework's S3-compatible provider for off-server storage.
App removal retains the volume separately and releases the application ID.
Recreating that ID gets a new volume and never adopts the retained files.
**Databases & backups → Retained application data** lists retained volume names;
names for recreated apps include an agent-generated resource suffix. Restoring a
host also requires the agent's state and encryption keys, together with the
volume's files.
