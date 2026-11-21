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

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// fakeShelf plays all eight services. It fails the test on anything that
// could change data, or on a missing credential.
type fakeShelf struct {
	t            *testing.T
	mu           sync.Mutex
	absFinished  int64
	steamMinutes int
	steamLast    int64
}

func (f *fakeShelf) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	post := p == "/api/v1/books/list" || p == "/v1/graphql"
	if (r.Method != http.MethodGet && !post) || (r.Method == http.MethodPost && !post) {
		f.t.Errorf("write attempt: %s %s", r.Method, p)
	}
	need := func(ok bool) bool {
		if !ok {
			http.Error(w, "auth", http.StatusUnauthorized)
		}
		return ok
	}
	send := func(s string) { io.WriteString(w, s) }
	q := r.URL.Query()
	switch {
	case p == "/api/me/listening-sessions":
		if !need(r.Header.Get("Authorization") == "Bearer abs-token") {
			return
		}
		send(`{"total":2,"numPages":1,"sessions":[
		 {"id":"s1","libraryItemId":"i1","mediaType":"book","displayTitle":"Dune","displayAuthor":"Frank Herbert","duration":1000,"timeListening":600,"currentTime":600,"startedAt":1700000000000,"updatedAt":1700000600000,"mediaMetadata":{"isbn":"111","publishedYear":"1965"},"deviceInfo":{"clientName":"Web"}},
		 {"id":"pod","libraryItemId":"p1","mediaType":"podcast","displayTitle":"A Podcast","timeListening":50,"updatedAt":1700000600000}]}`)
	case p == "/api/me":
		if !need(r.Header.Get("Authorization") == "Bearer abs-token") {
			return
		}
		fmt.Fprintf(w, `{"mediaProgress":[
		 {"libraryItemId":"i1","mediaItemType":"book","progress":1,"isFinished":true,"lastUpdate":%[1]d,"startedAt":1700000000000,"finishedAt":%[1]d},
		 {"libraryItemId":"i2","mediaItemType":"book","ebookProgress":0.4,"lastUpdate":1700000000000},
		 {"libraryItemId":"gone","mediaItemType":"book","progress":0.1,"lastUpdate":1700000000000},
		 {"libraryItemId":"ep","mediaItemType":"podcastEpisode","isFinished":true,"finishedAt":1700000000000}]}`, f.absFinished)
	case p == "/api/items/i2":
		send(`{"id":"i2","mediaType":"book","media":{"duration":0,"audioFiles":[],"metadata":{"title":"An Ebook","authorName":"E. Author"}}}`)
	case p == "/api/items/gone":
		http.NotFound(w, r)
	case p == "/api/v1/books/list":
		if !need(r.Header.Get("X-API-Key") == "komga-key") {
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"readStatus"`) {
			f.t.Errorf("komga search: %s", body)
		}
		send(`{"last":true,"content":[
		 {"id":"k1","name":"k1","seriesTitle":"Novels","metadata":{"title":"Neuromancer","authors":[{"name":"William Gibson","role":"writer"}]},"media":{"mediaProfile":"EPUB","pagesCount":300},"readProgress":{"page":300,"completed":true,"readDate":"2024-02-03T10:00:00Z","deviceName":"Kobo"}},
		 {"id":"k2","name":"k2","seriesTitle":"Saga","metadata":{"title":"Chapter One","number":"1"},"media":{"mediaProfile":"DIVINA","pagesCount":24},"readProgress":{"page":24,"completed":true,"readDate":"2024-02-04T10:00:00Z"}}]}`)
	case p == "/api/Stats/reading-history":
		if !need(r.Header.Get("x-api-key") == "kavita-key") {
			return
		}
		if q.Get("PageNumber") != "1" {
			send(`[]`)
			return
		}
		send(`[{"seriesId":1,"seriesName":"Foundation","seriesFormat":3,"chapters":[{"chapterId":10,"label":"","startTimeUtc":"2024-03-01T10:00:00Z","endTimeUtc":"2024-03-01T11:00:00Z","durationSeconds":3600,"endPage":100,"totalPages":100,"completed":true}]},
		 {"seriesId":2,"seriesName":"Manga","seriesFormat":1,"chapters":[{"chapterId":20,"label":"3","startTimeUtc":"2024-03-02T10:00:00Z","endTimeUtc":"2024-03-02T10:20:00Z","durationSeconds":1200,"completed":true}]}]`)
	case p == "/v1/graphql":
		if !need(r.Header.Get("Authorization") == "Bearer hc-token") {
			return
		}
		var b struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		if !strings.HasPrefix(b.Query, "query ") || strings.Contains(b.Query, "mutation") {
			f.t.Errorf("hardcover must only query: %s", b.Query)
		}
		send(`{"data":{"me":[{"user_books":[{"id":5,"status_id":3,"rating":4.5,"read_count":3,"book":{"title":"Hyperion","release_year":1989,"pages":480,"cached_contributors":[{"author":{"name":"Dan Simmons"}}]},
		 "edition":{"isbn_13":"978","pages":480,"audio_seconds":0},
		 "user_book_reads":[{"id":1,"started_at":"2020-01-01","finished_at":"2020-01-10"},{"id":2,"started_at":"2022-05-01","finished_at":"2022-05-09"}]}]}]}}`)
	case strings.HasPrefix(p, "/people/reader/books/"):
		if strings.Contains(p, "already-read") {
			send(`{"page":1,"numFound":1,"reading_log_entries":[{"work":{"title":"Emma","key":"/works/OL1W","author_names":["Jane Austen"],"first_publish_year":1815},"logged_date":"2023/06/12, 10:11:12"}]}`)
			return
		}
		send(`{"page":1,"numFound":0,"reading_log_entries":[]}`)
	case p == "/IPlayerService/GetOwnedGames/v1/":
		if !need(q.Get("key") == "steam-secret" && q.Get("steamid") == "765") {
			return
		}
		fmt.Fprintf(w, `{"response":{"game_count":2,"games":[{"appid":10,"name":"Portal","playtime_forever":%d,"rtime_last_played":%d},{"appid":11,"name":"Unplayed","playtime_forever":0,"rtime_last_played":0}]}}`, f.steamMinutes, f.steamLast)
	case p == "/API/API_GetUserCompletionProgress.php":
		if !need(q.Get("y") == "ra-key" && q.Get("u") == "player") {
			return
		}
		send(`{"Count":1,"Total":1,"Results":[{"GameID":7,"Title":"Sonic","ConsoleName":"Mega Drive","MaxPossible":10,"NumAwarded":10,"MostRecentAwardedDate":"2023-10-27T02:52:34+00:00","HighestAwardKind":"beaten-hardcore","HighestAwardDate":"2023-10-27T02:52:34+00:00"}]}`)
	case p == "/API/API_GetUserRecentlyPlayedGames.php":
		send(`[{"GameID":7,"Title":"Sonic","ConsoleName":"Mega Drive","LastPlayed":"2023-10-28 00:30:04"}]`)
	case p == "/api/roms":
		if !need(r.Header.Get("Authorization") == "Bearer romm-token") {
			return
		}
		send(`{"total":1,"items":[{"id":3,"name":"Chrono Trigger","fs_name":"ct.sfc","platform_display_name":"SNES","rom_user":{"last_played":"2024-04-01T12:00:00Z","updated_at":"2024-04-02T12:00:00Z","rating":9,"completion":100,"status":"finished"}}]}`)
	default:
		f.t.Errorf("unexpected request %s", p)
		http.NotFound(w, r)
	}
}

func shelfEngine(t *testing.T, s config.ShelfConfig) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Shelf: s}
	return New(context.Background(), cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func shelfCount(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func eventCount(t *testing.T, st *store.Store, source, kind string) int {
	return shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE source=? AND kind=?`, source, kind)
}

func TestShelfSyncAllSources(t *testing.T) {
	f := &fakeShelf{t: t, absFinished: 1700001000000, steamMinutes: 120, steamLast: 1700000000}
	srv := httptest.NewServer(f)
	defer srv.Close()
	e, st := shelfEngine(t, config.ShelfConfig{
		AudiobookshelfURL: srv.URL, AudiobookshelfToken: "abs-token", KomgaURL: srv.URL, KomgaKey: "komga-key",
		KavitaURL: srv.URL, KavitaKey: "kavita-key", HardcoverURL: srv.URL, HardcoverToken: "hc-token",
		OpenLibraryURL: srv.URL, OpenLibraryUser: "reader", SteamURL: srv.URL, SteamKey: "steam-secret", SteamID: "765",
		RetroURL: srv.URL, RetroUser: "player", RetroKey: "ra-key", RommURL: srv.URL, RommToken: "romm-token"})
	ctx := context.Background()
	nop := func(string) {}
	if err := e.SyncShelf(ctx, "", nop); err != nil {
		t.Fatal(err)
	}
	total := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events`)
	items := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items`)

	// A second sync with nothing new changes nothing.
	if err := e.SyncShelf(ctx, "", nop); err != nil {
		t.Fatal(err)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events`); n != total {
		t.Fatalf("resync added events: %d -> %d", total, n)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items`); n != items {
		t.Fatalf("resync added items: %d -> %d", items, n)
	}

	for _, c := range []struct {
		source, kind string
		want         int
	}{
		{"audiobookshelf", "session", 1}, {"audiobookshelf", "finish", 1},
		{"komga", "finish", 1}, {"kavita", "session", 1}, {"kavita", "finish", 1},
		{"hardcover", "finish", 3}, // two dated reads, one more counted without a date
		{"openlibrary", "finish", 1}, {"steam", "baseline", 1},
		{"retroachievements", "finish", 1}, {"retroachievements", "play", 1},
		{"romm", "play", 1}, {"romm", "finish", 1},
	} {
		if got := eventCount(t, st, c.source, c.kind); got != c.want {
			t.Errorf("%s %s: got %d, want %d", c.source, c.kind, got, c.want)
		}
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE title LIKE '%Podcast%'`); n != 0 {
		t.Error("podcasts must be skipped")
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE title='An Ebook' AND media_type='book'`); n != 1 {
		t.Error("an item with no audio should be a book")
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM comics c JOIN comic_reads r ON r.comic_id=c.id WHERE c.ext_key IN ('komga:k2','kavita:20')`); n != 2 {
		t.Errorf("comics should land on the Comics page with reads, got %d", n)
	}

	// A relisten and more playtime are new rows, never overwrites.
	f.mu.Lock()
	f.absFinished, f.steamMinutes, f.steamLast = 1800000000000, 150, 1800000000
	f.mu.Unlock()
	if err := e.SyncShelf(ctx, "", nop); err != nil {
		t.Fatal(err)
	}
	if got := eventCount(t, st, "audiobookshelf", "finish"); got != 2 {
		t.Errorf("relisten should add a finish, got %d", got)
	}
	if got := shelfCount(t, st, `SELECT duration_s FROM shelf_events WHERE source='steam' AND kind='session'`); got != 30*60 {
		t.Errorf("session should hold the added 30 minutes, got %d s", got)
	}
	gs, err := st.ShelfStats(ctx, "game")
	if err != nil {
		t.Fatal(err)
	}
	if gs.Seconds != 150*60 {
		t.Errorf("game time must not double count baseline and sessions: %d", gs.Seconds)
	}
	var raw []byte
	if err := st.DB.QueryRow(`SELECT raw FROM shelf_items WHERE source='steam' AND title='Portal'`).Scan(&raw); err != nil || len(raw) == 0 {
		t.Errorf("raw source data should be kept: %v", err)
	}
}

func TestShelfErrorsHideKeysAndIsolateFailures(t *testing.T) {
	f := &fakeShelf{t: t, absFinished: 1700001000000, steamMinutes: 60, steamLast: 1700000000}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	e, st := shelfEngine(t, config.ShelfConfig{SteamURL: deadURL, SteamKey: "steam-secret", SteamID: "765",
		RetroURL: srv.URL, RetroUser: "player", RetroKey: "ra-key"})
	err := e.SyncShelf(context.Background(), "games", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "Steam") {
		t.Fatalf("want a Steam failure, got %v", err)
	}
	if strings.Contains(err.Error(), "steam-secret") {
		t.Fatalf("API key leaked into the error: %v", err)
	}
	if got := eventCount(t, st, "retroachievements", "finish"); got != 1 {
		t.Errorf("one failed source must not stop the others, got %d", got)
	}
}

func TestShelfRedirectToOtherHostRefused(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	var out any
	if err := shelfGetJSON(context.Background(), srv.URL+"/x?key=secret", map[string]string{"x-api-key": "secret"}, &out); err == nil {
		t.Fatal("redirect to another host should fail")
	}
	if leaked {
		t.Fatal("credentials followed a redirect")
	}
}

func TestImportGoodreadsAndStoryGraph(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	gr := "Book Id,Title,Author,ISBN,ISBN13,My Rating,Binding,Number of Pages,Original Publication Year,Date Read,Exclusive Shelf,Read Count\n" +
		`1,Dune,Frank Herbert,"=""0441""","=""9780441""",5,Paperback,600,1965,2023/04/05,read,3` + "\n" +
		`2,Project Hail Mary,Andy Weir,,,0,Audible Audio,,2021,,to-read,0` + "\n"
	res, err := e.ImportBooks(ctx, strings.NewReader(gr), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "goodreads" || res.New != 2 {
		t.Fatalf("%+v", res)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish'`); n != 3 {
		t.Fatalf("read count 3 should be 3 rows, got %d", n)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE title='Project Hail Mary' AND media_type='audiobook'`); n != 1 {
		t.Error("Audible binding should be an audiobook")
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE isbn='9780441'`); n != 1 {
		t.Error("Goodreads ISBN quoting should be removed")
	}

	// Re-import is idempotent, and a real date replaces an undated row.
	if _, err := e.ImportBooks(ctx, strings.NewReader(gr), ""); err != nil {
		t.Fatal(err)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish'`); n != 3 {
		t.Fatalf("re-import changed rows: %d", n)
	}
	later := strings.Replace(gr, "2023/04/05", "2024/08/09", 1)
	if _, err := e.ImportBooks(ctx, strings.NewReader(later), ""); err != nil {
		t.Fatal(err)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish'`); n != 3 {
		t.Fatalf("still three reads, got %d", n)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish' AND at>0`); n != 2 {
		t.Fatalf("two dated reads, got %d", n)
	}

	sg := "Title,Authors,ISBN/UID,Format,Read Status,Last Date Read,Dates Read,Read Count,Star Rating\n" +
		`Circe,Madeline Miller,978111,audio,read,2024/03/05,"2023/01/02-2023/01/10, 2024/03/01-2024/03/05",2,4.5` + "\n"
	res, err = e.ImportBooks(ctx, strings.NewReader(sg), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "storygraph" || res.Events != 2 {
		t.Fatalf("%+v", res)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events e JOIN shelf_items i ON i.id=e.item_id WHERE i.title='Circe' AND e.started_at IS NOT NULL`); n != 2 {
		t.Errorf("start dates should be kept, got %d", n)
	}

	// The same read in two sources on one day counts once in stats.
	dup := "Book Id,Title,Author,Date Read,Exclusive Shelf,Read Count\n9,Circe,Madeline Miller,2024/03/05,read,1\n"
	if _, err := e.ImportBooks(ctx, strings.NewReader(dup), "audiobook"); err != nil {
		t.Fatal(err)
	}
	stats, err := st.ShelfStats(ctx, "audiobook")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Reads != 2 {
		t.Errorf("same-day read in two sources should count once, got %d", stats.Reads)
	}
	if _, err := e.ImportBooks(ctx, strings.NewReader("a,b\n1,2\n"), ""); err == nil {
		t.Error("a file with no title column should be refused")
	}
}

func TestImportPlayniteJSONAndCSV(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	js := func(play int) string {
		return fmt.Sprintf(`[{"Name":"Hades","GameId":"123","PluginId":"abc","Playtime":%d,"PlayCount":4,"LastActivity":"2024-05-01T20:00:00.1234567-05:00",
		 "CompletionStatus":{"Name":"Beaten"},"Platforms":[{"Name":"PC (Windows)"}],"Source":{"Name":"GOG"},"Developers":[{"Name":"Supergiant"}],"UserScore":90}]`, play)
	}
	res, err := e.ImportGames(ctx, strings.NewReader(js(7200)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "playnite" || res.New != 1 {
		t.Fatalf("%+v", res)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE platform='PC (Windows)' AND creator='Supergiant' AND playtime_s=7200 AND status='beaten'`); n != 1 {
		t.Error("nested Playnite fields should be read")
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish'`); n != 1 {
		t.Error("Beaten should record a finish")
	}
	if _, err := e.ImportGames(ctx, strings.NewReader(js(9000))); err != nil {
		t.Fatal(err)
	}
	if got := shelfCount(t, st, `SELECT duration_s FROM shelf_events WHERE kind='session'`); got != 1800 {
		t.Errorf("added playtime should be a session, got %d", got)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_events WHERE kind='finish'`); n != 1 {
		t.Error("re-import must not add a finish")
	}
	csvFile := "Name,Platform,Hours Played,Last Played\nCeleste,Switch,12.5,2024-01-02\n"
	if _, err := e.ImportGames(ctx, strings.NewReader(csvFile)); err != nil {
		t.Fatal(err)
	}
	if n := shelfCount(t, st, `SELECT COUNT(*) FROM shelf_items WHERE title='Celeste' AND playtime_s=45000 AND last_activity>0`); n != 1 {
		t.Error("hours column should scale to seconds")
	}
}
