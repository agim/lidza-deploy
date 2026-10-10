#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d /tmp/lidza-swap-test.XXXXXX)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch"/{mock,etc,proc,run,var/lib/lidza-agent,var/tmp}
python3 - "$scratch" <<'PY'
import pathlib,sys
r=pathlib.Path(sys.argv[1]);s=pathlib.Path('scripts/upgrade-agent.sh').read_text()
for path in ['/var/lib','/var/tmp','/run','/etc/fstab','/proc/swaps']:
 s=s.replace(path,str(r)+path)
s=s.replace('[[ $EUID == 0 ]]','true')
(r/'helper').write_text(s);(r/'helper').chmod(0o755)
PY
cat > "$scratch/mock/tool" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf '%s %s\n' "${0##*/}" "$*" >> "$SWAP_FIXTURE/actions"
case ${0##*/} in
 findmnt) echo "${SWAP_FS:-ext4}";;
 df) printf 'Filesystem 1024-blocks Used Available Capacity Mounted\n/dev/test 20000000 1000 %s 1%% /\n' "${SWAP_FREE:-10000000}";;
 dd)for arg;do case "$arg" in of=*)printf allocated > "${arg#of=}";;esac;done;;
 mkswap)printf swap > "$1";;
 swapon) [[ ${SWAP_FAIL:-0} != 1 ]] || exit 1;if [[ "$1" == -a ]];then printf '/existing/swap file 1024 0 -2\n' >> "$SWAP_FIXTURE/proc/swaps";else printf '%s file 4194304 0 -2\n' "$1" >> "$SWAP_FIXTURE/proc/swaps";fi;;
 blkid)cat "${@: -1}";;
 stat)echo 0:600:1;;
 id|chown)exit 0;;
esac
MOCK
chmod +x "$scratch/mock/tool"
for t in findmnt df dd mkswap swapon blkid stat id chown;do ln -s tool "$scratch/mock/$t";done
export SWAP_FIXTURE="$scratch" PATH="$scratch/mock:$PATH"
reset(){ printf 'Filename Type Size Used Priority\n' > "$scratch/proc/swaps";: > "$scratch/etc/fstab";: > "$scratch/actions";rm -f "$scratch/var/lib/lidza-deploy.swap"; }
reset
"$scratch/helper" --ensure-swap
[[ $(cat "$scratch/var/lib/lidza-deploy.swap") == swap ]]
[[ $(grep -c 'none swap sw' "$scratch/etc/fstab") == 1 ]]
rg -q 'Created and activated' "$scratch/var/lib/lidza-agent/swap-status.json"
"$scratch/helper" --ensure-swap
[[ $(grep -c '^dd ' "$scratch/actions") == 1 ]]
[[ $(grep -c 'none swap sw' "$scratch/etc/fstab") == 1 ]]
# Repair persistence after an interrupted allocation, preserving the active file.
: > "$scratch/etc/fstab"
"$scratch/helper" --ensure-swap
[[ $(grep -c 'none swap sw' "$scratch/etc/fstab") == 1 ]]
# Existing foreign active swap is kept; no new backing file or fstab changes.
reset
printf '/existing/swap file 1024 0 -2\n' >> "$scratch/proc/swaps"
"$scratch/helper" --ensure-swap
[[ ! -e "$scratch/var/lib/lidza-deploy.swap" && ! -s "$scratch/etc/fstab" ]]
! rg -q '^dd |^swapon ' "$scratch/actions"
# Configured but inactive swap is activated rather than duplicated.
reset
printf '/existing/swap none swap sw 0 0\n' > "$scratch/etc/fstab"
"$scratch/helper" --ensure-swap
[[ ! -e "$scratch/var/lib/lidza-deploy.swap" ]]
rg -q '^swapon -a$' "$scratch/actions"
# Refuse to overwrite files and symlinks at the reserved path.
for kind in file symlink;do
 reset
 if [[ $kind == file ]];then printf important > "$scratch/var/lib/lidza-deploy.swap";else printf important > "$scratch/original";ln -s "$scratch/original" "$scratch/var/lib/lidza-deploy.swap";fi
 if "$scratch/helper" --ensure-swap;then echo 'Unsafe path accepted' >&2;exit 1;fi
 [[ $(cat "$scratch/var/lib/lidza-deploy.swap") == important ]]
 ! rg -q '^dd |^mkswap |^swapon ' "$scratch/actions"
done
reset
if SWAP_FREE=1000 "$scratch/helper" --ensure-swap;then echo 'Low disk accepted' >&2;exit 1;fi
[[ ! -e "$scratch/var/lib/lidza-deploy.swap" ]]
reset
if SWAP_FS=btrfs "$scratch/helper" --ensure-swap;then echo 'Unsupported filesystem accepted' >&2;exit 1;fi
reset
if SWAP_FAIL=1 "$scratch/helper" --ensure-swap;then echo 'Failed activation accepted' >&2;exit 1;fi
[[ ! -e "$scratch/var/lib/lidza-deploy.swap" && ! -s "$scratch/etc/fstab" ]]
rg -q '"state":"failed"' "$scratch/var/lib/lidza-agent/swap-status.json"
# Retry with a valid preformatted managed file repairs persistence.
reset
printf swap > "$scratch/var/lib/lidza-deploy.swap";chmod 0600 "$scratch/var/lib/lidza-deploy.swap"
"$scratch/helper" --ensure-swap
! rg -q '^dd |^mkswap ' "$scratch/actions"
[[ $(grep -c 'none swap sw' "$scratch/etc/fstab") == 1 ]]
echo 'PASS: swap allocation, active/configured preservation, repeatability, reserved-file protection, disk/filesystem checks, activation failure cleanup and persistence'
