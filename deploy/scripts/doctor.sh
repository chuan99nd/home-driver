#!/usr/bin/env bash
# doctor.sh - read-only health report. The first command to run when something seems off.
set -uo pipefail

usage() {
  cat <<'EOF'
Usage: doctor.sh [--quiet]

Read-only health report for home-driver. Changes nothing, sends nothing.
Run it first whenever something seems wrong (or paste its output to whoever
is helping you).

Checks:
  disks      both mounted, btrfs, marker file present, free space
  backups    age of the last successful local backup, newest snapshot,
             offsite backup (only if configured)
  scrub      result and age of the last scrub of each disk
  SMART      last result recorded by smart-check.sh for each disk
  services   Immich, Drive, curator running; timers scheduled; nothing failed
  network    ports 2283, 8090, 8091 listen on 127.0.0.1 only
  config     env files exist with mode 0600

Each line starts with OK, WARN or FAIL.
Exit status: 0 no FAIL lines; 1 at least one FAIL.

Options:
  --quiet      only print WARN and FAIL lines and the summary
  -h, --help   this text

Run with sudo for the complete picture:  sudo /opt/home-driver/scripts/doctor.sh
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh" || { echo "cannot load lib.sh" >&2; exit 1; }

QUIET=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quiet) QUIET=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

hd_load_env "$HD_ETC_DIR/backup.env" 2>/dev/null
MAX_AGE_H=${BACKUP_MAX_AGE_HOURS:-36}
WARN_PCT=${BACKUP_DISK_WARN_PERCENT:-80}
FAIL_PCT=${BACKUP_DISK_FAIL_PERCENT:-92}
hd_is_uint "$MAX_AGE_H" || MAX_AGE_H=36
hd_is_uint "$WARN_PCT" || WARN_PCT=80
hd_is_uint "$FAIL_PCT" || FAIL_PCT=92

N_OK=0; N_WARN=0; N_FAIL=0
ok()   { N_OK=$((N_OK + 1)); [[ $QUIET == 1 ]] || printf '  OK    %s\n' "$*"; }
warn() { N_WARN=$((N_WARN + 1)); printf '  WARN  %s\n' "$*"; }
fail() { N_FAIL=$((N_FAIL + 1)); printf '  FAIL  %s\n' "$*"; }
section() { [[ $QUIET == 1 ]] || printf '\n%s\n' "$*"; }
ago() { # <epoch> -> "5 h ago"
  local h; h=$(hd_age_hours "$1")
  if (( h < 48 )); then echo "$h h ago"; else echo "$((h / 24)) days ago"; fi
}

echo "home-driver doctor - $(date '+%F %T') - $(hostname 2>/dev/null || echo unknown)"
if [[ "$EUID" -ne 0 ]] && ! hd_is_test_mode; then
  warn "not running as root: some checks may be incomplete. Use: sudo $0"
fi

# ------------------------------------------------------------------- disks
section "Disks"
DISK_B_OK=0
check_disk_report() { # <role> <dir> <letter>
  local role=$1 dir=$2 letter=$3 reason pct avail
  if ! reason=$(hd_check_disk "$role" "$dir"); then
    fail "Disk $letter ($role): $reason"
    return 1
  fi
  ok "Disk $letter ($role) is mounted at $dir and carries its marker file"
  pct=$(df -P "$dir" 2>/dev/null | awk 'NR == 2 {gsub("%", "", $5); print $5}')
  avail=$(df -Ph "$dir" 2>/dev/null | awk 'NR == 2 {print $4}')
  if ! hd_is_uint "$pct"; then
    warn "Disk $letter: cannot read disk usage"
  elif (( pct >= FAIL_PCT )); then
    fail "Disk $letter is ${pct}% full (${avail} free). Free up space or get a bigger disk."
  elif (( pct >= WARN_PCT )); then
    warn "Disk $letter is ${pct}% full (${avail} free)"
  else
    ok "Disk $letter is ${pct}% full (${avail} free)"
  fi
  return 0
}
check_disk_report data "$HD_DATA_DIR" A || true
check_disk_report backup "$HD_BACKUP_DIR" B && DISK_B_OK=1

# ----------------------------------------------------------------- backups
section "Backups"
st="$HD_STATE_DIR/status/backup-local.status"
last=$(hd_status_get "$st" last_success_epoch)
result=$(hd_status_get "$st" result)
if ! hd_is_uint "$last"; then
  fail "no successful local backup recorded yet. Run: sudo $HD_OPT_DIR/scripts/backup-local.sh"
else
  age=$(hd_age_hours "$last")
  if (( age > MAX_AGE_H )); then
    fail "last successful local backup was $(ago "$last") (limit: $MAX_AGE_H h). See: journalctl -u home-driver-backup-local -n 50"
  else
    ok "last successful local backup: $(ago "$last")"
  fi
  if [[ "$result" == errors ]]; then
    fail "the most recent backup run had errors: $(hd_status_get "$st" errors)"
  fi
fi
if [[ $DISK_B_OK == 1 ]]; then
  n_snap=$(hd_list_snapshots | wc -l)
  newest=$(hd_latest_snapshot)
  if (( n_snap == 0 )); then
    fail "no snapshots on Disk B"
  else
    ok "$n_snap snapshots on Disk B, newest: $newest"
  fi
  if [[ -e "$HD_BACKUP_DIR/.sync-not-yet-snapshotted" ]]; then
    warn "the last backup run was interrupted before its snapshot (the next run repairs this)"
  fi
fi
if [[ -n "${RESTIC_REPOSITORY:-}" ]]; then
  st="$HD_STATE_DIR/status/backup-offsite.status"
  last=$(hd_status_get "$st" last_success_epoch)
  if ! hd_is_uint "$last"; then
    fail "offsite backup is configured but has never succeeded. Run: sudo $HD_OPT_DIR/scripts/backup-offsite.sh"
  elif (( $(hd_age_hours "$last") > MAX_AGE_H + 48 )); then
    fail "last successful offsite backup was $(ago "$last")"
  else
    ok "last successful offsite backup: $(ago "$last")"
  fi
else
  warn "offsite backup is not configured: a fire or theft would take both disks (optional; see INSTALL.md)"
fi

# ------------------------------------------------------------------- scrub
section "Scrub (silent corruption check)"
for role in data backup; do
  st="$HD_STATE_DIR/status/scrub-$role.status"
  e=$(hd_status_get "$st" epoch)
  r=$(hd_status_get "$st" result)
  if ! hd_is_uint "$e"; then
    warn "$role disk has never been scrubbed (runs on the first Sunday of the month; by hand: sudo $HD_OPT_DIR/scripts/scrub.sh)"
  elif [[ "$r" != ok ]]; then
    fail "$role disk: last scrub ($(ago "$e")) found errors: $(hd_status_get "$st" summary) $(hd_status_get "$st" device_stats)"
  elif (( $(hd_age_hours "$e") > 45 * 24 )); then
    warn "$role disk: last scrub was $(ago "$e") (expected monthly)"
  else
    ok "$role disk: last scrub $(ago "$e"), no errors"
  fi
done

# ------------------------------------------------------------------- SMART
section "SMART (disk health, as recorded by smart-check.sh)"
for role in data backup; do
  st="$HD_STATE_DIR/smart/$role.state"
  e=$(hd_status_get "$st" checked_epoch)
  h=$(hd_status_get "$st" health)
  if ! hd_is_uint "$e"; then
    warn "$role disk: no SMART check recorded yet (by hand: sudo $HD_OPT_DIR/scripts/smart-check.sh)"
    continue
  fi
  detail="reallocated=$(hd_status_get "$st" reallocated) pending=$(hd_status_get "$st" pending) uncorrectable=$(hd_status_get "$st" uncorrectable), checked $(ago "$e")"
  case "$h" in
    PASSED)
      bad=0
      for k in reallocated pending uncorrectable; do
        v=$(hd_status_get "$st" "$k")
        if hd_is_uint "$v" && (( v > 0 )); then bad=1; fi
      done
      if [[ $bad == 1 ]]; then
        warn "$role disk: SMART passed but has bad sectors ($detail). Plan a replacement."
      elif (( $(hd_age_hours "$e") > 72 )); then
        warn "$role disk: SMART passed, but the last check is old ($detail)"
      else
        ok "$role disk: SMART passed ($detail)"
      fi
      ;;
    UNREADABLE) warn "$role disk: the USB enclosure does not pass SMART data; health is not monitored" ;;
    *) fail "$role disk: SMART health is '$h' ($detail). Replace this disk. See RUNBOOK.md." ;;
  esac
done

# ---------------------------------------------------------------- services
section "Services"
if ! command -v systemctl >/dev/null 2>&1; then
  warn "systemctl not available: cannot check services"
else
  for u in home-driver-immich.service home-driver-drive.service home-driver-curator.service; do
    s=$(systemctl is-active "$u" 2>/dev/null); s=${s:-unknown}
    if [[ "$s" == active ]]; then ok "$u is running"; else fail "$u is $s. Try: sudo systemctl start $u ; journalctl -u $u -n 30"; fi
  done
  for t in backup-local backup-offsite scrub smart-check curator-scan; do
    s=$(systemctl is-active "home-driver-$t.timer" 2>/dev/null); s=${s:-unknown}
    if [[ "$s" == active ]]; then
      ok "home-driver-$t.timer is scheduled"
    elif [[ "$t" == backup-offsite && -z "${RESTIC_REPOSITORY:-}" ]]; then
      ok "home-driver-$t.timer is $s (offsite backup not configured)"
    else
      fail "home-driver-$t.timer is $s: this job will not run. Fix: sudo systemctl enable --now home-driver-$t.timer"
    fi
    if systemctl is-failed --quiet "home-driver-$t.service" 2>/dev/null; then
      fail "home-driver-$t.service failed on its last run. See: journalctl -u home-driver-$t -n 50"
    fi
  done
fi

# ----------------------------------------------------------------- network
section "Network (nothing may listen on a public interface)"
if ! command -v ss >/dev/null 2>&1; then
  warn "ss not available: cannot check listening ports"
else
  listeners=$(ss -H -ltn 2>/dev/null | awk '{print $4}')
  lan_addr=$(hd_env_get "$HD_ETC_DIR/immich.env" IMMICH_LAN_ADDR "")
  for spec in "2283 Immich" "8090 Drive" "8091 Curator"; do
    port=${spec%% *}; name=${spec#* }
    addrs=$(awk -v p=":$port" 'substr($0, length($0) - length(p) + 1) == p {print substr($0, 1, length($0) - length(p))}' <<<"$listeners" | sort -u | tr '\n' ' ')
    if [[ -z "${addrs// /}" ]]; then
      fail "$name: nothing is listening on port $port"
      continue
    fi
    bad=""
    for a in $addrs; do
      case "$a" in
        127.0.0.1|"[::1]") ;;
        *) if [[ $port == 2283 && -n "$lan_addr" && "$a" == "$lan_addr" ]]; then
             warn "$name also listens on $a:$port (IMMICH_LAN_ADDR, for uploads on home Wi-Fi)"
           else
             bad+="$a "
           fi ;;
      esac
    done
    if [[ -n "$bad" ]]; then
      fail "$name listens on ${bad}port $port. It must listen on 127.0.0.1 only."
    else
      ok "$name listens on 127.0.0.1:$port"
    fi
  done
fi

# ------------------------------------------------------------------ config
section "Configuration"
for f in common immich drive curator backup; do
  p="$HD_ETC_DIR/$f.env"
  if [[ ! -e "$p" ]]; then
    fail "$p is missing (run install.sh, then edit it)"
    continue
  fi
  mode=$(stat -c '%a' "$p" 2>/dev/null)
  owner=$(stat -c '%U' "$p" 2>/dev/null)
  if [[ "$mode" != 600 ]]; then
    fail "$p has mode $mode, must be 600 (it holds secrets). Fix: sudo chmod 600 $p"
  elif [[ "$owner" != root ]] && ! hd_is_test_mode; then
    fail "$p is owned by $owner, must be root. Fix: sudo chown root:root $p"
  else
    ok "$p present, mode 600"
  fi
done
if [[ -r "$HD_ETC_DIR/common.env" ]]; then
  m=$(hd_env_get "$HD_ETC_DIR/common.env" NOTIFY_METHOD "")
  if [[ -z "$m" || "$m" == none ]]; then
    fail "notifications are not configured (NOTIFY_METHOD in common.env): the server cannot alert you when a backup or disk fails"
  else
    ok "notifications use $m (test: sudo $HD_OPT_DIR/scripts/notify.sh test hello)"
  fi
  if grep -qs 'CHANGE_ME' "$HD_ETC_DIR"/*.env 2>/dev/null; then
    warn "an env file still contains a CHANGE_ME placeholder"
  fi
fi

# ----------------------------------------------------------------- summary
echo
echo "Summary: $N_OK OK, $N_WARN warnings, $N_FAIL failures"
if (( N_FAIL > 0 )); then
  echo "Something needs attention. Look up the FAIL lines in deploy/docs/RUNBOOK.md."
  exit 1
fi
echo "All essential checks passed."
exit 0
