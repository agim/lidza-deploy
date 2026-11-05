# Signed update channel

The public build updates hosts from this project's published releases (see
[automatic control-panel updates](self-updates.md)). A distribution that
publishes its **own** builds, for example a fork with additional features, can
instead ship **signed bundles** as GitHub Releases on its own repository.

The channel is active only in a build that pins at least one Ed25519 public
key. The public source tree pins none (`internal/updates/keys.go` is empty), so
the channel is off and nothing in the public build trusts any signing key. A
build that pins keys installs only bundles from its configured channel and
never offers the public releases, so a distribution's binaries are not
replaced by public ones.

## How a release is verified

1. **Control panel.** On *Check now* (and hourly), the panel lists releases
   of the channel repository with the GitHub App installation that already
   covers it, picks the newest published tag of the form `vX.Y.Z-private.N`
   that carries `manifest.json`, `manifest.sig` and
   `lidza-agent-linux-amd64.tar.gz`, downloads manifest and signature, and
   verifies the Ed25519 signature with the pinned keys. The manifest must
   name the same repository and version. Only then is the version shown as
   available.
2. **Transfer.** When an update is installed, the panel downloads the archive
   once, checks its SHA-256 against the signed manifest, and uploads
   manifest, signature and archive to each agent in turn
   (`POST /v1/upgrade/manifest`, `PUT /v1/upgrade/bundle/{version}`).
3. **Agent.** The agent verifies the signature with its own pinned keys,
   refuses versions that are not newer than the running build, checks the
   archive digest while receiving it, and only then queues the upgrade for
   the root helper (`POST /v1/upgrade`).
4. **Root helper.** `/usr/local/libexec/lidza-agent-upgrade` runs
   `<installed binary> -verify-bundle <staged dir>` with the currently
   installed agent (or control) binary, so the keys that decide are the ones
   already on the host. It then checks `SHA256SUMS` and the embedded
   versions, replaces only the binaries whose units exist on the host
   (`lidza-agent`, `lidza-control`), restarts them, waits for their health
   endpoints (`127.0.0.1:9090/health`, `127.0.0.1:3000/readyz`), and rolls
   back on failure.

Remote hosts are updated first, one at a time; the control panel's own host
is last. A failed host stops the batch. Hosted applications are never
touched. Unit files are not replaced by bundle upgrades; unit changes remain
reviewed host changes.

## One-time setup for a distribution

1. **Create the signing key** (on a workstation, never on a host):

   ```sh
   go run ./cmd/bundle-sign keygen -out lidza-bundle-signing.key
   ```

   The command prints the public key and its id. Store the private key in a
   password manager or hardware-backed secret store.
2. **Pin the public key**, either by adding the printed hex key to
   `TrustedPublicKeys` in your distribution's `internal/updates/keys.go`, or
   without a source change at build time:

   ```sh
   go build -ldflags "-X github.com/agim/lidza-deploy/internal/updates.BuildTrustedKeys=<hex>[,<hex>]" ./cmd/agent ./cmd/web
   ```

   `scripts/bundle-build.sh` passes `LIDZA_BUNDLE_TRUSTED_KEYS` this way.
   Ship a build that trusts the key to every host once (installer `--bundle`
   or `--source`). A host whose installed binaries trust no key refuses
   every bundle.
3. **Bind the channel** on the Updates page (installation owner only): enter
   the repository, for example `owner/deploy-distribution`. The GitHub App
   must be installed on that repository.

## Cutting a release

```sh
LIDZA_BUNDLE_SIGNING_KEY_FILE=lidza-bundle-signing.key \
LIDZA_BUNDLE_TRUSTED_KEYS=<hex> \
scripts/bundle-build.sh v0.3.0-private.1
```

The script builds both binaries from a clean checkout, writes and signs the
manifest, and verifies the signature against the trust set compiled into the
same build, so a release signed with a key the binaries do not trust fails
here rather than on a host. Publish `dist/signed/*` as a GitHub Release with
the same tag on the channel repository. `LIDZA_BUNDLE_SIGNING_KEY` (PEM
contents) can replace the key file in CI.

Version rules: channel versions are semantic versions with the `private.N`
pre-release identifier. Agents refuse anything that is not strictly newer
than their running version, so a distribution line must be numbered above
the public version it was forked from (`v0.3.0-private.1` after `v0.2.105`).

## Key rotation

1. Generate a new key; trust it next to the old one; release that build
   signed with the **old** key.
2. Once every host runs it, sign later releases with the new key.
3. Remove the old public key in a following release.

If a private key is compromised, remove its public key immediately and ship
that build manually; the panel then refuses every bundle signed with it.

## Rolling back a bad agent

The helper keeps `<binary>.previous` beside each replaced binary and restores
it automatically when health checks fail. To roll back after a successful
install, publish a new release built from the earlier commit with a higher
version number; downgrades by version are refused by design. For an emergency
on a single host, as root:

```sh
install -m 0755 /usr/local/bin/lidza-agent.previous /usr/local/bin/lidza-agent
systemctl restart lidza-agent
```

## API

- `GET /api/control/updates` adds `channel` (`configured`, `repository`,
  `installation`, `keys`, `keys_pinned`, `bundle_version`, `key_id`).
- `POST /api/control/updates/channel {"repository": "owner/name"}` binds the
  channel (empty repository clears it). Owner only.
- Agent: `GET /v1/upgrade` reports `channel`, `arch` and `keys`;
  `POST /v1/upgrade/manifest`, `PUT /v1/upgrade/bundle/{version}` stage a
  bundle; `POST /v1/upgrade {"version": ...}` queues it after verification.
- CLI: `lidza-agent -verify-bundle <dir>` and `lidza-control -verify-bundle
  <dir>` verify a staged bundle with the pinned keys and print its version.
