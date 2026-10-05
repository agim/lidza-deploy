# Application errors

Each hosting agent automatically reads its apps' Docker console output and sends error reports to the Deploy control panel. No analytics pack, app database, repository change or separate reporting command is required for server console errors. Installing/upgrading the agent and pairing it with this control panel enables the collector; pairing configuration is refreshed automatically.

Open **Errors** in the workspace or on an application card. **Agent console errors** is the default. Search messages/routes/stack traces/request IDs, filter reports, and select **Inspect error** for occurrence times, stack, container, release and commit. Repeated framework fingerprints form one group. Counts and search cover up to 500 recent reports, rather than the entire retention period. The GUI refreshes every 30 seconds and also offers Refresh errors.

## Collection and delivery

The agent checks current and rollback web containers and continuous worker containers every 15 seconds. It recognizes structured JSON ERROR/FATAL/PANIC records, structured HTTP status 500 or higher, and explicit text error/panic markers. Līdza already emits structured request and panic logs without analytics. JSON stack/route/request metadata is retained; plain Go panic continuation lines are included when present in the same read. Arbitrary prose containing the word “error” is not classified as a failure. Scheduled task output remains available in Workers & jobs and is not part of this continuous container collector.

Reports and per-container timestamp cursors are saved in the agent's existing encrypted state before transmission. Each report has a deterministic ID incorporating its app incarnation, container and log occurrence. Identical lines at the same timestamp retain separate occurrence IDs. The agent retries failed delivery, including after a restart; the control panel deduplicates retries and acknowledges only after its database transaction commits.

The collector uses the paired agent's existing server key over HTTPS (loopback HTTP for local development). Redirects are refused. The panel verifies the server and current app assignment/incarnation, so stale reports cannot be attributed to a removed-and-recreated app or another server's app. Console reports are stored centrally in the control-panel database using released framework error tables/fingerprints. They remain readable while the hosting agent is offline. Heartbeats and collection warnings distinguish stale/offline/error collection from an empty result. Admins, deployers and viewers share their existing fleet read access.

Central console reports are retained for 30 days; the scheduled collector-configuration job also prunes older deployment-owned records. The agent buffers up to 1,000 unsent reports, sending at most 20 per batch; collection pauses when this buffer is full. On first capture of a container it looks back 15 minutes. Each read is limited to 1,000 log lines and 64 KiB, and fields/responses are bounded. Reads reaching these limits produce a warning. Docker log rotation, bursts beyond a read limit, a full backlog, or a stopped agent can lose uncaptured output; this is not a guarantee of complete log archival. Keep high-volume logs in a dedicated logging service when exhaustive retention is required.

Known currently configured environment values and database passwords are masked before reports enter the agent's outbox. Structured request paths omit query strings; user IDs and arbitrary structured fields are not forwarded. The GUI escapes text. Arbitrary, encoded or formerly configured secrets in messages/stack traces/plain logs cannot be reliably recognized: applications must avoid logging them. Both agent encrypted state/master key and the control-panel database need normal backup protection. Existing readiness/deployment email alerts remain separate; console reports do not automatically send emails.

## Optional app analytics

Choose **Optional app analytics** to inspect the app's own framework error store, including frontend reports. This mode is independent of automatic console capture. It reads `analytics.Recent` through the agent from the app's configured primary PostgreSQL connection. A missing database/schema displays instructions; database/network/permission failures show an unavailable state.

Enable the pack through the framework's project setup command:

```sh
lidza install --packs analytics
```

Or use `lidza pack add analytics` in an existing project and follow its normal schema/migration workflow. Keep `db` enabled, commit the app changes and redeploy. This project setup command uses development database configuration; follow the app's production migration workflow for a hosted database. The analytics pack captures recovered panics and typed-handler 500 errors; handled failures can use `report.Capture` from `github.com/agim/lidza/pkg/report`.

For frontend reports, enable the framework reporter with `VITE_ANALYTICS=1` during the frontend build and mount its generated analytics routes. A runtime variable after frontend compilation cannot enable a build-time flag. `ANALYTICS_RETENTION` controls the app store's retention. Apps sharing a primary database can share analytics records; the GUI warns when configured connections show that database is shared. Console reports remain isolated by hosting assignment even when apps share databases.

## Validation and upgrade

Automated tests cover parsing, multiline panics, same-timestamp occurrences, replay, redaction, encrypted outbox restart/retry, scoped authentication, stale app rejection and central deduplication. `node tests/browser/errors.cjs` runs a real Docker container, agent and control panel, captures console errors with no app database or analytics, and checks grouping/search/details/mobile layout and retained reports while the agent is offline. Existing PostgreSQL analytics tests cover the optional mode.

Upgrade the control panel and hosting agents together. The existing v0.2.1 binaries do not include this collector/dashboard. No new framework release or manually copied reporting secret is required.
