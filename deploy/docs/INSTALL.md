# Installing home-driver: first-time setup

This guide takes you from a bare laptop to a working photo and file server with
nightly backups. Do the steps **in order**. Budget an afternoon; most of the
time is waiting.

Conventions:

- Lines starting with `$` are commands to type in a terminal on the laptop
  (without the `$`). `sudo` asks for your password.
- `example.com` stands for your own domain everywhere.
- "Disk A" is the disk that holds your data. "Disk B" is the backup disk.
- If anything looks different from what this guide says, **stop** and run
  `sudo /opt/home-driver/scripts/doctor.sh` (once installed). Do not improvise
  with disks.

What you need before starting:

- The laptop, with Ubuntu 24.04 or newer installed on its internal SSD, plugged
  into power and into the router (cable is better than Wi-Fi). (22.04 also
  works for everything except the optional offsite backup, whose `restic`
  package is too old there; update it with `sudo restic self-update`.)
- Docker with the compose plugin: <https://docs.docker.com/engine/install/ubuntu/>
  Check: `docker compose version` prints a version.
- Your existing, working Cloudflare Tunnel (`cloudflared`) on the laptop.
- Two USB hard disks with their own power supplies. **Both will be erased.**
- A Google account (for "Sign in with Google").
- This repository on the laptop, with the Drive program built:

  ```
  $ git clone <repository-url> ~/home-driver
  $ cd ~/home-driver
  $ make build
  ```

  `make build` needs Go and `uv` (see `drive/CLAUDE.md` and `curator/CLAUDE.md`).
  Expected: a file `drive/bin/drive` exists afterwards (`ls -l drive/bin/drive`).

All commands below are run from `~/home-driver`.

---

## Step 1 - Make the laptop behave like a server

```
$ sudo deploy/scripts/laptop-setup.sh --dry-run     # shows what it would do
$ sudo deploy/scripts/laptop-setup.sh
```

Expected output ends with something like:

```
Changed:
  - wrote /etc/systemd/logind.conf.d/50-home-driver.conf
  - masked sleep.target (the laptop can no longer suspend or hibernate)
  ...
  - battery charge limit 100 -> 80 % (/sys/class/power_supply/BAT0/charge_control_end_threshold)
```

If it says the battery limit is "not supported here", look for a "battery care"
or "conservation mode" option in the laptop's BIOS/UEFI setup instead.

Then **reboot**, close the lid, wait a minute, and check from your phone or
another computer that the laptop still answers (for example `ping <laptop-ip>`).

---

## Step 2 - Prepare the two disks

> This step erases disks. The script refuses the laptop's own disk, mounted
> disks, non-USB disks and disks that already belong to home-driver, but it
> cannot know which of two empty USB disks you mean. **Plug in only ONE new disk
> at a time.**

### Disk A (data)

1. Plug in **only** the disk that will be Disk A. Put a sticker on it: "A - DATA".
2. Find its name:

   ```
   $ lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN
   NAME          SIZE MODEL                 SERIAL          TRAN
   sda           3.6T WDC WD40EZAZ-00SF3B0  WD-WX12345678   usb
   nvme0n1     476.9G SAMSUNG MZVL2512      S1234567890     nvme
   ```

   The USB disk here is `sda` (TRAN = `usb`, the size matches). The device path
   is `/dev/sda`. Yours may be `sdb` or `sdc`.
3. Rehearse, then format. Replace `/dev/sdX`:

   ```
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdX --role data --dry-run
   ```

   Read the "DISK TO BE ERASED" box: model, size and serial must be the disk in
   front of you. Then:

   ```
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdX --role data --yes-wipe-this-disk
   ```

   It asks you to type the device path once more. Expected last lines:

   ```
   ... [setup-disks.sh] done: /dev/sdX is now the home-driver data disk (label hd-data)
   ```

### Disk B (backup)

4. Plug in the second disk (Disk A may stay plugged in; it is now protected by
   its label). Sticker: "B - BACKUP".
5. Run `lsblk -o NAME,SIZE,MODEL,SERIAL,TRAN,LABEL` again. The new disk is the
   one **without** the label `hd-data`.
6. ```
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdY --role backup --dry-run
   $ sudo deploy/scripts/setup-disks.sh --device /dev/sdY --role backup --yes-wipe-this-disk
   ```

Check both:

```
$ lsblk -o NAME,SIZE,FSTYPE,LABEL
NAME      SIZE FSTYPE LABEL
sda       3.6T btrfs  hd-data
sdb       3.6T btrfs  hd-backup
```

From now on the disks are found by these labels, whatever USB port they are in.

---

## Step 3 - Install

```
$ sudo deploy/scripts/install.sh --dry-run     # prints every action, changes nothing
$ sudo deploy/scripts/install.sh
```

This installs packages, creates the `home-driver` user and folders, copies the
programs to `/opt/home-driver`, creates the settings files in
`/etc/home-driver/`, adds the two disks to `/etc/fstab` and installs the
systemd units. It does not start anything yet.

Now mount the disks and run the installer once more so it can create the folders
on Disk A:

```
$ sudo systemctl daemon-reload
$ sudo mount /srv/home-driver/data
$ sudo mount /srv/home-driver/backup
$ sudo deploy/scripts/install.sh
```

Check:

```
$ findmnt /srv/home-driver/data /srv/home-driver/backup
TARGET                  SOURCE            FSTYPE OPTIONS
/srv/home-driver/data   /dev/sda[/@data]  btrfs  rw,noatime,...
/srv/home-driver/backup /dev/sdb[/@backup] btrfs rw,noatime,...
```

`install.sh` is safe to run again at any time (for example after `git pull`). It
never overwrites a settings file you have edited.

---

## Step 4 - Settings files

The settings live in `/etc/home-driver/`. Edit them with
`sudo nano /etc/home-driver/<name>.env` (save: Ctrl+O, Enter; quit: Ctrl+X).
Rules: one `NAME=value` per line, no spaces around `=`, nothing after the value.

### 4a. `common.env` - domain and alerts (do this properly: it is how the server tells you something is wrong)

- `HD_DOMAIN=example.com`
- Choose **Telegram** or **ntfy**; the file explains how to get the values.
  Example for Telegram:

  ```
  NOTIFY_METHOD=telegram
  NOTIFY_TELEGRAM_BOT_TOKEN=123456789:AAExampleExampleExample
  NOTIFY_TELEGRAM_CHAT_ID=987654321
  ```

Test it. A message must arrive on your phone:

```
$ sudo /opt/home-driver/scripts/notify.sh "test" "hello from the server"
```

If nothing arrives, the terminal shows a `WARNING:` line saying why.

### 4b. `immich.env`

Nothing is required. Optionally set `TZ=` to your timezone. Do **not** change
`UPLOAD_LOCATION`, `DB_DATA_LOCATION` or `DB_PASSWORD`.

### 4c. `drive.env`

- `DRIVE_BASE_URL=https://drive.example.com`
- `DRIVE_ADMIN_EMAILS=you@gmail.com` (your Google address; several separated by commas)
- `DRIVE_GOOGLE_CLIENT_ID` / `DRIVE_GOOGLE_CLIENT_SECRET`: filled in during step 5.

### 4d. `curator.env`

`CURATOR_IMMICH_API_KEY` is filled in during step 8 (Immich must be running first).

### 4e. `backup.env`

The defaults are fine. The offsite backup is optional; see "Optional: offsite
backup" at the end.

---

## Step 5 - Google sign-in (OAuth clients)

You create **one Google Cloud project** with **two OAuth clients**: one for
Immich, one for Drive.

1. Open <https://console.cloud.google.com/> and sign in. Create a project
   (top bar > project list > **New project**), name it `home-driver`.
2. Go to **APIs & Services > OAuth consent screen** (newer consoles call it
   **Google Auth Platform**). Choose **External**, fill in an app name
   (`Family server`) and your email, save.
   - While the app is in "Testing" mode only the Google accounts you list under
     **Test users** (or **Audience**) can sign in. Add every family member's
     address there, or press **Publish app** to allow any Google account to
     reach the sign-in page (who actually gets in is still decided by Immich
     and Drive).
3. Go to **APIs & Services > Credentials > Create credentials > OAuth client ID**.

### Client 1: Immich

- Application type: **Web application**. Name: `Immich`.
- **Authorized redirect URIs** - add exactly these three:

  ```
  https://photos.example.com/auth/login
  https://photos.example.com/user-settings
  https://photos.example.com/api/oauth/mobile-redirect
  ```

  (The third one is for the phone app. Google does not accept the app's own
  `app.immich:///oauth-callback` address, so Immich provides this web address
  that forwards to it.)
- Press **Create** and copy the **Client ID** and **Client secret** into a note;
  you paste them into Immich in step 8.

### Client 2: Drive

- **Create credentials > OAuth client ID** again. Type **Web application**. Name: `Drive`.
- **Authorized redirect URIs** - exactly one:

  ```
  https://drive.example.com/auth/callback
  ```

- Create, then put the two values into `/etc/home-driver/drive.env`:

  ```
  DRIVE_GOOGLE_CLIENT_ID=1234567890-abc...apps.googleusercontent.com
  DRIVE_GOOGLE_CLIENT_SECRET=GOCSPX-...
  ```

The addresses must match character for character (https, no trailing slash).
The classic error "redirect_uri_mismatch" always means a typo here.

---

## Step 6 - Cloudflare Tunnel

home-driver never touches your cloudflared configuration. Open
`deploy/cloudflared/ingress.example.yml`, read the notes in it, and add the two
entries to **your** existing tunnel:

| Public hostname | Service |
|---|---|
| `photos.example.com` | `http://127.0.0.1:2283` |
| `drive.example.com` | `http://127.0.0.1:8090` |

Do **not** add anything for port 8091 (the curator is reached through Drive),
and do **not** put Cloudflare Access in front of `photos.example.com` (it
breaks the phone app).

If your tunnel uses a config file:

```
$ sudo nano /etc/cloudflared/config.yml        # add the two entries above the last line
$ sudo cloudflared tunnel ingress validate
OK
```

Do not restart cloudflared yet; that happens at the end of step 7.

---

## Step 7 - Start everything

```
$ sudo deploy/scripts/install.sh --enable
```

The first start downloads Immich (several GB); give it 5-15 minutes. Watch:

```
$ sudo /opt/home-driver/scripts/immich-compose.sh ps
NAME                      STATUS
immich_server             Up 2 minutes (healthy)
immich_machine_learning   Up 2 minutes (healthy)
immich_postgres           Up 2 minutes (healthy)
immich_redis              Up 2 minutes (healthy)
```

**Create the Immich administrator now, before the site is public.** The first
person to register becomes the administrator. On the laptop itself open
<http://127.0.0.1:2283> in a browser, press **Getting started**, and create the
admin account with a real email address and a strong password. (No screen on the
laptop? From your own computer run `ssh -L 2283:127.0.0.1:2283 <user>@<laptop>`
and open the same address there.)

Then publish:

```
$ sudo systemctl restart cloudflared
```

Check from your phone (mobile data, not Wi-Fi): `https://photos.example.com`
shows the Immich login, `https://drive.example.com` shows the Drive login.

---

## Step 8 - Finish the configuration inside the apps

### Immich: Google sign-in

In Immich as administrator: **Administration > Settings > Authentication Settings > OAuth**

| Setting | Value |
|---|---|
| Enabled | on |
| Issuer URL | `https://accounts.google.com` |
| Client ID / Client secret | from step 5, client 1 |
| Scope | `openid email profile` |
| Signing algorithm | `RS256` |
| Button text | `Sign in with Google` |
| Auto register | on if every allowed Google user may get an account automatically; off to create accounts by hand first |
| Mobile redirect URI override | **on** |
| Mobile redirect URI | `https://photos.example.com/api/oauth/mobile-redirect` |

Save. Log out and try **Sign in with Google**. To let a family member in
without auto-register: **Administration > Users > Create user** with their
Google email address; their first Google login then links to that account.

Also set **Administration > Settings > Server Settings > External domain** to
`https://photos.example.com` so share links are correct.

### Curator: API key

In Immich: click your avatar > **Account Settings > API Keys > New API Key**,
name `curator`, copy the key, then:

```
$ sudo nano /etc/home-driver/curator.env      # CURATOR_IMMICH_API_KEY=<the key>
$ sudo systemctl restart home-driver-curator
```

### Drive

Open `https://drive.example.com`, sign in with the Google address you put in
`DRIVE_ADMIN_EMAILS`. The curator is at `https://drive.example.com/curator/`.

### Health check

```
$ sudo /opt/home-driver/scripts/doctor.sh
```

At this point it is expected to report **exactly these** problems, which the
next steps fix: "no successful local backup recorded yet", "no snapshots on
Disk B", and warnings that scrub and SMART have not run yet.

---

## Step 9 - Phones

1. Install the **Immich** app (App Store / Play Store).
2. Server address: `https://photos.example.com`. Sign in (Google button or
   email and password).
3. In the app, open the backup screen (cloud icon), choose the albums to back
   up, switch backup on. Leave the phone on Wi-Fi and charging for the first run.

### Large videos (over about 100 MB)

Cloudflare's free plan refuses uploads larger than about 100 MB per request, and
the Immich app sends each video in one request. So a long video fails to upload
through `photos.example.com`. The fix is to let the phone talk to the laptop
directly whenever it is on your home Wi-Fi:

1. Give the laptop a fixed home address: in your router, add a "DHCP
   reservation" (also called "static lease") for the laptop. Note the address,
   for example `192.168.1.50`. (`hostname -I` on the laptop shows the current one.)
2. ```
   $ sudo nano /etc/home-driver/immich.env      # IMMICH_LAN_ADDR=192.168.1.50
   $ sudo systemctl restart home-driver-immich
   ```
   Only private home addresses are accepted. Immich is then reachable at
   `http://192.168.1.50:2283` **from inside your home network only**.
3. In the Immich app: **Settings > Networking**, switch on **Automatic URL
   switching**, select your home Wi-Fi, and set
   - Local network server URL: `http://192.168.1.50:2283`
   - External network server URL: `https://photos.example.com`

   (Menu names can differ slightly between app versions.)

Big videos now upload at home; everything else keeps working everywhere.
`doctor.sh` shows this extra listener as a `WARN` line, which is expected.

---

## Step 10 - First backup

Upload a few photos from your phone first, then run the backup by hand instead
of waiting for 02:30:

```
$ sudo /opt/home-driver/scripts/backup-local.sh
... [backup-local.sh] preflight OK: A=/srv/home-driver/data B=/srv/home-driver/backup
... [backup-local.sh] Immich database dumped: /var/lib/home-driver/dumps/immich-2026-01-15_103000.sql.gz (1.2M)
... [backup-local.sh] drive database backed up: /var/lib/home-driver/dumps/drive-2026-01-15_103000.db
... [backup-local.sh] curator database backed up: /var/lib/home-driver/dumps/curator-2026-01-15_103000.db
... [backup-local.sh] sync plan: 412 files on Disk A, 412 new, 0 to delete from the backup copy (of about 0)
... [backup-local.sh] mirror complete (rsync exit 0)
... [backup-local.sh] snapshot created: /srv/home-driver/backup/snapshots/2026-01-15_103000
... [backup-local.sh] backup OK: snapshot 2026-01-15_103000
```

Then the disk health check and the full report:

```
$ sudo /opt/home-driver/scripts/smart-check.sh
$ sudo /opt/home-driver/scripts/doctor.sh
...
Summary: 24 OK, 3 warnings, 0 failures
All essential checks passed.
```

Remaining warnings at this point are normal: "never been scrubbed" (the first
scrub runs on the first Sunday of the month; to run it now, which takes hours:
`sudo systemctl start home-driver-scrub.service`) and "offsite backup is not
configured".

From now on, every night: 00:30 curator scan, 02:30 local backup, 04:30
offsite backup (if configured), 09:00 disk health check. If any of them fails
you get an alert.

---

## Step 11 - FIRST RESTORE TEST (do not skip)

A backup you have never restored from is a hope, not a backup. This takes ten
minutes and proves the whole chain.

**1. See the snapshot**

```
$ sudo /opt/home-driver/scripts/restore.sh list-snapshots
Snapshots on Disk B (/srv/home-driver/backup/snapshots), oldest first:
  2026-01-15_103000

1 snapshots. Newest: 2026-01-15_103000
```

**2. Pick one real photo and restore it**

```
$ sudo find /srv/home-driver/data/immich -type f \( -name '*.jpg' -o -name '*.jpeg' -o -name '*.heic' \) | head -n 1
/srv/home-driver/data/immich/upload/3f0c.../ab/cd/abcd1234.jpg
```

Copy that path and use it below (twice):

```
$ sudo /opt/home-driver/scripts/restore.sh file <that path> --dry-run
PLAN: restore from snapshot 2026-01-15_103000
  copy   /srv/home-driver/backup/snapshots/2026-01-15_103000/immich/upload/3f0c.../abcd1234.jpg
  into   /srv/home-driver/data/restored/2026-01-15_103000/immich/upload/3f0c.../ab/cd/
  Existing files are NOT overwritten (rsync --ignore-existing).

$ sudo /opt/home-driver/scripts/restore.sh file <that path>
... Type 'yes' to continue: yes
... [restore.sh] restored to /srv/home-driver/data/restored/2026-01-15_103000/immich/upload/.../abcd1234.jpg
```

**3. Prove the restored copy is identical to the original**

```
$ sudo cmp <that path> <the "restored to" path> && echo IDENTICAL
IDENTICAL
```

Copy the restored file to your own computer and open it: you must see the photo.

**4. Check the database dump is real**

```
$ sudo sh -c 'ls -lh /srv/home-driver/backup/current/dumps/'
$ sudo sh -c 'gzip -t /srv/home-driver/backup/current/dumps/immich-*.sql.gz && echo DUMPS-OK'
DUMPS-OK
```

**5. Clean up**

```
$ sudo rm -r /srv/home-driver/data/restored
```

**6. Test the alarm.** Unplug Disk B's USB cable, then:

```
$ sudo /opt/home-driver/scripts/backup-local.sh
... ERROR: Disk B (backup) is not usable: /srv/home-driver/backup is not a mounted disk ... Backup NOT run.
```

An alert must arrive on your phone. Plug the disk back in, then
`sudo mount /srv/home-driver/backup` and `sudo /opt/home-driver/scripts/doctor.sh`.

Put a reminder in your calendar to repeat steps 1-3 every three months.

---

## Optional: offsite backup

The two disks sit next to each other: a fire, a burglary or a power surge takes
both. The offsite backup stores an **encrypted** copy with a storage provider
(Backblaze B2 costs roughly 6 USD per TB per month) or on a computer at a
relative's house.

1. Create a bucket and an application key at the provider (or an account on the
   other computer for `sftp:`).
2. `sudo nano /etc/home-driver/backup.env`: fill in `RESTIC_REPOSITORY`,
   `RESTIC_PASSWORD` (invent a long one) and the credentials.
3. **Write `RESTIC_PASSWORD` and the repository address on paper** and keep the
   paper somewhere else (a relative, a safe). Without that password the offsite
   backup is unreadable for everyone, forever.
4. Create the repository and run the first upload (can take days on a home
   connection; it continues where it stopped every night):

   ```
   $ sudo /opt/home-driver/scripts/backup-offsite.sh --init
   $ sudo systemctl start --no-block home-driver-backup-offsite.service
   $ journalctl -u home-driver-backup-offsite -f          # Ctrl+C stops watching, not the upload
   ```

5. Restore test, once the first upload is done (downloads everything; use
   `--dry-run` first):

   ```
   $ sudo /opt/home-driver/scripts/restore.sh offsite /srv/home-driver/data/restored/offsite-test
   $ sudo rm -r /srv/home-driver/data/restored
   ```

Thumbnails and transcoded videos are not uploaded by default (they can be
regenerated); the originals always are.

---

## Where things are

| What | Where |
|---|---|
| Your photos and files (Disk A) | `/srv/home-driver/data` |
| Backup copy and snapshots (Disk B) | `/srv/home-driver/backup/current`, `/srv/home-driver/backup/snapshots` |
| Settings (secrets!) | `/etc/home-driver/*.env` |
| Scripts | `/opt/home-driver/scripts/` |
| Databases | `/var/lib/home-driver/` |
| Database dumps | `/var/lib/home-driver/dumps`, copied to `/srv/home-driver/data/dumps` |
| What to do when something breaks | `deploy/docs/RUNBOOK.md` (also `/opt/home-driver/docs/RUNBOOK.md`) |
