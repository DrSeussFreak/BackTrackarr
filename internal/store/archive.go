package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// The archive is a read-only copy of what lives in Plex (and in Sonarr /
// Radarr), kept so it can be rebuilt if a server dies. Full JSON documents
// are stored zlib-compressed; every earlier version is kept in plex_versions.

func pack(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func unpack(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil
	}
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func digest(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func jsonList(v []string) any {
	if len(v) == 0 {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s string) []string {
	var out []string
	if s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// ---------- Plex items ----------

type PlexItem struct {
	ID               int64    `json:"-"`
	ServerID         string   `json:"server_id"`
	RatingKey        string   `json:"rating_key"`
	SectionID        string   `json:"section_id,omitempty"`
	SectionTitle     string   `json:"section_title,omitempty"`
	Type             string   `json:"type"`
	GUID             string   `json:"guid,omitempty"`
	Title            string   `json:"title"`
	TitleSort        string   `json:"title_sort,omitempty"`
	OriginalTitle    string   `json:"original_title,omitempty"`
	Edition          string   `json:"edition,omitempty"`
	Year             int      `json:"year,omitempty"`
	ParentKey        string   `json:"parent_key,omitempty"`
	ParentTitle      string   `json:"parent_title,omitempty"`
	GrandparentKey   string   `json:"grandparent_key,omitempty"`
	GrandparentTitle string   `json:"grandparent_title,omitempty"`
	Index            int      `json:"index,omitempty"`
	ParentIndex      int      `json:"parent_index,omitempty"`
	UserRating       float64  `json:"user_rating,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	Collections      []string `json:"collections,omitempty"`
	Locked           []string `json:"locked,omitempty"`
	File             string   `json:"file,omitempty"`
	MediaID          int64    `json:"media_id,omitempty"`
	AddedAt          int64    `json:"added_at,omitempty"`
	UpdatedAt        int64    `json:"plex_updated_at,omitempty"`
	Sig              string   `json:"-"` // cheap fingerprint from the library listing
	Data             []byte   `json:"-"` // full metadata JSON (volatile fields removed)
	DeletedAt        int64    `json:"deleted_at,omitempty"`
}

// ItemSigs returns rating_key -> listing fingerprint for one library section.
func (s *Store) ItemSigs(ctx context.Context, serverID, sectionID string) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT rating_key, COALESCE(sig,'') FROM plex_items
		WHERE server_id=? AND section_id=? AND deleted_at IS NULL`, serverID, sectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveItem stores an item's current state. When its metadata differs from
// what was stored, the previous version is kept in plex_versions first.
// It reports whether anything changed.
func (s *Store) SaveItem(ctx context.Context, it *PlexItem) (bool, error) {
	now := time.Now().Unix()
	hash := digest(it.Data)
	var id int64
	var oldHash string
	var oldData []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id, COALESCE(hash,''), data FROM plex_items WHERE server_id=? AND rating_key=?`,
		it.ServerID, it.RatingKey).Scan(&id, &oldHash, &oldData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed := oldHash != hash
	if id != 0 && changed && len(oldData) > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plex_versions(kind,ref_id,captured_at,hash,data) VALUES('item',?,?,?,?)`,
			id, now, oldHash, oldData); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plex_items
		(server_id,rating_key,section_id,section_title,type,guid,title,title_sort,original_title,edition,year,
		 parent_key,parent_title,grandparent_key,grandparent_title,idx,parent_idx,user_rating,labels,collections,locked,
		 file,media_id,added_at,plex_updated_at,sig,hash,data,seen_at,deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
		ON CONFLICT(server_id,rating_key) DO UPDATE SET
		 section_id=excluded.section_id, section_title=excluded.section_title, type=excluded.type, guid=excluded.guid,
		 title=excluded.title, title_sort=excluded.title_sort, original_title=excluded.original_title, edition=excluded.edition,
		 year=excluded.year, parent_key=excluded.parent_key, parent_title=excluded.parent_title,
		 grandparent_key=excluded.grandparent_key, grandparent_title=excluded.grandparent_title, idx=excluded.idx,
		 parent_idx=excluded.parent_idx, user_rating=excluded.user_rating, labels=excluded.labels,
		 collections=excluded.collections, locked=excluded.locked, file=excluded.file,
		 media_id=COALESCE(excluded.media_id, plex_items.media_id), added_at=excluded.added_at,
		 plex_updated_at=excluded.plex_updated_at, sig=excluded.sig, hash=excluded.hash, data=excluded.data,
		 seen_at=excluded.seen_at, deleted_at=NULL`,
		it.ServerID, it.RatingKey, ns(it.SectionID), ns(it.SectionTitle), it.Type, ns(it.GUID), ns(it.Title),
		ns(it.TitleSort), ns(it.OriginalTitle), ns(it.Edition), ni(it.Year), ns(it.ParentKey), ns(it.ParentTitle),
		ns(it.GrandparentKey), ns(it.GrandparentTitle), ni(it.Index), ni(it.ParentIndex), nf(it.UserRating),
		jsonList(it.Labels), jsonList(it.Collections), jsonList(it.Locked), ns(it.File), ni64(it.MediaID),
		ni64(it.AddedAt), ni64(it.UpdatedAt), ns(it.Sig), hash, pack(it.Data), now)
	if err != nil {
		return false, err
	}
	return changed, tx.Commit()
}

// TouchItems marks unchanged items as still present.
func (s *Store) TouchItems(ctx context.Context, serverID string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `UPDATE plex_items SET seen_at=? WHERE server_id=? AND rating_key=?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, k := range keys {
		if _, err := st.ExecContext(ctx, now, serverID, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkItemsDeleted flags items of a section that a full sweep no longer saw.
// Nothing is removed: the archive keeps deleted items forever.
func (s *Store) MarkItemsDeleted(ctx context.Context, serverID, sectionID string, notSeenSince int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE plex_items SET deleted_at=? WHERE server_id=? AND section_id=?
		AND seen_at < ? AND deleted_at IS NULL`, time.Now().Unix(), serverID, sectionID, notSeenSince)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const itemCols = `id, server_id, rating_key, COALESCE(section_id,''), COALESCE(section_title,''), type, COALESCE(guid,''),
 COALESCE(title,''), COALESCE(title_sort,''), COALESCE(original_title,''), COALESCE(edition,''), COALESCE(year,0),
 COALESCE(parent_key,''), COALESCE(parent_title,''), COALESCE(grandparent_key,''), COALESCE(grandparent_title,''),
 COALESCE(idx,0), COALESCE(parent_idx,0), COALESCE(user_rating,0), COALESCE(labels,''), COALESCE(collections,''),
 COALESCE(locked,''), COALESCE(file,''), COALESCE(media_id,0), COALESCE(added_at,0), COALESCE(plex_updated_at,0),
 COALESCE(deleted_at,0)`

func scanItem(sc interface{ Scan(...any) error }, withData bool) (*PlexItem, error) {
	var it PlexItem
	var labels, colls, locked string
	dest := []any{&it.ID, &it.ServerID, &it.RatingKey, &it.SectionID, &it.SectionTitle, &it.Type, &it.GUID,
		&it.Title, &it.TitleSort, &it.OriginalTitle, &it.Edition, &it.Year, &it.ParentKey, &it.ParentTitle,
		&it.GrandparentKey, &it.GrandparentTitle, &it.Index, &it.ParentIndex, &it.UserRating, &labels, &colls,
		&locked, &it.File, &it.MediaID, &it.AddedAt, &it.UpdatedAt, &it.DeletedAt}
	var data []byte
	if withData {
		dest = append(dest, &data)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	it.Labels, it.Collections, it.Locked = parseList(labels), parseList(colls), parseList(locked)
	if withData {
		var err error
		if it.Data, err = unpack(data); err != nil {
			return nil, err
		}
	}
	return &it, nil
}

// EachItem streams the archived items of one server (all servers when
// serverID is empty), including full metadata when withData is set.
func (s *Store) EachItem(ctx context.Context, serverID string, withData bool, fn func(*PlexItem) error) error {
	cols := itemCols
	if withData {
		cols += ", data"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+cols+` FROM plex_items WHERE (?1='' OR server_id=?1) ORDER BY id`, serverID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows, withData)
		if err != nil {
			return err
		}
		if err := fn(it); err != nil {
			return err
		}
	}
	return rows.Err()
}

// FindItem finds a live item on a server by GUID, then by file path.
func (s *Store) FindItem(ctx context.Context, serverID, guid, file string) (*PlexItem, error) {
	for _, c := range []struct{ col, v string }{{"guid", guid}, {"file", file}} {
		if c.v == "" {
			continue
		}
		it, err := scanItem(s.DB.QueryRowContext(ctx, `SELECT `+itemCols+` FROM plex_items
			WHERE server_id=? AND `+c.col+`=? AND deleted_at IS NULL LIMIT 1`, serverID, c.v), false)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return it, err
	}
	return nil, nil
}

func (s *Store) GetItemByKey(ctx context.Context, serverID, ratingKey string) (*PlexItem, error) {
	it, err := scanItem(s.DB.QueryRowContext(ctx, `SELECT `+itemCols+`, data FROM plex_items WHERE server_id=? AND rating_key=?`,
		serverID, ratingKey), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

type ItemQuery struct {
	ServerID, SectionID, Type, Search string
	Offset, Limit                     int
}

func (s *Store) Items(ctx context.Context, q ItemQuery) ([]*PlexItem, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	like := ""
	if q.Search != "" {
		like = "%" + q.Search + "%"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+itemCols+` FROM plex_items
		WHERE (?1='' OR server_id=?1) AND (?2='' OR section_id=?2) AND (?3='' OR type=?3)
		  AND (?4='' OR title LIKE ?4 OR title_sort LIKE ?4 OR grandparent_title LIKE ?4 OR original_title LIKE ?4)
		ORDER BY COALESCE(title_sort, title) COLLATE NOCASE, id LIMIT ?5 OFFSET ?6`,
		q.ServerID, q.SectionID, q.Type, like, q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlexItem
	for rows.Next() {
		it, err := scanItem(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ItemVersions returns the earlier versions of one item, newest first.
func (s *Store) ItemVersions(ctx context.Context, kind string, refID int64) ([]Version, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT captured_at, data FROM plex_versions WHERE kind=? AND ref_id=? ORDER BY captured_at DESC`, kind, refID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var data []byte
		if err := rows.Scan(&v.CapturedAt, &data); err != nil {
			return nil, err
		}
		if v.Data, err = unpack(data); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type Version struct {
	CapturedAt int64
	Data       []byte
}

// ---------- playlists and collections ----------

type ListItem struct {
	Position  int    `json:"position"`
	RatingKey string `json:"rating_key,omitempty"`
	GUID      string `json:"guid,omitempty"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title,omitempty"`
	File      string `json:"file,omitempty"`
}

type PlexList struct {
	ID        int64      `json:"-"`
	ServerID  string     `json:"server_id"`
	Kind      string     `json:"kind"` // playlist | collection
	Owner     string     `json:"owner,omitempty"`
	RatingKey string     `json:"rating_key"`
	SectionID string     `json:"section_id,omitempty"`
	Title     string     `json:"title"`
	ListType  string     `json:"list_type,omitempty"` // video | audio | photo, or the collection's subtype
	Smart     bool       `json:"smart"`
	Content   string     `json:"content,omitempty"` // smart filter URI
	Summary   string     `json:"summary,omitempty"`
	ItemCount int        `json:"item_count"`
	Data      []byte     `json:"-"`
	Items     []ListItem `json:"items,omitempty"`
	SeenAt    int64      `json:"seen_at"`
	DeletedAt int64      `json:"deleted_at,omitempty"`
}

// SaveList stores a playlist or collection and its items, keeping the
// previous version when anything changed.
func (s *Store) SaveList(ctx context.Context, l *PlexList) (bool, error) {
	now := time.Now().Unix()
	itemsJSON, _ := json.Marshal(l.Items)
	hash := digest(l.Data, itemsJSON)
	var id int64
	var oldHash string
	var oldData []byte
	err := s.DB.QueryRowContext(ctx, `SELECT id, COALESCE(hash,''), data FROM plex_lists WHERE server_id=? AND kind=? AND owner=? AND rating_key=?`,
		l.ServerID, l.Kind, l.Owner, l.RatingKey).Scan(&id, &oldHash, &oldData)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	changed := oldHash != hash
	if id != 0 && changed {
		old, err := s.listItemsTx(ctx, tx, id)
		if err != nil {
			return false, err
		}
		meta, _ := unpack(oldData)
		snap, _ := json.Marshal(map[string]any{"list": json.RawMessage(nonEmptyJSON(meta)), "items": old})
		if _, err := tx.ExecContext(ctx, `INSERT INTO plex_versions(kind,ref_id,captured_at,hash,data) VALUES(?,?,?,?,?)`,
			l.Kind, id, now, oldHash, pack(snap)); err != nil {
			return false, err
		}
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO plex_lists
		(server_id,kind,owner,rating_key,section_id,title,list_type,smart,content,summary,item_count,hash,data,seen_at,deleted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
		ON CONFLICT(server_id,kind,owner,rating_key) DO UPDATE SET section_id=excluded.section_id, title=excluded.title,
		 list_type=excluded.list_type, smart=excluded.smart, content=excluded.content, summary=excluded.summary,
		 item_count=excluded.item_count, hash=excluded.hash, data=excluded.data, seen_at=excluded.seen_at, deleted_at=NULL
		RETURNING id`,
		l.ServerID, l.Kind, l.Owner, l.RatingKey, ns(l.SectionID), l.Title, ns(l.ListType), l.Smart, ns(l.Content),
		ns(l.Summary), len(l.Items), hash, pack(l.Data), now).Scan(&id)
	if err != nil {
		return false, err
	}
	l.ID = id
	if changed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM plex_list_items WHERE list_id=?`, id); err != nil {
			return false, err
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO plex_list_items(list_id,position,rating_key,guid,type,title,file) VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return false, err
		}
		defer st.Close()
		for i, it := range l.Items {
			if _, err := st.ExecContext(ctx, id, i, ns(it.RatingKey), ns(it.GUID), ns(it.Type), ns(it.Title), ns(it.File)); err != nil {
				return false, err
			}
		}
	}
	return changed, tx.Commit()
}

func nonEmptyJSON(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

func (s *Store) listItemsTx(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, listID int64) ([]ListItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT position, COALESCE(rating_key,''), COALESCE(guid,''), COALESCE(type,''),
		COALESCE(title,''), COALESCE(file,'') FROM plex_list_items WHERE list_id=? ORDER BY position`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListItem
	for rows.Next() {
		var it ListItem
		if err := rows.Scan(&it.Position, &it.RatingKey, &it.GUID, &it.Type, &it.Title, &it.File); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkListsDeleted flags lists of one owner (or one library, for collections)
// that the latest sweep no longer saw.
func (s *Store) MarkListsDeleted(ctx context.Context, serverID, kind, owner string, notSeenSince int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE plex_lists SET deleted_at=? WHERE server_id=? AND kind=? AND owner=?
		AND seen_at < ? AND deleted_at IS NULL`, time.Now().Unix(), serverID, kind, owner, notSeenSince)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const listCols = `id, server_id, kind, owner, rating_key, COALESCE(section_id,''), title, COALESCE(list_type,''), smart,
 COALESCE(content,''), COALESCE(summary,''), item_count, seen_at, COALESCE(deleted_at,0)`

func scanList(sc interface{ Scan(...any) error }) (*PlexList, error) {
	var l PlexList
	err := sc.Scan(&l.ID, &l.ServerID, &l.Kind, &l.Owner, &l.RatingKey, &l.SectionID, &l.Title, &l.ListType, &l.Smart,
		&l.Content, &l.Summary, &l.ItemCount, &l.SeenAt, &l.DeletedAt)
	return &l, err
}

// Lists returns archived lists. Empty filters match everything.
func (s *Store) Lists(ctx context.Context, serverID, kind, owner string) ([]*PlexList, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+listCols+` FROM plex_lists
		WHERE (?1='' OR server_id=?1) AND (?2='' OR kind=?2) AND (?3='' OR owner=?3)
		ORDER BY deleted_at IS NOT NULL, kind, owner, title COLLATE NOCASE`, serverID, kind, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PlexList
	for rows.Next() {
		l, err := scanList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetList returns one list with its items and full metadata.
func (s *Store) GetList(ctx context.Context, id int64) (*PlexList, error) {
	var data []byte
	row := s.DB.QueryRowContext(ctx, `SELECT `+listCols+`, data FROM plex_lists WHERE id=?`, id)
	var l PlexList
	err := row.Scan(&l.ID, &l.ServerID, &l.Kind, &l.Owner, &l.RatingKey, &l.SectionID, &l.Title, &l.ListType, &l.Smart,
		&l.Content, &l.Summary, &l.ItemCount, &l.SeenAt, &l.DeletedAt, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if l.Data, err = unpack(data); err != nil {
		return nil, err
	}
	if l.Items, err = s.listItemsTx(ctx, s.DB, id); err != nil {
		return nil, err
	}
	return &l, nil
}

// ---------- summary ----------

type ArchiveSection struct {
	ServerID, SectionID, Title string
	Counts                     map[string]int64
	Deleted                    int64
}

type ArchiveSummary struct {
	Sections    []*ArchiveSection
	Playlists   int64
	Collections int64
	Versions    int64
	Servers     []string
	Owners      []string
	ArrItems    map[string]int64
}

func (s *Store) ArchiveSummary(ctx context.Context) (*ArchiveSummary, error) {
	sum := &ArchiveSummary{ArrItems: map[string]int64{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, COALESCE(section_id,''), COALESCE(section_title,''), type,
		SUM(deleted_at IS NULL), SUM(deleted_at IS NOT NULL) FROM plex_items GROUP BY 1,2,3,4 ORDER BY 3, 1`)
	if err != nil {
		return nil, err
	}
	idx := map[string]*ArchiveSection{}
	for rows.Next() {
		var srv, sec, title, typ string
		var live, del int64
		if err := rows.Scan(&srv, &sec, &title, &typ, &live, &del); err != nil {
			rows.Close()
			return nil, err
		}
		k := srv + "|" + sec
		a := idx[k]
		if a == nil {
			a = &ArchiveSection{ServerID: srv, SectionID: sec, Title: title, Counts: map[string]int64{}}
			idx[k] = a
			sum.Sections = append(sum.Sections, a)
		}
		a.Counts[typ] = live
		a.Deleted += del
	}
	rows.Close()
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(kind='playlist' AND deleted_at IS NULL),0),
		COALESCE(SUM(kind='collection' AND deleted_at IS NULL),0) FROM plex_lists`).Scan(&sum.Playlists, &sum.Collections); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM plex_versions`).Scan(&sum.Versions); err != nil {
		return nil, err
	}
	if sum.Servers, err = s.strings(ctx, `SELECT server_id FROM plex_items UNION SELECT server_id FROM plex_lists ORDER BY 1`); err != nil {
		return nil, err
	}
	if sum.Owners, err = s.strings(ctx, `SELECT DISTINCT owner FROM plex_lists WHERE kind='playlist' AND owner!='' ORDER BY 1`); err != nil {
		return nil, err
	}
	r2, err := s.DB.QueryContext(ctx, `SELECT instance, COUNT(*) FROM arr_items WHERE deleted_at IS NULL GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer r2.Close()
	for r2.Next() {
		var k string
		var n int64
		if err := r2.Scan(&k, &n); err != nil {
			return nil, err
		}
		sum.ArrItems[k] = n
	}
	return sum, r2.Err()
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
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

// ---------- Sonarr / Radarr / Whisparr ----------

type ArrItem struct {
	Instance  string
	ArrID     int64
	Kind      string // series | movie
	Title     string
	Year      int
	TVDB      int
	TMDB      int
	IMDB      string
	Monitored bool
	Path      string
	Data      []byte
}

// SaveArrItem stores one series/movie from an *arr instance. It reports
// whether it was new or changed.
func (s *Store) SaveArrItem(ctx context.Context, it ArrItem) (bool, error) {
	hash := digest(it.Data)
	var old string
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(hash,'') FROM arr_items WHERE instance=? AND arr_id=?`, it.Instance, it.ArrID).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	now := time.Now().Unix()
	if old == hash {
		_, err := s.DB.ExecContext(ctx, `UPDATE arr_items SET seen_at=?, deleted_at=NULL WHERE instance=? AND arr_id=?`, now, it.Instance, it.ArrID)
		return false, err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO arr_items(instance,arr_id,kind,title,year,tvdb,tmdb,imdb,monitored,path,hash,data,seen_at,deleted_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
		ON CONFLICT(instance,arr_id) DO UPDATE SET kind=excluded.kind, title=excluded.title, year=excluded.year,
		 tvdb=excluded.tvdb, tmdb=excluded.tmdb, imdb=excluded.imdb, monitored=excluded.monitored, path=excluded.path,
		 hash=excluded.hash, data=excluded.data, seen_at=excluded.seen_at, deleted_at=NULL`,
		it.Instance, it.ArrID, it.Kind, ns(it.Title), ni(it.Year), ni(it.TVDB), ni(it.TMDB), ns(it.IMDB), it.Monitored,
		ns(it.Path), hash, pack(it.Data), now)
	return true, err
}

func (s *Store) MarkArrDeleted(ctx context.Context, instance string, notSeenSince int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE arr_items SET deleted_at=? WHERE instance=? AND seen_at < ? AND deleted_at IS NULL`,
		time.Now().Unix(), instance, notSeenSince)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SaveArrConfig keeps a new copy of a settings document (quality profiles,
// root folders, tags...) whenever it changes.
func (s *Store) SaveArrConfig(ctx context.Context, instance, name string, data []byte) (bool, error) {
	hash := digest(data)
	var last string
	err := s.DB.QueryRowContext(ctx, `SELECT hash FROM arr_config WHERE instance=? AND name=? ORDER BY captured_at DESC LIMIT 1`,
		instance, name).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if last == hash {
		return false, nil
	}
	_, err = s.DB.ExecContext(ctx, `INSERT OR REPLACE INTO arr_config(instance,name,hash,data,captured_at) VALUES(?,?,?,?,?)`,
		instance, name, hash, pack(data), time.Now().Unix())
	return true, err
}

// InArr reports which instances already have a title (by external id).
func (s *Store) InArr(ctx context.Context, tmdb, tvdb int, imdb string) ([]string, error) {
	return s.strings(ctx, `SELECT DISTINCT instance FROM arr_items WHERE deleted_at IS NULL AND
		((?1 != 0 AND tmdb=?1) OR (?2 != 0 AND tvdb=?2) OR (?3 != '' AND imdb=?3))`, tmdb, tvdb, imdb)
}

// ---------- helpers ----------

func nf(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func ni64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// NormTitle lowercases and strips punctuation for loose title matching.
func NormTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MarkCollectionsDeleted flags collections of one library that the latest
// sweep no longer saw.
func (s *Store) MarkCollectionsDeleted(ctx context.Context, serverID, sectionID string, notSeenSince int64) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE plex_lists SET deleted_at=? WHERE server_id=? AND kind='collection' AND section_id=?
		AND seen_at < ? AND deleted_at IS NULL`, time.Now().Unix(), serverID, sectionID, notSeenSince)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
