#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
scratch=$(mktemp -d /tmp/lidza-root-installer-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
sh -n install.sh
if sh install.sh --stage "$scratch/missing-fqdn" > "$scratch/missing.log" 2>&1;then echo 'Missing FQDN accepted' >&2;exit 1;fi
rg -q -- '--fqdn localhost' "$scratch/missing.log"
[[ ! -e "$scratch/missing-fqdn" ]]
for bad in https://deploy.example.com deploy.example.com:443 127.0.0.1 bare 'bad.example.com;id' 'bad..example.com' '-bad.example.com';do
 if sh install.sh --fqdn "$bad" --stage "$scratch/invalid-fqdn" >/dev/null 2>&1;then echo 'Invalid FQDN accepted' >&2;exit 1;fi
done
sh install.sh --help > "$scratch/help"
sh install.sh --fqdn localhost --plan > "$scratch/plan"
sh install.sh --check > "$scratch/check"
sh install.sh --fqdn localhost --stage "$scratch/source-stage" > "$scratch/source.log"
[[ -x "$scratch/source-stage/usr/local/bin/lidza-agent" ]]
[[ -x "$scratch/source-stage/usr/local/bin/lidza-control" ]]
[[ -f "$scratch/source-stage/var/lib/lidza-control/servers.json" ]]
cp "$scratch/source-stage/etc/lidza-agent/agent.env" "$scratch/agent.env"
sh install.sh --fqdn localhost --bundle "$PWD/dist/lidza-agent-linux-amd64.tar.gz" --stage "$scratch/source-stage" > "$scratch/repeat.log"
cmp "$scratch/agent.env" "$scratch/source-stage/etc/lidza-agent/agent.env"
sh install.sh --fqdn localhost --bundle "$PWD/dist/lidza-agent-linux-amd64.tar.gz" --agent-only --fqdn agent.example.com --email ops@example.com --stage "$scratch/agent-stage" > "$scratch/agent.log"
[[ -x "$scratch/agent-stage/usr/local/bin/lidza-agent" ]]
[[ ! -e "$scratch/agent-stage/usr/local/bin/lidza-control" ]]
# Exercise the curl|sh path against a local copy of the exact application source.
tar -czf "$scratch/source.tar.gz" --transform 's,^,lidza-deploy-main/,' cmd internal web deploy scripts go.mod go.sum
mkdir "$scratch/mockbin"
cat > "$scratch/mockbin/curl" <<'SH'
#!/bin/sh
set -eu
output='';url=''
while [ "$#" -gt 0 ];do
 case "$1" in -o) output=$2;shift 2;; https://*) url=$1;shift;; *) shift;; esac
done
case "$url" in https://codeload.github.com/agim/lidza-deploy/tar.gz/main) cp "$INSTALL_TEST_SOURCE" "$output";; https://go.dev/dl/go1.27.1.linux-*.tar.gz) printf 'invalid Go archive\n' > "$output";; *) exit 1;; esac
SH
chmod +x "$scratch/mockbin/curl"
export INSTALL_TEST_SOURCE="$scratch/source.tar.gz"
cat install.sh | PATH="$scratch/mockbin:$PATH" sh -s -- --fqdn deploy.example.com --stage "$scratch/download-stage" > "$scratch/download.log"
[[ -x "$scratch/download-stage/usr/local/bin/lidza-control" ]]
rg -q '^CONTROL_SETUP_ORIGIN=https://deploy.example.com$' "$scratch/download-stage/etc/lidza-control/control.env"
rg -q 'deploy.example.com' "$scratch/download-stage/etc/caddy/Caddyfile"
if sh install.sh --fqdn other.example.com --bundle "$PWD/dist/lidza-agent-linux-amd64.tar.gz" --stage "$scratch/download-stage" >/dev/null 2>&1;then echo 'Implicit hostname migration accepted' >&2;exit 1;fi
rg -q '^CONTROL_SETUP_ORIGIN=http://localhost:3000$' "$scratch/source-stage/etc/lidza-control/control.env"
if rg -q 'reverse_proxy 127.0.0.1:3000' "$scratch/source-stage/etc/caddy/Caddyfile";then echo 'Local GUI exposed in Caddy' >&2;exit 1;fi
cat > "$scratch/mockbin/go" <<'SH'
#!/bin/sh
printf '%s\n' go1.24.0
SH
chmod +x "$scratch/mockbin/go"
if PATH="$scratch/mockbin:$PATH" sh install.sh --fqdn localhost --stage "$scratch/go-bad-stage" > "$scratch/go-bad.log" 2>&1;then echo 'Unverified Go accepted' >&2;exit 1;fi
rg -q 'checksum verification failed' "$scratch/go-bad.log"
[[ ! -e "$scratch/go-bad-stage" ]]

cp dist/lidza-agent-linux-amd64.tar.gz "$scratch/bad.tar.gz"
cp dist/lidza-agent-linux-amd64.tar.gz.sha256 "$scratch/bad.tar.gz.sha256"
printf 'tampered\n' >> "$scratch/bad.tar.gz"
if sh install.sh --fqdn localhost --bundle "$scratch/bad.tar.gz" --stage "$scratch/bad-stage" > /dev/null 2>&1;then echo 'Tampered archive accepted' >&2;exit 1;fi
[[ ! -e "$scratch/bad-stage" ]]
if sh install.sh --fqdn localhost --version '../bad' --stage "$scratch/bad-stage" >/dev/null 2>&1;then echo 'Invalid source ref accepted' >&2;exit 1;fi
printf '%s\n' 'PASS: root installer, source build, curl|sh path, control/agent modes, repeated install, archive/Go checksum rejection, and ref validation'
