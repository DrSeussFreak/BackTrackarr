# BackTrackarr

A self-hosted, Trakt-style watch history for Plex and your streaming
services. It records everything you watch, keeps it for decades, and
forwards each play to Trakt, Simkl, Floppy, and anything that accepts Plex
webhooks.

- **Small footprint:** one static Go binary on distroless (~20 MB image). It
  uses about 15–50 MB of RAM and almost no CPU when idle.
- **Long-term storage:** SQLite in WAL mode, with a pure-Go driver (no CGO).
  Compressed snapshots go to `/data/backups` daily, and you can export
  everything as NDJSON.
- **Delivery that survives outages:** every outgoing event goes into a
  durable per-service queue. Events are delivered in order, retried with
  backoff, and rate-limited for Trakt. If a service is down for a day,
  nothing is lost: completed plays are sent later as history entries with
  the original timestamp.

## Install

Docker, Docker Compose, Portainer, Linux (systemd) and Windows are all
covered step by step in **[docs/INSTALL.md](docs/INSTALL.md)**. The quickest
way:

```bash
git clone https://github.com/DrSeussFreak/BackTrackarr.git && cd BackTrackarr
cp .env.example .env    # then edit it
docker compose up -d
```

Open `http://<host>:8787`. Then go to **Sync**:

1. **Plex.** Copy the webhook URL into Plex under *Settings → Webhooks*.
   This needs Plex Pass. Without Plex Pass, set `PLEX_URL`, `PLEX_TOKEN` and
   `PLEX_POLL_SECONDS=10` instead.
2. **Trakt / Simkl.** Click **Connect** and enter the code shown on the
   service's site.
3. **Back-fill.** Click **Import Plex watch history**, and/or **Import Trakt
   history**. Then use **Push history** to send your full history to each
   service.
4. **Streaming services.** See the **Streaming** page (below).

## Why SQLite, and how big it gets

SQLite is the right database here even at many gigabytes. It handles
databases up to 281 TB, and multi-GB files are routine for it. What matters
for speed is indexes, not file size. Every page here uses an index: history
uses keyset pagination, so paging back to 1998 is as fast as loading today,
and the dashboard's totals are cached.

Postgres or MySQL would only help with things this app doesn't do: many
programs writing at once over a network, or several BackTrackarr copies
sharing one database. The cost would be a second container, 50–100 MB more
RAM, and harder backups.

Rough sizes:

- **Watch history:** 50 years of heavy use (400k plays) is about 45 MB.
- **Library archive:** compressed full metadata is a few KB per item, so a
  large Plex library (50k movies and episodes, with music) is a few hundred
  MB to a couple of GB, growing slowly as versions are kept.

**At 5 GB or more, mind the disk space for backups.** Each daily snapshot is
a full copy, so they're gzip-compressed (`BACKUP_COMPRESS=true`, roughly
3–5× smaller), and `BACKUP_KEEP` controls how many are kept. Memory use
doesn't grow with the file: SQLite reads pages from disk and BackTrackarr
caps its page cache at 8 MB.

## Streaming services (Netflix, Disney+, Hulu, Prime Video…)

None of the big streaming services offers a public API for your watch
history, so there's no account to connect. The **Streaming** page offers
four ways in instead. Use any or all of them; duplicates are dropped.

1. **History exports.** Upload or drop in a folder:
   - Netflix's *Viewing activity* download (`NetflixViewingHistory.csv`), or
     the fuller `ViewingActivity.csv` from *Download your personal
     information*. Profiles become users; trailers and previews are skipped.
   - Amazon's *Request My Data* (Prime Video).
   - Any CSV or JSON from Disney+, Hulu, Max, Paramount+, Peacock, Apple TV+,
     Crunchyroll and others. Columns are matched by name.

   Titles like *"Stranger Things: Season 1: Chapter One"* are split into
   show, season and episode. IMDb/TMDb/TVDB ids and episode numbers are
   looked up via TMDB (`TMDB_API_KEY`) or Trakt.
2. **Live in your browser.** A one-click userscript (Tampermonkey or
   Violentmonkey) reports what you play on about 25 streaming sites, live,
   to `POST /api/scrobble/{token}`. Anything else can use that API too.
3. **Jellyfin and Emby** webhooks, live.
4. **Trakt and Simkl pulls.** Plays that their apps, imports and browser
   extensions recorded are pulled in every hour.

Recommendations show **where each title streams** in your region (TMDB
watch providers). **Only what I can watch** limits picks to what's in your
Plex or on `MY_STREAMING_SERVICES`.

## Supported services

| Service | How | Live scrobble | History | Ratings |
|---|---|---|---|---|
| **Trakt** | API v2, device-code OAuth with auto refresh | ✓ start/pause/stop | ✓ push and import | ✓ |
| **Simkl** | API, PIN auth | ✓ start/pause/stop | ✓ push | movies |
| **Floppy** | `POST /api/v1/scrobble` with an API token | ✓ | ✓ push (one per item) | – |
| **Floppy/Yamtrack, CrossWatch, Trakt's own Plex scrobbler, Scrob…** | Plex webhook relay (`RELAY_URLS`) | ✓ (raw Plex events) | – | ✓ (as `media.rate`) |
| **Anything else** | Generic JSON webhook (`WEBHOOK_URLS`) | ✓ | ✓ | ✓ |

**Plekt and WeTrakr:** I couldn't find a public API for either one. When
they publish one, adding a sink is about 80 lines in
`internal/sinks/`. Until then, if either accepts Plex webhooks, put its URL
in `RELAY_URLS`.

**CrossWatch** is a sync engine itself. Point a relay at its Plex webhook,
or let CrossWatch pull from Trakt/Simkl after BackTrackarr has pushed to them.

## How scrobbling works

```
Plex webhook ─┐                       ┌─► plays (deduped, all users)
              ├─► normalize ─► session ┤
Plex poller ──┘    + resolve IDs       └─► outbox ─► worker ─► Trakt / Simkl / Floppy / relays / webhooks
```

- **Events:** `media.play` and `media.resume` map to *start*,
  `media.pause` to *pause*, and `media.stop` to *stop*. `media.scrobble` (or
  a stop at or above `WATCHED_PERCENT`) maps to *watched*, and `media.rate`
  to *rate*.
- **After a play counts as watched:** BackTrackarr ignores that session's later
  pause, resume, and stop events (for example, the credits), so services
  don't see a second watch.
- **Duplicates:** a second play of the same item by the same user within 3
  hours is treated as a duplicate. This covers webhook + poller + import
  overlap.
- **ID lookup:** Plex webhooks carry episode IDs but not show IDs. When
  `PLEX_URL`/`PLEX_TOKEN` are set, BackTrackarr asks Plex once per show and caches
  the result. Simkl and Floppy match episodes by show ID, so setting these
  is recommended.
- **Who gets sent:** all users' plays are stored locally. Only `SCROBBLE_USERS`
  (default: the server owner) are sent to external services.
- **Stale events:** queued live events (start/pause/stop) older than 15
  minutes are dropped. Queued *watched* events older than 1 hour are sent
  as history entries with the original `watched_at`.

## Plex archive: never lose your library work

BackTrackarr keeps a **read-only** copy of your Plex server in its database:

- **Every item:** movies, shows, seasons, episodes, artists, albums and
  tracks, with full metadata.
- **Hand-edited fields:** sort title, original title, edition, labels,
  collections, user rating, and which fields are locked.
- **Every user's playlists,** regular and smart (smart playlists keep their
  filter), and **every collection**.
- **Earlier versions** of anything that changes. A bad edit or a metadata
  refresh never loses what was there.
- **Deleted items** are marked, never removed.

Incremental archives run every `PLEX_ARCHIVE_MINUTES`, with a full sweep
daily. **Library → Export archive** downloads everything as NDJSON.

**Restoring after Plex dies:** point BackTrackarr at the new server and let it
archive once. Then open **Library → Restore**, pick the old server, preview
the changes, and apply. This recreates every user's playlists and puts back
sort titles, labels, collections and ratings. Restored fields are locked so a
refresh won't undo them.

**BackTrackarr never changes Plex** unless you set `PLEX_ALLOW_WRITES=true`. Even
then, it only writes when you press a button (restore, copy). It only ever
creates playlists or fills in fields; it never deletes anything.

## Copying playlists

From any archived playlist or collection (**Library → a playlist**):

- **To another Plex user.** Smart playlists stay smart. A collection becomes
  a playlist of its items. Needs `PLEX_ALLOW_WRITES=true`.
- **To Jellyfin or Emby** (`JELLYFIN_URL`/`EMBY_URL`), for example for
  [Pleazy](https://github.com/MasterTroll650/plezy) connected to those
  servers. Items are matched by IMDb/TMDb/TVDB/MusicBrainz id, then by file
  name. Smart playlists are copied as their current items.
- **As M3U.**

Pleazy connected to Plex already shows your Plex playlists, so there's
nothing to copy in that case. If Pleazy scrobbles to Trakt or Simkl itself,
turn that off in Pleazy *or* here, or you'll get double plays.

## "Mark as watched" and rewatches

Pressing **Mark as watched** in Plex sends no webhook. BackTrackarr notices it from
Plex's `lastViewedAt` and records the play at that moment, the time of the
press. Every play is its own row, so rewatches are always kept. The
dashboard shows your most rewatched titles.

## Music

Track plays from webhooks, the session poller and the history import are
stored. A track counts as played at 50%, the same rule Last.fm uses. The
dashboard shows listening time and top artists. Trakt, Simkl and Floppy
don't take music, so tracks go only to relays and generic webhooks.

## Comics (CLZ)

Upload a **CLZ Comics** CSV export on the **Comics** page, or drop it into
`/data/import/clz`. Every column is kept. Re-importing is safe: a later
read date is stored as a reread, and fields missing from a smaller export
are not wiped.

## Recommendations (Claude and Gemini)

**For you** sends a taste profile to Claude and/or Gemini, using your own
`ANTHROPIC_API_KEY` / `GEMINI_API_KEY`, and keeps every run. The profile
covers your best and worst rated titles, from both plays and Plex ratings,
plus your most rewatched and recent titles. Titles you've seen are filtered
out, and ones already in Plex are marked. **Add** sends a pick to any
Sonarr, Radarr or Whisparr instance.

## Sonarr, Radarr, Whisparr

Configure any number of each (`SONARR_URL`, `SONARR_2_URL`, …). BackTrackarr
archives each instance's library and settings: quality profiles, custom
formats, root folders, tags and naming. Credentials such as download clients
and indexers are not archived.

## Adult content

Adult content is supported, and kept private:

- List adult libraries in `PRIVATE_LIBRARIES`. Their plays are stored and
  archived like everything else, but never sent to Trakt, Simkl, relays or
  webhooks.
- They are left out of AI recommendations unless you tick
  **include adult**.
- Whisparr instances are always treated as adult.

## API

All API routes sit behind the admin basic auth.

Exceptions: the webhook and scrobble endpoints below use the webhook token in the URL instead.

- `GET /api/stats?user=`
- `GET /api/history?user=&kind=movie|episode&q=&limit=&before=<cursor>`
  returns `{items, next}`
- `GET /api/export` streams every play as NDJSON
- `POST /webhook/plex/{token}` takes a Plex multipart webhook, or a raw JSON
  payload for testing. It needs no basic auth; the token protects it.
- `POST /webhook/jellyfin/{token}` and `POST /webhook/emby/{token}` take
  Jellyfin (webhook plugin, Generic destination) and Emby webhooks.
- `POST /api/scrobble/{token}` takes a play from anywhere:
  `{"action":"start|pause|stop|watched","service":"Netflix","raw_title":"Show: Season 1: Episode","progress":42}`,
  or structured `title`/`show`/`season`/`episode`/`ids` fields.

## Configuration

See [.env.example](.env.example). Every setting is an environment
variable.

## Development

```bash
go test ./...
DATA_DIR=./data go run .
```

Layout:

- `internal/plex` parses webhooks and talks to the Plex server
- `internal/store` holds the SQLite schema and queries
- `internal/core` has ingest, sessions, the delivery worker, the poller, and
  imports
- `internal/sinks` has one file per destination
- `internal/web` has the UI, API, and webhook handler
