# Deployment logs

The agent reconciles Docker's published loopback ports at startup and every 15 seconds. This repairs saved routes when Docker assigns new ports after a container or daemon restart, without rebuilding the application. Current and rollback release bindings are persisted; missing bindings are marked unavailable rather than forwarding to a stale port.

Deployment build logs include candidate startup diagnostics when `/readyz` fails: the latest health-check error and response excerpt, Docker container state (including exit and OOM information), and the last 200 runtime log lines. These are captured before candidate cleanup and use the deployment log's configured-secret redaction. The previous healthy release remains active. Open **Deployments → Details & build logs** to copy or download the diagnostic output. Runtime logs on the Applications page refer to the retained live release, not the discarded candidate.

A successful Docker build followed by `candidate failed /readyz; previous release retained` is a startup/readiness failure. The agent requires HTTP 200 from `/readyz` within 90 seconds before switching traffic. The saved health response and candidate logs distinguish configuration/database failures, crashes, memory limits, and missing readiness routes.

Older agent versions removed the failed candidate before collecting its startup logs. Update the hosting agent and redeploy to obtain the new diagnostics; discarded container logs cannot be recovered by this change.

## Authentication secrets

Each app receives a unique, randomly generated 256-bit `AUTH_SECRET` when added. Existing apps missing it receive one on their next deployment or reload. The agent saves the secret in encrypted state before queueing work and reuses it for manual deployments, webhooks, retries, reloads, and restarts. Supplied values are preserved, including when full app updates omit the secret. Migrated apps should import their existing `AUTH_SECRET` through Settings before deployment. Values are write-only in the GUI and redacted from logs. Explicit replacement rotates signing credentials; clearing or deleting this key is rejected to prevent accidental rotation.

`LIDZA_MASTER_KEY` remains manual. This feature does not generate or replace master keys or OAuth provider credentials.

## Runtime file permissions

The agent keeps its private `0077` umask for credentials, environment files and state. Only Git subprocesses use `0022`, giving tracked source files standard Git checkout modes (directories `0755`, ordinary files `0644`, executables `0755`). This prevents owner-only build inputs being copied into images that run as UID/GID 65532. The checkout remains inside the agent's private build directory; authentication tokens are not placed in repository URLs or source files. Existing images with unreadable templates must be rebuilt with Deploy after updating the agent; Reload reuses the image. Framework generator ownership improvements are tracked in [Līdza #48](https://github.com/agim/lidza/issues/48) and await a framework release.

## Live refresh behavior

Background polling updates existing application cards, release rows, database/backup tables, server cards and update controls. Deployment log growth does not rebuild unrelated sections. Table scrollers keep their DOM identity and offsets; new releases preserve the visible release's position. Integration forms and open application menus remain in place. Refresh failures retain loaded data, and persistent notifications do not change page height.

Build logs update every two seconds while a deployment is active. Scrolling up automatically turns off **Follow latest output**; incoming output retains your reading position. Enable follow to return to the latest lines, or pause requests with **Pause updates**. Background workspace refresh does not redraw behind an open dialog.

Regression checks: `TEST_WEB_URL=http://127.0.0.1:8880 node tests/browser/refresh-stability.cjs` and `tests/browser/live-logs.cjs` against a static server for `web/static`; the former exercises changed data, inserted releases, mobile/desktop layouts, focus, menus, and database/integration scrollers. The released Līdza layout audit passes the desktop table trigger without resets or layout shift. Document-only mobile stability coverage is tracked in [Līdza #49](https://github.com/agim/lidza/issues/49); independent browser checks cover it until the framework audit is released.
