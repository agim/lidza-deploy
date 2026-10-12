# Create an application

Open **Applications → New application**. Creation opens a full page at
`/console.html#new-application`, with normal browser Back/Forward navigation.

1. Choose an authorized GitHub repository or enter a public `owner/repository`.
   Load and pick its deployment branch; do not assume every repository uses `main`.
   Enter a unique application ID.
   IDs from removed applications can be reused. Recreation starts fresh; retained
   data stays separate in **Databases & backups**.
   Optionally check **Deploy automatically on branch pushes**. This requires a
   connected GitHub App with access to the repository and a public HTTPS control
   panel. Only pushes to the selected branch trigger deployments; the shared
   GitHub App webhook handles delivery without another repository webhook.
2. Choose the hosting server and hostname. Point DNS to that server for automatic
   HTTPS. Select local PostgreSQL, an existing managed PostgreSQL connection, or
   no database. Configure backup frequency and retention when using a database.
3. Add app-specific environment variables, including an initial admin allowlist
   if the app requires one. Application defaults are copied on first deployment;
   app-specific values take precedence. Use each variable name once across the
   editor and JSON import. Required app-specific secrets remain your responsibility.

**Create application** saves configuration and starts database provisioning where
selected. Wait for the database to be ready, then choose **Deploy** for the first
release. Selecting auto-deploy does not deploy immediately when you create the
app. You can also enable or disable auto-deploy later from the app's actions.

Live refreshes preserve entries, focus and scroll position. Leaving with unsaved
changes prompts before discarding them. Creation errors remain visible on the
page so you can correct the input and retry.

Initial loading and page changes show a loading overlay until dynamic values
arrive. Background refreshes keep the current page visible and retain focus,
entries and scroll position. Request failures dismiss the overlay and display
the error.
