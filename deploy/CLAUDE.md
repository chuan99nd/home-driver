# deploy/ - guide for contributors (human or AI)

This component is everything around the applications: the Immich compose file,
systemd units, disk / backup / restore scripts, config examples, and the owner's
documentation. Stack: bash, systemd, docker compose. No other language.

Read the root `CLAUDE.md` first: it is the contract (paths, ports, unit names,
env files, hard rules). This file explains how `deploy/` honours it.

**The one thing to remember:** this code protects irreplaceable family photos
and is operated by a non-expert. When in doubt a script must **refuse and
explain**, never guess. A false alarm costs the owner five minutes; a wrong
guess can cost twenty years of photos.

## File map

```
deploy/
  immich/
    docker-compose.yml       Immich, adapted from the official release file (pinned)
    docker-compose.lan.yml   optional override: publish Immich on one private LAN address
  systemd/
    home-driver-immich.service          wraps immich-compose.sh up/down
    home-driver-drive.service           Go binary, User=home-driver, hardened
    home-driver-curator.service         Python venv, User=home-driver, hardened
    home-driver-curator-scan.{service,timer}     nightly 00:30
    home-driver-backup-local.{service,timer}     nightly 02:30
    home-driver-backup-offsite.{service,timer}   nightly 04:30, After= the local backup
    home-driver-smart-check.{service,timer}      daily 09:00
    home-driver-scrub.{service,timer}            first Sunday of the month 06:00
    home-driver-notify-failure@.service          OnFailure= target of every unit
    fstab.snippet            the two mounts by LABEL (install.sh reads the lines from here)
  scripts/
    lib.sh               shared helpers (sourced): paths, logging, env parser, disk check, lock
    notify.sh            the only way alerts are sent; never fails
    immich-compose.sh    the only way `docker compose` is invoked for Immich
    setup-disks.sh       DESTRUCTIVE: format one disk
    backup-local.sh      dumps -> rsync A to B -> snapshot -> prune
    backup-offsite.sh    restic backup / forget --prune / check; no-op when unconfigured
    scrub.sh  smart-check.sh
    restore.sh           list-snapshots | file | disk-a | disk-b | offsite | immich-db | drive-db | curator-db
    laptop-setup.sh      lid, sleep targets, battery threshold
    install.sh           idempotent installer (run from a repo checkout)
    doctor.sh            read-only health report, exit 1 on any FAIL
  config/*.env.example   one per /etc/home-driver/*.env, every variable commented
  cloudflared/ingress.example.yml   example only; we never edit the user's cloudflared
  docs/INSTALL.md  docs/RUNBOOK.md  for the owner: exact commands, expected output
  tests/run.sh           the test runner (no root, no disks)
  tests/stubs/           fake btrfs, docker, restic, smartctl, systemctl, ss, curl, uv, mkfs.btrfs...
  tests/stubs-disk/      fake lsblk and findmnt describing a made-up machine
```

On the server: scripts live in `/opt/home-driver/scripts`, compose files in
`/opt/home-driver/immich`, docs in `/opt/home-driver/docs`.

## On-disk layout the scripts assume

```
Disk A  label hd-data    subvolume @data   -> /srv/home-driver/data
          .home-driver-disk  (marker, "role=data")
          immich/  drive/{blobs,tmp}/  dumps/  [restored/ = scratch of `restore.sh file`]
Disk B  label hd-backup  subvolume @backup -> /srv/home-driver/backup
          .home-driver-disk  (marker, "role=backup")
          current/              nested subvolume: rsync mirror of Disk A
          snapshots/<YYYY-MM-DD_HHMMSS>/   read-only snapshots of current/
          .sync-not-yet-snapshotted        exists only between rsync and snapshot
SSD     /var/lib/home-driver/{immich-postgres,drive,curator,dumps,status,smart,locks}
```

`status/*.status` and `smart/*.state` are `key=value` files written by the jobs
and read by `doctor.sh`. If you add a job, write a status file and teach
`doctor.sh` to read it.

## Invariants (and why). Do not weaken these.

1. **Marker file before any disk I/O.** Every script that reads or writes a data
   disk calls `hd_check_disk <role> <dir>` first: mounted + btrfs + marker with
   the right role.
   *Why:* an unplugged USB disk leaves an empty mountpoint directory on the SSD.
   `rsync --delete` from it would empty the backup; writing to it fills the SSD.
   The role in the marker also catches swapped disks.
2. **`setup-disks.sh` has no defaults.** `--device`, `--role` and
   `--yes-wipe-this-disk` are all mandatory; it prints the disk identity first
   and refuses the system disk (walks LVM/LUKS with `lsblk -s`), mounted disks,
   non-USB disks, existing home-driver disks and duplicate labels. If it cannot
   work out where the system lives, it refuses.
   *Why:* formatting the wrong disk is the one mistake nothing can undo.
3. **A snapshot of the mirror's current state always exists before rsync may
   delete from it.** Normally last night's snapshot; the
   `.sync-not-yet-snapshotted` marker makes the next run take the missing one
   first. Snapshots, not the mirror, are the protection against propagated
   deletions (that is the reasoning behind using `rsync --delete`; see the long
   comment in `backup-local.sh`).
4. **Pruning only touches names matching `HD_SNAPSHOT_RE`, never the newest
   snapshot, and keeps *every* snapshot of the newest N days** (so a second run
   on the same day cannot evict the first). Unparseable names are kept.
5. **Brakes in `backup-local.sh`:** refuse when Disk A has no files but the
   mirror does (fresh replacement disk), and refuse when the rehearsal rsync
   would delete more than `BACKUP_MAX_DELETE_PERCENT` (override:
   `--allow-mass-delete`).
6. **Dumps before files.** The database dump is taken before the rsync, so the
   copied files are a superset of what the dump references. A failed dump is
   reported (exit 1, alert) but does not stop the file backup.
7. **Restores never overwrite and never delete.** `file` uses
   `rsync --ignore-existing`; `disk-a` refuses a non-empty target; `offsite`
   needs an empty folder; the `*-db` commands move the old database aside.
   Every subcommand prints a plan, honours `--dry-run`, and needs a typed "yes"
   or `--yes`. Restores never write to Disk B. `disk-a` holds the backup lock so
   a nightly run cannot mirror a half-restored disk.
8. **`notify.sh` always exits 0.** A broken notifier must not break a backup.
   Alerts come from two layers: the script itself (`hd_fail`, with a useful
   sentence) and systemd (`OnFailure=home-driver-notify-failure@%n.service`, for
   crashes, timeouts, missing files). Two messages for one failure is accepted.
9. **Services cannot start without Disk A:** `RequiresMountsFor=` plus
   `AssertPathExists=` on the marker; and the Immich upload bind mount uses
   `create_host_path: false`, so Docker itself refuses when the folder is absent
   (this also covers containers Docker would restart on its own at boot).
10. **Nothing public.** Immich publishes on `127.0.0.1:2283`. The only exception
    is `IMMICH_LAN_ADDR`, which `immich-compose.sh` restricts to RFC1918
    addresses. `doctor.sh` fails on any other listener.
11. **`install.sh` never overwrites an existing `/etc/home-driver/*.env`**, never
    formats, never touches cloudflared, and only enables units with `--enable`.
12. **Env files are parsed, not sourced** (`hd_load_env`), and may not set
    `HD_*` path variables or `PATH`. A typo in a config file must not run code.
13. **Every script is safe to run twice** and supports `--help`; anything that
    changes state supports `--dry-run`, which must change nothing at all (the
    tests fingerprint the whole sandbox before and after).

## Testing without disks or root

```
deploy/tests/run.sh            # everything
deploy/tests/run.sh restore    # only behavioural tests whose section matches
deploy/tests/run.sh -v         # full output of failures
```

How it works:

- Scripts take their paths from `HD_DATA_DIR`, `HD_BACKUP_DIR`, `HD_STATE_DIR`,
  `HD_ETC_DIR`, `HD_OPT_DIR` (or `HD_ROOT` as a prefix for all of them; with
  `HD_ROOT` set, `install.sh` and `laptop-setup.sh` do a *staged* install and
  skip apt, useradd, chown and systemctl).
- `HD_TEST_MODE=1` skips the root requirement. A plain temp directory is only
  accepted as a "disk" when `HD_TEST_ALLOW_NON_MOUNTPOINT=1` is also set, which
  lets the tests prove the *real* mountpoint refusal by leaving it unset.
  `HD_TEST_FAKE_BLOCKDEV=1` lets `setup-disks.sh` accept the fake `/dev/fake*`
  names of the lsblk stub. None of these can be set from an env file.
- External tools are stubs on `PATH` that append their command line to
  `$STUB_LOG`. The `btrfs` stub implements snapshots as `cp -a`, so restore
  tests work on real file contents. `STUB_*` variables steer failure cases.
- Static checks: `bash -n`, shellcheck (binary, else `uvx --from shellcheck-py`,
  else the `koalaman/shellcheck` image; the runner says which, or that none was
  found), `systemd-analyze verify`, `docker compose config`, and a check that
  every `BACKUP_*`/`NOTIFY_*`/`RESTIC_*` variable the scripts read is documented
  in a config example.

**Every bug fix gets a test.** Every new refusal gets a test that proves it
refuses *and* that nothing was changed or called (`fingerprint`, `stub_called`).

What the tests cannot prove (no btrfs, no real disks, no root): real
`mkfs.btrfs` / `btrfs subvolume` / `btrfs scrub` output formats, real
`smartctl` output from a USB bridge, the systemd behaviour when a disk
disappears, and the Immich database restore. When you get access to a real
machine, a loop-device run of setup-disks -> backup-local -> restore is the
most valuable test you can add (it needs root, so keep it out of `run.sh`).

## Validating units

`systemd-analyze verify` needs `ExecStart=` binaries to exist, and
`/opt/home-driver` does not on a dev box. `run.sh` therefore verifies a copy of
each unit with that prefix rewritten to a directory of empty executables, and
ignores messages about the host's own units. To do it by hand:

```
systemd-analyze verify deploy/systemd/home-driver-drive.service
# expected on a dev box, and harmless: "Command /opt/home-driver/bin/drive is not executable: No such file or directory"
systemd-analyze calendar 'Sun *-*-01..07 06:00:00'      # check a timer expression
systemd-analyze security home-driver-drive.service      # on the server: hardening score
```

Mount unit names contain an escaped dash: `/srv/home-driver/data` is
`srv-home\x2ddriver-data.mount` (`systemd-escape -p --suffix=mount <path>`).

## Common pitfalls

- **rsync from an unmounted mountpoint** looks like "everything was deleted".
  Never call rsync on Disk A or B without `hd_check_disk` first. Mind trailing
  slashes: `"$A/" "$B/current/"`.
- **btrfs snapshots** are per subvolume and not recursive: a nested subvolume
  shows up as an empty directory in a snapshot. Keep `current/` free of nested
  subvolumes. `btrfs subvolume delete` (not `rm -rf`) removes a read-only
  snapshot. Snapshots share blocks, so deleting one frees only what is unique.
  Single-disk btrfs *detects* data corruption (scrub) but cannot repair it:
  repair means copying the file from the other disk.
- **`RequiresMountsFor=` on a oneshot job hides failures:** if the disk is
  missing the job is skipped with "dependency failed" and `OnFailure=` never
  fires. That is why the backup/scrub/SMART units only use `After=` on the mount
  units and let the script detect and report the missing disk.
- **USB SMART passthrough** depends on the enclosure's bridge chip. Plain
  `smartctl` often fails where `-d sat` works; some bridges pass nothing.
  `smartctl`'s exit status is a bit mask: only bits 0-1 mean "could not read".
- **Docker restarts containers by itself** (`restart: always`) at daemon start,
  before systemd has checked any mount. Hence `create_host_path: false`, and
  `ExecStop=... down` so no containers exist while the unit is stopped. Docker
  also bypasses ufw for published ports: the bind address is the only firewall.
- **ERR traps and `$(...)`**: failure reporting uses an EXIT trap
  (`hd_alert_on_unexpected_exit`), not an ERR trap. `hd_die`/`hd_fail` inside a
  command substitution only exit the subshell: call them from the main shell
  (`x=$(...) || hd_fail ...`).
- **SQLite and root:** opening a WAL database as root can leave root-owned
  `-wal`/`-shm` files that lock the service out. Use `hd_as_service_user`.
- **Env file syntax** must stay within what systemd `EnvironmentFile=`, docker
  compose and `hd_load_env` all accept: `KEY=value`, optional quotes, no inline
  comments, no `export`-only tricks.
- **Dry-run purity:** do not create lock files, status files or directories
  before checking `HD_DRY_RUN`. Use `hd_run` for commands that change state.

## Upgrading the pinned Immich version

The compose file is derived from Immich's release asset, never from memory.

1. Read the release notes between the versions.
2. Download `https://github.com/immich-app/immich/releases/download/<new>/docker-compose.yml`
   and `example.env`; diff them against the previous release's files.
3. Re-apply the five changes marked `home-driver:` at the top of
   `immich/docker-compose.yml`; update the `SOURCE:` line.
4. Bump `IMMICH_VERSION` in `config/immich.env.example` and the version examples
   in `docs/`.
5. Check the backup/restore page of that version's docs
   (`docs/docs/administration/backup-and-restore.md` in the Immich repo at that
   tag): the `pg_dump` command in `backup-local.sh` and the `psql` command in
   `restore.sh immich-db` must match it.
6. `deploy/tests/run.sh`, then tell the curator maintainer (root `CLAUDE.md`:
   the curator's API calls must be re-checked against the new OpenAPI spec).

Currently pinned: **v3.4.2**.

## Checklist: adding a script

- [ ] `#!/usr/bin/env bash`, `set -euo pipefail`, `source lib.sh`
- [ ] `--help` (usage in plain language: what, when, exit status), unknown
      arguments rejected
- [ ] `--dry-run` if it changes anything; nothing is written in dry-run
- [ ] paths only through `HD_*` variables; no hard-coded `/srv`, `/var/lib`, `/etc`
- [ ] `hd_check_disk` before touching a data disk; `hd_lock` if two runs must not overlap
- [ ] failures go through `hd_fail` (alert) with a sentence the owner can act on;
      `hd_alert_on_unexpected_exit` for unattended jobs
- [ ] safe to run twice
- [ ] new settings: `BACKUP_*`/`NOTIFY_*` prefix, documented in `config/*.env.example`
- [ ] behavioural tests in `tests/run.sh` (success, each refusal, dry-run purity)
- [ ] shellcheck clean at `-S info`
- [ ] `docs/RUNBOOK.md` entry for every new alert text

## Checklist: adding a unit

- [ ] name starts with `home-driver-`; timers have `Persistent=true`
- [ ] `OnFailure=home-driver-notify-failure@%n.service`
- [ ] long-running service that needs Disk A: `RequiresMountsFor=` + `AssertPathExists=` marker;
      unattended job: `After=` the mount units only (see pitfalls)
- [ ] not root unless it must be (`User=home-driver`), hardening block copied
      from `home-driver-drive.service`, explicit `ReadWritePaths=`
- [ ] binds `127.0.0.1` only; add the port to `doctor.sh` and the root contract
- [ ] `install.sh` enables it under `--enable`; `doctor.sh` checks it
- [ ] `deploy/tests/run.sh` passes (it verifies every unit and the contract greps)
- [ ] root `CLAUDE.md` "systemd units" list updated in the same change

## Dependencies (keep them few and boring)

`btrfs-progs` (snapshots, scrub), `rsync` (mirror), `smartmontools` (disk
health), `restic` (encrypted offsite, optional), `sqlite3` (online DB backup),
`curl` (alerts), `util-linux` (`flock`, `lsblk`, `findmnt`, `mountpoint`,
`runuser`; always present), Docker with the compose plugin (Immich). No `jq`,
no Python in the scripts. Justify any addition here.
