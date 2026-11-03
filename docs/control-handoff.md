# Moving the existing control instance

Build `./cmd/control-handoff` to review and stage the retained control directory.
The tool is offline by default. It never starts/stops a service, changes DNS,
alters the source directory, dumps/restores a database or activates the output.

Create a private mapping file naming **every** retained server ID and its exact
old and new URLs, including unchanged remote URLs. Keep the canonical origin:

```json
{
  "origin": "https://deploy.example.com",
  "endpoints": [
    {"id": "hosting", "from": "http://127.0.0.1:19092", "to": "http://127.0.0.1:9092"}
  ]
}
```

Loopback and tunnel listeners are relative to the machine running the dashboard.
They cannot be copied blindly to another host. Keep server IDs and tokens paired
with their actual agent. Remove unneeded source registrations through the existing
server UI only after no apps depend on them. The staging tool does not delete IDs,
move app ownership or infer URLs.

```sh
control-handoff -source /absolute/retained-control -mapping /private/mapping.json
```

Review outputs a content fingerprint, counts, endpoint mappings and warnings; no
credentials, tokens or database connection values. It checks the retained key,
sealed credentials/control inventory, completed-setup marker, canonical origin,
complete mapping and app-to-server references. It refuses symlinks, special files,
oversized snapshots, invalid endpoints and master-key environment overrides.

After stopping/draining the source writer and dispatch jobs for the final snapshot,
transfer the directory privately to an offline location on the destination. Review
that snapshot again, then stage it with the resulting fingerprint:

```sh
control-handoff -source /absolute/retained-control -mapping /private/mapping.json \
  -output /absolute/new-staged-control -expected-sha256 REVIEWED_SHA256
```

Staging contacts each mapped agent using the retained token and existing HTTPS/
loopback policy. It verifies the app ID, repository and domain assignments before
writing a new private directory. Unknown and private JSON fields are preserved,
as are the exact encryption key, credentials and setup marker. Only server URLs
in the encrypted registry and optional boot seed are rewritten. Source changes
invalidate the review. Existing output paths and paths inside the source are
refused. The source is not modified even when staging fails.

The output explicitly reports `database_included: false` and
`ready_to_start: false`. Restore and verify the dashboard's database separately,
review the retained database endpoint/service environment and external file paths,
configure ownership of the new data directory and test the private build before
activation. Do not run the fresh setup wizard or generate replacement keys.

Use one active writer/dispatcher during the handoff. Dashboard maintenance can
cover the final database/state snapshot and DNS propagation. Keep the public
origin, OAuth/GitHub configuration, sessions/MFA keys, grants, audit/team records,
webhook signatures and dispatch deduplication/retry state. Hosted applications
and their database processes remain independent of this dashboard maintenance.

DNS convergence may need a reviewed old-host forwarding path or a maintenance
response. Keep the old dashboard process stopped. After the destination accepts
writes, rolling back requires a compatible **current** database/state snapshot,
reverse endpoint review and one source writer; never restart an obsolete snapshot.
