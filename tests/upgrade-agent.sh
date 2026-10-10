#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d /tmp/lidza-upgrade-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch"/{mock,release/bin,release/deploy,usr/local/bin,usr/local/libexec,etc/lidza-control,var/lib/lidza-agent/upgrade,var/tmp,run}
cat > "$scratch/release/bin/lidza-agent" <<'BIN'
#!/usr/bin/env bash
echo v0.2.0
BIN
cp "$scratch/release/bin/lidza-agent" "$scratch/release/bin/lidza-control"
chmod +x "$scratch/release/bin/"*
cp scripts/upgrade-agent.sh "$scratch/release/deploy/upgrade-agent.sh"
printf "%s\n" "LIDZA_SELF_UPDATE_SECRET=retired-secret" "LIDZA_SELF_UPDATE_SERVER=local" "AUTH_CONNECT=github" > "$scratch/etc/lidza-control/control.env"
(cd "$scratch/release" && sha256sum bin/lidza-agent bin/lidza-control deploy/upgrade-agent.sh > SHA256SUMS)
tar -C "$scratch/release" -czf "$scratch/lidza-agent-linux-amd64.tar.gz" .
(cd "$scratch" && sha256sum lidza-agent-linux-amd64.tar.gz > checksum)
cat > "$scratch/mock/curl" <<'MOCK'
#!/usr/bin/env bash
set -eu
output='';url=''
while (($#));do case "$1" in -o) output=$2;shift 2;;http*)url=$1;shift;;--max-time|--connect-timeout|--proto|--proto-redir|--tlsv1.2)if [[ "$1" == --tlsv1.2 ]];then shift;else shift 2;fi;;*)shift;;esac;done
case "$url" in
 */lidza-agent-linux-amd64.tar.gz.sha256)cp "$UPGRADE_FIXTURE/checksum" "$output";;
 */lidza-agent-linux-amd64.tar.gz)cp "$UPGRADE_FIXTURE/lidza-agent-linux-amd64.tar.gz" "$output";;
 http://127.0.0.1:9090/health|http://127.0.0.1:3000/readyz)[[ ! -e "$UPGRADE_FIXTURE/bad-health" ]];;
 *)exit 1;;esac
MOCK
cat > "$scratch/mock/systemctl" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$UPGRADE_FIXTURE/service-actions"
exit 0
MOCK
for tool in chown sleep;do printf '#!/usr/bin/env bash\nexit 0\n' > "$scratch/mock/$tool";done
chmod +x "$scratch/mock/"*
python3 - "$scratch" <<'PY'
import pathlib,sys
root=pathlib.Path(sys.argv[1]);s=pathlib.Path('scripts/upgrade-agent.sh').read_text()
for p in ['/var/lib/lidza-agent','/usr/local/bin','/usr/local/libexec','/etc/lidza-control','/var/tmp','/run/lidza-agent-upgrade.lock']:
 s=s.replace(p,str(root)+p)
(root/'helper').write_text(s);(root/'helper').chmod(0o755)
PY
export UPGRADE_FIXTURE="$scratch"
export PATH="$scratch/mock:$PATH"
old(){ printf '#!/usr/bin/env bash\necho old-version\n' > "$scratch/usr/local/bin/lidza-agent";cp "$scratch/usr/local/bin/lidza-agent" "$scratch/usr/local/bin/lidza-control";chmod +x "$scratch/usr/local/bin/"*;printf 'v0.2.0\n' > "$scratch/var/lib/lidza-agent/upgrade/request";}
old
"$scratch/helper"
[[ $("$scratch/usr/local/bin/lidza-agent" -version) == v0.2.0 ]]
! rg -q "^LIDZA_SELF_UPDATE_" "$scratch/etc/lidza-control/control.env"
rg -q "^AUTH_CONNECT=github$" "$scratch/etc/lidza-control/control.env"
cmp scripts/upgrade-agent.sh "$scratch/usr/local/libexec/lidza-agent-upgrade"
rg -q 'succeeded' "$scratch/var/lib/lidza-agent/upgrade/status.json"
old
touch "$scratch/bad-health"
if "$scratch/helper";then echo 'Unhealthy upgrade was accepted' >&2;exit 1;fi
[[ $("$scratch/usr/local/bin/lidza-agent" -version) == old-version ]]
[[ $("$scratch/usr/local/bin/lidza-control") == old-version ]]
rg -q 'previous managed binaries restored' "$scratch/var/lib/lidza-agent/upgrade/status.json"
rm "$scratch/bad-health"
old
printf '%064d  lidza-agent-linux-amd64.tar.gz\n' 0 > "$scratch/checksum"
if "$scratch/helper";then echo 'Bad checksum accepted' >&2;exit 1;fi
[[ $("$scratch/usr/local/bin/lidza-agent") == old-version ]]
printf '../bad\n' > "$scratch/var/lib/lidza-agent/upgrade/request"
if "$scratch/helper";then echo 'Unsafe version accepted' >&2;exit 1;fi
[[ $("$scratch/usr/local/bin/lidza-agent") == old-version ]]
echo 'PASS: official release validation, healthy upgrade, failed-health rollback, checksum rejection and invalid-version rejection in isolated fixtures'

# SSH changes dispatch only to the fixed privileged operation.
cat > "$scratch/usr/local/bin/lidza-agent" <<'BIN'
#!/usr/bin/env bash
[[ "$*" == -apply-ssh-keys ]] || exit 1
printf applied > "$UPGRADE_FIXTURE/ssh-applied"
BIN
chmod +x "$scratch/usr/local/bin/lidza-agent"
printf 'ssh-keys\n' > "$scratch/var/lib/lidza-agent/upgrade/request"
"$scratch/helper"
[[ $(cat "$scratch/ssh-applied") == applied ]]
echo 'PASS: fixed SSH helper dispatch'
