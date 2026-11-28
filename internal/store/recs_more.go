package store

import (
	"context"
	"strings"
	"time"
)

// ---------- ignored recommendations ----------

// IgnoredRec is a title someone asked never to be recommended again.
type IgnoredRec struct {
	Kind  string
	Title string
	Norm  string
	Year  int
	At    int64
}

// RecKey identifies a recommended title regardless of spelling or year.
func RecKey(kind, title string) string { return kind + "|" + NormTitle(title) }

func (s *Store) IgnoreRec(ctx context.Context, user, kind, title string, year int) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO rec_ignores(user,kind,norm,title,year,created_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(user,kind,norm) DO UPDATE SET title=excluded.title, year=excluded.year, created_at=excluded.created_at`,
		user, kind, NormTitle(title), strings.TrimSpace(title), ni(year), time.Now().Unix())
	return err
}

func (s *Store) UnignoreRec(ctx context.Context, user, kind, norm string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM rec_ignores WHERE user=? AND kind=? AND norm=?`, user, kind, norm)
	return err
}

func (s *Store) IgnoredRecs(ctx context.Context, user string) ([]IgnoredRec, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT kind, title, norm, COALESCE(year,0), created_at FROM rec_ignores
		WHERE user=? ORDER BY created_at DESC`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IgnoredRec
	for rows.Next() {
		var r IgnoredRec
		if err := rows.Scan(&r.Kind, &r.Title, &r.Norm, &r.Year, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IgnoredSet returns RecKey values a person has ignored.
func (s *Store) IgnoredSet(ctx context.Context, user string) (map[string]bool, error) {
	list, err := s.IgnoredRecs(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, r := range list {
		out[r.Kind+"|"+r.Norm] = true
	}
	return out, nil
}

// ---------- refreshing ----------

// RecUsers lists everyone who has asked for recommendations at least once.
func (s *Store) RecUsers(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT user FROM rec_runs ORDER BY user`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LastRecRunAt is when a person's newest run was made (0 if never).
func (s *Store) LastRecRunAt(ctx context.Context, user string) (int64, error) {
	var at int64
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),0) FROM rec_runs WHERE user=?`, user).Scan(&at)
	return at, err
}

// ---------- sources ----------

// PlaySourceFamilies counts plays by where they came from, grouped by the
// part before the first dash ("plex-webhook" and "plex-live" are both "plex").
func (s *Store) PlaySourceFamilies(ctx context.Context) ([]NameCount, error) {
	return s.nameCounts(ctx, `SELECT substr(source,1,instr(source||'-','-')-1) f, COUNT(*) c FROM plays
		WHERE source<>'' GROUP BY f ORDER BY c DESC`)
}

// familyFilter is the SQL test for "this play's source family is in fams".
// fams is ",plex,trakt," style; "" means every family.
const familyFilter = `(?F='' OR instr(?F, ','||substr(p.source,1,instr(p.source||'-','-')-1)||',')>0)`

func withFamilies(q, fams string) (string, string) {
	return strings.ReplaceAll(q, "?F", "?9"), fams
}

// MostPlayedFrom is MostPlayed limited to plays from the given source families.
func (s *Store) MostPlayedFrom(ctx context.Context, user string, includePrivate bool, fams string, limit int) ([]Rated, error) {
	q, f := withFamilies(`SELECT CASE WHEN m.kind='episode' THEN 'show' ELSE m.kind END, `+titleExpr+`,
		COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0), 0, COUNT(*) c
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE (?1='' OR p.user=?1) AND (?2 OR p.private=0) AND m.kind!='track' AND `+familyFilter+`
		GROUP BY 1, 2 ORDER BY c DESC LIMIT ?3`, fams)
	return s.rated(ctx, q, user, includePrivate, limit, nil, nil, nil, nil, nil, f)
}

// RecentTitlesFrom is RecentTitles limited to plays from the given source families.
func (s *Store) RecentTitlesFrom(ctx context.Context, user string, includePrivate bool, fams string, limit int) ([]Rated, error) {
	q, f := withFamilies(`SELECT CASE WHEN m.kind='episode' THEN 'show' ELSE m.kind END, `+titleExpr+`,
		COALESCE(CASE WHEN m.kind='movie' THEN m.year END,0), 0, COUNT(*)
		FROM plays p JOIN media m ON m.id=p.media_id
		WHERE (?1='' OR p.user=?1) AND (?2 OR p.private=0) AND m.kind!='track' AND p.watched_at > ?4 AND `+familyFilter+`
		GROUP BY 1, 2 ORDER BY MAX(p.watched_at) DESC LIMIT ?3`, fams)
	return s.rated(ctx, q, user, includePrivate, limit, time.Now().AddDate(0, -6, 0).Unix(), nil, nil, nil, nil, f)
}

// ArrLibrary returns titles in one Sonarr, Radarr or Whisparr instance,
// newest releases first.
func (s *Store) ArrLibrary(ctx context.Context, instance string, limit int) ([]Rated, error) {
	return s.rated(ctx, `SELECT CASE WHEN kind='series' THEN 'show' ELSE 'movie' END, COALESCE(title,''), COALESCE(year,0), 0, 0
		FROM arr_items WHERE instance=? AND deleted_at IS NULL AND COALESCE(title,'')<>''
		ORDER BY COALESCE(year,0) DESC, title LIMIT ?`, instance, limit)
}

// ArrHaveSet returns RecKey values for everything in the given instances.
func (s *Store) ArrHaveSet(ctx context.Context, instances []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, in := range instances {
		rows, err := s.ArrLibrary(ctx, in, 1_000_000)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[RecKey(r.Kind, r.Title)] = true
		}
	}
	return out, nil
}

// RecRunUser is whose taste a run was made for.
func (s *Store) RecRunUser(ctx context.Context, runID int64) (string, error) {
	var u string
	err := s.DB.QueryRowContext(ctx, `SELECT user FROM rec_runs WHERE id=?`, runID).Scan(&u)
	return u, err
}

// VisibleRecCount counts a person's recommendations that aren't ignored, from
// the newest successful run of each provider.
func (s *Store) VisibleRecCount(ctx context.Context, user string) (int, error) {
	ignored, err := s.IgnoredSet(ctx, user)
	if err != nil {
		return 0, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT c.kind, c.title FROM recs c WHERE c.run_id IN (
		SELECT MAX(id) FROM rec_runs WHERE user=? AND COALESCE(error,'')='' GROUP BY provider)`, user)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, title string
		if err := rows.Scan(&kind, &title); err != nil {
			return 0, err
		}
		if k := RecKey(kind, title); !ignored[k] {
			seen[k] = true
		}
	}
	return len(seen), rows.Err()
}
