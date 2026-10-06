#!/bin/sh
# Līdza Deploy host installer.
# curl -fsSL https://raw.githubusercontent.com/agim/lidza-deploy/main/install.sh | sudo sh -s -- --fqdn deploy.example.com
# sh install.sh --fqdn deploy.example.com|localhost [--agent-only] [--hostname agent.example.com] [--email ops@example.com]
# Defaults to installing the agent and browser-configured control panel.
set -eu
umask 077
version=main
version_given=0
with_control=1
fqdn=''
hostname=''
email=''
stage=''
bundle=''
source_build=0
check=0
plan=0
usage() {
 cat <<'TXT'
Līdza Deploy installer
Usage: sh install.sh --fqdn deploy.example.com|localhost [options]
  --fqdn HOST         Required: GUI hostname, or explicit localhost for local setup
  --agent-only         Install only the hosting agent on a remote app server
  --with-control       Install agent + web control panel (default)
  --hostname FQDN      Expose the agent API over HTTPS for a remote control panel
  --email ADDRESS      Caddy certificate contact email
  --source             Build from source instead of using released binaries
  --version REF        Build this GitHub branch/tag/commit (implies --source)
  --bundle PATH        Use a local tar.gz bundle and adjacent .sha256 file
  --stage DIRECTORY    Write an inspectable tree; no packages/services changed
  --check              Report host prerequisites without installing
  --plan               Show installation steps without installing
  --yes, -y            Noninteractive install (already the default)
  --help, -h           Show help
Requires Debian 12/13 or Ubuntu 22.04/24.04 with systemd.
Public hosting requires a dedicated host; localhost mode leaves ports 80/443 alone.
Installs Git, Docker and services; public mode also installs Caddy.
First-run settings are entered in the GUI.
Public DNS must point to this host and ports 80/443 must be reachable.
Default installs verified release binaries; Go is not needed.
Source builds reuse Go or download it temporarily with a pinned checksum.
TXT
}
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }
while [ "$#" -gt 0 ]; do
 case "$1" in
  --agent-only) with_control=0; shift;;
  --with-control) with_control=1; shift;;
  --source) source_build=1; shift;;
  --fqdn|--hostname|--email|--version|--bundle|--stage)
   [ "$#" -ge 2 ] || die "$1 requires a value"
   case "$1" in
    --fqdn) fqdn=$2;; --hostname) hostname=$2;; --email) email=$2;;
    --version) version=$2;version_given=1;source_build=1;; --bundle) bundle=$2;; --stage) stage=$2;;
   esac
   shift 2;;
  --check) check=1;shift;; --plan) plan=1;shift;;
  --yes|-y) shift;; --help|-h) usage;exit 0;;
  *) die "unknown option: $1";;
 esac
done
if [ "$check" -eq 0 ]; then
 [ -n "$fqdn" ] || die 'Missing --fqdn. Run: sudo sh install.sh --fqdn deploy.example.com (public HTTPS), or sudo sh install.sh --fqdn localhost (local/tunnel setup). No installation changes were made.'
fi
if [ -n "$fqdn" ] && [ "$fqdn" != localhost ]; then
 case "$fqdn" in *[!a-z0-9.-]*) die 'invalid --fqdn: use a DNS hostname or localhost';; esac
 printf '%s\n' "$fqdn" | awk '
  length($0)>253 || $0 ~ /^[0-9.]+$/ {exit 1}
  {n=split($0,a,".");if(n<2)exit 1;for(i=1;i<=n;i++)if(length(a[i])>63 || a[i]!~/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/)exit 1}
 ' || die '--fqdn must be a DNS hostname without a scheme, port or path, or exactly localhost'
fi
case "$version" in ''|*[!a-zA-Z0-9._-]*|.*|-*) die 'version must be a simple GitHub branch, tag or commit';; esac
case "$stage" in '') ;; /*) [ "$stage" != / ] || die 'stage must not be /'; case "$stage" in *..*) die 'stage must not contain ..';; esac;; *) die 'stage must be absolute';; esac
[ "$(uname -s)" = Linux ] || die 'only Linux hosting servers are supported'
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die 'only amd64 and arm64 are supported';; esac
if [ "$plan" -eq 1 ]; then
 printf '%s\n' "Līdza Deploy ($arch)" \
  '1. Check the Debian/Ubuntu systemd host.' \
  '2. Verify released binaries (or an explicit local bundle/source build).' \
  '3. Install Git, Docker Engine and the agent; public mode also installs Caddy.'
 if [ "$with_control" -eq 1 ]; then printf '%s\n' '4. Install and start the control panel, paired with the local agent.' "5. Open the chosen GUI address ($fqdn) and complete browser setup.";fi
 if [ "$fqdn" = localhost ] && [ -z "$hostname" ];then printf '%s\n' 'Local mode: leave Caddy and ports 80/443 untouched; use public agents for app HTTPS.';fi
 exit 0
fi
if [ "$check" -eq 1 ]; then
 printf 'Architecture: %s\n' "$arch"
 if [ -r /etc/os-release ]; then . /etc/os-release;printf 'Host: %s %s\n' "$ID" "$VERSION_ID";fi
 for tool in curl tar sha256sum go docker caddy systemctl; do
  if command -v "$tool" >/dev/null 2>&1; then printf '[ok] %s\n' "$tool";else printf '[missing] %s\n' "$tool";fi
 done
 if [ -d /run/systemd/system ];then printf '[ok] running systemd\n';else printf '[missing] running systemd (required for live install)\n';fi
 exit 0
fi
if [ -z "$stage" ]; then
 [ "$(id -u)" -eq 0 ] || die 'run with sudo (or use --stage for a preview)'
 [ -r /etc/os-release ] || die 'cannot identify Linux distribution'
 . /etc/os-release
 case "$ID:$VERSION_ID" in debian:12|debian:13|ubuntu:22.04|ubuntu:24.04) ;; *) die 'use Debian 12/13 or Ubuntu 22.04/24.04';; esac
 [ -d /run/systemd/system ] || die 'a running systemd host is required'
 if [ "$fqdn" != localhost ] || [ -n "$hostname" ]; then
  if [ -f /etc/caddy/Caddyfile ] && ! head -1 /etc/caddy/Caddyfile | grep -qx '# Managed by lidza-deploy installer';then
   die 'existing unmanaged Caddy configuration detected; use a dedicated public hosting server or --fqdn localhost without --hostname'
  fi
  if command -v ss >/dev/null 2>&1 && ss -ltnH '( sport = :80 or sport = :443 )' | grep -q . && [ ! -f /etc/lidza-agent/config.json ];then
   die 'ports 80/443 are already in use; use a dedicated public hosting server or --fqdn localhost without --hostname'
  fi
 fi
 if command -v ss >/dev/null 2>&1;then
  service_ports='lidza-agent:9090 lidza-agent:8081'
  if [ "$with_control" -eq 1 ];then service_ports="$service_ports lidza-control:3000";fi
  for service_port in $service_ports;do
   service=${service_port%:*};port=${service_port#*:}
   if [ ! -f "/etc/systemd/system/$service.service" ] && ss -ltnH "sport = :$port" | grep -q .;then
    die "port $port is already in use; free this port before installing $service. No installation changes were made"
   fi
  done
 fi
fi
for tool in tar sha256sum; do command -v "$tool" >/dev/null 2>&1 || die "$tool is required";done
scratch=$(mktemp -d /tmp/lidza-deploy-install.XXXXXX)
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
fetch() {
 command -v curl >/dev/null 2>&1 || die 'curl is required; install it with your package manager'
 curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 "$1" -o "$2"
}
verify() {
 expected=$1;file=$2
 [ "${#expected}" -eq 64 ] || die 'invalid SHA-256 checksum'
 case "$expected" in *[!a-f0-9]*) die 'invalid SHA-256 checksum';; esac
 actual=$(sha256sum "$file");actual=${actual%% *}
 [ "$actual" = "$expected" ] || die 'checksum verification failed'
}
if [ -z "$bundle" ] && [ "$source_build" -eq 0 ]; then
 printf '%s\n' 'Downloading verified Līdza Deploy release binaries…'
 asset="lidza-agent-linux-$arch.tar.gz"
 release_url='https://github.com/agim/lidza-deploy/releases/latest/download'
 fetch "$release_url/$asset" "$scratch/$asset" || die 'Could not download release binaries. Check GitHub access from this host. Use --source only if you intend to build with Go.'
 fetch "$release_url/$asset.sha256" "$scratch/$asset.sha256" || die 'Could not download the release checksum; installation stopped before host changes.'
 bundle="$scratch/$asset"
fi
if [ -n "$bundle" ]; then
 [ -f "$bundle" ] && [ -f "$bundle.sha256" ] || die 'bundle and adjacent .sha256 file are required'
 checksum=$(awk 'NR==1 {print $1}' "$bundle.sha256")
 verify "$checksum" "$bundle"
 # Extraction is allowed only within the temporary bundle directory.
 tar -tzf "$bundle" > "$scratch/members"
 if awk '/^\// || /(^|\/)\.\.(\/|$)/ {bad=1} END {exit !bad}' "$scratch/members";then die 'unsafe bundle member path';fi
 mkdir "$scratch/bundle"
 tar -xzf "$bundle" -C "$scratch/bundle"
 prepared="$scratch/bundle"
else
 # A checked-out install.sh uses that checkout; curl|sh downloads the source.
 source_dir=''
 if [ "$version_given" -eq 0 ] && [ -f "$0" ]; then
  candidate=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
  if [ -f "$candidate/go.mod" ] && [ -f "$candidate/scripts/install-agent.sh" ];then source_dir=$candidate;fi
 fi
 if [ -z "$source_dir" ]; then
  printf 'Downloading Līdza Deploy source (%s)…\n' "$version"
  fetch "https://codeload.github.com/agim/lidza-deploy/tar.gz/$version" "$scratch/source.tar.gz"
  mkdir "$scratch/source";tar -xzf "$scratch/source.tar.gz" -C "$scratch/source" --strip-components=1
  source_dir="$scratch/source"
 fi
 go_cmd=''
 if command -v go >/dev/null 2>&1; then
  installed=$(go env GOVERSION 2>/dev/null || true)
  minor=$(printf '%s' "$installed" | sed -n 's/^go1\.\([0-9][0-9]*\).*/\1/p')
  if [ -n "$minor" ] && [ "$minor" -ge 27 ];then go_cmd=$(command -v go);fi
 fi
 if [ -z "$go_cmd" ]; then
  case "$arch" in
   amd64) go_sum=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445;;
   arm64) go_sum=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec;;
  esac
  printf '%s\n' 'Downloading verified Go 1.27.1 for this build…'
  if ! fetch "https://go.dev/dl/go1.27.1.linux-$arch.tar.gz" "$scratch/go.tar.gz"; then
   printf '%s\n' 'Go download failed through go.dev; trying the direct Google download…' >&2
   fetch "https://dl.google.com/go/go1.27.1.linux-$arch.tar.gz" "$scratch/go.tar.gz" || die "Go 1.27.1 could not be downloaded from either official URL. Check https://dl.google.com/go/go1.27.1.linux-$arch.tar.gz from this host, then retry. No packages or services were changed."
  fi
  verify "$go_sum" "$scratch/go.tar.gz"
  tar -xzf "$scratch/go.tar.gz" -C "$scratch"
  go_cmd="$scratch/go/bin/go"
 fi
 prepared="$scratch/bundle"
 mkdir -p "$prepared/bin" "$prepared/deploy"
 printf '%s\n' 'Building agent and control panel…'
 (cd "$source_dir"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_cmd" build -buildvcs=false -trimpath -ldflags "-X github.com/agim/lidza-deploy/internal/buildinfo.Version=$version" -o "$prepared/bin/lidza-agent" ./cmd/agent
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" "$go_cmd" build -buildvcs=false -trimpath -ldflags "-X github.com/agim/lidza-deploy/internal/buildinfo.Version=$version" -o "$prepared/bin/lidza-control" ./cmd/web
 )
 cp "$source_dir/scripts/install-agent.sh" "$prepared/install-agent.sh"
 cp "$source_dir/scripts/upgrade-agent.sh" "$prepared/deploy/upgrade-agent.sh"
 cp "$source_dir/deploy/lidza-agent.service" "$source_dir/deploy/lidza-control.service" "$source_dir/deploy/agent.example.json" "$source_dir/deploy/lidza-agent-upgrade.path" "$source_dir/deploy/lidza-agent-upgrade.service" "$prepared/deploy/"
 chmod 0755 "$prepared/install-agent.sh" "$prepared/bin/"*
 printf '%s\n' "$arch" > "$prepared/ARCH"
 (cd "$prepared";sha256sum ARCH bin/lidza-agent bin/lidza-control install-agent.sh deploy/lidza-agent.service deploy/lidza-control.service deploy/agent.example.json deploy/upgrade-agent.sh deploy/lidza-agent-upgrade.path deploy/lidza-agent-upgrade.service > SHA256SUMS)
fi
set -- --fqdn "$fqdn"
if [ "$with_control" -eq 1 ];then set -- "$@" --with-control;fi
if [ -n "$hostname" ];then set -- "$@" --hostname "$hostname";fi
if [ -n "$email" ];then set -- "$@" --email "$email";fi
if [ -n "$stage" ];then set -- "$@" --stage "$stage";fi
bash "$prepared/install-agent.sh" "$@"
