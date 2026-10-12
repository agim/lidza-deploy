# Control panel and agent updates

Open **Updates** in the GUI. No GitHub account, application registration, repository webhook or webhook secret is required for release updates.

- **Check now** fetches the latest stable public Līdza Deploy release and shows installed control-panel and agent versions.
- **Update control panel & agents** queues a durable batch using the installed CLI updater on each connected server.
- **Automatically install stable releases** opts this installation into hourly release checks and installation. It is disabled by default. Checks and queued work continue when the browser is closed.

Remote agents update first, one at a time. The colocated agent on the control-panel host updates last; its helper replaces both the agent and GUI binaries. The browser can briefly lose connection while the GUI restarts. Reload Updates after it reconnects. An installation with a remote-only GUI must also connect the local agent on its GUI host to update that host.

The root helper downloads assets and checksums from the fixed official release repository, verifies the archive and bundled checksums, restarts services and checks readiness. Failed startup restores previous binaries. Hosted application containers continue running. OAuth, operator accounts, database data and application configuration are preserved.

## Browser assets after an update

The embedded interface uses Līdza's released `devserver.Static` handler. HTML and
unhashed JavaScript/CSS use `Cache-Control: no-cache` and content ETags. Reloading
revalidates each file: an unchanged file can return 304, while a changed file
returns its new bytes. Cache invalidation does not clear cookies or browser
storage, and does not disable OAuth or discard settings.

The regular workspace status response includes the running control-panel version.
After a version change in an open tab, a **Reload interface** notice appears.
Reload keeps the current URL; unsaved edits require confirmation before they are
discarded. The interface does not automatically reload or redraw forms during an
update. A tab loaded before this behavior was installed needs an initial reload.
Static responses include `X-Lidza-Deploy-Version` for diagnosing the serving binary.

A fresh response containing older code can also mean the control-panel service
has not updated. Check **Updates → Control panel**, rather than relying on the
agent version alone. A hard refresh cannot install a newer server binary.

The optional framework head-hook metadata integration is paused pending
[Līdza #54](https://github.com/agim/lidza/issues/54), because direct HTML files skip
the hook in v0.1.107. The cache policy and release-change notice do not depend on it.

A busy or temporarily unreachable server waits and is retried by the framework's durable minute schedule. Unsupported installers and failed upgrades stop the batch before updating remaining servers. Fix that host, then click Update again. Automatic mode does not repeatedly install a failed release; a manual retry or a newer release is required.

Install this release once with the current installer on the GUI host and each hosting server to provision the latest CLI helper. Subsequent updates refresh the verified helper too. Existing **Servers → Agent upgrade** remains available for updating individual hosts.

## Removal of the former self-update webhook

The `/hooks/self-update` endpoint and `LIDZA_SELF_UPDATE_SECRET` / `LIDZA_SELF_UPDATE_SERVER` settings have been removed. The current installer and CLI helper delete these two retired environment entries; they leave OAuth and hosted-app webhook secrets intact.

If you manually created a repository hook whose URL ends in `/hooks/self-update`, delete that hook in GitHub repository settings. App deployment hooks (`/hooks/github/...` and `/hooks/github-app`) continue to work and must be kept.

## Automatic swap checks

The installer and updated hosting agents check swap automatically. Existing active swap is preserved; configured inactive swap is activated. If none exists, Līdza Deploy creates a 4 GiB, root-owned swap file at `/var/lib/lidza-deploy.swap` and registers it in `/etc/fstab`. Provisioning requires ext4 or XFS and at least 6 GiB free space, leaving a 2 GiB reserve. Existing files at the reserved path are never reformatted or overwritten.

An existing agent updated through the GUI checks after its verified root helper has been refreshed, normally within one minute. Agents recheck daily. **Servers → Server health** shows total/free swap and the provisioning result. Failures are also logged in `lidza-agent-upgrade.service`. Swap checks do not require a deploy-user sudo grant. Installation staging never provisions host swap.

Swap reduces the chance of build-time out-of-memory failures, but heavy builds can still need a larger server. Disk-backed swapping can slow builds; it is not equivalent to additional RAM.
