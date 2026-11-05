#!/usr/bin/env bash
# Build, sign and verify a signed distribution bundle for the Updates page.
#
#   scripts/bundle-build.sh VERSION [OUTPUT_DIR]
#
# VERSION must be a channel release version such as v0.3.0-private.1 and
# must equal the tag being released. Signing uses LIDZA_BUNDLE_SIGNING_KEY
# (PEM) or LIDZA_BUNDLE_SIGNING_KEY_FILE; without either the bundle is built
# and left unsigned so it can be inspected.
#
# The binaries trust the keys in internal/updates/keys.go plus the
# comma-separated hex public keys in LIDZA_BUNDLE_TRUSTED_KEYS, which are
# compiled in with -ldflags. The public source tree pins none, so a
# distribution must set one of the two; the manifest is verified against the
# resulting trust set.
set -euo pipefail
umask 022
cd "$(dirname "$0")/.."
version=${1:?channel release version required, for example v0.3.0-private.1}
output=${2:-dist/signed}
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-private\.[0-9]+$ ]] || { echo 'Version must look like v0.3.0-private.1' >&2;exit 2; }
[[ -z $(git status --porcelain) ]] || { echo 'Commit or stash changes before building a release bundle.' >&2;exit 1; }
repository=${LIDZA_BUNDLE_REPOSITORY:-$(git remote get-url origin | sed -E 's#^(https://github\.com/|git@github\.com:)##; s#\.git$##')}
commit=$(git rev-parse HEAD)
go=${GO:-go}
stage=$(mktemp -d /tmp/lidza-bundle.XXXXXX)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/bin" "$stage/deploy" "$output"
for target in agent:lidza-agent web:lidza-control;do
 CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$go" build -trimpath -ldflags "-X github.com/agim/lidza-deploy/internal/buildinfo.Version=$version -X github.com/agim/lidza-deploy/internal/updates.BuildTrustedKeys=${LIDZA_BUNDLE_TRUSTED_KEYS:-}" -o "$stage/bin/${target#*:}" "./cmd/${target%%:*}"
done
printf 'amd64\n' > "$stage/ARCH"
install -m 0755 scripts/upgrade-agent.sh "$stage/deploy/upgrade-agent.sh"
install -m 0644 deploy/lidza-agent-upgrade.path deploy/lidza-agent-upgrade.service deploy/lidza-agent.service deploy/lidza-control.service "$stage/deploy/"
printf '%s\n' "$commit" > "$stage/REVISION"
(cd "$stage" && sha256sum ARCH REVISION bin/lidza-agent bin/lidza-control deploy/* > SHA256SUMS && sha256sum --strict -c SHA256SUMS >/dev/null)
[[ $("$stage/bin/lidza-agent" -version) == "$version" && $("$stage/bin/lidza-control" -version) == "$version" ]]
archive=$output/lidza-agent-linux-amd64.tar.gz
tar --sort=name --owner=0 --group=0 --numeric-owner -czf "$archive" -C "$stage" .
"$go" run ./cmd/bundle-sign manifest -repository "$repository" -version "$version" -commit "$commit" -out "$output/manifest.json" "$archive"
if [[ -n ${LIDZA_BUNDLE_SIGNING_KEY_FILE:-} ]];then
 "$go" run ./cmd/bundle-sign sign -key "$LIDZA_BUNDLE_SIGNING_KEY_FILE" -manifest "$output/manifest.json" -out "$output/manifest.sig"
elif [[ -n ${LIDZA_BUNDLE_SIGNING_KEY:-} ]];then
 "$go" run ./cmd/bundle-sign sign -manifest "$output/manifest.json" -out "$output/manifest.sig"
else
 echo "Built unsigned bundle in $output (set LIDZA_BUNDLE_SIGNING_KEY to sign)."
 exit 0
fi
if [[ -n ${LIDZA_BUNDLE_PUBLIC_KEY:-} ]];then
 "$go" run ./cmd/bundle-sign verify -public-key "$LIDZA_BUNDLE_PUBLIC_KEY" -manifest "$output/manifest.json" -signature "$output/manifest.sig" "$archive"
else
 "$go" run -ldflags "-X github.com/agim/lidza-deploy/internal/updates.BuildTrustedKeys=${LIDZA_BUNDLE_TRUSTED_KEYS:-}" ./cmd/bundle-sign verify -manifest "$output/manifest.json" -signature "$output/manifest.sig" "$archive"
fi
echo "Signed bundle ready in $output: $(basename "$archive") manifest.json manifest.sig"
