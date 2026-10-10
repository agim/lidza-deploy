#!/usr/bin/env bash
# Root service: only official GitHub release assets may replace managed binaries.
set -euo pipefail
umask 077
exec 9>/run/lidza-agent-upgrade.lock
flock -n 9 || exit 0
upgrade_dir=/var/lib/lidza-agent/upgrade
request=$upgrade_dir/request
[[ -d "$upgrade_dir" && ! -L "$upgrade_dir" && -f "$request" && ! -L "$request" ]] || exit 0
version=$(cat "$request")
rm -f "$request"
mkdir -p "$upgrade_dir"
report(){ status_tmp=$(mktemp /var/tmp/lidza-upgrade-status.XXXXXX);printf '{"state":"%s","message":"%s"}\n' "$1" "$2" > "$status_tmp";chown lidza-agent:lidza-agent "$status_tmp";chmod 0600 "$status_tmp";mv -T "$status_tmp" "$upgrade_dir/status.json"; }
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { report failed 'Invalid release version';exit 1; }
case $(uname -m) in x86_64) arch=amd64;;aarch64|arm64) arch=arm64;;*) report failed 'Unsupported architecture';exit 1;;esac
report upgrading 'Downloading and verifying official release'
upgrade_tmp=$(mktemp -d /var/tmp/lidza-upgrade.XXXXXX)
changed=false
control=false
rollback(){
 if $changed;then
  install -m 0755 /usr/local/bin/lidza-agent.previous /usr/local/bin/lidza-agent.rollback
  mv /usr/local/bin/lidza-agent.rollback /usr/local/bin/lidza-agent
  if $control;then install -m 0755 /usr/local/bin/lidza-control.previous /usr/local/bin/lidza-control.rollback;mv /usr/local/bin/lidza-control.rollback /usr/local/bin/lidza-control;fi
  systemctl restart lidza-agent || true
  if $control;then systemctl restart lidza-control || true;fi
 fi
 if $changed;then report failed 'Upgrade failed; previous managed binaries restored. Check service logs.';else report failed 'Upgrade failed before replacing binaries. Check service logs.';fi
}
finish(){ result=$?;trap - EXIT;if ((result!=0));then rollback;fi;rm -rf "$upgrade_tmp";exit "$result"; }
trap finish EXIT
asset=lidza-agent-linux-$arch.tar.gz
url=https://github.com/agim/lidza-deploy/releases/download/$version
curl --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 600 -fsSL "$url/$asset" -o "$upgrade_tmp/$asset"
curl --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 -fsSL "$url/$asset.sha256" -o "$upgrade_tmp/checksum"
read -r checksum filename < "$upgrade_tmp/checksum"
[[ "$checksum" =~ ^[a-f0-9]{64}$ && "$filename" == "$asset" ]]
[[ $(sha256sum "$upgrade_tmp/$asset" | cut -d' ' -f1) == "$checksum" ]]
# Release files are trusted only after downloading both asset and checksum from the fixed repository.
if tar -tzf "$upgrade_tmp/$asset" | grep -Eq '(^/|(^|/)\.\.(/|$))';then exit 1;fi
mkdir "$upgrade_tmp/bundle"
tar --no-same-owner -xzf "$upgrade_tmp/$asset" -C "$upgrade_tmp/bundle"
(cd "$upgrade_tmp/bundle" && sha256sum -c SHA256SUMS >/dev/null)
[[ $("$upgrade_tmp/bundle/bin/lidza-agent" -version) == "$version" ]]
[[ $("$upgrade_tmp/bundle/bin/lidza-control" -version) == "$version" ]]
install -m 0755 /usr/local/bin/lidza-agent /usr/local/bin/lidza-agent.previous
if systemctl is-enabled --quiet lidza-control;then control=true;install -m 0755 /usr/local/bin/lidza-control /usr/local/bin/lidza-control.previous;fi
install -m 0755 "$upgrade_tmp/bundle/bin/lidza-agent" /usr/local/bin/lidza-agent.next
if $control;then install -m 0755 "$upgrade_tmp/bundle/bin/lidza-control" /usr/local/bin/lidza-control.next;fi
changed=true
mv /usr/local/bin/lidza-agent.next /usr/local/bin/lidza-agent
if $control;then mv /usr/local/bin/lidza-control.next /usr/local/bin/lidza-control;fi
systemctl restart lidza-agent
if $control;then
 # Retired self-update credentials have no role in GUI-driven release checks.
 if [[ -f /etc/lidza-control/control.env ]];then sed -i '/^LIDZA_SELF_UPDATE_SECRET=/d;/^LIDZA_SELF_UPDATE_SERVER=/d' /etc/lidza-control/control.env;fi
 systemctl restart lidza-control
fi
healthy=false
for attempt in {1..60};do
 if curl --max-time 2 -fsS http://127.0.0.1:9090/health >/dev/null && { ! $control || curl --max-time 2 -fsS http://127.0.0.1:3000/readyz >/dev/null; };then healthy=true;break;fi
 sleep 1
done
$healthy
changed=false
# Keep the verified CLI updater current for subsequent releases.
if [[ -f "$upgrade_tmp/bundle/deploy/upgrade-agent.sh" ]];then
 install -m 0755 "$upgrade_tmp/bundle/deploy/upgrade-agent.sh" /usr/local/libexec/lidza-agent-upgrade.next
 mv /usr/local/libexec/lidza-agent-upgrade.next /usr/local/libexec/lidza-agent-upgrade
fi
report succeeded 'Official release installed; health checks passed'
