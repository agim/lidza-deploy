# Credentials, two-factor verification and root console

Application environment values are classified as either **Setting** or
**Credential**. Names containing `KEY`, `SECRET`, `TOKEN`, `PASSWORD`, `PASS`,
`CREDENTIAL` or `PRIVATE` are credentials by default. URL variables containing
an embedded password are credentials too. An administrator can correct the
classification in Application settings, except `LIDZA_MASTER_KEY`, which always remains a credential.

Settings are returned to the browser so they remain directly editable.
Credentials are not returned by the settings API: the browser receives only
the first four characters and character count. **Reveal** requires a Līdza
authenticator factor and a fresh grant bound to the current account, session,
application and variable. The grant lasts 15 minutes. Reveal requests and
results are written to the audit log without the value.

Manage the authenticator and single-use recovery codes from **Account →
Two-factor authentication**. After enrollment, password sign-in also requires
an authenticator or recovery code.

Administrators can open **Servers → server → Root console**. It is disabled
until the current account enrolls two-factor authentication. Verification
unlocks only the selected server for 15 minutes; **Extend access** starts a new
15-minute window. Every command request and result is audited by actor and
server, while command text and output are deliberately excluded from audit
metadata. The paired agent must run as root. Commands have a two-minute timeout,
64 KiB input limit and 1 MiB combined output limit.

The console accepts complete POSIX shell programs, including heredocs:

```sh
docker exec -i lidza-db-example sh -c \
  'exec psql -X -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT status, count(*) FROM mail_message GROUP BY status;
SQL
```

Root access is inherently destructive. Prefer the normal deployment,
database, backup and log controls when they cover the operation. The console
does not provide an interactive TTY, so commands that prompt for input will
wait until the two-minute timeout.

## Application master keys

New app identities get a random 256-bit `LIDZA_MASTER_KEY` from the hosting agent.
The creation wizard's **Application master key** field is optional: leave it blank
to generate, or enter the original 64-character hex key when importing sealed
credentials or encrypted data. The generated value is saved in encrypted agent
state before deployment, masked in normal API responses, redacted from logs and
reused for updates, reloads and restarts. Back up the agent state and its own
encryption key together.

Existing applications keep their keys. Missing keys on pre-existing installations
are not silently replaced: import the original through Environment settings. For an app with no existing encryption key, **Settings → Environment → Generate master key** saves a new key on the agent after confirmation. It preserves other unsaved edits and does not reload the running app; use Deploy or Reload when ready. Generation rejects an existing key atomically, including concurrent requests, and the response contains only masked metadata. A
master key cannot be cleared, and explicit replacement must match the app's data.

For a new repository, `lidza credentials init` creates `config/master.key` and
`config/credentials.yml.enc` when missing; it uses `LIDZA_MASTER_KEY` when supplied.
Use the same key in Deploy and when sealing the repository's credential file.
Initialization refuses to invent a replacement for an existing encrypted file
whose key is unavailable. Deploy's framework decryption check remains enforced.
