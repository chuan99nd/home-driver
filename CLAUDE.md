# home-driver

Self-hosted family "Google Drive + Google Photos" running on one always-on laptop
with two USB HDDs. Photos are handled by [Immich](https://immich.app) (not our code);
this repo holds everything around it.

This file is the contract between components. If you change anything in
"Shared contract" below, update this file and every component that depends on it in
the same change.

## Components

| Dir | What | Stack | Own guide |
|---|---|---|---|
| `deploy/` | Immich compose, systemd units, disk/backup/restore scripts, runbook | bash, systemd, docker compose | `deploy/CLAUDE.md` |
| `drive/` | Drive web app: folders, upload, share by email, links, trash | Go (stdlib `net/http`), SQLite, embedded vanilla-JS UI | `drive/CLAUDE.md` |
| `curator/` | Finds junk / burst / blurry photos in Immich, fills a review queue | Python (uv), FastAPI, SQLite | `curator/CLAUDE.md` |

Each component is independent: its own build, tests and guide. Read the component
guide before editing inside it.

## Goals, in priority order

1. **Never lose data.** Family photos cannot be retaken.
2. **Cheap.** One laptop, two HDDs, free-tier services. No paid dependency is required.
3. **Private.** Photos and files stay on the laptop. Any cloud AI is opt-in and off by default.
4. **Low maintenance.** One owner, not a sysadmin. Things must alert when broken and
   be recoverable with one documented command.

## Hard rules (do not break these)

- **No code path deletes user data permanently without an explicit human action.**
  The curator only *suggests*; approving a suggestion moves the asset to the Immich
  trash (recoverable), never force-deletes. Drive deletes go to trash first.
- **Scripts never format, wipe or repartition a disk** unless the user passes the
  device explicitly *and* a confirmation flag. No defaults for device names.
- **Services must not start when their disk is not mounted** (`RequiresMountsFor=`),
  so nothing ever writes user data onto the root filesystem by accident. The
  backup, scrub and SMART jobs are different on purpose: they use `After=` the
  mounts and check the disks themselves (sentinel files), because a failed unit
  dependency would skip `OnFailure=` and a missing disk must produce an alert.
- **Nothing listens on a public interface.** Every service binds `127.0.0.1`; the
  only way in from the internet is the user's existing Cloudflare Tunnel. The one
  opt-in exception is `IMMICH_LAN_ADDR` (empty by default): it additionally
  publishes Immich on one private home-network address so phones can upload large
  videos over home Wi-Fi, past Cloudflare's 100 MB limit. `immich-compose.sh`
  refuses any non-private address. Drive and Curator have no such option.
- **No secrets in git.** Only `*.example` env files are committed.
- **Do not edit the user's cloudflared config.** Ship example ingress snippets only.
- **Do not add a frontend build step** (no npm/bundler) to `drive/` or `curator/`.
  The UI is plain HTML/CSS/JS embedded in the binary/package.
- **Every UI change follows `docs/DESIGN.md`** (phone-first, Vietnamese + English,
  shared tokens, undo instead of confirm, its "definition of done"). The users are
  non-technical family members; design for the least technical one.
- Keep dependencies few and boring. Justify each new one in the component guide.

## Shared contract

### Paths on the server

| Path | Disk | Content |
|---|---|---|
| `/srv/home-driver/data` | Disk A (btrfs, label `hd-data`) | all user data |
| `/srv/home-driver/data/immich` | Disk A | Immich `UPLOAD_LOCATION` (originals) |
| `/srv/home-driver/data/drive` | Disk A | Drive blobs (`blobs/`) and upload temp (`tmp/`) |
| `/srv/home-driver/backup` | Disk B (btrfs, label `hd-backup`) | nightly copy + read-only snapshots |
| `/var/lib/home-driver/immich-postgres` | internal SSD | Immich database |
| `/var/lib/home-driver/drive/drive.db` | internal SSD | Drive metadata (SQLite) |
| `/var/lib/home-driver/curator/curator.db` | internal SSD | Curator state (SQLite) |
| `/var/lib/home-driver/dumps` | internal SSD | DB dumps taken before each backup |
| `/etc/home-driver/` | internal SSD | env files (mode 0600) |
| `/opt/home-driver/` | internal SSD | installed binaries, venv, scripts |

Databases live on the SSD (speed, and a USB dropout must not corrupt them); they are
dumped to `/var/lib/home-driver/dumps` and copied to Disk A before each nightly backup.

### Ports (all on 127.0.0.1)

| Port | Service | Public hostname (via Cloudflare Tunnel) |
|---|---|---|
| 2283 | Immich | `photos.<domain>` |
| 8090 | Drive | `drive.<domain>` |
| 8091 | Curator | none — reached only through Drive at `/curator/` |

### systemd units (all prefixed `home-driver-`)

`home-driver-immich.service`, `home-driver-drive.service`,
`home-driver-curator.service`, plus `.service`+`.timer` pairs for
`backup-local`, `backup-offsite`, `scrub`, `smart-check`, `curator-scan`.
Services run as the unprivileged system user `home-driver` (Immich runs in Docker).

### Env files

- `/etc/home-driver/common.env` — shared: `HD_DOMAIN`, notification settings.
- `/etc/home-driver/immich.env`, `drive.env`, `curator.env`, `backup.env`.
- Variable prefixes: `DRIVE_*`, `CURATOR_*`, `BACKUP_*`, `NOTIFY_*`. Exceptions:
  `backup.env` also carries restic's own `RESTIC_*` / `AWS_*` names, and
  `immich.env` carries Immich's own names.
- The disks are mounted by `/etc/fstab` lines (by label, `nofail`) that
  `install.sh` appends, not by mount units.
- The Immich database is dumped with `pg_dump` of its one database (what Immich's
  docs prescribe), restored by `restore.sh immich-db`.
- Every variable must appear in the matching `deploy/config/*.env.example` with a comment.

### Auth

- Immich: its own accounts with Google OAuth configured in the Immich admin UI.
- Drive: "Sign in with Google" (OIDC). A Google account may sign in if its email is
  in `DRIVE_ADMIN_EMAILS`, is in the allowlist table, or something has been shared
  with it. Sharing with an email therefore grants login.
- Curator has no auth of its own. Drive reverse-proxies `/curator/*` to
  `CURATOR_URL` (default `http://127.0.0.1:8091`) **for admin users only**, strips
  the `/curator` prefix, and sets `X-Home-Driver-Email: <admin email>`. Curator
  must work when served under that prefix (use relative URLs in its UI).

### Notifications

One script, `/opt/home-driver/scripts/notify.sh "<title>" "<body>" [priority]`,
is the only way anything sends alerts (Telegram or ntfy, chosen in `common.env`).
Other components call it; they do not talk to Telegram/ntfy themselves.

### Cloudflare limit

Free Cloudflare plans reject request bodies over ~100 MB. Drive uploads are
therefore chunked (chunks ≤ 32 MB). Do not add an upload path that sends a whole
file in one request.

The one exception is WebDAV (`/dav/`, for phone and desktop sync clients): the
protocol sends a file in a single PUT, so files over ~100 MB fail through the
tunnel. That limit is stated in the UI; large files go through the website.

### Sync clients (WebDAV)

`drive.<domain>/dav/` with HTTP Basic: the user's email plus an **app password**
created in Drive's "Sync devices" screen (never the Google password, never a
session cookie). Deletes over WebDAV go to the trash like everywhere else.

## Working in this repo

```bash
make build     # build drive binary, sync curator venv
make test      # all tests (go test, pytest, script checks)
make lint      # go vet, ruff, shellcheck, systemd-analyze verify
```

- Run the tests of the component you touched before finishing; add a test for every
  bug fix and every new endpoint.
- Tests must not need Google, Immich, real disks or root. Use the fakes in each
  component (`DRIVE_DEV_LOGIN`, the fake Immich server in `curator/tests`).
- Bash: `set -euo pipefail`, shellcheck-clean, every destructive script supports `--dry-run`.
- Go: stdlib first, `gofmt`, errors wrapped with context, no global state outside `main`.
- Python: typed, `ruff` clean, no heavy ML frameworks in the default install.
- Write user-facing docs for a non-expert owner: exact commands, expected output.
- Immich's API and compose file change between releases. `deploy/immich/` pins a
  version; when bumping it, re-check the curator's API calls against that
  version's OpenAPI spec and note the checked version in `curator/CLAUDE.md`.

## Roadmap (good first tasks for a contributor)

- Curator: aesthetic score ("beautiful" album) from CLIP embeddings; closed-eyes detection.
- Curator: optional cloud captions (opt-in per album; off by default).
- Drive: test WebDAV with Finder, Windows, FolderSync and iOS clients (only rclone
  and curl were verified); copy of whole folders.
- Deploy: a loop-device test of setup-disks → backup-local → restore on real btrfs.
- Drive: full-text search inside documents.
- Deploy: monthly automated restore test that restores a random sample and compares hashes.
