# Workspace application defaults

Open **Workspace → Application defaults** as an administrator. Defaults are
copied once when an app is first queued for deployment, including a first attempt
that fails. Retries, automatic deployments, reloads and server restarts retain
that snapshot. Editing the workspace profile affects apps that have not attempted
deployment yet. Existing applications are never opted in automatically.

| Variable | Initial profile | Purpose |
| --- | --- | --- |
| `DB_MIGRATE` | `true` | Apply committed database migrations on startup. |
| `ADMIN_USERS` | Empty | Comma-separated emails or IDs for apps using the framework admin allowlist. |
| `AUTH_OWNER_CLAIM` | Empty | Set `true` to require a one-time first-account claim where the app supports it. |
| `MAIL_FROM` | Empty | Shared verified sender address. |
| `MAIL_PROVIDER` | Empty | A provider supported by each app; configure provider credentials per app. |
| `LOG_LEVEL` | `info` | Logging preference for apps that support this variable. |

Empty profile fields are omitted from the app environment. Explicit app values
take precedence, including empty strings and `false`. A workspace migration policy
can override the built-in `DB_MIGRATE=true` on a newly created app, but cannot
override an explicit app policy.

For example, save `ADMIN_USERS=agim@albaspot.com` to use the same administrator
allowlist for future apps. Each app must still authenticate that account and use
the framework allowlist; this does not grant access in unrelated role systems or
create accounts automatically. Albaspot supports this setting.

`AUTH_SECRET` is generated separately for each app. `APP_URL` follows the domain.
Database, cache and persistent-storage attachments manage their own connection
variables. `LIDZA_MASTER_KEY` remains manual per app. Shared defaults cannot
contain these secrets, connections, OAuth credentials or arbitrary variable names.

## Apply defaults to an existing app

Open **Applications → Settings → Review workspace defaults**. Select the values
to copy. Missing variables are selected initially; configured variables are not.
Existing values are write-only and are never returned by the settings API.

By default, existing variables are preserved. To replace a configured value,
select it and explicitly check **Replace selected variables that are already
configured**. Confirmation uses the exact profile revision reviewed; if another
operator changes it, reopen the review. Save any pending app-settings edits first.

A deployed app reloads automatically after the confirmed changes. An app that
has never deployed successfully saves the values for its next deployment.
Settings show **Workspace default**, **Built-in default**, or **App setting**
next to saved variable names. Explicit app edits remove the inherited origin.

Upgrade the control panel and hosting agents before using this feature.
