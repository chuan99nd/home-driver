#!/usr/bin/env bash
# shellcheck disable=SC2034
# (file-wide: the constants below are used by the scripts that source this file)
#
# lib.sh - shared helpers for the home-driver scripts. Sourced, never executed.
#
# Everything here is prefixed hd_/HD_. See deploy/CLAUDE.md for the invariants.

# ---- paths (overridable ONLY from the process environment, never from env files) ----
HD_ROOT="${HD_ROOT:-}"
HD_DATA_DIR="${HD_DATA_DIR:-$HD_ROOT/srv/home-driver/data}"
HD_BACKUP_DIR="${HD_BACKUP_DIR:-$HD_ROOT/srv/home-driver/backup}"
HD_STATE_DIR="${HD_STATE_DIR:-$HD_ROOT/var/lib/home-driver}"
HD_ETC_DIR="${HD_ETC_DIR:-$HD_ROOT/etc/home-driver}"
HD_OPT_DIR="${HD_OPT_DIR:-$HD_ROOT/opt/home-driver}"

# ---- constants from the contract ----
HD_SERVICE_USER="home-driver"
HD_SENTINEL=".home-driver-disk"
HD_LABEL_DATA="hd-data"
HD_LABEL_BACKUP="hd-backup"
HD_SUBVOL_DATA="@data"
HD_SUBVOL_BACKUP="@backup"
# Snapshot names are YYYY-MM-DD_HHMMSS. Anything else in snapshots/ is never pruned.
HD_SNAPSHOT_RE='^[0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{6}$'

HD_DRY_RUN="${HD_DRY_RUN:-0}"
HD_ASSUME_YES="${HD_ASSUME_YES:-0}"
HD_SCRIPT_NAME="$(basename "${0:-home-driver}")"
HD_SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---- logging ----
hd_log()  { printf '%s [%s] %s\n' "$(date '+%F %T')" "$HD_SCRIPT_NAME" "$*" >&2; }
hd_warn() { hd_log "WARNING: $*"; }
hd_err()  { _HD_LAST_ERR="$*"; hd_log "ERROR: $*"; }
hd_die()  { hd_err "$*"; exit 1; }

# hd_notify <title> <body> [priority] - never fails, silent in dry-run.
hd_notify() {
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would notify: $1"
    return 0
  fi
  "${HD_NOTIFY_CMD:-$HD_SCRIPTS_DIR/notify.sh}" "$@" || true
}

# hd_fail <message> - log, notify with high priority, exit 1.
hd_fail() {
  hd_err "$*"
  _HD_REPORTED=1
  hd_notify "$HD_SCRIPT_NAME FAILED" "$*" high
  exit 1
}

# hd_alert_on_unexpected_exit: install an EXIT trap that sends an alert when the
# script ends with a non-zero status that hd_fail did not already report
# (a command failing under `set -e`, a missing tool, a busy lock...).
_HD_REPORTED=0
_HD_LAST_ERR=""
_hd_exit_trap() {
  local rc=$?
  trap - EXIT
  if [[ $rc -ne 0 && "$_HD_REPORTED" == 0 ]]; then
    hd_notify "$HD_SCRIPT_NAME FAILED" "Stopped unexpectedly with exit status $rc. ${_HD_LAST_ERR} Run: sudo $HD_OPT_DIR/scripts/doctor.sh" high
  fi
  exit "$rc"
}
hd_alert_on_unexpected_exit() { trap _hd_exit_trap EXIT; }

# hd_run <cmd...> - run the command, or only print it in dry-run mode.
hd_run() {
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would run: $(printf '%q ' "$@")"
    return 0
  fi
  "$@"
}

hd_is_test_mode() { [[ "${HD_TEST_MODE:-0}" == 1 ]]; }

hd_require_root() {
  [[ "$HD_DRY_RUN" == 1 ]] && return 0
  hd_is_test_mode && return 0
  [[ "$EUID" -eq 0 ]] || hd_die "must be run as root (use sudo)"
}

hd_require_cmd() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || hd_die "required command not found: $c (run install.sh)"
  done
}

hd_is_uint() { [[ "${1:-}" =~ ^[0-9]+$ ]]; }

# ---- env files ----
# Env files are KEY=value, one per line, optional surrounding quotes, no inline
# comments (the same subset systemd EnvironmentFile= and docker compose accept).
# We parse instead of `source` so a typo in a config file can never run code.

# Names an env file may never set: they would redirect scripts to other paths.
_hd_env_blocked() {
  case "$1" in
    HD_ROOT|HD_DATA_DIR|HD_BACKUP_DIR|HD_STATE_DIR|HD_ETC_DIR|HD_OPT_DIR|HD_TEST_*|HD_DRY_RUN|HD_ASSUME_YES|HD_NOTIFY_CMD) return 0 ;;
    PATH|IFS|BASH_ENV|ENV|LD_*|SHELLOPTS|BASHOPTS) return 0 ;;
  esac
  return 1
}

# _hd_env_parse_line <line>: sets _HD_KEY/_HD_VAL, returns 1 for comments/blank, 2 for garbage.
_hd_env_parse_line() {
  local line=${1%$'\r'}
  [[ "$line" =~ ^[[:space:]]*(#.*)?$ ]] && return 1
  if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]]; then
    _HD_KEY=${BASH_REMATCH[2]}
    _HD_VAL=${BASH_REMATCH[3]}
    if [[ "$_HD_VAL" =~ ^\"(.*)\"[[:space:]]*$ ]]; then
      _HD_VAL=${BASH_REMATCH[1]}
    elif [[ "$_HD_VAL" =~ ^\'(.*)\'[[:space:]]*$ ]]; then
      _HD_VAL=${BASH_REMATCH[1]}
    else
      _HD_VAL="${_HD_VAL%"${_HD_VAL##*[![:space:]]}"}"
    fi
    return 0
  fi
  return 2
}

# hd_load_env <file>: export every variable in the file. Missing file is not an error.
hd_load_env() {
  local file=$1 line rc
  [[ -r "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    rc=0
    _hd_env_parse_line "$line" || rc=$?
    case $rc in
      0)
        if _hd_env_blocked "$_HD_KEY"; then
          hd_warn "$file: ignoring $_HD_KEY (not allowed in env files)"
        else
          export "$_HD_KEY=$_HD_VAL"
        fi
        ;;
      2) hd_warn "$file: ignoring malformed line: ${line:0:40}" ;;
    esac
  done <"$file"
}

# hd_env_get <file> <KEY> [default]: print one value without exporting anything.
hd_env_get() {
  local file=$1 key=$2 default=${3:-} line val found=0
  if [[ -r "$file" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
      if _hd_env_parse_line "$line" && [[ "$_HD_KEY" == "$key" ]]; then
        val=$_HD_VAL
        found=1
      fi
    done <"$file"
  fi
  if [[ $found == 1 && -n "$val" ]]; then printf '%s\n' "$val"; else printf '%s\n' "$default"; fi
}

# ---- disks ----

# hd_check_disk <role: data|backup> <dir>
# Prints the reason and returns 1 unless <dir> is a mounted btrfs filesystem that
# carries the sentinel file written by setup-disks.sh for that role.
#
# WHY: if a USB disk is unplugged, its mountpoint is just an empty directory on
# the root filesystem. Syncing FROM that would look like "everything was deleted",
# syncing TO it would fill the SSD. The sentinel proves we look at the real disk
# and that A and B are not swapped.
hd_check_disk() {
  local role=$1 dir=$2 fstype
  if [[ ! -d "$dir" ]]; then
    echo "$dir does not exist"
    return 1
  fi
  if mountpoint -q "$dir" 2>/dev/null; then
    if ! hd_is_test_mode; then
      fstype=$(findmnt -n -o FSTYPE -M "$dir" 2>/dev/null || true)
      if [[ "$fstype" != "btrfs" ]]; then
        echo "$dir is mounted but is '$fstype', expected btrfs"
        return 1
      fi
    fi
  elif hd_is_test_mode && [[ "${HD_TEST_ALLOW_NON_MOUNTPOINT:-0}" == 1 ]]; then
    : # tests use plain temp directories
  else
    echo "$dir is not a mounted disk (is the USB disk plugged in? try: sudo mount $dir)"
    return 1
  fi
  if [[ ! -f "$dir/$HD_SENTINEL" ]]; then
    echo "$dir has no $HD_SENTINEL marker file (not a disk prepared by setup-disks.sh, or wrong disk)"
    return 1
  fi
  if ! grep -qx "role=$role" "$dir/$HD_SENTINEL"; then
    echo "$dir/$HD_SENTINEL does not say role=$role (are the two disks swapped?)"
    return 1
  fi
  return 0
}

# hd_list_snapshots: names of date-named snapshots on Disk B, oldest first.
hd_list_snapshots() {
  local d name
  [[ -d "$HD_BACKUP_DIR/snapshots" ]] || return 0
  for d in "$HD_BACKUP_DIR/snapshots"/*/; do
    [[ -d "$d" ]] || continue
    name=$(basename "$d")
    [[ "$name" =~ $HD_SNAPSHOT_RE ]] && printf '%s\n' "$name"
  done | LC_ALL=C sort
}

hd_latest_snapshot() { hd_list_snapshots | tail -n 1; }

# ---- locking ----
# hd_lock <name> [wait-seconds]: take an exclusive lock for the life of the process.
hd_lock() {
  local name=$1 wait=${2:-0} dir="$HD_STATE_DIR/locks"
  [[ "$HD_DRY_RUN" == 1 ]] && return 0
  mkdir -p "$dir"
  exec {_hd_lock_fd}>"$dir/$name.lock"
  if ! flock -w "$wait" "$_hd_lock_fd"; then
    hd_die "another '$name' run is in progress (lock: $dir/$name.lock)"
  fi
}

# ---- status files (read by doctor.sh) ----
# hd_write_status <name> <key=value>...
hd_write_status() {
  local name=$1 dir="$HD_STATE_DIR/status" kv
  shift
  [[ "$HD_DRY_RUN" == 1 ]] && return 0
  mkdir -p "$dir"
  for kv in "$@"; do printf '%s\n' "$kv"; done >"$dir/$name.status.tmp"
  mv -f "$dir/$name.status.tmp" "$dir/$name.status"
}

# hd_status_get <file> <key>
hd_status_get() {
  [[ -r "$1" ]] || return 0
  sed -n "s/^$2=//p" "$1" | tail -n 1
}

# ---- misc ----
# hd_confirm <question>: require the user to type yes, unless --yes was given.
hd_confirm() {
  local answer
  [[ "$HD_ASSUME_YES" == 1 ]] && return 0
  if [[ ! -t 0 ]]; then
    hd_die "confirmation needed but there is no terminal. Review the plan above, then re-run with --yes."
  fi
  read -r -p "$1 Type 'yes' to continue: " answer
  [[ "$answer" == "yes" ]] || hd_die "aborted by user, nothing was changed"
}

# hd_as_service_user <cmd...>: run as the home-driver user when we are root, so
# files such as SQLite -wal/-shm are never left behind owned by root.
hd_as_service_user() {
  if [[ "$EUID" -eq 0 ]] && id -u "$HD_SERVICE_USER" >/dev/null 2>&1; then
    runuser -u "$HD_SERVICE_USER" -- "$@"
  else
    "$@"
  fi
}

# hd_immich_compose <args...>: the one way scripts invoke docker compose for Immich.
hd_immich_compose() { "$HD_SCRIPTS_DIR/immich-compose.sh" "$@"; }

hd_timestamp() { date '+%Y-%m-%d_%H%M%S'; }

# hd_age_hours <epoch>: whole hours since epoch.
hd_age_hours() { echo $(( ($(date +%s) - $1) / 3600 )); }
