#!/usr/bin/env bash
# Install a verified local agent bundle on a dedicated Debian/Ubuntu host.
set -euo pipefail
umask 077
bundle=$(cd "$(dirname "$0")" && pwd)
stage='' fqdn='' hostname='' email='' plan=false with_control=false
usage() {
 cat <<'TXT'
Usage: sudo ./install-agent.sh --fqdn deploy.example.com|localhost [--with-control] [--hostname agent.example.com] [--email ops@example.com]
       ./install-agent.sh --fqdn localhost --plan
       ./install-agent.sh --fqdn deploy.example.com --stage /absolute/staging-directory [options]

Installs Docker Engine, Git, the bundled agent and systemd services.
Public mode also installs Caddy for application HTTPS.
--with-control also installs the browser-configured control panel on this host.
--fqdn is required: the GUI hostname with --with-control, otherwise the agent hostname.
Use --fqdn localhost explicitly for loopback access without taking ports 80/443.
Local mode leaves Caddy untouched; use remote public agents for automatic HTTPS.
Adding --hostname enables public hosting and requires free ports 80/443.
With --with-control, the agent API stays on loopback unless --hostname is supplied.
--hostname exposes the authenticated API over automatic HTTPS for a remote GUI.
--stage writes an inspectable filesystem tree without installing packages or services.
Requires Debian 12/13 or Ubuntu 22.04/24.04 with systemd.
Public hosting requires a dedicated server.
TXT
}
while (($#)); do
 case "$1" in
  --fqdn|--hostname|--email|--stage|--bundle) (($#>=2)) || { usage; exit 2; }; case "$1" in --fqdn) fqdn=$2;; --hostname) hostname=$2;; --email) email=$2;; --stage) stage=$2;; --bundle) bundle=$2;; esac;shift 2;;
  --plan) plan=true;shift;;
  --with-control) with_control=true;shift;;
  --help|-h) usage;exit 0;;
  *) usage;exit 2;;
 esac
done
[[ -n "$fqdn" ]] || { echo 'Missing --fqdn. Add --fqdn deploy.example.com for public HTTPS or --fqdn localhost for local/tunnel setup. No installation changes were made.' >&2;exit 2; }
if [[ "$fqdn" != localhost ]]; then
 [[ "$fqdn" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && "$fqdn" == *.* && "$fqdn" != *..* && ${#fqdn} -le 253 && ! "$fqdn" =~ ^[0-9.]+$ ]] || { echo 'Invalid --fqdn: supply a DNS hostname or localhost, without scheme, port or path.' >&2;exit 2; }
 IFS=. read -ra labels <<< "$fqdn"
 for label in "${labels[@]}";do [[ -n "$label" && ${#label} -le 63 && "$label" != -* && "$label" != *- ]] || { echo 'Invalid FQDN label' >&2;exit 2; };done
fi
if ! $with_control;then
 [[ -z "$hostname" || "$hostname" == "$fqdn" ]] || { echo 'Agent-only installation uses --fqdn; remove the conflicting --hostname.' >&2;exit 2; }
 [[ "$fqdn" == localhost ]] || hostname=$fqdn
elif [[ "$fqdn" != localhost && "$hostname" == "$fqdn" ]];then
 echo 'The GUI --fqdn and agent --hostname must be different hostnames.' >&2;exit 2
fi
public_host=false
[[ "$fqdn" == localhost && -z "$hostname" ]] || public_host=true
setup_origin=http://localhost:3000
[[ "$fqdn" == localhost ]] || setup_origin="https://$fqdn"
# Refuse an implicit hostname migration before any installation side effects.
if $with_control && [[ -f "$stage/etc/lidza-control/control.env" ]];then
 existing_origin=$(sed -n 's/^CONTROL_SETUP_ORIGIN=//p' "$stage/etc/lidza-control/control.env")
 if [[ -n "$existing_origin" && "$existing_origin" != "$setup_origin" ]];then
  echo "This control panel was installed at $existing_origin. Use the same --fqdn when reinstalling; changing its canonical hostname requires a migration." >&2;exit 2
 fi
 if [[ -z "$existing_origin" && -f "$stage/var/lib/lidza-control/setup-complete.json" ]];then
  echo 'Existing completed setup predates installer FQDN configuration. Migrate its canonical hostname explicitly before rerunning this installer.' >&2;exit 2
 fi
fi
if [[ -n "$hostname" ]]; then
 [[ "$hostname" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ && "$hostname" == *.* && "$hostname" != *..* && ${#hostname} -le 253 && ! "$hostname" =~ ^[0-9.]+$ ]] || { echo 'Invalid agent FQDN' >&2;exit 2; }
 IFS=. read -ra labels <<< "$hostname"
 for label in "${labels[@]}"; do [[ ${#label} -le 63 && "$label" != -* && "$label" != *- ]] || { echo 'Invalid DNS label' >&2;exit 2; }; done
fi
[[ -z "$email" || "$email" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]+$ ]] || { echo 'Invalid contact email' >&2;exit 2; }
[[ -z "$stage" || ( "$stage" == /* && "$stage" != / && "$stage" != *..* ) ]] || { echo 'Stage path must be an absolute non-root path without ..' >&2;exit 2; }
if $plan; then
 cat <<TXT
1. Verify the local bundle checksum and supported host.
2. Install Git and Docker Engine through signed apt repositories.
3. Preserve an existing agent token; otherwise generate a random token.
4. Install the agent, private configuration, and systemd service.
5. Public mode only: install and validate Caddy routing for application FQDNs.
   Also allow ports 80/443 in an already active UFW or firewalld host firewall.
   Install a timer that retries HTTPS certificates once the hostname resolves.
   Create the deployment account so the server also runs native applications.
6. Start services; check the authenticated agent API.
7. Save private GUI connection details in /etc/lidza-agent/connection.json.
GUI setup address: $setup_origin
Agent API hostname: ${hostname:-loopback only}
Public hosting/Caddy: $public_host (local mode leaves ports 80/443 untouched)
TXT
 exit 0
fi
[[ -f "$bundle/bin/lidza-agent" && -f "$bundle/SHA256SUMS" ]] || { echo 'Run this installer from a bundle produced by scripts/package-agent.sh.' >&2;exit 1; }
(cd "$bundle" && sha256sum --check --status SHA256SUMS) || { echo 'Bundle checksum verification failed' >&2;exit 1; }
if [[ -z "$stage" ]]; then
 [[ $EUID -eq 0 ]] || { echo 'Live installation requires sudo.' >&2;exit 1; }
 . /etc/os-release
 case "$ID:$VERSION_ID" in debian:12|debian:13|ubuntu:22.04|ubuntu:24.04) ;; *) echo 'Unsupported host; use Debian 12/13 or Ubuntu 22.04/24.04.' >&2;exit 1;; esac
 [[ $(cat "$bundle/ARCH") == "$(dpkg --print-architecture)" ]] || { echo 'Bundle architecture does not match this host.' >&2;exit 1; }
 [[ -d /run/systemd/system ]] || { echo 'A running systemd host is required.' >&2;exit 1; }
 if $public_host && [[ -f /etc/caddy/Caddyfile ]] && ! head -1 /etc/caddy/Caddyfile | grep -qx '# Managed by lidza-deploy installer'; then
  echo 'Existing unmanaged Caddy configuration detected; installation stopped without replacing it. Use a dedicated host or integrate the staged configuration.' >&2;exit 1
 fi
 if $public_host && command -v ss >/dev/null && ss -ltnH '( sport = :80 or sport = :443 )' | grep -q . && [[ ! -f /etc/lidza-agent/config.json ]]; then
  echo 'Ports 80/443 are already in use. Use a dedicated host.' >&2;exit 1
 fi
 # Stop before changing packages when a new service cannot bind its loopback port.
 if command -v ss >/dev/null; then
  for service_port in 'lidza-agent:9090' 'lidza-agent:8081'; do
   service=${service_port%:*}; port=${service_port#*:}
   if [[ ! -f /etc/systemd/system/$service.service ]] && ss -ltnH "sport = :$port" | grep -q .; then
    echo "Port $port is already in use; free this port before installing $service. No installation changes were made." >&2;exit 1
   fi
  done
  if $with_control && [[ ! -f /etc/systemd/system/lidza-control.service ]] && ss -ltnH 'sport = :3000' | grep -q .; then
   echo 'Port 3000 is already in use; free this port before installing the control panel. No installation changes were made.' >&2;exit 1
  fi
 fi
 export DEBIAN_FRONTEND=noninteractive
 # BEGIN base dependency check
 missing_packages=()
 for package in ca-certificates curl gnupg git openssl util-linux openssh-server sudo python3 iptables; do
  if [[ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null || true)" != 'install ok installed' ]]; then
   missing_packages+=("$package")
  fi
 done
 if [[ ${#missing_packages[@]} -gt 0 ]]; then
  apt-get update
  apt-get install -y "${missing_packages[@]}"
 else
  echo 'Base dependencies already installed; skipping apt refresh.'
 fi
 # END base dependency check
 install -d -m 0755 /etc/apt/keyrings
 if ! command -v docker >/dev/null; then
  curl --proto '=https' --tlsv1.2 -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/lidza-docker.asc
  chmod 0644 /etc/apt/keyrings/lidza-docker.asc
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/lidza-docker.asc] https://download.docker.com/linux/%s %s stable\n' "$(dpkg --print-architecture)" "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/lidza-docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin
 fi
 if $public_host && ! command -v caddy >/dev/null; then
  key=$(mktemp);trap 'rm -f "$key"' EXIT
  curl --proto '=https' --tlsv1.2 -fsSL https://dl.cloudsmith.io/public/caddy/stable/gpg.key -o "$key"
  gpg --batch --yes --dearmor -o /etc/apt/keyrings/lidza-caddy.gpg "$key"
  chmod 0644 /etc/apt/keyrings/lidza-caddy.gpg
  printf '%s\n' 'deb [signed-by=/etc/apt/keyrings/lidza-caddy.gpg] https://dl.cloudsmith.io/public/caddy/stable/deb/debian any-version main' > /etc/apt/sources.list.d/lidza-caddy.list
  apt-get update
  apt-get install -y caddy
 fi
 getent group lidza-agent >/dev/null || groupadd --system lidza-agent
 id lidza-agent >/dev/null 2>&1 || useradd --system --gid lidza-agent --home-dir /var/lib/lidza-agent --shell /usr/sbin/nologin lidza-agent
 # Native applications are built and run by a deployment account (the one managed SSH access uses).
 id deploy >/dev/null 2>&1 || useradd --create-home --shell /bin/bash --password '*' deploy
 if $with_control; then
  getent group lidza-control >/dev/null || groupadd --system lidza-control
  id lidza-control >/dev/null 2>&1 || useradd --system --gid lidza-control --home-dir /var/lib/lidza-control --shell /usr/sbin/nologin lidza-control
  usermod -aG docker lidza-control
 fi
fi
root=$stage
install -d -m 0755 "$root/usr/local/bin" "$root/etc/systemd/system"
install -d -m 0700 "$root/etc/lidza-agent" "$root/var/lib/lidza-agent" "$root/var/lib/lidza-agent/docker"
install -m 0755 "$bundle/bin/lidza-agent" "$root/usr/local/bin/lidza-agent"
install -m 0644 "$bundle/deploy/lidza-agent.service" "$root/etc/systemd/system/lidza-agent.service"
if [[ ! -f "$root/etc/lidza-agent/config.json" ]]; then
 install -m 0600 "$bundle/deploy/agent.example.json" "$root/etc/lidza-agent/config.json"
fi
# Every installed server can run native applications as well as containers:
# name the deployment account and its home, keeping an existing native section.
native_home=$( { getent passwd deploy 2>/dev/null || true; } | cut -d: -f6)
python3 - "$root/etc/lidza-agent/config.json" "${native_home:-/home/deploy}" <<'PYNATIVE'
import json, os, sys
path, home = sys.argv[1], sys.argv[2]
with open(path) as f: config = json.load(f)
if 'native' not in config and not config.get('ingress_only'):
    config['native'] = {'root': home, 'user': 'deploy'}
    temp = path + '.next'
    with open(os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), 'w') as f:
        json.dump(config, f, indent=2); f.write('\n')
    os.replace(temp, path)
PYNATIVE
if [[ ! -f "$root/etc/lidza-agent/agent.env" ]]; then
 token=$(openssl rand -hex 32)
 printf 'LIDZA_AGENT_TOKEN=%s\nDOCKER_CONFIG=/var/lib/lidza-agent/docker\n' "$token" > "$root/etc/lidza-agent/agent.env"
else
 token=$(sed -n 's/^LIDZA_AGENT_TOKEN=//p' "$root/etc/lidza-agent/agent.env")
 [[ "$token" =~ ^[a-f0-9]{64}$ ]] || { echo 'Existing token format is not installer-managed; refusing to replace it.' >&2;exit 1; }
fi
# This installer sets the host up for containers (it installs Docker when missing),
# so the agent keeps Docker's live-restore on here even if Docker was already present.
: > "$root/etc/lidza-agent/docker-managed"
api_url=http://127.0.0.1:9090
[[ -z "$hostname" ]] || api_url="https://$hostname"
server_id=local
if [[ -n "$hostname" ]]; then server_id="agent-$(printf '%s' "$hostname" | sha256sum | cut -c1-12)";fi
printf '{"id":"%s","name":"%s","url":"%s","token":"%s"}\n' "$server_id" "${hostname:-Local server}" "$api_url" "$token" > "$root/etc/lidza-agent/connection.json"
# Use a stable, valid local ID when no hostname is supplied.
if [[ -z "$hostname" ]]; then sed -i 's/"id":""/"id":"local"/' "$root/etc/lidza-agent/connection.json";fi
if $public_host; then
install -d -m 0755 "$root/etc/caddy"
# GUI-written Caddy configuration is autosaved and restored on restarts.
install -d -m 0755 "$root/etc/systemd/system/caddy.service.d"
cat > "$root/etc/systemd/system/caddy.service.d/10-lidza-resume.conf" <<'UNIT'
[Service]
ExecStart=
ExecStart=/usr/bin/caddy run --environ --resume --config /etc/caddy/Caddyfile
ExecReload=
UNIT
fi
if $with_control; then
 install -d -m 0700 "$root/etc/lidza-control" "$root/var/lib/lidza-control" "$root/var/lib/lidza-control/docker"
 install -m 0755 "$bundle/bin/lidza-control" "$root/usr/local/bin/lidza-control"
 install -m 0644 "$bundle/deploy/lidza-control.service" "$root/etc/systemd/system/lidza-control.service"
 if [[ ! -f "$root/etc/lidza-control/control.env" ]]; then
  printf '%s\n' 'CONTROL_DATA_DIR=/var/lib/lidza-control' 'LIDZA_ADDR=127.0.0.1:3000' 'DOCKER_CONFIG=/var/lib/lidza-control/docker' > "$root/etc/lidza-control/control.env"
 fi
 # Remove retired self-update webhook settings without touching OAuth configuration.
 sed -i '/^LIDZA_SELF_UPDATE_SECRET=/d;/^LIDZA_SELF_UPDATE_SERVER=/d' "$root/etc/lidza-control/control.env"
 if ! grep -q '^CONTROL_SETUP_ORIGIN=.' "$root/etc/lidza-control/control.env";then
  sed -i '/^CONTROL_SETUP_ORIGIN=/d' "$root/etc/lidza-control/control.env"
  printf 'CONTROL_SETUP_ORIGIN=%s\n' "$setup_origin" >> "$root/etc/lidza-control/control.env"
 fi
 if [[ ! -f "$root/var/lib/lidza-control/servers.json" ]]; then
  printf '[{"id":"local","name":"This server","url":"http://127.0.0.1:9090","token":"%s"}]\n' "$token" > "$root/var/lib/lidza-control/servers.json"
 fi
fi
install -d -m 0755 "$root/usr/local/libexec"
install -m 0755 "$bundle/deploy/upgrade-agent.sh" "$root/usr/local/libexec/lidza-agent-upgrade"
if [[ -z "$stage" ]];then
 "$root/usr/local/libexec/lidza-agent-upgrade" --ensure-swap || echo "Warning: automatic swap provisioning failed; check Server health before building apps." >&2
fi
install -m 0644 "$bundle/deploy/lidza-agent-upgrade.path" "$bundle/deploy/lidza-agent-upgrade.service" "$root/etc/systemd/system/"
if $public_host; then
# Hostnames with an installer-managed (not on-demand) certificate.
{
 [[ -z "$hostname" ]] || printf '%s\n' "$hostname"
 if $with_control && [[ "$fqdn" != localhost ]];then printf '%s\n' "$fqdn";fi
} > "$root/etc/lidza-agent/tls-hostnames"
chmod 0644 "$root/etc/lidza-agent/tls-hostnames"
cat > "$root/usr/local/libexec/lidza-tls-retry" <<'RETRY'
#!/bin/sh
# Managed by lidza-deploy installer.
# After a failed certificate attempt Caddy waits up to several hours before its
# next one, so a hostname whose DNS or ports become ready after installation can
# stay without HTTPS for a long time. While an installer-configured hostname has
# no trusted certificate, re-apply the running Caddy configuration unchanged:
# that is a graceful reload which starts a fresh attempt immediately.
set -u
hosts=/etc/lidza-agent/tls-hostnames
state=/var/lib/lidza-agent/tls-retry
admin=http://127.0.0.1:2019
[ -r "$hosts" ] || exit 0
mkdir -p "$state"
now=$(date +%s)
retry=''
while read -r host; do
 case "$host" in ''|*[!a-z0-9.-]*) continue;; esac
 # A verified handshake with the local Caddy means the certificate is issued and trusted.
 if curl -sS -o /dev/null --max-time 10 --resolve "$host:443:127.0.0.1" "https://$host/" 2>/dev/null; then
  rm -f "$state/$host";continue
 fi
 answer=$(getent ahosts "$host" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ' | sed 's/ *$//')
 # Nothing can be issued for a hostname that does not resolve yet.
 [ -n "$answer" ] || continue
 previous='';changed=0;last=0
 if [ -r "$state/$host" ]; then { read -r previous;read -r changed;read -r last; } < "$state/$host" || true;fi
 case "$changed" in ''|*[!0-9]*) changed=0;; esac
 case "$last" in ''|*[!0-9]*) last=0;; esac
 due=0
 if [ "$answer" != "$previous" ]; then
  # New or changed DNS answer: retry now.
  changed=$now;due=1
 elif [ $((now-changed)) -lt 21600 ] && [ $((now-last)) -ge 900 ]; then
  # Same answer (for example, ports opened later): at most four attempts an hour,
  # below the certificate authority's failure limit, for six hours. After that
  # Caddy's own schedule continues.
  due=1
 fi
 if [ "$due" -eq 1 ]; then last=$now;retry="$retry $host";fi
 printf '%s\n%s\n%s\n' "$answer" "$changed" "$last" > "$state/$host"
done < "$hosts"
[ -n "$retry" ] || exit 0
config=$(mktemp)
trap 'rm -f "$config"' EXIT
curl -fsS --max-time 10 "$admin/config/" -o "$config" || { echo 'Caddy admin API unavailable; certificate retry skipped.' >&2;exit 1; }
[ -s "$config" ] && [ "$(head -c 5 "$config")" != null ] || { echo 'Caddy has no running configuration; certificate retry skipped.' >&2;exit 1; }
curl -fsS --max-time 60 -o /dev/null -X POST -H 'Content-Type: application/json' -H 'Cache-Control: must-revalidate' --data-binary @"$config" "$admin/load" || { echo 'Caddy rejected the reload; certificate retry failed.' >&2;exit 1; }
echo "Retried certificate issuance for:$retry"
RETRY
chmod 0755 "$root/usr/local/libexec/lidza-tls-retry"
cat > "$root/etc/systemd/system/lidza-tls-retry.service" <<'UNIT'
[Unit]
Description=Retry HTTPS certificates for Lidza hostnames that became reachable after installation
After=network-online.target caddy.service
[Service]
Type=oneshot
ExecStart=/usr/local/libexec/lidza-tls-retry
User=root
UMask=0077
TimeoutStartSec=3min
UNIT
cat > "$root/etc/systemd/system/lidza-tls-retry.timer" <<'UNIT'
[Unit]
Description=Check Lidza hostnames that still lack an HTTPS certificate
[Timer]
OnActiveSec=5min
OnUnitActiveSec=2min
AccuracySec=30s
[Install]
WantedBy=timers.target
UNIT
fi
if $public_host; then
config=$(mktemp "$root/etc/caddy/.lidza-config.XXXXXX")
{
 echo '# Managed by lidza-deploy installer'
 echo '{'
 [[ -z "$email" ]] || printf ' email %s\n' "$email"
 printf ' on_demand_tls {\n  ask http://127.0.0.1:9090/tls/allow\n }\n}\n'
 printf 'http:// {\n handle /.well-known/lidza-deploy-host {\n  reverse_proxy 127.0.0.1:8081\n }\n handle {\n  redir https://{host}{uri} permanent\n }\n}\n'
 printf 'https:// {\n tls {\n  on_demand\n }\n reverse_proxy 127.0.0.1:8081\n}\n'
 if [[ -n "$hostname" ]]; then
  printf '%s {\n handle /v1/* {\n  reverse_proxy 127.0.0.1:9090\n }\n handle {\n  respond 404\n }\n}\n' "$hostname"
 fi
 if $with_control && [[ "$fqdn" != localhost ]];then
  printf '%s {\n @private path /metrics /readyz /healthz /tls-allow /tls-allow/*\n respond @private 404\n reverse_proxy 127.0.0.1:3000\n}\n' "$fqdn"
 fi
} > "$config"
chmod 0644 "$config"
fi
if [[ -n "$stage" ]]; then
 if $public_host; then mv "$config" "$root/etc/caddy/Caddyfile";fi
 echo "Staged installation in $stage. No host packages or services changed."
 exit 0
fi
chown -R lidza-agent:lidza-agent /etc/lidza-agent /var/lib/lidza-agent
if $with_control; then chown -R lidza-control:lidza-control /etc/lidza-control /var/lib/lidza-control;fi
if $public_host; then
caddy validate --config "$config" --adapter caddyfile
if [[ -f /etc/caddy/Caddyfile ]]; then cp -p /etc/caddy/Caddyfile /etc/caddy/Caddyfile.lidza-backup;fi
mv "$config" /etc/caddy/Caddyfile
fi
systemctl daemon-reload
systemctl enable --now docker
# The root host-management service must be able to reach Docker before the
# installer claims readiness. Its API remains private and token-authenticated.
docker info >/dev/null
systemctl enable lidza-agent
if $public_host; then systemctl enable caddy;fi
systemctl enable --now lidza-agent-upgrade.path
if $public_host; then systemctl enable --now lidza-tls-retry.timer;fi
systemctl restart lidza-agent
if $public_host; then
systemctl restart caddy
# Apply the installer routing even if Caddy has an older autosave (including its apt default).
caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
# Certificates and app traffic need 80/443 through an already active host firewall.
# Only allow rules are added; an inactive firewall is never enabled and no rule is removed.
firewall_opened=''
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
 if ufw allow 80/tcp >/dev/null && ufw allow 443/tcp >/dev/null && ufw allow 443/udp >/dev/null; then firewall_opened=UFW
 else firewall_opened=failed;echo 'Warning: could not open ports 80/443 in UFW; run: ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 443/udp' >&2;fi
elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
 if firewall-cmd --quiet --permanent --add-service=http --add-service=https --add-port=443/udp && firewall-cmd --quiet --reload; then firewall_opened=firewalld
 else firewall_opened=failed;echo 'Warning: could not open ports 80/443 in firewalld; run: firewall-cmd --permanent --add-service=http --add-service=https --add-port=443/udp && firewall-cmd --reload' >&2;fi
fi
fi
for attempt in {1..30}; do
 if curl -fsS http://127.0.0.1:9090/health >/dev/null; then break;fi
 [[ "$attempt" != 30 ]] || { echo 'Agent failed readiness; inspect journalctl -u lidza-agent.' >&2;exit 1; }
 sleep 1
done
# Keep the token out of command arguments and installer output.
printf 'header = "Authorization: Bearer %s"\n' "$token" | curl --config - -fsS http://127.0.0.1:9090/v1/apps >/dev/null
# Repair what would stop an application from running here (for example a
# database service that cannot start) and say what still needs a person.
printf 'header = "Authorization: Bearer %s"\n' "$token" | curl --config - -fsS --max-time 240 -X POST http://127.0.0.1:9090/v1/host/readiness/repair 2>/dev/null | python3 -c '
import json, sys
try: report = json.load(sys.stdin)
except Exception: sys.exit(0)
for line in report.get("repaired") or []: print("Repaired: " + line + ".")
for line in report.get("failed") or []: print("Could not repair: " + line)
for check in report.get("checks") or []:
    if not check.get("ok") and not (check.get("id") == "native" and check.get("repairable")): print("Needs attention - " + check.get("title", "") + ": " + check.get("detail", ""))
' || true
if $public_host; then
systemctl is-active --quiet caddy
printf '%s\n' 'Agent, Docker and Caddy are installed and running.' 'Private GUI connection details: /etc/lidza-agent/connection.json'
case "$firewall_opened" in
 '') echo 'No active UFW or firewalld host firewall was found; none was changed.';;
 failed) echo 'Ports 80/443 are still closed in the host firewall; see the warning above.';;
 *) echo "Opened ports 80/tcp, 443/tcp and 443/udp in $firewall_opened.";;
esac
printf '%s\n' 'For a remote GUI, the agent hostname must resolve to this host. Cloud security groups, routers and other external firewalls must also allow ports 80/443.'
printf '%s\n' 'If DNS or ports are not ready yet, the certificate is requested again automatically within minutes of the hostname resolving; no reinstall is needed.'
else
 printf '%s\n' 'Agent and Docker are installed and running in local mode. Existing Caddy and ports 80/443 were left untouched.' 'Private GUI connection details: /etc/lidza-agent/connection.json' 'Automatic HTTPS for hosted apps requires a public hosting agent; add one in the GUI.'
fi

if $with_control; then
 systemctl enable lidza-control
 systemctl restart lidza-control
 for attempt in {1..30}; do
  if curl -fsS http://127.0.0.1:3000/healthz >/dev/null; then break;fi
  [[ "$attempt" != 30 ]] || { echo 'Control-panel startup failed; inspect journalctl -u lidza-control.' >&2;exit 1; }
  sleep 1
 done
 printf 'Control panel installed. Finish setup at %s\n' "$setup_origin"
 if [[ "$fqdn" == localhost ]];then printf '%s\n' 'Use an SSH tunnel to port 3000 for remote local-mode setup.';else printf '%s\n' 'DNS must point to this server and ports 80/443 must be reachable. Caddy obtains HTTPS automatically.';fi
 printf '%s\n' 'One-time setup credential: /var/lib/lidza-control/setup-token (read locally on the server).' 'Database, GitHub and operator account are configured in the browser.'
fi
