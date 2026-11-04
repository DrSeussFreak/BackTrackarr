# Reelog

A self-hosted, Trakt-style watch history for Plex. It records everything you
watch, keeps it for decades, and forwards each play to Trakt, Simkl, Floppy,
and anything that accepts Plex webhooks.

- **Small footprint:** one static Go binary on distroless (~20 MB image). It
  uses about 15–30 MB of RAM and almost no CPU when idle.
- **Long-term storage:** SQLite in WAL mode, with a pure-Go driver (no CGO).
  Daily snapshots go to `/data/backups`, and you can export everything as
  NDJSON.
- **Delivery that survives outages:** every outgoing event goes into a
  durable per-service queue. Events are delivered in order, retried with
  backoff, and rate-limited for Trakt. If a service is down for a day,
  nothing is lost: completed plays are sent later as history entries with
  the original timestamp.

## Why SQLite

Personal watch history is small. Fifty years at 20 plays a day is about
365k rows, and SQLite handles hundreds of millions. Postgres would add a
second container and 50–100 MB of RAM with no real gain at this scale.
History pages use keyset pagination, so paging back to 1998 is as fast as
loading today. The database is one file, so backing it up is a file copy.

## Quick start

```bash
cp .env.example .env    # then edit it
docker compose up -d --build
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

The first build runs `go mod tidy`. Copy the generated `go.sum` out of the
build if you want reproducible builds.

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
or let CrossWatch pull from Trakt/Simkl after Reelog has pushed to them.

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
- **After a play counts as watched:** Reelog ignores that session's later
  pause, resume, and stop events (for example, the credits), so services
  don't see a second watch.
- **Duplicates:** a second play of the same item by the same user within 3
  hours is treated as a duplicate. This covers webhook + poller + import
  overlap.
- **ID lookup:** Plex webhooks carry episode IDs but not show IDs. When
  `PLEX_URL`/`PLEX_TOKEN` are set, Reelog asks Plex once per show and caches
  the result. Simkl and Floppy match episodes by show ID, so setting these
  is recommended.
- **Who gets sent:** all users' plays are stored locally. Only `SCROBBLE_USERS`
  (default: the server owner) are sent to external services.
- **Stale events:** queued live events (start/pause/stop) older than 15
  minutes are dropped. Queued *watched* events older than 1 hour are sent
  as history entries with the original `watched_at`.

## Plex archive: never lose your library work

Reelog keeps a **read-only** copy of your Plex server in its database:

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

**Restoring after Plex dies:** point Reelog at the new server and let it
archive once. Then open **Library → Restore**, pick the old server, preview
the changes, and apply. This recreates every user's playlists and puts back
sort titles, labels, collections and ratings. Restored fields are locked so a
refresh won't undo them.

**Reelog never changes Plex** unless you set `PLEX_ALLOW_WRITES=true`. Even
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

Pressing **Mark as watched** in Plex sends no webhook. Reelog notices it from
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

Configure any number of each (`SONARR_URL`, `SONARR_2_URL`, …). Reelog
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

- `GET /api/stats?user=`
- `GET /api/history?user=&kind=movie|episode&q=&limit=&before=<cursor>`
  returns `{items, next}`
- `GET /api/export` streams every play as NDJSON
- `POST /webhook/plex/{token}` takes a Plex multipart webhook, or a raw JSON
  payload for testing. It needs no basic auth; the token protects it.

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
