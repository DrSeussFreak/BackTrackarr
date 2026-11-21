package core

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// The universal import takes a file or ZIP from nearly any tracker: Trakt,
// Simkl, Letterboxd, IMDb, Netflix and other streaming services, or any list
// with a title column. Plays and ratings go into history. Everything else
// (watchlists, lists, comments) is kept as raw records.

const (
	uniMaxEntry = 512 << 20
	uniMaxTotal = 2 << 30
	uniMaxFiles = 5000
	uniBatch    = 300
)

// UniversalResult summarizes an import.
type UniversalResult struct {
	Files, Items, Plays, Duplicates, Ratings, Kept, Skipped, Comics, Shelf int
	Formats                                                                []string
}

func (r *UniversalResult) String() string {
	s := fmt.Sprintf("%d new plays, %d already there, %d ratings, %d other records kept", r.Plays, r.Duplicates, r.Ratings, r.Kept)
	if r.Shelf > 0 {
		s += fmt.Sprintf(", %d books or games", r.Shelf)
	}
	if r.Comics > 0 {
		s += fmt.Sprintf(", %d comics", r.Comics)
	}
	if r.Skipped > 0 {
		s += fmt.Sprintf(", %d unreadable", r.Skipped)
	}
	if len(r.Formats) > 0 {
		s += " (" + strings.Join(r.Formats, ", ") + ")"
	}
	return s
}

type uniRec struct {
	media   *model.Media
	at      int64
	rating  float64
	ratedAt int64
	source  string
	kind    string // for kept records
	title   string
	raw     []byte
}

type uniRun struct {
	e       *Engine
	user    string
	res     *UniversalResult
	pending []uniRec
	formats map[string]bool
	report  func(string)
}

func (u *uniRun) emit(ctx context.Context, r uniRec) error {
	u.res.Items++
	u.pending = append(u.pending, r)
	if len(u.pending) >= uniBatch {
		return u.flush(ctx)
	}
	return nil
}

func (u *uniRun) flush(ctx context.Context) error {
	if len(u.pending) == 0 {
		return nil
	}
	batch := u.pending
	u.pending = nil
	err := u.e.st.InTx(ctx, func(ctx context.Context) error {
		for _, r := range batch {
			if r.media == nil {
				ok, err := u.e.st.SaveImportExtra(ctx, r.source, r.kind, r.title, r.raw)
				if err != nil {
					return err
				}
				if ok {
					u.res.Kept++
				}
				continue
			}
			m := *r.media
			if err := u.e.st.UpsertMedia(ctx, &m); err != nil {
				u.res.Skipped++
				continue
			}
			if r.at > 0 {
				ins, err := u.e.st.RecordPlay(ctx, m.ID, u.user, time.Unix(r.at, 0), r.source, "", false)
				if err != nil {
					return err
				}
				if ins {
					u.res.Plays++
				} else {
					u.res.Duplicates++
				}
			}
			if r.rating > 0 {
				at := r.ratedAt
				if at == 0 {
					at = r.at
				}
				if at == 0 {
					at = time.Now().Unix()
				}
				if err := u.e.st.SetRating(ctx, m.ID, u.user, min(r.rating, 10), time.Unix(at, 0)); err != nil {
					return err
				}
				u.res.Ratings++
			}
		}
		return nil
	})
	if err == nil && u.report != nil {
		u.report(u.res.String())
	}
	return err
}

// ImportAnything reads a file or ZIP already saved at p.
func (e *Engine) ImportAnything(ctx context.Context, p, name, user string, report func(string)) (*UniversalResult, error) {
	if user == "" {
		user = e.defaultUser(ctx)
	}
	u := &uniRun{e: e, user: user, res: &UniversalResult{}, formats: map[string]bool{}, report: report}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if n == 4 && bytes.Equal(magic, []byte("PK\x03\x04")) {
		err = u.readZip(ctx, p)
	} else {
		u.res.Files = 1
		err = u.readFile(ctx, f, name)
	}
	if ferr := u.flush(ctx); err == nil {
		err = ferr
	}
	for k := range u.formats {
		u.res.Formats = append(u.res.Formats, k)
	}
	sort.Strings(u.res.Formats)
	if err == nil && u.res.Items == 0 {
		return u.res, errors.New("nothing readable found in that file")
	}
	return u.res, err
}

func (u *uniRun) shelfDone(res *ShelfImportResult) {
	u.formats[res.Source] = true
	u.res.Items += res.Rows
	u.res.Shelf += res.Rows
}

func (u *uniRun) readZip(ctx context.Context, p string) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return fmt.Errorf("couldn't open the ZIP: %w", err)
	}
	defer zr.Close()
	var total uint64
	for _, f := range zr.File {
		base := path.Base(f.Name)
		if f.FileInfo().IsDir() || strings.HasPrefix(f.Name, "__MACOSX/") || strings.HasPrefix(base, ".") {
			continue
		}
		switch strings.ToLower(path.Ext(base)) {
		case ".json", ".csv", ".tsv", ".txt", ".xml":
		default:
			continue
		}
		if u.res.Files >= uniMaxFiles {
			return errors.New("that ZIP holds too many files")
		}
		total += f.UncompressedSize64
		if f.UncompressedSize64 > uniMaxEntry || total > uniMaxTotal {
			return errors.New("that ZIP is too large to import")
		}
		rc, err := f.Open()
		if err != nil {
			u.res.Skipped++
			continue
		}
		u.res.Files++
		err = u.readFile(ctx, io.LimitReader(rc, uniMaxEntry), f.Name)
		rc.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			u.res.Skipped++ // one bad file doesn't stop the rest
		}
	}
	return nil
}

func (u *uniRun) readFile(ctx context.Context, r io.Reader, name string) error {
	br := bufio.NewReaderSize(r, 64<<10)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	head, _ := br.Peek(4096)
	if t := bytes.TrimSpace(head); len(t) > 0 && (t[0] == '[' || t[0] == '{') {
		// Playnite libraries belong on the Games page.
		if bytes.Contains(head, []byte(`"PluginId"`)) || (bytes.Contains(head, []byte(`"Playtime"`)) && bytes.Contains(head, []byte(`"GameId"`))) {
			if err := u.flush(ctx); err != nil {
				return err
			}
			res, err := u.e.ImportGames(ctx, br)
			if res != nil {
				u.shelfDone(res)
			}
			return err
		}
		return u.readJSON(ctx, br, name)
	}
	if t := bytes.TrimSpace(head); len(t) > 0 && t[0] == '<' {
		if err := u.flush(ctx); err != nil {
			return err
		}
		res, err := u.e.ImportCLZ(ctx, br, "clz:"+path.Base(name))
		if res != nil {
			u.formats["CLZ Comics"] = true
			u.res.Items += res.Rows
			u.res.Comics += res.New + res.Updated
		}
		return err
	}
	return u.readCSV(ctx, br, head, name)
}

// ---------- JSON (Trakt, Simkl and similar) ----------

func (u *uniRun) readJSON(ctx context.Context, r io.Reader, name string) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	hint := strings.ToLower(name)
	each := func(h string) error {
		for dec.More() {
			var v any
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if err := u.jsonItem(ctx, v, h); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	if tok == json.Delim('[') {
		return each(hint)
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		var peek json.RawMessage
		if err := dec.Decode(&peek); err != nil {
			return err
		}
		h := hint + "/" + strings.ToLower(key)
		if t := bytes.TrimSpace(peek); len(t) > 0 && t[0] == '[' {
			sub := json.NewDecoder(bytes.NewReader(peek))
			sub.UseNumber()
			var list []any
			if err := sub.Decode(&list); err != nil {
				return err
			}
			for _, v := range list {
				if err := u.jsonItem(ctx, v, h); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func uObj(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func uStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		}
	}
	return ""
}

func uNum(m map[string]any, keys ...string) float64 {
	f, _ := strconv.ParseFloat(uStr(m, keys...), 64)
	return f
}

func uIDs(m map[string]any) model.IDs {
	ids := uObj(m, "ids")
	if ids == nil {
		return model.IDs{}
	}
	n := func(k string) int { v, _ := strconv.Atoi(uStr(ids, k)); return v }
	return model.IDs{IMDB: uStr(ids, "imdb"), TMDB: n("tmdb"), TVDB: n("tvdb"), Trakt: n("trakt")}
}

func uTime(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if t := shelfTime(uStr(m, k)); t > 0 {
			return t
		}
	}
	return 0
}

func uSource(hint string, el map[string]any) string {
	for _, k := range []string{"movie", "show", "anime", "episode"} {
		if ids := uObj(uObj(el, k), "ids"); ids != nil {
			if uStr(ids, "simkl", "simkl_id") != "" {
				return "simkl-import"
			}
			if uStr(ids, "trakt") != "" {
				return "trakt-import"
			}
		}
	}
	for _, s := range []string{"trakt", "simkl"} {
		if strings.Contains(hint, s) {
			return s + "-import"
		}
	}
	return "json-import"
}

// uKeptKind names records that aren't plays, from the file or section name.
func uKeptKind(hint string, el map[string]any) string {
	if _, ok := el["listed_at"]; ok || strings.Contains(hint, "watchlist") {
		return "watchlist"
	}
	for _, k := range []string{"collection", "comment", "hidden", "like", "recommend", "list", "network", "follow"} {
		if strings.Contains(hint, k) {
			return k
		}
	}
	return ""
}

func (u *uniRun) jsonItem(ctx context.Context, v any, hint string) error {
	el, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	source := uSource(hint, el)
	u.formats[strings.TrimSuffix(source, "-import")] = true
	raw, _ := json.Marshal(el)
	keep := func(kind, title string) error {
		return u.emit(ctx, uniRec{source: source, kind: kind, title: title, raw: raw})
	}
	kept := uKeptKind(hint, el)
	status := strings.ToLower(uStr(el, "status"))
	if status == "plantowatch" || status == "plan_to_watch" {
		kept = "watchlist"
	}
	at := uTime(el, "watched_at", "last_watched_at")
	rating := uNum(el, "user_rating", "rating")
	ratedAt := uTime(el, "rated_at", "user_rated_at")
	if status == "dropped" || status == "hold" {
		at = 0 // not finished, so its last date isn't a completed watch
	}

	movie, show, ep := uObj(el, "movie"), uObj(el, "show"), uObj(el, "episode")
	if show == nil {
		show = uObj(el, "anime")
	}
	if kept != "" {
		title := uStr(movie, "title")
		if title == "" {
			title = uStr(show, "title")
		}
		return keep(kept, title)
	}
	year := func(m map[string]any) int { y, _ := strconv.Atoi(uStr(m, "year")); return y }
	switch {
	case movie != nil && uStr(movie, "title") != "":
		m := model.Media{Kind: model.Movie, Title: uStr(movie, "title"), Year: year(movie), IDs: uIDs(movie)}
		return u.emit(ctx, uniRec{media: &m, at: at, rating: rating, ratedAt: ratedAt, source: source})
	case ep != nil && show != nil && uStr(show, "title") != "":
		if uObj(el, "season") != nil && uStr(ep, "number") == "" {
			return keep("rating", uStr(show, "title"))
		}
		m := uEpisode(show, int(uNum(ep, "season")), int(uNum(ep, "number")), uStr(ep, "title"), uIDs(ep), year(show))
		return u.emit(ctx, uniRec{media: &m, at: at, rating: rating, ratedAt: ratedAt, source: source})
	case show != nil && uStr(show, "title") != "":
		seasons, _ := el["seasons"].([]any)
		count := 0
		for _, sv := range seasons {
			sm, _ := sv.(map[string]any)
			eps, _ := sm["episodes"].([]any)
			for _, ev := range eps {
				em, _ := ev.(map[string]any)
				if em == nil {
					continue
				}
				when := uTime(em, "watched_at", "last_watched_at")
				if when == 0 {
					continue
				}
				m := uEpisode(show, int(uNum(sm, "number")), int(uNum(em, "number")), uStr(em, "title"), model.IDs{}, year(show))
				if err := u.emit(ctx, uniRec{media: &m, at: when, source: source}); err != nil {
					return err
				}
				count++
			}
		}
		// A rating or status on the whole show has no single episode to sit on.
		if count == 0 || rating > 0 {
			return keep("show", uStr(show, "title"))
		}
		return nil
	}
	return keep("other", uStr(el, "title", "name"))
}

func uEpisode(show map[string]any, season, number int, title string, ids model.IDs, year int) model.Media {
	if title == "" {
		title = "Episode " + strconv.Itoa(number)
	}
	return model.Media{Kind: model.Episode, Title: title, ShowTitle: uStr(show, "title"), Season: season, Episode: number,
		Year: year, IDs: ids, ShowIDs: uIDs(show)}
}

// ---------- CSV ----------

var uniColumns = map[string][]string{
	"title":   {"title", "name", "movie", "moviename", "movietitle"},
	"show":    {"show", "showname", "showtitle", "series", "seriesname", "seriestitle"},
	"season":  {"season", "seasonnumber"},
	"episode": {"episode", "episodenumber", "ep"},
	"eptitle": {"episodetitle", "episodename"},
	"year":    {"year", "releaseyear"},
	"type":    {"titletype", "type", "mediatype"},
	"watched": {"watcheddate", "datewatched", "watchedat", "lastwatchedat", "lastwatched", "date", "watchdate"},
	"rating":  {"yourrating", "myrating", "userrating", "personalrating", "rating"},
	"rated":   {"daterated", "ratedat"},
	"imdb":    {"const", "imdbid", "imdb"},
	"tmdb":    {"tmdbid", "tmdb"},
	"tvdb":    {"tvdbid", "tvdb"},
	"trakt":   {"traktid", "trakt"},
	"uri":     {"letterboxduri"},
}

func (u *uniRun) readCSV(ctx context.Context, br *bufio.Reader, head []byte, name string) error {
	// Spooled to disk, because plain viewing histories go to the streaming
	// importer and it has to read from the start.
	tmp, err := os.CreateTemp("", "backtrackarr-csv-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, br); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	cr := csv.NewReader(bufio.NewReader(tmp))
	cr.Comma = detectComma(head)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	// Goodreads and StoryGraph exports belong on the Books page.
	bookFile := false
	for _, h := range header {
		if n := normHeader(h); n == "exclusiveshelf" || n == "readstatus" {
			bookFile = true
		}
	}
	if bookFile {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := u.flush(ctx); err != nil {
			return err
		}
		res, err := u.e.ImportBooks(ctx, tmp, "")
		if res != nil {
			u.shelfDone(res)
		}
		return err
	}
	col := shelfColumns(header, uniColumns)
	lower := strings.ToLower(path.Base(name))
	_, hasRating := col["rating"]
	_, hasURI := col["uri"]
	_, hasShow := col["show"]
	_, hasTitle := col["title"]
	imdb := false
	if i, ok := col["imdb"]; ok && normHeader(header[i]) == "const" {
		imdb = true
	}
	if !hasRating && !hasURI && !hasShow && !imdb {
		// A plain viewing history (Netflix and friends).
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := u.flush(ctx); err != nil {
			return err
		}
		res, err := u.e.ImportStreaming(ctx, tmp, StreamImport{User: u.user}, u.report)
		if err != nil {
			return err
		}
		u.formats[res.Service] = true
		u.res.Items += res.Rows
		u.res.Plays += res.Added
		u.res.Duplicates += res.Duplicates
		u.res.Skipped += res.Skipped
		return nil
	}
	if !hasTitle && !hasShow {
		return errors.New("no title column")
	}
	source := "csv-import"
	switch {
	case hasURI:
		source = "letterboxd-import"
	case imdb:
		source = "imdb-import"
	case strings.Contains(lower, "simkl"):
		source = "simkl-import"
	case strings.Contains(lower, "trakt"):
		source = "trakt-import"
	}
	u.formats[strings.TrimSuffix(source, "-import")] = true
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			u.res.Skipped++
			continue
		}
		get := func(f string) string {
			if i, ok := col[f]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if err := u.csvRow(ctx, get, header, rec, source, lower, hasURI, imdb); err != nil {
			return err
		}
	}
}

func uDate(s string) int64 {
	if t, ok := parseStreamDate(s, false); ok {
		return t.Unix()
	}
	return 0
}

func (u *uniRun) csvRow(ctx context.Context, get func(string) string, header, rec []string, source, file string, letterboxd, imdb bool) error {
	title, showName := get("title"), get("show")
	if title == "" && showName == "" {
		return nil
	}
	rawMap := map[string]string{}
	for i, h := range header {
		if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
			rawMap[strings.TrimSpace(h)] = rec[i]
		}
	}
	raw, _ := json.Marshal(rawMap)
	keep := func(kind string) error {
		return u.emit(ctx, uniRec{source: source, kind: kind, title: title + showName, raw: raw})
	}
	for _, k := range []string{"watchlist", "likes", "comments", "reviews", "lists"} {
		if strings.Contains(file, k) {
			return keep(strings.TrimSuffix(k, "s"))
		}
	}
	if strings.Contains(strings.ToLower(get("watched")+get("type")), "plantowatch") {
		return keep("watchlist")
	}
	rating, _ := strconv.ParseFloat(strings.Split(get("rating"), "/")[0], 64)
	if letterboxd {
		rating *= 2 // Letterboxd rates out of 5
	}
	at, ratedAt := uDate(get("watched")), uDate(get("rated"))
	if letterboxd && strings.Contains(file, "ratings") || imdb {
		// There, the date is when it was rated, not when it was watched.
		if ratedAt == 0 {
			ratedAt = at
		}
		at = 0
	}
	year, _ := strconv.Atoi(get("year"))
	num := func(k string) int { n, _ := strconv.Atoi(get(k)); return n }
	ids := model.IDs{IMDB: get("imdb"), TMDB: num("tmdb"), TVDB: num("tvdb"), Trakt: num("trakt")}
	typ := strings.ToLower(get("type"))
	var m model.Media
	switch {
	case showName != "" || strings.Contains(typ, "episode"):
		if showName == "" {
			it := ParseStreamTitle(title, StreamItem{})
			if it.Show == "" {
				return keep("other")
			}
			showName, title = it.Show, it.EpisodeTitle
		}
		ep := get("eptitle")
		if ep == "" {
			ep = "Episode " + get("episode")
		}
		m = model.Media{Kind: model.Episode, ShowTitle: showName, Title: ep, Season: num("season"), Episode: num("episode"), Year: year, ShowIDs: ids}
	case strings.Contains(typ, "tv") || strings.Contains(typ, "show") || strings.Contains(typ, "series") || strings.Contains(typ, "anime"):
		return keep("show") // a whole-show rating or status
	case imdb || letterboxd || strings.Contains(typ, "movie") || strings.Contains(typ, "film"):
		m = model.Media{Kind: model.Movie, Title: title, Year: year, IDs: ids}
	default:
		it := ParseStreamTitle(title, StreamItem{Year: year})
		if it.Kind == model.Episode && it.Show != "" {
			m = model.Media{Kind: model.Episode, ShowTitle: it.Show, Title: it.EpisodeTitle, Season: it.Season, Episode: it.Episode, Year: year, ShowIDs: ids}
			if m.Title == "" {
				m.Title = "Episode " + strconv.Itoa(it.Episode)
			}
		} else {
			m = model.Media{Kind: model.Movie, Title: title, Year: year, IDs: ids}
		}
	}
	if at == 0 && rating <= 0 {
		return keep("other") // no watch date or rating to record
	}
	return u.emit(ctx, uniRec{media: &m, at: at, rating: rating, ratedAt: ratedAt, source: source})
}

// BrowserHistoryItem is one row captured from a signed-in history page.
type BrowserHistoryItem struct {
	Title string `json:"title"`
	Date  string `json:"date"`
}

// ImportBrowserHistory stores history the browser script read from a page
// you're signed in to. BackTrackarr never sees that site's password.
func (e *Engine) ImportBrowserHistory(ctx context.Context, service, user string, items []BrowserHistoryItem) (*StreamResult, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"Title", "Date"})
	for _, it := range items {
		if strings.TrimSpace(it.Title) != "" && strings.TrimSpace(it.Date) != "" {
			_ = w.Write([]string{it.Title, it.Date})
		}
	}
	w.Flush()
	return e.ImportStreaming(ctx, &b, StreamImport{Service: service, User: user}, nil)
}
