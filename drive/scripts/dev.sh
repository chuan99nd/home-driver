#!/usr/bin/env bash
# Start a throw-away local Drive with DRIVE_DEV_LOGIN=1 and sample data.
# Usage: scripts/dev.sh [port]             (default 8090)
# Everything lives in a temp directory that is removed when you press Ctrl-C.
# Never use DRIVE_DEV_LOGIN outside local development.
set -euo pipefail

PORT="${1:-8090}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE/.."
make build

WORK="$(mktemp -d)"
mkdir "$WORK/data"
PID=""
cleanup() {
  [ -z "$PID" ] || kill "$PID" 2>/dev/null || true
  [ -z "$PID" ] || wait "$PID" 2>/dev/null || true
  rm -rf -- "$WORK"
}
trap cleanup EXIT
trap 'exit 0' INT TERM

DRIVE_DEV_LOGIN=1 \
DRIVE_ADDR="127.0.0.1:$PORT" \
DRIVE_BASE_URL="http://127.0.0.1:$PORT" \
DRIVE_ADMIN_EMAILS="admin@example.com" \
DRIVE_SESSION_SECRET="dev-only-secret-dev-only-secret-0123" \
DRIVE_DB_PATH="$WORK/drive.db" \
DRIVE_DATA_DIR="$WORK/data" \
CURATOR_URL="${CURATOR_URL-}" \
  ./bin/drive &
PID=$!

for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null; then break; fi
  sleep 0.2
done
"$HERE/seed.sh" "http://127.0.0.1:$PORT"
echo
echo "Drive is running. Press Ctrl-C to stop and delete the temporary data."
wait "$PID"
