# Automatic control-panel updates

The systemd installation can update itself when a stable Līdza Deploy release is published. Repository pushes alone do not trigger updates: the release workflow tests and packages both architectures first.

Configure these values in the control panel's existing `/etc/lidza-control/control.env` file (keep its other values):

- `LIDZA_SELF_UPDATE_SECRET`: a unique secret generated with `openssl rand -hex 32`.
- `LIDZA_SELF_UPDATE_SERVER`: the ID shown in Servers for the colocated agent, typically `local`. Its URL must use loopback.

Restart `lidza-control`, then create a webhook in **agim/lidza-deploy → Settings → Webhooks**:

- Payload URL: `https://lidza-deploy.albaspot.com/hooks/self-update`
- Content type: `application/json`
- Secret: the same `LIDZA_SELF_UPDATE_SECRET`
- SSL verification: enabled
- Events: **Releases** only
- Active: enabled

GitHub's ping should return 200. Only signed `release/published` events for this repository are accepted. The agent independently checks the latest stable release. Drafts, prereleases, older versions and unrelated repositories do not queue updates. The webhook stays disabled until both settings are configured.

The existing root upgrade helper verifies official release checksums, replaces agent and control-panel binaries, restarts services, checks readiness and restores previous binaries if the update fails. OAuth and database configuration stay in the existing data directory and environment file.

Check webhook delivery responses in GitHub. If the host is busy deploying, backing up, restoring or already upgrading, the request returns 503; use **Redeliver** once it is idle. GitHub does not automatically retry failed deliveries. This endpoint does not provide unattended retry scheduling.

Creating a hook through the GitHub API requires repository webhook write permission. Connecting a GitHub App with read-only repository access is insufficient for that administration operation. Do not paste the webhook secret into issues, logs or chat.
