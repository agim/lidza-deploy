# Stack configuration requirements

Optional version-1 `configuration` in `deploy/lidza.stack.json` declares ownership
of settings. Agents predating this extension reject the unknown field: upgrade
both agent and control panel before using it. Omission preserves strict framework
production metadata checks. This extends the existing stack contract; it does not
change `lidza.json` or the framework production rules.

```json
"configuration": [
  {"name":"LIDZA_MASTER_KEY","stage":"deploy","label":"Application master key","help":"Import the original key when restoring sealed credentials.","secret":true},
  {"name":"MAIL_PROVIDER","stage":"setup","label":"Mail provider","help":"Choose a provider in /setup."},
  {"name":"MAIL_FROM","stage":"setup","label":"Sender address","help":"Set and verify the sender in /setup."},
  {"name":"LLM_PROVIDER","stage":"optional","label":"AI provider","help":"AI stays disabled until configured."},
  {"name":"REPORT_ENDPOINT","stage":"optional","label":"Reporting endpoint","required_when":{"name":"REPORTS_ENABLED","equals":"true"}}
]
```

* `deploy`: a nonblank effective production value is mandatory before building.
  Errors identify the setting, label and help and point to Settings → Environment.
* `setup`: the app owns collection, persistence and production validation. This
  requires an explicit valid `bootstrap_readiness` declaration. Missing static
  environment values are delegated to the app, including subsequent deployments
  where configuration lives in the app database. Only a first installation or
  unfinished bootstrap release can expose the guarded wizard. An established
  release must pass genuine `/readyz`200 and all service health checks; it cannot
  regain bootstrap fallback. App readiness must validate its published settings.
* `optional`: absence is allowed and the app must leave the feature disabled.
  `required_when` can make it mandatory when an effective production setting
  exactly matches `equals` (case-sensitive). Conditions read decrypted production
  credentials, manifest deploy defaults and explicit app environment; they do not
  read the application's database. The app must validate database-backed feature
  configuration at runtime. Conditions are supported only on optional settings.

`name` is an uppercase environment key; stages are required. Labels (80 chars)
and help (300 chars) are optional plain text, without control characters. At most
40 unique entries. `secret:true` forces stored environment metadata to credential;
values are never included in guidance. It does not generate a value. Use existing
`generated_secrets` for generated bootstrap secrets. Never put secret values or
secret comparisons in a manifest. Condition keys cannot be credentials or other
conditional settings; `equals` is a nonblank public literal (max80 chars).

Existing `required_environment` remains unconditional, including for a setting
listed here. A conflicting setup/optional entry is rejected, as is one for a
`generated_secrets` key. Platform infrastructure (`LIDZA_*`, `DATABASE_URL`,
`CACHE_URL`, `CLICKHOUSE_*`, `STORAGE_*`, `AUTH_SECRET`, `APP_URL`) cannot be deferred.
Master-key validation and decryption of existing sealed credentials always run;
this contract never replaces a missing original key or accepts undecryptable data.

A declaration overrides only the framework's missing-value requirement for that
exact setting. Explicit development providers (`fake`, `log`, `outbox`, etc.)
remain invalid even for setup/optional settings. Undeclared framework requirements
remain strict. Custom deploy requirements and enabled optional requirements are
also checked. App `/readyz` and sidecar health are never replaced with declarations.

For a fresh wizard-based app, retain `required_environment:["LIDZA_MASTER_KEY"]`,
declare mail fields as setup-owned and AI as optional, and keep infrastructure
provisioning and guarded bootstrap readiness. Deploy supplies no fake provider or
dummy sender. The app's production wizard configures mail; optional AI stays off.
