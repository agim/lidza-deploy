# Create an application

Open **Applications → New application**. Creation opens a full page at
`/console.html#new-application`, with normal browser Back/Forward navigation.

1. Choose an authorized GitHub repository or enter a public `owner/repository`.
   Load and pick its deployment branch; do not assume every repository uses `main`.
   Enter a unique application ID.
2. Choose the hosting server and hostname. Point DNS to that server for automatic
   HTTPS. Select local PostgreSQL, an existing managed PostgreSQL connection, or
   no database. Configure backup frequency and retention when using a database.
3. Add app-specific environment variables, including an initial admin allowlist
   if the app requires one. Application defaults are copied on first deployment;
   app-specific values take precedence. Use each variable name once across the
   editor and JSON import. Required app-specific secrets remain your responsibility.

**Create application** saves configuration and starts database provisioning where
selected. Wait for the database to be ready, then choose **Deploy**. Enable
repository auto-deploy afterward from **More actions**.

Live refreshes preserve entries, focus and scroll position. Leaving with unsaved
changes prompts before discarding them. Creation errors remain visible on the
page so you can correct the input and retry.
