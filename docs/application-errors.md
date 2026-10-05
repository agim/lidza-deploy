# Application errors

Open **Errors** in the workspace, or **Errors** on an application card. Choose an application, search messages/routes/stack traces/request IDs, filter server or frontend reports, and select **Inspect error** to see the most recent stack and occurrence times.

The dashboard reuses the released Līdza `analytics.Recent` API and stored fingerprints. It reads up to 500 recent errors from the selected app's configured primary PostgreSQL database. Repeated fingerprints form one group; counts and search cover that sample, not the entire retention period. It refreshes every 30 seconds while open, with a manual refresh button. Responses and long fields are bounded; a size-capped response is labeled.

## Enable capture in a hosted app

In the app repository:

```sh
lidza pack add analytics
```

Keep the `db` pack enabled, apply the generated schema/migrations using the app's normal migration workflow, commit and redeploy. Attach local or external PostgreSQL as `DATABASE_URL` through the deployment GUI. The platform does not edit or push changes to your repository or create framework tables behind your app's migration system.

The analytics pack captures recovered server panics and typed-handler 500 errors. To record other handled failures explicitly, use the existing framework API:

```go
report.Capture(ctx, report.Error{
    Source: "server",
    Message: "checkout failed",
    Route: "POST /checkout",
})
```

Import `github.com/agim/lidza/pkg/report`. Ordinary `slog.Error` output remains available in runtime Logs; it is not automatically a stored analytics report.

For frontend reports, enable the framework's frontend reporter with `VITE_ANALYTICS=1` during the frontend build and mount its generated analytics routes. Setting a runtime variable after the frontend is compiled cannot enable a build-time flag. The dashboard can read existing reports even when the web container is stopped. The framework's `ANALYTICS_RETENTION` setting controls database retention; this dashboard does not delete reports or install a second error collector.

## Access and database scope

The control panel authenticates the team member and delegates to the application's assigned agent using the existing server key. Admins, deployers and viewers can read errors in their shared fleet. The browser receives no database URL, password or SQL control. Reads use the app's own database credentials, a read-only connection, one connection per request, a statement timeout and a bounded operation deadline. Managed local PostgreSQL is reached on its private Docker bridge; no database port is published. External databases must be reachable by the hosting agent, with the connection's normal TLS settings.

Known currently configured environment values and database passwords are masked in messages/stacks/routes/request IDs. Full URLs and user identifiers are omitted. Arbitrary, encoded or previously configured secrets cannot be reliably recognized; app code must avoid placing credentials or personal data in reports. Output is escaped as text in the GUI.

Framework error records belong to a database and do not identify the hosting app. Apps sharing a primary database can share its error store; the GUI warns when configured connections show that database is shared. Separate primary databases provide separate stores. Aliases or externally shared databases the platform cannot recognize may also contain other writers' reports. Switching the primary connection changes the store shown; it does not migrate old reports.

Missing database/analytics schema displays setup instructions. Database/network/permission/schema failures show an unavailable state, rather than a successful empty result. An empty store is not proof that reporting is enabled or the app is healthy. Existing readiness/deployment email alerts remain separate; captured errors do not automatically send emails.

## Validation

`TEST_DOCKER=1 go test -race ./internal/agent` covers the released framework reporter writing real PostgreSQL errors, agent reads/redaction, separate and shared assignments, response limits, incompatible schema handling, and authenticated access. The control integration checks owner/viewer access and anonymous rejection. `node tests/browser/errors.cjs` provisions an isolated real database/agent, seeds the framework schema, exercises grouping/search/source filters/details, checks mobile layout and database outages, and cleans up its resources.

Upgrade the control panel and agents together to obtain the new read endpoint. Existing v0.2.1 binaries do not include this dashboard.
