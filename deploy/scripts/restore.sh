#!/usr/bin/env bash
# restore.sh - guided recovery. Every subcommand prints its plan and asks first.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: restore.sh [--dry-run] [--yes] <command> [arguments]

Guided recovery for home-driver. Every command first prints exactly what it
will do, then asks you to type "yes". Nothing here ever deletes a backup, and
nothing overwrites an existing file or database without keeping the old one.

Commands:
  list-snapshots
        Show the snapshots on Disk B (one per night), oldest first.

  file <path> [--from <snapshot>] [--to <dir>] [--in-place]
        Get one file or folder back from a Disk B snapshot.
        <path> is relative to Disk A, e.g.  drive/blobs/ab/cd...  or
        immich/library/anna/2024 . An absolute /srv/home-driver/data/... path works too.
        Without --from, the newest snapshot that contains the path is used.
        By default the copy is placed in /srv/home-driver/data/restored/<snapshot>/
        so nothing existing is touched. --to <dir> picks another folder;
        --in-place puts it back at its original location (existing files are
        never overwritten).

  disk-a [--from <snapshot>] [--allow-non-empty]
        Disk A died and you bought a new one. After formatting the NEW disk with
        setup-disks.sh --role data and mounting it, this copies everything
        back from the newest snapshot on Disk B.

  disk-b [--allow-non-empty]
        Disk B died and you bought a new one. After formatting the NEW disk with
        setup-disks.sh --role backup and mounting it, this fills it from Disk A
        (it runs the normal backup once).

  offsite <target-dir> [--snapshot <id>]
        Download the offsite (restic) backup into an EMPTY folder. Use when both
        disks are gone. <id> defaults to "latest"; list with: restic snapshots

  immich-db <dump.sql.gz>
        Replace the Immich database with a dump from /var/lib/home-driver/dumps
        (or .../dumps inside a snapshot). The current database folder is moved
        aside, not deleted.

  drive-db <dump.db>
  curator-db <dump.db>
        Replace the Drive / curator SQLite database with a dump. The current
        database file is moved aside, not deleted.

Options (anywhere on the command line):
  --dry-run    print the plan and stop; change nothing
  --yes        do not ask for confirmation (for use by scripts)
  -h, --help   this text

Start here when unsure:  sudo /opt/home-driver/scripts/restore.sh list-snapshots
Step-by-step scenarios are in deploy/docs/RUNBOOK.md.
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

FROM=""
TO=""
IN_PLACE=0
ALLOW_NON_EMPTY=0
RESTIC_SNAPSHOT="latest"
ARGS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --yes) HD_ASSUME_YES=1 ;;
    --from) FROM=${2:-}; [[ -n "$FROM" ]] || hd_die "--from needs a snapshot name"; shift ;;
    --to) TO=${2:-}; [[ -n "$TO" ]] || hd_die "--to needs a directory"; shift ;;
    --snapshot) RESTIC_SNAPSHOT=${2:-}; [[ -n "$RESTIC_SNAPSHOT" ]] || hd_die "--snapshot needs an id"; shift ;;
    --in-place) IN_PLACE=1 ;;
    --allow-non-empty) ALLOW_NON_EMPTY=1 ;;
    -h|--help) usage; exit 0 ;;
    --*) usage >&2; hd_die "unknown option: $1" ;;
    *) ARGS+=("$1") ;;
  esac
  shift
done
[[ ${#ARGS[@]} -gt 0 ]] || { usage >&2; exit 2; }
CMD=${ARGS[0]}

A="$HD_DATA_DIR"
B="$HD_BACKUP_DIR"
SERVICES=(home-driver-immich.service home-driver-drive.service home-driver-curator.service)

plan() { printf '%s\n' "$@"; }

# finish_plan: after printing the plan: stop here in dry-run, else ask.
finish_plan() {
  echo
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] plan only; nothing was changed"
    exit 0
  fi
  hd_require_root
  hd_confirm "Proceed?"
}

need_disk() { # <role> <dir>
  local reason
  reason=$(hd_check_disk "$1" "$2") || hd_die "the $1 disk is not usable: $reason"
}

# resolve_snapshot <name-or-empty>: prints the snapshot directory.
resolve_snapshot() {
  local name=$1
  if [[ -z "$name" ]]; then
    name=$(hd_latest_snapshot)
    [[ -n "$name" ]] || hd_die "there are no snapshots on Disk B ($B/snapshots)"
  fi
  [[ "$name" != */* && "$name" != .* ]] || hd_die "invalid snapshot name: $name"
  [[ -d "$B/snapshots/$name" ]] || hd_die "no such snapshot: $name (see: restore.sh list-snapshots)"
  printf '%s\n' "$B/snapshots/$name"
}

# has_user_files <dir>: true if the directory holds any file besides the marker.
has_user_files() {
  [[ -n "$(find "$1" -mindepth 1 -type f ! -path "$1/$HD_SENTINEL" -print -quit 2>/dev/null)" ]]
}

active_services() {
  local s
  command -v systemctl >/dev/null 2>&1 || return 0
  for s in "${SERVICES[@]}"; do
    systemctl is-active --quiet "$s" 2>/dev/null && printf '%s ' "$s"
  done
  return 0
}

# ------------------------------------------------------------ list-snapshots
cmd_list_snapshots() {
  need_disk backup "$B"
  local -a snaps=()
  mapfile -t snaps < <(hd_list_snapshots)
  if (( ${#snaps[@]} == 0 )); then
    echo "No snapshots yet on $B/snapshots. The first one appears after the first backup."
    return 0
  fi
  echo "Snapshots on Disk B ($B/snapshots), oldest first:"
  printf '  %s\n' "${snaps[@]}"
  echo
  echo "${#snaps[@]} snapshots. Newest: ${snaps[-1]}"
  echo "Each one is a complete read-only copy of Disk A as it was that night."
  echo "Browse one with:  sudo ls $B/snapshots/${snaps[-1]}/"
}

# ---------------------------------------------------------------------- file
cmd_file() {
  [[ ${#ARGS[@]} -eq 2 ]] || hd_die "usage: restore.sh file <path> [--from <snapshot>] [--to <dir>] [--in-place]"
  [[ $IN_PLACE == 0 || -z "$TO" ]] || hd_die "use either --to or --in-place, not both"
  need_disk backup "$B"
  local rel=${ARGS[1]} snapdir="" s src dest_parent
  rel=${rel#"$A"/}
  rel=${rel#/}
  rel=${rel%/}
  [[ -n "$rel" ]] || hd_die "empty path"
  case "/$rel/" in */../*|*/./*) hd_die "path must not contain . or .. components" ;; esac

  if [[ -n "$FROM" ]]; then
    snapdir=$(resolve_snapshot "$FROM")
    [[ -e "$snapdir/$rel" ]] || hd_die "'$rel' does not exist in snapshot $FROM"
  else
    while IFS= read -r s; do
      if [[ -e "$B/snapshots/$s/$rel" ]]; then snapdir="$B/snapshots/$s"; break; fi
    done < <(hd_list_snapshots | LC_ALL=C sort -r)
    [[ -n "$snapdir" ]] || hd_die "'$rel' was not found in any snapshot on Disk B"
  fi
  src="$snapdir/$rel"

  if [[ $IN_PLACE == 1 ]]; then
    need_disk data "$A"
    dest_parent=$(dirname "$A/$rel")
  elif [[ -n "$TO" ]]; then
    dest_parent=$TO
  else
    need_disk data "$A"
    dest_parent=$(dirname "$A/restored/$(basename "$snapdir")/$rel")
  fi

  plan "PLAN: restore from snapshot $(basename "$snapdir")" \
       "  copy   $src" \
       "  into   $dest_parent/" \
       "  Existing files are NOT overwritten (rsync --ignore-existing)."
  if [[ $IN_PLACE == 1 ]]; then
    plan "  NOTE: putting a file back on disk does not put it back in the Immich or Drive" \
         "        database. For a deleted photo, re-upload the restored file in Immich."
  fi
  finish_plan
  mkdir -p "$dest_parent"
  rsync --archive --ignore-existing "$src" "$dest_parent/"
  hd_log "restored to $dest_parent/$(basename "$rel")"
}

# -------------------------------------------------------------------- disk-a
cmd_disk_a() {
  need_disk backup "$B"
  need_disk data "$A"
  local snapdir active
  if [[ "$FROM" == "current" ]]; then
    snapdir="$B/current"
  else
    snapdir=$(resolve_snapshot "$FROM")
  fi
  has_user_files "$snapdir" || hd_die "$snapdir contains no files; refusing to restore from an empty source"
  if has_user_files "$A" && [[ $ALLOW_NON_EMPTY != 1 ]]; then
    hd_die "Disk A at $A already contains files. This command is for a NEW, empty Disk A. To restore single files use 'restore.sh file'. (--allow-non-empty copies only what is missing or different.)"
  fi
  active=$(active_services)
  [[ -z "$active" ]] || hd_die "these services are running: $active. Stop them first: sudo systemctl stop ${SERVICES[*]}"

  plan "PLAN: rebuild Disk A from the backup" \
       "  copy everything from  $snapdir/" \
       "  to                    $A/" \
       "  The backup itself is only read. Nightly backups are blocked while this runs." \
       "  This can take many hours. Do not unplug anything."
  finish_plan
  hd_lock backup-local     # a nightly backup must not mirror a half-restored disk
  local rc=0
  rsync --archive --numeric-ids --exclude="/$HD_SENTINEL" --info=progress2 "$snapdir/" "$A/" || rc=$?
  [[ $rc -eq 0 ]] || hd_die "rsync failed with exit code $rc. Fix the cause and run the same command again with --allow-non-empty; it continues where it stopped."
  hd_log "Disk A rebuilt from $(basename "$snapdir")"
  cat <<EOF

NEXT STEPS (see RUNBOOK.md, "Disk A dies"):
  1. Make the databases match the restored files. The dumps that belong to
     this snapshot are in $A/dumps/ :
       sudo $HD_OPT_DIR/scripts/restore.sh immich-db $A/dumps/immich-<date>.sql.gz
       sudo $HD_OPT_DIR/scripts/restore.sh drive-db  $A/dumps/drive-<date>.db
  2. Start the services:   sudo systemctl start ${SERVICES[*]}
  3. Check:                sudo $HD_OPT_DIR/scripts/doctor.sh
EOF
}

# -------------------------------------------------------------------- disk-b
cmd_disk_b() {
  need_disk data "$A"
  need_disk backup "$B"
  [[ -d "$B/current" && -d "$B/snapshots" ]] || hd_die "$B was not prepared by setup-disks.sh --role backup"
  has_user_files "$A" || hd_die "Disk A at $A contains no files; refusing to seed a backup from an empty disk"
  if [[ $ALLOW_NON_EMPTY != 1 ]]; then
    if has_user_files "$B/current" || [[ -n "$(hd_list_snapshots)" ]]; then
      hd_die "Disk B at $B already contains a backup. This command is for a NEW, empty Disk B. A normal backup is: backup-local.sh"
    fi
  fi
  plan "PLAN: fill the new Disk B from Disk A" \
       "  run   $HD_SCRIPTS_DIR/backup-local.sh" \
       "  which dumps the databases, copies $A/ to $B/current/" \
       "  and takes the first snapshot. Disk A is only read." \
       "  This can take many hours. Do not unplug anything."
  finish_plan
  "$HD_SCRIPTS_DIR/backup-local.sh"
  hd_log "Disk B is seeded. Check with: sudo $HD_OPT_DIR/scripts/doctor.sh"
}

# ------------------------------------------------------------------- offsite
cmd_offsite() {
  [[ ${#ARGS[@]} -eq 2 ]] || hd_die "usage: restore.sh offsite <target-dir> [--snapshot <id>]"
  local target=${ARGS[1]}
  hd_load_env "$HD_ETC_DIR/backup.env"
  [[ -n "${RESTIC_REPOSITORY:-}" ]] || hd_die "RESTIC_REPOSITORY is not set in $HD_ETC_DIR/backup.env. On a new laptop, fill in backup.env first (repository address, RESTIC_PASSWORD from your paper note, storage credentials)."
  [[ -n "${RESTIC_PASSWORD:-}${RESTIC_PASSWORD_FILE:-}${RESTIC_PASSWORD_COMMAND:-}" ]] || hd_die "RESTIC_PASSWORD is not set in $HD_ETC_DIR/backup.env"
  if [[ -e "$target" ]]; then
    [[ -d "$target" ]] || hd_die "$target exists and is not a directory"
    [[ -z "$(find "$target" -mindepth 1 -print -quit 2>/dev/null)" ]] \
      || hd_die "$target is not empty. Restore into an empty folder so nothing can be overwritten."
  fi
  hd_require_cmd restic
  export RESTIC_CACHE_DIR="${RESTIC_CACHE_DIR:-$HD_STATE_DIR/restic-cache}"
  plan "PLAN: download the offsite backup" \
       "  repository  $RESTIC_REPOSITORY" \
       "  snapshot    $RESTIC_SNAPSHOT (host home-driver)" \
       "  into        $target/" \
       "  Files appear under $target/srv/home-driver/data and $target/var/lib/home-driver/dumps." \
       "  This downloads everything and can take a long time. The repository is only read."
  finish_plan
  mkdir -p "$target"
  restic restore "$RESTIC_SNAPSHOT" --host home-driver --target "$target"
  hd_log "offsite backup restored into $target"
  cat <<EOF

NEXT STEPS (see RUNBOOK.md, "Both disks die"):
  Copy the restored data onto a freshly prepared Disk A:
    sudo rsync -a --info=progress2 $target/srv/home-driver/data/ $A/
  then restore the databases from $target/var/lib/home-driver/dumps/ with
  'restore.sh immich-db' and 'restore.sh drive-db'.
EOF
}

# ----------------------------------------------------------------- immich-db
cmd_immich_db() {
  [[ ${#ARGS[@]} -eq 2 ]] || hd_die "usage: restore.sh immich-db <dump.sql.gz>"
  local dump=${ARGS[1]} env_file="$HD_ETC_DIR/immich.env" user db dbdir aside container=immich_postgres
  [[ -f "$dump" ]] || hd_die "dump file not found: $dump"
  gzip -t "$dump" 2>/dev/null || hd_die "$dump is not a valid gzip file"
  gzip -dc "$dump" | head -n 20 | grep -q "PostgreSQL database dump" \
    || hd_die "$dump does not look like a PostgreSQL dump"
  user=$(hd_env_get "$env_file" DB_USERNAME postgres)
  db=$(hd_env_get "$env_file" DB_DATABASE_NAME immich)
  dbdir=$(hd_env_get "$env_file" DB_DATA_LOCATION "$HD_STATE_DIR/immich-postgres")
  aside="$dbdir.before-restore-$(hd_timestamp)"

  plan "PLAN: replace the Immich database (procedure from the Immich v3 documentation)" \
       "  1. stop Immich:            systemctl stop home-driver-immich.service" \
       "  2. keep the old database:  mv $dbdir $aside" \
       "  3. start an empty Postgres (docker compose create; docker start $container)" \
       "  4. load the dump:          $dump  ->  database '$db'" \
       "  Immich stays STOPPED afterwards so you can check before starting it." \
       "  To undo: stop Immich, delete $dbdir, rename $aside back."
  finish_plan
  hd_require_cmd docker systemctl
  systemctl stop home-driver-immich.service
  if [[ -d "$dbdir" ]]; then mv "$dbdir" "$aside"; fi
  mkdir -p "$dbdir"
  hd_immich_compose create
  docker start "$container" >/dev/null
  hd_log "waiting for Postgres to accept connections"
  local i ok=0
  for (( i = 0; i < 60; i++ )); do
    # --host: during first initialisation the image runs a temporary server that
    # only listens on a socket; answering on TCP means the real server is up.
    if docker exec "$container" pg_isready --host=127.0.0.1 --dbname="$db" --username="$user" >/dev/null 2>&1; then ok=1; break; fi
    sleep 2
  done
  [[ $ok == 1 ]] || hd_die "Postgres did not become ready. Old database is safe at $aside. See: docker logs $container"
  # The sed line and psql flags are the ones documented by Immich for v3.
  if ! gzip -dc "$dump" \
      | sed "s/SELECT pg_catalog.set_config('search_path', '', false);/SELECT pg_catalog.set_config('search_path', 'public, pg_catalog', true);/g" \
      | docker exec -i "$container" psql --dbname="$db" --username="$user" --single-transaction --set ON_ERROR_STOP=on >/dev/null; then
    hd_die "loading the dump FAILED; the new database is empty. Your old database is untouched at $aside. To go back: sudo $HD_SCRIPTS_DIR/immich-compose.sh down; sudo rm -rf $dbdir; sudo mv $aside $dbdir"
  fi
  hd_immich_compose down
  hd_log "Immich database restored from $dump"
  cat <<EOF

NEXT STEPS:
  sudo systemctl start home-driver-immich.service
  Open Immich and check that your photos are there.
  When you are sure, free the space of the old database:
    sudo rm -rf $aside
EOF
}

# ------------------------------------------------- drive-db / curator-db
# restore_sqlite <name> <unit> <env-file> <env-key> <default-path>
restore_sqlite() {
  local name=$1 unit=$2 dbpath dump check ts f
  [[ ${#ARGS[@]} -eq 2 ]] || hd_die "usage: restore.sh $name-db <dump.db>"
  dump=${ARGS[1]}
  dbpath=$(hd_env_get "$3" "$4" "$5")
  [[ -f "$dump" ]] || hd_die "dump file not found: $dump"
  hd_require_cmd sqlite3
  check=$(sqlite3 -readonly "$dump" "PRAGMA integrity_check;" 2>&1 || true)
  [[ "$check" == "ok" ]] || hd_die "$dump is not a healthy SQLite database: ${check:0:200}"
  ts=$(hd_timestamp)

  plan "PLAN: replace the $name database" \
       "  1. stop the service:       systemctl stop $unit" \
       "  2. keep the old database:  mv $dbpath $dbpath.before-restore-$ts" \
       "  3. copy the dump:          $dump  ->  $dbpath" \
       "  The service stays STOPPED afterwards so you can check before starting it." \
       "  To undo: move $dbpath.before-restore-$ts back to $dbpath."
  finish_plan
  if command -v systemctl >/dev/null 2>&1; then systemctl stop "$unit"; fi
  mkdir -p "$(dirname "$dbpath")"
  for f in "$dbpath" "$dbpath-wal" "$dbpath-shm"; do
    if [[ -e "$f" ]]; then mv "$f" "$f.before-restore-$ts"; fi
  done
  install -m 0640 "$dump" "$dbpath"
  if [[ "$EUID" -eq 0 ]] && id -u "$HD_SERVICE_USER" >/dev/null 2>&1; then
    chown "$HD_SERVICE_USER:$HD_SERVICE_USER" "$dbpath"
  fi
  hd_log "$name database restored from $dump"
  echo
  echo "NEXT STEP:  sudo systemctl start $unit"
}

case "$CMD" in
  list-snapshots) cmd_list_snapshots ;;
  file) cmd_file ;;
  disk-a) cmd_disk_a ;;
  disk-b) cmd_disk_b ;;
  offsite) cmd_offsite ;;
  immich-db) cmd_immich_db ;;
  drive-db) restore_sqlite drive home-driver-drive.service "$HD_ETC_DIR/drive.env" DRIVE_DB_PATH "$HD_STATE_DIR/drive/drive.db" ;;
  curator-db) restore_sqlite curator home-driver-curator.service "$HD_ETC_DIR/curator.env" CURATOR_DB_PATH "$HD_STATE_DIR/curator/curator.db" ;;
  *) usage >&2; hd_die "unknown command: $CMD" ;;
esac
