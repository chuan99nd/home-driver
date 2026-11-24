# drive/ — the family Drive web app

One static Go binary: files and folders, chunked upload, sharing by email, public
links, trash, WebDAV for sync clients, and an admin-only reverse proxy to the curator. Read the root
`CLAUDE.md` (the cross-component contract) and `docs/DESIGN.md` (the UI/UX standard,
binding for everything under `internal/server/web/`) before changing anything here.

Priorities, in order: **authorization correctness, not losing data, then features.**

## Run, test, lint

```bash
make build          # CGO_ENABLED=0 -> bin/drive (static, ~12 MB)
make test           # go test ./...   (no network, no Google)
make race           # go test -race   (needs gcc)
make lint           # go vet + gofmt check
scripts/dev.sh      # throw-away local instance on :8090 with DRIVE_DEV_LOGIN=1 + sample data
scripts/dev.sh 8190 # same, other port; Ctrl-C stops it and deletes its temp dir
scripts/seed.sh http://127.0.0.1:8090   # (re)create the sample data on a running dev instance
```

`scripts/dev.sh` prints sign-in URLs for an owner, an editor, a viewer, a not-invited
account, a public link, and a ready-made WebDAV device password with `curl` and
`rclone` commands. The module targets Go 1.24 (`GOTOOLCHAIN=local` in the
Makefile); dependency versions are pinned to ones that build with it — do not
`go get -u` without checking the `go` line in `go.mod` stays at 1.24.

Dependencies (keep it at these four): `modernc.org/sqlite` (pure-Go SQLite, so no
cgo), `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2` (Google sign-in),
`golang.org/x/net/webdav` (the WebDAV protocol handler and lock table; pinned at
x/net v0.40.0, which builds with Go 1.24).

## Package map

| Path | What lives there |
|---|---|
| `cmd/drive/main.go` | flag-less entry point: `drive` / `drive serve` runs the server, `drive backup-db <dest>` writes a consistent DB copy (`VACUUM INTO`). Signal handling, graceful shutdown, server timeouts. |
| `internal/config` | env parsing and **all** startup validation (`FromEnv`, `Validate`), `NormalizeEmail`, `IsAdmin`. |
| `internal/db` | `Open` (WAL, `busy_timeout`, `foreign_keys`, `_txlock=immediate`) and the embedded forward-only migrations in `migrations/NNNN_name.sql`. |
| `internal/access` | `Resolve(principal, nodeID)` — **the only authorization decision in the code base**. Also the `Node` type and its column list. |
| `internal/blob` | content-addressed files on disk: `Path`, `Open`, `Ingest` (fsync + atomic rename), `Remove`. Knows nothing about the DB. |
| `internal/server` | everything HTTP. `server.go` routes + middleware (logging, panic recovery, CSRF) + `authz` helpers; `auth.go` sessions, login rule, Google OIDC, dev login; `nodes.go` list/mkdir/rename/move/copy/trash/search/usage; `uploads.go` chunk protocol; `content.go` download, safe content types, thumbnails, zip; `shares.go` shares, public links, public endpoints; `trash.go` restore, **purge**, background jobs; `admin.go` allowlist + curator proxy; `apppw.go` app passwords, Basic auth for WebDAV, failure rate limit, `clientIP`; `dav.go` the `/dav/` handler and the node-backed `webdav.FileSystem`; `web.go` embedded UI. |
| `internal/server/web/` | the UI: `index.html`, `public.html`, `tokens.css` (identical to `docs/DESIGN.md`), `app.css`, `i18n.js` (vi + en dictionaries), `ui.js` (shared components), `app.js`, `public.js`, `sw.js`, `manifest.webmanifest`, `icons.svg` (sprite), app icons. |
| `scripts/` | `dev.sh`, `seed.sh` (local development only). |

## Request lifecycle

`recoverer → logging → csrf → ServeMux → handler`

1. **logging** writes one JSON line per request with the *route pattern* (never the raw
   path, so link tokens stay out of logs), status, bytes, ms, user email.
2. **csrf** (all non-GET/HEAD/OPTIONS): `Sec-Fetch-Site`, if present, must be
   `same-origin`/`none`; `Origin`, if present, must equal the `DRIVE_BASE_URL` origin;
   and every `/api/` request must carry the custom header `X-Drive-Csrf` (any value).
   `/curator/*` gets the first two checks only (the curator's pages cannot send our header).
3. **mux** uses Go 1.22 method+path patterns. Handlers that need a user are wrapped in
   `s.user(...)` (401 otherwise) or `s.admin(...)` (403 for non-admins).
4. `currentUser` loads the session and **re-applies the login rule on every request**:
   removing someone's allowlist entry or last share ends their session at once.
5. The handler calls `s.authz(w, r, principal, nodeID, minLevel)` (or `authzMutate`,
   `authzShareable`, `authzTrashed`) — all thin wrappers over `access.Resolve`. `None`
   and trashed answer **404** (existence is not leaked); too low a level answers 403.
6. Responses: JSON via `s.json`/`s.fail` (`{"error": code, "message": text}`); the UI
   maps `error` codes to human sentences in `i18n.js` (`err.<code>`).

## Data model (SQLite) and the invariants that must hold

Tables: `users`, `allowlist`, `sessions`, `blobs`, `nodes`, `shares`, `links`,
`uploads`, `app_passwords`, `schema_migrations`. Times are unix seconds; emails are stored lower-cased.

**1. Access has one entry point.** Every handler that reads or changes a node goes
through `access.Resolve`. Rules: owner → `owner`; otherwise the strongest share
addressed to the user's email on the node *or any ancestor* (`viewer` < `editor`);
a public-link visitor is a `Principal{LinkNodeID}` with `viewer` on the linked node
and its descendants only, never combined with a user identity; a node in the trash
(itself or via an ancestor) is visible to its owner only; broken or too-deep chains
fail closed. `Result.GrantIdx`/`Visible()` give the breadcrumbs a principal may see —
never show ancestors above it. The only queries that skip `Resolve` are lists that are
scoped by construction: children of an already-authorized folder, and the caller's own
trash (`owner_id = me`). Do not add ad-hoc `owner_id ==` checks in handlers.

**2. A tree has one owner.** Nodes created inside a shared folder (by an editor)
belong to the folder's owner. Consequences: *move* never crosses owners (403
`cross_owner`; use copy); *copy* makes a node owned by the destination's owner.

**3. Changing a node itself needs Editor on its parent too** (`authzMutate`). So an
editor can rename/move/trash things *inside* a shared folder but not the shared item
itself, and nobody can touch a root. Only the owner shares, makes links, lists shares,
or deletes forever. An editor may restore what *they* trashed (in place) — this is what
the UI's Undo uses; everything else in the trash is owner-only.

**4. Blob refcounts.** `blobs.refcount` = number of `nodes` rows (trashed included)
with that `blob_sha`. It changes in exactly four places, each in the same transaction
as the row change: upload completion (+1, upsert), copy (+1), purge (−n), and the
WebDAV commit (`davFS.commit`: +1 for the new content; the old blob's reference moves
to the trashed previous-version row, or is −1 when the old content was empty — so a
`blobs` row for the empty file may sit at refcount 0, which is harmless). A blob file
is removed only in `server.purge`, after commit, when the refcount reached 0 **and** no
row references it; purge aborts if the two disagree. `Server.blobMu` serializes upload
completion against purge so a blob can never be deleted while an identical upload is
being committed. Upload completion ingests the file (fsync, rename, dir fsync)
*before* committing the row; a failed commit leaves an unreferenced file, which is
harmless and reused.

**5. Trash is the only permanent-delete path.** `DELETE /api/nodes/{id}` only sets
`trashed_at` (+ `trashed_by`) on that node; descendants are trashed implicitly.
`server.purge(id)` refuses any node whose own `trashed_at` is NULL and is called from
three places only: delete-forever, empty-trash, and the hourly auto-purge
(`DRIVE_TRASH_DAYS`, 0 disables). Restore puts a node back in its folder, or in the
owner's root when that folder is in the trash; name clashes get a ` (n)` suffix.

**6. Names.** Unique among the *live* children of a folder (partial unique index;
case-sensitive). Conflict policy: **reject with 409 `name_conflict`** for create
folder, rename, move, and upload by default; **auto-suffix** `name (1).ext` for copy,
restore, and uploads that ask with `"on_conflict": "rename"` (the UI does).
Moving a folder into itself/a descendant is rejected inside the transaction
(`into_self`). Folder depth is capped at 100 (`access.MaxDepth` is 128).

**7. Chunk size.** Uploads are chunked because Cloudflare's free plan rejects bodies
over ~100 MB. The server dictates the chunk size (16 MiB; hard cap
`config.MaxChunkSize` = 32 MiB). Never add an endpoint that takes a whole file in one
request. Chunks are strictly sequential, streamed to `DATA_DIR/tmp/<id>.part`, never
buffered; the running sha256 state is stored in `uploads.hash_state` after each chunk.

**8. User content is never active on our origin.** `content.go` has an allowlist:
images/video/audio/PDF are served inline with their real type; text-like extensions
(including `.html`, `.svg`, `.js`, `.xml`) inline as `text/plain`; everything else is
`application/octet-stream` + `attachment`. Always `nosniff` and
`Content-Security-Policy: default-src 'none'; …; sandbox` (no `sandbox` for PDF, the
browser viewer needs it off). App pages have a strict CSP without `unsafe-inline`:
**no inline scripts or styles in the UI**, ever.

**9. WebDAV obeys all of the above.** `dav.go` never touches a node without
`access.Resolve`: a path is cleaned lexically (`path.Clean`, so `..`, `//` and encoded
slashes cannot leave the tree), walked by exact, case-sensitive child-name lookups
from the user's own root or from an item shared with them, and the resulting id is
then authorized like any API call. `davFS.mutable` is the twin of `authzMutate`.
DELETE (and MOVE/COPY overwriting a target) only trashes. PUT over an existing file
keeps the node id (shares and links survive) and leaves the previous content as a
trashed sibling row — recoverable until the trash is purged; identical content is a
no-op and an empty previous file is not kept. A PUT whose body is cut short commits
nothing. MOVE **and COPY** never cross owners over WebDAV (stricter than the JSON
copy, which lets a viewer copy into their own drive).

## API summary

All under `/api`, JSON, session cookie + `X-Drive-Csrf` on writes.

- Session: `GET /api/session`, `POST /api/logout`; `GET /auth/login`, `/auth/callback`; `GET /dev/login?email=` (dev only).
- Nodes: `GET /api/nodes/{id}?sort=name|date|size&order=asc|desc` → `{node, role, breadcrumbs, children}`; `POST /api/nodes/{id}/children {name}`; `PATCH /api/nodes/{id} {name?, parent_id?}`; `POST /api/nodes/{id}/copy {parent_id, name?}`; `DELETE /api/nodes/{id}` (to trash); `GET …/content[?download=1]` (Range, ETag = sha256), `…/thumb`, `…/zip`.
- `GET /api/search?q=` (own drive + everything shared; empty `q` → recent files), `GET /api/usage`, `GET /api/shared`.
- Uploads: `POST /api/uploads {parent_id, name, size, sha256?, on_conflict?}` → `{id, chunk_size}`; `PUT /api/uploads/{id}/chunk?offset=N` (exactly the next chunk; 409 + `offset` when out of order); `GET /api/uploads/{id}` → `{offset}` (resume); `POST /api/uploads/{id}/complete {sha256?}` (422 `hash_mismatch`); `DELETE /api/uploads/{id}`.
- Sharing: `GET|POST /api/nodes/{id}/shares {emails[], role}`, `DELETE /api/shares/{sid}`; `GET|POST /api/nodes/{id}/links {expires_in_days?, password?}` (the URL is returned once; only the token hash is stored), `DELETE /api/links/{lid}`.
- Trash: `GET /api/trash`, `POST /api/trash/{id}/restore`, `DELETE /api/trash/{id}`, `DELETE /api/trash`.
- Admin: `GET|POST /api/admin/allowlist`, `DELETE /api/admin/allowlist?email=`.
- Public (no session): `GET /s/{token}` (page), `GET /api/public/{token}`, `POST …/unlock {password}`, `GET …/nodes/{id}`, `…/content`, `…/thumb`, `…/zip`.
- App passwords (session): `GET /api/app-passwords` → `{items, dav_url, username}`, `POST {name}` (the password is returned once), `DELETE /api/app-passwords/{id}`.
- WebDAV: `/dav/` (HTTP Basic, see below).
- `GET /healthz` (no auth; checks DB and data dir). `/curator/*` → `CURATOR_URL`.

## Auth details

- Login rule (`loginAllowed`): email in `DRIVE_ADMIN_EMAILS`, in `allowlist`, or the
  recipient of at least one share. Case-insensitive. Google ID token must have
  `email_verified`. A refused login redirects to `/?denied=1` (friendly page).
- Sessions: random 256-bit cookie token, stored as HMAC (keyed by
  `DRIVE_SESSION_SECRET`); cookie `HttpOnly`, `SameSite=Lax`, `Secure` when the base
  URL is https; 30 days.
- OIDC discovery happens on first login, not at startup, so the server boots offline.
- Public links: 256-bit token; password stored as PBKDF2-SHA256 (600k); the unlock
  cookie is an HMAC bound to the link id and password hash; 10 attempts/minute/link.
- `DRIVE_DEV_LOGIN=1` registers `/dev/login`. `config.Validate` refuses it unless
  `DRIVE_ADDR` is loopback **and** `DRIVE_BASE_URL` is `http://localhost…` or
  `http://127.0.0.1…`; `main` logs a loud warning. It applies the same login rule.

## WebDAV (`/dav/`) and app passwords

For phone/desktop sync clients. Tree: `/dav/` is the user's My Drive; the virtual,
read-only folder `/dav/_Shared with me/` has one entry per item shared with the user
(two shared items with the same name get ` (<id prefix>)` appended). That name is
**reserved** at the top of every drive: `reservedName` rejects it in mkdir, rename,
move, copy and upload (`reserved_name`), and migration 0002 renamed any existing one,
so a real folder can never shadow or be shadowed by the virtual one.

- **Auth**: HTTP Basic, email + an app password, nothing else. Session cookies are
  ignored on `/dav/` (a cookie-only request gets 401), so DAV verbs are not reachable
  with ambient browser credentials; the CSRF middleware's `Origin`/`Sec-Fetch-Site`
  checks still apply (sync clients send neither), and no `X-Drive-Csrf` is needed
  because the path is not under `/api/`. Unauthenticated `OPTIONS` answers the
  capability probe.
- **App passwords** (`apppw.go`): `hd-` + 160 random bits (base32), shown once, stored
  as sha256 (fast hash is fine at that entropy), compared in constant time, listed
  with name / created / last used, revocable. The login rule is re-checked on every
  request: an account that may no longer sign in cannot use its app passwords.
- **Rate limit**: failed attempts per 5 minutes — 10 per account, 20 per source
  address — then 429. `clientIP` trusts `CF-Connecting-IP` / the right-most
  `X-Forwarded-For` entry **only when the connection comes from loopback** (always
  true behind the tunnel); otherwise the socket address is used. In memory only.
- **Reads**: GET/HEAD are served by `serveFileAs` as attachments with the sandbox CSP
  (the generic handler would pick an active content type). `PROPFIND` needs
  `Depth: 0` or `1`; infinite depth is refused (403 `propfind-finite-depth`).
- **Writes**: PUT streams to `DATA_DIR/tmp/dav-*.part` while hashing, then the same
  ingest + transaction as an upload (`davFS.commit`). `X-OC-Mtime` is honoured (rclone
  with `--webdav-vendor owncloud`/`nextcloud`, FolderSync). Leftover temp files are
  removed by the stale-upload cleanup. PROPPATCH is not supported (dead properties
  answer 403 per property), so clients that set times only that way keep server times.
- **Locks**: class 2 via `webdav.NewMemLS`, one table per user, in memory — **locks do
  not survive a restart** and are not shared between users of a shared folder.
- **Size**: a PUT is one request, so files over ~100 MB fail through the Cloudflare
  tunnel. This is the documented exception to invariant 7; the UI says so and points
  to the website for large files.
- Status codes: a refused write is 403 (`davStatusWriter` rewrites the handler's
  404/405/500 when a permission check failed).

Try it locally: `scripts/dev.sh` prints a device password and commands. With rclone:
`rclone lsf :webdav: --webdav-url http://127.0.0.1:8090/dav/ --webdav-vendor owncloud --webdav-user admin@example.com --webdav-pass "$(rclone obscure '<password>')"`.
Tests live in `dav_test.go` (`e.invariants()` checks refcounts over the whole DB).

## Curator proxy

`/curator/*` → `CURATOR_URL`, admins only (403 otherwise; anonymous GET is redirected
to `/`). `/curator` redirects (308) to `/curator/` because the curator UI uses relative
URLs. The prefix is stripped; every client-supplied `X-Home-Driver-*` header is removed
and `X-Home-Driver-Email` is set; Drive's cookies are not forwarded. `Host` and `Origin`
are passed through unchanged and `X-Forwarded-Host` is set to the browser's Host,
because the curator runs its own same-origin check. The UI shows "Dọn ảnh / Photo
cleanup" to admins when the proxy is configured.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `DRIVE_ADDR` | `127.0.0.1:8090` | listen address; a non-loopback address logs a warning |
| `DRIVE_DB_PATH` | `/var/lib/home-driver/drive/drive.db` | SQLite file (its directory is created) |
| `DRIVE_DATA_DIR` | `/srv/home-driver/data/drive` | holds `blobs/`, `tmp/`, `thumbs/`. **Must already exist** — the server refuses to start otherwise, so an unmounted data disk never gets written to the root filesystem |
| `DRIVE_BASE_URL` | required | public URL, `http(s)://host[:port]`, no path; used for the OAuth redirect (`<base>/auth/callback`), share links and the CSRF origin check |
| `DRIVE_GOOGLE_CLIENT_ID` | required unless dev login | Google OAuth client id |
| `DRIVE_GOOGLE_CLIENT_SECRET` | required unless dev login | Google OAuth client secret |
| `DRIVE_ADMIN_EMAILS` | required | comma-separated admin emails |
| `DRIVE_SESSION_SECRET` | required | ≥ 32 characters (`openssl rand -hex 32`); changing it signs everyone out |
| `CURATOR_URL` | `http://127.0.0.1:8091` | curator upstream; set to empty (`CURATOR_URL=`) to disable the proxy |
| `DRIVE_TRASH_DAYS` | `30` | auto-purge age of trashed items; `0` disables auto-purge |
| `DRIVE_DEV_LOGIN` | `0` | `1` enables `/dev/login` — local development and tests only |

Add any new variable here, in `config.go` (with validation) and in
`deploy/config/drive.env.example`.

## How to add an endpoint (checklist)

1. **Route** in `routes()` (`server.go`) with method + pattern, wrapped in `s.user` or
   `s.admin` unless it is deliberately public.
2. **Access check**: get every node id through `s.authz…` with the right minimum
   level, *before* doing anything. Ids from the body (e.g. `parent_id`) count too. If
   the id comes from another table (a share id, a link id), load the row, then
   authorize its `node_id`.
3. **CSRF**: use a non-GET method for anything that changes state — the middleware
   then covers it. Never change state in a GET.
4. **Input**: `s.readJSON` (64 KiB limit, unknown fields rejected), `validName`,
   `parseEmail`. Multi-statement changes go in one transaction.
5. **Test** in `internal/server/*_test.go` using the harness (`newEnv`, `e.login`,
   `c.call`): the happy path, the other roles (viewer/editor/stranger/anonymous), and
   an id from outside the caller's subtree. Add the escape attempt to
   `TestSharedSubtreeEscape` if the endpoint takes a node id.
6. **UI**: strings in both dictionaries in `i18n.js` (`TestI18nCoverage` enforces it),
   `textContent` only, new error codes mapped under `err.<code>`.
7. Update this file.

## How to add a migration

Create `internal/db/migrations/NNNN_description.sql` with the next number (contiguous,
4 digits). It runs once, in a transaction, at startup. Migrations are forward-only:
never edit one that has shipped; a DB newer than the binary is refused. Keep the
invariants above true for existing rows (e.g. backfill, do not just add a NOT NULL).
`internal/db/db_test.go` checks the bookkeeping.

## UI notes

- No build step, no framework, no CDN. ES modules served from `/static/`; the service
  worker is at `/sw.js` (root scope) and caches **only** `/` and `/static/*`,
  network-first — it does not intercept `/api`, `/s/`, `/auth` or `/curator` at all.
- `ui.js` holds the single implementation of dialog, menu/bottom sheet, toast, item
  card/row, lightbox; `app.js` uses them. `actionsFor(nodes)` is the one place that
  decides which actions an item offers.
- Reversible actions (trash, move, rename, unshare) act immediately and show a toast
  with Undo. Only delete-forever, empty-trash and disconnecting a sync device (its
  password cannot be shown again) ask for confirmation.
- "Đồng bộ thiết bị / Sync devices" (`#/sync`, `renderSync`) is in the sidebar and
  the account menu: server address, username, device passwords, per-platform steps.
- Name sorting is done in the browser with `Intl.Collator` (SQLite's `NOCASE` only
  folds ASCII, which mis-sorts Vietnamese).

## Known limitations

- WebDAV: files over ~100 MB cannot be uploaded through the tunnel (single PUT); no
  PROPPATCH; locks are in memory; COPY of a big file re-reads it (then dedups); every
  overwrite by a sync client adds a previous-version entry to the owner's trash
  until the auto-purge (`DRIVE_TRASH_DAYS`), which uses disk space for files that are
  edited often. No new environment variables.
- Only files can be copied (not folders). Folder upload works through the picker and
  drag-and-drop where the browser supports `webkitdirectory`.
- The browser does not send a sha256 (hashing multi-GB files in JS is too slow); the
  server computes it. API clients can pass `sha256` at init or complete for an
  end-to-end check.
- A public link's URL cannot be shown again after creation (only its hash is stored);
  create a new link instead.
- Thumbnails only for JPEG/PNG/GIF (stdlib decoders; EXIF orientation honoured). No
  HEIC, no video posters. `thumbs/` is a cache and may be deleted at any time.
- Editor on a single shared *file* can do nothing more than a viewer (there is no
  "replace content" operation yet).
- No per-user quota; `GET /api/usage` reports bytes owned and, for the UI's admin view,
  free disk space.
- Search is by name only, max 200 results.
- Sessions of a removed admin end only when `DRIVE_ADMIN_EMAILS` changes, i.e. at restart.
- The service worker and "Add to Home Screen" need a real https origin (or localhost)
  and were not verifiable in the embedded test browser; check them on a phone.
