package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Comic is one issue from a CLZ Comics export. Raw keeps every column of the
// export, so nothing is lost even for fields BackTrackarr doesn't know about.
type Comic struct {
	ID          int64             `json:"id"`
	ExtKey      string            `json:"ext_key"`
	Series      string            `json:"series,omitempty"`
	Issue       string            `json:"issue,omitempty"`
	Variant     string            `json:"variant,omitempty"`
	Title       string            `json:"title,omitempty"`
	Publisher   string            `json:"publisher,omitempty"`
	ReleaseDate string            `json:"release_date,omitempty"`
	CoverDate   string            `json:"cover_date,omitempty"`
	Barcode     string            `json:"barcode,omitempty"`
	Format      string            `json:"format,omitempty"`
	Read        bool              `json:"read"`
	Rating      float64           `json:"rating,omitempty"`
	Raw         map[string]string `json:"raw"`
	Reads       int64             `json:"reads"`
	LastRead    int64             `json:"last_read,omitempty"`
}

// UpsertComic inserts or updates one comic by its export key. It reports
// whether the comic was new.
func (s *Store) UpsertComic(ctx context.Context, c *Comic) (bool, error) {
	raw, _ := json.Marshal(c.Raw)
	var id int64
	var oldRaw string
	err := s.DB.QueryRowContext(ctx, `SELECT id, raw FROM comics WHERE ext_key=?`, c.ExtKey).Scan(&id, &oldRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if id != 0 {
		// A later export may have fewer columns: keep what earlier ones had.
		merged := map[string]string{}
		_ = json.Unmarshal([]byte(oldRaw), &merged)
		for k, v := range c.Raw {
			merged[k] = v
		}
		c.Raw = merged
		raw, _ = json.Marshal(merged)
	}
	isNew := id == 0
	err = s.DB.QueryRowContext(ctx, `INSERT INTO comics(ext_key,series,issue,variant,title,publisher,release_date,cover_date,
		barcode,format,is_read,rating,raw,imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ext_key) DO UPDATE SET series=COALESCE(excluded.series, comics.series),
		 issue=COALESCE(excluded.issue, comics.issue), variant=COALESCE(excluded.variant, comics.variant),
		 title=COALESCE(excluded.title, comics.title), publisher=COALESCE(excluded.publisher, comics.publisher),
		 release_date=COALESCE(excluded.release_date, comics.release_date), cover_date=COALESCE(excluded.cover_date, comics.cover_date),
		 barcode=COALESCE(excluded.barcode, comics.barcode), format=COALESCE(excluded.format, comics.format), is_read=MAX(comics.is_read, excluded.is_read),
		 rating=COALESCE(excluded.rating, comics.rating), raw=excluded.raw, imported_at=excluded.imported_at
		RETURNING id`,
		c.ExtKey, ns(c.Series), ns(c.Issue), ns(c.Variant), ns(c.Title), ns(c.Publisher), ns(c.ReleaseDate), ns(c.CoverDate),
		ns(c.Barcode), ns(c.Format), c.Read, nf(c.Rating), string(raw), time.Now().Unix()).Scan(&id)
	c.ID = id
	return isNew, err
}

// AddComicRead records one read. Rereads (a new read date on a later import)
// are kept as separate rows; the same date twice is ignored.
func (s *Store) AddComicRead(ctx context.Context, comicID int64, at time.Time, source string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO comic_reads(comic_id,read_at,source) VALUES(?,?,?)`,
		comicID, at.Unix(), source)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type ComicQuery struct {
	Search   string
	ReadOnly bool
	Offset   int
	Limit    int
}

func (s *Store) Comics(ctx context.Context, q ComicQuery) ([]Comic, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	like := ""
	if q.Search != "" {
		like = "%" + q.Search + "%"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, c.ext_key, COALESCE(series,''), COALESCE(issue,''), COALESCE(variant,''),
		COALESCE(title,''), COALESCE(publisher,''), COALESCE(release_date,''), COALESCE(cover_date,''), COALESCE(barcode,''),
		COALESCE(format,''), is_read, COALESCE(rating,0), raw,
		(SELECT COUNT(*) FROM comic_reads r WHERE r.comic_id=c.id), (SELECT COALESCE(MAX(read_at),0) FROM comic_reads r WHERE r.comic_id=c.id)
		FROM comics c
		WHERE (?1='' OR series LIKE ?1 OR title LIKE ?1 OR publisher LIKE ?1 OR barcode LIKE ?1) AND (?2=0 OR is_read=1)
		ORDER BY series COLLATE NOCASE, CAST(issue AS REAL), issue, variant LIMIT ?3 OFFSET ?4`,
		like, q.ReadOnly, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comic
	for rows.Next() {
		var c Comic
		var raw string
		if err := rows.Scan(&c.ID, &c.ExtKey, &c.Series, &c.Issue, &c.Variant, &c.Title, &c.Publisher, &c.ReleaseDate,
			&c.CoverDate, &c.Barcode, &c.Format, &c.Read, &c.Rating, &raw, &c.Reads, &c.LastRead); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &c.Raw)
		out = append(out, c)
	}
	return out, rows.Err()
}

type ComicStats struct {
	Comics     int64       `json:"comics"`
	Series     int64       `json:"series"`
	Read       int64       `json:"read"`
	Reads      int64       `json:"reads"`
	Rereads    int64       `json:"rereads"`
	Publishers []NameCount `json:"publishers"`
	ReadByYear []NameCount `json:"read_by_year"`
}

func (s *Store) ComicStats(ctx context.Context) (*ComicStats, error) {
	st := &ComicStats{}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT series), COALESCE(SUM(is_read),0) FROM comics`).
		Scan(&st.Comics, &st.Series, &st.Read); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(n-1),0) FROM (SELECT COUNT(*) n FROM comic_reads GROUP BY comic_id)`).
		Scan(&st.Reads, &st.Rereads); err != nil {
		return nil, err
	}
	// Reads holds the number of comics with at least one dated read here; turn it into total reads.
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM comic_reads`).Scan(&st.Reads); err != nil {
		return nil, err
	}
	var err error
	if st.Publishers, err = s.nameCounts(ctx, `SELECT publisher, COUNT(*) c FROM comics WHERE publisher IS NOT NULL GROUP BY 1 ORDER BY c DESC LIMIT 10`); err != nil {
		return nil, err
	}
	if st.ReadByYear, err = s.nameCounts(ctx, `SELECT strftime('%Y', read_at, 'unixepoch'), COUNT(*) FROM comic_reads WHERE read_at > 0 GROUP BY 1 ORDER BY 1 DESC`); err != nil {
		return nil, err
	}
	return st, nil
}

// EachComic streams every comic with its reads, for export.
func (s *Store) EachComic(ctx context.Context, fn func(Comic, []int64) error) error {
	all, err := s.Comics(ctx, ComicQuery{Limit: 500})
	if err != nil {
		return err
	}
	for off := 0; len(all) > 0; {
		for _, c := range all {
			reads, err := s.int64s(ctx, `SELECT read_at FROM comic_reads WHERE comic_id=? ORDER BY read_at`, c.ID)
			if err != nil {
				return err
			}
			if err := fn(c, reads); err != nil {
				return err
			}
		}
		off += len(all)
		if all, err = s.Comics(ctx, ComicQuery{Limit: 500, Offset: off}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) int64s(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
