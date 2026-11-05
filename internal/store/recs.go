package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ---------- taste profile (input to recommendations) ----------

type Rated struct {
	Kind   string  `json:"kind"`
	Title  string  `json:"title"`
	Year   int     `json:"year,omitempty"`
	Rating float64 `json:"rating,omitempty"`
	Plays  int64   `json:"plays,omitempty"`
}

// visibleClause hides plays from private libraries unless includePrivate.
const titleExpr = `CASE WHEN m.kind='episode' THEN m.show_title ELSE m.title END`

// RatedTitles returns a user's ratings (shows aggregated from episode ratings),
// best first when desc, worst first otherwise.
func (s *Store) RatedTitles(ctx context.Context, user string, desc bool, limit int) ([]Rated, error) {
	order := "ASC"
	if desc {
		order = "DESC"
	}
	return s.rated(ctx, `SELECT m.kind, `+titleExpr+`, COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0), AVG(r.rating), 0
		FROM ratings r JOIN media m ON m.id=r.media_id
		WHERE (?1='' OR r.user=?1) AND m.kind!='track'
		GROUP BY m.kind='movie', `+titleExpr+` ORDER BY AVG(r.rating) `+order+`, COUNT(*) DESC LIMIT ?2`, user, limit)
}

// MostPlayed returns the titles a user went back to most (rewatches and long
// series runs), excluding private plays unless includePrivate.
func (s *Store) MostPlayed(ctx context.Context, user string, includePrivate bool, limit int) ([]Rated, error) {
	return s.rated(ctx, `SELECT CASE WHEN m.kind='episode' THEN 'show' ELSE m.kind END, `+titleExpr+`,
		COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0), 0, COUNT(*) c
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE (?1='' OR p.user=?1) AND (?2 OR p.private=0) AND m.kind!='track'
		GROUP BY 1, 2 ORDER BY c DESC LIMIT ?3`, user, includePrivate, limit)
}

// RecentTitles returns what a user watched most recently.
func (s *Store) RecentTitles(ctx context.Context, user string, includePrivate bool, limit int) ([]Rated, error) {
	return s.rated(ctx, `SELECT CASE WHEN m.kind='episode' THEN 'show' ELSE m.kind END, `+titleExpr+`,
		COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0), 0, COUNT(*)
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE (?1='' OR p.user=?1) AND (?2 OR p.private=0) AND m.kind!='track' AND p.watched_at > ?3
		GROUP BY 1, 2 ORDER BY MAX(p.watched_at) DESC LIMIT ?4`, user, includePrivate, time.Now().AddDate(0, -6, 0).Unix(), limit)
}

// TopArtists returns a user's most played artists.
func (s *Store) TopArtistsFor(ctx context.Context, user string, limit int) ([]Rated, error) {
	return s.rated(ctx, `SELECT 'artist', m.show_title, 0, 0, COUNT(*) c FROM plays p JOIN media m ON m.id=p.media_id
		WHERE (?1='' OR p.user=?1) AND m.kind='track' AND m.show_title IS NOT NULL
		GROUP BY m.show_title ORDER BY c DESC LIMIT ?2`, user, limit)
}

func (s *Store) rated(ctx context.Context, q string, args ...any) ([]Rated, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rated
	for rows.Next() {
		var r Rated
		if err := rows.Scan(&r.Kind, &r.Title, &r.Year, &r.Rating, &r.Plays); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeenSet returns normalized "kind|title|year" keys (and "kind|title|" for any
// year) for everything a user has played, to filter recommendations.
func (s *Store) SeenSet(ctx context.Context, user string) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT CASE WHEN m.kind='episode' THEN 'show' ELSE m.kind END, `+titleExpr+`,
		COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0)
		FROM plays p JOIN media m ON m.id=p.media_id WHERE (?1='' OR p.user=?1) AND m.kind!='track'`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k, t string
		var y int
		if err := rows.Scan(&k, &t, &y); err != nil {
			return nil, err
		}
		out[k+"|"+NormTitle(t)+"|"] = true
		if y > 0 {
			out[k+"|"+NormTitle(t)+"|"+strconv.Itoa(y)] = true
		}
	}
	return out, rows.Err()
}

// InPlex reports whether a movie or show with this title (and year when
// known) is in the Plex archive.
func (s *Store) InPlex(ctx context.Context, kind, title string, year int) (bool, error) {
	typ := "movie"
	if kind == "show" {
		typ = "show"
	}
	var x int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM plex_items WHERE type=? AND deleted_at IS NULL
		AND (title=? COLLATE NOCASE OR original_title=? COLLATE NOCASE) AND (?=0 OR year IS NULL OR ABS(year-?)<=1) LIMIT 1`,
		typ, title, title, year, year).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ---------- recommendation runs ----------

type RecRun struct {
	ID        int64
	CreatedAt int64
	User      string
	Provider  string
	Model     string
	Prompt    string
	Response  string
	Error     string
	Recs      []Rec
}

type Rec struct {
	ID        int64    `json:"id"`
	RunID     int64    `json:"-"`
	Kind      string   `json:"kind"` // movie | show
	Title     string   `json:"title"`
	Year      int      `json:"year,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	IMDB      string   `json:"imdb,omitempty"`
	TMDB      int      `json:"tmdb,omitempty"`
	TVDB      int      `json:"tvdb,omitempty"`
	InPlex    bool     `json:"in_plex"`
	Seen      bool     `json:"seen"`
	AddedTo   string   `json:"added_to,omitempty"`
	Providers []string `json:"providers,omitempty"` // where it streams (TMDB, STREAMING_REGION)
}

func (s *Store) SaveRecRun(ctx context.Context, r *RecRun) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `INSERT INTO rec_runs(created_at,user,provider,model,prompt,response,error) VALUES(?,?,?,?,?,?,?) RETURNING id`,
		time.Now().Unix(), r.User, r.Provider, r.Model, r.Prompt, ns(r.Response), ns(r.Error)).Scan(&r.ID)
	if err != nil {
		return err
	}
	for i := range r.Recs {
		c := &r.Recs[i]
		if err := tx.QueryRowContext(ctx, `INSERT INTO recs(run_id,kind,title,year,reason,imdb,tmdb,tvdb,in_plex,seen,providers) VALUES(?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			r.ID, c.Kind, c.Title, ni(c.Year), ns(c.Reason), ns(c.IMDB), ni(c.TMDB), ni(c.TVDB), c.InPlex, c.Seen, jsonList(c.Providers)).Scan(&c.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecRuns returns the latest runs with their recommendations.
func (s *Store) RecRuns(ctx context.Context, limit int) ([]*RecRun, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, created_at, user, provider, model, COALESCE(error,'') FROM rec_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []*RecRun
	for rows.Next() {
		r := &RecRun{}
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.User, &r.Provider, &r.Model, &r.Error); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for _, r := range out {
		if r.Recs, err = s.recs(ctx, `WHERE run_id=? ORDER BY id`, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) GetRec(ctx context.Context, id int64) (*Rec, error) {
	rs, err := s.recs(ctx, `WHERE id=?`, id)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

func (s *Store) recs(ctx context.Context, where string, args ...any) ([]Rec, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, run_id, kind, title, COALESCE(year,0), COALESCE(reason,''), COALESCE(imdb,''),
		COALESCE(tmdb,0), COALESCE(tvdb,0), in_plex, seen, COALESCE(added_to,''), COALESCE(providers,'') FROM recs `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rec
	for rows.Next() {
		var c Rec
		var prov string
		if err := rows.Scan(&c.ID, &c.RunID, &c.Kind, &c.Title, &c.Year, &c.Reason, &c.IMDB, &c.TMDB, &c.TVDB, &c.InPlex, &c.Seen, &c.AddedTo, &prov); err != nil {
			return nil, err
		}
		c.Providers = parseList(prov)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) MarkRecAdded(ctx context.Context, id int64, instance string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE recs SET added_to = CASE WHEN added_to IS NULL THEN ? ELSE added_to || ', ' || ? END WHERE id=?`,
		instance, instance, id)
	return err
}

// KV helpers for small bits of state (checkpoints, last run times).
func (s *Store) GetKV(ctx context.Context, k string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetKV(ctx context.Context, k, v string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ArchiveRated returns movies and shows the Plex owner rated in Plex (from
// the archive), best first when desc.
func (s *Store) ArchiveRated(ctx context.Context, desc bool, limit int) ([]Rated, error) {
	order := "ASC"
	if desc {
		order = "DESC"
	}
	return s.rated(ctx, `SELECT type, title, COALESCE(year,0), MAX(user_rating), 0 FROM plex_items
		WHERE type IN ('movie','show') AND user_rating > 0 AND deleted_at IS NULL
		GROUP BY type, title, year ORDER BY MAX(user_rating) `+order+` LIMIT ?`, limit)
}

// SourceCounts returns how many plays came from each source (plex-webhook,
// netflix-import, disneyplus, trakt-pull...).
func (s *Store) SourceCounts(ctx context.Context) ([]NameCount, error) {
	return s.nameCounts(ctx, `SELECT source, COUNT(*) c FROM plays GROUP BY source ORDER BY c DESC`)
}
