CREATE TABLE IF NOT EXISTS media (
  id          INTEGER PRIMARY KEY,
  kind        TEXT    NOT NULL CHECK (kind IN ('movie','episode','track')),
  title       TEXT    NOT NULL,
  year        INTEGER,
  show_title  TEXT,
  album       TEXT,
  season      INTEGER,
  episode     INTEGER,
  duration_s  INTEGER,
  imdb        TEXT,
  tmdb        INTEGER,
  tvdb        INTEGER,
  trakt       INTEGER,
  mbid        TEXT,
  show_imdb   TEXT,
  show_tmdb   INTEGER,
  show_tvdb   INTEGER,
  show_trakt  INTEGER,
  plex_guid   TEXT,
  plex_key    TEXT,
  updated_at  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS media_plex_guid ON media(plex_guid) WHERE plex_guid IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_imdb  ON media(kind, imdb)  WHERE imdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_tmdb  ON media(kind, tmdb)  WHERE tmdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_tvdb  ON media(kind, tvdb)  WHERE tvdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_trakt ON media(kind, trakt) WHERE trakt IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_show_tvdb  ON media(show_tvdb, season, episode)  WHERE show_tvdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_show_tmdb  ON media(show_tmdb, season, episode)  WHERE show_tmdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_show_imdb  ON media(show_imdb, season, episode)  WHERE show_imdb  IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_show_trakt ON media(show_trakt, season, episode) WHERE show_trakt IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_show_title ON media(show_title, season, episode) WHERE show_title IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_title ON media(kind, title, year);
CREATE INDEX IF NOT EXISTS media_mbid  ON media(mbid) WHERE mbid IS NOT NULL;
CREATE INDEX IF NOT EXISTS media_track ON media(show_title, album, title) WHERE kind = 'track';

CREATE TABLE IF NOT EXISTS plays (
  id          INTEGER PRIMARY KEY,
  media_id    INTEGER NOT NULL REFERENCES media(id),
  user        TEXT    NOT NULL DEFAULT '',
  watched_at  INTEGER NOT NULL,
  source      TEXT    NOT NULL,
  player      TEXT,
  private     INTEGER NOT NULL DEFAULT 0, -- from a private (e.g. adult) library: kept locally, never sent out
  UNIQUE (media_id, user, watched_at)
);
CREATE INDEX IF NOT EXISTS plays_watched      ON plays(watched_at);
CREATE INDEX IF NOT EXISTS plays_user_watched ON plays(user, watched_at);

CREATE TABLE IF NOT EXISTS ratings (
  media_id  INTEGER NOT NULL REFERENCES media(id),
  user      TEXT    NOT NULL,
  rating    REAL    NOT NULL,
  rated_at  INTEGER NOT NULL,
  PRIMARY KEY (media_id, user)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS sessions (
  key         TEXT PRIMARY KEY,
  media_id    INTEGER NOT NULL,
  user        TEXT    NOT NULL,
  state       TEXT    NOT NULL,
  progress    REAL    NOT NULL DEFAULT 0,
  scrobbled   INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS outbox (
  id          INTEGER PRIMARY KEY,
  sink        TEXT    NOT NULL,
  action      TEXT    NOT NULL,
  payload     TEXT    NOT NULL,
  status      TEXT    NOT NULL DEFAULT 'pending',
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_at     INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  done_at     INTEGER,
  last_error  TEXT
);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(sink, id) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS outbox_done    ON outbox(done_at)  WHERE done_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS tokens (
  sink        TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT,
  expires_at  INTEGER
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
) WITHOUT ROWID;

-- ---------- Plex archive (read-only copy of the server's library) ----------
-- Every item Plex knows about, with the custom fields people edit by hand
-- (sort title, original title, edition, labels, collections, locks).
-- data holds the full metadata JSON, zlib-compressed.
CREATE TABLE IF NOT EXISTS plex_items (
  id             INTEGER PRIMARY KEY,
  server_id      TEXT    NOT NULL,
  rating_key     TEXT    NOT NULL,
  section_id     TEXT,
  section_title  TEXT,
  type           TEXT    NOT NULL,
  guid           TEXT,
  title          TEXT,
  title_sort     TEXT,
  original_title TEXT,
  edition        TEXT,
  year           INTEGER,
  parent_key     TEXT,
  parent_title   TEXT,
  grandparent_key   TEXT,
  grandparent_title TEXT,
  idx            INTEGER,
  parent_idx     INTEGER,
  user_rating    REAL,
  labels         TEXT,
  collections    TEXT,
  locked         TEXT,
  file           TEXT,
  media_id       INTEGER,
  added_at       INTEGER,
  plex_updated_at INTEGER,
  sig            TEXT,
  hash           TEXT,
  data           BLOB,
  seen_at        INTEGER NOT NULL,
  deleted_at     INTEGER,
  UNIQUE (server_id, rating_key)
);
CREATE INDEX IF NOT EXISTS plex_items_guid    ON plex_items(guid) WHERE guid IS NOT NULL;
CREATE INDEX IF NOT EXISTS plex_items_file    ON plex_items(file) WHERE file IS NOT NULL;
CREATE INDEX IF NOT EXISTS plex_items_section ON plex_items(server_id, section_id, type);
CREATE INDEX IF NOT EXISTS plex_items_title   ON plex_items(title);

-- Playlists (per user) and collections (per library).
CREATE TABLE IF NOT EXISTS plex_lists (
  id           INTEGER PRIMARY KEY,
  server_id    TEXT    NOT NULL,
  kind         TEXT    NOT NULL CHECK (kind IN ('playlist','collection')),
  owner        TEXT    NOT NULL DEFAULT '',
  rating_key   TEXT    NOT NULL,
  section_id   TEXT,
  title        TEXT    NOT NULL,
  list_type    TEXT,
  smart        INTEGER NOT NULL DEFAULT 0,
  content      TEXT,
  summary      TEXT,
  item_count   INTEGER NOT NULL DEFAULT 0,
  hash         TEXT,
  data         BLOB,
  seen_at      INTEGER NOT NULL,
  deleted_at   INTEGER,
  UNIQUE (server_id, kind, owner, rating_key)
);
CREATE TABLE IF NOT EXISTS plex_list_items (
  list_id     INTEGER NOT NULL REFERENCES plex_lists(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  rating_key  TEXT,
  guid        TEXT,
  type        TEXT,
  title       TEXT,
  file        TEXT,
  PRIMARY KEY (list_id, position)
) WITHOUT ROWID;

-- Every earlier version of an item or list, so a bad edit or a metadata
-- refresh never loses what was there before.
CREATE TABLE IF NOT EXISTS plex_versions (
  id          INTEGER PRIMARY KEY,
  kind        TEXT    NOT NULL,
  ref_id      INTEGER NOT NULL,
  captured_at INTEGER NOT NULL,
  hash        TEXT    NOT NULL,
  data        BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS plex_versions_ref ON plex_versions(kind, ref_id, captured_at);

-- ---------- comics (CLZ Comics exports) ----------
CREATE TABLE IF NOT EXISTS comics (
  id           INTEGER PRIMARY KEY,
  ext_key      TEXT    NOT NULL UNIQUE,
  series       TEXT,
  issue        TEXT,
  variant      TEXT,
  title        TEXT,
  publisher    TEXT,
  release_date TEXT,
  cover_date   TEXT,
  barcode      TEXT,
  format       TEXT,
  is_read      INTEGER NOT NULL DEFAULT 0,
  rating       REAL,
  raw          TEXT    NOT NULL,
  imported_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS comics_series ON comics(series, issue);
CREATE TABLE IF NOT EXISTS comic_reads (
  comic_id  INTEGER NOT NULL REFERENCES comics(id) ON DELETE CASCADE,
  read_at   INTEGER NOT NULL,
  source    TEXT    NOT NULL,
  PRIMARY KEY (comic_id, read_at)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS comic_reads_at ON comic_reads(read_at);

-- ---------- Sonarr / Radarr / Whisparr library snapshots ----------
CREATE TABLE IF NOT EXISTS arr_items (
  instance    TEXT    NOT NULL,
  arr_id      INTEGER NOT NULL,
  kind        TEXT    NOT NULL,
  title       TEXT,
  year        INTEGER,
  tvdb        INTEGER,
  tmdb        INTEGER,
  imdb        TEXT,
  monitored   INTEGER,
  path        TEXT,
  hash        TEXT,
  data        BLOB,
  seen_at     INTEGER NOT NULL,
  deleted_at  INTEGER,
  PRIMARY KEY (instance, arr_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS arr_items_tmdb ON arr_items(tmdb) WHERE tmdb IS NOT NULL;
CREATE INDEX IF NOT EXISTS arr_items_tvdb ON arr_items(tvdb) WHERE tvdb IS NOT NULL;
CREATE TABLE IF NOT EXISTS arr_config (
  instance    TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  hash        TEXT    NOT NULL,
  data        BLOB    NOT NULL,
  captured_at INTEGER NOT NULL,
  PRIMARY KEY (instance, name, captured_at)
) WITHOUT ROWID;

-- ---------- AI recommendations ----------
CREATE TABLE IF NOT EXISTS rec_runs (
  id          INTEGER PRIMARY KEY,
  created_at  INTEGER NOT NULL,
  user        TEXT    NOT NULL,
  provider    TEXT    NOT NULL,
  model       TEXT    NOT NULL,
  prompt      TEXT    NOT NULL,
  response    TEXT,
  error       TEXT
);
CREATE TABLE IF NOT EXISTS recs (
  id          INTEGER PRIMARY KEY,
  run_id      INTEGER NOT NULL REFERENCES rec_runs(id) ON DELETE CASCADE,
  kind        TEXT    NOT NULL,
  title       TEXT    NOT NULL,
  year        INTEGER,
  reason      TEXT,
  imdb        TEXT,
  tmdb        INTEGER,
  tvdb        INTEGER,
  in_plex     INTEGER NOT NULL DEFAULT 0,
  seen        INTEGER NOT NULL DEFAULT 0,
  added_to    TEXT,
  providers   TEXT
);
CREATE INDEX IF NOT EXISTS recs_run ON recs(run_id)
;

-- ---------- sign-in (sessions, MFA, passkeys, API tokens) ----------
-- Secrets that grant access (session ids, recovery codes, API tokens) are
-- stored only as SHA-256 hashes.
CREATE TABLE IF NOT EXISTS auth_sessions (
  id_hash    TEXT PRIMARY KEY,
  user       TEXT    NOT NULL,
  mfa_done   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  seen_at    INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  ip         TEXT,
  agent      TEXT
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS auth_totp (
  user       TEXT PRIMARY KEY,
  secret     TEXT    NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 0,
  last_step  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS auth_recovery (
  user      TEXT NOT NULL,
  code_hash TEXT NOT NULL,
  used_at   INTEGER,
  PRIMARY KEY (user, code_hash)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS auth_passkeys (
  id         TEXT PRIMARY KEY,
  user       TEXT    NOT NULL,
  name       TEXT    NOT NULL,
  rp_id      TEXT    NOT NULL,
  credential TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  last_used  INTEGER
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS auth_tokens (
  hash       TEXT PRIMARY KEY,
  user       TEXT    NOT NULL,
  name       TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  last_used  INTEGER
) WITHOUT ROWID
