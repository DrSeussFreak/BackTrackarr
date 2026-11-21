package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ShelfItem is one book, audiobook or game as one source knows it. The same
// title from two sources stays two rows, so no source's data is overwritten.
type ShelfItem struct {
	ID           int64           `json:"id"`
	MediaType    string          `json:"media_type"`
	Source       string          `json:"source"`
	ExtKey       string          `json:"ext_key"`
	Title        string          `json:"title"`
	Creator      string          `json:"creator,omitempty"`
	Series       string          `json:"series,omitempty"`
	Platform     string          `json:"platform,omitempty"`
	ISBN         string          `json:"isbn,omitempty"`
	ASIN         string          `json:"asin,omitempty"`
	Year         int64           `json:"year,omitempty"`
	Pages        int64           `json:"pages,omitempty"`
	DurationS    int64           `json:"duration_s,omitempty"`
	Status       string          `json:"status,omitempty"`
	Rating       float64         `json:"rating,omitempty"`
	Progress     float64         `json:"progress,omitempty"`
	PlaytimeS    int64           `json:"playtime_s,omitempty"`
	LastActivity int64           `json:"last_activity,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
	Finishes     int64           `json:"finishes"`
	Events       int64           `json:"events"`
}

// ShelfPrev is what was stored before an upsert. Games use it to turn a
// lifetime playtime counter into separate sessions.
type ShelfPrev struct {
	New          bool
	PlaytimeS    int64
	LastActivity int64
}

// ShelfEvent is one read, listen or play.
type ShelfEvent struct {
	ID        int64           `json:"id"`
	ItemID    int64           `json:"item_id"`
	User      string          `json:"user,omitempty"`
	Kind      string          `json:"kind"`
	ExtID     string          `json:"ext_id"`
	At        int64           `json:"at"`
	StartedAt int64           `json:"started_at,omitempty"`
	DurationS int64           `json:"duration_s,omitempty"`
	Progress  float64         `json:"progress,omitempty"`
	Device    string          `json:"device,omitempty"`
	Source    string          `json:"source"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// ShelfEventRow is an event with its title, for history tables.
type ShelfEventRow struct {
	ShelfEvent
	MediaType string
	Title     string
	Creator   string
	Platform  string
}

func packRaw(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return pack(b)
}

// UpsertShelfItem inserts or updates a title. Empty fields never wipe what an
// earlier sync stored, and the last activity time only moves forward.
func (s *Store) UpsertShelfItem(ctx context.Context, it *ShelfItem) (ShelfPrev, error) {
	var prev ShelfPrev
	var id, play, last sql.NullInt64
	err := s.q(ctx).QueryRowContext(ctx, `SELECT id, playtime_s, last_activity FROM shelf_items WHERE source=? AND ext_key=?`,
		it.Source, it.ExtKey).Scan(&id, &play, &last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return prev, err
	}
	prev = ShelfPrev{New: !id.Valid, PlaytimeS: play.Int64, LastActivity: last.Int64}
	now := time.Now().Unix()
	s.shelfGen.Add(1)
	err = s.q(ctx).QueryRowContext(ctx, `INSERT INTO shelf_items(media_type,source,ext_key,title,norm_key,creator,series,platform,
		isbn,asin,year,pages,duration_s,status,rating,progress,playtime_s,last_activity,raw,first_seen,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source,ext_key) DO UPDATE SET media_type=excluded.media_type, title=excluded.title, norm_key=excluded.norm_key,
		 creator=COALESCE(excluded.creator, shelf_items.creator), series=COALESCE(excluded.series, shelf_items.series),
		 platform=COALESCE(excluded.platform, shelf_items.platform), isbn=COALESCE(excluded.isbn, shelf_items.isbn),
		 asin=COALESCE(excluded.asin, shelf_items.asin), year=COALESCE(excluded.year, shelf_items.year),
		 pages=COALESCE(excluded.pages, shelf_items.pages), duration_s=COALESCE(excluded.duration_s, shelf_items.duration_s),
		 status=COALESCE(excluded.status, shelf_items.status), rating=COALESCE(excluded.rating, shelf_items.rating),
		 progress=COALESCE(excluded.progress, shelf_items.progress), playtime_s=COALESCE(excluded.playtime_s, shelf_items.playtime_s),
		 last_activity=MAX(COALESCE(excluded.last_activity,0), COALESCE(shelf_items.last_activity,0)),
		 raw=COALESCE(excluded.raw, shelf_items.raw), updated_at=excluded.updated_at
		RETURNING id`,
		it.MediaType, it.Source, it.ExtKey, it.Title, NormTitle(it.Title), ns(it.Creator), ns(it.Series), ns(it.Platform),
		ns(it.ISBN), ns(it.ASIN), ni64(it.Year), ni64(it.Pages), ni64(it.DurationS), ns(it.Status), nf(it.Rating), nf(it.Progress),
		ni64(it.PlaytimeS), ni64(it.LastActivity), packRaw(it.Raw), now, now).Scan(&it.ID)
	return prev, err
}

// ShelfItemID finds a stored title without fetching it from its source again.
func (s *Store) ShelfItemID(ctx context.Context, source, key string) (int64, error) {
	var id int64
	err := s.q(ctx).QueryRowContext(ctx, `SELECT id FROM shelf_items WHERE source=? AND ext_key=?`, source, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// TouchShelfItem updates progress on a title that is already stored.
func (s *Store) TouchShelfItem(ctx context.Context, id int64, status string, progress float64, last int64) error {
	_, err := s.q(ctx).ExecContext(ctx, `UPDATE shelf_items SET status=COALESCE(?,status), progress=COALESCE(?,progress),
		last_activity=MAX(COALESCE(?,0), COALESCE(last_activity,0)), updated_at=? WHERE id=?`,
		ns(status), nf(progress), ni64(last), time.Now().Unix(), id)
	return err
}

// AddShelfEvent records one read or play and reports whether it was new. A
// session still in progress updates its own row, and becomes a finish when
// the source says it was completed.
func (s *Store) AddShelfEvent(ctx context.Context, ev *ShelfEvent) (bool, error) {
	s.shelfGen.Add(1)
	res, err := s.q(ctx).ExecContext(ctx, `INSERT OR IGNORE INTO shelf_events(item_id,user,kind,ext_id,at,started_at,duration_s,progress,device,source,raw)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ev.ItemID, ev.User, ev.Kind, ev.ExtID, ev.At, ni64(ev.StartedAt), ni64(ev.DurationS),
		nf(ev.Progress), ns(ev.Device), ev.Source, packRaw(ev.Raw))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	switch ev.Kind {
	case "session":
		_, err = s.q(ctx).ExecContext(ctx, `UPDATE shelf_events SET at=?, duration_s=?, progress=?, raw=COALESCE(?,raw)
			WHERE item_id=? AND ext_id=? AND kind='session' AND (at<>? OR duration_s IS NOT ? OR progress IS NOT ?)`,
			ev.At, ni64(ev.DurationS), nf(ev.Progress), packRaw(ev.Raw), ev.ItemID, ev.ExtID, ev.At, ni64(ev.DurationS), nf(ev.Progress))
		return false, err
	case "finish":
		res, err := s.q(ctx).ExecContext(ctx, `UPDATE shelf_events SET kind='finish', at=?, started_at=COALESCE(?,started_at),
			duration_s=COALESCE(?,duration_s), progress=COALESCE(?,progress), raw=COALESCE(?,raw)
			WHERE item_id=? AND ext_id=? AND kind='session'`,
			ev.At, ni64(ev.StartedAt), ni64(ev.DurationS), nf(ev.Progress), packRaw(ev.Raw), ev.ItemID, ev.ExtID)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	return false, nil
}

// HasShelfFinish reports whether a title already has a finish recorded.
func (s *Store) HasShelfFinish(ctx context.Context, itemID int64) (bool, error) {
	var n int
	err := s.q(ctx).QueryRowContext(ctx, `SELECT COUNT(*) FROM shelf_events WHERE item_id=? AND kind='finish'`, itemID).Scan(&n)
	return n > 0, err
}

// SetUndatedFinishes keeps a source's read count honest when it gives fewer
// dates than reads: the difference is stored as undated finishes. Those rows
// are replaced when a later export supplies a real date.
func (s *Store) SetUndatedFinishes(ctx context.Context, itemID int64, user, source string, readCount int64) error {
	return s.InTx(ctx, func(ctx context.Context) error {
		q := s.q(ctx)
		if _, err := q.ExecContext(ctx, `DELETE FROM shelf_events WHERE item_id=? AND kind='finish' AND at=0 AND ext_id LIKE 'count:%'`, itemID); err != nil {
			return err
		}
		var dated int64
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM shelf_events WHERE item_id=? AND kind='finish'`, itemID).Scan(&dated); err != nil {
			return err
		}
		for i := dated + 1; i <= readCount; i++ {
			if _, err := q.ExecContext(ctx, `INSERT INTO shelf_events(item_id,user,kind,ext_id,at,source) VALUES(?,?,'finish',?,0,?)`,
				itemID, user, "count:"+strconv.FormatInt(i, 10), source); err != nil {
				return err
			}
		}
		s.shelfGen.Add(1)
		return nil
	})
}

// shelfTypes turns a page filter into a fixed SQL list. Only these constant
// strings ever reach the query.
func shelfTypes(t string) string {
	switch t {
	case "book":
		return `('book')`
	case "audiobook":
		return `('audiobook')`
	case "game":
		return `('game')`
	}
	return `('book','audiobook')`
}

type ShelfQuery struct {
	Type   string // book, audiobook, game, or empty for books and audiobooks
	Search string
	Status string
	Offset int
	Limit  int
}

const shelfCols = `i.id, i.media_type, i.source, i.ext_key, i.title, COALESCE(i.creator,''), COALESCE(i.series,''), COALESCE(i.platform,''),
	COALESCE(i.isbn,''), COALESCE(i.asin,''), COALESCE(i.year,0), COALESCE(i.pages,0), COALESCE(i.duration_s,0), COALESCE(i.status,''),
	COALESCE(i.rating,0), COALESCE(i.progress,0), COALESCE(i.playtime_s,0), COALESCE(i.last_activity,0),
	(SELECT COUNT(*) FROM shelf_events e WHERE e.item_id=i.id AND e.kind='finish'), (SELECT COUNT(*) FROM shelf_events e WHERE e.item_id=i.id)`

func scanShelfItem(rows *sql.Rows, extra ...any) (ShelfItem, error) {
	var it ShelfItem
	dst := append([]any{&it.ID, &it.MediaType, &it.Source, &it.ExtKey, &it.Title, &it.Creator, &it.Series, &it.Platform, &it.ISBN, &it.ASIN,
		&it.Year, &it.Pages, &it.DurationS, &it.Status, &it.Rating, &it.Progress, &it.PlaytimeS, &it.LastActivity, &it.Finishes, &it.Events}, extra...)
	return it, rows.Scan(dst...)
}

// ShelfItems lists titles, most recently active first.
func (s *Store) ShelfItems(ctx context.Context, q ShelfQuery) ([]ShelfItem, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	like := ""
	if q.Search != "" {
		like = "%" + q.Search + "%"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+shelfCols+` FROM shelf_items i
		WHERE i.media_type IN `+shelfTypes(q.Type)+`
		 AND (?1='' OR i.title LIKE ?1 OR i.creator LIKE ?1 OR i.series LIKE ?1 OR i.platform LIKE ?1 OR i.isbn LIKE ?1 OR i.asin LIKE ?1)
		 AND (?2='' OR i.status=?2)
		ORDER BY COALESCE(i.last_activity,0) DESC, i.title COLLATE NOCASE, i.id LIMIT ?3 OFFSET ?4`,
		like, q.Status, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShelfItem
	for rows.Next() {
		it, err := scanShelfItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RecentShelfEvents returns the latest dated activity. mediaType "all"
// covers books, audiobooks and games.
func (s *Store) RecentShelfEvents(ctx context.Context, mediaType string, limit int) ([]ShelfEventRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	where := `i.media_type IN ` + shelfTypes(mediaType)
	if mediaType == "all" {
		where = `1=1`
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT e.id, e.item_id, e.user, e.kind, e.ext_id, e.at, COALESCE(e.started_at,0), COALESCE(e.duration_s,0),
		COALESCE(e.progress,0), COALESCE(e.device,''), e.source, i.media_type, i.title, COALESCE(i.creator,''), COALESCE(i.platform,'')
		FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id
		WHERE e.at>0 AND e.kind<>'baseline' AND `+where+` ORDER BY e.at DESC, e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShelfEventRow
	for rows.Next() {
		var r ShelfEventRow
		if err := rows.Scan(&r.ID, &r.ItemID, &r.User, &r.Kind, &r.ExtID, &r.At, &r.StartedAt, &r.DurationS, &r.Progress, &r.Device,
			&r.Source, &r.MediaType, &r.Title, &r.Creator, &r.Platform); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ShelfStats counts a title once even when several sources track it, and a
// read once when two sources logged it on the same day.
type ShelfStats struct {
	Titles    int64       `json:"titles"`
	Finished  int64       `json:"finished"`
	Reads     int64       `json:"reads"`
	Repeats   int64       `json:"repeats"`
	Sessions  int64       `json:"sessions"`
	Seconds   int64       `json:"seconds"`
	ThisYear  int64       `json:"this_year"`
	ByYear    []NameCount `json:"by_year"`
	Creators  []NameCount `json:"creators"`
	Platforms []NameCount `json:"platforms"`
	Sources   []NameCount `json:"sources"`
	TopPlayed []NameCount `json:"top_played,omitempty"`
}

// ShelfStats is cached until a book or game changes, so page loads stay cheap
// on a large library.
func (s *Store) ShelfStats(ctx context.Context, mediaType string) (*ShelfStats, error) {
	gen, year := s.shelfGen.Load(), time.Now().Year()
	s.shelfMu.Lock()
	c, ok := s.shelfCache[mediaType]
	s.shelfMu.Unlock()
	if ok && c.gen == gen && c.year == year {
		return c.st, nil
	}
	st, err := s.computeShelfStats(ctx, mediaType)
	if err != nil {
		return nil, err
	}
	s.shelfMu.Lock()
	if s.shelfCache == nil {
		s.shelfCache = map[string]cachedShelf{}
	}
	s.shelfCache[mediaType] = cachedShelf{gen: gen, year: year, st: st}
	s.shelfMu.Unlock()
	return st, nil
}

type cachedShelf struct {
	gen  int64
	year int
	st   *ShelfStats
}

func (s *Store) computeShelfStats(ctx context.Context, mediaType string) (*ShelfStats, error) {
	t := shelfTypes(mediaType)
	st := &ShelfStats{}
	finishes := `SELECT DISTINCT i.norm_key k, strftime('%Y-%m-%d', e.at, 'unixepoch') d FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id
		WHERE e.kind='finish' AND e.at>0 AND i.media_type IN ` + t
	var dated, undated int64
	year := time.Now().Format("2006")
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(substr(d,1,4)=?),0) FROM (`+finishes+`)`, year).Scan(&dated, &st.ThisYear); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id
		WHERE e.kind='finish' AND e.at=0 AND i.media_type IN `+t).Scan(&undated); err != nil {
		return nil, err
	}
	st.Reads = dated + undated
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(DISTINCT i.norm_key), COUNT(DISTINCT CASE WHEN EXISTS
		(SELECT 1 FROM shelf_events e WHERE e.item_id=i.id AND e.kind='finish') THEN i.norm_key END)
		FROM shelf_items i WHERE i.media_type IN `+t).Scan(&st.Titles, &st.Finished); err != nil {
		return nil, err
	}
	if st.Repeats = st.Reads - st.Finished; st.Repeats < 0 {
		st.Repeats = 0
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id
		WHERE e.kind IN ('session','play') AND i.media_type IN `+t).Scan(&st.Sessions); err != nil {
		return nil, err
	}
	// Games report a lifetime total per source, so take the largest for each
	// title. Books and audiobooks add up their sessions.
	q := `SELECT COALESCE(SUM(n),0) FROM (SELECT MAX(COALESCE(i.playtime_s,0)) n FROM shelf_items i WHERE i.media_type IN ` + t + ` GROUP BY i.norm_key)`
	if mediaType != "game" {
		q = `SELECT COALESCE(SUM(e.duration_s),0) FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id WHERE e.kind='session' AND i.media_type IN ` + t
	}
	if err := s.DB.QueryRowContext(ctx, q).Scan(&st.Seconds); err != nil {
		return nil, err
	}
	var err error
	if st.ByYear, err = s.nameCounts(ctx, `SELECT substr(d,1,4), COUNT(*) FROM (`+finishes+`) GROUP BY 1 ORDER BY 1 DESC`); err != nil {
		return nil, err
	}
	if st.Creators, err = s.nameCounts(ctx, `SELECT creator, COUNT(DISTINCT norm_key) c FROM shelf_items i WHERE creator IS NOT NULL AND media_type IN `+t+` GROUP BY 1 ORDER BY c DESC, 1 LIMIT 10`); err != nil {
		return nil, err
	}
	if st.Platforms, err = s.nameCounts(ctx, `SELECT platform, COUNT(DISTINCT norm_key) c FROM shelf_items i WHERE platform IS NOT NULL AND media_type IN `+t+` GROUP BY 1 ORDER BY c DESC, 1 LIMIT 10`); err != nil {
		return nil, err
	}
	if st.Sources, err = s.nameCounts(ctx, `SELECT source, COUNT(*) c FROM shelf_items i WHERE media_type IN `+t+` GROUP BY 1 ORDER BY c DESC, 1`); err != nil {
		return nil, err
	}
	if mediaType == "game" {
		if st.TopPlayed, err = s.nameCounts(ctx, `SELECT MIN(title), MAX(playtime_s)/3600 h FROM shelf_items i WHERE media_type='game' AND playtime_s>=3600
			GROUP BY norm_key ORDER BY h DESC, 1 LIMIT 10`); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// EachShelfItem streams every title with its events and raw source data, for
// export. It pages through the table so a large library stays out of memory.
func (s *Store) EachShelfItem(ctx context.Context, mediaType string, fn func(ShelfItem, []ShelfEvent) error) error {
	var after int64
	for {
		rows, err := s.DB.QueryContext(ctx, `SELECT `+shelfCols+`, i.raw FROM shelf_items i WHERE i.id>? AND i.media_type IN `+shelfTypes(mediaType)+` ORDER BY i.id LIMIT 200`, after)
		if err != nil {
			return err
		}
		var page []ShelfItem
		for rows.Next() {
			var raw []byte
			it, err := scanShelfItem(rows, &raw)
			if err != nil {
				rows.Close()
				return err
			}
			if b, err := unpack(raw); err == nil {
				it.Raw = b
			}
			page = append(page, it)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, it := range page {
			evs, err := s.shelfEvents(ctx, it.ID)
			if err != nil {
				return err
			}
			if err := fn(it, evs); err != nil {
				return err
			}
			after = it.ID
		}
	}
}

func (s *Store) shelfEvents(ctx context.Context, itemID int64) ([]ShelfEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, item_id, user, kind, ext_id, at, COALESCE(started_at,0), COALESCE(duration_s,0),
		COALESCE(progress,0), COALESCE(device,''), source, raw FROM shelf_events WHERE item_id=? ORDER BY at, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShelfEvent
	for rows.Next() {
		var e ShelfEvent
		var raw []byte
		if err := rows.Scan(&e.ID, &e.ItemID, &e.User, &e.Kind, &e.ExtID, &e.At, &e.StartedAt, &e.DurationS, &e.Progress, &e.Device, &e.Source, &raw); err != nil {
			return nil, err
		}
		if b, err := unpack(raw); err == nil {
			e.Raw = b
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ShelfStatuses lists the status values present, for the page filter.
func (s *Store) ShelfStatuses(ctx context.Context, mediaType string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT status FROM shelf_items WHERE status IS NOT NULL AND media_type IN `+shelfTypes(mediaType)+` ORDER BY 1`)
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
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}
