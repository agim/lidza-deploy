#!/usr/bin/env bash
# The root helper installs a staged signed bundle only after the installed
# binary verifies it, touches only the managed units present on the host,
# rolls back on failed health, and refuses unverifiable or tampered bundles.
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d /tmp/lidza-signed-bundle-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
version=v0.9.0-private.3
upgrade_dir="$scratch/var/lib/lidza-agent/upgrade"
mkdir -p "$scratch"/{mock,release/bin,release/deploy,usr/local/bin,usr/local/libexec,etc/systemd/system,var/tmp,run} "$upgrade_dir"
cat > "$scratch/release/bin/lidza-agent" <<BIN
#!/usr/bin/env bash
[[ \${1:-} == -version ]] && { echo $version; exit 0; }
echo new-binary
BIN
cp "$scratch/release/bin/lidza-agent" "$scratch/release/bin/lidza-control"
chmod +x "$scratch/release/bin/"*
cp scripts/upgrade-agent.sh "$scratch/release/deploy/upgrade-agent.sh"
(cd "$scratch/release" && sha256sum bin/lidza-agent bin/lidza-control deploy/upgrade-agent.sh > SHA256SUMS)
cat > "$scratch/mock/curl" <<'MOCK'
#!/usr/bin/env bash
set -eu
url=''
for arg in "$@";do case "$arg" in http*) url=$arg;;esac;done
case "$url" in
 http://127.0.0.1:3000/readyz|http://127.0.0.1:9090/health) [[ ! -e "$UPGRADE_FIXTURE/bad-health" ]];;
 *) exit 1;;
esac
MOCK
cat > "$scratch/mock/systemctl" <<'MOCK'
#!/usr/bin/env bash
if [[ ${1:-} == cat ]];then [[ -f "$UPGRADE_FIXTURE/etc/systemd/system/$2" ]];exit $?;fi
printf '%s\n' "$*" >> "$UPGRADE_FIXTURE/service-actions"
exit 0
MOCK
for tool in chown sleep;do printf '#!/usr/bin/env bash\nexit 0\n' > "$scratch/mock/$tool";done
chmod +x "$scratch/mock/"*
python3 - "$scratch" <<'PY'
import pathlib,sys
root=pathlib.Path(sys.argv[1]);s=pathlib.Path('scripts/upgrade-agent.sh').read_text()
for p in ['/var/lib/lidza-agent','/usr/local/bin','/usr/local/libexec','/etc/systemd/system','/var/tmp','/run/lidza-agent-upgrade.lock']:
 s=s.replace(p,str(root)+p)
(root/'helper').write_text(s);(root/'helper').chmod(0o755)
PY
export UPGRADE_FIXTURE="$scratch"
export PATH="$scratch/mock:$PATH"
# The installed agent verifies bundles when the fixture allows it and reports its (old) version.
installed(){
 cat > "$scratch/usr/local/bin/lidza-agent" <<'BIN'
#!/usr/bin/env bash
case "${1:-}" in
 -verify-bundle) [[ -e "$UPGRADE_FIXTURE/predates" ]] && exit 2; [[ -e "$UPGRADE_FIXTURE/verify-ok" && -f "$2/bundle.tar.gz" ]] && { echo verified; exit 0; } || exit 1;;
 -version) echo v0.8.0;;
 *) echo old-binary;;
esac
BIN
 chmod +x "$scratch/usr/local/bin/lidza-agent"
 cp deploy/lidza-agent.service "$scratch/etc/systemd/system/lidza-agent.service"
 rm -f "$scratch/service-actions"
 printf '%s\n' "$version" > "$upgrade_dir/request"
}
stage(){
 mkdir -p "$upgrade_dir/staged/$version"
 tar -C "$scratch/release" -czf "$upgrade_dir/staged/$version/bundle.tar.gz" .
 printf '{}\n' > "$upgrade_dir/staged/$version/manifest.json";printf '{}\n' > "$upgrade_dir/staged/$version/manifest.sig"
}
# 1. Verification failure by the installed binary stops before any change.
installed;stage
if "$scratch/helper";then echo 'Unverified bundle installed' >&2;exit 1;fi
grep -q 'failed signature or digest verification' "$upgrade_dir/status.json"
[[ $("$scratch/usr/local/bin/lidza-agent") == old-binary ]]
# 2. A verified bundle installs the agent only (no control unit), updates the helper and clears staging.
touch "$scratch/verify-ok"
installed;stage
"$scratch/helper"
[[ $("$scratch/usr/local/bin/lidza-agent" -version) == "$version" ]]
[[ ! -e "$scratch/usr/local/bin/lidza-control" ]]
grep -q '^restart lidza-agent$' "$scratch/service-actions"
! grep -q 'restart lidza-control$' "$scratch/service-actions"
cmp scripts/upgrade-agent.sh "$scratch/usr/local/libexec/lidza-agent-upgrade"
grep -q "Signed bundle $version installed" "$upgrade_dir/status.json"
[[ ! -e "$upgrade_dir/staged/$version" ]]
# 3. An installed binary that predates signed bundles (exit 2) is skipped and the next verifier decides.
touch "$scratch/predates"
cat > "$scratch/usr/local/bin/lidza-control" <<'BIN'
#!/usr/bin/env bash
case "${1:-}" in -verify-bundle) [[ -f "$2/bundle.tar.gz" ]] && { echo verified; exit 0; } || exit 1;; -version) echo v0.8.0;; esac
BIN
chmod +x "$scratch/usr/local/bin/lidza-control"
installed;stage
"$scratch/helper"
grep -q "Signed bundle $version installed" "$upgrade_dir/status.json"
[[ $("$scratch/usr/local/bin/lidza-agent" -version) == "$version" ]]
rm -f "$scratch/predates" "$scratch/usr/local/bin/lidza-control"
# 4. Failed health rolls the binary back.
installed;stage
touch "$scratch/bad-health"
if "$scratch/helper";then echo 'Unhealthy upgrade accepted' >&2;exit 1;fi
[[ $("$scratch/usr/local/bin/lidza-agent") == old-binary ]]
grep -q 'previous managed binaries restored' "$upgrade_dir/status.json"
rm "$scratch/bad-health"
# 5. A bundle whose archive digests do not match SHA256SUMS is refused.
installed
printf 'tampered\n' >> "$scratch/release/bin/lidza-control"
stage
if "$scratch/helper";then echo 'Tampered archive installed' >&2;exit 1;fi
[[ $("$scratch/usr/local/bin/lidza-agent") == old-binary ]]
echo 'PASS: signed bundle helper verifies with the installed binary, installs only present units, rolls back on failed health, and refuses tampered bundles'
