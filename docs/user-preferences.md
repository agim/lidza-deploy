# Saved display choices

The console saves display choices to the signed-in account through the released
Līdza auth preference API (framework v0.1.129). Refreshing or signing out does not
reset a saved choice. Accounts have separate preferences, including across
browsers. Hosting agents do not store or receive these preferences.

Saved choices include application list/grid layout, application status/search,
application filters in deployments/databases, search in backups/team/audit and
diagnostics, the diagnostic application selection, log following and the settings
tab. An explicit application in the URL takes precedence over the saved filter.
Only an allowlist of display keys is accepted by the client. Search values are
bounded to 256 characters. Credentials, form values, commands, destructive choices
and service/deployment actions are never saved by this feature.

The console waits for preference hydration before displaying the workspace. It
retains edits across slow reads and failed writes, serializes partial patches and
retries on reconnection. A warning and Retry button appear when synchronization
fails. Sign-out flushes queued edits first; a preference outage cannot trap the
user in a session. A bounded per-account sessionStorage buffer retains unsaved
display edits across a fast refresh. It is read only after the server identifies
the account. The optional account expectation on every write prevents a delayed
request from writing to another account after a sign-in change.

## Rollout

The control-panel bootstrap adds the framework-owned `auth_preference` table in
its normal transaction/advisory lock. It contains one JSON document per subject
and does not change existing session, OAuth, authorization or application tables.
Back up the control database/state/keys before a dashboard update. A previous
dashboard binary can run with this extra table left in place; do not drop saved
preferences or restore an older whole database to roll the UI back.

## Verification

Framework integration tests use real PostgreSQL for account isolation, concurrent
partial updates, size limits, deletion and revoked/new sessions. Console tests
cover slow hydration, failed writes, refresh retry buffers, ordering, account
switching, inventory outage/recovery and browser reconnection. The control auth
integration test boots the actual preference DDL and verifies logout/login.
Rendered desktop/phone checks verify the selected layout/search through refresh,
logout/login and a second synthetic account, with no clipped controls.
