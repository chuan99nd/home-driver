#!/usr/bin/env bash
# install.sh - idempotent installer. Run with sudo from a checkout of the repo.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: sudo deploy/scripts/install.sh [--dry-run] [--enable]

Installs or updates home-driver on this machine from the repo checkout that
contains this script. Safe to run again at any time (e.g. after `git pull`).

What it does:
  - installs missing packages with apt: btrfs-progs smartmontools restic sqlite3 rsync curl
  - creates the system user "home-driver"
  - creates /etc/home-driver, /opt/home-driver, /var/lib/home-driver and the
    two mountpoints under /srv/home-driver
  - copies the scripts, the Immich compose files and the docs to /opt/home-driver
  - copies the Drive binary (drive/bin/drive; build it first with `make build`)
  - installs the curator into /opt/home-driver/curator/.venv
  - creates /etc/home-driver/*.env from the examples ONLY IF MISSING.
    An existing env file is never modified or overwritten.
  - adds the two disks to /etc/fstab by label (only if not already there)
  - installs the systemd units and runs `systemctl daemon-reload`
  - prints the steps that are left for you

What it never does:
  - format or wipe a disk (that is setup-disks.sh, by hand)
  - touch cloudflared or its configuration
  - start or enable anything, unless you pass --enable

Options:
  --enable     also enable and start the services and timers. Use it after the
               disks are mounted and the env files are filled in.
  --dry-run    print every action; change nothing
  -h, --help   this text
EOF
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ENABLE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --enable) ENABLE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done

REPO="$(cd "$HD_SCRIPTS_DIR/../.." && pwd)"
DEPLOY="$REPO/deploy"
[[ -d "$DEPLOY/systemd" && -d "$DEPLOY/config" ]] \
  || hd_die "run this from a checkout of the repository: sudo <repo>/deploy/scripts/install.sh (not the copy in /opt)"

# Staged install: with HD_ROOT set everything is written below that directory
# and nothing on the live system (packages, users, systemd) is touched.
STAGED=0
[[ -n "$HD_ROOT" ]] && STAGED=1
[[ $STAGED == 1 ]] || hd_require_root

SYSTEMD_DIR="$HD_ROOT/etc/systemd/system"
FSTAB="$HD_ROOT/etc/fstab"
TODO=()       # manual steps printed at the end
WARNINGS=()

step() { printf '\n== %s\n' "$*" >&2; }
# live <cmd...>: a command that changes the live system; skipped in a staged install.
live() {
  if [[ $STAGED == 1 ]]; then
    hd_log "staged install (HD_ROOT set): skipping: $*"
    return 0
  fi
  hd_run "$@"
}
can_chown() { [[ $STAGED == 0 && "$EUID" -eq 0 ]]; }

# make_dir <path> <mode> <owner:group>
make_dir() {
  local path=$1 mode=$2 owner=$3
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would ensure directory $path (mode $mode, owner $owner)"
    return 0
  fi
  mkdir -p "$path"
  chmod "$mode" "$path"
  if can_chown; then chown "$owner" "$path"; fi
}

# copy_file <src> <dst> <mode>: copy only when different.
copy_file() {
  local src=$1 dst=$2 mode=$3
  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    [[ "$HD_DRY_RUN" == 1 ]] || chmod "$mode" "$dst"
    return 0
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would install $dst (mode $mode)"
    return 0
  fi
  install -m "$mode" "$src" "$dst.new"
  mv -f "$dst.new" "$dst"
  hd_log "installed $dst"
}

random_secret() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# ------------------------------------------------------------------ packages
step "Packages"
declare -A PKG_FOR=([btrfs]=btrfs-progs [smartctl]=smartmontools [restic]=restic [sqlite3]=sqlite3 [rsync]=rsync [curl]=curl)
missing=()
for c in btrfs smartctl restic sqlite3 rsync curl; do
  command -v "$c" >/dev/null 2>&1 || missing+=("${PKG_FOR[$c]}")
done
if (( ${#missing[@]} == 0 )); then
  hd_log "all required packages are present"
elif [[ $STAGED == 1 ]]; then
  hd_log "staged install (HD_ROOT set): skipping package install of: ${missing[*]}"
elif command -v apt-get >/dev/null 2>&1; then
  live env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  live env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${missing[@]}"
else
  hd_die "missing packages (${missing[*]}) and apt-get is not available; install them by hand and run again"
fi
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  WARNINGS+=("Docker with the compose plugin is not installed. Immich needs it: https://docs.docker.com/engine/install/ubuntu/")
fi

# ---------------------------------------------------------------------- user
step "System user"
if id -u "$HD_SERVICE_USER" >/dev/null 2>&1; then
  hd_log "user $HD_SERVICE_USER exists"
else
  live useradd --system --user-group --home-dir /var/lib/home-driver --no-create-home \
    --shell /usr/sbin/nologin "$HD_SERVICE_USER"
fi
SVC="$HD_SERVICE_USER:$HD_SERVICE_USER"

# --------------------------------------------------------------- directories
step "Directories"
make_dir "$HD_ETC_DIR" 0755 root:root
make_dir "$HD_OPT_DIR" 0755 root:root
for d in bin scripts immich curator docs; do make_dir "$HD_OPT_DIR/$d" 0755 root:root; done
make_dir "$HD_STATE_DIR" 0755 root:root
make_dir "$HD_STATE_DIR/drive" 0750 "$SVC"
make_dir "$HD_STATE_DIR/curator" 0750 "$SVC"
# Owned by the service user so the SQLite backups can be written without root;
# the Immich dump inside is root-only (0600).
make_dir "$HD_STATE_DIR/dumps" 0750 "$SVC"
for d in smart status locks; do make_dir "$HD_STATE_DIR/$d" 0755 root:root; done
# The Postgres container manages ownership of its own directory: create it if
# missing, never chmod/chown an existing one.
if [[ ! -d "$HD_STATE_DIR/immich-postgres" ]]; then
  make_dir "$HD_STATE_DIR/immich-postgres" 0700 root:root
fi
# Mountpoints. Only created when missing: never chmod a mounted disk's root.
for d in "$HD_DATA_DIR" "$HD_BACKUP_DIR"; do
  [[ -d "$d" ]] || make_dir "$d" 0755 root:root
done

# Directories ON Disk A: only when it is really our disk (marker present).
# Otherwise we would create them on the SSD underneath the mountpoint.
data_reason=""
if data_reason=$(hd_check_disk data "$HD_DATA_DIR"); then
  make_dir "$HD_DATA_DIR/immich" 0755 root:root
  make_dir "$HD_DATA_DIR/drive" 0750 "$SVC"
  make_dir "$HD_DATA_DIR/drive/blobs" 0750 "$SVC"
  make_dir "$HD_DATA_DIR/drive/tmp" 0750 "$SVC"
  make_dir "$HD_DATA_DIR/dumps" 0700 root:root
else
  hd_log "Disk A not ready ($data_reason): skipping the directories on it"
  TODO+=("Disk A is not mounted yet. Prepare it (setup-disks.sh), mount it (sudo mount $HD_DATA_DIR), then run install.sh again to create the folders on it.")
fi
backup_reason=""
if ! backup_reason=$(hd_check_disk backup "$HD_BACKUP_DIR"); then
  TODO+=("Disk B is not mounted yet ($backup_reason). Prepare it (setup-disks.sh) and mount it: sudo mount $HD_BACKUP_DIR")
fi

# --------------------------------------------------------------------- files
step "Scripts, compose files, docs"
for f in "$DEPLOY"/scripts/*.sh; do
  mode=0755
  [[ "$(basename "$f")" == lib.sh ]] && mode=0644
  copy_file "$f" "$HD_OPT_DIR/scripts/$(basename "$f")" "$mode"
done
for f in "$DEPLOY"/immich/*.yml; do copy_file "$f" "$HD_OPT_DIR/immich/$(basename "$f")" 0644; done
for f in "$DEPLOY"/docs/*.md; do
  [[ -f "$f" ]] && copy_file "$f" "$HD_OPT_DIR/docs/$(basename "$f")" 0644
done

step "Drive binary"
if [[ -x "$REPO/drive/bin/drive" ]]; then
  copy_file "$REPO/drive/bin/drive" "$HD_OPT_DIR/bin/drive" 0755
else
  WARNINGS+=("drive/bin/drive not found: Drive was NOT installed. Build it (make build) and run install.sh again.")
fi

step "Curator"
VENV="$HD_OPT_DIR/curator/.venv"
find_uv() {
  local candidate
  if command -v uv >/dev/null 2>&1; then command -v uv; return 0; fi
  # Under sudo, root's PATH usually lacks the invoking user's ~/.local/bin.
  if [[ -n "${SUDO_USER:-}" ]]; then
    candidate="$(getent passwd "$SUDO_USER" | cut -d: -f6)/.local/bin/uv"
    if [[ -x "$candidate" ]]; then echo "$candidate"; return 0; fi
  fi
  return 1
}
if [[ -f "$REPO/curator/pyproject.toml" ]]; then
  # The venv's python must be readable by the home-driver service user. uv
  # prefers a system python (/usr/bin/python3 under sudo); if that is too old
  # for the curator it downloads one, and UV_PYTHON_INSTALL_DIR makes sure the
  # download lands in /opt/home-driver instead of somebody's home directory.
  UV_ENV=(UV_PYTHON_PREFERENCE=system "UV_PYTHON_INSTALL_DIR=$HD_OPT_DIR/python")
  if UV=$(find_uv); then
    if [[ -f "$REPO/curator/uv.lock" ]]; then
      hd_run env "${UV_ENV[@]}" UV_PROJECT_ENVIRONMENT="$VENV" \
        "$UV" sync --project "$REPO/curator" --frozen --no-dev --no-editable --quiet
    else
      hd_run env "${UV_ENV[@]}" "$UV" venv --quiet --allow-existing "$VENV"
      hd_run env "${UV_ENV[@]}" "$UV" pip install --quiet --python "$VENV/bin/python" --upgrade "$REPO/curator"
    fi
  elif command -v python3 >/dev/null 2>&1; then
    [[ -x "$VENV/bin/python" ]] || hd_run python3 -m venv "$VENV"
    hd_run "$VENV/bin/python" -m pip install --quiet --upgrade "$REPO/curator"
  else
    WARNINGS+=("neither uv nor python3 found: the curator was NOT installed.")
  fi
  if [[ "$HD_DRY_RUN" != 1 && -e "$VENV/bin/python" ]]; then
    case "$(readlink -f "$VENV/bin/python")" in
      /home/*|/root/*) WARNINGS+=("the curator's python is $(readlink -f "$VENV/bin/python"), inside a home directory: the home-driver user may not be able to run it. Install python3 (>= the version in curator/pyproject.toml) system-wide and run install.sh again.") ;;
    esac
  fi
  if [[ "$HD_DRY_RUN" != 1 && ! -x "$VENV/bin/home-driver-curator" ]]; then
    WARNINGS+=("the curator install did not produce $VENV/bin/home-driver-curator; the curator will not start.")
  fi
else
  WARNINGS+=("curator/pyproject.toml not found: the curator was NOT installed.")
fi

# ----------------------------------------------------------------- env files
step "Configuration in $HD_ETC_DIR (existing files are never touched)"
for ex in "$DEPLOY"/config/*.env.example; do
  name=$(basename "$ex" .example)
  dst="$HD_ETC_DIR/$name"
  if [[ -e "$dst" ]]; then
    hd_log "keeping existing $dst"
    continue
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would create $dst from $(basename "$ex") (mode 0600), generating random secrets"
    continue
  fi
  # Write under a temporary name and rename: never leaves a half-written config.
  tmp="$dst.tmp.$$"
  ( umask 077
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == *=CHANGE_ME_GENERATED_BY_INSTALL ]]; then
        printf '%s=%s\n' "${line%%=*}" "$(random_secret)"
      else
        printf '%s\n' "$line"
      fi
    done <"$ex" >"$tmp" )
  chmod 0600 "$tmp"
  if can_chown; then chown root:root "$tmp"; fi
  mv -n "$tmp" "$dst"
  rm -f "$tmp"
  hd_log "created $dst"
  TODO+=("Edit $dst (sudo nano $dst)")
done

# --------------------------------------------------------------------- fstab
step "Disks in $FSTAB"
add_fstab_line() { # <mountpoint> <line>
  local mp=$1 line=$2
  if [[ -f "$FSTAB" ]] && awk -v mp="$mp" '$1 !~ /^#/ && $2 == mp {found=1} END {exit !found}' "$FSTAB"; then
    hd_log "$FSTAB already has an entry for $mp (left as it is)"
    return 0
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would append to $FSTAB: $line"
    return 0
  fi
  if [[ -f "$FSTAB" && ! -f "$FSTAB.before-home-driver" ]]; then cp -p "$FSTAB" "$FSTAB.before-home-driver"; fi
  mkdir -p "$(dirname "$FSTAB")"
  printf '%s\n' "$line" >>"$FSTAB"
  hd_log "added to $FSTAB: $line"
}
# The fstab lines are taken from deploy/systemd/fstab.snippet (single source).
while IFS= read -r line; do
  [[ "$line" =~ ^LABEL= ]] || continue
  read -r _ mp _ <<<"$line"
  add_fstab_line "$mp" "$line"
done <"$DEPLOY/systemd/fstab.snippet"

# --------------------------------------------------------------------- units
step "systemd units"
make_dir "$SYSTEMD_DIR" 0755 root:root
for f in "$DEPLOY"/systemd/*.service "$DEPLOY"/systemd/*.timer; do
  copy_file "$f" "$SYSTEMD_DIR/$(basename "$f")" 0644
done
live systemctl daemon-reload

# -------------------------------------------------------------------- enable
TIMERS=(home-driver-backup-local.timer home-driver-backup-offsite.timer home-driver-scrub.timer home-driver-smart-check.timer)
if [[ $ENABLE == 1 ]]; then
  step "Enabling services and timers"
  if [[ -n "$data_reason" || -n "$backup_reason" ]]; then
    hd_die "--enable refused: both disks must be mounted first. Disk A: ${data_reason:-ok}. Disk B: ${backup_reason:-ok}."
  fi
  live systemctl enable --now "${TIMERS[@]}"
  live systemctl enable --now home-driver-immich.service
  if [[ -x "$HD_OPT_DIR/bin/drive" ]]; then
    live systemctl enable --now home-driver-drive.service
  else
    WARNINGS+=("Drive not enabled: binary missing.")
  fi
  if [[ -x "$VENV/bin/home-driver-curator" ]]; then
    live systemctl enable --now home-driver-curator.service home-driver-curator-scan.timer
  else
    WARNINGS+=("Curator not enabled: not installed.")
  fi
else
  TODO+=("When the disks are mounted and the env files are filled in: sudo $DEPLOY/scripts/install.sh --enable")
fi

# -------------------------------------------------------------------- report
echo
if [[ "$HD_DRY_RUN" == 1 ]]; then
  echo "Dry run finished: nothing was changed."
else
  echo "Install finished."
fi
if (( ${#WARNINGS[@]} > 0 )); then
  echo
  echo "WARNINGS:"
  printf '  ! %s\n' "${WARNINGS[@]}"
fi
echo
echo "What is left for you to do (details: $DEPLOY/docs/INSTALL.md):"
if (( ${#TODO[@]} > 0 )); then printf '  - %s\n' "${TODO[@]}"; fi
cat <<EOF
  - Set up notifications in $HD_ETC_DIR/common.env, then test:
      sudo $HD_OPT_DIR/scripts/notify.sh "test" "hello from the server"
  - Add the two hostnames to your Cloudflare Tunnel (example: $DEPLOY/cloudflared/ingress.example.yml).
    install.sh does not touch cloudflared.
  - Check everything:  sudo $HD_OPT_DIR/scripts/doctor.sh
EOF
