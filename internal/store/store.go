// Package store is the SQLite persistence layer.
//
// SQLite in WAL mode handles decades of personal watch history easily:
// 50 years x 20 plays/day is ~365k rows, and SQLite stays fast into the
// hundreds of millions. It also needs no separate server, which keeps
// memory and CPU use low. Backups are one file (see Backup).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure Go, no CGO

	"github.com/DrSeussFreak/BacklotTracker/internal/model"
)

//go:embed schema.sql
var schema string

// Two plays of the same item by the same user within this window are treated
// as one play (covers duplicates between webhook, poller, and imports).
const dedupeWindow = 3 * 60 * 60

type Store struct {
	DB  *sql.DB
	dir string

	// gen counts writes to plays and media. Stats results are cached per user
	// and reused until gen changes, so dashboard loads stay cheap on large
	// histories.
	gen        atomic.Int64
	statsMu    sync.Mutex
	statsCache map[string]cachedStats
}

type cachedStats struct {
	gen  int64
	year int
	st   *Stats
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "backlottracker.db")
	migrateOldName(dir, path)
	dsn := "file:" + path + "?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)&_pragma=cache_size(-8000)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, stmt := range strings.Split(schema, ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("schema: %w\n%s", err, stmt)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, dir: dir}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func ns(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func ni(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func epNum(m *model.Media, v int) any {
	if m.Kind == model.Track && v == 0 {
		return nil
	}
	if m.Kind == model.Movie {
		return nil
	}
	return v
}

// ---------- media ----------

const mediaCols = `m.id, m.kind, m.title, COALESCE(m.year,0), COALESCE(m.show_title,''), COALESCE(m.album,''), COALESCE(m.season,0), COALESCE(m.episode,0), COALESCE(m.duration_s,0),
 COALESCE(m.imdb,''), COALESCE(m.tmdb,0), COALESCE(m.tvdb,0), COALESCE(m.trakt,0), COALESCE(m.mbid,''),
 COALESCE(m.show_imdb,''), COALESCE(m.show_tmdb,0), COALESCE(m.show_tvdb,0), COALESCE(m.show_trakt,0),
 COALESCE(m.plex_guid,''), COALESCE(m.plex_key,'')`

func mediaDest(m *model.Media) []any {
	return []any{&m.ID, &m.Kind, &m.Title, &m.Year, &m.ShowTitle, &m.Album, &m.Season, &m.Episode, &m.DurationS,
		&m.IDs.IMDB, &m.IDs.TMDB, &m.IDs.TVDB, &m.IDs.Trakt, &m.IDs.MBID,
		&m.ShowIDs.IMDB, &m.ShowIDs.TMDB, &m.ShowIDs.TVDB, &m.ShowIDs.Trakt,
		&m.PlexGUID, &m.PlexKey}
}

func (s *Store) GetMedia(ctx context.Context, id int64) (model.Media, error) {
	var m model.Media
	err := s.DB.QueryRowContext(ctx, `SELECT `+mediaCols+` FROM media m WHERE m.id=?`, id).Scan(mediaDest(&m)...)
	return m, err
}

type candidate struct {
	ok   bool
	q    string
	args []any
}

// findID locates an existing media row matching m, trying the most specific
// identifiers first. Returns 0 if none matches.
func (s *Store) findID(ctx context.Context, m *model.Media) (int64, error) {
	cs := []candidate{{m.PlexGUID != "", `SELECT id FROM media WHERE plex_guid=?`, []any{m.PlexGUID}}}
	switch m.Kind {
	case model.Track:
		cs = append(cs,
			candidate{m.IDs.MBID != "", `SELECT id FROM media WHERE kind='track' AND mbid=? LIMIT 1`, []any{m.IDs.MBID}},
			candidate{m.Title != "", `SELECT id FROM media WHERE kind='track' AND show_title IS ? AND album IS ? AND title=? LIMIT 1`, []any{ns(m.ShowTitle), ns(m.Album), m.Title}},
		)
	case model.Movie:
		cs = append(cs,
			candidate{m.IDs.IMDB != "", `SELECT id FROM media WHERE kind='movie' AND imdb=? LIMIT 1`, []any{m.IDs.IMDB}},
			candidate{m.IDs.TMDB != 0, `SELECT id FROM media WHERE kind='movie' AND tmdb=? LIMIT 1`, []any{m.IDs.TMDB}},
			candidate{m.IDs.Trakt != 0, `SELECT id FROM media WHERE kind='movie' AND trakt=? LIMIT 1`, []any{m.IDs.Trakt}},
			candidate{m.IDs.Empty() && m.Title != "", `SELECT id FROM media WHERE kind='movie' AND title=? AND COALESCE(year,0)=? LIMIT 1`, []any{m.Title, m.Year}},
		)
	default:
		se := func(col string, v any) candidate {
			return candidate{true, `SELECT id FROM media WHERE kind='episode' AND ` + col + `=? AND season=? AND episode=? LIMIT 1`, []any{v, m.Season, m.Episode}}
		}
		if m.ShowIDs.TVDB != 0 {
			cs = append(cs, se("show_tvdb", m.ShowIDs.TVDB))
		}
		if m.ShowIDs.TMDB != 0 {
			cs = append(cs, se("show_tmdb", m.ShowIDs.TMDB))
		}
		if m.ShowIDs.IMDB != "" {
			cs = append(cs, se("show_imdb", m.ShowIDs.IMDB))
		}
		if m.ShowIDs.Trakt != 0 {
			cs = append(cs, se("show_trakt", m.ShowIDs.Trakt))
		}
		cs = append(cs,
			candidate{m.IDs.TVDB != 0, `SELECT id FROM media WHERE kind='episode' AND tvdb=? LIMIT 1`, []any{m.IDs.TVDB}},
			candidate{m.IDs.IMDB != "", `SELECT id FROM media WHERE kind='episode' AND imdb=? LIMIT 1`, []any{m.IDs.IMDB}},
			candidate{m.IDs.TMDB != 0, `SELECT id FROM media WHERE kind='episode' AND tmdb=? LIMIT 1`, []any{m.IDs.TMDB}},
			candidate{m.IDs.Trakt != 0, `SELECT id FROM media WHERE kind='episode' AND trakt=? LIMIT 1`, []any{m.IDs.Trakt}},
			candidate{m.IDs.Empty() && m.ShowIDs.Empty() && m.ShowTitle != "", `SELECT id FROM media WHERE kind='episode' AND show_title=? AND season=? AND episode=? LIMIT 1`, []any{m.ShowTitle, m.Season, m.Episode}},
		)
	}
	for _, c := range cs {
		if !c.ok {
			continue
		}
		var id int64
		err := s.DB.QueryRowContext(ctx, c.q, c.args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return id, nil
	}
	return 0, nil
}

// FindMedia returns the stored media matching m, if any.
func (s *Store) FindMedia(ctx context.Context, m model.Media) (model.Media, bool, error) {
	id, err := s.findID(ctx, &m)
	if err != nil || id == 0 {
		return model.Media{}, false, err
	}
	got, err := s.GetMedia(ctx, id)
	return got, err == nil, err
}

// UpsertMedia finds or creates m, fills in any newly known identifiers, and
// loads the merged row back into m (so m.ID is set).
func (s *Store) UpsertMedia(ctx context.Context, m *model.Media) error {
	if m.Kind != model.Movie && m.Kind != model.Episode && m.Kind != model.Track {
		return fmt.Errorf("unsupported media kind %q", m.Kind)
	}
	id, err := s.findID(ctx, m)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if id == 0 {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO media
			(kind,title,year,show_title,album,season,episode,duration_s,imdb,tmdb,tvdb,trakt,mbid,show_imdb,show_tmdb,show_tvdb,show_trakt,plex_guid,plex_key,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			m.Kind, m.Title, ni(m.Year), ns(m.ShowTitle), ns(m.Album), epNum(m, m.Season), epNum(m, m.Episode), ni(m.DurationS),
			ns(m.IDs.IMDB), ni(m.IDs.TMDB), ni(m.IDs.TVDB), ni(m.IDs.Trakt), ns(m.IDs.MBID),
			ns(m.ShowIDs.IMDB), ni(m.ShowIDs.TMDB), ni(m.ShowIDs.TVDB), ni(m.ShowIDs.Trakt),
			ns(m.PlexGUID), ns(m.PlexKey), now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
	} else {
		args := []any{m.Title, ni(m.Year), ns(m.ShowTitle), ns(m.Album), ni(m.DurationS),
			ns(m.IDs.IMDB), ni(m.IDs.TMDB), ni(m.IDs.TVDB), ni(m.IDs.Trakt), ns(m.IDs.MBID),
			ns(m.ShowIDs.IMDB), ni(m.ShowIDs.TMDB), ni(m.ShowIDs.TVDB), ni(m.ShowIDs.Trakt),
			ns(m.PlexGUID), ns(m.PlexKey), now, id}
		const q = `UPDATE media SET
			title=COALESCE(NULLIF(title,''),?), year=COALESCE(year,?), show_title=COALESCE(show_title,?), album=COALESCE(album,?), duration_s=COALESCE(duration_s,?),
			imdb=COALESCE(imdb,?), tmdb=COALESCE(tmdb,?), tvdb=COALESCE(tvdb,?), trakt=COALESCE(trakt,?), mbid=COALESCE(mbid,?),
			show_imdb=COALESCE(show_imdb,?), show_tmdb=COALESCE(show_tmdb,?), show_tvdb=COALESCE(show_tvdb,?), show_trakt=COALESCE(show_trakt,?),
			plex_guid=COALESCE(plex_guid,?), plex_key=COALESCE(?,plex_key), updated_at=? WHERE id=?`
		_, err = s.DB.ExecContext(ctx, q, args...)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			args[14] = nil // plex guid already belongs to another row; keep this one as is
			_, err = s.DB.ExecContext(ctx, q, args...)
		}
		if err != nil {
			return err
		}
	}
	s.gen.Add(1)
	got, err := s.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	*m = got
	return nil
}

// ---------- plays ----------

// RecordPlay stores a completed play. It returns false if an equivalent play
// already exists (same item and user within the dedupe window).
func (s *Store) RecordPlay(ctx context.Context, mediaID int64, user string, at time.Time, source, player string, private bool) (bool, error) {
	ts := at.Unix()
	var x int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM plays WHERE media_id=? AND user=? AND watched_at BETWEEN ? AND ? LIMIT 1`,
		mediaID, user, ts-dedupeWindow, ts+dedupeWindow).Scan(&x)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO plays(media_id,user,watched_at,source,player,private) VALUES(?,?,?,?,?,?)`,
		mediaID, user, ts, source, ns(player), private)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.gen.Add(1)
	}
	return n > 0, nil
}

func (s *Store) DeletePlay(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM plays WHERE id=?`, id)
	s.gen.Add(1)
	return err
}

type PlayRow struct {
	ID        int64       `json:"id"`
	WatchedAt int64       `json:"watched_at"`
	User      string      `json:"user"`
	Source    string      `json:"source"`
	Player    string      `json:"player,omitempty"`
	Private   bool        `json:"private,omitempty"`
	Media     model.Media `json:"media"`
}

const playCols = `p.id, p.watched_at, p.user, p.source, COALESCE(p.player,''), p.private, ` + mediaCols

func scanPlays(rows *sql.Rows) ([]PlayRow, error) {
	defer rows.Close()
	var out []PlayRow
	for rows.Next() {
		var p PlayRow
		dest := append([]any{&p.ID, &p.WatchedAt, &p.User, &p.Source, &p.Player, &p.Private}, mediaDest(&p.Media)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type HistoryQuery struct {
	User, Kind, Search string
	BeforeTS, BeforeID int64
	Limit              int
}

// History returns plays newest first, using keyset pagination so it stays
// fast no matter how deep into the decades you page.
func (s *Store) History(ctx context.Context, q HistoryQuery) ([]PlayRow, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	like := ""
	if q.Search != "" {
		like = "%" + q.Search + "%"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+playCols+`
		FROM plays p JOIN media m ON m.id = p.media_id
		WHERE (?1 = 0 OR p.watched_at < ?1 OR (p.watched_at = ?1 AND p.id < ?2))
		  AND (?3 = '' OR p.user = ?3)
		  AND (?4 = '' OR m.kind = ?4)
		  AND (?5 = '' OR m.title LIKE ?5 OR m.show_title LIKE ?5)
		ORDER BY p.watched_at DESC, p.id DESC LIMIT ?6`,
		q.BeforeTS, q.BeforeID, q.User, q.Kind, like, q.Limit)
	if err != nil {
		return nil, err
	}
	return scanPlays(rows)
}

// PlaysAfter returns a user's plays with id > afterID, oldest id first.
func (s *Store) PlaysAfter(ctx context.Context, user string, afterID int64, limit int) ([]PlayRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+playCols+`
		FROM plays p JOIN media m ON m.id = p.media_id
		WHERE p.user = ? AND p.id > ? ORDER BY p.id LIMIT ?`, user, afterID, limit)
	if err != nil {
		return nil, err
	}
	return scanPlays(rows)
}

// ExportPlays streams every play, oldest first.
func (s *Store) ExportPlays(ctx context.Context, fn func(PlayRow) error) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+playCols+`
		FROM plays p JOIN media m ON m.id = p.media_id ORDER BY p.watched_at, p.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p PlayRow
		dest := append([]any{&p.ID, &p.WatchedAt, &p.User, &p.Source, &p.Player, &p.Private}, mediaDest(&p.Media)...)
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return rows.Err()
}

type NameCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

func (s *Store) Users(ctx context.Context) ([]NameCount, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT user, COUNT(*) FROM plays GROUP BY user ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var n NameCount
		if err := rows.Scan(&n.Name, &n.Count); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------- stats ----------
type YearCount struct {
	Year     int   `json:"year"`
	Plays    int64 `json:"plays"`
	Movies   int64 `json:"movies"`
	Episodes int64 `json:"episodes"`
	Tracks   int64 `json:"tracks"`
	Seconds  int64 `json:"seconds"`
}

type Stats struct {
	Plays         int64       `json:"plays"` // movies + episodes
	Movies        int64       `json:"movies"`
	Episodes      int64       `json:"episodes"`
	Shows         int64       `json:"shows"`
	Seconds       int64       `json:"seconds"` // time watched (video)
	Tracks        int64       `json:"tracks"`
	Artists       int64       `json:"artists"`
	ListenSeconds int64       `json:"listen_seconds"`
	ThisYear      int64       `json:"this_year"`
	Years         []YearCount `json:"years"`
	MaxYear       int64       `json:"-"`
	TopShows      []NameCount `json:"top_shows"`
	TopMovies     []NameCount `json:"top_movies"`
	TopArtists    []NameCount `json:"top_artists"`
	Rewatched     []NameCount `json:"rewatched"`
}

// Stats returns totals for one user (or everyone when user is empty). Results
// are cached until the next write to plays or media. Callers must not modify
// the returned value.
func (s *Store) Stats(ctx context.Context, user string) (*Stats, error) {
	gen, year := s.gen.Load(), time.Now().Year()
	s.statsMu.Lock()
	c, ok := s.statsCache[user]
	s.statsMu.Unlock()
	if ok && c.gen == gen && c.year == year {
		return c.st, nil
	}
	st, err := s.computeStats(ctx, user)
	if err != nil {
		return nil, err
	}
	s.statsMu.Lock()
	if s.statsCache == nil || len(s.statsCache) > 64 {
		s.statsCache = map[string]cachedStats{}
	}
	s.statsCache[user] = cachedStats{gen: gen, year: year, st: st}
	s.statsMu.Unlock()
	return st, nil
}

func (s *Store) computeStats(ctx context.Context, user string) (*Stats, error) {
	st := &Stats{}
	err := s.DB.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(m.kind='movie'),0), COALESCE(SUM(m.kind='episode'),0), COALESCE(SUM(m.kind='track'),0),
		COALESCE(SUM(CASE WHEN m.kind!='track' THEN m.duration_s END),0),
		COALESCE(SUM(CASE WHEN m.kind='track' THEN m.duration_s END),0),
		COUNT(DISTINCT CASE WHEN m.kind='episode' THEN COALESCE(m.show_tvdb, m.show_tmdb, m.show_title) END),
		COUNT(DISTINCT CASE WHEN m.kind='track' THEN m.show_title END)
		FROM plays p JOIN media m ON m.id=p.media_id WHERE (?1='' OR p.user=?1)`, user).
		Scan(&st.Movies, &st.Episodes, &st.Tracks, &st.Seconds, &st.ListenSeconds, &st.Shows, &st.Artists)
	if err != nil {
		return nil, err
	}
	st.Plays = st.Movies + st.Episodes
	yearStart := time.Date(time.Now().Year(), 1, 1, 0, 0, 0, 0, time.Local).Unix()
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM plays p JOIN media m ON m.id=p.media_id
		WHERE p.watched_at >= ?2 AND m.kind!='track' AND (?1='' OR p.user=?1)`, user, yearStart).Scan(&st.ThisYear); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT CAST(strftime('%Y', p.watched_at, 'unixepoch') AS INTEGER) y,
		COALESCE(SUM(m.kind='movie'),0), COALESCE(SUM(m.kind='episode'),0), COALESCE(SUM(m.kind='track'),0),
		COALESCE(SUM(CASE WHEN m.kind!='track' THEN m.duration_s END),0)
		FROM plays p JOIN media m ON m.id=p.media_id WHERE (?1='' OR p.user=?1) GROUP BY y ORDER BY y DESC`, user)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var y YearCount
		if err := rows.Scan(&y.Year, &y.Movies, &y.Episodes, &y.Tracks, &y.Seconds); err != nil {
			rows.Close()
			return nil, err
		}
		y.Plays = y.Movies + y.Episodes
		if y.Plays > st.MaxYear {
			st.MaxYear = y.Plays
		}
		st.Years = append(st.Years, y)
	}
	rows.Close()
	if st.TopShows, err = s.nameCounts(ctx, `SELECT m.show_title, COUNT(*) c FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind='episode' AND (?1='' OR p.user=?1)
		GROUP BY COALESCE(m.show_tvdb, m.show_tmdb, m.show_title) ORDER BY c DESC LIMIT 10`, user); err != nil {
		return nil, err
	}
	if st.TopMovies, err = s.nameCounts(ctx, `SELECT m.title || CASE WHEN m.year IS NULL THEN '' ELSE ' (' || m.year || ')' END, COUNT(*) c
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind='movie' AND (?1='' OR p.user=?1) GROUP BY m.id ORDER BY c DESC, m.title LIMIT 10`, user); err != nil {
		return nil, err
	}
	if st.TopArtists, err = s.nameCounts(ctx, `SELECT m.show_title, COUNT(*) c FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind='track' AND m.show_title IS NOT NULL AND (?1='' OR p.user=?1)
		GROUP BY m.show_title ORDER BY c DESC LIMIT 10`, user); err != nil {
		return nil, err
	}
	// Rewatches: video items played more than once by the same user.
	if st.Rewatched, err = s.nameCounts(ctx, `SELECT CASE WHEN m.kind='episode'
			THEN m.show_title || ' · S' || printf('%02d', m.season) || 'E' || printf('%02d', m.episode) ELSE m.title END, COUNT(*) c
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind!='track' AND (?1='' OR p.user=?1) GROUP BY m.id, p.user HAVING c > 1 ORDER BY c DESC LIMIT 10`, user); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Store) nameCounts(ctx context.Context, q string, args ...any) ([]NameCount, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var n NameCount
		if err := rows.Scan(&n.Name, &n.Count); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------- ratings ----------

func (s *Store) SetRating(ctx context.Context, mediaID int64, user string, rating float64, at time.Time) error {
	if rating <= 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM ratings WHERE media_id=? AND user=?`, mediaID, user)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO ratings(media_id,user,rating,rated_at) VALUES(?,?,?,?)
		ON CONFLICT(media_id,user) DO UPDATE SET rating=excluded.rating, rated_at=excluded.rated_at`,
		mediaID, user, rating, at.Unix())
	return err
}

// ---------- playback sessions ----------

type Session struct {
	Key       string
	MediaID   int64
	User      string
	State     string
	Progress  float64
	Scrobbled bool
	UpdatedAt time.Time
}

func (s *Store) GetSession(ctx context.Context, key string) (*Session, error) {
	var ss Session
	var upd int64
	err := s.DB.QueryRowContext(ctx, `SELECT key, media_id, user, state, progress, scrobbled, updated_at FROM sessions WHERE key=?`, key).
		Scan(&ss.Key, &ss.MediaID, &ss.User, &ss.State, &ss.Progress, &ss.Scrobbled, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ss.UpdatedAt = time.Unix(upd, 0)
	return &ss, nil
}

func (s *Store) PutSession(ctx context.Context, ss Session) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions(key,media_id,user,state,progress,scrobbled,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET media_id=excluded.media_id, user=excluded.user, state=excluded.state,
		progress=excluded.progress, scrobbled=excluded.scrobbled, updated_at=excluded.updated_at`,
		ss.Key, ss.MediaID, ss.User, ss.State, ss.Progress, ss.Scrobbled, time.Now().Unix())
	return err
}

func (s *Store) DeleteSession(ctx context.Context, key string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE key=?`, key)
	return err
}

// ---------- outbox (durable per-service delivery queue) ----------

type OutboxRow struct {
	ID        int64
	Sink      string
	Action    string
	Payload   []byte
	Attempts  int
	NextAt    int64
	CreatedAt int64
}

func (s *Store) Enqueue(ctx context.Context, sink, action string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO outbox(sink,action,payload,next_at,created_at) VALUES(?,?,?,?,?)`,
		sink, action, string(b), now, now)
	return err
}

// NextOutbox returns the oldest undelivered row for a sink (delivery is
// strictly ordered per sink), or nil.
func (s *Store) NextOutbox(ctx context.Context, sink string) (*OutboxRow, error) {
	var r OutboxRow
	var payload string
	err := s.DB.QueryRowContext(ctx, `SELECT id, sink, action, payload, attempts, next_at, created_at
		FROM outbox WHERE sink=? AND done_at IS NULL ORDER BY id LIMIT 1`, sink).
		Scan(&r.ID, &r.Sink, &r.Action, &payload, &r.Attempts, &r.NextAt, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Payload = []byte(payload)
	return &r, nil
}

func (s *Store) FinishOutbox(ctx context.Context, id int64, status, msg string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox SET status=?, done_at=?, last_error=? WHERE id=?`,
		status, time.Now().Unix(), ns(msg), id)
	return err
}

func (s *Store) RetryOutbox(ctx context.Context, id int64, attempts int, next time.Time, msg string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox SET attempts=?, next_at=?, last_error=? WHERE id=?`,
		attempts, next.Unix(), msg, id)
	return err
}

func (s *Store) RetryFailed(ctx context.Context, sink string) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE outbox SET status='pending', done_at=NULL, attempts=0, next_at=? WHERE sink=? AND status='failed'`,
		time.Now().Unix(), sink)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type SinkStats struct {
	Pending   int64
	Failed    int64
	Sent      int64
	LastOK    int64
	LastError string
}

func (s *Store) OutboxStats(ctx context.Context) (map[string]*SinkStats, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT sink, COALESCE(SUM(done_at IS NULL),0), COALESCE(SUM(status='failed'),0),
		COALESCE(SUM(status='ok'),0), COALESCE(MAX(CASE WHEN status='ok' THEN done_at END),0) FROM outbox GROUP BY sink`)
	if err != nil {
		return nil, err
	}
	out := map[string]*SinkStats{}
	for rows.Next() {
		var name string
		st := &SinkStats{}
		if err := rows.Scan(&name, &st.Pending, &st.Failed, &st.Sent, &st.LastOK); err != nil {
			rows.Close()
			return nil, err
		}
		out[name] = st
	}
	rows.Close()
	for name, st := range out {
		_ = s.DB.QueryRowContext(ctx, `SELECT COALESCE(last_error,'') FROM outbox WHERE sink=? AND last_error IS NOT NULL ORDER BY id DESC LIMIT 1`, name).Scan(&st.LastError)
	}
	return out, nil
}

// Maintain prunes delivered queue rows and stale sessions, and lets SQLite
// refresh its query planner statistics.
func (s *Store) Maintain(ctx context.Context) error {
	now := time.Now()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM outbox WHERE done_at IS NOT NULL AND status IN ('ok','dropped') AND done_at < ?`, now.Add(-30*24*time.Hour).Unix()); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE updated_at < ?`, now.Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// ---------- tokens / kv ----------

type Token struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

func (s *Store) GetToken(ctx context.Context, sink string) (*Token, error) {
	var t Token
	var exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT access, COALESCE(refresh,''), COALESCE(expires_at,0) FROM tokens WHERE sink=?`, sink).
		Scan(&t.Access, &t.Refresh, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if exp > 0 {
		t.ExpiresAt = time.Unix(exp, 0)
	}
	return &t, nil
}

func (s *Store) SetToken(ctx context.Context, sink string, t Token) error {
	var exp any
	if !t.ExpiresAt.IsZero() {
		exp = t.ExpiresAt.Unix()
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO tokens(sink,access,refresh,expires_at) VALUES(?,?,?,?)
		ON CONFLICT(sink) DO UPDATE SET access=excluded.access, refresh=excluded.refresh, expires_at=excluded.expires_at`,
		sink, t.Access, ns(t.Refresh), exp)
	return err
}

func (s *Store) DeleteToken(ctx context.Context, sink string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM tokens WHERE sink=?`, sink)
	return err
}

// EnsureSecret returns a stored random secret, generating it on first use.
func (s *Store) EnsureSecret(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, key).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	v = hex.EncodeToString(b)
	_, err = s.DB.ExecContext(ctx, `INSERT INTO kv(k,v) VALUES(?,?)`, key, v)
	return v, err
}

// ---------- backups ----------

// Backup writes a consistent, compacted copy of the database to
// <data>/backups and keeps only the newest `keep` copies.
func (s *Store) Backup(ctx context.Context, keep int) (string, error) {
	dir := filepath.Join(s.dir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := filepath.Join(dir, "backlottracker-"+time.Now().Format("20060102-150405")+".db")
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, name); err != nil {
		return "", err
	}
	if keep > 0 {
		files, _ := filepath.Glob(filepath.Join(dir, "backlottracker-*.db"))
		sort.Strings(files)
		for len(files) > keep {
			os.Remove(files[0])
			files = files[1:]
		}
	}
	return name, nil
}

// migrateOldName moves a database created under the app's earlier name
// (reelog.db, with its WAL and shared-memory files) to the current name, so
// upgrading keeps all data. It does nothing if the new file already exists.
func migrateOldName(dir, path string) {
	old := filepath.Join(dir, "reelog.db")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if _, err := os.Stat(old); err != nil {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(old + suffix); err == nil {
			_ = os.Rename(old+suffix, path+suffix)
		}
	}
}
