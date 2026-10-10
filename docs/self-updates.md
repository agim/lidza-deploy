# Control panel and agent updates

Open **Updates** in the GUI. No GitHub account, application registration, repository webhook or webhook secret is required for release updates.

- **Check now** fetches the latest stable public Līdza Deploy release and shows installed control-panel and agent versions.
- **Update control panel & agents** queues a durable batch using the installed CLI updater on each connected server.
- **Automatically install stable releases** opts this installation into hourly release checks and installation. It is disabled by default. Checks and queued work continue when the browser is closed.

Remote agents update first, one at a time. The colocated agent on the control-panel host updates last; its helper replaces both the agent and GUI binaries. The browser can briefly lose connection while the GUI restarts. Reload Updates after it reconnects. An installation with a remote-only GUI must also connect the local agent on its GUI host to update that host.

The root helper downloads assets and checksums from the fixed official release repository, verifies the archive and bundled checksums, restarts services and checks readiness. Failed startup restores previous binaries. Hosted application containers continue running. OAuth, operator accounts, database data and application configuration are preserved.

A busy or temporarily unreachable server waits and is retried by the framework's durable minute schedule. Unsupported installers and failed upgrades stop the batch before updating remaining servers. Fix that host, then click Update again. Automatic mode does not repeatedly install a failed release; a manual retry or a newer release is required.

Install this release once with the current installer on the GUI host and each hosting server to provision the latest CLI helper. Subsequent updates refresh the verified helper too. Existing **Servers → Agent upgrade** remains available for updating individual hosts.

## Removal of the former self-update webhook

The `/hooks/self-update` endpoint and `LIDZA_SELF_UPDATE_SECRET` / `LIDZA_SELF_UPDATE_SERVER` settings have been removed. The current installer and CLI helper delete these two retired environment entries; they leave OAuth and hosted-app webhook secrets intact.

If you manually created a repository hook whose URL ends in `/hooks/self-update`, delete that hook in GitHub repository settings. App deployment hooks (`/hooks/github/...` and `/hooks/github-app`) continue to work and must be kept.

## Automatic swap checks

The installer and updated hosting agents check swap automatically. Existing active swap is preserved; configured inactive swap is activated. If none exists, Līdza Deploy creates a 4 GiB, root-owned swap file at `/var/lib/lidza-deploy.swap` and registers it in `/etc/fstab`. Provisioning requires ext4 or XFS and at least 6 GiB free space, leaving a 2 GiB reserve. Existing files at the reserved path are never reformatted or overwritten.

An existing agent updated through the GUI checks after its verified root helper has been refreshed, normally within one minute. Agents recheck daily. **Servers → Server health** shows total/free swap and the provisioning result. Failures are also logged in `lidza-agent-upgrade.service`. Swap checks do not require a deploy-user sudo grant. Installation staging never provisions host swap.

Swap reduces the chance of build-time out-of-memory failures, but heavy builds can still need a larger server. Disk-backed swapping can slow builds; it is not equivalent to additional RAM.
