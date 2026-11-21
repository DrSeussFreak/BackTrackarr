# Book, audiobook and game sources

BackTrackarr only reads from these services. It never changes anything on
them. Set them up on the **Settings** page. Researched November 2026 against
each project's own API documentation, OpenAPI file or source code. The
importers are tested against fake servers built from those documents, so a
service that has changed its API since then may need a small fix.

## Built in

| Source | Covers | How | Limits |
|---|---|---|---|
| **Audiobookshelf** | Audiobooks, ebooks | API token | One user per token. Every listening session is kept. Podcasts are skipped. |
| **Komga** | Books, comics | API key | Keeps only the latest read per book, so a reread is seen only if a sync runs between reads. Comics go to the Comics page. |
| **Kavita** | Books, comics | Auth key | Needs a version with the reading history API (present in 0.9.1). Older versions show an HTTP 404 on the Books page. |
| **Hardcover** | Books, audiobooks | API token | Personal use. Free plan: 60 requests a minute, 5,000 a day. Each read is a separate row. |
| **Open Library** | Books | Username | The reading log must be public. One date per book, no ratings or progress. One request a second. |
| **Goodreads** | Books, audiobooks | CSV export | No usable API. Exports can miss read dates, so a Read Count above the dates given is kept as undated reads. |
| **StoryGraph** | Books, audiobooks | CSV export | No public API. Multiple read dates are kept. |
| **Audible** | Audiobooks | Audible Library Extractor CSV, or Amazon Request My Data files | Auto-detected on Books and Import. Keeps authors, narrators, length, progress, your own rating and every original column. A library file has no finish dates, so a finished title counts as one undated listen. Dated sessions come only from listening files, whose format Amazon doesn't document. Audible may keep only a short listening history, so export regularly. |
| **Steam** | Games | Web API key | Game details must be visible. Steam gives lifetime totals, so sessions are the change between syncs. 100,000 calls a day. |
| **RetroAchievements** | Retro games | Web API key | Achievement and last-played dates, no playtime. |
| **RomM** | ROM library | Client API token | Last played, status and rating, no playtime. |
| **Playnite** | GOG, PlayStation, Xbox, Epic and more | JSON or CSV export | Manual or folder-drop. Coverage depends on the Playnite plugins you use. |

## Not built, and why

| Source | Reason |
|---|---|
| **Audible (direct)** | No official API, and BackTrackarr never stores an Amazon login. Use the file route below. |
| **Calibre, Calibre-Web** | No documented read-history API. Read status is a single flag, shared by all users in Calibre-Web. |
| **GOG** | No official public playtime API. Use Playnite. |
| **PlayStation, Xbox, Nintendo** | No official API for your play history. Unofficial ones break and can put an account at risk. Use Playnite where a plugin exists. |
| **IGDB** | Game metadata only, with no personal history. |

## Protection from rate limits and lockouts

Every outbound request goes through one throttle, including Trakt, Simkl,
TMDB, Plex.tv, AI providers and webhooks.

- Each site is paced to its published limit, or cautiously if it has none.
- A "slow down" reply (HTTP 429 or `Retry-After`) pauses that site for as long
  as it asks, and the wait grows if it keeps asking.
- Three sign-in failures in a row pause that site for 15 minutes, doubling up
  to 4 hours, so a wrong key can't get an account blocked.
- Pauses survive restarts. Queued plays wait and don't use up their retries.
- **Settings** lists paused sites, with **Resume now** for after you fix a key.
- Servers on your own network are not paced.

## Streaming services

Netflix, Hulu, Prime Video and the others have no official history API.
BackTrackarr doesn't store their passwords or sign in as you, because
automated logins break their terms and can get an account locked. Use the
browser scrobbler for live plays and their own history downloads for the past.
**Setup** links to both.

## Importing history

**Import** in the menu takes a file or ZIP and works out the format. It reads
Trakt and Simkl exports, Letterboxd, IMDb, Netflix and other streaming history,
and any list with a title plus a date or rating. Plays and ratings go into your
history. Watchlists, lists and comments are kept as raw records. Exports whose
exact fields aren't published are read by field name, so an unusual file may
need a small fix.

## Connected history imports

Connect these on **Setup**, then use **Import**. They only read, and refresh daily.

| App | Brings in | How |
|---|---|---|
| **Tautulli** | Finished Plex plays for every user | API key |
| **Floppy** | Movies, episodes and scores | API token, CSV export |
| **CrossWatch** | Latest history and ratings snapshots | API token |
| **Plekt** | Watch history | API key, or a `.json.gz` backup file |

These were built from each project's own source or docs and tested against fake
servers, so a new release of one of them may need a small fix.

## Auto-delete

**Off by default.** A file is deleted only when the master switch is on, its title
has a rule, and every user on that rule has watched it. It's removed through the
Sonarr, Radarr or Whisparr that owns it, and unmonitored first so it isn't
downloaded again.

- Scope it to chosen servers and Plex libraries, by include or exclude list.
- Only plays matched by a real id count. A same-named show, remake or play with
  no ids never unlocks a delete.
- A double episode is kept until everyone has watched both halves.
- Titles someone is watching are skipped, and each rule has a waiting period.
- At most 25 files are removed per run, and every deletion is logged.
- **Preview** shows what would go and deletes nothing.

## How counting works

- Every read, listen or play is its own row, and raw source data is kept.
  **Export (NDJSON)** on each page includes it.
- The same title in two sources stays as two rows. Stats count the title once,
  and count a read once if two sources logged it on the same day.
- A game's first sync records a **baseline** of lifetime playtime. Only later
  increases become sessions.
- Each source covers one account, assigned to `SHELF_USER`.
