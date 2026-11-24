#!/usr/bin/env bash
# Fill a local DRIVE_DEV_LOGIN instance with sample data for UI work.
# Usage: scripts/seed.sh [base-url]        (default http://127.0.0.1:8090)
# Only works against a server started with DRIVE_DEV_LOGIN=1 (see scripts/dev.sh).
set -euo pipefail

BASE="${1:-http://127.0.0.1:8090}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WEB="$HERE/../internal/server/web"
WORK="$(mktemp -d)"
trap 'rm -rf -- "$WORK"' EXIT
JAR="$WORK/admin.jar"
CSRF='X-Drive-Csrf: 1'
JSON='Content-Type: application/json'

field() { # field <name>: first string value of "name" in JSON on stdin
  sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n 1
}

mkdir_node() { # mkdir_node <parent> <name> -> id
  curl -fsS -b "$JAR" -H "$CSRF" -H "$JSON" -d "{\"name\":\"$2\"}" "$BASE/api/nodes/$1/children" | field id
}

upload() { # upload <parent> <name> <file> -> id   (files under one chunk)
  local size id
  size="$(wc -c <"$3" | tr -d ' ')"
  id="$(curl -fsS -b "$JAR" -H "$CSRF" -H "$JSON" \
    -d "{\"parent_id\":\"$1\",\"name\":\"$2\",\"size\":$size,\"on_conflict\":\"rename\"}" "$BASE/api/uploads" | field id)"
  if [ "$size" -gt 0 ]; then
    curl -fsS -o /dev/null -b "$JAR" -H "$CSRF" -X PUT --data-binary "@$3" "$BASE/api/uploads/$id/chunk?offset=0"
  fi
  curl -fsS -b "$JAR" -H "$CSRF" -X POST "$BASE/api/uploads/$id/complete" | field id
}

curl -fsS -o /dev/null -c "$JAR" "$BASE/dev/login?email=admin@example.com"
ROOT="$(curl -fsS -b "$JAR" "$BASE/api/session" | field root_id)"
[ -n "$ROOT" ] || { echo "seed: could not sign in as admin@example.com at $BASE (is DRIVE_DEV_LOGIN=1 and is it in DRIVE_ADMIN_EMAILS?)" >&2; exit 1; }

printf 'Danh sách đi chợ\n\n- Rau muống\n- Cá\n- Trứng\n' >"$WORK/note.txt"
printf '# Kế hoạch Tết\n\n1. Dọn nhà\n2. Gói bánh chưng\n3. Về quê\n' >"$WORK/plan.md"
printf 'ten,tuoi\nBa,62\nMe,58\n' >"$WORK/family.csv"
printf '<h1>Not rendered</h1><script>alert(1)</script>\n' >"$WORK/page.html"
head -c 300000 /dev/urandom >"$WORK/backup.zip"
printf '%%PDF-1.1\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 300 200]>>endobj\ntrailer<</Root 1 0 R>>\n' >"$WORK/doc.pdf"

PHOTOS="$(mkdir_node "$ROOT" 'Ảnh gia đình')"
DOCS="$(mkdir_node "$ROOT" 'Giấy tờ')"
TET="$(mkdir_node "$PHOTOS" 'Tết 2026')"
mkdir_node "$ROOT" 'Thư mục trống' >/dev/null

upload "$PHOTOS" 'Cả nhà.png' "$WEB/icon-512.png" >/dev/null
upload "$PHOTOS" 'Ông bà.png' "$WEB/icon-maskable-512.png" >/dev/null
upload "$TET" 'Mùng một.png' "$WEB/icon-192.png" >/dev/null
upload "$DOCS" 'Kế hoạch Tết.md' "$WORK/plan.md" >/dev/null
upload "$DOCS" 'Gia đình.csv' "$WORK/family.csv" >/dev/null
upload "$DOCS" 'Hợp đồng thuê nhà.pdf' "$WORK/doc.pdf" >/dev/null
upload "$DOCS" 'trang-web.html' "$WORK/page.html" >/dev/null
upload "$ROOT" 'Đi chợ.txt' "$WORK/note.txt" >/dev/null
upload "$ROOT" 'Sao lưu điện thoại.zip' "$WORK/backup.zip" >/dev/null
OLD="$(upload "$ROOT" 'Bản nháp cũ.txt' "$WORK/note.txt")"

# One item in the trash, one folder shared with two people, one public link.
curl -fsS -o /dev/null -b "$JAR" -H "$CSRF" -X DELETE "$BASE/api/nodes/$OLD"
curl -fsS -o /dev/null -b "$JAR" -H "$CSRF" -H "$JSON" \
  -d '{"emails":["me@example.com"],"role":"editor"}' "$BASE/api/nodes/$PHOTOS/shares"
curl -fsS -o /dev/null -b "$JAR" -H "$CSRF" -H "$JSON" \
  -d '{"emails":["ba@example.com"],"role":"viewer"}' "$BASE/api/nodes/$PHOTOS/shares"
LINK="$(curl -fsS -b "$JAR" -H "$CSRF" -H "$JSON" -d '{"expires_in_days":7}' "$BASE/api/nodes/$DOCS/links" | field url)"

# A device password for WebDAV sync clients (shown once by the API).
APP_PW="$(curl -fsS -b "$JAR" -H "$CSRF" -H "$JSON" -d '{"name":"dev script"}' "$BASE/api/app-passwords" | field password)"

cat <<EOF
Sample data created at $BASE

  Owner (admin):        $BASE/dev/login?email=admin@example.com
  Editor of "Ảnh gia đình": $BASE/dev/login?email=me@example.com
  Viewer of "Ảnh gia đình": $BASE/dev/login?email=ba@example.com
  Not invited (denied): $BASE/dev/login?email=stranger@example.com
  Public link to "Giấy tờ": $LINK

WebDAV (sync clients): $BASE/dav/
  user: admin@example.com    device password: $APP_PW
  curl:   curl -u 'admin@example.com:$APP_PW' -X PROPFIND -H 'Depth: 1' $BASE/dav/
  rclone: rclone lsf :webdav: --webdav-url $BASE/dav/ --webdav-vendor owncloud \\
            --webdav-user admin@example.com --webdav-pass "\$(rclone obscure '$APP_PW')"
EOF
