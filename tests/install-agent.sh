#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
scratch=$(mktemp -d /tmp/lidza-installer-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
mkdir "$scratch/bundle"
tar -xzf dist/lidza-agent-linux-amd64.tar.gz -C "$scratch/bundle"
installer="$scratch/bundle/install-agent.sh"
if "$installer" --with-control --stage "$scratch/missing-fqdn" >/dev/null 2>&1;then echo 'Bundled installer accepted missing FQDN' >&2;exit 1;fi
[[ ! -e "$scratch/missing-fqdn" ]]
"$installer" --fqdn localhost --plan > "$scratch/plan"
"$installer" --stage "$scratch/stage" --with-control --fqdn deploy.example.com --hostname agent.example.com --email ops@example.com >/dev/null
cp "$scratch/stage/etc/lidza-agent/agent.env" "$scratch/original.env"
printf "%s\n" "LIDZA_SELF_UPDATE_SECRET=retired-secret" "LIDZA_SELF_UPDATE_SERVER=local" "AUTH_CONNECT=github" >> "$scratch/stage/etc/lidza-control/control.env"
"$installer" --stage "$scratch/stage" --with-control --fqdn deploy.example.com --hostname agent.example.com --email ops@example.com >/dev/null
cmp -s "$scratch/original.env" "$scratch/stage/etc/lidza-agent/agent.env"
! rg -q "^LIDZA_SELF_UPDATE_" "$scratch/stage/etc/lidza-control/control.env"
rg -q "^AUTH_CONNECT=github$" "$scratch/stage/etc/lidza-control/control.env"
[[ $(stat -c %a "$scratch/stage/etc/lidza-agent/agent.env") == 600 ]]
# Installer-prepared hosts are container hosts: the agent manages Docker's daemon settings there.
[[ -f "$scratch/stage/etc/lidza-agent/docker-managed" ]]
# Public installs ship a certificate retry for installer-managed hostnames.
[[ "$(cat "$scratch/stage/etc/lidza-agent/tls-hostnames")" == $'agent.example.com\ndeploy.example.com' ]]
[[ -x "$scratch/stage/usr/local/libexec/lidza-tls-retry" && -f "$scratch/stage/etc/systemd/system/lidza-tls-retry.timer" && -f "$scratch/stage/etc/systemd/system/lidza-tls-retry.service" ]]
sh -n "$scratch/stage/usr/local/libexec/lidza-tls-retry"
mkdir -p "$scratch/retry-bin" "$scratch/retry-etc"
sed "s#/etc/lidza-agent#$scratch/retry-etc#;s#/var/lib/lidza-agent#$scratch/retry-var#" "$scratch/stage/usr/local/libexec/lidza-tls-retry" > "$scratch/tls-retry"
printf 'agent.example.com\n' > "$scratch/retry-etc/tls-hostnames"
cat > "$scratch/retry-bin/curl" <<'SH'
#!/bin/sh
# TLS probe: trusted only when RETRY_TRUSTED=1. Admin API: return and accept a configuration.
out='';post=0;prev=''
for arg in "$@";do [ "$prev" != -o ] || out=$arg;[ "$arg" != POST ] || post=1;prev=$arg;done
case "$*" in
 *--resolve*) [ "${RETRY_TRUSTED:-0}" = 1 ];exit;;
 *2019/load*) [ "$post" = 1 ] || exit 1;case "$*" in *'Cache-Control: must-revalidate'*) echo load >> "$RETRY_LOG";; *) exit 1;; esac;;
 *2019/config/*) printf '{"apps":{}}' > "$out";;
esac
SH
printf '#!/bin/sh\n[ -z "${RETRY_DNS:-}" ] || printf "%%s STREAM x\\n" $RETRY_DNS\n' > "$scratch/retry-bin/getent"
printf '#!/bin/sh\necho "$RETRY_NOW"\n' > "$scratch/retry-bin/date"
chmod +x "$scratch/retry-bin/"*
export RETRY_LOG="$scratch/retry-log";: > "$RETRY_LOG"
retry() { env "$@" PATH="$scratch/retry-bin:$PATH" sh "$scratch/tls-retry" >/dev/null; }
loads() { wc -l < "$RETRY_LOG"; }
# Unresolved hostname: nothing to retry.
retry RETRY_NOW=1000;[[ $(loads) == 0 ]]
# The hostname starts resolving: Caddy is reloaded immediately, with its running configuration.
retry RETRY_NOW=1100 RETRY_DNS=203.0.113.7;[[ $(loads) == 1 ]]
# Unchanged answer: no reload every run, at most one per 15 minutes.
retry RETRY_NOW=1220 RETRY_DNS=203.0.113.7;[[ $(loads) == 1 ]]
retry RETRY_NOW=2000 RETRY_DNS=203.0.113.7;[[ $(loads) == 2 ]]
# A corrected DNS record retries at once.
retry RETRY_NOW=2100 RETRY_DNS=203.0.113.8;[[ $(loads) == 3 ]]
# Six hours after the last DNS change, Caddy's own schedule takes over.
retry RETRY_NOW=30000 RETRY_DNS=203.0.113.8;[[ $(loads) == 3 ]]
# A trusted certificate ends retries and clears state.
retry RETRY_NOW=30100 RETRY_DNS=203.0.113.8 RETRY_TRUSTED=1;[[ $(loads) == 3 && ! -e "$scratch/retry-var/tls-retry/agent.example.com" ]]
[[ $(stat -c %a "$scratch/stage/etc/lidza-agent/connection.json") == 600 ]]
[[ -x "$scratch/stage/usr/local/bin/lidza-agent" ]]
python3 - "$scratch/stage" <<'PY'
import json,pathlib,sys
root=pathlib.Path(sys.argv[1]);connection=json.loads((root/'etc/lidza-agent/connection.json').read_text())
assert connection['url']=='https://agent.example.com'
assert len(connection['token'])==64
assert connection['id'].startswith('agent-') and len(connection['id'])<48
assert (root/'usr/local/bin/lidza-control').is_file()
assert (root/'etc/lidza-control/control.env').stat().st_mode & 0o777 == 0o600
paired=json.loads((root/'var/lib/lidza-control/servers.json').read_text())
assert paired[0]['token']==connection['token']
assert '--resume' in (root/'etc/systemd/system/caddy.service.d/10-lidza-resume.conf').read_text()
config=(root/'etc/caddy/Caddyfile').read_text()
assert (root/'usr/local/libexec/lidza-agent-upgrade').is_file()
assert (root/'etc/systemd/system/lidza-agent-upgrade.path').is_file()
assert (root/'etc/systemd/system/lidza-agent-upgrade.service').is_file()
service=(root/'etc/systemd/system/lidza-agent.service').read_text()
assert 'User=root' in service and 'Group=root' in service
assert 'User=lidza-agent' not in service and 'ProtectSystem=strict' not in service
assert 'ask http://127.0.0.1:9090/tls/allow' in config
assert 'handle /.well-known/lidza-deploy-host' in config
assert 'redir https://{host}{uri} permanent' in config
assert 'handle /v1/*' in config and 'agent.example.com' in config
assert connection['token'] not in config
assert 'deploy.example.com' in config and 'reverse_proxy 127.0.0.1:3000' in config
assert 'respond @private 404' in config
assert 'CONTROL_SETUP_ORIGIN=https://deploy.example.com' in (root/'etc/lidza-control/control.env').read_text()
PY
if "$installer" --fqdn localhost --stage "$scratch/bad" --hostname 'bad.example.com;id' > /dev/null 2>&1; then
 echo 'Invalid hostname accepted' >&2
 exit 1
fi
# Exercise live-mode decisions using an isolated filesystem and fake host tools.
# No apt, systemd, Caddy or real /etc changes are made by this fixture.
mkdir -p "$scratch/host/etc/caddy" "$scratch/host/run/systemd/system" "$scratch/host-bin"
printf 'ID=ubuntu\nVERSION_ID=24.04\nVERSION_CODENAME=noble\n' > "$scratch/host/etc/os-release"
printf '# Existing user-owned Caddy\n:80 { respond "existing" }\n' > "$scratch/host/etc/caddy/Caddyfile"
cp "$scratch/host/etc/caddy/Caddyfile" "$scratch/original-caddy"
python3 - "$installer" "$scratch/host" "$scratch/live-installer" <<'PY'
import pathlib,sys
s=pathlib.Path(sys.argv[1]).read_text()
s=s.replace('"$root/usr/local/libexec/lidza-agent-upgrade" --ensure-swap', 'echo swap-check >> "$INSTALL_HOST_LOG"')
for path in ['/etc', '/var/lib', '/usr/local', '/run/systemd/system']:
 s=s.replace(path, sys.argv[2]+path)
s=s.replace('[[ $EUID -eq 0 ]]', 'true')
pathlib.Path(sys.argv[3]).write_text(s)
PY
cat > "$scratch/host-bin/mock" <<'SH'
#!/bin/sh
set -eu
name=${0##*/}
printf '%s %s\n' "$name" "$*" >> "$INSTALL_HOST_LOG"
case "$name" in
 ss) case "$*" in *':80 or sport = :443'*) [ "${INSTALL_TEST_FREE_WEB:-0}" = 1 ] || printf 'LISTEN 0 128 *:80 *:*\n';; *':3000'*) if [ "${INSTALL_TEST_BUSY_CONTROL:-0}" = 1 ];then printf 'LISTEN 0 128 127.0.0.1:3000 *:*\n';fi;; esac;;
 dpkg) printf 'amd64\n';;
 caddy) [ "${INSTALL_TEST_FREE_WEB:-0}" = 1 ] || { echo 'Local installer called Caddy' >&2;exit 1; };;
 curl) cat >/dev/null;;
 ufw) case "$1" in status) printf 'Status: %s\n' "${INSTALL_TEST_UFW:-active}";; allow) [ "${INSTALL_TEST_UFW_FAIL:-0}" != 1 ] || exit 1;; esac;;
 firewall-cmd) exit 252;;
esac
SH
chmod +x "$scratch/host-bin/mock"
for cmd in apt-get dpkg ss docker caddy getent id usermod chown systemctl runuser curl ufw firewall-cmd;do ln -s mock "$scratch/host-bin/$cmd";done
export INSTALL_HOST_LOG="$scratch/host-actions"
# Public mode must refuse the existing Caddy before package/service changes.
if PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn deploy.example.com --with-control > "$scratch/public-conflict.log" 2>&1;then
 echo 'Public installer accepted unmanaged Caddy' >&2;exit 1
fi
rg -q 'Existing unmanaged Caddy' "$scratch/public-conflict.log"
! rg -q 'apt-get|systemctl|usermod|chown' "$scratch/host-actions"
# Also check the occupied-port refusal independently of unmanaged config.
mv "$scratch/host/etc/caddy/Caddyfile" "$scratch/saved-caddy"
if PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn deploy.example.com --with-control > "$scratch/public-port.log" 2>&1;then
 echo 'Public installer accepted occupied ports' >&2;exit 1
fi
rg -q 'Ports 80/443 are already in use' "$scratch/public-port.log"
mv "$scratch/saved-caddy" "$scratch/host/etc/caddy/Caddyfile"
# An occupied GUI port must fail before package/service changes, even locally.
: > "$scratch/host-actions"
if INSTALL_TEST_BUSY_CONTROL=1 PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn localhost --with-control > "$scratch/local-busy.log" 2>&1;then
 echo 'Local installer accepted occupied GUI port' >&2;exit 1
fi
rg -q 'Port 3000 is already in use' "$scratch/local-busy.log"
! rg -q 'apt-get|systemctl|usermod|chown' "$scratch/host-actions"
: > "$scratch/host-actions"
PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn localhost --with-control > "$scratch/local-live.log"
cmp "$scratch/original-caddy" "$scratch/host/etc/caddy/Caddyfile"
[[ ! -e "$scratch/host/etc/systemd/system/caddy.service.d" ]]
! rg -q '^caddy |^systemctl .*caddy|^apt-get .*caddy' "$scratch/host-actions"
rg -q '^systemctl restart lidza-control$' "$scratch/host-actions"
rg -q '^swap-check$' "$scratch/host-actions"
rg -q 'http://localhost:3000' "$scratch/local-live.log"
rg -q 'Automatic HTTPS for hosted apps requires a public hosting agent' "$scratch/local-live.log"
# Local mode has no installer-managed certificate to retry.
[[ ! -e "$scratch/host/etc/lidza-agent/tls-hostnames" && ! -e "$scratch/host/etc/systemd/system/lidza-tls-retry.timer" ]]
! rg -q 'lidza-tls-retry' "$scratch/host-actions"
# Local mode never touches the host firewall, even when one is active.
! rg -q '^ufw |^firewall-cmd ' "$scratch/host-actions"
# A public agent opens 80/443 only in an already active host firewall.
mv "$scratch/host/etc/caddy/Caddyfile" "$scratch/saved-caddy"
: > "$scratch/host-actions"
INSTALL_TEST_FREE_WEB=1 PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn agent.example.com > "$scratch/firewall-active.log"
for rule in 80/tcp 443/tcp 443/udp;do rg -q "^ufw allow $rule\$" "$scratch/host-actions";done
! rg -q '^ufw (enable|delete|reset|default)' "$scratch/host-actions"
rg -q 'Opened ports 80/tcp, 443/tcp and 443/udp in UFW' "$scratch/firewall-active.log"
rg -q '^systemctl enable --now lidza-tls-retry.timer$' "$scratch/host-actions"
[[ "$(cat "$scratch/host/etc/lidza-agent/tls-hostnames")" == agent.example.com ]]
: > "$scratch/host-actions"
INSTALL_TEST_FREE_WEB=1 INSTALL_TEST_UFW=inactive PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn agent.example.com > "$scratch/firewall-inactive.log"
! rg -q '^ufw (allow|enable)|^firewall-cmd .*--add' "$scratch/host-actions"
rg -q 'none was changed' "$scratch/firewall-inactive.log"
# A firewall error is reported with the manual command and does not abort a working installation.
INSTALL_TEST_FREE_WEB=1 INSTALL_TEST_UFW_FAIL=1 PATH="$scratch/host-bin:$PATH" bash "$scratch/live-installer" --bundle "$scratch/bundle" --fqdn agent.example.com > "$scratch/firewall-failed.log" 2>&1
rg -q 'Warning: could not open ports 80/443 in UFW' "$scratch/firewall-failed.log"
rg -q 'still closed in the host firewall' "$scratch/firewall-failed.log"
mv "$scratch/saved-caddy" "$scratch/host/etc/caddy/Caddyfile"
# Local + public API hostname still generates full public hosting configuration.
"$installer" --fqdn localhost --hostname agent.example.com --with-control --stage "$scratch/local-public" >/dev/null
rg -q 'agent.example.com' "$scratch/local-public/etc/caddy/Caddyfile"

printf 'tampered\n' >> "$scratch/bundle/deploy/agent.example.json"
if "$installer" --fqdn localhost --stage "$scratch/bad" > /dev/null 2>&1; then echo 'Tampered bundle accepted' >&2;exit 1;fi
if [[ ${TEST_CADDY:-0} == 1 ]]; then
 docker run --rm -v "$scratch/stage/etc/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2.10.2-alpine caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile > "$scratch/caddy.log" 2>&1 || { cat "$scratch/caddy.log";exit 1; }
 docker run --rm -v "$scratch/stage/etc/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2.10.2-alpine caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile > "$scratch/caddy.json" 2> "$scratch/adapt.log"
 python3 - "$scratch/caddy.json" <<'PYJSON'
import json,sys
config=json.load(open(sys.argv[1]))
policies=config['apps']['tls']['automation']['policies']
assert any('deploy.example.com' in p.get('subjects',[]) and not p.get('on_demand',False) for p in policies)
assert any(p.get('on_demand',False) for p in policies)
PYJSON
fi
printf '%s\n' 'PASS: installer staging, private credentials, repeatability, checksum rejection, host validation, local coexistence with occupied web ports, host firewall opening, certificate retry, and Caddy configuration'
