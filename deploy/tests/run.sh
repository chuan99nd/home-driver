#!/usr/bin/env bash
# deploy/tests/run.sh - test runner for the deploy component.
# Needs no root and no real disks: scripts run against temp directories with
# HD_* path overrides, and btrfs/docker/restic/smartctl/... are stubs on PATH.
#
# shellcheck disable=SC2010,SC2012,SC2016,SC2030,SC2031,SC2034,SC2119,SC2120,SC2329
# (ls for counting sandbox entries, single-quoted conditions handed to eval,
#  variables and functions only used inside those eval strings, and per-test
#  variables set inside subshells are all intentional in this file.)
#
# Usage: deploy/tests/run.sh [-v] [filter]
#   -v       show the output of failing commands in full
#   filter   only run behavioural tests whose name contains this text
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="$(dirname "$TESTS_DIR")"
SCRIPTS="$DEPLOY/scripts"
STUBS="$TESTS_DIR/stubs"
STUBS_DISK="$TESTS_DIR/stubs-disk"
ORIG_PATH=$PATH

VERBOSE=0
FILTER=""
for a in "$@"; do
  case "$a" in
    -v) VERBOSE=1 ;;
    -h|--help) sed -n '2,9p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) FILTER=$a ;;
  esac
done

N_PASS=0; N_FAIL=0; N_SKIP=0
FAILED=()
SKIPPED=()
pass() { N_PASS=$((N_PASS + 1)); printf '  ok    %s\n' "$1"; }
fail() { N_FAIL=$((N_FAIL + 1)); FAILED+=("$1"); printf '  FAIL  %s\n' "$1"; [[ -n "${2:-}" ]] && printf '%s\n' "$2" | sed 's/^/        | /' | { if [[ $VERBOSE == 1 ]]; then cat; else head -n 15; fi; }; return 0; }
skip() { N_SKIP=$((N_SKIP + 1)); SKIPPED+=("$1: $2"); printf '  SKIP  %s (%s)\n' "$1" "$2"; }
section() { printf '\n== %s\n' "$1"; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/hd-deploy-tests.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

###############################################################################
section "Static: bash -n"
for f in "$SCRIPTS"/*.sh "$TESTS_DIR"/run.sh "$STUBS"/* "$STUBS_DISK"/*; do
  [[ -L "$f" ]] && continue
  if out=$(bash -n "$f" 2>&1); then pass "bash -n ${f#"$DEPLOY"/}"; else fail "bash -n ${f#"$DEPLOY"/}" "$out"; fi
done

###############################################################################
section "Static: shellcheck"
SC_FILES=("$SCRIPTS"/*.sh "$TESTS_DIR"/run.sh)
SHELLCHECK=()
if command -v shellcheck >/dev/null 2>&1; then
  SHELLCHECK=(shellcheck)
elif command -v uvx >/dev/null 2>&1 && uvx --quiet --from shellcheck-py shellcheck --version >/dev/null 2>&1; then
  SHELLCHECK=(uvx --quiet --from shellcheck-py shellcheck)
elif command -v docker >/dev/null 2>&1 && docker image inspect koalaman/shellcheck:stable >/dev/null 2>&1; then
  SHELLCHECK=(docker run --rm -v "$DEPLOY:$DEPLOY:ro" -w "$SCRIPTS" koalaman/shellcheck:stable)
elif command -v docker >/dev/null 2>&1 && docker pull -q koalaman/shellcheck:stable >/dev/null 2>&1; then
  SHELLCHECK=(docker run --rm -v "$DEPLOY:$DEPLOY:ro" -w "$SCRIPTS" koalaman/shellcheck:stable)
fi
if (( ${#SHELLCHECK[@]} == 0 )); then
  skip "shellcheck" "NOT RUN: no shellcheck binary, no working 'uvx --from shellcheck-py', no koalaman/shellcheck docker image. Install one of them."
else
  echo "  (using: ${SHELLCHECK[*]})"
  for f in "${SC_FILES[@]}"; do
    if out=$(cd "$SCRIPTS" && "${SHELLCHECK[@]}" -x -S info "$f" 2>&1); then
      pass "shellcheck ${f#"$DEPLOY"/}"
    else
      fail "shellcheck ${f#"$DEPLOY"/}" "$out"
    fi
  done
fi

###############################################################################
section "Static: systemd units"
if ! command -v systemd-analyze >/dev/null 2>&1; then
  skip "systemd-analyze verify" "systemd-analyze not installed"
else
  # systemd-analyze insists that ExecStart= binaries exist. They live in
  # /opt/home-driver, which does not exist on a dev box, so verify a copy of the
  # units in which that prefix points at a directory of empty executables.
  # Everything else about the units is checked unmodified.
  U="$WORK/units"; FAKEOPT="$WORK/fakeopt"
  mkdir -p "$U" "$FAKEOPT/bin" "$FAKEOPT/scripts" "$FAKEOPT/curator/.venv/bin"
  for f in "$SCRIPTS"/*.sh; do install -m 0755 /dev/null "$FAKEOPT/scripts/$(basename "$f")"; done
  install -m 0755 /dev/null "$FAKEOPT/bin/drive"
  install -m 0755 /dev/null "$FAKEOPT/curator/.venv/bin/home-driver-curator"
  for f in "$DEPLOY"/systemd/*.service "$DEPLOY"/systemd/*.timer; do
    sed "s#/opt/home-driver/\(bin\|scripts\|curator\)#$FAKEOPT/\1#g" "$f" >"$U/$(basename "$f")"
  done
  for f in "$U"/*; do
    name=$(basename "$f")
    raw=$(systemd-analyze verify --man=no "$f" 2>&1)
    # Keep only messages about OUR units: the host's own unit problems are noise.
    # A dev box without Docker has no docker.service; that is not a unit bug.
    out=$(grep -E "home-driver-" <<<"$raw" | grep -v -E "Unit docker\.service not found" || true)
    if [[ -z "$out" ]]; then pass "systemd-analyze verify $name"; else fail "systemd-analyze verify $name" "$out"; fi
  done
  # Every ExecStart must point at something this repo (or a sibling component) installs.
  bad=""
  while IFS= read -r exe; do
    case "$exe" in
      /opt/home-driver/scripts/*) [[ -f "$SCRIPTS/$(basename "$exe")" ]] || bad+="$exe " ;;
      /opt/home-driver/bin/drive|/opt/home-driver/curator/.venv/bin/home-driver-curator) ;;
      *) bad+="$exe " ;;
    esac
  done < <(sed -n 's/^Exec[A-Za-z]*=[-@+!:]*\([^ ]*\).*/\1/p' "$DEPLOY"/systemd/*.service | sort -u)
  if [[ -z "$bad" ]]; then pass "every ExecStart= points at an installed file"; else fail "ExecStart= points at unknown files" "$bad"; fi
  # Contract checks that systemd-analyze cannot know about.
  bad=""
  for t in "$DEPLOY"/systemd/*.timer; do grep -q '^Persistent=true' "$t" || bad+="$(basename "$t") lacks Persistent=true; "; done
  for s in backup-local backup-offsite scrub smart-check; do
    grep -q '^OnFailure=home-driver-notify-failure@%n.service' "$DEPLOY/systemd/home-driver-$s.service" || bad+="$s lacks OnFailure; "
  done
  for s in immich drive; do
    grep -q '^RequiresMountsFor=/srv/home-driver/data' "$DEPLOY/systemd/home-driver-$s.service" || bad+="$s lacks RequiresMountsFor; "
  done
  for s in drive curator curator-scan; do
    grep -q '^User=home-driver' "$DEPLOY/systemd/home-driver-$s.service" || bad+="$s does not run as home-driver; "
  done
  grep -c 'nofail,x-systemd.device-timeout=30' "$DEPLOY/systemd/fstab.snippet" | grep -qx 2 || bad+="fstab.snippet must have nofail + device-timeout on both disks; "
  if [[ -z "$bad" ]]; then pass "units follow the contract (Persistent, OnFailure, RequiresMountsFor, User, nofail)"; else fail "units break the contract" "$bad"; fi
fi

###############################################################################
section "Static: docker compose"
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  skip "docker compose config" "docker compose not installed"
else
  ex="$DEPLOY/config/immich.env.example"
  if out=$(IMMICH_ENV_FILE="$ex" docker compose --env-file "$ex" -f "$DEPLOY/immich/docker-compose.yml" config 2>&1); then
    pass "docker compose config (immich/docker-compose.yml)"
    if grep -q 'host_ip: 127.0.0.1' <<<"$out" && ! grep -E 'host_ip: (0\.0\.0\.0|"")' <<<"$out" && [[ $(grep -c 'published:' <<<"$out") == 1 ]]; then
      pass "Immich publishes exactly one port, on 127.0.0.1"
    else
      fail "Immich port is not published on 127.0.0.1 only" "$(grep -B3 -A3 published <<<"$out")"
    fi
    if grep -E 'image: ghcr.io/immich-app/immich-server:v[0-9]+\.[0-9]+\.[0-9]+$' <<<"$out" >/dev/null; then
      pass "Immich version is pinned to an exact release"
    else
      fail "Immich version is not pinned" "$(grep 'image:' <<<"$out")"
    fi
    if grep -q 'source: /srv/home-driver/data/immich' <<<"$out" && grep -q 'source: /var/lib/home-driver/immich-postgres' <<<"$out"; then
      pass "Immich uploads on Disk A, database on the SSD"
    else
      fail "Immich volume locations are wrong" "$(grep 'source:' <<<"$out")"
    fi
  else
    fail "docker compose config (immich/docker-compose.yml)" "$out"
  fi
  if out=$(IMMICH_LAN_ADDR=192.168.1.50 IMMICH_ENV_FILE="$ex" docker compose --env-file "$ex" -f "$DEPLOY/immich/docker-compose.yml" -f "$DEPLOY/immich/docker-compose.lan.yml" config 2>&1); then
    pass "docker compose config (with docker-compose.lan.yml)"
  else
    fail "docker compose config (with docker-compose.lan.yml)" "$out"
  fi
  if IMMICH_ENV_FILE="$ex" docker compose --env-file /dev/null -f "$DEPLOY/immich/docker-compose.yml" config >/dev/null 2>&1; then
    fail "compose must fail when IMMICH_VERSION is unset (no floating default)"
  else
    pass "compose refuses to run without IMMICH_VERSION"
  fi
fi

###############################################################################
section "Static: config examples"
bad=""
for f in common immich drive curator backup; do [[ -f "$DEPLOY/config/$f.env.example" ]] || bad+="$f.env.example missing; "; done
# Every variable the scripts read from the environment must be documented.
for v in $(grep -ohE '\$\{?(BACKUP|NOTIFY|RESTIC)_[A-Z0-9_]+' "$SCRIPTS"/*.sh | tr -d '${' | sort -u); do
  case "$v" in RESTIC_CACHE_DIR|RESTIC_PASSWORD_FILE|RESTIC_PASSWORD_COMMAND|RESTIC_HOST|RESTIC_GLOBAL|RESTIC_SNAPSHOT|BACKUP_IMMICH_DB_CONTAINER) continue ;; esac
  grep -qE "^#? ?$v=" "$DEPLOY"/config/*.env.example || bad+="$v not in any env example; "
done
if [[ -z "$bad" ]]; then pass "all env examples exist and document every variable the scripts read"; else fail "env examples incomplete" "$bad"; fi

###############################################################################
# Behavioural tests
###############################################################################
SB=""   # sandbox of the current test

# new_sandbox: fresh fake server under $SB with both "disks" prepared.
new_sandbox() {
  SB=$(mktemp -d "$WORK/sb.XXXXXX")
  mkdir -p "$SB/data/immich/library" "$SB/data/drive/blobs" "$SB/data/drive/tmp" \
           "$SB/backup/current" "$SB/backup/snapshots" \
           "$SB/state/drive" "$SB/state/curator" "$SB/etc" "$SB/opt/scripts" "$SB/tmp"
  printf 'role=data\n' >"$SB/data/.home-driver-disk"
  printf 'role=backup\n' >"$SB/backup/.home-driver-disk"
  echo "photo-1" >"$SB/data/immich/library/a.jpg"
  echo "photo-2" >"$SB/data/immich/library/b.jpg"
  echo "blob" >"$SB/data/drive/blobs/x"
  echo "partial" >"$SB/data/drive/tmp/upload.part"
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$SB/state/drive/drive.db" "CREATE TABLE files(id INTEGER, name TEXT); INSERT INTO files VALUES (1, 'hello');"
    sqlite3 "$SB/state/curator/curator.db" "CREATE TABLE q(id INTEGER);"
  fi
  cat >"$SB/etc/common.env" <<EOF
HD_DOMAIN=example.test
NOTIFY_METHOD=telegram
NOTIFY_TELEGRAM_BOT_TOKEN=000:TESTTOKEN
NOTIFY_TELEGRAM_CHAT_ID=42
EOF
  cat >"$SB/etc/backup.env" <<EOF
BACKUP_KEEP_DAILY=14
BACKUP_KEEP_WEEKLY=8
BACKUP_KEEP_MONTHLY=12
BACKUP_DISK_WARN_PERCENT=101
BACKUP_DISK_FAIL_PERCENT=101
EOF
  printf 'IMMICH_VERSION=v0.0.0\nDB_USERNAME=postgres\nDB_DATABASE_NAME=immich\n' >"$SB/etc/immich.env"
  : >"$SB/etc/drive.env"; : >"$SB/etc/curator.env"
  chmod 600 "$SB"/etc/*.env
  : >"$SB/stub.log"
}

# run_hd <script> [args...]: run a script inside the sandbox; sets RC and OUT.
RC=0; OUT=""
run_hd() {
  local script=$1; shift
  OUT=$(
    export HD_TEST_MODE=1
    export HD_TEST_ALLOW_NON_MOUNTPOINT="${ALLOW_NON_MOUNTPOINT:-1}"
    export HD_DATA_DIR="$SB/data" HD_BACKUP_DIR="$SB/backup" HD_STATE_DIR="$SB/state"
    export HD_ETC_DIR="$SB/etc" HD_OPT_DIR="$SB/opt"
    export STUB_LOG="$SB/stub.log" TMPDIR="$SB/tmp"
    export PATH="${EXTRA_PATH:+$EXTRA_PATH:}$STUBS:$ORIG_PATH"
    "$SCRIPTS/$script" "$@" 2>&1 </dev/null
  )
  RC=$?
}

# fingerprint <dir>: changes whenever any file is added, removed or modified.
fingerprint() {
  (cd "$1" && find . -path ./stub.log -prune -o -printf '%p|%y|%s|%m|%T@|%l\n' | LC_ALL=C sort | sha256sum | cut -d' ' -f1)
}
stub_called() { grep -q -- "$1" "$SB/stub.log"; }

# check <name> <condition...>: pass/fail a named test from a condition.
check() {
  local name=$1; shift
  if "$@"; then pass "$name"; else fail "$name" "exit=$RC"$'\n'"$OUT"; fi
}
want_test() { [[ -z "$FILTER" || "$1" == *"$FILTER"* ]]; }
HAVE_SQLITE=0; command -v sqlite3 >/dev/null 2>&1 && HAVE_SQLITE=1

# ----------------------------------------------------------------------- help
if want_test help; then
  section "Behaviour: --help"
  for f in "$SCRIPTS"/*.sh; do
    n=$(basename "$f"); [[ $n == lib.sh ]] && continue
    new_sandbox; run_hd "$n" --help
    check "$n --help prints usage and exits 0" eval '[[ $RC == 0 && "$OUT" == *Usage:* ]]'
  done
fi

# ------------------------------------------------------------------ env parser
if want_test env; then
  section "Behaviour: env file parser (lib.sh)"
  new_sandbox
  cat >"$SB/etc/evil.env" <<EOF
# comment
GOOD=plain value
QUOTED="with spaces"
EVIL=\$(touch $SB/pwned)
HD_DATA_DIR=/
PATH=/nowhere
this is not an assignment
EOF
  OUT=$(HD_ETC_DIR="$SB/etc" bash -c 'source "$1/lib.sh"; hd_load_env "$2/evil.env"; echo "GOOD=$GOOD|QUOTED=$QUOTED|DATA=$HD_DATA_DIR|$(hd_env_get "$2/evil.env" QUOTED)"; command -v ls >/dev/null && echo PATH_OK' _ "$SCRIPTS" "$SB/etc" 2>&1); RC=$?
  check "env files are parsed, never executed" eval '[[ ! -e "$SB/pwned" && "$OUT" == *"GOOD=plain value|QUOTED=with spaces|"* ]]'
  check "env files cannot redirect HD_* paths or PATH" eval '[[ "$OUT" != *"DATA=/|"* && "$OUT" == *PATH_OK* ]]'
fi

# --------------------------------------------------------------------- notify
if want_test notify; then
  section "Behaviour: notify.sh"
  new_sandbox; sed -i 's/^NOTIFY_METHOD=.*/NOTIFY_METHOD=/' "$SB/etc/common.env"
  run_hd notify.sh "t" "b"
  check "notify: unconfigured is a logged no-op, exit 0" eval '[[ $RC == 0 && "$OUT" == *"not configured"* ]] && ! stub_called "^curl"'
  new_sandbox; run_hd notify.sh "disk failing" "replace it" high
  check "notify: telegram calls the Bot API" eval '[[ $RC == 0 ]] && stub_called "api.telegram.org/bot000:TESTTOKEN/sendMessage"'
  new_sandbox; printf 'NOTIFY_METHOD=ntfy\nNOTIFY_NTFY_URL=https://ntfy.example.test/topic\n' >"$SB/etc/common.env"
  run_hd notify.sh "t" "b" high
  check "notify: ntfy posts to the topic URL with priority 5" eval '[[ $RC == 0 ]] && stub_called "Priority: 5" && stub_called "https://ntfy.example.test/topic"'
  new_sandbox; STUB_CURL_RC=7 run_hd notify.sh "t" "b"
  check "notify: network failure never fails the caller" eval '[[ $RC == 0 && "$OUT" == *"failed"* ]]'
  new_sandbox; run_hd notify.sh
  check "notify: missing arguments still exit 0" eval '[[ $RC == 0 ]]'
  new_sandbox; printf 'NOTIFY_METHOD=carrier-pigeon\n' >"$SB/etc/common.env"; run_hd notify.sh "t" "b"
  check "notify: unknown method exits 0 without sending" eval '[[ $RC == 0 ]] && ! stub_called "^curl"'
fi

# --------------------------------------------------------------- backup-local
if want_test backup-local; then
  section "Behaviour: backup-local.sh"
  new_sandbox; run_hd backup-local.sh
  snap=$(ls "$SB/backup/snapshots" | head -n 1)
  check "backup: happy path exits 0" eval '[[ $RC == 0 ]]'
  check "backup: files are mirrored to current/" eval '[[ "$(cat "$SB/backup/current/immich/library/a.jpg" 2>/dev/null)" == photo-1 && -f "$SB/backup/current/drive/blobs/x" ]]'
  check "backup: a date-named read-only snapshot is taken" eval '[[ "$snap" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{6}$ ]] && stub_called "btrfs subvolume snapshot -r $SB/backup/current $SB/backup/snapshots/$snap" && [[ -f "$SB/backup/snapshots/$snap/immich/library/b.jpg" ]]'
  check "backup: Immich dump is taken with pg_dump, verified, copied to Disk A and B" eval 'stub_called "docker exec immich_postgres pg_dump --clean --if-exists --dbname=immich --username=postgres" && ls "$SB"/state/dumps/immich-*.sql.gz "$SB"/data/dumps/immich-*.sql.gz "$SB"/backup/current/dumps/immich-*.sql.gz >/dev/null 2>&1'
  if [[ $HAVE_SQLITE == 1 ]]; then
    d=$(ls "$SB"/state/dumps/drive-*.db 2>/dev/null | head -n 1)
    check "backup: Drive SQLite backup is a valid database with the data" eval '[[ -n "$d" && "$(sqlite3 "$d" "SELECT name FROM files")" == hello ]] && ls "$SB"/state/dumps/curator-*.db >/dev/null'
  else
    skip "backup: SQLite dump content" "sqlite3 not installed"
  fi
  check "backup: env files are saved with the dumps, mode 600" eval '[[ "$(stat -c %a "$SB/state/dumps/config/common.env")" == 600 ]]'
  check "backup: upload temp files and the marker are not mirrored" eval '[[ ! -e "$SB/backup/current/drive/tmp/upload.part" && ! -e "$SB/backup/current/.home-driver-disk" ]]'
  check "backup: Disk B keeps its own marker" eval 'grep -qx role=backup "$SB/backup/.home-driver-disk"'
  check "backup: status file records success" eval 'grep -q "^result=ok" "$SB/state/status/backup-local.status" && grep -q "^last_success_epoch=[0-9]" "$SB/state/status/backup-local.status"'
  check "backup: no failure alert on success" eval '! stub_called "^curl"'
  sleep 1; run_hd backup-local.sh
  check "backup: safe to run twice (second run exits 0, second snapshot)" eval '[[ $RC == 0 && $(ls "$SB/backup/snapshots" | wc -l) == 2 ]]'

  # deletion is mirrored, but the earlier snapshot still has the file
  rm "$SB/data/immich/library/a.jpg"; sleep 1; run_hd backup-local.sh
  check "backup: a deleted file leaves current/ but stays in the older snapshot" eval '[[ $RC == 0 && ! -e "$SB/backup/current/immich/library/a.jpg" && -f "$SB/backup/snapshots/$snap/immich/library/a.jpg" ]]'

  new_sandbox; mkdir -p "$SB/backup/current/immich"; echo old >"$SB/backup/current/immich/old.jpg"
  rm "$SB/data/.home-driver-disk"; before=$(fingerprint "$SB/backup"); run_hd backup-local.sh
  check "backup: REFUSES when Disk A has no marker file" eval '[[ $RC != 0 && "$OUT" == *"marker"* && "$(fingerprint "$SB/backup")" == "$before" ]]'
  check "backup: refusal sends an alert" stub_called "sendMessage"

  new_sandbox; rm "$SB/backup/.home-driver-disk"; run_hd backup-local.sh
  check "backup: REFUSES when Disk B has no marker file" eval '[[ $RC != 0 && "$OUT" == *"Disk B"* ]] && ! stub_called "btrfs subvolume snapshot"'

  new_sandbox; printf 'role=backup\n' >"$SB/data/.home-driver-disk"; printf 'role=data\n' >"$SB/backup/.home-driver-disk"; run_hd backup-local.sh
  check "backup: REFUSES when the two disks are swapped" eval '[[ $RC != 0 && "$OUT" == *"swapped"* ]]'

  new_sandbox; echo old >"$SB/backup/current/old.jpg"; before=$(fingerprint "$SB/backup")
  ALLOW_NON_MOUNTPOINT=0 run_hd backup-local.sh
  check "backup: REFUSES when a disk directory is not a mountpoint" eval '[[ $RC != 0 && "$OUT" == *"not a mounted disk"* && "$(fingerprint "$SB/backup")" == "$before" ]]'

  # a replaced, empty Disk A (marker present) must not wipe the mirror
  new_sandbox; run_hd backup-local.sh
  rm -rf "$SB/data/immich" "$SB/data/drive" "$SB/data/dumps"; before=$(fingerprint "$SB/backup/current"); sleep 1; run_hd backup-local.sh
  check "backup: REFUSES to mirror an empty Disk A over a full backup" eval '[[ $RC != 0 && "$OUT" == *"no files"* && "$(fingerprint "$SB/backup/current")" == "$before" ]]'

  # mass deletion brake
  new_sandbox; for i in $(seq 1 400); do echo "$i" >"$SB/data/immich/library/p$i.jpg"; done; run_hd backup-local.sh
  rm "$SB"/data/immich/library/p[1-3]*.jpg; before=$(fingerprint "$SB/backup/current"); sleep 1; run_hd backup-local.sh
  check "backup: REFUSES a sync that would delete >20% of the backup" eval '[[ $RC != 0 && "$OUT" == *"--allow-mass-delete"* && "$(fingerprint "$SB/backup/current")" == "$before" ]]'
  sleep 1; run_hd backup-local.sh --allow-mass-delete
  check "backup: --allow-mass-delete lets it through, old snapshot keeps the files" eval '[[ $RC == 0 && ! -e "$SB/backup/current/immich/library/p1.jpg" ]] && ls "$SB"/backup/snapshots/*/immich/library/p1.jpg >/dev/null'

  new_sandbox; before=$(fingerprint "$SB"); run_hd backup-local.sh --dry-run
  check "backup: --dry-run changes nothing at all" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "subvolume snapshot" && ! stub_called "^curl" && ! stub_called "docker exec"'

  # retention
  new_sandbox
  for i in $(seq 1 60); do mkdir "$SB/backup/snapshots/$(date -d "2026-03-01 +$i days" +%F)_023000"; done
  mkdir "$SB/backup/snapshots/keep-me-manual"
  printf 'BACKUP_KEEP_DAILY=3\nBACKUP_KEEP_WEEKLY=2\nBACKUP_KEEP_MONTHLY=3\n' >"$SB/etc/backup.env"
  run_hd backup-local.sh
  kept=$(ls "$SB/backup/snapshots" | grep -c '^[0-9]')
  check "backup: retention prunes old snapshots (61 snapshots -> 4: today, 2 more days, 1 older month)" eval '[[ $RC == 0 && $kept == 4 ]]'
  check "backup: retention keeps the newest and never touches hand-made snapshots" eval '[[ -d "$SB/backup/snapshots/keep-me-manual" && -n "$(ls "$SB/backup/snapshots" | grep "^$(date +%F)_")" ]]'
  check "backup: retention keeps one snapshot from an older month" eval '[[ -n "$(ls "$SB/backup/snapshots" | grep "^2026-03-31_")" ]]'

  new_sandbox; printf 'BACKUP_KEEP_DAILY=0\n' >"$SB/etc/backup.env"; run_hd backup-local.sh
  check "backup: REFUSES a retention of zero" eval '[[ $RC != 0 ]] && ! stub_called "subvolume snapshot"'
  new_sandbox; printf 'BACKUP_KEEP_DAILY=abc\n' >"$SB/etc/backup.env"; run_hd backup-local.sh
  check "backup: REFUSES a non-numeric retention" eval '[[ $RC != 0 ]] && ! stub_called "subvolume snapshot"'

  # interrupted previous run
  new_sandbox; echo keep >"$SB/backup/current/only-in-backup.txt"; : >"$SB/backup/.sync-not-yet-snapshotted"; run_hd backup-local.sh
  check "backup: after an interrupted run, snapshots the old state BEFORE deleting" eval '[[ $RC == 0 && $(ls "$SB/backup/snapshots" | wc -l) == 2 && ! -e "$SB/backup/.sync-not-yet-snapshotted" ]] && ls "$SB"/backup/snapshots/*/only-in-backup.txt >/dev/null'

  new_sandbox; STUB_DOCKER_FAIL=1 run_hd backup-local.sh
  check "backup: a failed DB dump is reported (exit 1, alert) but files are still backed up" eval '[[ $RC != 0 && $(ls "$SB/backup/snapshots" | wc -l) == 1 && -f "$SB/backup/current/immich/library/a.jpg" ]] && stub_called "sendMessage" && grep -q "^result=errors" "$SB/state/status/backup-local.status"'

  new_sandbox; STUB_BTRFS_SNAPSHOT_FAIL=1 run_hd backup-local.sh
  check "backup: a failed snapshot fails the run and leaves the repair marker" eval '[[ $RC != 0 && -e "$SB/backup/.sync-not-yet-snapshotted" ]] && stub_called "sendMessage"'

  new_sandbox; mkdir -p "$SB/state/locks"
  ( exec 9>"$SB/state/locks/backup-local.lock"; flock 9; sleep 3 ) &
  sleep 0.5; run_hd backup-local.sh; wait
  check "backup: refuses to run twice at the same time" eval '[[ $RC != 0 && "$OUT" == *"in progress"* ]]'
fi

# -------------------------------------------------------------- backup-offsite
if want_test offsite; then
  section "Behaviour: backup-offsite.sh"
  new_sandbox; run_hd backup-offsite.sh
  check "offsite: not configured -> clean exit 0, restic never called" eval '[[ $RC == 0 && "$OUT" == *"not configured"* ]] && ! stub_called "^restic"'
  new_sandbox; printf 'RESTIC_REPOSITORY=s3:https://example.test/bucket\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"; run_hd backup-offsite.sh
  check "offsite: runs backup, forget --prune and check" eval '[[ $RC == 0 ]] && stub_called "restic backup --host home-driver" && stub_called "forget --host home-driver --tag home-driver --prune --keep-daily 7" && stub_called "^restic check" && grep -q "^result=ok" "$SB/state/status/backup-offsite.status"'
  check "offsite: excludes regenerable thumbnails by default" stub_called "--exclude $SB/data/immich/thumbs/\* --exclude !$SB/data/immich/thumbs/.immich"
  : >"$SB/stub.log"; run_hd backup-offsite.sh
  check "offsite: check is periodic, not every night" eval '[[ $RC == 0 ]] && stub_called "restic backup" && ! stub_called "^restic check"'
  new_sandbox; printf 'RESTIC_REPOSITORY=x\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"; rm "$SB/data/.home-driver-disk"; run_hd backup-offsite.sh
  check "offsite: REFUSES to upload when Disk A has no marker" eval '[[ $RC != 0 ]] && ! stub_called "restic backup" && stub_called "sendMessage"'
  new_sandbox; printf 'RESTIC_REPOSITORY=x\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"; STUB_RESTIC_NO_REPO=1 run_hd backup-offsite.sh
  check "offsite: never creates a repository on its own" eval '[[ $RC != 0 && "$OUT" == *"--init"* ]] && ! stub_called "restic init" && ! stub_called "restic backup"'
  new_sandbox; printf 'RESTIC_REPOSITORY=x\n' >>"$SB/etc/backup.env"; run_hd backup-offsite.sh
  check "offsite: REFUSES without a password" eval '[[ $RC != 0 ]] && ! stub_called "^restic"'
  new_sandbox; printf 'RESTIC_REPOSITORY=x\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"; before=$(fingerprint "$SB"); run_hd backup-offsite.sh --dry-run
  check "offsite: --dry-run changes nothing and uploads nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "restic backup" && ! stub_called "restic forget"'
  new_sandbox; printf 'RESTIC_REPOSITORY=x\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"; STUB_RESTIC_BACKUP_RC=1 run_hd backup-offsite.sh
  check "offsite: a failed upload exits non-zero, alerts, and does not prune" eval '[[ $RC != 0 ]] && stub_called "sendMessage" && ! stub_called "restic forget"'
fi

# ----------------------------------------------------------------- setup-disks
if want_test setup-disks; then
  section "Behaviour: setup-disks.sh"
  DESTRUCTIVE='^(mkfs.btrfs|wipefs|mount|btrfs subvolume create)'
  sd() { EXTRA_PATH="$STUBS_DISK" HD_TEST_FAKE_BLOCKDEV=1 run_hd setup-disks.sh "$@"; }
  export HD_TEST_FAKE_BLOCKDEV=1
  new_sandbox; sd
  check "setup-disks: REFUSES without --device (no default device)" eval '[[ $RC != 0 && "$OUT" == *"no default device"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakeusb --yes-wipe-this-disk
  check "setup-disks: REFUSES without --role" eval '[[ $RC != 0 ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakeusb --role data
  check "setup-disks: REFUSES without --yes-wipe-this-disk" eval '[[ $RC != 0 && "$OUT" == *"--yes-wipe-this-disk"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  check "setup-disks: shows the disk before refusing" eval '[[ "$OUT" == *"DISK TO BE ERASED"* ]]'
  new_sandbox; sd --device /dev/fakeroot --role data --yes-wipe-this-disk --allow-non-usb
  check "setup-disks: REFUSES the disk holding the root filesystem" eval '[[ $RC != 0 && "$OUT" == *"running system"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakeroot1 --role data --yes-wipe-this-disk --allow-non-usb
  check "setup-disks: REFUSES a partition (whole disks only)" eval '[[ $RC != 0 && "$OUT" == *"WHOLE disk"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; STUB_ROOT_SOURCE="" sd --device /dev/fakeusb --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES when the root disk cannot be determined" eval '[[ $RC != 0 && "$OUT" == *"too risky"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; STUB_ROOT_SOURCE="overlay" sd --device /dev/fakeusb --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES when root is not on a recognisable disk" eval '[[ $RC != 0 && "$OUT" == *"too risky"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; STUB_USB_MOUNT=/mnt/something sd --device /dev/fakeusb --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES a mounted disk" eval '[[ $RC != 0 && "$OUT" == *"in use"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakesata --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES a non-USB disk by default" eval '[[ $RC != 0 && "$OUT" == *"not USB"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; STUB_USB_LABEL=hd-data sd --device /dev/fakeusb --role backup --yes-wipe-this-disk
  check "setup-disks: REFUSES a disk that already is a home-driver disk" eval '[[ $RC != 0 && "$OUT" == *"already is a home-driver disk"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; STUB_OTHER_LABEL=hd-backup sd --device /dev/fakeusb --role backup --yes-wipe-this-disk
  check "setup-disks: REFUSES when another disk already has the label" eval '[[ $RC != 0 && "$OUT" == *"already has the label"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device fakeusb --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES a device path outside /dev" eval '[[ $RC != 0 ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakeusb --role data --dry-run
  check "setup-disks: --dry-run shows the plan and erases nothing" eval '[[ $RC == 0 && "$OUT" == *"mkfs.btrfs -L hd-data"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  new_sandbox; sd --device /dev/fakeusb --role backup --yes-wipe-this-disk
  mnt=$(ls -d "$SB"/tmp/tmp.* 2>/dev/null | head -n 1)
  check "setup-disks: with every flag, wipes then formats with the right label" eval '[[ $RC == 0 ]] && grep -q "^wipefs -a /dev/fakeusb$" "$SB/stub.log" && grep -q "^mkfs.btrfs -f -L hd-backup -m dup -d single /dev/fakeusb$" "$SB/stub.log" && [[ $(grep -n "^wipefs" "$SB/stub.log" | head -n1 | cut -d: -f1) -lt $(grep -n "^mkfs" "$SB/stub.log" | cut -d: -f1) ]]'
  check "setup-disks: creates @backup with current/, snapshots/ and the marker" eval '[[ -d "$mnt/@backup/current" && -d "$mnt/@backup/snapshots" ]] && grep -qx role=backup "$mnt/@backup/.home-driver-disk"'
  unset HD_TEST_FAKE_BLOCKDEV

  # The same refusal against this machine's REAL root disk, with real lsblk/findmnt.
  new_sandbox
  real_src=$(findmnt -n -o SOURCE / 2>/dev/null | head -n 1); real_src=${real_src%%\[*}
  real_disk=""
  [[ "$real_src" == /dev/* ]] && real_disk=$(lsblk -snrpo NAME,TYPE "$real_src" 2>/dev/null | awk '$2 == "disk" {print $1; exit}')
  if [[ -n "$real_disk" && -b "$real_disk" ]]; then
    run_hd setup-disks.sh --device "$real_disk" --role data --yes-wipe-this-disk --allow-non-usb --overwrite-home-driver-disk
    check "setup-disks: REFUSES this machine's real system disk ($real_disk)" eval '[[ $RC != 0 && "$OUT" == *"running system"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
  else
    skip "setup-disks: real root disk refusal" "could not determine this machine's root disk (root source: ${real_src:-unknown})"
  fi
  new_sandbox; run_hd setup-disks.sh --device /dev/does-not-exist --role data --yes-wipe-this-disk
  check "setup-disks: REFUSES a device that does not exist" eval '[[ $RC != 0 && "$OUT" == *"not a block device"* ]] && ! grep -qE "$DESTRUCTIVE" "$SB/stub.log"'
fi

# --------------------------------------------------------------------- restore
if want_test restore; then
  section "Behaviour: restore.sh"
  # a sandbox with two snapshots: the first has a.jpg, the second does not
  restore_sandbox() {
    new_sandbox; run_hd backup-local.sh; S1=$(ls "$SB/backup/snapshots" | head -n 1)
    rm "$SB/data/immich/library/a.jpg"; sleep 1; run_hd backup-local.sh; S2=$(ls "$SB/backup/snapshots" | tail -n 1)
    : >"$SB/stub.log"
    export STUB_SYSTEMCTL_ACTIVE=inactive
  }
  restore_sandbox; run_hd restore.sh list-snapshots
  check "restore list-snapshots: lists both snapshots" eval '[[ $RC == 0 && "$OUT" == *"$S1"* && "$OUT" == *"Newest: $S2"* ]]'

  before=$(fingerprint "$SB"); run_hd restore.sh file immich/library/a.jpg --dry-run
  check "restore file --dry-run: prints the plan, changes nothing" eval '[[ $RC == 0 && "$OUT" == *PLAN* && "$OUT" == *"$S1"* && "$(fingerprint "$SB")" == "$before" ]]'
  run_hd restore.sh file immich/library/a.jpg
  check "restore file: REFUSES without confirmation when there is no terminal" eval '[[ $RC != 0 && "$OUT" == *"--yes"* && "$(fingerprint "$SB")" == "$before" ]]'
  run_hd restore.sh file immich/library/a.jpg --yes
  check "restore file: finds the newest snapshot that has the file, restores beside the data" eval '[[ $RC == 0 && "$(cat "$SB/data/restored/$S1/immich/library/a.jpg")" == photo-1 && ! -e "$SB/data/immich/library/a.jpg" ]]'
  run_hd restore.sh file "$SB/data/immich/library/a.jpg" --in-place --yes
  check "restore file --in-place: puts the file back (absolute path accepted)" eval '[[ $RC == 0 && "$(cat "$SB/data/immich/library/a.jpg")" == photo-1 ]]'
  echo "newer-edit" >"$SB/data/immich/library/b.jpg"; run_hd restore.sh file immich/library/b.jpg --in-place --yes
  check "restore file --in-place: NEVER overwrites an existing file" eval '[[ $RC == 0 && "$(cat "$SB/data/immich/library/b.jpg")" == newer-edit ]]'
  run_hd restore.sh file immich/library --from "$S1" --to "$SB/out" --yes
  check "restore file: a whole folder, --from a named snapshot, --to a folder" eval '[[ $RC == 0 && -f "$SB/out/library/a.jpg" && -f "$SB/out/library/b.jpg" ]]'
  run_hd restore.sh file ../etc/passwd --yes
  check "restore file: REFUSES paths with .." eval '[[ $RC != 0 ]]'
  run_hd restore.sh file immich/library/a.jpg --from 2001-01-01_000000 --yes
  check "restore file: REFUSES an unknown snapshot" eval '[[ $RC != 0 && "$OUT" == *"no such snapshot"* ]]'
  run_hd restore.sh file does/not/exist --yes
  check "restore file: REFUSES a path found in no snapshot" eval '[[ $RC != 0 ]]'
  before=$(fingerprint "$SB/backup"); run_hd restore.sh file immich/library --in-place --yes
  check "restore: Disk B is never modified by a restore" eval '[[ "$(fingerprint "$SB/backup")" == "$before" ]]'

  # disk-a
  restore_sandbox; before=$(fingerprint "$SB"); run_hd restore.sh disk-a --yes
  check "restore disk-a: REFUSES a Disk A that already has files" eval '[[ $RC != 0 && "$OUT" == *"already contains files"* && "$(fingerprint "$SB")" == "$before" ]]'
  restore_sandbox; rm -rf "$SB/data"; mkdir "$SB/data"; printf 'role=data\nfresh=1\n' >"$SB/data/.home-driver-disk"
  before=$(fingerprint "$SB"); run_hd restore.sh disk-a --dry-run
  check "restore disk-a --dry-run: prints the plan, changes nothing" eval '[[ $RC == 0 && "$OUT" == *PLAN* && "$(fingerprint "$SB")" == "$before" ]]'
  STUB_SYSTEMCTL_ACTIVE=active run_hd restore.sh disk-a --yes
  check "restore disk-a: REFUSES while services are running" eval '[[ $RC != 0 && "$OUT" == *"systemctl stop"* && "$(fingerprint "$SB")" == "$before" ]]'
  rm "$SB/data/.home-driver-disk"; run_hd restore.sh disk-a --yes
  check "restore disk-a: REFUSES a new disk without marker (not prepared)" eval '[[ $RC != 0 ]]'
  printf 'role=data\nfresh=1\n' >"$SB/data/.home-driver-disk"
  bbefore=$(fingerprint "$SB/backup"); run_hd restore.sh disk-a --yes
  check "restore disk-a: rebuilds Disk A from the newest snapshot" eval '[[ $RC == 0 && "$(cat "$SB/data/immich/library/b.jpg")" == photo-2 && -f "$SB/data/drive/blobs/x" && ! -e "$SB/data/immich/library/a.jpg" ]] && ls "$SB"/data/dumps/immich-*.sql.gz >/dev/null'
  check "restore disk-a: keeps the new disk's own marker, leaves Disk B untouched" eval 'grep -qx fresh=1 "$SB/data/.home-driver-disk" && [[ "$(fingerprint "$SB/backup")" == "$bbefore" ]]'
  restore_sandbox; rm -rf "$SB/data"; mkdir "$SB/data"; printf 'role=data\n' >"$SB/data/.home-driver-disk"; run_hd restore.sh disk-a --from "$S1" --yes
  check "restore disk-a --from: uses the chosen older snapshot" eval '[[ $RC == 0 && -f "$SB/data/immich/library/a.jpg" ]]'

  # disk-b
  restore_sandbox; before=$(fingerprint "$SB"); run_hd restore.sh disk-b --yes
  check "restore disk-b: REFUSES a Disk B that already holds a backup" eval '[[ $RC != 0 && "$OUT" == *"already contains a backup"* && "$(fingerprint "$SB")" == "$before" ]]'
  restore_sandbox; rm -rf "$SB/backup"; mkdir -p "$SB/backup/current" "$SB/backup/snapshots"; printf 'role=backup\n' >"$SB/backup/.home-driver-disk"
  before=$(fingerprint "$SB"); run_hd restore.sh disk-b --dry-run
  check "restore disk-b --dry-run: changes nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]]'
  run_hd restore.sh disk-b --yes
  check "restore disk-b: seeds the new Disk B and takes its first snapshot" eval '[[ $RC == 0 && -f "$SB/backup/current/immich/library/b.jpg" && $(ls "$SB/backup/snapshots" | wc -l) == 1 ]]'

  # offsite
  restore_sandbox; run_hd restore.sh offsite "$SB/out" --yes
  check "restore offsite: REFUSES when restic is not configured" eval '[[ $RC != 0 && "$OUT" == *"RESTIC_REPOSITORY"* ]]'
  printf 'RESTIC_REPOSITORY=x\nRESTIC_PASSWORD=secret\n' >>"$SB/etc/backup.env"
  run_hd restore.sh offsite "$SB/data" --yes
  check "restore offsite: REFUSES a target folder that is not empty" eval '[[ $RC != 0 && "$OUT" == *"not empty"* ]] && ! stub_called "restic restore"'
  before=$(fingerprint "$SB"); run_hd restore.sh offsite "$SB/out" --dry-run
  check "restore offsite --dry-run: changes nothing, downloads nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "restic restore"'
  run_hd restore.sh offsite "$SB/out" --yes
  check "restore offsite: runs restic restore into the empty target" stub_called "restic restore latest --host home-driver --target $SB/out"

  # databases
  restore_sandbox; dump=$(ls "$SB"/state/dumps/immich-*.sql.gz | tail -n 1); mkdir -p "$SB/state/immich-postgres"; echo pgdata >"$SB/state/immich-postgres/PG_VERSION"
  mkdir -p "$SB/opt/immich"; cp "$DEPLOY"/immich/*.yml "$SB/opt/immich/"
  before=$(fingerprint "$SB"); run_hd restore.sh immich-db "$dump" --dry-run
  check "restore immich-db --dry-run: changes nothing, touches no container" eval '[[ $RC == 0 && "$OUT" == *PLAN* && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "^docker" && ! stub_called "^systemctl stop"'
  echo "not a dump" | gzip >"$SB/bad.sql.gz"; before=$(fingerprint "$SB"); run_hd restore.sh immich-db "$SB/bad.sql.gz" --yes
  check "restore immich-db: REFUSES a file that is not a Postgres dump" eval '[[ $RC != 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "^docker"'
  run_hd restore.sh immich-db "$dump" --yes
  check "restore immich-db: stops Immich, keeps the old database aside, loads with psql" eval '[[ $RC == 0 ]] && stub_called "systemctl stop home-driver-immich.service" && stub_called "psql --dbname=immich --username=postgres --single-transaction --set ON_ERROR_STOP=on" && [[ "$(cat "$SB"/state/immich-postgres.before-restore-*/PG_VERSION)" == pgdata ]]'
  check "restore immich-db: leaves Immich stopped for the owner to check" eval '! stub_called "systemctl start"'

  if [[ $HAVE_SQLITE == 1 ]]; then
    restore_sandbox; dump=$(ls "$SB"/state/dumps/drive-*.db | tail -n 1)
    sqlite3 "$SB/state/drive/drive.db" "INSERT INTO files VALUES (2, 'after-backup');"
    before=$(fingerprint "$SB"); run_hd restore.sh drive-db "$dump" --dry-run
    check "restore drive-db --dry-run: changes nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "^systemctl stop"'
    run_hd restore.sh drive-db "$dump" --yes
    check "restore drive-db: replaces the database and keeps the old one aside" eval '[[ $RC == 0 && "$(sqlite3 "$SB/state/drive/drive.db" "SELECT count(*) FROM files")" == 1 && "$(sqlite3 "$SB"/state/drive/drive.db.before-restore-* "SELECT count(*) FROM files")" == 2 ]] && stub_called "systemctl stop home-driver-drive.service"'
    echo garbage >"$SB/garbage.db"; before=$(fingerprint "$SB/state/drive"); run_hd restore.sh drive-db "$SB/garbage.db" --yes
    check "restore drive-db: REFUSES a corrupt dump" eval '[[ $RC != 0 && "$(fingerprint "$SB/state/drive")" == "$before" ]]'
  else
    skip "restore drive-db" "sqlite3 not installed"
  fi
  unset STUB_SYSTEMCTL_ACTIVE
  new_sandbox; run_hd restore.sh
  check "restore: no command prints usage and exits non-zero" eval '[[ $RC != 0 && "$OUT" == *Usage:* ]]'
fi

# ----------------------------------------------------------------------- scrub
if want_test scrub; then
  section "Behaviour: scrub.sh"
  new_sandbox; run_hd scrub.sh
  check "scrub: clean disks -> exit 0, both scrubbed in the foreground, status recorded" eval '[[ $RC == 0 ]] && stub_called "btrfs scrub start -B -d $SB/data" && stub_called "btrfs scrub start -B -d $SB/backup" && grep -q "^result=ok" "$SB/state/status/scrub-data.status" && ! stub_called "^curl"'
  new_sandbox; STUB_SCRUB_SUMMARY="csum=3" STUB_SCRUB_RC=3 run_hd scrub.sh
  check "scrub: checksum errors -> exit 1 and an alert" eval '[[ $RC != 0 ]] && stub_called "sendMessage" && grep -q "^result=errors" "$SB/state/status/scrub-backup.status"'
  new_sandbox; STUB_SCRUB_STATUS="aborted" run_hd scrub.sh
  check "scrub: an unfinished scrub is not reported as clean" eval '[[ $RC != 0 ]]'
  new_sandbox; STUB_DEV_CORRUPTION=2 run_hd scrub.sh
  check "scrub: non-zero device error counters -> exit 1" eval '[[ $RC != 0 && "$OUT" == *"corruption_errs=2"* ]]'
  new_sandbox; rm "$SB/backup/.home-driver-disk"; run_hd scrub.sh
  check "scrub: a missing disk -> exit 1, alert, the other disk is still scrubbed" eval '[[ $RC != 0 ]] && stub_called "scrub start -B -d $SB/data" && ! stub_called "scrub start -B -d $SB/backup" && stub_called "sendMessage"'
  new_sandbox; before=$(fingerprint "$SB"); run_hd scrub.sh --dry-run
  check "scrub: --dry-run starts nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "scrub start"'
fi

# ----------------------------------------------------------------------- smart
if want_test smart; then
  section "Behaviour: smart-check.sh"
  sm() { EXTRA_PATH="$STUBS_DISK" STUB_USB_LABEL=hd-data STUB_OTHER_LABEL=hd-backup run_hd smart-check.sh "$@"; }
  new_sandbox; sm
  check "smart: finds both disks by label; healthy but unmounted -> alert" eval '[[ $RC != 0 && "$OUT" == *"NOT MOUNTED"* ]] && stub_called "smartctl -i -H -A /dev/fakeusb" && stub_called "smartctl -i -H -A /dev/fakeother" && grep -q "^health=PASSED" "$SB/state/smart/data.state"'
  # From here on pretend the disks are mounted, to test the SMART logic itself.
  fake_mounted() { printf '#!/bin/sh\nexit 0\n' >"$SB/tmp/mountpoint"; chmod +x "$SB/tmp/mountpoint"; }
  smm() { EXTRA_PATH="$SB/tmp:$STUBS_DISK" STUB_USB_LABEL=hd-data STUB_OTHER_LABEL=hd-backup run_hd smart-check.sh "$@"; }
  new_sandbox; fake_mounted; smm
  check "smart: healthy disks -> exit 0, state recorded, no alert" eval '[[ $RC == 0 ]] && grep -q "^reallocated=0" "$SB/state/smart/backup.state" && ! stub_called "^curl"'
  STUB_SMART_REALLOC=8 smm
  check "smart: growing reallocated sectors -> exit 1 and an alert" eval '[[ $RC != 0 && "$OUT" == *"grew from 0 to 8"* ]] && stub_called "sendMessage"'
  : >"$SB/stub.log"; STUB_SMART_REALLOC=8 smm
  check "smart: an unchanged count does not alert again" eval '[[ $RC == 0 ]] && ! stub_called "^curl"'
  new_sandbox; fake_mounted; STUB_SMART_HEALTH="FAILED!" smm
  check "smart: failing health -> exit 1 and an alert" eval '[[ $RC != 0 && "$OUT" == *FAILING* ]] && stub_called "sendMessage"'
  new_sandbox; fake_mounted; STUB_SMART_NEEDS_SAT=1 smm
  check "smart: USB bridge needing -d sat is handled" eval '[[ $RC == 0 ]] && stub_called "smartctl -i -H -A -d sat /dev/fakeusb" && grep -q "^health=PASSED" "$SB/state/smart/data.state"'
  new_sandbox; fake_mounted; STUB_SMART_UNREADABLE=1 smm
  check "smart: enclosure without SMART -> one notice, exit 0" eval '[[ $RC == 0 ]] && stub_called "sendMessage" && grep -q "^health=UNREADABLE" "$SB/state/smart/data.state"'
  : >"$SB/stub.log"; STUB_SMART_UNREADABLE=1 smm
  check "smart: ...and the notice is not repeated daily" eval '[[ $RC == 0 ]] && ! stub_called "^curl"'
  new_sandbox; EXTRA_PATH="$STUBS_DISK" STUB_USB_LABEL=hd-data run_hd smart-check.sh
  check "smart: a missing disk -> exit 1 and an alert" eval '[[ $RC != 0 && "$OUT" == *"backup disk"*MISSING* ]] && stub_called "sendMessage"'
  new_sandbox; before=$(fingerprint "$SB"); sm --dry-run
  check "smart: --dry-run changes nothing" eval '[[ "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "^smartctl"'
fi

# --------------------------------------------------------------------- install
if want_test install; then
  section "Behaviour: install.sh"
  inst() { # staged install into $SB/root
    OUT=$(env -u HD_DATA_DIR -u HD_BACKUP_DIR -u HD_STATE_DIR -u HD_ETC_DIR -u HD_OPT_DIR \
      HD_ROOT="$SB/root" HD_TEST_MODE=1 HD_TEST_ALLOW_NON_MOUNTPOINT=1 STUB_LOG="$SB/stub.log" \
      PATH="$STUBS:$ORIG_PATH" "$SCRIPTS/install.sh" "$@" 2>&1 </dev/null); RC=$?
  }
  new_sandbox; mkdir "$SB/root"; before=$(fingerprint "$SB"); inst --dry-run
  check "install --dry-run: exits 0 and writes nothing" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" ]]'
  check "install --dry-run: runs no apt-get, useradd or systemctl" eval '! grep -qE "^(apt-get|useradd|systemctl)" "$SB/stub.log"'
  check "install --dry-run: prints the actions it would take" eval '[[ "$OUT" == *"would install"*"/etc/systemd/system/home-driver-backup-local.timer"* && "$OUT" == *"would create"*"common.env"* ]]'
  new_sandbox; mkdir "$SB/root"; before=$(fingerprint "$SB")
  OUT=$(PATH="$STUBS:$ORIG_PATH" STUB_LOG="$SB/stub.log" "$SCRIPTS/install.sh" 2>&1 </dev/null); RC=$?
  if [[ $EUID -ne 0 ]]; then
    check "install: REFUSES to run for real without root" eval '[[ $RC != 0 && "$OUT" == *"root"* && "$(fingerprint "$SB")" == "$before" ]] && ! grep -qE "^(apt-get|useradd|systemctl)" "$SB/stub.log"'
  else
    skip "install: refuses without root" "tests are running as root"
  fi

  new_sandbox; mkdir "$SB/root"; inst
  R="$SB/root"
  check "install (staged): exits 0" eval '[[ $RC == 0 ]]'
  check "install: scripts installed executable, lib.sh not" eval '[[ -x "$R/opt/home-driver/scripts/backup-local.sh" && -x "$R/opt/home-driver/scripts/doctor.sh" && ! -x "$R/opt/home-driver/scripts/lib.sh" ]]'
  check "install: all units and timers installed" eval '[[ $(ls "$R/etc/systemd/system" | grep -c "^home-driver-") == $(ls "$DEPLOY"/systemd/*.service "$DEPLOY"/systemd/*.timer | wc -l) ]]'
  check "install: compose files installed" eval '[[ -f "$R/opt/home-driver/immich/docker-compose.yml" && -f "$R/opt/home-driver/immich/docker-compose.lan.yml" ]]'
  check "install: all five env files created with mode 600" eval '[[ "$(stat -c %a "$R"/etc/home-driver/{common,immich,drive,curator,backup}.env | sort -u)" == 600 ]]'
  check "install: placeholder secrets replaced by random values" eval '! grep -q CHANGE_ME "$R"/etc/home-driver/*.env && grep -qE "^DRIVE_SESSION_SECRET=[0-9a-f]{64}$" "$R/etc/home-driver/drive.env" && grep -qE "^DB_PASSWORD=[0-9a-f]{64}$" "$R/etc/home-driver/immich.env"'
  check "install: fstab gets both disks by LABEL with nofail" eval '[[ $(grep -c "^LABEL=hd-.*nofail,x-systemd.device-timeout=30" "$R/etc/fstab") == 2 ]]'
  check "install: does not create folders under an unmounted Disk A mountpoint" eval '[[ -d "$R/srv/home-driver/data" && -z "$(ls -A "$R/srv/home-driver/data")" && "$OUT" == *"Disk A is not mounted yet"* ]]'
  check "install: never formats, never touches cloudflared, enables nothing by default" eval '! grep -qE "^(mkfs|wipefs|systemctl enable|systemctl start)" "$SB/stub.log" && [[ ! -e "$R/etc/cloudflared" ]]'

  # second run: idempotent, never overwrites config
  echo "NOTIFY_METHOD=ntfy # my own edit" >"$R/etc/home-driver/common.env"; secret=$(grep DRIVE_SESSION_SECRET "$R/etc/home-driver/drive.env")
  printf 'role=data\n' >"$R/srv/home-driver/data/.home-driver-disk"
  inst
  check "install (second run): exits 0" eval '[[ $RC == 0 ]]'
  check "install: NEVER overwrites an existing env file" eval '[[ "$(cat "$R/etc/home-driver/common.env")" == "NOTIFY_METHOD=ntfy # my own edit" && "$(grep DRIVE_SESSION_SECRET "$R/etc/home-driver/drive.env")" == "$secret" ]]'
  check "install: fstab entries are not duplicated" eval '[[ $(grep -c "^LABEL=hd-" "$R/etc/fstab") == 2 ]]'
  check "install: creates the Disk A folders once the disk (marker) is there" eval '[[ -d "$R/srv/home-driver/data/immich" && -d "$R/srv/home-driver/data/drive/blobs" && -d "$R/srv/home-driver/data/drive/tmp" ]]'
  before=$(fingerprint "$R"); inst
  check "install (third run): changes nothing more" eval '[[ $RC == 0 && "$(fingerprint "$R")" == "$before" ]]'
  inst --enable
  check "install --enable: REFUSES while Disk B is not mounted" eval '[[ $RC != 0 && "$OUT" == *"both disks must be mounted"* ]]'
fi

# ---------------------------------------------------------------- laptop-setup
if want_test laptop; then
  section "Behaviour: laptop-setup.sh"
  lap() {
    OUT=$(HD_ROOT="$SB/root" HD_TEST_MODE=1 HD_TEST_SYS_POWER_SUPPLY="$SB/sys" STUB_LOG="$SB/stub.log" \
      PATH="$STUBS:$ORIG_PATH" "$SCRIPTS/laptop-setup.sh" "$@" 2>&1 </dev/null); RC=$?
  }
  new_sandbox; mkdir -p "$SB/root" "$SB/sys/BAT0"; echo 100 >"$SB/sys/BAT0/charge_control_end_threshold"
  before=$(fingerprint "$SB"); lap --dry-run
  check "laptop-setup --dry-run: changes nothing, says what it would do" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" && "$OUT" == *"HandleLidSwitch=ignore"* && "$OUT" == *"100 -> 80"* ]]'
  lap
  check "laptop-setup: writes the lid drop-in" eval '[[ $RC == 0 ]] && grep -qx "HandleLidSwitch=ignore" "$SB/root/etc/systemd/logind.conf.d/50-home-driver.conf"'
  check "laptop-setup: sets the battery limit to 80 and makes it persistent" eval '[[ "$(cat "$SB/sys/BAT0/charge_control_end_threshold")" == 80 ]] && grep -q "^w /sys/class/power_supply/BAT0/charge_control_end_threshold - - - - 80$" "$SB/root/etc/tmpfiles.d/home-driver-battery.conf"'
  before=$(fingerprint "$SB"); lap
  check "laptop-setup: second run changes nothing and reports that" eval '[[ $RC == 0 && "$(fingerprint "$SB")" == "$before" && "$OUT" == *"already in place"* ]]'
  new_sandbox; mkdir -p "$SB/root" "$SB/sys"; lap
  check "laptop-setup: no battery threshold support is reported, not an error" eval '[[ $RC == 0 && "$OUT" == *"not supported"* && ! -e "$SB/root/etc/tmpfiles.d/home-driver-battery.conf" ]]'
  new_sandbox; mkdir -p "$SB/root"; lap --charge-limit 5
  check "laptop-setup: REFUSES a silly charge limit" eval '[[ $RC != 0 ]]'
fi

# ---------------------------------------------------------------------- doctor
if want_test doctor; then
  section "Behaviour: doctor.sh"
  # a healthy sandbox: one backup done, scrub and SMART recorded
  healthy_sandbox() {
    new_sandbox; run_hd backup-local.sh; run_hd scrub.sh
    mkdir -p "$SB/state/smart"
    for r in data backup; do printf 'checked_epoch=%s\nhealth=PASSED\nreallocated=0\npending=0\nuncorrectable=0\n' "$(date +%s)" >"$SB/state/smart/$r.state"; done
    : >"$SB/stub.log"
  }
  healthy_sandbox; before=$(fingerprint "$SB"); run_hd doctor.sh
  check "doctor: healthy system -> exit 0" eval '[[ $RC == 0 && "$OUT" == *"0 failures"* ]]'
  check "doctor: is read-only (changes nothing, sends nothing)" eval '[[ "$(fingerprint "$SB")" == "$before" ]] && ! stub_called "^curl"'
  rm "$SB/data/.home-driver-disk"; run_hd doctor.sh
  check "doctor: missing marker on Disk A -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"FAIL  Disk A"* ]]'
  healthy_sandbox; ALLOW_NON_MOUNTPOINT=0 run_hd doctor.sh
  check "doctor: unmounted disks -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"not a mounted disk"* ]]'
  healthy_sandbox; chmod 644 "$SB/etc/drive.env"; run_hd doctor.sh
  check "doctor: env file not 0600 -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"drive.env has mode 644"* ]]'
  healthy_sandbox; rm "$SB/etc/curator.env"; run_hd doctor.sh
  check "doctor: missing env file -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"curator.env is missing"* ]]'
  healthy_sandbox; STUB_SS_LISTEN="0.0.0.0:2283 127.0.0.1:8090 127.0.0.1:8091" run_hd doctor.sh
  check "doctor: a port on 0.0.0.0 -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"Immich listens on 0.0.0.0"* ]]'
  healthy_sandbox; STUB_SS_LISTEN="127.0.0.1:2283 127.0.0.1:8090 *:8091" run_hd doctor.sh
  check "doctor: a wildcard listener -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"Curator listens on"* ]]'
  healthy_sandbox; STUB_SS_LISTEN="127.0.0.1:2283 127.0.0.1:8091" run_hd doctor.sh
  check "doctor: a service not listening -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"nothing is listening on port 8090"* ]]'
  healthy_sandbox; echo "IMMICH_LAN_ADDR=192.168.1.50" >>"$SB/etc/immich.env"; STUB_SS_LISTEN="127.0.0.1:2283 192.168.1.50:2283 127.0.0.1:8090 127.0.0.1:8091" run_hd doctor.sh
  check "doctor: the configured home-network address for Immich is a warning, not a failure" eval '[[ $RC == 0 && "$OUT" == *"WARN  Immich also listens on 192.168.1.50"* ]]'
  healthy_sandbox; STUB_SYSTEMCTL_ACTIVE=failed run_hd doctor.sh
  check "doctor: stopped services -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"home-driver-drive.service is failed"* ]]'
  healthy_sandbox; sed -i "s/^last_success_epoch=.*/last_success_epoch=$(( $(date +%s) - 4 * 86400 ))/" "$SB/state/status/backup-local.status"; run_hd doctor.sh
  check "doctor: a stale backup -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"last successful local backup was 4 days ago"* ]]'
  healthy_sandbox; sed -i 's/^health=.*/health=FAILED!/' "$SB/state/smart/backup.state"; run_hd doctor.sh
  check "doctor: failing SMART -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"backup disk: SMART health"* ]]'
  healthy_sandbox; sed -i 's/^result=.*/result=errors/' "$SB/state/status/scrub-data.status"; run_hd doctor.sh
  check "doctor: scrub errors -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"last scrub"*"found errors"* ]]'
  healthy_sandbox; sed -i 's/^NOTIFY_METHOD=.*/NOTIFY_METHOD=/' "$SB/etc/common.env"; run_hd doctor.sh
  check "doctor: notifications not configured -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"notifications are not configured"* ]]'
  new_sandbox; run_hd doctor.sh
  check "doctor: fresh install without any backup -> FAIL, exit 1" eval '[[ $RC != 0 && "$OUT" == *"no successful local backup"* ]]'
fi

# -------------------------------------------------------------- immich-compose
if want_test immich-compose; then
  section "Behaviour: immich-compose.sh"
  new_sandbox; mkdir -p "$SB/opt/immich"; cp "$DEPLOY"/immich/*.yml "$SB/opt/immich/"
  run_hd immich-compose.sh ps
  check "immich-compose: default uses only the main compose file" eval '[[ $RC == 0 ]] && stub_called "docker compose --env-file $SB/etc/immich.env -f $SB/opt/immich/docker-compose.yml ps"'
  echo "IMMICH_LAN_ADDR=192.168.1.50" >>"$SB/etc/immich.env"; : >"$SB/stub.log"; run_hd immich-compose.sh ps
  check "immich-compose: a private IMMICH_LAN_ADDR adds the LAN override" stub_called "docker-compose.lan.yml ps"
  for bad in 0.0.0.0 8.8.8.8 172.32.0.1 "192.168.1.50; rm -rf /"; do
    new_sandbox; mkdir -p "$SB/opt/immich"; cp "$DEPLOY"/immich/*.yml "$SB/opt/immich/"; echo "IMMICH_LAN_ADDR=$bad" >>"$SB/etc/immich.env"; run_hd immich-compose.sh up -d
    check "immich-compose: REFUSES IMMICH_LAN_ADDR='$bad'" eval '[[ $RC != 0 ]] && ! stub_called "^docker"'
  done
fi

###############################################################################
echo
echo "=============================================================="
echo "deploy tests: $N_PASS passed, $N_FAIL failed, $N_SKIP skipped"
if (( N_SKIP > 0 )); then printf '  skipped: %s\n' "${SKIPPED[@]}"; fi
if (( N_FAIL > 0 )); then
  printf '  FAILED:  %s\n' "${FAILED[@]}"
  echo "=============================================================="
  exit 1
fi
echo "=============================================================="
exit 0
