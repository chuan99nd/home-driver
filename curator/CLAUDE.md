# curator

Finds photos that are probably not worth keeping in the family's Immich library (blurry,
dark/blank, screenshots, burst leftovers, Immich's own duplicates) and puts them in a review
queue. A human decides. Read the root `CLAUDE.md` (contract) and `docs/DESIGN.md` (UI
standard) before changing anything here.

## Invariants (do not break; each has a test)

- **Never force-delete.** The only removal call is `ImmichClient.trash_assets`, which always
  sends `"force": false`; it has no parameter to change that. Trashed items stay in Immich's
  trash (30 days by default in Immich) and can be restored from the UI (Undo) or in Immich.
- **The scan is read-only towards Immich.** Only a human action (POST with the proxy header)
  reaches a mutating endpoint.
- **Keep-list is permanent.** No code deletes from `keep_list`; a kept asset is never
  suggested again in any category. There is deliberately no "undo keep".
- **Favourites are untouchable.** Never suggested, always win as burst/duplicate keeper, and
  re-checked live in Immich at approval time (so is archived / trashed / keep-listed).
- **Never suggested:** favourites, anything whose visibility is not `timeline` (archived,
  hidden, locked), trashed assets, non-IMAGE assets (v1 analyses photos only; the UI says so).
- **A group can never lose every copy:** the keeper has no suggestion row, and a group
  approval is refused if the keeper is no longer available in Immich.
- **A human decision is never reopened automatically** (`kept` / `trashed` / `stacked`);
  only `pending` rows are changed by a scan (to `withdrawn`). Undo reopens exactly the rows
  its own trash action resolved.
- **Loopback only.** `CURATOR_ADDR` must be 127.0.0.1 / ::1 / localhost or the process
  exits with status 2.
- **Dry-run sends nothing to Immich** for trash, undo and stack, and says so.
- Everything is local: numpy + Pillow heuristics, no cloud AI, no torch/tensorflow/opencv.

## Module map (`src/home_driver_curator/`)

| File | Role |
|---|---|
| `config.py` | `Settings.from_env()`; loopback check (`parse_addr`); all defaults |
| `immich.py` | **The only Immich access.** Client, typed `Asset`, errors, retries, pagination |
| `imaging.py` | Pixel maths: decode/normalise (`Frame`), tile sharpness, dHash, flat-colour |
| `detectors.py` | Per-asset detectors + `DETECTORS` registry (`DetectorSpec`) |
| `scan.py` | Scan state machine, lock, analysis cache, burst grouping, suggestion rebuild, notify |
| `db.py` | SQLite schema + queries (`offer`, `reconcile`, keep-list, activity) |
| `actions.py` | Human actions: `keep`, `trash`, `undo_trash`, `set_keeper`, `stack` (+ activity log) |
| `i18n.py` | Every UI string, Vietnamese + English, one dict per language |
| `web.py` | FastAPI app: pages, thumbnail proxy, POST actions, security checks |
| `cli.py` | `home-driver-curator serve | scan | doctor` |
| `templates/`, `static/` | Jinja2 templates (autoescape on), `tokens.css`, `style.css`, `app.js`, `theme.js` |

Tests: `tests/fake_immich.py` (spec-shaped fake, also runnable), `tests/images.py`
(synthetic photos), `tests/dev_proxy.py` (stand-in for Drive's `/curator/` proxy).

## Immich client boundary

Checked against the Immich OpenAPI spec at release tag **v3.4.2**
(`open-api/immich-openapi-specs.json`). Requires Immich **>= 3.2.0** (cursor pagination and
the nested search `filter` were added there; the older flat fields and `page` are
deprecated). `doctor` prints the server version and fails below 3.2.0.

Auth: header `x-api-key`. API keys carry a permission list; `GET /api-keys/me` returns it.

| Call | Permission | Used for |
|---|---|---|
| `GET /api/server/version` | none | doctor |
| `GET /api/api-keys/me` | none | doctor: list missing permissions |
| `POST /api/search/metadata` body `{size, withExif:true, filter:{type:{eq:"IMAGE"}}, orderBy:{field:"fileCreatedAt",direction:"asc"}, cursor?}` → `assets.items`, `assets.nextCursor` | `asset.read` | listing, paginated |
| `GET /api/assets/{id}` | `asset.read` | live re-check before trashing |
| `GET /api/assets/{id}/thumbnail?size=preview\|thumbnail` | `asset.view` | analysis (preview) and UI images |
| `DELETE /api/assets` body `{ids, force:false}` → 204 | `asset.delete` | move to trash |
| `POST /api/trash/restore/assets` body `{ids}` → `{count}` | `asset.delete` | Undo |
| `GET /api/duplicates` → `[{duplicateId, assets[], suggestedKeepAssetIds[]}]` | `duplicate.read` | duplicate groups |
| `POST /api/stacks` body `{assetIds}` (first id becomes primary, min 2) | `stack.create` | "stack instead" |

The API key therefore needs: `asset.read`, `asset.view`, `asset.delete`, `duplicate.read`,
`stack.create` (or `all`). A 403 becomes `ImmichPermissionError` naming the permission.
Fields read from `AssetResponseDto`: `id, ownerId, type, originalFileName, originalMimeType,
checksum, updatedAt, fileCreatedAt, localDateTime, width, height, isFavorite, isTrashed,
isArchived, visibility, duplicateId, exifInfo.{make, model, exifImageWidth, exifImageHeight,
fileSizeInByte}`. Albums are not used.

Behaviour: timeouts (`CURATOR_IMMICH_TIMEOUT`), retries with exponential backoff on network
errors / 429 / 5xx (`CURATOR_IMMICH_RETRIES`; `POST /stacks` is not retried), 401 →
`ImmichAuthError`, 404 or 400 "Not found…" → `ImmichNotFound`, other 4xx → `ImmichBadRequest`.

**When bumping Immich:** fetch that tag's spec, re-check every row above, update
`CHECKED_IMMICH_VERSION` in `immich.py`, this section and `tests/fake_immich.py` together.

**Not proven against a real server** (only against the spec-shaped fake): every call above.
Specific assumptions to confirm on first real use, all visible via `doctor` or a dry-run:
the 403 body/permission behaviour, 400-vs-404 for unknown ids, whether the default search
visibility includes archived assets (we filter client-side either way), cursor stability
while assets are added mid-scan, and that `fileCreatedAt` carries the EXIF capture time.

## Detectors

Interface (`detectors.py`): a pure function `fn(frame: Frame, meta: AssetMeta) -> Finding`.
`Finding(score, reason, veto)`: a raw score, an English technical reason (shown only under
"Why was this suggested?" and in `doctor`), and `veto=True` when the detector refuses to
judge. Detectors never see thresholds or do I/O. All findings are stored per asset
(`assets.scores` JSON); `DetectorSpec.flags(score, veto, settings)` decides at suggest time,
so **changing a threshold or switch needs no re-download** — the next `scan` applies it.

| Detector | Score | Flags when | Guards (veto) |
|---|---|---|---|
| `blurry` | relative sharpness: Laplacian variance / detail variance per tile (8x8 grid on a 512 px grey image), mean of the 4 sharpest textured tiles | `< CURATOR_BLUR_THRESHOLD` (0.12) | night shots (mean luma < 40); fewer than 25 % textured tiles (sky, wall, fog). Using the sharpest tiles protects bokeh. |
| `dark` | tonal information 0..255: `min(p99.9 − p0.1, p99.9/2, (255 − p0.1)/2)` | `< CURATOR_DARK_THRESHOLD` (12) | none |
| `screenshot` | evidence 0..1: no camera 0.15, file name 0.45, PNG 0.2, screen aspect 0.1, flat colour 0.1–0.2, few colours 0.1 | `>= CURATOR_SCREENSHOT_THRESHOLD` (0.6) | any EXIF make/model |

Groups (not per-asset detectors, see `scan.py`):
- **burst**: same owner + same camera (unknown camera never groups), each frame within
  `CURATOR_BURST_WINDOW_SECONDS` of the previous one and within `CURATOR_BURST_HAMMING`
  dHash bits of the group's *first* frame. Keeper: a favourite if any, else highest absolute
  sharpness; a keeper the user picked is remembered (`groups.keeper_by_user`).
- **duplicate**: Immich's `GET /duplicates`, not re-implemented. Keeper: favourite, else
  Immich's `suggestedKeepAssetIds`, else the largest file.

Calibration honesty: defaults were set from synthetic images plus ~20 public sample photos
(sharp ≈ 0.8–8, heavily blurred ≈ 0.03–0.12). They are deliberately conservative: only
gross blur is caught, motion blur and photos of paper receipts are not. Tune on the real
library (below) before trusting them.

### Adding a detector (checklist)

1. Write `detect_x(frame, meta) -> Finding` in `detectors.py` (pure; add guards that veto).
2. Add `detect_x: bool` and `x_threshold: float` to `Settings` + `from_env`
   (`CURATOR_DETECT_X`, `CURATOR_X_THRESHOLD`); add them to the env table below and to
   `deploy/config/curator.env.example` (root contract).
3. Append a `DetectorSpec` to `DETECTORS`; bump `ANALYSIS_VERSION` in `scan.py` so existing
   assets are re-analysed over the next nights.
4. Add generators to `tests/images.py` and tests in `tests/test_detectors.py`, including at
   least one image that must NOT be flagged.
5. UI category: add `cat_x`, `help_x`, `reason_x` to **both** languages in `i18n.py`; the
   dashboard and review screens pick it up. Use `DEFAULT_MODE` in `web.py` for grid-first.

## Scan state machine

```
lock ──> running ──> (list page → upsert metadata → analyse new/changed → commit) * N
                        │ stop requested          → interrupted   (queue untouched)
                        │ Immich down / denied    → failed        (queue untouched)
                        └ listing complete ──> mark unseen assets absent ──> rebuild
                           suggestions (single detectors, bursts, duplicates) ──> completed
                           ──> notify.sh if new suggestions > 0 (best effort)
```

- **Lock:** `flock` on `<db>.scan.lock`; a second scan exits 0 with a log line. A `running`
  row found while holding the lock is a crashed scan and is marked `interrupted`.
- **Incremental:** an asset is analysed when its signature
  `v{ANALYSIS_VERSION}|checksum|updatedAt` differs from the stored one. Metadata flags are
  refreshed on every listing regardless. Failed analyses are retried up to 3 times per
  signature.
- **Resumable / bounded:** results are committed per page; `CURATOR_SCAN_BATCH` caps
  analyses per run (the rest is reported as `remaining` and done on later nights); memory is
  bounded by one page of metadata plus `CURATOR_SCAN_CONCURRENCY` decoded previews.
  ~30 ms CPU per photo on a dev machine; 100k photos at the default 5000/night ≈ 20 nights —
  raise the batch if the laptop copes.
- **Rebuild** runs only after a complete listing, never on a partial one.

## Database (`CURATOR_DB_PATH`, WAL mode)

- `assets` — one row per Immich asset seen: metadata, `present`, `last_seen_scan`, and the
  analysis cache (`signature`, `sharpness`, `dhash`, `scores` JSON, `error`, `attempts`).
- `suggestions` — `UNIQUE(asset_id, category)`; `status` ∈ `pending | withdrawn` (automatic)
  `| kept | trashed | stacked` (human); `group_id` for burst/duplicate.
- `groups`, `group_members` — burst/duplicate groups; `key` = hash of kind + member ids.
- `keep_list` — permanent.
- `activity` — every action: time, user (from `X-Home-Driver-Email`), action, assets,
  `dry_run`, `undone`.
- `scans` — one row per run with counters and status.

Schema changes: bump `SCHEMA_VERSION` and write a migration; never drop `keep_list` or
`activity`.

## Web UI

- Served by Drive under `/curator/` with the prefix stripped. **Every route is one path
  segment deep and every URL is relative** (`review?...`, `thumb?...`, `static/x`, redirects
  too); `../` is the link back to Drive. `tests/test_web.py` crawls every page in both
  languages and fails on any absolute URL. Drive must redirect `/curator` → `/curator/`.
- Routes: `GET /` (overview), `GET review?category=&mode=one|grid&i=&page=`, `GET activity`,
  `GET thumb?id=&size=thumbnail|preview` (proxied, so the browser never sees the API key),
  `GET static/<file>`, `GET healthz`; `POST keep | trash | undo | stack | keeper`.
- Security: every route except `healthz` and `static/` requires `X-Home-Driver-Email`
  (403 otherwise). POST routes additionally require a same-origin request
  (`Sec-Fetch-Site: same-origin`, or `Origin` host equal to `Host` / `X-Forwarded-Host`).
  CSP `default-src 'none'` with self-only images/styles/scripts; no inline script or style.
- Follows `docs/DESIGN.md`: phone first, vi/en from `i18n.py` (`?lang=` → cookie →
  `Accept-Language`), one decision at a time (K keep, X trash, arrows, swipe), burst compare
  with a changeable keeper and "stack instead", grid mode with select-all, Undo toast,
  "N left" progress, skeleton / failed-image / empty / error states, inline SVG sprite.
  `static/tokens.css` must stay identical to the token block in `docs/DESIGN.md` (tested).
- No user-visible literal in templates or JS (tested): add strings to `i18n.py`.
- Removal flow: a single photo or a burst group is trashed immediately and offers Undo;
  a multi-selection in grid mode is confirmed once with the count (dialog, or a plain page
  without JS), then also offers Undo. The History page keeps Undo available afterwards.
- Known deviations from DESIGN.md: on phone review screens the toast sits under the app bar
  (at the bottom it would cover the Keep / Move-to-trash buttons); no bottom sheet, search,
  pinch-zoom or web-app manifest (the curator lives inside Drive's shell).

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `CURATOR_ADDR` | `127.0.0.1:8091` | listen address; must be loopback |
| `CURATOR_DB_PATH` | `/var/lib/home-driver/curator/curator.db` | SQLite file (a `.scan.lock` sits beside it) |
| `CURATOR_IMMICH_URL` | `http://127.0.0.1:2283` | Immich base URL (without `/api`) |
| `CURATOR_IMMICH_API_KEY` | (required) | Immich API key with the permissions above |
| `CURATOR_IMMICH_PUBLIC_URL` | empty | e.g. `https://photos.<domain>`; enables "Open in Immich" links |
| `CURATOR_IMMICH_TIMEOUT` | `30` | seconds per Immich request |
| `CURATOR_IMMICH_RETRIES` | `3` | retries on network errors / 429 / 5xx |
| `CURATOR_DRY_RUN` | `off` | `on`: trash / undo / stack send nothing to Immich and say so |
| `CURATOR_SCAN_BATCH` | `5000` | max photos analysed per scan run (`0` = no limit) |
| `CURATOR_SCAN_PAGE_SIZE` | `250` | assets per listing request (max 1000) |
| `CURATOR_SCAN_CONCURRENCY` | `2` | parallel preview downloads/analyses |
| `CURATOR_SCAN_SLEEP_MS` | `0` | pause before each download, to be gentler |
| `CURATOR_NOTIFY_SCRIPT` | `/opt/home-driver/scripts/notify.sh` | called as `<script> "<title>" "<body>"` when a scan adds suggestions; skipped if missing |
| `CURATOR_DETECT_BLURRY` | `on` | switch |
| `CURATOR_BLUR_THRESHOLD` | `0.12` | flag when relative sharpness is below this |
| `CURATOR_DETECT_DARK` | `on` | switch |
| `CURATOR_DARK_THRESHOLD` | `12` | flag when tonal information (0..255) is below this |
| `CURATOR_DETECT_SCREENSHOT` | `on` | switch |
| `CURATOR_SCREENSHOT_THRESHOLD` | `0.6` | flag when evidence (0..1) reaches this |
| `CURATOR_DETECT_BURST` | `on` | switch |
| `CURATOR_BURST_WINDOW_SECONDS` | `3` | max gap between consecutive burst frames |
| `CURATOR_BURST_HAMMING` | `6` | max dHash distance (of 64 bits) to the burst's first frame |
| `CURATOR_DETECT_DUPLICATES` | `on` | surface Immich's duplicate groups |

## Commands

```bash
cd curator
uv sync                      # venv with dev tools (production: uv sync --no-dev)
uv run pytest                # no network, no real Immich
uv run ruff check . && uv run ruff format --check .

home-driver-curator serve    # review UI (systemd: home-driver-curator.service)
home-driver-curator scan     # one incremental scan (systemd timer); --limit N, --no-suggest
home-driver-curator doctor   # config, Immich version, API-key permissions
```

### Run locally against the fake (three terminals, from `curator/`)

```bash
uv run python -m tests.fake_immich --port 2299 --demo        # sample library, key "test-key"

export CURATOR_ADDR=127.0.0.1:8191 CURATOR_DB_PATH=/tmp/curator-demo.db \
       CURATOR_IMMICH_URL=http://127.0.0.1:2299 CURATOR_IMMICH_API_KEY=test-key
uv run home-driver-curator scan && uv run home-driver-curator serve

uv run python -m tests.dev_proxy --port 8190 --curator http://127.0.0.1:8191
# open http://127.0.0.1:8190/curator/   (the proxy adds X-Home-Driver-Email like Drive does)
```

`curl http://127.0.0.1:2299/__requests` lists every call the fake received. To start over,
stop the fake and the curator, delete `/tmp/curator-demo.db*`, and start them again.

### Tuning thresholds on the real library

1. Set `CURATOR_DRY_RUN=on` first.
2. `home-driver-curator doctor --sample 500` analyses up to 500 not-yet-analysed photos
   **without creating suggestions**, then prints per-detector score percentiles, how many
   photos each current threshold would flag, and the 8 most extreme file names.
   `doctor --report` prints the same from what is already analysed.
3. Look those photos up in Immich. Move the threshold toward "fewer flags" until the listed
   ones are all genuinely junk, set the env var, run `scan` (no re-download needed), review.
4. Turn dry-run off when the queue looks right.

## Dependencies (keep few)

`fastapi` + `uvicorn` (HTTP), `httpx` (Immich client), `pillow` (decode), `numpy` (pixel
maths), `jinja2` (autoescaped templates — the structural guarantee that Immich-provided
strings are escaped). Form bodies are parsed with the stdlib, so `python-multipart` is not
needed. Dev: `pytest`, `ruff`.

## Known limitations

- Analyses Immich's 1440 px previews reduced to 512 px: only strong blur is detectable.
- No motion-blur, closed-eyes, aesthetic or "photo of a document" detection (roadmap).
- Bursts need camera make/model and a capture time; messenger-saved copies never group.
- Videos, live-photo motion parts, archived and locked assets are ignored entirely.
- The API key sees one Immich user's library (plus partners as Immich allows); run one
  curator per key.
- No "scan now" button; scans come from the systemd timer or the CLI.
- The 30-day trash window is Immich's setting (Administration > Settings > Trash); the UI
  text assumes the default.
- Undo is offered for removals only; Keep and Stack are not undoable here (unstack in Immich).
