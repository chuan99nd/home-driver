#!/usr/bin/env bash
# setup-disks.sh - format ONE disk as a home-driver disk. DESTROYS everything on it.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: setup-disks.sh --device /dev/sdX --role data|backup --yes-wipe-this-disk [options]

Formats ONE disk for home-driver. EVERYTHING ON THAT DISK IS ERASED.

  --role data     Disk A: btrfs label hd-data,   subvolume @data
  --role backup   Disk B: btrfs label hd-backup, subvolume @backup with
                  current/ (nightly mirror) and snapshots/ (read-only history)

Each disk also gets a marker file (.home-driver-disk). Every other script
checks that marker before reading or writing, so an unplugged disk can never
be mistaken for an empty one.

Required, all three, every time. There are NO defaults:
  --device /dev/sdX        the whole disk to erase. Find it with:  lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN
  --role data|backup
  --yes-wipe-this-disk     confirms you understand the disk will be erased

Options:
  --dry-run                show the disk and the plan; erase nothing
  --allow-non-usb          allow a disk that is not connected over USB
  --overwrite-home-driver-disk
                           allow erasing a disk that already carries a home-driver
                           label (hd-data / hd-backup). Only for a disk you are
                           deliberately re-using; triple-check the serial number.
  -h, --help               this text

The script always shows the disk's model, size, serial number and current
contents first, and REFUSES when:
  - the disk holds the running system (/, /boot, swap)
  - anything on the disk is mounted
  - another connected disk already has the label it would create
  - the disk already is a home-driver disk (without the overwrite flag)
  - the disk is not USB (without --allow-non-usb)

Example (new backup disk):
  lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN
  sudo setup-disks.sh --device /dev/sdc --role backup --dry-run
  sudo setup-disks.sh --device /dev/sdc --role backup --yes-wipe-this-disk
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DEVICE=""
ROLE=""
CONFIRMED=0
ALLOW_NON_USB=0
OVERWRITE_HD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --device) DEVICE=${2:-}; shift ;;
    --role) ROLE=${2:-}; shift ;;
    --yes-wipe-this-disk) CONFIRMED=1 ;;
    --dry-run) HD_DRY_RUN=1 ;;
    --allow-non-usb) ALLOW_NON_USB=1 ;;
    --overwrite-home-driver-disk) OVERWRITE_HD=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

refuse() { hd_err "REFUSING: $*"; hd_err "Nothing was changed."; exit 1; }

# ---- arguments: no defaults, ever ----
[[ -n "$DEVICE" ]] || { usage >&2; refuse "--device is required (there is no default device)"; }
[[ -n "$ROLE" ]] || refuse "--role data|backup is required"
case "$ROLE" in
  data) LABEL=$HD_LABEL_DATA; SUBVOL=$HD_SUBVOL_DATA ;;
  backup) LABEL=$HD_LABEL_BACKUP; SUBVOL=$HD_SUBVOL_BACKUP ;;
  *) refuse "--role must be 'data' or 'backup', not '$ROLE'" ;;
esac
[[ "$DEVICE" == /dev/* ]] || refuse "--device must be a path under /dev, e.g. /dev/sdc"
if hd_is_test_mode && [[ "${HD_TEST_FAKE_BLOCKDEV:-0}" == 1 ]]; then
  : # tests cannot create block devices
else
  [[ -b "$DEVICE" ]] || refuse "$DEVICE is not a block device (is the disk plugged in? check with lsblk)"
  DEVICE=$(realpath "$DEVICE")
fi

hd_require_cmd lsblk findmnt awk

# ---- show the user exactly which disk this is ----
dev_type=$(lsblk -dnro TYPE "$DEVICE" 2>/dev/null | head -n 1 || true)
[[ "$dev_type" == "disk" ]] \
  || refuse "$DEVICE is of type '${dev_type:-unknown}'. Give the WHOLE disk (e.g. /dev/sdc), not a partition (e.g. /dev/sdc1)."

echo
echo "================ DISK TO BE ERASED ================"
lsblk -o NAME,SIZE,TYPE,TRAN,MODEL,SERIAL,FSTYPE,LABEL,MOUNTPOINT "$DEVICE" || refuse "cannot read information about $DEVICE"
echo "==================================================="
echo

# ---- refusals ----
# 1. the disk that holds the running system
mapfile -t tree < <(lsblk -nrpo NAME "$DEVICE")          # the disk and all its partitions
(( ${#tree[@]} > 0 )) || refuse "cannot list $DEVICE"
in_tree() { local x; for x in "${tree[@]}"; do [[ "$x" == "$1" ]] && return 0; done; return 1; }

system_sources=()
root_src=$(findmnt -n -o SOURCE / 2>/dev/null | head -n 1 || true)
root_src=${root_src%%\[*}
[[ -n "$root_src" ]] || refuse "cannot determine which disk holds the root filesystem; too risky to continue"
system_sources+=("$root_src")
for mp in /boot /boot/efi /home /var; do
  s=$(findmnt -n -o SOURCE -M "$mp" 2>/dev/null | head -n 1 || true)
  s=${s%%\[*}
  [[ -n "$s" ]] && system_sources+=("$s")
done
resolved_any=0
for s in "${system_sources[@]}"; do
  [[ "$s" == /dev/* ]] || continue
  # -s walks towards the physical disk: LVM volume -> LUKS -> partition -> disk.
  while IFS= read -r anc; do
    [[ -n "$anc" ]] || continue
    resolved_any=1
    in_tree "$anc" && refuse "$DEVICE holds the running system ($s is on it). This is the laptop's own disk."
  done < <(lsblk -snrpo NAME "$s" 2>/dev/null || true)
done
[[ $resolved_any == 1 ]] \
  || refuse "cannot work out which physical disk the system runs from (root is '$root_src'); too risky to continue"

# 2. anything mounted or used as swap
mounted=$(lsblk -nro MOUNTPOINT "$DEVICE" | awk 'NF' | tr '\n' ' ' || true)
[[ -z "${mounted// /}" ]] \
  || refuse "$DEVICE is in use (mounted at: $mounted). Stop the services and unmount it first: sudo umount <mountpoint>"

# 3. transport
tran=$(lsblk -dnro TRAN "$DEVICE" | head -n 1 || true)
if [[ "$tran" != "usb" && $ALLOW_NON_USB != 1 ]]; then
  refuse "$DEVICE is connected via '${tran:-unknown}', not USB. The home-driver disks are USB disks; an internal disk is almost certainly the wrong one. (--allow-non-usb overrides.)"
fi

# 4. it already is a home-driver disk
existing_labels=$(lsblk -nro LABEL "$DEVICE" | awk 'NF' | tr '\n' ' ' || true)
for l in $existing_labels; do
  if [[ "$l" == "$HD_LABEL_DATA" || "$l" == "$HD_LABEL_BACKUP" ]] && [[ $OVERWRITE_HD != 1 ]]; then
    refuse "$DEVICE already is a home-driver disk (label '$l'). It may hold your photos or your only backup. If you really mean to erase it, add --overwrite-home-driver-disk."
  fi
done

# 5. the label must stay unique, or mounting by LABEL becomes a coin toss
while read -r name label; do
  [[ "$label" == "$LABEL" ]] || continue
  in_tree "$name" && continue
  refuse "another connected disk ($name) already has the label '$LABEL'. Unplug it first, so that only one disk can ever be mounted as the $ROLE disk."
done < <(lsblk -nrpo NAME,LABEL)

# 6. the explicit confirmation flag
if [[ $CONFIRMED != 1 && "$HD_DRY_RUN" != 1 ]]; then
  refuse "this would ERASE the disk shown above. If that is the right disk (check model, size and serial), add --yes-wipe-this-disk"
fi

# ---- plan ----
echo "Plan for $DEVICE (role: $ROLE):"
echo "  1. wipefs -a            erase all partition tables and filesystem signatures"
echo "  2. mkfs.btrfs -L $LABEL    one btrfs filesystem on the whole disk (metadata duplicated)"
echo "  3. create subvolume $SUBVOL$([[ $ROLE == backup ]] && echo ', with current/ and snapshots/')"
echo "  4. write the marker file $HD_SENTINEL (role=$ROLE)"
echo

if [[ "$HD_DRY_RUN" == 1 ]]; then
  hd_log "[dry-run] all safety checks passed; nothing was changed"
  exit 0
fi

hd_require_root
hd_require_cmd wipefs mkfs.btrfs btrfs mount umount

if [[ -t 0 ]]; then
  read -r -p "LAST CHANCE. Type the device path ($DEVICE) to erase it: " answer
  [[ "$answer" == "$DEVICE" ]] || refuse "you typed '$answer', not '$DEVICE'"
fi

# ---- act ----
hd_log "erasing signatures on $DEVICE"
for (( i = ${#tree[@]} - 1; i >= 0; i-- )); do   # partitions first, the disk last
  wipefs -a "${tree[$i]}" >/dev/null
done
hd_log "creating btrfs filesystem labelled $LABEL"
# -m dup: two copies of the metadata, so one bad sector cannot lose the directory tree.
mkfs.btrfs -f -L "$LABEL" -m dup -d single "$DEVICE" >/dev/null
command -v udevadm >/dev/null 2>&1 && udevadm settle || true

mnt=$(mktemp -d)
cleanup() {
  mountpoint -q "$mnt" 2>/dev/null && umount "$mnt"
  rmdir "$mnt" 2>/dev/null || true
}
trap cleanup EXIT
mount -t btrfs "$DEVICE" "$mnt"
btrfs subvolume create "$mnt/$SUBVOL" >/dev/null
if [[ "$ROLE" == backup ]]; then
  btrfs subvolume create "$mnt/$SUBVOL/current" >/dev/null
  mkdir "$mnt/$SUBVOL/snapshots"
fi
cat >"$mnt/$SUBVOL/$HD_SENTINEL" <<EOF
# home-driver disk marker. DO NOT DELETE OR EDIT.
# The backup scripts refuse to run when this file is missing: that is how they
# tell the real disk from an empty mountpoint.
role=$ROLE
label=$LABEL
created=$(date '+%F %T')
EOF
chmod 444 "$mnt/$SUBVOL/$HD_SENTINEL"
sync
if ! hd_is_test_mode; then umount "$mnt"; fi

hd_log "done: $DEVICE is now the home-driver $ROLE disk (label $LABEL)"
target=$([[ $ROLE == data ]] && echo "$HD_DATA_DIR" || echo "$HD_BACKUP_DIR")
cat <<EOF

Next:
  - If install.sh has already been run:   sudo mount $target
    then check:                           sudo $HD_OPT_DIR/scripts/doctor.sh
  - Otherwise continue with install.sh; it adds the disk to /etc/fstab.
EOF
