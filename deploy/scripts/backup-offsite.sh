#!/usr/bin/env bash
# backup-offsite.sh - encrypted offsite copy of Disk A and the DB dumps with restic.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: backup-offsite.sh [--dry-run] [--init] [--force-check]

Optional encrypted offsite backup with restic. Configured in
/etc/home-driver/backup.env (RESTIC_REPOSITORY, RESTIC_PASSWORD, credentials).

If RESTIC_REPOSITORY is empty this script prints a note and exits 0: the
offsite backup is optional and "not configured" is not an error.

What it does:
  1. checks Disk A is really mounted (never uploads an empty mountpoint)
  2. restic backup of /srv/home-driver/data and /var/lib/home-driver/dumps
  3. restic forget --prune with the BACKUP_OFFSITE_KEEP_* retention
  4. restic check, when the last check is older than BACKUP_OFFSITE_CHECK_DAYS

Options:
  --init         create the restic repository (run ONCE, by hand, after filling
                 in backup.env). The nightly job never creates a repository on
                 its own: a typo in the address must fail, not start a new one.
  --force-check  run `restic check` now regardless of when it last ran
  --dry-run      print the restic commands instead of running them
  -h, --help     this text

Normally started by home-driver-backup-offsite.timer at 04:30, after the local backup.
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DO_INIT=0
FORCE_CHECK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --init) DO_INIT=1 ;;
    --force-check) FORCE_CHECK=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

hd_load_env "$HD_ETC_DIR/backup.env"

if [[ -z "${RESTIC_REPOSITORY:-}" ]]; then
  hd_log "offsite backup is not configured (RESTIC_REPOSITORY is empty in $HD_ETC_DIR/backup.env); nothing to do"
  exit 0
fi

hd_alert_on_unexpected_exit

if [[ -z "${RESTIC_PASSWORD:-}" && -z "${RESTIC_PASSWORD_FILE:-}" && -z "${RESTIC_PASSWORD_COMMAND:-}" ]]; then
  hd_fail "RESTIC_REPOSITORY is set but RESTIC_PASSWORD is empty in $HD_ETC_DIR/backup.env. Offsite backup NOT run."
fi

KEEP_DAILY=${BACKUP_OFFSITE_KEEP_DAILY:-7}
KEEP_WEEKLY=${BACKUP_OFFSITE_KEEP_WEEKLY:-4}
KEEP_MONTHLY=${BACKUP_OFFSITE_KEEP_MONTHLY:-12}
CHECK_DAYS=${BACKUP_OFFSITE_CHECK_DAYS:-7}
for v in KEEP_DAILY KEEP_WEEKLY KEEP_MONTHLY CHECK_DAYS; do
  hd_is_uint "${!v}" || hd_fail "offsite setting $v='${!v}' is not a whole number; fix $HD_ETC_DIR/backup.env"
done
(( KEEP_DAILY >= 1 )) || hd_fail "BACKUP_OFFSITE_KEEP_DAILY must be at least 1"

hd_require_root
hd_require_cmd restic mountpoint flock
hd_lock backup-offsite

# A fixed host name keeps restic's history in one series even if the laptop is
# renamed or replaced.
RESTIC_HOST="home-driver"
RESTIC_GLOBAL=()
if [[ -n "${BACKUP_OFFSITE_UPLOAD_LIMIT_KBPS:-}" ]]; then
  hd_is_uint "$BACKUP_OFFSITE_UPLOAD_LIMIT_KBPS" || hd_fail "BACKUP_OFFSITE_UPLOAD_LIMIT_KBPS must be a whole number"
  RESTIC_GLOBAL+=(--limit-upload "$BACKUP_OFFSITE_UPLOAD_LIMIT_KBPS")
fi
export RESTIC_CACHE_DIR="${RESTIC_CACHE_DIR:-$HD_STATE_DIR/restic-cache}"

if [[ "$DO_INIT" == 1 ]]; then
  if restic "${RESTIC_GLOBAL[@]}" cat config >/dev/null 2>&1; then
    hd_log "repository already exists and the password works; nothing to do"
    exit 0
  fi
  hd_run restic "${RESTIC_GLOBAL[@]}" init || hd_fail "restic init failed"
  hd_log "repository created. Write the RESTIC_PASSWORD down on paper NOW."
  exit 0
fi

# ---- preflight ----
reason=$(hd_check_disk data "$HD_DATA_DIR") \
  || hd_fail "Disk A (data) is not usable: $reason. Offsite backup NOT run."

# Wait for a running local backup to finish (it writes the dumps we upload).
if [[ "$HD_DRY_RUN" != 1 ]]; then
  mkdir -p "$HD_STATE_DIR/locks"
  exec {wait_fd}>"$HD_STATE_DIR/locks/backup-local.lock"
  flock -w 43200 "$wait_fd" || hd_warn "local backup still running after 12 h; continuing anyway"
  flock -u "$wait_fd"
  exec {wait_fd}>&-
fi

if ! restic "${RESTIC_GLOBAL[@]}" cat config >/dev/null 2>&1; then
  hd_fail "Cannot open the restic repository (wrong address, wrong password, no network, or not created yet). If this is the first time, run: sudo $HD_OPT_DIR/scripts/backup-offsite.sh --init"
fi

# ---- backup ----
EXCLUDES=(
  --exclude "$HD_DATA_DIR/drive/tmp"
  --exclude "$HD_DATA_DIR/restored"
)
if [[ "${BACKUP_OFFSITE_EXCLUDE_GENERATED:-1}" == 1 ]]; then
  # Regenerable from the originals (Immich: Administration > Job Queues).
  # The contents are skipped but each folder's hidden ".immich" marker file is
  # kept: Immich refuses to start when a storage folder lacks its marker.
  for d in thumbs encoded-video; do
    EXCLUDES+=(--exclude "$HD_DATA_DIR/immich/$d/*" --exclude "!$HD_DATA_DIR/immich/$d/.immich")
  done
fi
PATHS=("$HD_DATA_DIR")
[[ -d "$HD_STATE_DIR/dumps" ]] && PATHS+=("$HD_STATE_DIR/dumps")

rc=0
hd_run restic "${RESTIC_GLOBAL[@]}" backup --host "$RESTIC_HOST" --tag home-driver \
  --one-file-system --exclude-caches "${EXCLUDES[@]}" "${PATHS[@]}" || rc=$?
PARTIAL=""
if [[ $rc -eq 3 ]]; then
  # restic: snapshot created, but some files could not be read.
  PARTIAL="some files could not be read during the backup (restic exit 3); "
elif [[ $rc -ne 0 ]]; then
  hd_fail "restic backup failed with exit code $rc"
fi

# ---- retention ----
hd_run restic "${RESTIC_GLOBAL[@]}" forget --host "$RESTIC_HOST" --tag home-driver --prune \
  --keep-daily "$KEEP_DAILY" --keep-weekly "$KEEP_WEEKLY" --keep-monthly "$KEEP_MONTHLY" \
  || hd_fail "restic forget --prune failed (the new backup itself was uploaded)"

# ---- periodic check ----
status_file="$HD_STATE_DIR/status/backup-offsite.status"
last_check=$(hd_status_get "$status_file" last_check_epoch)
now=$(date +%s)
if [[ "$FORCE_CHECK" == 1 ]] || ! hd_is_uint "$last_check" || (( now - last_check >= CHECK_DAYS * 86400 )); then
  CHECK_ARGS=()
  [[ -n "${BACKUP_OFFSITE_CHECK_READ_SUBSET:-}" ]] && CHECK_ARGS+=(--read-data-subset "$BACKUP_OFFSITE_CHECK_READ_SUBSET")
  hd_run restic "${RESTIC_GLOBAL[@]}" check "${CHECK_ARGS[@]}" \
    || hd_fail "restic check reported problems with the offsite repository. See RUNBOOK.md, 'An alert arrives'."
  last_check=$now
fi

if [[ "$HD_DRY_RUN" == 1 ]]; then
  hd_log "[dry-run] finished; nothing was changed"
  exit 0
fi

if [[ -n "$PARTIAL" ]]; then
  hd_write_status backup-offsite "last_run_epoch=$now" "result=errors" "last_check_epoch=${last_check:-}" \
    "last_success_epoch=$(hd_status_get "$status_file" last_success_epoch)"
  hd_fail "Offsite backup finished with errors: ${PARTIAL}check: journalctl -u home-driver-backup-offsite -n 100"
fi
hd_write_status backup-offsite "last_run_epoch=$now" "last_success_epoch=$now" "result=ok" "last_check_epoch=${last_check:-}"
hd_log "offsite backup OK"
exit 0
