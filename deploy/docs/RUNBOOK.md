# home-driver runbook: what to do when...

Keep calm. Your data is on two disks (and offsite, if you set that up). Almost
every problem here is recoverable **as long as you do not format or "repair"
anything in a hurry**.

Three rules:

1. **Always start with the doctor.** It changes nothing and tells you what is wrong:

   ```
   $ sudo /opt/home-driver/scripts/doctor.sh
   ```

2. **Never run `setup-disks.sh` on a disk that has ever held your data** unless
   this runbook tells you to. It erases the disk.
3. Every `restore.sh` command accepts `--dry-run`, which only prints the plan.
   Use it first.

If you ask a person or an AI assistant for help, give them the full output of
`doctor.sh` and of the `journalctl` command mentioned in the alert.

Contents: [An alert arrives](#an-alert-arrives) ·
[A file was deleted by mistake](#a-file-was-deleted-by-mistake) ·
[Disk A dies](#disk-a-dies-the-data-disk) ·
[Disk B dies](#disk-b-dies-the-backup-disk) ·
[Both disks die](#both-disks-die) ·
[The laptop dies](#the-laptop-dies) ·
[Upgrading Immich](#upgrading-immich-safely) ·
[Routine](#routine-maintenance)

---

## An alert arrives

Alerts look like `[laptop-name] <something> FAILED` followed by a sentence.
Run `doctor.sh`, then find your alert below.

### "Disk A/B ... is not a mounted disk" or "... is MISSING" or "plugged in but NOT MOUNTED"

The disk dropped off the USB bus, lost power, or is dead.

```
$ lsblk -o NAME,SIZE,MODEL,LABEL,MOUNTPOINT
```

- **The disk is listed** with its label (`hd-data` or `hd-backup`) but has no
  mountpoint: mount it and restart what depends on it.

  ```
  $ sudo mount /srv/home-driver/data        # or /srv/home-driver/backup
  $ sudo systemctl start home-driver-immich home-driver-drive home-driver-curator
  $ sudo /opt/home-driver/scripts/doctor.sh
  ```

- **The disk is not listed**: check the disk's power supply and USB cable, try
  another USB port, listen for it spinning. Then look again with `lsblk`. If it
  comes back, mount it as above. A reboot is also fine: everything starts by
  itself when both disks are present.
- **Still not listed, or it clicks / does not spin**: the disk is dead. Go to
  [Disk A dies](#disk-a-dies-the-data-disk) or [Disk B dies](#disk-b-dies-the-backup-disk).

If this happens repeatedly with a disk that turns out fine, the enclosure, cable
or power supply is failing. Replace those (cheap) before the disk suffers.

While Disk A is missing, Immich and Drive are stopped on purpose, so nothing is
written to the wrong place. Nothing is lost.

### "SMART health ..." / "sectors grew from X to Y" / "FAILING"

The disk itself reports that it is wearing out. It may work for months or die
tomorrow.

1. Make sure the *other* disk is healthy: `sudo /opt/home-driver/scripts/doctor.sh`
2. If the failing disk is **Disk A**, run a backup right now while it still reads:
   `sudo /opt/home-driver/scripts/backup-local.sh`
3. Buy a replacement of at least the same size, then follow
   [Disk A dies](#disk-a-dies-the-data-disk) or
   [Disk B dies](#disk-b-dies-the-backup-disk) (the steps are the same whether
   the disk is already dead or merely dying; unplug the old one first).

Details: `sudo smartctl -a /dev/sdX` (find `sdX` with `lsblk`; add `-d sat` if it
prints "Unknown USB bridge").

### "Cannot read SMART data ..."

Not an emergency: the USB enclosure hides the disk's health data. The monthly
scrub still detects damage. Try `sudo smartctl --scan-open`, and if a `-d <type>`
is suggested there, put that type in `BACKUP_SMART_DEVICE_TYPE` in
`/etc/home-driver/backup.env`.

### "scrub ..." errors (checksum, corruption, uncorrectable)

The monthly scrub read every block and found some that are damaged ("bit rot" or
a failing disk). The other disk still has good copies.

1. Which files?

   ```
   $ sudo journalctl -k --since "3 days ago" | grep -i -E 'checksum error|csum failed' | tail -n 20
   ... BTRFS warning (device sdb): checksum error at logical ... path: immich/upload/.../abcd.jpg
   ```

2. Damaged file **on Disk B** (the backup): nothing to restore. If it is only a
   few files, delete them from the mirror and let the next backup copy them again:

   ```
   $ sudo rm /srv/home-driver/backup/current/<path from the log>
   $ sudo /opt/home-driver/scripts/backup-local.sh
   ```

   (Old snapshots keep pointing at the damaged block; that only matters for that
   one file in those snapshots.)
3. Damaged file **on Disk A**: get the good copy back from the backup.

   ```
   $ sudo mv /srv/home-driver/data/<path> /srv/home-driver/data/<path>.damaged
   $ sudo /opt/home-driver/scripts/restore.sh file <path> --in-place
   ```

   Open the file to check it, then delete the `.damaged` one.
4. Run `sudo /opt/home-driver/scripts/smart-check.sh`. Many errors, or errors
   again next month, mean the disk is dying: replace it.
5. After fixing, reset the btrfs error counters so the next scrub starts clean:
   `sudo btrfs device stats --reset /srv/home-driver/data` (or `.../backup`).

### "backup-local.sh FAILED: The sync would delete N of about M files ..."

The safety brake: tonight's backup would have removed more than 20 % of the
files from the backup copy. Nothing was deleted. Think before overriding:

- Did you (or the curator, or Immich's trash being emptied) really delete that
  much? Did you change Immich's "storage template" (which renames every file)?
  Then it is expected:

  ```
  $ sudo /opt/home-driver/scripts/backup-local.sh --allow-mass-delete
  ```

- If you did **not**, something is wrong on Disk A. Do not override. Look at
  Disk A (`ls /srv/home-driver/data/immich`), run `doctor.sh`, and get help. The
  backup copy and all snapshots are intact meanwhile.

### "Disk A contains no files but the backup does"

Disk A is mounted and is a home-driver disk, but empty: typically a brand-new
replacement disk. The backup refused to mirror "nothing" over your backup.
Restore first: [Disk A dies](#disk-a-dies-the-data-disk), step 4.

### "... database dump failed" / "Files were copied ... but with errors"

Your photos and files **were** backed up; a database dump was not.

```
$ sudo journalctl -u home-driver-backup-local -n 60 --no-pager
$ sudo /opt/home-driver/scripts/immich-compose.sh ps
```

Usually Immich or Drive is not running: `sudo systemctl start home-driver-immich
home-driver-drive home-driver-curator`, then run `backup-local.sh` again. If you
deliberately do not run one of the components, set its `BACKUP_*_DB=0` in
`/etc/home-driver/backup.env`.

### "backup-offsite.sh FAILED ..." / "Cannot open the restic repository"

Usually the internet connection or expired storage credentials. The local
backup is unaffected.

```
$ sudo journalctl -u home-driver-backup-offsite -n 60 --no-pager
$ sudo /opt/home-driver/scripts/backup-offsite.sh          # try again by hand
```

"repository is already locked" after a power cut: make sure no backup is running
(`systemctl is-active home-driver-backup-offsite`), then

```
$ sudo sh -c 'set -a; . /etc/home-driver/backup.env; restic unlock'
```

"restic check reported problems": do not delete anything; follow
<https://restic.readthedocs.io/en/stable/077_troubleshooting.html> or get help.
Your two local disks are still your primary copies.

### "home-driver-<name>.service FAILED" (generic)

```
$ sudo journalctl -u home-driver-<name> -n 60 --no-pager
$ sudo /opt/home-driver/scripts/doctor.sh
```

For `immich`, `drive`, `curator`: `sudo systemctl restart home-driver-<name>`.
If Drive or the curator refuse to start right after you edited a settings file,
the edit is the cause: compare with the matching file in `deploy/config/`.

### No alert for a long time?

Silence should mean health, but test the alarm now and then:
`sudo /opt/home-driver/scripts/notify.sh "test" "still alive"`.

---

## A file was deleted by mistake

Every night's snapshot on Disk B is a complete, read-only picture of Disk A.
With the default settings you can go back day by day for 14 days, week by week
for 8 weeks and month by month for a year.

**First, the easy places:** Immich has a trash (**Trash** in the sidebar, 30
days). Drive has a trash too. Restore there if you can; that also restores the
album and sharing information.

**Otherwise, from a snapshot:**

```
$ sudo /opt/home-driver/scripts/restore.sh list-snapshots
$ sudo ls /srv/home-driver/backup/snapshots/2026-01-14_023000/immich/library/     # browse
```

Restore a file or a whole folder. The path is relative to `/srv/home-driver/data`:

```
$ sudo /opt/home-driver/scripts/restore.sh file immich/library/anna/2025/2025-08-03 --dry-run
$ sudo /opt/home-driver/scripts/restore.sh file immich/library/anna/2025/2025-08-03
```

Without `--from <snapshot>` it uses the newest snapshot that still contains the
path. The copy lands in `/srv/home-driver/data/restored/<snapshot>/...`; nothing
existing is touched.

- **A photo:** upload the restored file to Immich again (web: **Upload**). Simply
  putting a file back into Immich's folders does not make Immich show it.
- **A Drive file:** download/copy the restored file and upload it to Drive again.
  If *many* Drive files or their folder structure are gone, restore the Drive
  database from the same night instead (this rewinds all of Drive to that night):

  ```
  $ sudo ls /srv/home-driver/backup/snapshots/2026-01-14_023000/dumps/
  $ sudo /opt/home-driver/scripts/restore.sh drive-db /srv/home-driver/backup/snapshots/2026-01-14_023000/dumps/drive-2026-01-14_023000.db
  $ sudo /opt/home-driver/scripts/restore.sh file drive/blobs --from 2026-01-14_023000 --in-place
  $ sudo systemctl start home-driver-drive
  ```

When done: `sudo rm -r /srv/home-driver/data/restored`.

---

## Disk A dies (the data disk)

You lose nothing older than last night's backup. Immich and Drive are down until
the new disk is filled.

1. **Unplug the dead Disk A** and label it "DEAD". Do not touch Disk B.
2. Buy a disk at least as large. Plug it in. Identify it:

   ```
   $ lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN,LABEL
   ```

   It is the USB disk **without** a label. (`hd-backup` is your backup: never
   format that one. The script refuses it anyway.)
3. Prepare and mount the new disk (from the repository folder, e.g. `~/home-driver`):

   ```
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdX --role data --dry-run
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdX --role data --yes-wipe-this-disk
   $ sudo mount /srv/home-driver/data
   ```

4. Copy everything back from the newest snapshot. This takes hours:

   ```
   $ sudo /opt/home-driver/scripts/restore.sh disk-a --dry-run
   $ sudo /opt/home-driver/scripts/restore.sh disk-a
   ```

   If it is interrupted (power cut), run it again with `--allow-non-empty`; it
   continues.
5. Make the databases match the restored files. The databases on the SSD are
   *newer* than the restored files (they know about photos uploaded after the
   last backup, which are gone). Load the dumps taken with that backup:

   ```
   $ sudo ls /srv/home-driver/data/dumps/
   $ sudo /opt/home-driver/scripts/restore.sh immich-db /srv/home-driver/data/dumps/immich-<newest>.sql.gz
   $ sudo /opt/home-driver/scripts/restore.sh drive-db  /srv/home-driver/data/dumps/drive-<newest>.db
   ```

6. Recreate folder ownership and start:

   ```
   $ sudo deploy/scripts/install.sh
   $ sudo systemctl start home-driver-immich home-driver-drive home-driver-curator
   $ sudo /opt/home-driver/scripts/doctor.sh
   ```

7. Photos taken between the last backup and the crash are still on the phones:
   open the Immich app on each phone and let the backup run; it uploads what
   the server no longer has.
8. Run a backup when everything looks right: `sudo /opt/home-driver/scripts/backup-local.sh`

---

## Disk B dies (the backup disk)

Your data is fine and the server keeps running, but you have **no local backup**
until this is fixed. Do it this week. You will get nightly alerts until then.

1. **Unplug the dead Disk B** and label it "DEAD".
2. Buy a disk at least as large as Disk A. Plug it in and identify it
   (`lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN,LABEL`): the USB disk **without** a
   label. `hd-data` is your live data: never format that one.
3. ```
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdY --role backup --dry-run
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdY --role backup --yes-wipe-this-disk
   $ sudo mount /srv/home-driver/backup
   ```
4. Fill it (hours; the server stays usable):

   ```
   $ sudo /opt/home-driver/scripts/restore.sh disk-b
   $ sudo /opt/home-driver/scripts/doctor.sh
   ```

The snapshot history starts again from today; the old history died with the old disk.

---

## Both disks die

Fire, theft, lightning. This is what the offsite backup is for. Without one,
the only copies left are on the phones (and the dead disks: a data-recovery
company can sometimes read them; do not power them on again).

With the offsite backup:

1. Buy two disks. Prepare **both** as in [INSTALL.md](INSTALL.md) step 2 and
   mount them:

   ```
   $ sudo mount /srv/home-driver/data && sudo mount /srv/home-driver/backup
   ```

2. `/etc/home-driver/backup.env` must contain the repository address, the
   `RESTIC_PASSWORD` (from your paper note) and the storage credentials. If the
   laptop survived, it still does.
3. Download (can take days; it is safe to interrupt and repeat into a fresh
   empty folder):

   ```
   $ sudo /opt/home-driver/scripts/restore.sh offsite /srv/home-driver/data/offsite-restore --dry-run
   $ sudo /opt/home-driver/scripts/restore.sh offsite /srv/home-driver/data/offsite-restore
   ```

4. Move the restored folders into place (instant, same disk):

   ```
   $ sudo sh -c 'mv /srv/home-driver/data/offsite-restore/srv/home-driver/data/* /srv/home-driver/data/'
   $ sudo mkdir -p /srv/home-driver/data/dumps
   $ sudo sh -c 'cp -a /srv/home-driver/data/offsite-restore/var/lib/home-driver/dumps/. /srv/home-driver/data/dumps/'
   $ sudo rm -r /srv/home-driver/data/offsite-restore
   ```

5. Continue with [Disk A dies](#disk-a-dies-the-data-disk) from **step 5**
   (databases, start, phones).
6. Thumbnails and transcoded videos are not in the offsite backup by default.
   In Immich: **Administration > Job Queues**, run **Generate Thumbnails > All**
   and **Transcode Videos > All**. If Immich refuses to start complaining about a
   missing `.immich` file in a folder, see
   <https://docs.immich.app/administration/system-integrity> for the fix.
7. Seed the new Disk B: `sudo /opt/home-driver/scripts/restore.sh disk-b`

---

## The laptop dies

The disks hold everything needed: your data, the database dumps and a copy of
the settings files.

1. Get another computer, install Ubuntu, Docker and your `cloudflared` tunnel
   (same tunnel credentials, or create a new tunnel and move the two hostnames).
2. Get the repository and build it (see the top of [INSTALL.md](INSTALL.md)).
3. **Do NOT run `setup-disks.sh`.** The disks are fine. Then:

   ```
   $ sudo deploy/scripts/laptop-setup.sh
   $ sudo deploy/scripts/install.sh
   $ sudo systemctl daemon-reload
   $ sudo mount /srv/home-driver/data && sudo mount /srv/home-driver/backup
   ```

4. Put your settings back (they were saved with every backup), **before**
   starting anything:

   ```
   $ sudo sh -c 'cp /srv/home-driver/data/dumps/config/*.env /etc/home-driver/ && chmod 600 /etc/home-driver/*.env && chown root:root /etc/home-driver/*.env'
   $ sudo deploy/scripts/install.sh
   ```

5. Restore the databases from the newest dumps on Disk A:

   ```
   $ sudo ls /srv/home-driver/data/dumps/
   $ sudo /opt/home-driver/scripts/restore.sh immich-db  /srv/home-driver/data/dumps/immich-<newest>.sql.gz
   $ sudo /opt/home-driver/scripts/restore.sh drive-db   /srv/home-driver/data/dumps/drive-<newest>.db
   $ sudo /opt/home-driver/scripts/restore.sh curator-db /srv/home-driver/data/dumps/curator-<newest>.db
   ```

6. The `home-driver` user on the new computer may have a different numeric id
   than on the old one. Hand Drive's files to it, then start everything:

   ```
   $ sudo chown -R home-driver:home-driver /srv/home-driver/data/drive /var/lib/home-driver/drive /var/lib/home-driver/curator
   $ sudo deploy/scripts/install.sh --enable
   $ sudo /opt/home-driver/scripts/doctor.sh
   ```

Photos uploaded after the last nightly dump exist as files on Disk A but are
unknown to the restored database; the phones upload them again on their next
backup run.

---

## Upgrading Immich safely

Immich is pinned to an exact version (`IMMICH_VERSION` in
`/etc/home-driver/immich.env`). Nothing upgrades by itself. Upgrade when you want
a feature or a security fix, on a day when you have time, never before a trip.

1. **Read the release notes** of *every* version between yours and the target:
   <https://github.com/immich-app/immich/releases>. Look for "breaking change"
   and for instructions to change the compose file. Do not skip major versions
   if the notes say not to.
2. **Back up first**, and make sure it succeeded:

   ```
   $ sudo /opt/home-driver/scripts/backup-local.sh
   ... backup OK: snapshot 2026-02-01_101500
   ```

3. **Compare the official compose files** of the old and new version (replace
   the versions):

   ```
   $ curl -fsSL -o /tmp/old.yml https://github.com/immich-app/immich/releases/download/v3.4.2/docker-compose.yml
   $ curl -fsSL -o /tmp/new.yml https://github.com/immich-app/immich/releases/download/v3.5.0/docker-compose.yml
   $ diff /tmp/old.yml /tmp/new.yml && echo "NO CHANGES"
   ```

   - `NO CHANGES`: continue with step 4.
   - Differences: the file `deploy/immich/docker-compose.yml` in the repository
     must be updated first: start from the new official file and re-apply the
     five "home-driver:" changes listed at its top (a contributor or an AI
     assistant can do this; `deploy/CLAUDE.md` describes it). Then run
     `deploy/tests/run.sh` and `sudo deploy/scripts/install.sh`.
4. **Change the version** and restart:

   ```
   $ sudo nano /etc/home-driver/immich.env          # IMMICH_VERSION=v3.5.0
   $ sudo /opt/home-driver/scripts/immich-compose.sh pull
   $ sudo systemctl restart home-driver-immich
   $ sudo /opt/home-driver/scripts/immich-compose.sh ps
   $ sudo /opt/home-driver/scripts/immich-compose.sh logs --tail 50 immich-server
   ```

   The first start after an upgrade can take several minutes (database migration).
5. **Check**: open the website, scroll the timeline, open a photo and a video,
   check the phone app still connects (update the app from the store too), run
   `sudo /opt/home-driver/scripts/doctor.sh`.
6. **Curator**: the curator talks to Immich's API, which changes between
   releases. Open `https://drive.example.com/curator/` and start a scan
   (`sudo systemctl start home-driver-curator-scan`). If it fails, the curator
   needs an update for this Immich version (see `curator/CLAUDE.md`); Immich
   itself is unaffected.

**If the upgrade went wrong:** an upgraded database cannot simply be run by the
older version. Go back completely:

```
$ sudo nano /etc/home-driver/immich.env          # put the OLD IMMICH_VERSION back
$ sudo /opt/home-driver/scripts/restore.sh immich-db /var/lib/home-driver/dumps/immich-<dump from step 2>.sql.gz
$ sudo systemctl start home-driver-immich
```

Photos uploaded between step 2 and the rollback are re-uploaded by the phones.

---

## Routine maintenance

| How often | What |
|---|---|
| When an alert arrives | This runbook |
| Monthly | `sudo /opt/home-driver/scripts/doctor.sh`; look at the WARN lines |
| Every 3 months | Restore test: INSTALL.md step 11, parts 1-3 |
| Every 6 months | `sudo apt update && sudo apt upgrade`, then reboot; check `doctor.sh` afterwards |
| Yearly | Is `RESTIC_PASSWORD` still on that piece of paper? Are the disks older than 5 years? Consider replacing Disk A proactively (the old one can become a spare) |

Useful commands:

```
$ systemctl list-timers 'home-driver-*'                  # when does each job run next
$ journalctl -u home-driver-backup-local -n 80 --no-pager # what did the last backup say
$ sudo /opt/home-driver/scripts/restore.sh list-snapshots
$ sudo /opt/home-driver/scripts/immich-compose.sh ps
$ sudo btrfs filesystem usage /srv/home-driver/backup     # how full is Disk B really
```

Disk B fills up faster than Disk A because it keeps history. If it gets full,
lower `BACKUP_KEEP_MONTHLY` / `BACKUP_KEEP_WEEKLY` in `/etc/home-driver/backup.env`;
the next backup removes the extra snapshots.
