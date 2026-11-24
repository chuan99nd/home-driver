#!/usr/bin/env bash
# laptop-setup.sh - make a laptop behave like an always-on server.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: laptop-setup.sh [--dry-run] [--charge-limit N]

Makes the laptop behave like a server. Safe to run more than once.

  1. Closing the lid no longer suspends the machine
       writes /etc/systemd/logind.conf.d/50-home-driver.conf
  2. Sleep, suspend and hibernate are disabled
       systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
  3. Battery charging stops at 80 % (a laptop that is plugged in 24/7 at 100 %
     wears its battery out and can swell), if the laptop supports it
       writes /etc/tmpfiles.d/home-driver-battery.conf so it survives a reboot

Options:
  --dry-run          print what would change; change nothing
  --charge-limit N   battery limit in percent, 50-100 (default 80)
  -h, --help         this text

To undo everything:
  sudo rm /etc/systemd/logind.conf.d/50-home-driver.conf /etc/tmpfiles.d/home-driver-battery.conf
  sudo systemctl unmask sleep.target suspend.target hibernate.target hybrid-sleep.target
  then reboot.
USAGE
}

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

LIMIT=80
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) HD_DRY_RUN=1 ;;
    --charge-limit) LIMIT=${2:-}; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; hd_die "unknown argument: $1" ;;
  esac
  shift
done
if ! hd_is_uint "$LIMIT" || (( LIMIT < 50 || LIMIT > 100 )); then
  hd_die "--charge-limit must be a number from 50 to 100"
fi
hd_require_root

CHANGED=()
UNCHANGED=()
# With HD_ROOT set (tests, staged installs) files are written below it and no
# live system command is run.
STAGED=0
[[ -n "$HD_ROOT" ]] && STAGED=1
SYS_POWER="${HD_TEST_SYS_POWER_SUPPLY:-/sys/class/power_supply}"

# write_file <path> <content>: write only when the content differs.
write_file() {
  local path=$1 content=$2
  if [[ -f "$path" ]] && [[ "$(cat "$path")" == "$content" ]]; then
    UNCHANGED+=("$path already in place")
    return 0
  fi
  if [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would write $path:"
    printf '%s\n' "$content" | sed 's/^/    /' >&2
  else
    mkdir -p "$(dirname "$path")"
    printf '%s\n' "$content" >"$path"
  fi
  CHANGED+=("wrote $path")
}

# ---- 1. lid ----
logind_conf="$HD_ROOT/etc/systemd/logind.conf.d/50-home-driver.conf"
before=${#CHANGED[@]}
write_file "$logind_conf" "# Written by home-driver laptop-setup.sh: closing the lid must not suspend a server.
[Login]
HandleLidSwitch=ignore
HandleLidSwitchExternalPower=ignore
HandleLidSwitchDocked=ignore"
if (( ${#CHANGED[@]} > before )) && [[ $STAGED == 0 ]]; then
  # SIGHUP makes logind re-read its configuration without ending the desktop session.
  hd_run systemctl kill --signal=HUP systemd-logind.service || hd_warn "could not signal systemd-logind; the lid setting applies after a reboot"
fi

# ---- 2. sleep targets ----
for t in sleep.target suspend.target hibernate.target hybrid-sleep.target; do
  if [[ $STAGED == 1 ]]; then
    hd_log "staged install (HD_ROOT set): skipping systemctl mask $t"
    continue
  fi
  state=$(systemctl is-enabled "$t" 2>/dev/null || true)
  if [[ "$state" == masked ]]; then
    UNCHANGED+=("$t already masked")
  else
    hd_run systemctl mask "$t" >/dev/null
    CHANGED+=("masked $t (the laptop can no longer suspend or hibernate)")
  fi
done

# ---- 3. battery charge limit ----
found_battery=0
tmpfiles_lines="# Written by home-driver laptop-setup.sh: stop charging at ${LIMIT} %."
for f in "$SYS_POWER"/BAT*/charge_control_end_threshold; do
  [[ -e "$f" ]] || continue
  found_battery=1
  real_path="/sys/class/power_supply/${f#"$SYS_POWER"/}"
  tmpfiles_lines+=$'\n'"w $real_path - - - - $LIMIT"
  current=$(cat "$f" 2>/dev/null || echo unknown)
  if [[ "$current" == "$LIMIT" ]]; then
    UNCHANGED+=("battery limit already $LIMIT % ($f)")
  elif [[ "$HD_DRY_RUN" == 1 ]]; then
    hd_log "[dry-run] would write $LIMIT to $f (now: $current)"
    CHANGED+=("battery charge limit $current -> $LIMIT % ($f)")
  elif echo "$LIMIT" >"$f" 2>/dev/null; then
    CHANGED+=("battery charge limit $current -> $LIMIT % ($f)")
  else
    hd_warn "could not write $f (this laptop may only accept certain values)"
  fi
done
if [[ $found_battery == 1 ]]; then
  write_file "$HD_ROOT/etc/tmpfiles.d/home-driver-battery.conf" "$tmpfiles_lines"
else
  UNCHANGED+=("no battery with charge_control_end_threshold found: charge limit not supported here (check the BIOS/UEFI for a battery care setting)")
fi

# ---- report ----
echo
if [[ "$HD_DRY_RUN" == 1 ]]; then echo "Would change (dry run, nothing was changed):"; else echo "Changed:"; fi
if (( ${#CHANGED[@]} > 0 )); then printf '  - %s\n' "${CHANGED[@]}"; else echo "  (nothing)"; fi
echo "Already OK / not applicable:"
if (( ${#UNCHANGED[@]} > 0 )); then printf '  - %s\n' "${UNCHANGED[@]}"; else echo "  (nothing)"; fi
echo
echo "Also do by hand: in the BIOS/UEFI setup, enable 'power on after AC power loss' if the laptop has it."
echo "Check the lid: close it, wait a minute, and confirm the server still answers (ping it from another device)."
