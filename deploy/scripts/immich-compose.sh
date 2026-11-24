#!/usr/bin/env bash
# immich-compose.sh - the one place that knows how to call `docker compose` for Immich.
# Used by home-driver-immich.service, restore.sh and the upgrade steps in the runbook.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: immich-compose.sh <docker compose arguments...>

Runs `docker compose` for Immich with the right project files and env file:

  --env-file /etc/home-driver/immich.env
  -f /opt/home-driver/immich/docker-compose.yml
  -f /opt/home-driver/immich/docker-compose.lan.yml   (only if IMMICH_LAN_ADDR is set)

Examples:
  sudo immich-compose.sh ps
  sudo immich-compose.sh pull
  sudo immich-compose.sh logs --tail 50 immich-server

IMMICH_LAN_ADDR (optional, in immich.env) additionally publishes Immich on that
private home-network address so phones can upload big videos over Wi-Fi without
Cloudflare. It must be a private IPv4 address of this laptop; 0.0.0.0 and public
addresses are refused.
EOF
}

case "${1:-}" in
  -h|--help|"") usage; [[ -n "${1:-}" ]] && exit 0 || exit 2 ;;
esac

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

env_file="$HD_ETC_DIR/immich.env"
compose_dir="$HD_OPT_DIR/immich"
[[ -r "$env_file" ]] || hd_die "$env_file not found or not readable (run as root; create it from immich.env.example)"
[[ -f "$compose_dir/docker-compose.yml" ]] || hd_die "$compose_dir/docker-compose.yml not found (run install.sh)"

args=(compose --env-file "$env_file" -f "$compose_dir/docker-compose.yml")

lan_addr=$(hd_env_get "$env_file" IMMICH_LAN_ADDR "")
if [[ -n "$lan_addr" ]]; then
  # Hard rule: nothing listens on a public interface. Only RFC1918 addresses pass.
  if [[ ! "$lan_addr" =~ ^(10\.[0-9]{1,3}|192\.168|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    hd_die "IMMICH_LAN_ADDR='$lan_addr' is not a private home-network IPv4 address (10.x.x.x, 172.16-31.x.x, 192.168.x.x). Refusing to publish Immich on it."
  fi
  args+=(-f "$compose_dir/docker-compose.lan.yml")
fi

# The compose file reads this to locate the container env file.
export IMMICH_ENV_FILE="$env_file"
exec docker "${args[@]}" "$@"
