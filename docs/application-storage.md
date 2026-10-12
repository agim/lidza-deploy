# Persistent application storage

Apps enabling `lidza/storage` receive a private persistent Docker volume when
`STORAGE_PROVIDER` is `local` or is omitted. The agent attaches it at
`/var/lib/lidza-storage` and supplies `STORAGE_PROVIDER=local` and `STORAGE_DIR`
automatically. This runs during deployment, including for apps that never
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

This is single-server storage: it does not replicate across agents, and PostgreSQL
backup schedules do not include it. Back up the app's Docker volume separately,
or configure the framework's S3-compatible provider for off-server storage.
App removal retains the volume separately and releases the application ID.
Recreating that ID gets a new volume and never adopts the retained files.
**Databases & backups → Retained application data** lists retained volume names;
names for recreated apps include an agent-generated resource suffix. Restoring a
host also requires the agent's state and encryption keys, together with the
volume's files.
