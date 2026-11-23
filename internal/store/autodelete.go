package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// NowPlayingRow is one playback in progress.
type NowPlayingRow struct {
	Key       string      `json:"key"`
	User      string      `json:"user"`
	State     string      `json:"state"`
	Progress  float64     `json:"progress"`
	UpdatedAt int64       `json:"updated_at"`
	Player    string      `json:"player,omitempty"`
	Source    string      `json:"source,omitempty"`
	Media     model.Media `json:"media"`
	Display   string      `json:"display"`
	Counted   bool        `json:"counted"`
	FinishAt  float64     `json:"finish_at"`
}

// NowPlaying lists current sessions. A source can vanish without saying it
// stopped, so sessions that have gone quiet for too long are left out.
func (s *Store) NowPlaying(ctx context.Context) ([]NowPlayingRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.key, s.user, s.state, s.progress, s.updated_at, COALESCE(s.player,''), COALESCE(s.source,''), s.scrobbled, `+mediaCols+`
		FROM sessions s JOIN media m ON m.id=s.media_id WHERE s.state IN (?,?,?) AND COALESCE(s.source,'')<>'plex-mark' ORDER BY s.updated_at DESC LIMIT 50`,
		model.ActStart, model.ActPause, model.ActWatched)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().Unix()
	out := []NowPlayingRow{}
	for rows.Next() {
		var r NowPlayingRow
		dest := append([]any{&r.Key, &r.User, &r.State, &r.Progress, &r.UpdatedAt, &r.Player, &r.Source, &r.Counted}, mediaDest(&r.Media)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		limit := int64(4 * 3600)
		switch {
		case r.State == model.ActPause:
			limit = 2 * 3600
		case r.State == model.ActWatched:
			limit = 30 * 60 // past the watched point, so only the credits remain
		case r.Media.DurationS > 0:
			left := float64(r.Media.DurationS) * (100 - min(r.Progress, 100)) / 100
			limit = int64(left) + 600
		}
		if now-r.UpdatedAt > limit {
			continue
		}
		r.Display = r.Media.Display()
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRule says who must watch a title before its files may be removed.
type DeleteRule struct {
	Instance  string
	ArrID     int64
	Kind      string // series or movie
	Title     string
	Year      int
	TVDB      int
	TMDB      int
	IMDB      string
	Users     []string
	Enabled   bool
	GraceDays int
}

const deleteRuleCols = `a.instance, a.arr_id, a.kind, COALESCE(a.title,''), COALESCE(a.year,0), COALESCE(a.tvdb,0), COALESCE(a.tmdb,0),
	COALESCE(a.imdb,''), COALESCE(r.users,'[]'), COALESCE(r.enabled,0), COALESCE(r.grace_days,1)`

func scanDeleteRules(rows *sql.Rows) ([]DeleteRule, error) {
	defer rows.Close()
	var out []DeleteRule
	for rows.Next() {
		var r DeleteRule
		var users string
		if err := rows.Scan(&r.Instance, &r.ArrID, &r.Kind, &r.Title, &r.Year, &r.TVDB, &r.TMDB, &r.IMDB, &users, &r.Enabled, &r.GraceDays); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(users), &r.Users)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteTitles lists Sonarr, Radarr and Whisparr titles with their rules.
func (s *Store) DeleteTitles(ctx context.Context, search string, onlyOn bool, offset int) ([]DeleteRule, error) {
	like := ""
	if search != "" {
		like = "%" + search + "%"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+deleteRuleCols+` FROM arr_items a
		LEFT JOIN autodelete_rules r ON r.instance=a.instance AND r.arr_id=a.arr_id
		WHERE a.deleted_at IS NULL AND (?1='' OR a.title LIKE ?1) AND (?2=0 OR r.enabled=1)
		ORDER BY a.title COLLATE NOCASE, a.instance LIMIT 100 OFFSET ?3`, like, onlyOn, offset)
	if err != nil {
		return nil, err
	}
	return scanDeleteRules(rows)
}

// ActiveDeleteRules returns switched-on rules that name at least one user. A
// rule with nobody assigned can never delete anything.
func (s *Store) ActiveDeleteRules(ctx context.Context) ([]DeleteRule, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+deleteRuleCols+` FROM autodelete_rules r
		JOIN arr_items a ON r.instance=a.instance AND r.arr_id=a.arr_id
		WHERE r.enabled=1 AND a.deleted_at IS NULL ORDER BY a.title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	all, err := scanDeleteRules(rows)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if len(r.Users) > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// SaveDeleteRule stores who must watch a title. It must exist in the archive.
func (s *Store) SaveDeleteRule(ctx context.Context, instance string, arrID int64, users []string, enabled bool, graceDays int) error {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM arr_items WHERE instance=? AND arr_id=? AND deleted_at IS NULL`, instance, arrID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("that title is no longer in Sonarr or Radarr")
	}
	if users == nil {
		users = []string{}
	}
	if len(users) == 0 {
		enabled = false
	}
	b, _ := json.Marshal(users)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO autodelete_rules(instance,arr_id,users,enabled,grace_days,updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(instance,arr_id) DO UPDATE SET users=excluded.users, enabled=excluded.enabled,
		grace_days=excluded.grace_days, updated_at=excluded.updated_at`,
		instance, arrID, string(b), enabled, max(graceDays, 0), time.Now().Unix())
	return err
}

// Only a matching id can unlock a delete. A play with no ids, or with an id
// that contradicts the title's, never counts: same-named shows and remakes
// must not vouch for each other. ?1 tvdb, ?2 tmdb, ?3 imdb.
const showMatch = `((?1<>0 AND m.show_tvdb=?1) OR (?2<>0 AND m.show_tmdb=?2) OR (?3<>'' AND m.show_imdb=?3))
	AND (?1=0 OR m.show_tvdb IS NULL OR m.show_tvdb=?1) AND (?2=0 OR m.show_tmdb IS NULL OR m.show_tmdb=?2)
	AND (?3='' OR m.show_imdb IS NULL OR m.show_imdb=?3)`
const movieMatch = `((?1<>0 AND m.tvdb=?1) OR (?2<>0 AND m.tmdb=?2) OR (?3<>'' AND m.imdb=?3))
	AND (?1=0 OR m.tvdb IS NULL OR m.tvdb=?1) AND (?2=0 OR m.tmdb IS NULL OR m.tmdb=?2)
	AND (?3='' OR m.imdb IS NULL OR m.imdb=?3)`

// EpisodeWatches maps season and episode to each user's latest watch.
func (s *Store) EpisodeWatches(ctx context.Context, r DeleteRule) (map[[2]int]map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT m.season, m.episode, lower(p.user), MAX(p.watched_at) FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind='episode' AND m.season IS NOT NULL AND m.episode IS NOT NULL AND `+showMatch+` GROUP BY 1,2,3`, r.TVDB, r.TMDB, r.IMDB)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int]map[string]int64{}
	for rows.Next() {
		var se, ep int
		var user string
		var at int64
		if err := rows.Scan(&se, &ep, &user, &at); err != nil {
			return nil, err
		}
		k := [2]int{se, ep}
		if out[k] == nil {
			out[k] = map[string]int64{}
		}
		out[k][user] = at
	}
	return out, rows.Err()
}

// MovieWatches maps each user to their latest watch of a movie.
func (s *Store) MovieWatches(ctx context.Context, r DeleteRule) (map[string]int64, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT lower(p.user), MAX(p.watched_at) FROM plays p JOIN media m ON m.id=p.media_id
		WHERE m.kind='movie' AND `+movieMatch+` GROUP BY 1`, r.TVDB, r.TMDB, r.IMDB)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var user string
		var at int64
		if err := rows.Scan(&user, &at); err != nil {
			return nil, err
		}
		out[user] = at
	}
	return out, rows.Err()
}

func (s *Store) ruleWhere(r DeleteRule) (string, []any) {
	if r.Kind == "series" {
		return `m.kind='episode' AND ` + showMatch, []any{r.TVDB, r.TMDB, r.IMDB}
	}
	return `m.kind='movie' AND ` + movieMatch, []any{r.TVDB, r.TMDB, r.IMDB}
}

// BeingWatched reports whether anyone may be watching this title. Unlike the
// checks above it also matches by name, because here a false match only delays
// a delete.
func (s *Store) BeingWatched(ctx context.Context, r DeleteRule) (bool, error) {
	where, args := s.ruleWhere(r)
	name := `m.kind='movie' AND lower(m.title)=lower(?4)`
	if r.Kind == "series" {
		name = `m.kind='episode' AND lower(m.show_title)=lower(?4)`
	}
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions x JOIN media m ON m.id=x.media_id WHERE ((`+where+`) OR (`+name+`))
		AND x.updated_at > `+itoa64(time.Now().Add(-6*time.Hour).Unix()), append(args, r.Title)...).Scan(&n)
	return n > 0, err
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TitleLibraries lists the Plex libraries holding a title, linking plays to
// the archive by Plex guid.
func (s *Store) TitleLibraries(ctx context.Context, r DeleteRule) ([]string, error) {
	where, args := s.ruleWhere(r)
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT i.section_title FROM plex_items i JOIN media m ON m.plex_guid=i.guid
		WHERE i.section_title IS NOT NULL AND i.deleted_at IS NULL AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PlexLibraryNames lists libraries seen in the Plex archive.
func (s *Store) PlexLibraryNames(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT section_title FROM plex_items WHERE section_title IS NOT NULL AND deleted_at IS NULL ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteLogRow records one deletion attempt.
type DeleteLogRow struct {
	At                                   int64
	Instance, Title, Item, Users, Status string
}

// LogDelete keeps a permanent record of what was removed and why.
func (s *Store) LogDelete(ctx context.Context, r DeleteLogRow) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO autodelete_log(at,instance,title,item,users,status) VALUES(?,?,?,?,?,?)`,
		time.Now().Unix(), r.Instance, r.Title, r.Item, r.Users, strings.TrimSpace(r.Status))
	return err
}

// DeleteLog returns the newest deletion records.
func (s *Store) DeleteLog(ctx context.Context, limit int) ([]DeleteLogRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, instance, title, item, users, status FROM autodelete_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeleteLogRow
	for rows.Next() {
		var r DeleteLogRow
		if err := rows.Scan(&r.At, &r.Instance, &r.Title, &r.Item, &r.Users, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
