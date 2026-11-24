#!/usr/bin/env bash
# scrub.sh - btrfs scrub of both disks: reads every block and verifies its checksum.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scrub.sh [--disk data|backup|all] [--dry-run]

Runs `btrfs scrub` on Disk A and Disk B, one after the other, and alerts if
any checksum, read or device error is found. This is how silent corruption
("bit rot") on an old hard disk is noticed while the other disk still has a
good copy. Takes several hours per disk; the server stays usable.

Options:
  --disk X     only scrub one disk (default: all)
  --dry-run    print the commands instead of running them
  -h, --help   this text

Exit status: 0 both disks clean; 1 any error or a disk is missing (alert sent).
Normally started by home-driver-scrub.timer on the first Sunday of the month.
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

WHICH=all
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --disk) WHICH=${2:-}; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done
case "$WHICH" in data|backup|all) ;; *) hd_die "--disk must be data, backup or all" ;; esac

hd_alert_on_unexpected_exit
hd_require_root
hd_require_cmd btrfs mountpoint flock
hd_lock scrub

PROBLEMS=()

# scrub_one <role> <dir>
scrub_one() {
  local role=$1 dir=$2 reason out rc=0 status_line summary stats
  if ! reason=$(hd_check_disk "$role" "$dir"); then
    PROBLEMS+=("$role disk not scrubbed: $reason")
    return 0
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would run: btrfs scrub start -B -d $dir ; btrfs scrub status $dir ; btrfs device stats $dir"
    return 0
  fi
  hd_log "scrubbing $role disk at $dir (this takes hours)"
  # -B: stay in the foreground until done. Exit status is non-zero when
  # uncorrectable errors were found; we do not rely on it alone.
  out=$(btrfs scrub start -B -d "$dir" 2>&1) || rc=$?
  hd_log "btrfs scrub start exit $rc"

  out=$(LC_ALL=C btrfs scrub status "$dir" 2>&1) || true
  status_line=$(sed -n 's/^Status: *//p' <<<"$out" | head -n 1)
  summary=$(sed -n 's/^Error summary: *//p' <<<"$out" | head -n 1)
  # Cumulative per-device error counters (read/write/flush/corruption/generation).
  stats=$(LC_ALL=C btrfs device stats "$dir" 2>&1 | awk '$2 != 0 {print $1 "=" $2}' | tr '\n' ' ') || true

  local result=ok
  # Be strict: anything we do not positively recognise as clean is a problem.
  if [[ $rc -ne 0 || "$status_line" != "finished" || "$summary" != "no errors found" ]]; then
    result=errors
    PROBLEMS+=("$role disk scrub: exit=$rc status='${status_line:-unknown}' errors='${summary:-unknown}'")
  fi
  if [[ -n "${stats// /}" ]]; then
    result=errors
    PROBLEMS+=("$role disk has non-zero btrfs error counters: $stats")
  fi
  hd_write_status "scrub-$role" "epoch=$(date +%s)" "result=$result" "status=${status_line:-unknown}" \
    "summary=${summary:-unknown}" "device_stats=${stats:-}"
  hd_log "$role disk scrub result: $result (${summary:-unknown})"
}

[[ "$WHICH" == all || "$WHICH" == data ]] && scrub_one data "$HD_DATA_DIR"
[[ "$WHICH" == all || "$WHICH" == backup ]] && scrub_one backup "$HD_BACKUP_DIR"

if (( ${#PROBLEMS[@]} > 0 )); then
  msg=$(printf '%s. ' "${PROBLEMS[@]}")
  hd_fail "${msg}Corrupted files are listed by: sudo journalctl -k | grep -i 'checksum error'. See RUNBOOK.md, 'An alert arrives'."
fi
hd_log "scrub OK"
exit 0
