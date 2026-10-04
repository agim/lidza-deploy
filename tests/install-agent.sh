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
"$installer" --stage "$scratch/stage" --with-control --fqdn deploy.example.com --hostname agent.example.com --email ops@example.com >/dev/null
cmp -s "$scratch/original.env" "$scratch/stage/etc/lidza-agent/agent.env"
[[ $(stat -c %a "$scratch/stage/etc/lidza-agent/agent.env") == 600 ]]
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
assert 'ask http://127.0.0.1:9090/tls/allow' in config
assert 'handle /.well-known/lidza-deploy-host' in config
assert 'redir https://{host}{uri} permanent' in config
assert 'handle /v1/*' in config and 'agent.example.com' in config
assert connection['token'] not in config
assert 'deploy.example.com' in config and 'reverse_proxy 127.0.0.1:3000' in config
assert 'respond @private 404' in config
assert 'CONTROL_SETUP_ORIGIN=https://deploy.example.com' in (root/'etc/lidza-control/control.env').read_text()
PY
if "$installer" --fqdn localhost --stage "$scratch/bad" --hostname 'bad.example.com;id' > /dev/null 2>&1; then echo 'Invalid hostname accepted' >&2;exit 1; docker run --rm -v "$scratch/stage/etc/caddy/Caddyfile:/etc/caddy/Caddyfile:ro" caddy:2.10.2-alpine caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile > "$scratch/caddy.json" 2> "$scratch/adapt.log"
 python3 - "$scratch/caddy.json" <<'PYJSON'
import json,sys
config=json.load(open(sys.argv[1]))
policies=config['apps']['tls']['automation']['policies']
assert any('deploy.example.com' in p.get('subjects',[]) and not p.get('on_demand',False) for p in policies)
assert any(p.get('on_demand',False) for p in policies)
PYJSON
fi
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
printf '%s\n' 'PASS: installer staging, private credentials, repeatability, checksum rejection, host validation, and Caddy configuration'
