package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// fakePlex is a tiny Plex Media Server + plex.tv stand-in.
type fakePlex struct {
	mu       sync.Mutex
	machine  string
	items    map[string]map[string]any // ratingKey -> metadata
	order    []string
	writes   []string // "METHOD path?query token"
	listHits int
}

func newFakePlex(machine string) *fakePlex {
	f := &fakePlex{machine: machine, items: map[string]map[string]any{}}
	f.add(map[string]any{"ratingKey": "10", "type": "movie", "title": "Heat", "titleSort": "Heat 1995", "year": 1995,
		"guid": "plex://movie/heat", "librarySectionID": 1, "librarySectionTitle": "Movies", "updatedAt": 100, "addedAt": 50,
		"duration": 10200000, "userRating": 9,
		"Guid":  []any{map[string]any{"id": "imdb://tt0113277"}, map[string]any{"id": "tmdb://949"}},
		"Label": []any{map[string]any{"tag": "favorite"}}, "Collection": []any{map[string]any{"tag": "Mann"}},
		"Field": []any{map[string]any{"name": "titleSort", "locked": true}},
		"Media": []any{map[string]any{"Part": []any{map[string]any{"file": "/movies/Heat (1995)/Heat.mkv"}}}}})
	f.add(map[string]any{"ratingKey": "11", "type": "movie", "title": "Ronin", "year": 1998, "guid": "plex://movie/ronin",
		"librarySectionID": 1, "librarySectionTitle": "Movies", "updatedAt": 90, "addedAt": 40, "duration": 7200000,
		"Guid":  []any{map[string]any{"id": "imdb://tt0122690"}},
		"Media": []any{map[string]any{"Part": []any{map[string]any{"file": "/movies/Ronin (1998)/Ronin.mkv"}}}}})
	return f
}

func (f *fakePlex) add(m map[string]any) {
	f.items[m["ratingKey"].(string)] = m
	f.order = append(f.order, m["ratingKey"].(string))
}

func mc(v map[string]any) map[string]any { return map[string]any{"MediaContainer": v} }

func (f *fakePlex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	tok := r.Header.Get("X-Plex-Token")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+p+"?"+r.URL.RawQuery+" "+tok)
		if p == "/playlists" && r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(mc(map[string]any{"Metadata": []any{map[string]any{"ratingKey": "900"}}}))
		}
		return
	}
	var out any
	switch {
	case p == "/identity":
		out = mc(map[string]any{"machineIdentifier": f.machine})
	case p == "/accounts":
		out = mc(map[string]any{"Account": []any{map[string]any{"id": 1, "name": "alice"}, map[string]any{"id": 2, "name": "bob"}}})
	case p == "/library/sections":
		out = mc(map[string]any{"Directory": []any{map[string]any{"key": "1", "type": "movie", "title": "Movies"}}})
	case p == "/library/sections/1/all":
		f.listHits++
		var md []any
		keys := append([]string{}, f.order...)
		if r.URL.Query().Get("sort") == "lastViewedAt:desc" {
			// newest viewed first; never-viewed last
			for i := 0; i < len(keys); i++ {
				for j := i + 1; j < len(keys); j++ {
					a, _ := f.items[keys[i]]["lastViewedAt"].(int64)
					b, _ := f.items[keys[j]]["lastViewedAt"].(int64)
					if b > a {
						keys[i], keys[j] = keys[j], keys[i]
					}
				}
			}
		}
		for _, k := range keys {
			md = append(md, f.items[k])
		}
		out = mc(map[string]any{"size": len(md), "totalSize": len(md), "Metadata": md})
	case strings.HasPrefix(p, "/library/metadata/"):
		var md []any
		for _, k := range strings.Split(strings.TrimPrefix(p, "/library/metadata/"), ",") {
			if it, ok := f.items[k]; ok {
				md = append(md, it)
			}
		}
		out = mc(map[string]any{"Metadata": md})
	case p == "/playlists":
		if tok == "bobtok" {
			out = mc(map[string]any{"size": 0})
			break
		}
		out = mc(map[string]any{"size": 2, "Metadata": []any{
			map[string]any{"ratingKey": "500", "title": "Heist night", "playlistType": "video", "smart": false, "leafCount": 2},
			map[string]any{"ratingKey": "501", "title": "Unwatched 90s", "playlistType": "video", "smart": true,
				"content": "library://abc/directory/%2Flibrary%2Fsections%2F1%2Fall%3Ftype%3D1%26year%3E%3E%3D1990"},
		}})
	case p == "/playlists/500/items":
		out = mc(map[string]any{"size": 2, "Metadata": []any{f.items["10"], f.items["11"]}})
	case p == "/playlists/501/items":
		out = mc(map[string]any{"size": 1, "Metadata": []any{f.items["11"]}})
	case p == "/library/sections/1/collections":
		out = mc(map[string]any{"size": 1, "Metadata": []any{map[string]any{"ratingKey": "700", "title": "Mann", "subtype": "movie"}}})
	case p == "/library/collections/700/children":
		out = mc(map[string]any{"size": 1, "Metadata": []any{f.items["10"]}})
	case strings.HasSuffix(p, "/shared_servers"):
		w.Header().Set("Content-Type", "application/xml")
		io.WriteString(w, `<MediaContainer><SharedServer username="bob" userID="2" accessToken="bobtok"/></MediaContainer>`)
		return
	default:
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(out)
}

func testEngine(t *testing.T, f *fakePlex, cfg *config.Config) (*Engine, *store.Store, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.WatchedPercent = 80
	cfg.PlexAllUsers = true
	px := plex.NewClient(srv.URL, "ownertok")
	px.PlexTV = srv.URL
	px.AllowWrites = cfg.PlexAllowWrites
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(context.Background(), cfg, st, px, nil, log), st, srv.URL
}

func noReport(string) {}

func TestArchiveVersionsAndLists(t *testing.T) {
	f := newFakePlex("srvA")
	eng, st, _ := testEngine(t, f, nil)
	ctx := context.Background()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	it, err := st.GetItemByKey(ctx, "srvA", "10")
	if err != nil || it == nil {
		t.Fatalf("Heat not archived: %v", err)
	}
	if it.TitleSort != "Heat 1995" || len(it.Labels) != 1 || it.Collections[0] != "Mann" || it.Locked[0] != "titleSort" ||
		it.File != "/movies/Heat (1995)/Heat.mkv" || it.UserRating != 9 {
		t.Fatalf("custom fields not kept: %+v", it)
	}
	lists, _ := st.Lists(ctx, "srvA", "", "")
	if len(lists) != 3 {
		t.Fatalf("want 2 playlists + 1 collection, got %d", len(lists))
	}
	var smart *store.PlexList
	for _, l := range lists {
		if l.Smart {
			smart = l
		}
	}
	if smart == nil || !strings.Contains(smart.Content, "directory") {
		t.Fatalf("smart playlist not archived with its filter: %+v", lists)
	}

	// An edit in Plex (sort title changed, updatedAt bumped) keeps the old version.
	f.mu.Lock()
	f.items["10"]["titleSort"] = "Heat (Mann)"
	f.items["10"]["updatedAt"] = 200
	f.mu.Unlock()
	if err := eng.ArchivePlex(ctx, false, noReport); err != nil {
		t.Fatal(err)
	}
	it, _ = st.GetItemByKey(ctx, "srvA", "10")
	vs, _ := st.ItemVersions(ctx, "item", it.ID)
	if it.TitleSort != "Heat (Mann)" || len(vs) != 1 || !strings.Contains(string(vs[0].Data), "Heat 1995") {
		t.Fatalf("edit not versioned: now %q, %d versions", it.TitleSort, len(vs))
	}
	// Watching (viewCount/lastViewedAt) must not create a new version.
	f.mu.Lock()
	f.items["10"]["viewCount"] = 5
	f.items["10"]["lastViewedAt"] = int64(12345)
	f.mu.Unlock()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	vs, _ = st.ItemVersions(ctx, "item", it.ID)
	if len(vs) != 1 {
		t.Fatalf("a play created a metadata version (%d)", len(vs))
	}
	// Deleting from Plex marks, never removes.
	time.Sleep(1100 * time.Millisecond) // seen_at has one-second resolution
	f.mu.Lock()
	delete(f.items, "11")
	f.order = []string{"10"}
	f.mu.Unlock()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	gone, _ := st.GetItemByKey(ctx, "srvA", "11")
	if gone == nil || gone.DeletedAt == 0 {
		t.Fatalf("deleted item should be kept and marked: %+v", gone)
	}
	if len(f.writes) != 0 {
		t.Fatalf("archiving wrote to Plex: %v", f.writes)
	}
}

func TestMarkWatchedRecordedAtPressTime(t *testing.T) {
	f := newFakePlex("srvA")
	eng, st, _ := testEngine(t, f, nil)
	ctx := context.Background()
	if err := eng.ScanWatched(ctx); err != nil { // first run: checkpoint only
		t.Fatal(err)
	}
	press := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	f.mu.Lock()
	f.items["11"]["lastViewedAt"] = press.Unix()
	f.items["11"]["viewCount"] = 1
	f.mu.Unlock()
	// the checkpoint is "now" after the first run, so move it back a bit
	_ = st.SetKV(ctx, "watchscan|alice|1", fmt.Sprint(press.Unix()-60))
	if err := eng.ScanWatched(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.History(ctx, store.HistoryQuery{User: "alice"})
	if len(rows) != 1 || rows[0].Media.Title != "Ronin" || rows[0].WatchedAt != press.Unix() || rows[0].Source != "plex-mark" {
		t.Fatalf("mark-as-watched not recorded at press time: %+v", rows)
	}
	if err := eng.ScanWatched(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.History(ctx, store.HistoryQuery{}); len(rows) != 1 {
		t.Fatalf("rescan duplicated the play: %d", len(rows))
	}
}

func TestCopyNeedsWritesAndCopiesSmart(t *testing.T) {
	f := newFakePlex("srvA")
	eng, st, _ := testEngine(t, f, nil)
	ctx := context.Background()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	lists, _ := st.Lists(ctx, "srvA", "playlist", "alice")
	var static, smart int64
	for _, l := range lists {
		if l.Smart {
			smart = l.ID
		} else {
			static = l.ID
		}
	}
	if err := eng.CopyList(ctx, CopyRequest{ListID: static, ToUser: "bob"}, noReport); !errors.Is(err, plex.ErrReadOnly) {
		t.Fatalf("copy without PLEX_ALLOW_WRITES should be refused, got %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("read-only mode wrote to Plex")
	}
	eng.cfg.PlexAllowWrites = true
	eng.plex.AllowWrites = true
	eng.users = nil // drop the cached clients so they pick up the flag
	if err := eng.CopyList(ctx, CopyRequest{ListID: static, ToUser: "bob"}, noReport); err != nil {
		t.Fatal(err)
	}
	if err := eng.CopyList(ctx, CopyRequest{ListID: smart, ToUser: "bob"}, noReport); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 2 {
		t.Fatalf("want 2 playlist creates, got %v", f.writes)
	}
	if w := f.writes[0]; !strings.Contains(w, "bobtok") || !strings.Contains(w, "smart=0") ||
		!strings.Contains(w, "metadata%2F10%2C11") {
		t.Fatalf("static copy wrong: %s", w)
	}
	if w := f.writes[1]; !strings.Contains(w, "smart=1") || !strings.Contains(w, "server%3A%2F%2FsrvA%2Fcom.plexapp.plugins.library%2Flibrary%2Fsections%2F1%2Fall") {
		t.Fatalf("smart copy should keep the filter: %s", w)
	}
}

func TestRestoreFieldsToNewServer(t *testing.T) {
	old := newFakePlex("old")
	eng, st, _ := testEngine(t, old, nil)
	ctx := context.Background()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	// New server: same files and GUIDs, custom fields gone.
	nw := newFakePlex("new")
	for _, k := range []string{"10", "11"} {
		delete(nw.items[k], "titleSort")
		delete(nw.items[k], "Label")
		delete(nw.items[k], "Collection")
		delete(nw.items[k], "Field")
		delete(nw.items[k], "userRating")
	}
	srv := httptest.NewServer(nw)
	defer srv.Close()
	eng.plex = plex.NewClient(srv.URL, "ownertok")
	eng.plex.PlexTV = srv.URL
	eng.machine, eng.users = "", nil
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	plan, err := eng.PlanFieldRestore(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	for _, c := range plan {
		fields[c.Field] = c.To
	}
	if fields["titleSort"] != "Heat 1995" || fields["label"] != "favorite" || fields["collection"] != "Mann" || fields["userRating"] != "9" {
		t.Fatalf("plan missing values: %+v", plan)
	}
	if err := eng.ApplyFieldRestore(ctx, "old", noReport); !errors.Is(err, plex.ErrReadOnly) {
		t.Fatalf("restore without writes should be refused: %v", err)
	}
	eng.cfg.PlexAllowWrites, eng.plex.AllowWrites = true, true
	if err := eng.ApplyFieldRestore(ctx, "old", noReport); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(nw.writes, "\n")
	for _, want := range []string{"titleSort.value=Heat+1995", "titleSort.locked=1", "label%5B0%5D.tag.tag=favorite", "collection%5B0%5D.tag.tag=Mann", "/:/rate"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("restore did not send %s:\n%s", want, joined)
		}
	}
	_ = st
}

func TestCLZImportKeepsRereads(t *testing.T) {
	f := newFakePlex("srvA")
	eng, st, _ := testEngine(t, f, nil)
	ctx := context.Background()
	csv1 := "\ufeffSeries,Issue Nr,Title,Publisher,Read,Read Date,My Rating,Comic ID,Custom Notes\n" +
		"Saga,1,Chapter One,Image,Yes,2019-03-04,10,123,first print\n" +
		"Saga,2,Chapter Two,Image,No,,,124,\n"
	res, err := eng.ImportCLZ(ctx, strings.NewReader(csv1), "test")
	if err != nil || res.New != 2 || res.Reads != 1 {
		t.Fatalf("first import: %+v %v", res, err)
	}
	csv2 := "Series;Issue Nr;Read Date;Comic ID\nSaga;1;2024-07-01;123\n"
	res, err = eng.ImportCLZ(ctx, strings.NewReader(csv2), "test")
	if err != nil || res.New != 0 || res.Reads != 1 {
		t.Fatalf("second import: %+v %v", res, err)
	}
	cs, _ := st.Comics(ctx, store.ComicQuery{Search: "Saga"})
	if len(cs) != 2 || cs[0].Reads != 2 || cs[0].Rating != 10 || cs[0].Publisher != "Image" {
		t.Fatalf("rereads/fields lost: %+v", cs)
	}
	stats, _ := st.ComicStats(ctx)
	if stats.Rereads != 1 || stats.Reads != 2 {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestRecommendClaudeAndGemini(t *testing.T) {
	var claudeBody, geminiBody string
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reply := `[{"kind":"movie","title":"Thief","year":1981,"imdb_id":"tt0083190","reason":"Mann"},
			{"kind":"movie","title":"Heat","year":1995,"reason":"already seen"},
			{"kind":"show","title":"Crime Story","year":1986,"tvdb_id":"78112","reason":"Mann TV"}]`
		switch {
		case r.URL.Path == "/v1/messages":
			claudeBody = string(b)
			if r.Header.Get("x-api-key") != "ck" || r.Header.Get("anthropic-version") == "" {
				http.Error(w, "auth", 401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text", "text": "```json\n" + reply + "\n```"}}})
		case strings.Contains(r.URL.Path, ":generateContent"):
			geminiBody = string(b)
			json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": reply}}}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ai.Close()
	f := newFakePlex("srvA")
	cfg := &config.Config{AnthropicKey: "ck", AnthropicModel: "claude-test", AnthropicURL: ai.URL,
		GeminiKey: "gk", GeminiModel: "gemini-test", GeminiURL: ai.URL}
	eng, st, _ := testEngine(t, f, cfg)
	ctx := context.Background()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	// alice watched Heat
	m, _, _ := eng.Resolve(ctx, &plex.Metadata{Type: "movie", Title: "Heat", Year: 1995, GUID: "plex://movie/heat"})
	st.RecordPlay(ctx, m.ID, "alice", time.Now(), "test", "", false)

	if err := eng.Recommend(ctx, RecRequest{User: "alice", Provider: "both", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.RecRuns(ctx, 10)
	if len(runs) != 2 {
		t.Fatalf("want a run per provider, got %d", len(runs))
	}
	for _, r := range runs {
		if r.Error != "" || len(r.Recs) != 2 {
			t.Fatalf("%s: err %q recs %+v", r.Provider, r.Error, r.Recs)
		}
		for _, c := range r.Recs {
			if c.Title == "Heat" {
				t.Fatal("already-watched title was recommended")
			}
			if c.Title == "Crime Story" && c.TVDB != 78112 {
				t.Fatalf("string id not parsed: %+v", c)
			}
		}
	}
	if !strings.Contains(claudeBody, "Heat (1995) [movie] rated 9.0/10") || !strings.Contains(claudeBody, "Do not recommend pornographic") {
		t.Fatalf("profile missing from prompt: %s", claudeBody)
	}
	if !strings.Contains(geminiBody, "responseMimeType") {
		t.Fatal("gemini request not in JSON mode")
	}
}

func TestCopyToJellyfin(t *testing.T) {
	var created string
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "jk" {
			http.Error(w, "auth", 401)
			return
		}
		switch {
		case r.URL.Path == "/Users":
			json.NewEncoder(w).Encode([]any{map[string]any{"Id": "u1", "Name": "alice"}})
		case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("IncludeItemTypes") == "Playlist":
			json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		case r.URL.Path == "/Users/u1/Items":
			json.NewEncoder(w).Encode(map[string]any{"TotalRecordCount": 2, "Items": []any{
				map[string]any{"Id": "jf-heat", "Type": "Movie", "ProviderIds": map[string]any{"Imdb": "tt0113277"}},
				map[string]any{"Id": "jf-ronin", "Type": "Movie", "Path": "/media/films/Ronin (1998)/Ronin.mkv"},
			}})
		case r.URL.Path == "/Playlists" && r.Method == http.MethodPost:
			created = r.URL.Query().Get("Ids") + "|" + r.URL.Query().Get("Name")
			json.NewEncoder(w).Encode(map[string]any{"Id": "pl1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer jf.Close()
	f := newFakePlex("srvA")
	cfg := &config.Config{MediaServers: []config.MediaServer{{Kind: "jellyfin", Name: "jellyfin", URL: jf.URL, APIKey: "jk"}}}
	eng, st, _ := testEngine(t, f, cfg)
	ctx := context.Background()
	if err := eng.ArchivePlex(ctx, true, noReport); err != nil {
		t.Fatal(err)
	}
	lists, _ := st.Lists(ctx, "srvA", "playlist", "alice")
	var id int64
	for _, l := range lists {
		if !l.Smart {
			id = l.ID
		}
	}
	if err := eng.CopyToMediaServer(ctx, id, "jellyfin", "alice", "", noReport); err != nil {
		t.Fatal(err)
	}
	if created != "jf-heat,jf-ronin|Heist night" {
		t.Fatalf("matched by imdb and by file name, got %q", created)
	}
}
