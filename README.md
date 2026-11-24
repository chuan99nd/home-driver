# home-driver

A private "Google Drive + Google Photos" for one family, running on an always-on
laptop with two USB hard disks.

| Part | What it does | Address |
|---|---|---|
| Photos — [Immich](https://immich.app) | Phone backup, faces, places, search, albums, duplicates | `photos.<domain>` |
| Drive — `drive/` | Files and folders, share by email, public links, trash, WebDAV sync | `drive.<domain>` |
| Photo clean-up — `curator/` | Suggests blurry shots, screenshots and bursts for review; never deletes by itself | `drive.<domain>/curator/` (admins) |
| Safety — `deploy/` | Nightly copy to the second disk with snapshots, optional encrypted offsite backup, disk health alerts, one-command restore | — |

Everything listens on `127.0.0.1` and is reached through your Cloudflare Tunnel.
Sign-in is with Google; sharing with an email address lets that Google account in.

## Install on the laptop

Follow **[deploy/docs/INSTALL.md](deploy/docs/INSTALL.md)** step by step. In short:

```bash
git clone <this repo> && cd home-driver
make build                                   # needs Go 1.24+ and uv
sudo deploy/scripts/laptop-setup.sh          # lid, sleep, battery
sudo deploy/scripts/setup-disks.sh --help    # format the two disks (asks for the device explicitly)
sudo deploy/scripts/install.sh --dry-run     # see what would happen
sudo deploy/scripts/install.sh               # install, nothing started yet
sudoedit /etc/home-driver/*.env              # domain, Google sign-in, alerts
sudo deploy/scripts/install.sh --enable      # start services and timers
sudo /opt/home-driver/scripts/doctor.sh      # health report
```

Then add the two hostnames from
[deploy/cloudflared/ingress.example.yml](deploy/cloudflared/ingress.example.yml)
to your tunnel, and **do the first restore test** described at the end of the
install guide. A backup that was never restored is not a backup.

When something goes wrong: [deploy/docs/RUNBOOK.md](deploy/docs/RUNBOOK.md).

## Develop

```bash
make build    # drive binary + curator venv
make test     # go test, pytest, deploy script tests
make lint
drive/scripts/dev.sh 8090    # local Drive with sample data and test sign-in links
```

Contributors (human or AI) start with [CLAUDE.md](CLAUDE.md) — the contract between
the parts and the rules that protect the data — then the guide of the part being
changed (`drive/CLAUDE.md`, `curator/CLAUDE.md`, `deploy/CLAUDE.md`) and
[docs/DESIGN.md](docs/DESIGN.md) for anything visible to users.
