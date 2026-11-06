# Settings worksheet

Answer the questions you need. Every answer becomes one `NAME=value` line,
either in your `.env` file (Docker Compose) or in Portainer's environment
variables (see [Portainer](#portainer) at the end).

- Only the **Basics** section is required. Leave anything else blank and
  BackTrackarr uses the default shown.
- Write lists comma-separated, with no quotes: `Netflix,Hulu`.
- `true`/`false` settings also accept `yes`/`no` and `1`/`0`.
- Keep the file private: it holds passwords and keys.

---

## 1. Basics (required)

**`ADMIN_USER`**: What username do you want to sign in with?
Default `admin`.

**`ADMIN_PASSWORD`**: What password do you want? Use a long one, e.g. from
your password manager. After first sign-in you can add an authenticator app
and passkeys on the **Security** page.

**`TZ`**: What time zone are you in? Use the
[tz database name](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones),
e.g. `America/Chicago`, `America/New_York`, `Europe/London`. Default `UTC`.

**`BASE_URL`**: What address will you open BackTrackarr at, and will Plex
use to reach it? Include `http://` or `https://`, and no trailing slash.
- On your home network, e.g. `http://192.168.1.10:8787`.
- Behind a reverse proxy, e.g. `https://backtrackarr.example.com`.
  Passkeys need this HTTPS form.

**`TRUST_PROXY`**: Is BackTrackarr behind a reverse proxy (Caddy, Nginx
Proxy Manager, Traefik, a Cloudflare tunnel) that every request goes
through? `true` or `false`. Default `false`.

**`PORT`** (Docker/Portainer only): Which port on the host should
BackTrackarr use? Default `8787`.

Optional, rarely changed:
- **`SESSION_DAYS`**: How many days should a sign-in last since your last
  visit? Default `30`.
- **`WEBHOOK_TOKEN`**: Leave blank. A random token is made for you and shown
  on the Sync page.
- **`LOG_LEVEL`**: `debug`, `info`, `warn` or `error`. Default `info`.
- **`AUTH_RESET`**: Leave `false`. Set it to `true` for one start only if you
  lose your authenticator and all recovery codes.

---

## 2. Plex

**`PLEX_URL`**: What's your Plex server's address *as BackTrackarr sees it*?
For example `http://192.168.1.10:32400`, or `http://plex:32400` if both run
in the same Docker network.

**`PLEX_TOKEN`**: What's your Plex token? To find it, open any movie in Plex
Web → **⋯ → Get Info → View XML**. The token is the value after
`X-Plex-Token=` in the address bar.
[Plex's guide](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/).

**`PLEX_POLL_SECONDS`**: Do you have Plex Pass?
- Yes: leave this `0`, and paste the webhook URL from BackTrackarr's Sync
  page into Plex → Settings → Webhooks.
- No: set `10`, so BackTrackarr checks what's playing every 10 seconds.

**`SCROBBLE_USERS`**: Whose plays should go to Trakt, Simkl and the other
services? Give Plex usernames, comma-separated. Blank means only you, the
server owner. Everyone's plays are still saved in BackTrackarr.

**`PRIVATE_LIBRARIES`**: Which Plex libraries should stay private, saved here
but never sent anywhere? Use exact library names, comma-separated, e.g.
`Adult,Home Videos`. These are also left out of AI recommendations unless
you tick "include adult".

**`WATCHED_PERCENT`**: At what percent does a movie or episode count as
watched? Default `80`.

Optional:
- **`PLEX_ARCHIVE_MINUTES`**: How often should BackTrackarr back up your
  library's metadata, playlists and collections? In minutes, `0` = off.
  Default `60`.
- **`PLEX_WATCH_SCAN_SECONDS`**: How often should it check for "Mark as
  watched"? In seconds, `0` = off. Default `300`.
- **`PLEX_ALL_USERS`**: Also cover shared and managed users' playlists and
  watch marks? Default `true`.
- **`PLEX_ALLOW_WRITES`**: Allow restoring to a new Plex server and copying
  playlists between users? These only run when you press a button. Default
  `false`, meaning BackTrackarr never changes Plex.

---

## 3. Trakt and Simkl

**`TRAKT_CLIENT_ID`** and **`TRAKT_CLIENT_SECRET`**: Do you use Trakt? If
so, create an app at <https://trakt.tv/oauth/applications/new>:
- Name: anything, e.g. BackTrackarr.
- Redirect URI: `urn:ietf:wg:oauth:2.0:oob`.

Copy its **Client ID** and **Client Secret** here. Afterwards, click
**Connect** on BackTrackarr's Sync page.

**`SIMKL_CLIENT_ID`**: Do you use Simkl? Create an app at
<https://simkl.com/settings/developer/new/> (redirect URI
`urn:ietf:wg:oauth:2.0:oob`), and copy its **Client ID**. Then **Connect**
on the Sync page.

Optional:
- **`TRACKER_PULL_MINUTES`**: How often should BackTrackarr pull new plays
  from Trakt and Simkl, such as what their apps recorded from Netflix? In
  minutes, `0` = off. Default `60`.
- **`TRACKER_PULL_USER`**: Which BackTrackarr user should pulled and
  streaming plays belong to? Default: the Plex owner.

---

## 4. Other places to send plays (optional)

**`FLOPPY_URL`** and **`FLOPPY_TOKEN`**: Do you use Floppy? Give its
address, e.g. `http://floppy:8000`, and the API token from Floppy →
Settings → Integrations.

**`RELAY_URLS`**: Which services should get a copy of every Plex webhook?
Comma-separated URLs, for example:
- CrossWatch's Plex webhook URL.
- Floppy's `/webhook/plex/<token>` URL.
- Trakt's own Plex scrobbler URL (Trakt VIP).

**`WEBHOOK_URLS`**: Any other tools (n8n, Home Assistant, your own scripts)
that should get each play as JSON? Comma-separated URLs.

---

## 5. Streaming services (Netflix, Disney+, Hulu, Prime Video…)

**`TMDB_API_KEY`**: Get a free key at
<https://www.themoviedb.org/settings/api>: create an account, request an
API key, choose "Developer". Paste the **API Key**, or the longer **API Read
Access Token**. Either works. With it, BackTrackarr can:
- match streaming titles to IMDb/TMDb ids;
- find episode numbers;
- show where recommendations stream.

**`STREAMING_REGION`**: Which country's streaming catalog applies to you?
Two letters, e.g. `US`, `CA`, `GB`, `AU`. Default `US`.

**`MY_STREAMING_SERVICES`**: Which services do you pay for?
Comma-separated, written the way TMDB names them, e.g.
`Netflix,Disney Plus,Hulu,Amazon Prime Video,Max,Paramount Plus,Peacock,Apple TV Plus`.

Optional:
- **`STREAMING_IMPORT_DIR`**: Folder BackTrackarr watches for history
  exports. Default `/data/import/streaming`.

---

## 6. Jellyfin and Emby (optional)

These are for copying Plex playlists to Jellyfin or Emby, for example for
Pleazy. To scrobble *from* Jellyfin or Emby, you don't need these: use the
webhook URLs on BackTrackarr's Streaming page.

**`JELLYFIN_URL`** and **`JELLYFIN_API_KEY`**: Your Jellyfin address, e.g.
`http://192.168.1.10:8096`. For the key, go to Dashboard → API Keys → **+**.

**`EMBY_URL`** and **`EMBY_API_KEY`**: Your Emby address, and a key from
Settings → Advanced → API Keys.

More servers: add `JELLYFIN_2_URL`/`JELLYFIN_2_API_KEY`, and so on, up to 9.

---

## 7. Sonarr, Radarr, Whisparr (optional, any number)

For each instance, answer: what's its address, and what's its API key
(Settings → General → Security → API Key)?

| | Address | API key |
|---|---|---|
| First Sonarr | `SONARR_URL` | `SONARR_API_KEY` |
| First Radarr | `RADARR_URL` | `RADARR_API_KEY` |
| First Whisparr | `WHISPARR_URL` | `WHISPARR_API_KEY` |
| More of each | `SONARR_2_URL`, `RADARR_2_URL`, `RADARR_3_URL`… (up to 9) | same with `_API_KEY` |

Per instance, optional (use the same prefix, e.g. `RADARR_2_`):
- **`_NAME`**: what to call it, e.g. `radarr-4k`.
- **`_ADULT`**: is it for adult content? `true`/`false`. Whisparr is always
  adult.
- **`_ROOT_FOLDER`**: where recommended titles get added. Default: its first
  root folder.
- **`_QUALITY_PROFILE`**: which quality profile, by name or id. Default:
  its first profile.

**`ARR_ARCHIVE_HOURS`**: How often should their libraries and settings be
backed up? In hours. Default `12`.

---

## 8. AI recommendations (optional)

Your titles and ratings are sent to whichever AI provider you use.

**`ANTHROPIC_API_KEY`**: Want Claude recommendations? Create a key at
<https://console.anthropic.com/settings/keys>.
- **`CLAUDE_MODEL`**: Default `claude-opus-5-5`.

**`GEMINI_API_KEY`**: Want Gemini recommendations? Create a key at
<https://aistudio.google.com/apikey>.
- **`GEMINI_MODEL`**: Default `gemini-2.5-pro`. Set a newer model name here
  if Google has one.

**`AI_INCLUDE_ADULT`**: Should "include adult" be ticked by default?
Default `false`.

---

## 9. Comics and backups (optional)

**`CLZ_IMPORT_DIR`**: Folder BackTrackarr watches for CLZ Comics CSV
exports. Default `/data/import/clz`.

**`BACKUP_KEEP`**: How many daily backups should be kept? Default `7`.

**`BACKUP_COMPRESS`**: Compress backups? Default `true`. Leave it on, since a
5 GB database shrinks to roughly 1–2 GB.

---

## Portainer

1. Create the stack with the **Web editor** (not Repository; see
   [INSTALL.md](INSTALL.md#portainer)), then open **Environment variables**
   and click **Advanced mode**.
2. Paste the list from
   [deploy/portainer/stack.env.example](../deploy/portainer/stack.env.example).
   It has every setting, one per line, already in the format Portainer
   wants.
3. Replace the values in `<angle brackets>` with your answers. Clear any
   line you don't use, or leave it ending in `=` for the default.

Portainer saves these as `stack.env`, and the BackTrackarr stack hands
every line to the container. That covers numbered extras like
`RADARR_2_URL` too, so you can add any setting from this page as a new
line.
