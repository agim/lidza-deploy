#!/usr/bin/env bash
# Root service: only official GitHub release assets may replace managed binaries.
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
