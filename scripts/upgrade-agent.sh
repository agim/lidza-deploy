#!/usr/bin/env bash
# Root service: only official GitHub release assets, or signed distribution
# bundles verified by the installed binary, may replace managed binaries.
set -euo pipefail
umask 077
# lidza-swap-v1
# Fixed root-only provisioning; never resize or overwrite an existing swap file.
ensure_swap(){
 [[ $EUID == 0 ]] || { echo 'Swap provisioning requires root.' >&2;return 1; }
 exec 8>/run/lidza-swap.lock
 flock -n 8 || return 0
 local state=ready message='' result=0 swap_path=/var/lib/lidza-deploy.swap swap_tmp=''
 swap_active(){ awk 'NR>1 {found=1} END {exit !found}' /proc/swaps; }
 swap_error(){ state=failed;message=$1;result=1;echo "Warning: $message" >&2; }
 if swap_active;then
  message='Existing active swap preserved.'
 elif [[ ! -f /etc/fstab || -L /etc/fstab ]];then
  swap_error 'Cannot safely inspect /etc/fstab; swap was not changed.'
 elif awk '$1 !~ /^#/ && $3=="swap" {found=1} END {exit !found}' /etc/fstab;then
  if swapon -a && swap_active;then message='Existing configured swap activated.';else swap_error 'Configured swap could not be activated; inspect swapon -a.';fi
 elif [[ -e "$swap_path" || -L "$swap_path" ]];then
  if [[ -f "$swap_path" && ! -L "$swap_path" && $(stat -c '%u:%a:%h' "$swap_path") == '0:600:1' && $(blkid -p -s TYPE -o value "$swap_path" 2>/dev/null) == swap ]];then
   if swapon "$swap_path";then message='Managed swap reactivated.';else swap_error 'Managed swap could not be activated.';fi
  else swap_error 'Reserved swap path already exists and is not a safe managed swap file; it was left untouched.';fi
 else
  local filesystem available
  filesystem=$(findmnt -n -o FSTYPE --target /var/lib) || filesystem=unknown
  available=$(df -Pk /var/lib | awk 'END {print $4}')
  if [[ "$filesystem" != ext4 && "$filesystem" != xfs ]];then
   swap_error 'Automatic swap supports ext4 and XFS; configure swap manually on this filesystem.'
  elif [[ ! "$available" =~ ^[0-9]+$ ]] || ((available<6291456));then
   swap_error 'Swap needs 6 GiB free disk space (4 GiB swap plus 2 GiB reserve); free space or resize the server.'
  else
   swap_tmp=$(mktemp /var/lib/.lidza-swap.XXXXXX)
   # Write allocated blocks: fallocate/sparse files are not portable swap backing.
   if chmod 0600 "$swap_tmp" && dd if=/dev/zero of="$swap_tmp" bs=1M count=4096 conv=fsync status=none && mkswap "$swap_tmp" >/dev/null;then
    # Never replace a file created concurrently by an administrator.
    if ln "$swap_tmp" "$swap_path";then
     rm -f "$swap_tmp";swap_tmp=''
     if swapon "$swap_path";then message='Created and activated 4 GiB managed swap.';else rm -f "$swap_path";swap_error 'Could not activate managed swap; the new file was removed.';fi
    else swap_error 'Reserved swap path appeared during provisioning; it was left untouched.';fi
   else swap_error 'Could not allocate or format the 4 GiB swap file.';fi
   [[ -z "$swap_tmp" ]] || rm -f "$swap_tmp"
  fi
 fi
 # Repair persistence even if a previous process stopped after swapon succeeded.
 if ((result==0)) && awk -v p="$swap_path" 'NR>1 && $1==p {found=1} END {exit !found}' /proc/swaps;then
  if ! awk -v p="$swap_path" '$1==p && $3=="swap" {found=1} END {exit !found}' /etc/fstab;then
   if [[ -L /etc/fstab || ! -f /etc/fstab ]];then swap_error 'Swap is active but persistence failed; check /etc/fstab.'
   elif ! printf '%s none swap sw 0 0\n' "$swap_path" >> /etc/fstab;then swap_error 'Swap is active but persistence failed; check /etc/fstab.';fi
  fi
 fi
 printf '%s\n' "$message"
 local status_tmp
 status_tmp=$(mktemp /var/tmp/lidza-swap-status.XXXXXX)
 printf '{"state":"%s","message":"%s","checked_at":%s}\n' "$state" "$message" "$(date +%s)" > "$status_tmp"
 if id lidza-agent >/dev/null 2>&1;then
  chown lidza-agent:lidza-agent "$status_tmp";chmod 0600 "$status_tmp"
  mv -T "$status_tmp" /var/lib/lidza-agent/swap-status.json
 else rm -f "$status_tmp";fi
 return "$result"
}
if [[ ${1:-} == --ensure-swap ]];then ensure_swap;exit $?;fi

exec 9>/run/lidza-agent-upgrade.lock
flock -n 9 || exit 0
upgrade_dir=/var/lib/lidza-agent/upgrade
request=$upgrade_dir/request
[[ -d "$upgrade_dir" && ! -L "$upgrade_dir" && -f "$request" && ! -L "$request" ]] || exit 0
version=$(cat "$request")
rm -f "$request"
# lidza-ssh-access-v1
if [[ "$version" == swap-check ]];then ensure_swap;exit $?;fi
# Fixed operation: no caller-supplied commands, account names or filesystem paths.
if [[ "$version" == ssh-keys ]]; then
 exec /usr/local/bin/lidza-agent -apply-ssh-keys
fi
mkdir -p "$upgrade_dir"
report(){ status_tmp=$(mktemp /var/tmp/lidza-upgrade-status.XXXXXX);printf '{"state":"%s","message":"%s"}\n' "$1" "$2" > "$status_tmp";chown "$(stat -c %u:%g "$upgrade_dir")" "$status_tmp";chmod 0600 "$status_tmp";mv -T "$status_tmp" "$upgrade_dir/status.json"; }
# lidza-console-root-v1
# Migrate agents installed before the MFA-gated root console existed. A fixed
# systemd override is used so no command or path supplied by a caller reaches
# this privileged helper.
if [[ "$version" == console-root ]];then
 service_dir=/etc/systemd/system/lidza-agent.service.d
 override=$service_dir/20-root-console.conf
 mkdir -p "$service_dir"
 override_tmp=$(mktemp "$service_dir/.20-root-console.XXXXXX")
 printf '%s\n' '[Service]' 'User=root' 'Group=root' 'SupplementaryGroups=' 'ProtectSystem=false' 'ProtectHome=false' 'PrivateTmp=false' > "$override_tmp"
 chmod 0644 "$override_tmp"
 mv -T "$override_tmp" "$override"
 systemctl daemon-reload
 systemctl restart lidza-agent
 healthy=false
 for attempt in {1..60};do
  if curl --max-time 2 -fsS http://127.0.0.1:9090/health >/dev/null;then healthy=true;break;fi
  sleep 1
 done
 if ! $healthy;then
  rm -f "$override"
  systemctl daemon-reload
  systemctl restart lidza-agent || true
  report failed 'Root console service migration failed and was rolled back.'
  exit 1
 fi
 # The retained state owner no longer runs the service and does not need the
 # root-equivalent Docker group after the migration.
 gpasswd -d lidza-agent docker >/dev/null 2>&1 || true
 report succeeded 'Root console service enabled; MFA is required for every unlock.'
 exit 0
fi
# lidza-control-boot-v1
# Hosts upgrading from an older helper do not receive the new control unit on
# that first upgrade. The newly installed agent queues this fixed drop-in after
# the new helper is present, closing that one-release migration gap.
if [[ "$version" == control-boot ]];then
 unit=/etc/systemd/system/lidza-control.service
 service_dir=/etc/systemd/system/lidza-control.service.d
 override=$service_dir/20-boot.conf
 [[ -f "$unit" && ! -L "$unit" ]] || { report failed 'Control panel service is not installed.';exit 1; }
 mkdir -p "$service_dir"
 [[ ! -L "$service_dir" ]] || { report failed 'Control panel service override directory is unsafe.';exit 1; }
 previous=false
 if [[ -f "$override" && ! -L "$override" ]];then cp -p "$override" "$override.previous";previous=true
 elif [[ -e "$override" ]];then report failed 'Control panel boot override path is unsafe.';exit 1
 fi
 override_tmp=$(mktemp "$service_dir/.20-boot.XXXXXX")
 printf '%s\n' '# lidza-control-boot-v1' '[Unit]' 'Wants=docker.service' 'After=docker.service' 'StartLimitIntervalSec=0' '[Service]' 'Restart=always' 'RestartSec=5' > "$override_tmp"
 chmod 0644 "$override_tmp"
 mv -T "$override_tmp" "$override"
 systemctl daemon-reload
 systemctl enable lidza-control
 systemctl restart lidza-control
 healthy=false
 for attempt in {1..120};do
  if curl --max-time 2 -fsS http://127.0.0.1:3000/readyz >/dev/null;then healthy=true;break;fi
  sleep 1
 done
 if ! $healthy;then
  if $previous;then mv -T "$override.previous" "$override";else rm -f "$override";fi
  systemctl daemon-reload
  systemctl restart lidza-control || true
  report failed 'Control panel boot migration failed and was rolled back; inspect journalctl -u lidza-control.'
  exit 1
 fi
 rm -f "$override.previous"
 report succeeded 'Control panel now waits for Docker and automatically recovers after reboot.'
 exit 0
fi
# lidza-control-move-v1
# Control panel moves. The agent stages inputs in <agent data dir>/control-move
# and queues one of these fixed operation names; paths are fixed here and every
# staged input is checked (regular files, no symbolic links) before use.
case "$version" in control-apply|control-remove|control-certificate|control-caddyfile|control-stop)
 stage=$(dirname "$upgrade_dir")/control-move
 control_report(){ local tmp;tmp=$(mktemp "$stage/.status.XXXXXX");printf '{"action":"%s","state":"%s","message":"%s"}\n' "$version" "$1" "$2" > "$tmp";chown "$(stat -c %u:%g "$stage")" "$tmp";chmod 0600 "$tmp";mv -T "$tmp" "$stage/status.json"; }
 control_fail(){ control_report failed "$1";exit 1; }
 [[ -d "$stage" && ! -L "$stage" ]] || exit 1
 if [[ -n $(find "$stage" -type l -print -quit) ]];then control_fail 'Staged control panel files may not contain symbolic links.';fi
 control_marker=/etc/lidza-control/.moved-in
 control_dir=/var/lib/lidza-control
 control_db="lidza-control-db-$(printf '%s' "$control_dir" | sha256sum | cut -c1-12)"
 control_ready(){ for _ in {1..120};do curl --max-time 2 -fsS http://127.0.0.1:3000/readyz >/dev/null 2>&1 && return 0;sleep 1;done;return 1; }
 control_report running 'Working'
 case "$version" in
 control-apply)
  [[ -f "$stage/bin/lidza-control" && -f "$stage/lidza-control.service" && -f "$stage/control.env" ]] || control_fail 'The control panel binary, unit or environment is not staged.'
  if [[ -e /etc/systemd/system/lidza-control.service && ! -f "$control_marker" ]];then control_fail 'A control panel that no move installed already runs on this server.';fi
  grep -q 'lidza-control' "$stage/lidza-control.service" || control_fail 'The staged unit is not the control panel unit.'
  id lidza-control >/dev/null 2>&1 || useradd --system --home-dir "$control_dir" --no-create-home --shell /usr/sbin/nologin lidza-control
  if getent group docker >/dev/null;then usermod -aG docker lidza-control;fi
  install -d -m 0750 -o root -g lidza-control /etc/lidza-control
  [[ -f "$control_marker" ]] || printf 'Installed by a control panel move.\n' > "$control_marker"
  install -m 0600 -o root -g root "$stage/control.env" /etc/lidza-control/control.env
  install -m 0755 -o root -g root "$stage/bin/lidza-control" /usr/local/bin/lidza-control
  install -m 0644 -o root -g root "$stage/lidza-control.service" /etc/systemd/system/lidza-control.service
  if [[ -d "$stage/data" ]];then
   systemctl stop lidza-control >/dev/null 2>&1 || true
   next=$(mktemp -d /var/lib/.lidza-control.XXXXXX)
   (cd "$stage/data" && find . -type d -print0) | while IFS= read -r -d '' d;do install -d -m 0700 -o lidza-control -g lidza-control "$next/$d";done
   (cd "$stage/data" && find . -type f -print0) | while IFS= read -r -d '' f;do install -m 0600 -o lidza-control -g lidza-control "$stage/data/$f" "$next/$f";done
   chmod 0700 "$next";chown lidza-control:lidza-control "$next"
   if [[ -d "$control_dir" ]];then rm -rf "$control_dir.previous";mv -T "$control_dir" "$control_dir.previous";fi
   mv -T "$next" "$control_dir"
   rm -rf "$control_dir.previous" "$stage/data"
  else
   install -d -m 0700 -o lidza-control -g lidza-control "$control_dir"
  fi
  systemctl daemon-reload
  systemctl enable lidza-control >/dev/null 2>&1
  systemctl restart lidza-control
  control_ready || control_fail 'The control panel did not answer on 127.0.0.1:3000; inspect journalctl -u lidza-control.'
  control_report succeeded 'The control panel is installed and answering.'
  exit 0;;
 control-remove)
  [[ -f "$control_marker" ]] || { [[ -e /etc/systemd/system/lidza-control.service ]] && control_fail 'This control panel was not installed by a move; nothing was removed.';control_report succeeded 'Nothing to remove.';exit 0; }
  systemctl disable --now lidza-control >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/lidza-control.service
  systemctl daemon-reload
  if command -v docker >/dev/null;then
   label=$(docker inspect --format '{{index .Config.Labels "io.lidza.control.database"}}' "$control_db" 2>/dev/null || true)
   if [[ "$label" == "${control_db#lidza-control-db-}" ]];then docker rm -f "$control_db" >/dev/null 2>&1 || true;docker volume rm "$control_db" >/dev/null 2>&1 || true;fi
  fi
  if [[ -f "$stage/cert-storage" && -d "$stage/cert" ]];then
   storage=$(head -n1 "$stage/cert-storage")
   if [[ "$storage" == /* && -d "$storage/certificates" ]];then (cd "$stage/cert" && find . -mindepth 2 -maxdepth 2 -type d -print0) | while IFS= read -r -d '' d;do rm -rf "${storage:?}/certificates/${d#./}";done;fi
  fi
  rm -rf "$control_dir" /etc/lidza-control /usr/local/bin/lidza-control
  userdel lidza-control >/dev/null 2>&1 || true
  control_report succeeded 'The control panel installed by the move was removed.'
  exit 0;;
 control-certificate)
  storage=$(head -n1 "$stage/cert-storage" 2>/dev/null || true)
  [[ "$storage" =~ ^/[A-Za-z0-9._/-]+/caddy$ && -d "$storage" && ! -L "$storage" && -d "$stage/cert" ]] || control_fail 'Caddy storage or the staged certificate is missing.'
  owner=$(stat -c %U:%G "$storage")
  (cd "$stage/cert" && find . -type d -print0) | while IFS= read -r -d '' d;do install -d -m 0700 -o "${owner%%:*}" -g "${owner##*:}" "$storage/certificates/$d";done
  (cd "$stage/cert" && find . -type f -print0) | while IFS= read -r -d '' f;do install -m 0600 -o "${owner%%:*}" -g "${owner##*:}" "$stage/cert/$f" "$storage/certificates/$f";done
  control_report succeeded 'The certificate is in Caddy storage.'
  exit 0;;
 control-caddyfile)
  target=/etc/caddy/Caddyfile
  [[ -f "$target" && ! -L "$target" && -f "$stage/Caddyfile.next" ]] || control_fail 'The Caddyfile or its staged change is missing.'
  command -v caddy >/dev/null || control_fail 'caddy is not installed.'
  candidate=/etc/caddy/.Caddyfile.lidza-next
  install -m 0644 "$stage/Caddyfile.next" "$candidate"
  if ! caddy validate --adapter caddyfile --config "$candidate" >/dev/null 2>&1;then rm -f "$candidate";control_fail 'caddy validate refused the changed Caddyfile; nothing was changed.';fi
  cp -p "$target" "$target.lidza-previous"
  install -m "$(stat -c %a "$target")" -o "$(stat -c %u "$target")" -g "$(stat -c %g "$target")" "$candidate" "$target"
  rm -f "$candidate" "$stage/Caddyfile.next"
  if ! systemctl reload caddy;then mv -T "$target.lidza-previous" "$target";systemctl reload caddy || true;control_fail 'Caddy did not reload the changed Caddyfile; the previous one was restored.';fi
  control_report succeeded 'Caddy reloaded the changed Caddyfile.'
  exit 0;;
 control-stop)
  [[ -f "$control_dir/control-moved.json" ]] || control_fail 'The control panel here has not handed over; it was not stopped.'
  systemctl disable --now lidza-control >/dev/null 2>&1 || control_fail 'The control panel unit could not be stopped.'
  control_report succeeded 'The control panel unit is stopped and disabled.'
  exit 0;;
 esac;;
esac
# lidza-signed-bundle-v1
# A signed distribution bundle was staged by the agent after it verified the
# manifest. The installed binary verifies the staged files again here, with
# the keys compiled into it, before anything is replaced. Only the two fixed
# managed layouts are touched: the agent and the control panel, each when its
# unit exists on this host.
has_unit(){ systemctl cat "$1.service" >/dev/null 2>&1; }
install_signed_bundle(){
 # State read by the EXIT trap stays global: the trap runs after the function frame is gone.
 local version=$1 staged verifier verified=false rc
 bundle_tmp='';changed=false;agent_target='';control_target=''
 staged=$upgrade_dir/staged/$version
 [[ -d "$staged" && ! -L "$staged" ]] || { report failed 'Signed bundle is not staged on this server.';return 1; }
 for verifier in /usr/local/bin/lidza-agent /usr/local/bin/lidza-control;do
  [[ -x "$verifier" && ! -L "$verifier" ]] || continue
  # Capture the command's own status: after a failed "if" condition $? is the if statement's (zero).
  "$verifier" -verify-bundle "$staged" >/dev/null 2>&1 && { verified=true;break; }
  rc=$?
  # Exit 2 means the installed binary predates signed bundles; try the next one.
  ((rc==2)) || { report failed 'Staged bundle failed signature or digest verification by the installed binary.';return 1; }
 done
 $verified || { report failed 'No installed binary can verify signed bundles yet; install this release manually once.';return 1; }
 report upgrading 'Verified signed bundle; installing'
 bundle_tmp=$(mktemp -d /var/tmp/lidza-upgrade.XXXXXX)
 rollback_signed(){
  if $changed;then
   [[ -z "$agent_target" ]] || { install -m 0755 /usr/local/bin/lidza-agent.previous /usr/local/bin/lidza-agent.rollback;mv /usr/local/bin/lidza-agent.rollback /usr/local/bin/lidza-agent;systemctl restart lidza-agent || true; }
   [[ -z "$control_target" ]] || { install -m 0755 /usr/local/bin/lidza-control.previous /usr/local/bin/lidza-control.rollback;mv /usr/local/bin/lidza-control.rollback /usr/local/bin/lidza-control;systemctl restart lidza-control || true; }
   report failed 'Signed bundle upgrade failed; previous managed binaries restored. Check service logs.'
  else
   report failed 'Signed bundle upgrade failed before replacing binaries. Check service logs.'
  fi
 }
 finish_signed(){ local result=$?;trap - EXIT;if ((result!=0));then rollback_signed;fi;rm -rf "$bundle_tmp";exit "$result"; }
 trap finish_signed EXIT
 if tar -tzf "$staged/bundle.tar.gz" | grep -Eq '(^/|(^|/)\.\.(/|$))';then exit 1;fi
 mkdir "$bundle_tmp/bundle"
 tar --no-same-owner -xzf "$staged/bundle.tar.gz" -C "$bundle_tmp/bundle"
 (cd "$bundle_tmp/bundle" && sha256sum -c SHA256SUMS >/dev/null)
 [[ $("$bundle_tmp/bundle/bin/lidza-agent" -version) == "$version" ]]
 [[ $("$bundle_tmp/bundle/bin/lidza-control" -version) == "$version" ]]
 if has_unit lidza-agent;then agent_target=/usr/local/bin/lidza-agent;fi
 if has_unit lidza-control;then control_target=/usr/local/bin/lidza-control;fi
 [[ -n "$agent_target$control_target" ]] || { echo 'No managed unit found on this host.' >&2;exit 1; }
 [[ -z "$agent_target" ]] || install -m 0755 "$agent_target" "$agent_target.previous"
 [[ -z "$control_target" ]] || install -m 0755 "$control_target" "$control_target.previous"
 [[ -z "$agent_target" ]] || install -m 0755 "$bundle_tmp/bundle/bin/lidza-agent" "$agent_target.next"
 [[ -z "$control_target" ]] || install -m 0755 "$bundle_tmp/bundle/bin/lidza-control" "$control_target.next"
 changed=true
 [[ -z "$agent_target" ]] || { mv "$agent_target.next" "$agent_target";systemctl restart lidza-agent; }
 [[ -z "$control_target" ]] || { mv "$control_target.next" "$control_target";systemctl restart lidza-control; }
 local healthy=false attempt
 for attempt in {1..90};do
  if { [[ -z "$agent_target" ]] || curl --max-time 2 -fsS http://127.0.0.1:9090/health >/dev/null; } \
   && { [[ -z "$control_target" ]] || curl --max-time 2 -fsS http://127.0.0.1:3000/readyz >/dev/null; };then healthy=true;break;fi
  sleep 1
 done
 $healthy
 changed=false
 # Keep the helper current for the next release; units are reviewed changes, not replaced here.
 if [[ -f "$bundle_tmp/bundle/deploy/upgrade-agent.sh" ]];then
  install -m 0755 "$bundle_tmp/bundle/deploy/upgrade-agent.sh" /usr/local/libexec/lidza-agent-upgrade.next
  mv /usr/local/libexec/lidza-agent-upgrade.next /usr/local/libexec/lidza-agent-upgrade
 fi
 rm -rf "$staged"
 report succeeded "Signed bundle $version installed; health checks passed"
 trap - EXIT
 rm -rf "$bundle_tmp"
 return 0
}
if [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-private\.[0-9]+$ ]];then install_signed_bundle "$version";exit $?;fi
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
  units_restored=false
  if [[ -f /etc/systemd/system/lidza-agent.service.previous ]];then install -m 0644 /etc/systemd/system/lidza-agent.service.previous /etc/systemd/system/lidza-agent.service;units_restored=true;fi
  if $control && [[ -f /etc/systemd/system/lidza-control.service.previous ]];then install -m 0644 /etc/systemd/system/lidza-control.service.previous /etc/systemd/system/lidza-control.service;units_restored=true;fi
  if $units_restored;then systemctl daemon-reload;fi
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
if [[ -f /etc/systemd/system/lidza-agent.service ]];then install -m 0644 /etc/systemd/system/lidza-agent.service /etc/systemd/system/lidza-agent.service.previous;fi
if $control && [[ -f /etc/systemd/system/lidza-control.service ]];then install -m 0644 /etc/systemd/system/lidza-control.service /etc/systemd/system/lidza-control.service.previous;fi
install -m 0755 "$upgrade_tmp/bundle/bin/lidza-agent" /usr/local/bin/lidza-agent.next
if $control;then install -m 0755 "$upgrade_tmp/bundle/bin/lidza-control" /usr/local/bin/lidza-control.next;fi
if [[ -f "$upgrade_tmp/bundle/deploy/lidza-agent.service" ]];then install -m 0644 "$upgrade_tmp/bundle/deploy/lidza-agent.service" /etc/systemd/system/lidza-agent.service.next;fi
if $control && [[ -f "$upgrade_tmp/bundle/deploy/lidza-control.service" ]];then install -m 0644 "$upgrade_tmp/bundle/deploy/lidza-control.service" /etc/systemd/system/lidza-control.service.next;fi
changed=true
mv /usr/local/bin/lidza-agent.next /usr/local/bin/lidza-agent
if $control;then mv /usr/local/bin/lidza-control.next /usr/local/bin/lidza-control;fi
units_changed=false
if [[ -f /etc/systemd/system/lidza-agent.service.next ]];then mv /etc/systemd/system/lidza-agent.service.next /etc/systemd/system/lidza-agent.service;units_changed=true;fi
if $control && [[ -f /etc/systemd/system/lidza-control.service.next ]];then mv /etc/systemd/system/lidza-control.service.next /etc/systemd/system/lidza-control.service;units_changed=true;fi
if $units_changed;then systemctl daemon-reload;fi
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
