# Installing BackTrackarr

BackTrackarr is one small program with one database file. Pick whichever
way of running it suits you; they all behave the same, and your data can
move between them (copy `backtrackarr.db` from the data folder).

| Method | Best for | Updates |
|---|---|---|
| [Docker](#docker) | Any machine with Docker | `docker pull` + recreate |
| [Docker Compose](#docker-compose) | Most home servers | `docker compose pull && docker compose up -d` |
| [Portainer](#portainer) | Managing containers in a web UI | Editor → *Re-pull image* → Update |
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

Everything you need, in order. It takes about 5 minutes.

**Before you start, have these ready.** Use [CONFIG.md](CONFIG.md) to work
out your answers:

- Your **Plex address and token** (CONFIG.md §2).
- The **address you'll open BackTrackarr at**: your server's IP and port
  8787, or a domain behind a reverse proxy.
- Any optional keys you want: Trakt, Simkl, TMDB, Sonarr/Radarr, AI.

### 1. Create the stack

**Stacks → + Add stack**, then:

| Field | Value |
|---|---|
| Name | `backtrackarr` (lowercase, no spaces) |
| Build method | **Web editor** |

Paste the contents of
[deploy/portainer/stack.yml](../deploy/portainer/stack.yml) into the editor.

> **Use the Web editor, not Repository.** The stack reads your settings
> from `stack.env`. Portainer creates that file from your environment
> variables for the Web editor, Upload and Custom template methods. With
> **Repository**, Portainer expects `stack.env` to already be in the Git
> repo, and your passwords and keys must never go in a public repo. Only
> use Repository from your own private fork that holds a `stack.env`.
> Portainer also only accepts lowercase stack names.

### 2. Environment variables

1. Scroll to **Environment variables** and click **Advanced mode**.
2. Paste
   [deploy/portainer/stack.env.example](../deploy/portainer/stack.env.example).
3. Replace every `<…>` value with your own. The minimum is
   `ADMIN_PASSWORD`, `TZ`, `BASE_URL`, `PLEX_URL` and `PLEX_TOKEN`.
4. Lines left ending in `=` use the default. Delete lines you'll never
   need if you like.
   Don't leave a `<…>` placeholder in place: it would be used as the real
   value. Delete the line instead.
5. Add extra lines for anything else in CONFIG.md, e.g. a second Radarr:
   ```
   RADARR_2_URL=http://192.168.1.10:7879
   RADARR_2_API_KEY=abc123
   RADARR_2_NAME=radarr-4k
   ```

Portainer saves these as `stack.env`, and the stack hands every line to
BackTrackarr. Don't use quotes around values, and no spaces around `=`.

### 3. Deploy

Click **Deploy the stack**. Portainer pulls
`ghcr.io/drseussfreak/backtrackarr`, about 20 MB; the image is public, so no
registry login is needed. It then creates:

- **Container** `backtrackarr`: restarts automatically, limited to 128 MB of
  RAM and half a CPU. It has a built-in health check, so it shows
  *healthy* after about 10 seconds.
- **Volume** `backtrackarr_backtrackarr-data`: your database, backups and
  imports. Back up or move this volume to keep your history.
- **Port**: the host's `PORT` (8787) to the container's 8787.

### 4. First sign-in

1. Open `BASE_URL`, e.g. `http://192.168.1.10:8787`, and sign in with
   `ADMIN_USER` / `ADMIN_PASSWORD`.
2. **Sync** page:
   - Copy the **Plex webhook URL** into Plex → Settings → Webhooks (needs
     Plex Pass).
   - **Connect** Trakt and Simkl.
   - Click **Import Plex watch history**.
3. **Security** page: set up an authenticator app and/or passkeys.
4. **Streaming** page: import Netflix and other exports, and install the
   browser scrobbler.

### Updating

| Build method | How to update |
|---|---|
| Web editor | Stack → **Editor** → **Update the stack** (tick *Re-pull image and redeploy*). |
| Pin a version | Set `BACKTRACKARR_VERSION=0.3.0` instead of `latest`. |

Your data stays in the volume across updates.

### Changing settings later

Stack → **Editor** → change the environment variables → **Update the
stack**. The container restarts with the new values.

### Optional extras

- **Import folder on the host.** In the stack file, uncomment
  `- /srv/backtrackarr/import:/data/import` and change the left side to a
  folder on your server. CLZ exports go in `clz/`, streaming exports in
  `streaming/netflix/`, `streaming/hulu/` and so on.
- **Reverse proxy and passkeys.** Put Nginx Proxy Manager, Caddy, Traefik
  or SWAG in front with a certificate. For SWAG, use the ready-made
  [deploy/swag/backtrackarr.subdomain.conf.sample](../deploy/swag/backtrackarr.subdomain.conf.sample). Then set `BASE_URL=https://…` and
  `TRUST_PROXY=true`, and update the stack.
  - If the proxy runs in Docker, either put both on the same network and
    proxy to `http://backtrackarr:8787`, or proxy to `http://<host-ip>:8787`.
  - To join BackTrackarr to an existing proxy network, add this to the
    stack (Web editor):
    ```yaml
        networks: [proxy]
    networks:
      proxy:
        external: true
    ```
- **Plex in Docker on the same host.** `PLEX_URL=http://<host-ip>:32400`
  always works. `http://plex:32400` only works if both containers share a
  network.

### Troubleshooting

- **Logs:** Containers → `backtrackarr` → **Logs**. It prints which
  services were found and any errors.
- **Webhook not arriving.** Plex must be able to reach `BASE_URL`. Test by
  opening the webhook URL's address from the Plex machine.
- **"Sign in required" from a script.** Two-step sign-in is on; use an API
  token from the Security page.
- **Locked out of two-step sign-in.** Add `AUTH_RESET=true`, update the
  stack, sign in with the password, then remove the line and update again.
- **"env file stack.env not found".** Either the stack has no
  environment variables yet (add at least `ADMIN_PASSWORD` and deploy
  again), or it was created with the **Repository** method. Recreate it
  with the **Web editor**.

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
