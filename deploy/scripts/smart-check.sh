#!/usr/bin/env bash
# smart-check.sh - daily SMART health check of both USB disks.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: smart-check.sh [--dry-run]

Asks both disks for their SMART self-assessment and alerts when:
  - a disk reports failing health
  - reallocated, pending or uncorrectable sector counts have grown since the
    last check (the classic early warning of a dying hard disk)
  - a disk is missing (unplugged, dead enclosure, dead power supply)

The previous values are kept in /var/lib/home-driver/smart/<role>.state.
USB enclosures often hide SMART; the script tries plain access first and then
`-d sat`. Set BACKUP_SMART_DEVICE_TYPE in backup.env if yours needs another type.
If the enclosure passes no SMART at all you are told once, not every day.

Options:
  --dry-run    show which devices would be checked; change nothing
  -h, --help   this text

Exit status: 0 healthy; 1 a disk is failing, degrading or missing (alert sent).
Normally started by home-driver-smart-check.timer every morning.
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

hd_alert_on_unexpected_exit
hd_load_env "$HD_ETC_DIR/backup.env"
DEVTYPE=${BACKUP_SMART_DEVICE_TYPE:-auto}
hd_require_root
hd_require_cmd smartctl lsblk findmnt
hd_lock smart-check

STATE_DIR="$HD_STATE_DIR/smart"
ALERTS=()      # things that make this run fail
NOTICES=()     # things worth one low-priority message

# device_for <dir> <label>: whole-disk device behind a mountpoint, else by label.
device_for() {
  local dir=$1 label=$2 src="" parent
  if mountpoint -q "$dir" 2>/dev/null; then
    src=$(findmnt -n -o SOURCE -M "$dir" 2>/dev/null || true)
    src=${src%%\[*}        # btrfs shows /dev/sdb[/@data]
  fi
  if [[ -z "$src" ]]; then
    src=$(lsblk -nrpo NAME,LABEL 2>/dev/null | awk -v l="$label" '$2 == l {print $1; exit}' || true)
  fi
  [[ -n "$src" ]] || return 1
  parent=$(lsblk -dnpo PKNAME "$src" 2>/dev/null | head -n 1 || true)
  [[ -n "$parent" ]] && src=$parent
  printf '%s\n' "$src"
}

# read_smart <dev>: prints smartctl output; returns 1 when SMART is unreadable.
read_smart() {
  local dev=$1 out rc t
  local -a types=()
  if [[ "$DEVTYPE" == auto ]]; then types=("" sat); else types=("$DEVTYPE"); fi
  for t in "${types[@]}"; do
    rc=0
    if [[ -n "$t" ]]; then
      out=$(LC_ALL=C smartctl -i -H -A -d "$t" "$dev" 2>&1) || rc=$?
    else
      out=$(LC_ALL=C smartctl -i -H -A "$dev" 2>&1) || rc=$?
    fi
    # smartctl's exit status is a bit mask: bits 0-1 (value 1, 2) mean the
    # command line was wrong or the device could not be opened. Higher bits
    # describe disk problems and still come with usable output.
    if (( (rc & 3) == 0 )) && grep -q "overall-health" <<<"$out"; then
      printf '%s\n' "$out"
      return 0
    fi
  done
  return 1
}

attr() { awk -v id="$1" '$1 == id {print $10; exit}' <<<"$2"; }

# check_one <role> <dir> <label>
check_one() {
  local role=$1 dir=$2 label=$3 dev out state_file
  state_file="$STATE_DIR/$role.state"
  if ! dev=$(device_for "$dir" "$label"); then
    ALERTS+=("$role disk (label $label) is MISSING: not mounted and not detected. Check the USB cable and the disk's power supply.")
    return 0
  fi
  if ! mountpoint -q "$dir" 2>/dev/null; then
    ALERTS+=("$role disk is plugged in ($dev) but NOT MOUNTED at $dir. Try: sudo mount $dir")
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would run: smartctl -i -H -A $dev (device type: $DEVTYPE)"
    return 0
  fi
  mkdir -p "$STATE_DIR"
  local old_health old_serial old_realloc old_pending old_uncorr
  old_health=$(hd_status_get "$state_file" health)
  old_serial=$(hd_status_get "$state_file" serial)
  old_realloc=$(hd_status_get "$state_file" reallocated)
  old_pending=$(hd_status_get "$state_file" pending)
  old_uncorr=$(hd_status_get "$state_file" uncorrectable)

  if ! out=$(read_smart "$dev"); then
    if [[ "$old_health" != "UNREADABLE" ]]; then
      NOTICES+=("Cannot read SMART data of the $role disk ($dev): the USB enclosure probably does not pass it through. Disk health is NOT being monitored for this disk; the monthly scrub still is. Try another BACKUP_SMART_DEVICE_TYPE, see: smartctl --scan-open")
    fi
    printf '%s\n' "checked_epoch=$(date +%s)" "device=$dev" "health=UNREADABLE" >"$state_file"
    hd_warn "$role disk ($dev): SMART unreadable"
    return 0
  fi

  local health serial model realloc pending uncorr
  health=$(sed -n 's/^SMART overall-health self-assessment test result: *//p' <<<"$out" | head -n 1)
  serial=$(sed -n 's/^Serial Number: *//p' <<<"$out" | head -n 1)
  model=$(sed -n 's/^Device Model: *//p' <<<"$out" | head -n 1)
  realloc=$(attr 5 "$out")
  pending=$(attr 197 "$out")
  uncorr=$(attr 198 "$out")

  if [[ "$health" != "PASSED" ]]; then
    ALERTS+=("$role disk ($dev, $model) reports SMART health '${health:-unknown}'. It is FAILING: replace it now. See RUNBOOK.md.")
  fi

  # A different serial number means the disk was replaced: start a new baseline.
  local same_disk=1
  [[ -n "$old_serial" && "$old_serial" != "$serial" ]] && same_disk=0
  local name new old
  for name in reallocated pending uncorrectable; do
    case "$name" in
      reallocated) new=$realloc; old=$old_realloc ;;
      pending) new=$pending; old=$old_pending ;;
      *) new=$uncorr; old=$old_uncorr ;;
    esac
    hd_is_uint "$new" || continue      # attribute not reported by this disk
    if [[ $same_disk == 1 ]] && hd_is_uint "$old"; then
      if (( new > old )); then
        ALERTS+=("$role disk ($dev): $name sectors grew from $old to $new. The disk is degrading: plan to replace it. See RUNBOOK.md.")
      fi
    elif (( new > 0 )); then
      NOTICES+=("$role disk ($dev): $name sectors = $new on first check. Not growing yet, but this disk is not in perfect health.")
    fi
  done

  printf '%s\n' "checked_epoch=$(date +%s)" "device=$dev" "model=$model" "serial=$serial" \
    "health=${health:-unknown}" "reallocated=$realloc" "pending=$pending" "uncorrectable=$uncorr" >"$state_file.tmp"
  mv -f "$state_file.tmp" "$state_file"
  hd_log "$role disk ($dev): health=${health:-unknown} reallocated=${realloc:-n/a} pending=${pending:-n/a} uncorrectable=${uncorr:-n/a}"
}

check_one data "$HD_DATA_DIR" "$HD_LABEL_DATA"
check_one backup "$HD_BACKUP_DIR" "$HD_LABEL_BACKUP"

if (( ${#NOTICES[@]} > 0 )); then
  hd_notify "disk notice" "$(printf '%s ' "${NOTICES[@]}")" default
fi
if (( ${#ALERTS[@]} > 0 )); then
  hd_fail "$(printf '%s ' "${ALERTS[@]}")"
fi
hd_log "SMART check OK"
exit 0
