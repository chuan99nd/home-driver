#!/usr/bin/env bash
# backup-local.sh - nightly backup: database dumps, then Disk A -> Disk B, then a
# read-only snapshot on Disk B, then prune old snapshots.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: backup-local.sh [--dry-run] [--allow-mass-delete]

Nightly local backup. In order:
  1. checks that Disk A and Disk B are really mounted (marker file on each)
  2. dumps the databases to /var/lib/home-driver/dumps and copies them to Disk A
       Immich Postgres (docker exec pg_dump), Drive and curator SQLite (.backup)
  3. mirrors Disk A to <Disk B>/current with rsync
  4. takes a read-only btrfs snapshot <Disk B>/snapshots/YYYY-MM-DD_HHMMSS
  5. removes snapshots beyond the retention in /etc/home-driver/backup.env

Options:
  --dry-run            show what would happen; change nothing (rsync runs in its
                       own dry-run mode so you see real numbers)
  --allow-mass-delete  go ahead even if the sync would delete more than
                       BACKUP_MAX_DELETE_PERCENT of the files in the backup copy
  -h, --help           this text

Exit status: 0 everything succeeded; 1 anything failed (an alert is sent).
Normally started by home-driver-backup-local.timer at 02:30.
Run by hand:  sudo /opt/home-driver/scripts/backup-local.sh
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ALLOW_MASS_DELETE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --allow-mass-delete) ALLOW_MASS_DELETE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

hd_alert_on_unexpected_exit

hd_load_env "$HD_ETC_DIR/backup.env"
KEEP_DAILY=${BACKUP_KEEP_DAILY:-14}
KEEP_WEEKLY=${BACKUP_KEEP_WEEKLY:-8}
KEEP_MONTHLY=${BACKUP_KEEP_MONTHLY:-12}
DUMP_KEEP=${BACKUP_DUMP_KEEP:-7}
MAX_DELETE_PERCENT=${BACKUP_MAX_DELETE_PERCENT:-20}
# Below this many deletions the percentage brake never triggers (tiny libraries).
MAX_DELETE_MIN_FILES=100

for v in KEEP_DAILY KEEP_WEEKLY KEEP_MONTHLY DUMP_KEEP MAX_DELETE_PERCENT; do
  hd_is_uint "${!v}" || hd_fail "BACKUP setting $v='${!v}' is not a whole number; fix $HD_ETC_DIR/backup.env"
done
# Refuse a retention that would keep nothing: that is never what the owner wants.
(( KEEP_DAILY >= 1 )) || hd_fail "BACKUP_KEEP_DAILY must be at least 1"
(( DUMP_KEEP >= 1 )) || hd_fail "BACKUP_DUMP_KEEP must be at least 1"

hd_require_root
hd_require_cmd rsync mountpoint flock gzip
hd_lock backup-local

A="$HD_DATA_DIR"
B="$HD_BACKUP_DIR"
DUMPS="$HD_STATE_DIR/dumps"
TS=$(hd_timestamp)
ERRORS=()

# ---------------------------------------------------------------- 1. preflight
# These checks are the heart of the script. If Disk A is unplugged, $A is an
# empty directory on the SSD and a blind `rsync --delete` would empty the backup.
reason=$(hd_check_disk data "$A") || hd_fail "Disk A (data) is not usable: $reason. Backup NOT run."
reason=$(hd_check_disk backup "$B") || hd_fail "Disk B (backup) is not usable: $reason. Backup NOT run."
[[ "$(realpath "$A")" != "$(realpath "$B")" ]] || hd_fail "data and backup directories are the same path"
[[ -d "$B/current" ]] || hd_fail "$B/current is missing: Disk B was not prepared by setup-disks.sh. Backup NOT run."
[[ -d "$B/snapshots" ]] || hd_fail "$B/snapshots is missing: Disk B was not prepared by setup-disks.sh. Backup NOT run."
if ! hd_is_test_mode || command -v btrfs >/dev/null 2>&1; then
  hd_require_cmd btrfs
  btrfs subvolume show "$B/current" >/dev/null 2>&1 \
    || hd_fail "$B/current is not a btrfs subvolume (cannot be snapshotted). Backup NOT run."
fi

# A freshly formatted replacement Disk A has the marker but no data. Mirroring it
# would empty the backup copy. Restore first (restore.sh disk-a), then back up.
data_has_files=$(find "$A" -mindepth 1 -type f ! -path "$A/$HD_SENTINEL" ! -path "$A/dumps/*" -print -quit 2>/dev/null || true)
backup_has_files=$(find "$B/current" -mindepth 1 -type f -print -quit 2>/dev/null || true)
if [[ -z "$data_has_files" && -n "$backup_has_files" ]]; then
  hd_fail "Disk A contains no files but the backup does. Refusing to mirror an empty disk over the backup. If Disk A was just replaced, run: restore.sh disk-a"
fi

hd_log "preflight OK: A=$A B=$B"

# ------------------------------------------------------------------- 2. dumps
# Dumps come BEFORE the file copy on purpose: the database then only references
# files that already exist on disk, so the copied files are a superset of what
# the dump knows about. The other order could back up a database pointing at
# photos that were not copied yet.
#
# A failed dump is recorded and reported, but does NOT stop the file backup:
# the photos themselves are the irreplaceable part.

dump_immich() {
  local container=${BACKUP_IMMICH_DB_CONTAINER:-immich_postgres}
  local user db out tmp
  user=$(hd_env_get "$HD_ETC_DIR/immich.env" DB_USERNAME postgres)
  db=$(hd_env_get "$HD_ETC_DIR/immich.env" DB_DATABASE_NAME immich)
  out="$DUMPS/immich-$TS.sql.gz"
  tmp="$out.partial"
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would dump Immich database '$db' from container $container to $out"
    return 0
  fi
  command -v docker >/dev/null 2>&1 || { hd_err "docker not found"; return 1; }
  # Same command as the Immich v3 documentation (pg_dump of the one database).
  if ! docker exec "$container" pg_dump --clean --if-exists --dbname="$db" --username="$user" | gzip >"$tmp"; then
    rm -f "$tmp"
    hd_err "pg_dump of Immich failed (is the container $container running?)"
    return 1
  fi
  # Verify before trusting it: valid gzip, looks like a Postgres dump, complete.
  if ! gzip -t "$tmp" 2>/dev/null \
     || ! gzip -dc "$tmp" | head -n 20 | grep -q "PostgreSQL database dump" \
     || ! gzip -dc "$tmp" | tail -n 5 | grep -q "PostgreSQL database dump complete"; then
    rm -f "$tmp"
    hd_err "Immich dump is empty, truncated or not a Postgres dump"
    return 1
  fi
  chmod 600 "$tmp"
  mv -f "$tmp" "$out"
  hd_log "Immich database dumped: $out ($(du -h "$out" | cut -f1))"
}

# dump_sqlite <name> <db-path>
dump_sqlite() {
  local name=$1 db=$2 out tmp check
  out="$DUMPS/$name-$TS.db"
  tmp="$out.partial"
  if [[ ! -f "$db" ]]; then
    hd_err "$name database not found at $db (set BACKUP_${name^^}_DB=0 in backup.env if you do not run $name)"
    return 1
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would back up SQLite database $db to $out"
    return 0
  fi
  command -v sqlite3 >/dev/null 2>&1 || { hd_err "sqlite3 not found"; return 1; }
  # .backup takes a consistent copy while the service keeps running. Run as the
  # service user so no root-owned -wal/-shm file is left next to the live database.
  rm -f "$tmp"
  if ! hd_as_service_user sqlite3 "$db" ".timeout 30000" ".backup '$tmp'"; then
    rm -f "$tmp"
    hd_err "sqlite3 .backup of $db failed"
    return 1
  fi
  check=$(sqlite3 "$tmp" "PRAGMA integrity_check;" 2>&1 || true)
  if [[ "$check" != "ok" ]]; then
    rm -f "$tmp"
    hd_err "backup copy of $db failed its integrity check: ${check:0:200}"
    return 1
  fi
  chmod 600 "$tmp"
  mv -f "$tmp" "$out"
  hd_log "$name database backed up: $out"
}

# prune_dumps <glob-prefix> <suffix>: keep the newest $DUMP_KEEP files.
prune_dumps() {
  local -a files=()
  local f n i
  for f in "$DUMPS"/"$1"-*"$2"; do [[ -f "$f" ]] && files+=("$f"); done
  n=${#files[@]}
  # Glob order is lexical = chronological because of the timestamp in the name.
  for (( i = 0; i < n - DUMP_KEEP; i++ )); do
    hd_run rm -f -- "${files[$i]}"
  done
}

run_dumps() {
  if [[ "$HD_DRY_RUN" != 1 ]]; then
    mkdir -p "$DUMPS"
    rm -f "$DUMPS"/*.partial
  fi
  if [[ "${BACKUP_IMMICH_DB:-1}" == 1 ]]; then
    dump_immich || ERRORS+=("Immich database dump failed")
  fi
  if [[ "${BACKUP_DRIVE_DB:-1}" == 1 ]]; then
    dump_sqlite drive "$(hd_env_get "$HD_ETC_DIR/drive.env" DRIVE_DB_PATH "$HD_STATE_DIR/drive/drive.db")" \
      || ERRORS+=("Drive database dump failed")
  fi
  if [[ "${BACKUP_CURATOR_DB:-1}" == 1 ]]; then
    dump_sqlite curator "$(hd_env_get "$HD_ETC_DIR/curator.env" CURATOR_DB_PATH "$HD_STATE_DIR/curator/curator.db")" \
      || ERRORS+=("Curator database dump failed")
  fi
  prune_dumps immich .sql.gz
  prune_dumps drive .db
  prune_dumps curator .db

  # The env files hold the settings and secrets needed to rebuild the server on
  # a new laptop. Keep a root-only copy with the dumps.
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would copy $HD_ETC_DIR/*.env to $DUMPS/config/ and mirror $DUMPS/ to $A/dumps/"
    return 0
  fi
  local f
  mkdir -p "$DUMPS/config"
  chmod 700 "$DUMPS/config"
  for f in "$HD_ETC_DIR"/*.env; do
    [[ -f "$f" ]] || continue
    install -m 0600 "$f" "$DUMPS/config/$(basename "$f")"
  done
  # Copy the dumps onto Disk A so they travel to Disk B and offsite with the data.
  mkdir -p "$A/dumps"
  chmod 700 "$A/dumps"
  rsync -a --delete "$DUMPS/" "$A/dumps/" || ERRORS+=("copying dumps to Disk A failed")
}

run_dumps

# ---------------------------------------------- 3. make sure nothing is unprotected
# Invariant: before rsync is allowed to delete anything from $B/current, a
# snapshot of the present state of $B/current must exist. Normally that is last
# night's snapshot. If last night's run died between rsync and snapshot, the
# marker file below is still there and we take the missing snapshot first.
MARKER="$B/.sync-not-yet-snapshotted"

take_snapshot() {
  local name=$1
  [[ "$name" =~ $HD_SNAPSHOT_RE ]] || hd_fail "internal error: bad snapshot name '$name'"
  [[ ! -e "$B/snapshots/$name" ]] || hd_fail "snapshot $name already exists"
  hd_run btrfs subvolume snapshot -r "$B/current" "$B/snapshots/$name" >/dev/null \
    || hd_fail "creating snapshot $name on Disk B failed"
  hd_log "snapshot created: $B/snapshots/$name"
}

if [[ -e "$MARKER" ]]; then
  hd_warn "the previous run did not finish its snapshot; taking it now, before syncing"
  take_snapshot "$TS"
  [[ "$HD_DRY_RUN" == 1 ]] || sleep 1
  TS=$(hd_timestamp)
fi

# ------------------------------------------------------------------ 4. rsync
# WHY --delete: $B/current is a *mirror* of Disk A. Without --delete it would
# keep every file ever deleted, grow without bound, and a rebuilt Disk A would
# resurrect files the family deleted on purpose.
# WHY that is safe: protection against a deletion (accidental, or a bug) being
# propagated does not come from $B/current, it comes from the read-only
# snapshots taken after every run. A file deleted today disappears from
# `current` tonight but stays in last night's snapshot, and in the weekly and
# monthly ones, until retention expires (see restore.sh file).
# Extra brakes: the empty-disk check above, and the mass-delete check below.
RSYNC_OPTS=(
  --archive --numeric-ids --delete --delete-excluded
  --exclude="/$HD_SENTINEL"       # each disk keeps its own marker
  --exclude="/drive/tmp/**"       # unfinished uploads, worthless after a restore
  --exclude="/restored/"          # scratch area of `restore.sh file`
)

# 4a. rehearsal: how many files would be deleted?
stats=$(LC_ALL=C rsync "${RSYNC_OPTS[@]}" --dry-run --stats "$A/" "$B/current/" 2>&1) \
  || hd_fail "rsync rehearsal failed: $(tail -n 3 <<<"$stats")"
num() { sed -n "s/^$1: *\([0-9.,]*\).*/\1/p" <<<"$stats" | head -n 1 | tr -d '.,'; }
n_src=$(num "Number of files"); n_src=${n_src:-0}
n_new=$(num "Number of created files"); n_new=${n_new:-0}
n_del=$(num "Number of deleted files"); n_del=${n_del:-0}
n_dst=$(( n_src - n_new + n_del ))
hd_log "sync plan: $n_src files on Disk A, $n_new new, $n_del to delete from the backup copy (of about $n_dst)"

if (( MAX_DELETE_PERCENT > 0 && n_del >= MAX_DELETE_MIN_FILES && n_dst > 0 )) \
   && (( n_del * 100 > MAX_DELETE_PERCENT * n_dst )); then
  if [[ "$ALLOW_MASS_DELETE" == 1 ]]; then
    hd_warn "mass deletion ($n_del of $n_dst files) allowed by --allow-mass-delete"
  else
    hd_fail "The sync would delete $n_del of about $n_dst files from the backup copy (more than ${MAX_DELETE_PERCENT}%). Backup stopped, nothing deleted. If this is expected (big clean-up, or the Immich storage template changed), run: sudo $HD_OPT_DIR/scripts/backup-local.sh --allow-mass-delete"
  fi
fi

if [[ "$HD_DRY_RUN" == 1 ]]; then
  hd_log "[dry-run] would mirror $A/ to $B/current/ ($n_new new, $n_del deleted)"
  hd_log "[dry-run] would create read-only snapshot $B/snapshots/$TS"
else
  # 4b. the real copy
  : >"$MARKER"
  rc=0
  rsync "${RSYNC_OPTS[@]}" "$A/" "$B/current/" || rc=$?
  # 24 = "some files vanished while copying": normal on a live server.
  if [[ $rc -ne 0 && $rc -ne 24 ]]; then
    hd_fail "rsync Disk A -> Disk B failed with exit code $rc. The backup copy may be incomplete; no snapshot taken."
  fi
  hd_log "mirror complete (rsync exit $rc)"

  # ------------------------------------------------------------- 5. snapshot
  take_snapshot "$TS"
  rm -f "$MARKER"
fi

# ---------------------------------------------------------------- 6. prune
# Keep every snapshot of the newest N days that have one, plus the newest
# snapshot of each of the last W ISO weeks and M months.
# Only names matching YYYY-MM-DD_HHMMSS are ever considered; a snapshot you
# created by hand under another name is never deleted.
prune_snapshots() {
  local -a snaps=()
  local -A keep=() seen_d=() seen_w=() seen_m=()
  local s day week month nd=0 nw=0 nm=0 removed=0
  mapfile -t snaps < <(hd_list_snapshots | LC_ALL=C sort -r)
  (( ${#snaps[@]} > 0 )) || return 0
  keep["${snaps[0]}"]=1   # the newest snapshot is always kept
  for s in "${snaps[@]}"; do
    day=${s:0:10}
    month=${s:0:7}
    if ! week=$(date -d "$day" +%G-W%V 2>/dev/null); then
      keep["$s"]=1        # cannot interpret the date: keep, never guess
      continue
    fi
    # Daily: EVERY snapshot of the newest N days is kept, not one per day. A
    # second run on the same day (by hand, or the catch-up snapshot after an
    # interrupted run) must never cause the earlier snapshot of that day to go.
    if [[ -n "${seen_d[$day]:-}" ]]; then
      keep["$s"]=1
    elif (( nd < KEEP_DAILY )); then
      seen_d[$day]=1; nd=$((nd + 1)); keep["$s"]=1
    fi
    if [[ -z "${seen_w[$week]:-}" ]] && (( nw < KEEP_WEEKLY )); then
      seen_w[$week]=1; nw=$((nw + 1)); keep["$s"]=1
    fi
    if [[ -z "${seen_m[$month]:-}" ]] && (( nm < KEEP_MONTHLY )); then
      seen_m[$month]=1; nm=$((nm + 1)); keep["$s"]=1
    fi
  done
  for s in "${snaps[@]}"; do
    [[ -n "${keep[$s]:-}" ]] && continue
    if hd_run btrfs subvolume delete "$B/snapshots/$s" >/dev/null; then
      removed=$((removed + 1))
    else
      ERRORS+=("could not delete old snapshot $s")
    fi
  done
  hd_log "retention: ${#snaps[@]} snapshots, $removed removed (keep $KEEP_DAILY daily, $KEEP_WEEKLY weekly, $KEEP_MONTHLY monthly)"
}
prune_snapshots

# ------------------------------------------------------------------ 7. result
if [[ "$HD_DRY_RUN" == 1 ]]; then
  hd_log "[dry-run] finished; nothing was changed"
  exit 0
fi

now=$(date +%s)
if (( ${#ERRORS[@]} > 0 )); then
  msg=$(printf '%s; ' "${ERRORS[@]}")
  hd_write_status backup-local \
    "last_sync_epoch=$now" "last_snapshot=$TS" "result=errors" "errors=$msg" \
    "last_success_epoch=$(hd_status_get "$HD_STATE_DIR/status/backup-local.status" last_success_epoch)"
  hd_fail "Files were copied and snapshot $TS was taken, but with errors: $msg"
fi
hd_write_status backup-local "last_sync_epoch=$now" "last_success_epoch=$now" "last_snapshot=$TS" "result=ok"
hd_log "backup OK: snapshot $TS"
exit 0
