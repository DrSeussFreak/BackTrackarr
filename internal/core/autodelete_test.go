package core

import (
	"context"
	"encoding/json"
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
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

type fakeArr struct {
	mu    sync.Mutex
	calls []string
	fail  string
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		f.calls = append(f.calls, key)
	}
	if key == f.fail {
		http.Error(w, "nope", 500)
		return
	}
	switch key {
	case "GET /api/v3/episode":
		io.WriteString(w, `[{"id":1,"seasonNumber":1,"episodeNumber":1,"episodeFileId":10,"hasFile":true},
		 {"id":2,"seasonNumber":1,"episodeNumber":2,"episodeFileId":20,"hasFile":true},
		 {"id":3,"seasonNumber":1,"episodeNumber":3,"episodeFileId":30,"hasFile":true},
		 {"id":4,"seasonNumber":1,"episodeNumber":4,"episodeFileId":30,"hasFile":true},
		 {"id":5,"seasonNumber":1,"episodeNumber":5,"episodeFileId":0,"hasFile":false}]`)
	case "GET /api/v3/movie/7":
		io.WriteString(w, `{"hasFile":true,"movieFile":{"id":70}}`)
	default:
		io.WriteString(w, `{}`)
	}
}

func (f *fakeArr) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func deleteEngine(t *testing.T, url string) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Arr: []config.ArrInstance{{Kind: "sonarr", Name: "sonarr", URL: url, APIKey: "k"}, {Kind: "radarr", Name: "radarr", URL: url, APIKey: "k"}}}
	ctx := context.Background()
	for _, it := range []store.ArrItem{{Instance: "sonarr", ArrID: 5, Kind: "series", Title: "Andor", TVDB: 393189, Data: []byte(`{"a":1}`)},
		{Instance: "radarr", ArrID: 7, Kind: "movie", Title: "Heat", Year: 1995, TMDB: 949, Data: []byte(`{"b":1}`)}} {
		if _, err := st.SaveArrItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	return New(ctx, cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func watch(t *testing.T, st *store.Store, user string, ep int, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	m := model.Media{Kind: model.Episode, Title: fmt.Sprintf("Ep %d", ep), ShowTitle: "Andor", Season: 1, Episode: ep, ShowIDs: model.IDs{TVDB: 393189}}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordPlay(ctx, m.ID, user, time.Now().Add(-ago), "test", "", false); err != nil {
		t.Fatal(err)
	}
}

func TestAutoDeleteRequiresEveryoneAndTheSwitch(t *testing.T) {
	arr := &fakeArr{}
	srv := httptest.NewServer(arr)
	defer srv.Close()
	e, st := deleteEngine(t, srv.URL)
	ctx := context.Background()
	nop := func(string) {}
	old := 72 * time.Hour
	watch(t, st, "Alex", 1, old)
	watch(t, st, "Sam", 1, old)  // episode 1: both watched
	watch(t, st, "Alex", 2, old) // episode 2: only one watched
	watch(t, st, "Alex", 3, old)
	watch(t, st, "Sam", 3, old) // double episode 3+4: 4 not watched

	// Default: no rule, switch off.
	if err := e.RunAutoDeleteOnce(ctx, nop); err != nil || len(arr.writes()) != 0 {
		t.Fatalf("default must delete nothing: %v %v", arr.writes(), err)
	}
	if err := st.SaveDeleteRule(ctx, "sonarr", 5, []string{"alex", "SAM"}, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.RunAutoDeleteOnce(ctx, nop); err != nil || len(arr.writes()) != 0 {
		t.Fatalf("a rule without the master switch must delete nothing: %v", arr.writes())
	}
	list, _, err := e.PreviewAutoDelete(ctx)
	if err != nil || len(list) != 1 || list[0].Item != "S01E01" || list[0].FileID != 10 {
		t.Fatalf("preview: %+v %v", list, err)
	}
	if len(arr.writes()) != 0 {
		t.Fatal("preview must not change anything")
	}
	if err := e.SetAutoDelete(ctx, true); err != nil {
		t.Fatal(err)
	}

	// Excluded server: nothing.
	e.SetAutoDeleteScope(ctx, DeleteScope{ServerMode: "exclude", Servers: []string{"sonarr"}})
	e.RunAutoDeleteOnce(ctx, nop)
	if len(arr.writes()) != 0 {
		t.Fatalf("excluded server was touched: %v", arr.writes())
	}
	// Library filter with an unknown library: nothing.
	e.SetAutoDeleteScope(ctx, DeleteScope{LibraryMode: "exclude", Libraries: []string{"Kids"}})
	e.RunAutoDeleteOnce(ctx, nop)
	if len(arr.writes()) != 0 {
		t.Fatalf("unknown library must be safe: %v", arr.writes())
	}
	e.SetAutoDeleteScope(ctx, DeleteScope{ServerMode: "include", Servers: []string{"sonarr"}})

	// If unmonitoring fails, the file stays.
	arr.fail = "PUT /api/v3/episode/monitor"
	if err := e.RunAutoDeleteOnce(ctx, nop); err == nil {
		t.Fatal("a failed unmonitor should be reported")
	}
	for _, c := range arr.writes() {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatal("deleted a file that was still monitored")
		}
	}
	arr.mu.Lock()
	arr.fail, arr.calls = "", nil
	arr.mu.Unlock()

	if err := e.RunAutoDeleteOnce(ctx, nop); err != nil {
		t.Fatal(err)
	}
	if got := arr.writes(); len(got) != 2 || got[0] != "PUT /api/v3/episode/monitor" || got[1] != "DELETE /api/v3/episodefile/10" {
		t.Fatalf("writes: %v", got)
	}
	log, _ := st.DeleteLog(ctx, 5)
	if len(log) == 0 || log[0].Status != "deleted" || log[0].Item != "S01E01" {
		t.Fatalf("log: %+v", log)
	}
}

func TestAutoDeleteGraceAndMovie(t *testing.T) {
	arr := &fakeArr{}
	srv := httptest.NewServer(arr)
	defer srv.Close()
	e, st := deleteEngine(t, srv.URL)
	ctx := context.Background()
	e.SetAutoDelete(ctx, true)
	m := model.Media{Kind: model.Movie, Title: "Heat", Year: 1995, IDs: model.IDs{TMDB: 949}}
	st.UpsertMedia(ctx, &m)
	st.RecordPlay(ctx, m.ID, "Alex", time.Now().Add(-2*time.Hour), "test", "", false)
	st.SaveDeleteRule(ctx, "radarr", 7, []string{"Alex"}, true, 1)
	e.RunAutoDeleteOnce(ctx, func(string) {})
	if len(arr.writes()) != 0 {
		t.Fatalf("inside the waiting period: %v", arr.writes())
	}
	st.SaveDeleteRule(ctx, "radarr", 7, []string{"Alex"}, true, 0)
	if err := st.PutSession(ctx, store.Session{Key: "x", MediaID: m.ID, User: "Sam", State: model.ActStart}); err != nil {
		t.Fatal(err)
	}
	e.RunAutoDeleteOnce(ctx, func(string) {})
	if len(arr.writes()) != 0 {
		t.Fatalf("someone is watching it: %v", arr.writes())
	}
	st.DeleteSession(ctx, "x")
	e.RunAutoDeleteOnce(ctx, func(string) {})
	if got := arr.writes(); len(got) != 2 || got[0] != "PUT /api/v3/movie/editor" || got[1] != "DELETE /api/v3/moviefile/70" {
		t.Fatalf("writes: %v", got)
	}
	if err := st.SaveDeleteRule(ctx, "radarr", 7, nil, true, 0); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.ActiveDeleteRules(ctx); len(r) != 0 {
		t.Fatal("a rule with no users must never be active")
	}
}

func TestPullHistorySources(t *testing.T) {
	writes := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		switch r.URL.Path {
		case "/api/v2":
			if r.URL.Query().Get("apikey") != "taut-key" {
				json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "error"}})
				return
			}
			io.WriteString(w, `{"response":{"result":"success","data":{"recordsFiltered":3,"data":[
			 {"media_type":"movie","title":"Heat","year":1995,"started":1700000000,"watched_status":1,"percent_complete":98,"user":"Alex"},
			 {"media_type":"episode","title":"Pilot","grandparent_title":"Lost","parent_media_index":"1","media_index":"1","started":1700100000,"watched_status":0.5,"percent_complete":92,"user":"Sam"},
			 {"media_type":"movie","title":"Unfinished","started":1700200000,"watched_status":0,"percent_complete":20,"user":"Alex"}]}}}`)
		case "/api/v1/export/csv":
			if r.Header.Get("Authorization") != "Bearer floppy-token" {
				http.Error(w, "no", 401)
				return
			}
			io.WriteString(w, "media_id,source,media_type,title,image,season_number,episode_number,score,status,notes,start_date,end_date,progress\n"+
				"603,tmdb,movie,The Matrix,,,,9,Completed,,,2022-01-02,1\n1399,tmdb,episode,Game of Thrones,,1,1,,Completed,,,2022-02-03,\n1399,tmdb,tv,Game of Thrones,,,,8,Completed,,,,\n")
		case "/api/snapshots/list":
			if r.Header.Get("Authorization") != "Bearer cw-token" {
				http.Error(w, "no", 401)
				return
			}
			io.WriteString(w, `{"ok":true,"snapshots":[{"path":"a/2024-history.json","feature":"history","provider":"PLEX"},{"path":"a/2025-history.json","feature":"history","provider":"PLEX"}]}`)
		case "/api/snapshots/read":
			if r.URL.Query().Get("path") != "a/2025-history.json" {
				t.Errorf("should read only the newest snapshot: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"ok":true,"snapshot":{"feature":"history","items":{"k1":{"type":"movie","title":"Alien","year":1979,"ids":{"imdb":"tt0078748"},"watched_at":"2023-01-02T03:04:05Z"}}}}`)
		case "/api/backup/export":
			if r.Header.Get("X-Api-Key") != "plekt-key" || r.URL.Query().Get("collection") != "watchHistory" {
				http.Error(w, "no", 401)
				return
			}
			if r.URL.Query().Get("cursor") == "" {
				io.WriteString(w, `{"documents":[{"id":"1","data":{"title":"Ran","media_type":"movie","watched_at":"2021-05-06T07:08:09Z","tmdb_id":"11645"}}],"nextCursor":"1","hasMore":true}`)
			} else {
				io.WriteString(w, `{"documents":[{"id":"2","data":{"title":"Two","show_title":"Severance","episode_title":"Two","media_type":"episode","season":1,"episode":2,"watched_at":"2022-05-06T07:08:09Z","tvdb_id":"371980"}}],"nextCursor":"","hasMore":false}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{WatchedPercent: 80, TrackerPullUser: "me", TautulliURL: srv.URL, TautulliKey: "taut-key", FloppyURL: srv.URL, FloppyToken: "floppy-token",
		CrossWatchURL: srv.URL, CrossWatchToken: "cw-token", PlektURL: srv.URL, PlektKey: "plekt-key"}
	e := New(context.Background(), cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := e.PullHistory(context.Background(), "", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("history imports must only read: %d writes", writes)
	}
	for source, want := range map[string]int{"tautulli-import": 2, "floppy-import": 2, "crosswatch-import": 1, "plekt-import": 2} {
		if got := shelfCount(t, st, `SELECT COUNT(*) FROM plays WHERE source=?`, source); got != want {
			t.Errorf("%s: %d plays, want %d", source, got, want)
		}
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM plays WHERE user='Sam'`); n != 1 {
		t.Errorf("Tautulli's user should be kept: %d", n)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM media WHERE title='Unfinished'`); n != 0 {
		t.Error("unfinished plays must not import")
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM ratings r JOIN media m ON m.id=r.media_id WHERE m.title='The Matrix' AND r.rating=9`); n != 1 {
		t.Error("Floppy's score should import")
	}
	before := shelfCount(t, st, `SELECT COUNT(*) FROM plays`)
	if err := e.PullHistory(context.Background(), "", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if after := shelfCount(t, st, `SELECT COUNT(*) FROM plays`); after != before {
		t.Errorf("re-import added plays: %d -> %d", before, after)
	}
}

func TestNowPlaying(t *testing.T) {
	e, st := deleteEngine(t, "http://127.0.0.1:1")
	ctx := context.Background()
	m := model.Media{Kind: model.Movie, Title: "Heat", Year: 1995, DurationS: 7200}
	if err := e.Ingest(ctx, model.Event{Action: model.ActStart, Media: m, User: "Alex", Player: "Living Room", Source: "plex", Progress: 25}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := st.NowPlaying(ctx)
	if err != nil || len(rows) != 1 || rows[0].User != "Alex" || rows[0].Player != "Living Room" || rows[0].Source != "plex" {
		t.Fatalf("%+v %v", rows, err)
	}
	if err := e.Ingest(ctx, model.Event{Action: model.ActStop, Media: rows[0].Media, User: "Alex", Player: "Living Room", Source: "plex", Progress: 25}, false); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.NowPlaying(ctx); len(rows) != 0 {
		t.Fatalf("a stopped session should disappear: %+v", rows)
	}
}
