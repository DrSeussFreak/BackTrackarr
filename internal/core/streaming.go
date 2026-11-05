package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// Streaming services (Netflix, Disney+, Hulu, Prime Video, Max, Paramount+,
// Peacock, Apple TV+, Crunchyroll...) have no public watch-history API.
// Plays come in three ways, all handled here:
//   - history exports (CSV/JSON) uploaded or dropped in STREAMING_IMPORT_DIR
//   - live events from the browser userscript or anything else, via the
//     scrobble API (POST /api/scrobble/{token})
//   - Jellyfin and Emby webhooks (POST /webhook/jellyfin/{token})

// Services lists the names offered in the UI; any other name works too.
var Services = []string{"Netflix", "Disney+", "Hulu", "Prime Video", "Max", "Paramount+", "Peacock", "Apple TV+",
	"Crunchyroll", "Discovery+", "Starz", "MGM+", "AMC+", "Tubi", "Pluto TV", "Roku Channel", "Shudder", "BritBox",
	"Criterion Channel", "Mubi", "YouTube", "Other"}

func serviceSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("+", "plus", " ", "", ".", "", "'", "").Replace(s)
	if s == "" {
		return "streaming"
	}
	return s
}

// ---------- title parsing ----------

var (
	reSxE      = regexp.MustCompile(`(?i)^(.*?)[\s:\-–—·|,]*\bS(?:eason)?\s*(\d{1,2})\s*[:.,]?\s*E(?:p(?:isode)?\.?)?\s*(\d{1,4})\b[\s:\-–—·|,.]*(.*)$`)
	reSeasonEp = regexp.MustCompile(`(?i)^(.*?)[\s:\-–—·|,]*\bSeason\s+(\d{1,2})[\s,:]*(?:Episode|Ep\.?)\s*(\d{1,4})\b[\s:\-–—·|,.]*(.*)$`)
	reMarker   = regexp.MustCompile(`(?i)^(season|series|part|volume|vol\.?|book|collection|chapter|staffel|temporada|saison)\s+([0-9]+|[a-z]+)$`)
	reYear     = regexp.MustCompile(`\s*\((19|20)\d{2}\)\s*$`)
)

var wordNums = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8,
	"nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "i": 1, "ii": 2, "iii": 3, "iv": 4, "v": 5, "vi": 6}

func numWord(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return wordNums[strings.ToLower(s)]
}

// ParseStreamTitle splits titles the way streaming services write them:
//
//	"Stranger Things: Season 1: Chapter One: The Vanishing of Will Byers"
//	"The Queen's Gambit: Limited Series: Openings"
//	"Andor - S01E03 - Reckoning", "Severance S2:E1 Hello, Ms. Cobel"
//	"Fargo: Season 3, Episode 2: The Principle of Restricted Choice"
//
// Anything else is treated as a movie title. Fields already set in base win.
func ParseStreamTitle(raw string, base StreamItem) StreamItem {
	it := base
	raw = strings.TrimSpace(raw)
	if it.Title == "" {
		it.Title = raw
	}
	set := func(show string, season, ep int, title string) {
		if it.Show == "" {
			it.Show = strings.TrimSpace(show)
		}
		if it.Season == 0 {
			it.Season = season
		}
		if it.Episode == 0 {
			it.Episode = ep
		}
		if it.EpisodeTitle == "" {
			it.EpisodeTitle = strings.TrimSpace(title)
		}
		it.Kind = model.Episode
	}
	if m := reSeasonEp.FindStringSubmatch(raw); m != nil && strings.TrimSpace(m[1]) != "" {
		s, _ := strconv.Atoi(m[2])
		e, _ := strconv.Atoi(m[3])
		set(m[1], s, e, m[4])
		return it
	}
	if m := reSxE.FindStringSubmatch(raw); m != nil && strings.TrimSpace(m[1]) != "" {
		s, _ := strconv.Atoi(m[2])
		e, _ := strconv.Atoi(m[3])
		set(m[1], s, e, m[4])
		return it
	}
	segs := strings.Split(raw, ": ")
	for i := 1; i < len(segs); i++ {
		seg := strings.TrimSpace(segs[i])
		low := strings.ToLower(seg)
		season := 0
		if low == "limited series" || low == "miniseries" || low == "mini-series" || low == "the complete series" {
			season = 1
		} else if m := reMarker.FindStringSubmatch(seg); m != nil {
			season = numWord(m[2])
		}
		if season > 0 {
			set(strings.Join(segs[:i], ": "), season, 0, strings.Join(segs[i+1:], ": "))
			return it
		}
	}
	if it.Kind == "" {
		it.Kind = model.Movie
		if m := reYear.FindString(it.Title); m != "" && it.Year == 0 {
			it.Year, _ = strconv.Atoi(strings.Trim(strings.TrimSpace(m), "()"))
			it.Title = strings.TrimSpace(strings.TrimSuffix(it.Title, m))
		}
	}
	return it
}

// ---------- export import ----------

var streamColumns = map[string][]string{
	"date":     {"date", "datewatched", "watchedat", "watched", "watchdate", "starttime", "starttimeutc", "playbackhour", "playbackhourutc", "playbackstarttime", "playbackstartdatetime", "timestamp", "time", "viewdate", "viewedat", "viewed", "lastwatched", "lastwatchedat", "datetime", "watchedon"},
	"title":    {"title", "name", "programtitle", "contenttitle", "videotitle", "movietitle", "programname", "assettitle", "itemtitle"},
	"show":     {"show", "series", "seriesname", "showtitle", "showname", "seriestitle", "parenttitle", "programseries"},
	"season":   {"season", "seasonnumber", "seasonno", "seasonnum"},
	"episode":  {"episode", "episodenumber", "episodeno", "episodenum", "ep"},
	"eptitle":  {"episodetitle", "episodename", "subtitle"},
	"user":     {"profilename", "profile", "user", "username", "viewer", "account"},
	"type":     {"type", "contenttype", "mediatype", "videotype", "programtype", "assettype"},
	"duration": {"duration", "watchtime", "durationwatched", "playbackduration", "minuteswatched", "secondswatched", "timewatched", "viewingduration"},
	"supp":     {"supplementalvideotype"},
	"year":     {"year", "releaseyear"},
	"imdb":     {"imdb", "imdbid"},
	"tmdb":     {"tmdb", "tmdbid"},
	"tvdb":     {"tvdb", "tvdbid"},
}

type StreamImport struct {
	Service string // Netflix, Disney+, ... (used as the play's source)
	User    string // local user; when empty, the export's profile column is used
	Profile string // only import rows of this profile (Netflix has one file for all)
}

type StreamResult struct {
	Rows, Added, Duplicates, Skipped, Unmatched int
	Service                                     string
	Profiles                                    []string
}

type streamRow struct {
	item     StreamItem
	at       time.Time
	user     string
	duration time.Duration
}

func rowsFromReader(r io.Reader) ([]string, [][]string, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(4096)
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	if t := bytes.TrimSpace(head); len(t) > 0 && (t[0] == '[' || t[0] == '{') {
		return jsonRows(br)
	}
	cr := csv.NewReader(br)
	cr.Comma = detectComma(head)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	all, err := cr.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(all) == 0 {
		return nil, nil, fmt.Errorf("empty file")
	}
	return all[0], all[1:], nil
}

// jsonRows flattens a JSON array of objects (or {"items": [...]}-style
// wrappers) into header + rows.
func jsonRows(r io.Reader) ([]string, [][]string, error) {
	var v any
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, nil, err
	}
	var list []any
	switch x := v.(type) {
	case []any:
		list = x
	case map[string]any:
		for _, val := range x {
			if l, ok := val.([]any); ok && len(l) > len(list) {
				list = l
			}
		}
	}
	idx := map[string]int{}
	var header []string
	var rows [][]string
	for _, el := range list {
		obj, ok := el.(map[string]any)
		if !ok {
			continue
		}
		row := make([]string, len(header))
		for k, val := range obj {
			i, ok := idx[k]
			if !ok {
				i = len(header)
				idx[k] = i
				header = append(header, k)
				for j := range rows {
					rows[j] = append(rows[j], "")
				}
				row = append(row, "")
			}
			switch t := val.(type) {
			case string:
				row[i] = t
			case float64:
				row[i] = strconv.FormatFloat(t, 'f', -1, 64)
			case bool:
				row[i] = strconv.FormatBool(t)
			}
		}
		rows = append(rows, row)
	}
	if len(header) == 0 {
		return nil, nil, fmt.Errorf("no records found in the JSON")
	}
	return header, rows, nil
}

var streamDateLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04:05 MST",
	"2006-01-02 15:04", "2006-01-02T15:04:05.000Z", "2006-01-02", "2006/01/02", "Jan 2, 2006", "January 2, 2006",
	"2 Jan 2006", "2 January 2006", "Mon, 2 Jan 2006 15:04:05 MST", "Jan 2, 2006, 3:04:05 PM", "Jan 2, 2006 3:04 PM"}

// parseStreamDate parses export dates. dayFirst selects 02/01/2006 over
// 01/02/2006 for slash dates (decided per file, see detectDayFirst).
func parseStreamDate(s string, dayFirst bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 100000000 {
		if n > 1e12 {
			return time.UnixMilli(n), true
		}
		return time.Unix(n, 0), true
	}
	for _, l := range streamDateLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			if len(s) <= 10 { // date only: noon local, so the day doesn't shift
				return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, time.Local), true
			}
			return t, true
		}
	}
	slash := []string{"1/2/06", "1/2/2006", "1/2/2006 15:04", "1/2/2006 3:04 PM", "1/2/2006 15:04:05"}
	if dayFirst {
		slash = []string{"2/1/06", "2/1/2006", "2/1/2006 15:04", "2/1/2006 3:04 PM", "2/1/2006 15:04:05"}
	}
	s2 := strings.ReplaceAll(strings.ReplaceAll(s, ".", "/"), "-", "/")
	for _, l := range slash {
		if t, err := time.ParseInLocation(l, s2, time.Local); err == nil {
			if !strings.Contains(l, ":") {
				t = t.Add(12 * time.Hour)
			}
			return t, true
		}
	}
	return time.Time{}, false
}

// detectDayFirst looks for a slash date whose first part can't be a month.
func detectDayFirst(vals []string) bool {
	for _, v := range vals {
		v = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(v), ".", "/"), "-", "/")
		parts := strings.Split(v, "/")
		if len(parts) >= 3 && len(parts[0]) <= 2 {
			if a, _ := strconv.Atoi(parts[0]); a > 12 {
				return true
			}
		}
	}
	return false
}

func parseDuration(s string, header string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.Count(s, ":") >= 1 {
		parts := strings.Split(s, ":")
		total := 0.0
		for _, p := range parts {
			f, _ := strconv.ParseFloat(strings.TrimSpace(p), 64)
			total = total*60 + f
		}
		if len(parts) == 2 { // MM:SS
			return time.Duration(total) * time.Second
		}
		return time.Duration(total) * time.Second
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if d, err := time.ParseDuration(strings.ReplaceAll(s, " ", "")); err == nil {
			return d
		}
		return 0
	}
	if strings.Contains(header, "minute") {
		return time.Duration(f * float64(time.Minute))
	}
	return time.Duration(f * float64(time.Second))
}

// ImportStreaming reads a streaming service's history export.
func (e *Engine) ImportStreaming(ctx context.Context, r io.Reader, opt StreamImport, report func(string)) (*StreamResult, error) {
	header, rows, err := rowsFromReader(r)
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	norm := make([]string, len(header))
	for i, h := range header {
		norm[i] = normHeader(h)
		for field, names := range streamColumns {
			if _, done := col[field]; done {
				continue
			}
			for _, n := range names {
				if norm[i] == n {
					col[field] = i
				}
			}
		}
	}
	_, hasTitle := col["title"]
	_, hasShow := col["show"]
	if !hasTitle && !hasShow {
		return nil, fmt.Errorf("no Title column found (columns: %s)", strings.Join(header, ", "))
	}
	if _, ok := col["date"]; !ok {
		return nil, fmt.Errorf("no date column found (columns: %s)", strings.Join(header, ", "))
	}
	res := &StreamResult{Service: opt.Service}
	if res.Service == "" {
		res.Service = "Streaming"
		if _, ok := col["supp"]; ok || (len(header) == 2 && norm[0] == "title" && norm[1] == "date") {
			res.Service = "Netflix"
		}
	}
	get := func(row []string, f string) string {
		if i, ok := col[f]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	var dates []string
	for _, row := range rows {
		dates = append(dates, get(row, "date"))
	}
	dayFirst := detectDayFirst(dates)
	durHeader := ""
	if i, ok := col["duration"]; ok {
		durHeader = norm[i]
	}

	profiles := map[string]bool{}
	var parsed []streamRow
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		res.Rows++
		if p := get(row, "user"); p != "" {
			profiles[p] = true
			if opt.Profile != "" && !strings.EqualFold(p, opt.Profile) {
				res.Skipped++
				continue
			}
		}
		if get(row, "supp") != "" { // Netflix trailers, previews, recaps
			res.Skipped++
			continue
		}
		at, ok := parseStreamDate(get(row, "date"), dayFirst)
		if !ok {
			res.Skipped++
			continue
		}
		d := parseDuration(get(row, "duration"), durHeader)
		if _, has := col["duration"]; has && d > 0 && d < 5*time.Minute {
			res.Skipped++ // sampling, not watching
			continue
		}
		it := StreamItem{Title: get(row, "title"), Show: get(row, "show"), EpisodeTitle: get(row, "eptitle")}
		it.Season, _ = strconv.Atoi(get(row, "season"))
		if ep := get(row, "episode"); ep != "" {
			if n, err := strconv.Atoi(ep); err == nil {
				it.Episode = n
			} else if it.EpisodeTitle == "" {
				it.EpisodeTitle = ep
			}
		}
		it.Year, _ = strconv.Atoi(get(row, "year"))
		it.IDs.IMDB = get(row, "imdb")
		it.IDs.TMDB, _ = strconv.Atoi(get(row, "tmdb"))
		typ := strings.ToLower(get(row, "type"))
		switch {
		case it.Show != "":
			it.Kind = model.Episode
			if it.EpisodeTitle == "" && it.Title != it.Show {
				it.EpisodeTitle = it.Title
			}
		case strings.Contains(typ, "movie") || strings.Contains(typ, "film"):
			it.Kind = model.Movie
		}
		if it.Kind == "" || (it.Kind == model.Episode && it.Show == "") {
			it = ParseStreamTitle(it.Title, it)
		}
		user := opt.User
		if user == "" {
			user = get(row, "user")
		}
		if user == "" {
			user = e.defaultUser(ctx)
		}
		parsed = append(parsed, streamRow{item: it, at: at, user: user, duration: d})
	}
	for p := range profiles {
		res.Profiles = append(res.Profiles, p)
	}
	source := serviceSlug(res.Service) + "-import"
	private := e.IsPrivateSection(res.Service, "")
	cache := map[string]model.Media{}
	for i, r := range parsed {
		key := fmt.Sprintf("%s|%s|%d|%d|%s|%d", r.item.Show, r.item.Title, r.item.Season, r.item.Episode, r.item.EpisodeTitle, r.item.Year)
		m, ok := cache[key]
		if !ok {
			var err error
			if m, err = e.ResolveStream(ctx, r.item); err != nil {
				res.Skipped++
				continue
			}
			cache[key] = m
		}
		if (m.Kind == model.Movie && m.IDs.Empty()) || (m.Kind == model.Episode && m.ShowIDs.Empty()) {
			res.Unmatched++
		}
		ins, err := e.st.RecordPlay(ctx, m.ID, r.user, r.at, source, res.Service, private)
		if err != nil {
			return res, err
		}
		if ins {
			res.Added++
		} else {
			res.Duplicates++
		}
		if report != nil && (i+1)%200 == 0 {
			report(fmt.Sprintf("%s: %d of %d rows, %d new plays", res.Service, i+1, len(parsed), res.Added))
		}
	}
	return res, nil
}

// defaultUser is who streaming plays belong to when nothing says otherwise.
func (e *Engine) defaultUser(ctx context.Context) string {
	if e.cfg.TrackerPullUser != "" {
		return e.cfg.TrackerPullUser
	}
	if len(e.cfg.ScrobbleUsers) > 0 {
		return e.cfg.ScrobbleUsers[0]
	}
	if e.plex != nil {
		if acc, err := e.plex.Accounts(ctx); err == nil && acc["1"] != "" {
			return acc["1"]
		}
	}
	return "me"
}

// RunStreamWatcher imports exports dropped in STREAMING_IMPORT_DIR. A file
// in a subfolder named after a service (netflix/, disney+/, hulu/...) is
// tagged with that service.
func (e *Engine) RunStreamWatcher(ctx context.Context) {
	dir := e.cfg.StreamImportDir
	if err := os.MkdirAll(filepath.Join(dir, "imported"), 0o755); err != nil {
		e.log.Warn("streaming import folder", "err", err)
		return
	}
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if d != nil && d.IsDir() && d.Name() == "imported" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext != ".csv" && ext != ".json" && ext != ".txt" && ext != ".tsv" {
				return nil
			}
			service := ""
			if rel, _ := filepath.Rel(dir, filepath.Dir(p)); rel != "." {
				service = serviceName(rel)
			}
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			res, err := e.ImportStreaming(ctx, f, StreamImport{Service: service}, nil)
			f.Close()
			if err != nil {
				e.log.Warn("streaming import", "file", p, "err", err)
				return nil
			}
			e.log.Info("streaming import", "file", filepath.Base(p), "service", res.Service, "rows", res.Rows, "new", res.Added)
			_ = os.Rename(p, filepath.Join(dir, "imported", time.Now().Format("20060102-150405-")+filepath.Base(p)))
			return nil
		})
		t.Reset(time.Hour)
	}
}

func serviceName(folder string) string {
	slug := serviceSlug(filepath.Base(folder))
	for _, s := range Services {
		if serviceSlug(s) == slug {
			return s
		}
	}
	return filepath.Base(folder)
}

// ---------- live events (scrobble API, userscript) ----------

// ScrobbleRequest is the body of POST /api/scrobble/{token}. Send either the
// structured fields or just raw_title (and optionally subtitle) and let
// BackTrackarr parse it.
type ScrobbleRequest struct {
	Action       string    `json:"action"` // start | pause | stop | watched
	Service      string    `json:"service"`
	User         string    `json:"user"`
	Kind         string    `json:"kind"` // movie | episode
	Title        string    `json:"title"`
	Year         int       `json:"year"`
	Show         string    `json:"show"`
	Season       int       `json:"season"`
	Episode      int       `json:"episode"`
	EpisodeTitle string    `json:"episode_title"`
	RawTitle     string    `json:"raw_title"`
	Subtitle     string    `json:"subtitle"`
	Progress     float64   `json:"progress"` // 0-100
	Position     float64   `json:"position"` // seconds, with Duration as an alternative to Progress
	Duration     float64   `json:"duration"`
	IDs          model.IDs `json:"ids"`
	ShowIDs      model.IDs `json:"show_ids"`
	Device       string    `json:"device"`
}

var scrobbleActions = map[string]string{"start": model.ActStart, "play": model.ActStart, "playing": model.ActStart,
	"resume": model.ActStart, "pause": model.ActPause, "paused": model.ActPause, "stop": model.ActStop,
	"stopped": model.ActStop, "ended": model.ActStop, "watched": model.ActWatched, "scrobble": model.ActWatched}

func (e *Engine) HandleScrobble(ctx context.Context, req ScrobbleRequest) (model.Media, error) {
	act, ok := scrobbleActions[strings.ToLower(req.Action)]
	if !ok {
		return model.Media{}, fmt.Errorf("unknown action %q", req.Action)
	}
	it := StreamItem{Kind: req.Kind, Title: req.Title, Year: req.Year, Show: req.Show, Season: req.Season,
		Episode: req.Episode, EpisodeTitle: req.EpisodeTitle, IDs: req.IDs, ShowIDs: req.ShowIDs}
	if req.RawTitle != "" && it.Title == "" && it.Show == "" {
		raw := req.RawTitle
		if req.Subtitle != "" {
			raw += ": " + req.Subtitle
			// "S1:E3 Title" / "S1 E3 · Title" / "Season 1, Ep. 3 Title" subtitles
			sub := ParseStreamTitle("x "+req.Subtitle, StreamItem{})
			if sub.Kind == model.Episode {
				it = StreamItem{Kind: model.Episode, Show: req.RawTitle, Season: sub.Season, Episode: sub.Episode,
					EpisodeTitle: sub.EpisodeTitle, IDs: req.IDs, ShowIDs: req.ShowIDs}
			}
		}
		if it.Show == "" {
			it = ParseStreamTitle(raw, it)
		}
	}
	if it.Title == "" && it.Show == "" {
		return model.Media{}, fmt.Errorf("missing title")
	}
	m, err := e.ResolveStream(ctx, it)
	if err != nil {
		return m, err
	}
	progress := req.Progress
	if progress == 0 && req.Duration > 0 {
		progress = math.Min(100, req.Position*100/req.Duration)
	}
	service := req.Service
	if service == "" {
		service = "Streaming"
	}
	user := req.User
	if user == "" {
		user = e.defaultUser(ctx)
	}
	player := service
	if req.Device != "" {
		player += " · " + req.Device
	}
	ev := model.Event{Action: act, Media: m, User: user, Player: player, Progress: progress,
		At: time.Now(), Source: serviceSlug(service), Private: e.IsPrivateSection(service, "")}
	// Streaming events have no Plex owner flag; send them out only when the
	// user is allowed (SCROBBLE_USERS, or anyone when it's empty).
	ev.Owner = len(e.cfg.ScrobbleUsers) == 0
	return m, e.Ingest(ctx, ev, false)
}

// ---------- Jellyfin / Emby webhooks ----------

// HandleMediaServerWebhook accepts the Jellyfin webhook plugin's JSON
// (Generic destination, NotificationType/ItemType/Provider_* fields) and
// Emby's native webhooks (Event/Item/User/PlaybackInfo).
func (e *Engine) HandleMediaServerWebhook(ctx context.Context, raw []byte) error {
	var j map[string]any
	if err := json.Unmarshal(raw, &j); err != nil {
		return fmt.Errorf("bad payload: %w", err)
	}
	str := func(m map[string]any, k string) string {
		switch v := m[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	num := func(m map[string]any, k string) float64 {
		f, _ := strconv.ParseFloat(str(m, k), 64)
		return f
	}
	sub := func(m map[string]any, k string) map[string]any {
		x, _ := m[k].(map[string]any)
		if x == nil {
			x = map[string]any{}
		}
		return x
	}
	var req ScrobbleRequest
	var event, itemType string
	var completed bool
	if _, ok := j["NotificationType"]; ok { // Jellyfin
		req.Service = "Jellyfin"
		event, itemType = str(j, "NotificationType"), str(j, "ItemType")
		req.User = str(j, "NotificationUsername")
		req.Title, req.Show = str(j, "Name"), str(j, "SeriesName")
		req.Season, req.Episode = int(num(j, "SeasonNumber")), int(num(j, "EpisodeNumber"))
		req.Year = int(num(j, "Year"))
		req.IDs = model.IDs{IMDB: str(j, "Provider_imdb")}
		req.IDs.TMDB, _ = strconv.Atoi(str(j, "Provider_tmdb"))
		req.IDs.TVDB, _ = strconv.Atoi(str(j, "Provider_tvdb"))
		req.Position, req.Duration = num(j, "PlaybackPositionTicks")/1e7, num(j, "RunTimeTicks")/1e7
		req.Device = str(j, "DeviceName")
		completed = str(j, "PlayedToCompletion") == "true"
	} else if _, ok := j["Event"]; ok { // Emby
		req.Service = "Emby"
		event = str(j, "Event")
		item, user, pb := sub(j, "Item"), sub(j, "User"), sub(j, "PlaybackInfo")
		itemType = str(item, "Type")
		req.User = str(user, "Name")
		req.Title, req.Show = str(item, "Name"), str(item, "SeriesName")
		req.Season, req.Episode = int(num(item, "ParentIndexNumber")), int(num(item, "IndexNumber"))
		req.Year = int(num(item, "ProductionYear"))
		p := sub(item, "ProviderIds")
		req.IDs = model.IDs{IMDB: str(p, "Imdb")}
		req.IDs.TMDB, _ = strconv.Atoi(str(p, "Tmdb"))
		req.IDs.TVDB, _ = strconv.Atoi(str(p, "Tvdb"))
		req.Position, req.Duration = num(pb, "PositionTicks")/1e7, num(item, "RunTimeTicks")/1e7
		req.Device = str(sub(j, "Session"), "DeviceName")
		completed = str(pb, "PlayedToCompletion") == "true"
	} else {
		return fmt.Errorf("not a Jellyfin or Emby webhook")
	}
	switch strings.ToLower(itemType) {
	case "movie":
		req.Kind = model.Movie
	case "episode":
		req.Kind = model.Episode
		// The provider ids on an episode are the episode's own; the show is
		// found by its title (SeriesName).
		req.EpisodeTitle, req.Title = req.Title, ""
	default:
		return nil // music, trailers, live TV...
	}
	switch strings.ToLower(event) {
	case "playbackstart", "playback.start", "playback.unpause":
		req.Action = "start"
	case "playbackprogress", "playback.progress":
		return nil // too chatty; start/stop are enough
	case "playback.pause":
		req.Action = "pause"
	case "playbackstop", "playback.stop":
		req.Action = "stop"
		if completed {
			req.Action = "watched"
		}
	case "userdatasaved", "item.markplayed":
		if strings.ToLower(event) == "userdatasaved" && str(j, "Played") != "true" {
			return nil
		}
		req.Action = "watched" // marked as watched: recorded at the moment it was marked
	default:
		return nil
	}
	_, err := e.HandleScrobble(ctx, req)
	return err
}

// ---------- pulling from Trakt and Simkl ----------

// RunTrackerPull imports new plays from connected Trakt and Simkl accounts
// every TRACKER_PULL_MINUTES. Those services collect streaming plays from
// their own apps, imports and browser extensions (Netflix, Disney+, Prime
// Video, Max, Hulu...), so this brings them in without extra setup. Pulled
// plays are stored, not sent back out, and BackTrackarr's own plays coming
// back are dropped as duplicates.
func (e *Engine) RunTrackerPull(ctx context.Context) {
	t := time.NewTimer(3 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.Jobs.Start(ctx, "pull ← trakt/simkl", e.PullTrackers)
		t.Reset(e.cfg.TrackerPull)
	}
}

func (e *Engine) PullTrackers(ctx context.Context, report func(string)) error {
	user := e.defaultUser(ctx)
	var errs []string
	pull := func(name string, run func(since time.Time, fn func(model.Media, time.Time) error) error) {
		key := "pull|" + name
		v, _ := e.st.GetKV(ctx, key)
		since := time.Now().AddDate(0, 0, -30) // first run: the last month (use Import for everything)
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = time.Unix(ts, 0).Add(-time.Hour) // overlap a little; duplicates are dropped
		}
		start := time.Now()
		added := 0
		err := run(since, func(m model.Media, at time.Time) error {
			if err := e.st.UpsertMedia(ctx, &m); err != nil {
				return err
			}
			ins, err := e.st.RecordPlay(ctx, m.ID, user, at, name+"-pull", "", false)
			if ins {
				added++
			}
			return err
		})
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			return
		}
		_ = e.st.SetKV(ctx, key, strconv.FormatInt(start.Unix(), 10))
		report(fmt.Sprintf("%s: %d new plays for %s", name, added, user))
	}
	if t, ok := e.Sink("trakt").(interface {
		Ready(context.Context) bool
		ImportHistory(context.Context, time.Time, func(model.Media, time.Time) error, func(string)) error
	}); ok && t.Ready(ctx) {
		pull("trakt", func(since time.Time, fn func(model.Media, time.Time) error) error {
			return t.ImportHistory(ctx, since, fn, func(string) {})
		})
	}
	if s, ok := e.Sink("simkl").(interface {
		Ready(context.Context) bool
		ImportHistory(context.Context, time.Time, func(model.Media, time.Time) error) (int, error)
	}); ok && s.Ready(ctx) {
		pull("simkl", func(since time.Time, fn func(model.Media, time.Time) error) error {
			_, err := s.ImportHistory(ctx, since, fn)
			return err
		})
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
