#!/usr/bin/env bash
# notify.sh - the ONLY way anything in home-driver sends an alert.
#
# Contract: this script NEVER fails its caller. It always exits 0, even when
# it is unconfigured, the network is down, or it is called with bad arguments.
set -uo pipefail

usage() {
  cat <<'EOF'
Usage: notify.sh "<title>" "<body>" [priority]

Send an alert to the owner's phone through Telegram or ntfy.

  priority   low | default | high      (default: default)

Configuration is read from /etc/home-driver/common.env when readable, otherwise
from the environment (systemd units pass it in with EnvironmentFile=):

  NOTIFY_METHOD=telegram   NOTIFY_TELEGRAM_BOT_TOKEN, NOTIFY_TELEGRAM_CHAT_ID
  NOTIFY_METHOD=ntfy       NOTIFY_NTFY_URL (full topic URL), NOTIFY_NTFY_TOKEN (optional)
  NOTIFY_METHOD=           (empty) notifications disabled: logs a warning, exits 0

Always exits 0 so a broken notifier can never break a backup.

Test it:  sudo /opt/home-driver/scripts/notify.sh "test" "hello from the server"
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac

# shellcheck source=lib.sh
if ! source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"; then
  echo "notify.sh: cannot load lib.sh; notification not sent" >&2
  exit 0
fi

say() {
  hd_log "$*"
  if command -v logger >/dev/null 2>&1; then logger -t home-driver-notify -- "$*" 2>/dev/null || true; fi
}

main() {
  if [[ $# -lt 2 ]]; then
    say "WARNING: called with $# argument(s), need <title> <body>; nothing sent"
    return 0
  fi
  local title=$1 body=$2 priority=${3:-default}
  case "$priority" in low|default|high) ;; *) priority=default ;; esac

  hd_load_env "$HD_ETC_DIR/common.env"
  local method=${NOTIFY_METHOD:-}
  local host
  host=$(hostname 2>/dev/null || echo server)
  title="[$host] $title"

  # Always leave a trace in the journal, whatever happens next.
  say "notify($priority): $title - $body"

  if ! command -v curl >/dev/null 2>&1; then
    say "WARNING: curl not installed; notification not sent"
    return 0
  fi

  local -a curl_base=(curl --silent --show-error --fail --max-time 20 --retry 2 --retry-delay 3 --output /dev/null)

  case "$method" in
    telegram)
      if [[ -z "${NOTIFY_TELEGRAM_BOT_TOKEN:-}" || -z "${NOTIFY_TELEGRAM_CHAT_ID:-}" ]]; then
        say "WARNING: NOTIFY_METHOD=telegram but NOTIFY_TELEGRAM_BOT_TOKEN/NOTIFY_TELEGRAM_CHAT_ID are not set; nothing sent"
        return 0
      fi
      local prefix="" silent=false
      case "$priority" in
        high) prefix="[ALERT] " ;;
        low) silent=true ;;
      esac
      if ! "${curl_base[@]}" \
          --data-urlencode "chat_id=${NOTIFY_TELEGRAM_CHAT_ID}" \
          --data-urlencode "text=${prefix}${title}"$'\n\n'"${body}" \
          --data-urlencode "disable_notification=${silent}" \
          "https://api.telegram.org/bot${NOTIFY_TELEGRAM_BOT_TOKEN}/sendMessage" 2>/dev/null; then
        say "WARNING: sending Telegram notification failed (network down or wrong token/chat id)"
      fi
      ;;
    ntfy)
      if [[ -z "${NOTIFY_NTFY_URL:-}" ]]; then
        say "WARNING: NOTIFY_METHOD=ntfy but NOTIFY_NTFY_URL is not set; nothing sent"
        return 0
      fi
      local prio=3 tags="information_source"
      case "$priority" in
        high) prio=5; tags="rotating_light" ;;
        low) prio=2 ;;
      esac
      local -a auth=()
      [[ -n "${NOTIFY_NTFY_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer ${NOTIFY_NTFY_TOKEN}")
      if ! "${curl_base[@]}" "${auth[@]}" \
          -H "Title: ${title}" -H "Priority: ${prio}" -H "Tags: ${tags}" \
          --data-binary "${body}" \
          "${NOTIFY_NTFY_URL}" 2>/dev/null; then
        say "WARNING: sending ntfy notification failed (network down or wrong URL/token)"
      fi
      ;;
    ""|none)
      say "WARNING: notifications are not configured (NOTIFY_METHOD is empty in $HD_ETC_DIR/common.env); nothing sent"
      ;;
    *)
      say "WARNING: unknown NOTIFY_METHOD='$method' (use telegram or ntfy); nothing sent"
      ;;
  esac
  return 0
}

main "$@" || true
exit 0
