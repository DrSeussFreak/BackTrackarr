# Installing BackTrackarr

BackTrackarr is one small program with one database file. Pick whichever
way of running it suits you; they all behave the same, and your data can
move between them (copy `backtrackarr.db` from the data folder).

| Method | Best for | Updates |
|---|---|---|
| [Docker](#docker) | Any machine with Docker | `docker pull` + recreate |
| [Docker Compose](#docker-compose) | Most home servers | `docker compose pull && docker compose up -d` |
| [Portainer](#portainer) | Managing containers in a web UI | *Pull and redeploy* |
| [Linux (systemd)](#linux) | A Pi, VM or server without Docker | re-run the installer |
| [Windows](#windows) | A Windows PC or server | re-run the installer |
| [From source](#from-source) | Development | `git pull && go build` |

After installing, open `http://<host>:8787`, then go to **Sync** to connect
Plex, Trakt and Simkl, and **Streaming** for Netflix and the other services.
Set `ADMIN_PASSWORD` before exposing it beyond your own network.

Images are published for `linux/amd64`, `linux/arm64` and `linux/arm/v7`
(Raspberry Pi 2 and later). Downloads are on the
[Releases](https://github.com/DrSeussFreak/BackTrackarr/releases) page for
Linux (amd64, arm64, armv7), Windows (amd64, arm64) and macOS.

---

## Docker

```bash
docker volume create backtrackarr-data
docker run -d --name backtrackarr --restart unless-stopped \
  -p 8787:8787 \
  -v backtrackarr-data:/data \
  -e TZ=America/New_York \
  -e ADMIN_PASSWORD=change-me \
  -e PLEX_URL=http://192.168.1.10:32400 \
  -e PLEX_TOKEN=your-plex-token \
  --memory 128m --cpus 0.5 \
  ghcr.io/drseussfreak/backtrackarr:latest
```

Add any other setting from [.env.example](../.env.example) with `-e NAME=value`,
or keep them in a file and use `--env-file backtrackarr.env`.

Update:

```bash
docker pull ghcr.io/drseussfreak/backtrackarr:latest
docker rm -f backtrackarr   # data stays in the volume
# then run the same docker run command again
```

## Docker Compose

```bash
git clone https://github.com/DrSeussFreak/BackTrackarr.git
cd BackTrackarr
cp .env.example .env      # edit it: ADMIN_PASSWORD, PLEX_URL, PLEX_TOKEN...
docker compose up -d
```

Without git, download [docker-compose.yml](../docker-compose.yml) and
[.env.example](../.env.example) into a folder, rename the example to `.env`,
and run `docker compose up -d` there.

To build the image yourself instead of pulling it:

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

Update: `docker compose pull && docker compose up -d`.

To have CLZ and streaming exports imported from a folder on the host,
uncomment the `./imports:/data/import` line in `docker-compose.yml`. Then
put files in `imports/clz/` or `imports/streaming/netflix/` (any service
name works as the subfolder).

## Portainer

1. **Stacks → Add stack**, name it `backtrackarr`.
2. Choose one:
   - **Web editor:** paste [deploy/portainer/stack.yml](../deploy/portainer/stack.yml).
   - **Repository:** URL `https://github.com/DrSeussFreak/BackTrackarr`,
     reference `refs/heads/main`, compose path `deploy/portainer/stack.yml`.
     This lets Portainer pull updates for you, and can do it automatically
     with *GitOps updates*.
3. Under **Environment variables**, add at least `ADMIN_PASSWORD`,
   `PLEX_URL` and `PLEX_TOKEN`, plus any others you want, such as
   `TRAKT_CLIENT_ID` and `TMDB_API_KEY`. Anything not set uses its default.
   *Advanced mode* lets you paste the whole `.env.example`.
4. **Deploy the stack.**

Update: open the stack → **Pull and redeploy** (tick *Re-pull image*).

## Linux

For any systemd distribution (Debian, Ubuntu, Raspberry Pi OS, Fedora,
Arch…) on x86-64, ARM64 or ARMv7:

```bash
curl -fsSL https://raw.githubusercontent.com/DrSeussFreak/BackTrackarr/main/deploy/linux/install.sh | sudo sh
```

The installer:

- downloads the latest release for your CPU;
- creates a `backtrackarr` system user;
- installs the program to `/usr/local/bin/backtrackarr`;
- writes your settings to `/etc/backtrackarr/backtrackarr.env` (first install only);
- keeps your data in `/var/lib/backtrackarr`;
- starts a hardened systemd service.

```bash
sudo nano /etc/backtrackarr/backtrackarr.env   # set ADMIN_PASSWORD, PLEX_URL, PLEX_TOKEN...
sudo systemctl restart backtrackarr
journalctl -u backtrackarr -f                  # logs
```

Options: `curl … | sudo VERSION=v0.2.0 PORT=8080 sh`. Update by running the
installer again; settings and data are kept. Remove with
`sudo sh deploy/linux/uninstall.sh` (add `--purge` to delete data too).

**Manual install:** download `backtrackarr_linux_<arch>.tar.gz` from Releases,
extract it, and run `./backtrackarr`. It reads `backtrackarr.env` from the
current folder or from the folder the program is in. The included
`backtrackarr.service` is the same unit the installer uses.

## Windows

In **PowerShell as Administrator**:

```powershell
irm https://raw.githubusercontent.com/DrSeussFreak/BackTrackarr/main/deploy/windows/install.ps1 | iex
```

Or download `backtrackarr_windows_amd64.zip` from Releases, extract it,
right-click `install.ps1` → *Run with PowerShell* (as Administrator).

| What | Where |
|---|---|
| Program | `C:\Program Files\BackTrackarr\backtrackarr.exe` |
| Settings | `C:\ProgramData\BackTrackarr\backtrackarr.env` |
| Data and backups | `C:\ProgramData\BackTrackarr\data` |

The installer also:

- registers a scheduled task that starts BackTrackarr at boot, hidden, and
  restarts it if it stops;
- allows port 8787 on private networks in Windows Firewall, so Plex on
  another machine can reach it.

Edit the settings file in Notepad, then restart:

```powershell
Stop-ScheduledTask BackTrackarr; Start-ScheduledTask BackTrackarr
```

Update by running the installer again. Remove with
`& "C:\Program Files\BackTrackarr\uninstall.ps1"` (add `-Purge` to delete
data too).

**Just try it:** extract the zip, copy `backtrackarr.env.example` to
`backtrackarr.env` next to `backtrackarr.exe`, edit it, and double-click
`backtrackarr.exe`. Data goes to a `data` folder beside it.

> If you'd rather run it as a real Windows service, [NSSM](https://nssm.cc/)
> works: `nssm install BackTrackarr "C:\Program Files\BackTrackarr\backtrackarr.exe"`
> with the startup directory set to `C:\ProgramData\BackTrackarr`.

## From source

Requires Go 1.26 or newer. No C compiler is needed: the SQLite driver is pure Go.

```bash
git clone https://github.com/DrSeussFreak/BackTrackarr.git
cd BackTrackarr
go test ./...
go build -o backtrackarr .
cp .env.example backtrackarr.env   # edit it
./backtrackarr
```

---

## Moving between methods

Stop BackTrackarr and copy `backtrackarr.db` from one data folder to the
other: `/data` in Docker, `/var/lib/backtrackarr` on Linux,
`C:\ProgramData\BackTrackarr\data` on Windows. Every play, rating, archive
snapshot, comic and setting travels with that one file. Databases from older
versions (named `reelog.db` or `backlottracker.db`) are upgraded
automatically.

## Backups

A compressed snapshot is written to the data folder's `backups/` every day.
The last 7 are kept (`BACKUP_KEEP`). To restore one, stop BackTrackarr,
unzip it (`gunzip backtrackarr-….db.gz`), and put it in place as
`backtrackarr.db`.

## HTTPS and passkeys

Two-step sign-in with an authenticator app works on any address. Passkeys
only work on HTTPS with a domain name (or on `localhost`), because browsers
won't offer them anywhere else. Typical setup:

1. Point a name at your server, e.g. `backtrackarr.example.com`. A local DNS
   name or a Cloudflare/Tailscale address works too.
2. Put a reverse proxy with a certificate in front: Caddy, Nginx Proxy
   Manager, Traefik, SWAG, or a Cloudflare tunnel. Forward to port 8787 and
   keep the original `Host` header.
3. Set `BASE_URL=https://backtrackarr.example.com` and `TRUST_PROXY=true`.

Caddy, for example:

```
backtrackarr.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

Only set `TRUST_PROXY=true` when every request comes through the proxy.
Otherwise someone could fake their IP address and get around the
failed-attempt lockout.
