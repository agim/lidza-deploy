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
 for package in ca-certificates curl gnupg git openssl util-linux; do
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
 usermod -aG docker lidza-agent
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
if [[ ! -f "$root/etc/lidza-agent/agent.env" ]]; then
 token=$(openssl rand -hex 32)
 printf 'LIDZA_AGENT_TOKEN=%s\nDOCKER_CONFIG=/var/lib/lidza-agent/docker\n' "$token" > "$root/etc/lidza-agent/agent.env"
else
 token=$(sed -n 's/^LIDZA_AGENT_TOKEN=//p' "$root/etc/lidza-agent/agent.env")
 [[ "$token" =~ ^[a-f0-9]{64}$ ]] || { echo 'Existing token format is not installer-managed; refusing to replace it.' >&2;exit 1; }
fi
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
install -m 0644 "$bundle/deploy/lidza-agent-upgrade.path" "$bundle/deploy/lidza-agent-upgrade.service" "$root/etc/systemd/system/"
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
  printf '%s {\n @private path /metrics /readyz /healthz\n respond @private 404\n reverse_proxy 127.0.0.1:3000\n}\n' "$fqdn"
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
# Verify access with the actual service account before claiming readiness.
runuser -u lidza-agent -- docker info >/dev/null
systemctl enable lidza-agent
if $public_host; then systemctl enable caddy;fi
systemctl enable --now lidza-agent-upgrade.path
systemctl restart lidza-agent
if $public_host; then
systemctl restart caddy
# Apply the installer routing even if Caddy has an older autosave (including its apt default).
caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
fi
for attempt in {1..30}; do
 if curl -fsS http://127.0.0.1:9090/health >/dev/null; then break;fi
 [[ "$attempt" != 30 ]] || { echo 'Agent failed readiness; inspect journalctl -u lidza-agent.' >&2;exit 1; }
 sleep 1
done
# Keep the token out of command arguments and installer output.
printf 'header = "Authorization: Bearer %s"\n' "$token" | curl --config - -fsS http://127.0.0.1:9090/v1/apps >/dev/null
if $public_host; then
systemctl is-active --quiet caddy
printf '%s\n' 'Agent, Docker and Caddy are installed and running.' 'Private GUI connection details: /etc/lidza-agent/connection.json' 'For a remote GUI, the agent hostname must resolve to this host and ports 80/443 must be reachable.'
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
