#!/usr/bin/env bash
# Install a verified local agent bundle on a dedicated Debian/Ubuntu host.
set -euo pipefail
umask 077
bundle=$(cd "$(dirname "$0")" && pwd)
stage='' hostname='' email='' plan=false with_control=false
usage() {
 cat <<'TXT'
Usage: sudo ./install-agent.sh [--with-control] [--hostname agent.example.com] [--email ops@example.com]
       ./install-agent.sh --plan
       ./install-agent.sh --stage /absolute/staging-directory [options]

Installs Docker Engine, Caddy, Git, the bundled agent and systemd services.
--with-control also installs the browser-configured control panel on this host.
Without --hostname, the agent API stays accessible only on loopback.
--hostname exposes the authenticated API over automatic HTTPS for a remote GUI.
--stage writes an inspectable filesystem tree without installing packages or services.
Requires a dedicated Debian 12/13 or Ubuntu 22.04/24.04 server for live installation.
TXT
}
while (($#)); do
 case "$1" in
  --hostname|--email|--stage|--bundle) (($#>=2)) || { usage; exit 2; }; case "$1" in --hostname) hostname=$2;; --email) email=$2;; --stage) stage=$2;; --bundle) bundle=$2;; esac;shift 2;;
  --plan) plan=true;shift;;
  --with-control) with_control=true;shift;;
  --help|-h) usage;exit 0;;
  *) usage;exit 2;;
 esac
done
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
2. Install Git, Docker Engine and Caddy through signed apt repositories.
3. Preserve an existing agent token; otherwise generate a random token.
4. Install the agent, private configuration, and systemd service.
5. Validate Caddy routing for application FQDNs and the optional agent hostname.
6. Start services; check the authenticated agent API and Caddy configuration.
7. Save private GUI connection details in /etc/lidza-agent/connection.json.
Agent API hostname: ${hostname:-loopback only}
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
 if [[ -f /etc/caddy/Caddyfile ]] && ! head -1 /etc/caddy/Caddyfile | grep -qx '# Managed by lidza-deploy installer'; then
  echo 'Existing unmanaged Caddy configuration detected; installation stopped without replacing it. Use a dedicated host or integrate the staged configuration.' >&2;exit 1
 fi
 if command -v ss >/dev/null && ss -ltnH '( sport = :80 or sport = :443 )' | grep -q . && [[ ! -f /etc/lidza-agent/config.json ]]; then
  echo 'Ports 80/443 are already in use. Use a dedicated host.' >&2;exit 1
 fi
 export DEBIAN_FRONTEND=noninteractive
 apt-get update
 apt-get install -y ca-certificates curl gnupg git openssl
 install -d -m 0755 /etc/apt/keyrings
 if ! command -v docker >/dev/null; then
  curl --proto '=https' --tlsv1.2 -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/lidza-docker.asc
  chmod 0644 /etc/apt/keyrings/lidza-docker.asc
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/lidza-docker.asc] https://download.docker.com/linux/%s %s stable\n' "$(dpkg --print-architecture)" "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/lidza-docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin
 fi
 if ! command -v caddy >/dev/null; then
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
install -d -m 0755 "$root/usr/local/bin" "$root/etc/systemd/system" "$root/etc/caddy"
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
# GUI-written Caddy configuration is autosaved and restored on restarts.
install -d -m 0755 "$root/etc/systemd/system/caddy.service.d"
cat > "$root/etc/systemd/system/caddy.service.d/10-lidza-resume.conf" <<'UNIT'
[Service]
ExecStart=
ExecStart=/usr/bin/caddy run --environ --resume --config /etc/caddy/Caddyfile
ExecReload=
UNIT
if $with_control; then
 install -d -m 0700 "$root/etc/lidza-control" "$root/var/lib/lidza-control" "$root/var/lib/lidza-control/docker"
 install -m 0755 "$bundle/bin/lidza-control" "$root/usr/local/bin/lidza-control"
 install -m 0644 "$bundle/deploy/lidza-control.service" "$root/etc/systemd/system/lidza-control.service"
 if [[ ! -f "$root/etc/lidza-control/control.env" ]]; then
  printf '%s\n' 'CONTROL_DATA_DIR=/var/lib/lidza-control' 'LIDZA_ADDR=127.0.0.1:3000' 'DOCKER_CONFIG=/var/lib/lidza-control/docker' > "$root/etc/lidza-control/control.env"
 fi
 if [[ ! -f "$root/var/lib/lidza-control/servers.json" ]]; then
  printf '[{"id":"local","name":"This server","url":"http://127.0.0.1:9090","token":"%s"}]\n' "$token" > "$root/var/lib/lidza-control/servers.json"
 fi
fi
config=$(mktemp "$root/etc/caddy/.lidza-config.XXXXXX")
{
 echo '# Managed by lidza-deploy installer'
 echo '{'
 [[ -z "$email" ]] || printf ' email %s\n' "$email"
 printf ' on_demand_tls {\n  ask http://127.0.0.1:9090/tls/allow\n }\n}\n'
 printf 'https:// {\n tls {\n  on_demand\n }\n reverse_proxy 127.0.0.1:8081\n}\n'
 if [[ -n "$hostname" ]]; then
  printf '%s {\n handle /v1/* {\n  reverse_proxy 127.0.0.1:9090\n }\n handle {\n  respond 404\n }\n}\n' "$hostname"
 fi
} > "$config"
chmod 0644 "$config"
if [[ -n "$stage" ]]; then
 mv "$config" "$root/etc/caddy/Caddyfile"
 echo "Staged installation in $stage. No host packages or services changed."
 exit 0
fi
chown -R lidza-agent:lidza-agent /etc/lidza-agent /var/lib/lidza-agent
if $with_control; then chown -R lidza-control:lidza-control /etc/lidza-control /var/lib/lidza-control;fi
caddy validate --config "$config" --adapter caddyfile
if [[ -f /etc/caddy/Caddyfile ]]; then cp -p /etc/caddy/Caddyfile /etc/caddy/Caddyfile.lidza-backup;fi
mv "$config" /etc/caddy/Caddyfile
systemctl daemon-reload
systemctl enable --now docker
# Verify access with the actual service account before claiming readiness.
runuser -u lidza-agent -- docker info >/dev/null
systemctl enable lidza-agent caddy
systemctl restart lidza-agent
systemctl restart caddy
for attempt in {1..30}; do
 if curl -fsS http://127.0.0.1:9090/health >/dev/null; then break;fi
 [[ "$attempt" != 30 ]] || { echo 'Agent failed readiness; inspect journalctl -u lidza-agent.' >&2;exit 1; }
 sleep 1
done
# Keep the token out of command arguments and installer output.
printf 'header = "Authorization: Bearer %s"\n' "$token" | curl --config - -fsS http://127.0.0.1:9090/v1/apps >/dev/null
systemctl is-active --quiet caddy
printf '%s\n' 'Agent, Docker and Caddy are installed and running.' 'Private GUI connection details: /etc/lidza-agent/connection.json' 'For a remote GUI, the agent hostname must resolve to this host and ports 80/443 must be reachable.'

if $with_control; then
 systemctl enable lidza-control
 systemctl restart lidza-control
 for attempt in {1..30}; do
  if curl -fsS http://127.0.0.1:3000/healthz >/dev/null; then break;fi
  [[ "$attempt" != 30 ]] || { echo 'Control-panel startup failed; inspect journalctl -u lidza-control.' >&2;exit 1; }
  sleep 1
 done
 printf '%s\n' 'Control panel installed. Open it through an SSH tunnel to port 3000 to finish setup.' 'One-time setup credential: /var/lib/lidza-control/setup-token (read locally on the server).' 'Database, domain/DNS, GitHub and operator account are configured in the browser.'
fi
