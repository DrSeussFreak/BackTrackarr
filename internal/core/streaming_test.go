package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestParseStreamTitle(t *testing.T) {
	cases := []struct {
		in, show string
		season   int
		ep       int
		eptitle  string
		movie    string
	}{
		{in: "Stranger Things: Season 1: Chapter One: The Vanishing of Will Byers", show: "Stranger Things", season: 1, eptitle: "Chapter One: The Vanishing of Will Byers"},
		{in: "The Queen's Gambit: Limited Series: Openings", show: "The Queen's Gambit", season: 1, eptitle: "Openings"},
		{in: "Money Heist: Part 3: Episode 2", show: "Money Heist", season: 3, eptitle: "Episode 2"},
		{in: "Andor - S01E03 - Reckoning", show: "Andor", season: 1, ep: 3, eptitle: "Reckoning"},
		{in: "Severance S2:E1 Hello, Ms. Cobel", show: "Severance", season: 2, ep: 1, eptitle: "Hello, Ms. Cobel"},
		{in: "Fargo: Season 3, Episode 2: The Principle of Restricted Choice", show: "Fargo", season: 3, ep: 2, eptitle: "The Principle of Restricted Choice"},
		{in: "Mission: Impossible", movie: "Mission: Impossible"},
		{in: "Glass Onion: A Knives Out Mystery", movie: "Glass Onion: A Knives Out Mystery"},
		{in: "Heat (1995)", movie: "Heat"},
	}
	for _, c := range cases {
		got := ParseStreamTitle(c.in, StreamItem{})
		if c.movie != "" {
			if got.Kind != "movie" || got.Title != c.movie {
				t.Errorf("%q: want movie %q, got %+v", c.in, c.movie, got)
			}
			continue
		}
		if got.Show != c.show || got.Season != c.season || got.Episode != c.ep || got.EpisodeTitle != c.eptitle {
			t.Errorf("%q: got show=%q s=%d e=%d title=%q", c.in, got.Show, got.Season, got.Episode, got.EpisodeTitle)
		}
	}
}

// fakeTMDB knows one show and one movie.
func fakeTMDB(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "tk" {
			http.Error(w, "auth", 401)
			return
		}
		var out any
		switch r.URL.Path {
		case "/3/search/tv":
			if strings.Contains(strings.ToLower(r.URL.Query().Get("query")), "stranger") {
				out = map[string]any{"results": []any{map[string]any{"id": 66732, "name": "Stranger Things", "first_air_date": "2016-07-15"}}}
			} else {
				out = map[string]any{"results": []any{}}
			}
		case "/3/tv/66732/external_ids":
			out = map[string]any{"imdb_id": "tt4574334", "tvdb_id": 305288}
		case "/3/tv/66732/season/1":
			out = map[string]any{"episodes": []any{
				map[string]any{"episode_number": 1, "name": "Chapter One: The Vanishing of Will Byers"},
				map[string]any{"episode_number": 2, "name": "Chapter Two: The Weirdo on Maple Street"},
			}}
		case "/3/search/movie":
			if strings.Contains(strings.ToLower(r.URL.Query().Get("query")), "glass onion") {
				out = map[string]any{"results": []any{map[string]any{"id": 661374, "title": "Glass Onion: A Knives Out Mystery", "release_date": "2022-11-23"}}}
			} else {
				out = map[string]any{"results": []any{}}
			}
		case "/3/movie/661374/external_ids":
			out = map[string]any{"imdb_id": "tt11564570"}
		case "/3/movie/661374/watch/providers":
			out = map[string]any{"results": map[string]any{"US": map[string]any{"flatrate": []any{map[string]any{"provider_name": "Netflix"}}}}}
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestNetflixImportResolvesEpisodes(t *testing.T) {
	tm := fakeTMDB(t)
	defer tm.Close()
	cfg := &config.Config{TMDBKey: "tk", TMDBURL: tm.URL, StreamingRegion: "US"}
	eng, st, _ := testEngine(t, newFakePlex("srvA"), cfg)
	ctx := context.Background()

	simple := "Title,Date\n" +
		"\"Stranger Things: Season 1: Chapter One: The Vanishing of Will Byers\",1/2/23\n" +
		"\"Stranger Things: Season 1: Chapter Two: The Weirdo on Maple Street\",1/3/23\n" +
		"\"Glass Onion: A Knives Out Mystery\",12/25/22\n" +
		"\"Some Unknown Doc\",12/26/22\n"
	res, err := eng.ImportStreaming(ctx, strings.NewReader(simple), StreamImport{User: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Service != "Netflix" || res.Added != 4 || res.Unmatched != 1 {
		t.Fatalf("import result: %+v", res)
	}
	rows, _ := st.History(ctx, store.HistoryQuery{User: "alice", Limit: 10})
	byTitle := map[string]store.PlayRow{}
	for _, r := range rows {
		byTitle[r.Media.Title] = r
	}
	ep2 := byTitle["Chapter Two: The Weirdo on Maple Street"]
	if ep2.Media.Episode != 2 || ep2.Media.ShowIDs.TVDB != 305288 || ep2.Source != "netflix-import" || ep2.Player != "Netflix" {
		t.Fatalf("episode not resolved: %+v", ep2)
	}
	if got := time.Unix(ep2.WatchedAt, 0); got.Month() != time.January || got.Day() != 3 {
		t.Fatalf("US date parsed wrong: %v", got)
	}
	if m := byTitle["Glass Onion: A Knives Out Mystery"].Media; m.IDs.IMDB != "tt11564570" {
		t.Fatalf("movie not resolved: %+v", m)
	}
	// Re-importing the same file adds nothing.
	res, _ = eng.ImportStreaming(ctx, strings.NewReader(simple), StreamImport{User: "alice"}, nil)
	if res.Added != 0 || res.Duplicates != 4 {
		t.Fatalf("re-import: %+v", res)
	}

	// The full "Download your personal information" export: profiles become
	// users, trailers and short samples are skipped, UK day-first dates work.
	full := "Profile Name,Start Time,Duration,Attributes,Title,Supplemental Video Type,Device Type,Bookmark,Latest Bookmark,Country\n" +
		"Bob,2023-05-01 20:00:00,00:48:10,,\"Stranger Things: Season 1: Chapter Two: The Weirdo on Maple Street\",,TV,00:48:00,00:48:00,GB (United Kingdom)\n" +
		"Bob,2023-05-01 19:55:00,00:01:30,,\"Stranger Things: Season 2 (Trailer)\",TRAILER,TV,,,GB\n" +
		"Bob,2023-05-02 21:00:00,00:02:00,,\"Glass Onion: A Knives Out Mystery\",,TV,,,GB\n" +
		"Kid,2023-05-03 10:00:00,01:10:00,,\"Glass Onion: A Knives Out Mystery\",,TV,,,GB\n"
	res, err = eng.ImportStreaming(ctx, strings.NewReader(full), StreamImport{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Skipped != 2 || len(res.Profiles) != 2 {
		t.Fatalf("full export: %+v", res)
	}
	if rows, _ := st.History(ctx, store.HistoryQuery{User: "Bob"}); len(rows) != 1 {
		t.Fatalf("profile Bob should have 1 play, got %d", len(rows))
	}
	uk := "Title,Date\n\"Glass Onion: A Knives Out Mystery\",25/12/2021\n"
	if _, err := eng.ImportStreaming(ctx, strings.NewReader(uk), StreamImport{User: "carol"}, nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.History(ctx, store.HistoryQuery{User: "carol"})
	if d := time.Unix(rows[0].WatchedAt, 0); d.Month() != time.December || d.Day() != 25 {
		t.Fatalf("day-first date parsed wrong: %v", d)
	}
}

func TestGenericExportAndScrobbleAPI(t *testing.T) {
	tm := fakeTMDB(t)
	defer tm.Close()
	cfg := &config.Config{TMDBKey: "tk", TMDBURL: tm.URL}
	eng, st, _ := testEngine(t, newFakePlex("srvA"), cfg)
	ctx := context.Background()

	// A Prime-Video-style export with separate columns.
	prime := "Date Watched,Type,Title,Episode Title,Season\n" +
		"2024-02-01,TV Show,Stranger Things,Chapter One: The Vanishing of Will Byers,1\n" +
		"2024-02-02,Movie,Glass Onion: A Knives Out Mystery,,\n"
	res, err := eng.ImportStreaming(ctx, strings.NewReader(prime), StreamImport{Service: "Prime Video", User: "alice"}, nil)
	if err != nil || res.Added != 2 {
		t.Fatalf("prime import: %+v %v", res, err)
	}
	rows, _ := st.History(ctx, store.HistoryQuery{User: "alice"})
	for _, r := range rows {
		if r.Source != "primevideo-import" {
			t.Fatalf("source: %s", r.Source)
		}
		if r.Media.Kind == "episode" && r.Media.Episode != 1 {
			t.Fatalf("episode number not found: %+v", r.Media)
		}
	}

	// Userscript-style live event with a Hulu-like subtitle.
	m, err := eng.HandleScrobble(ctx, ScrobbleRequest{Action: "watched", Service: "Hulu", User: "bob",
		RawTitle: "Stranger Things", Subtitle: "S1 E2 · Chapter Two: The Weirdo on Maple Street"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Episode != 2 || m.Season != 1 || m.ShowIDs.TMDB != 66732 {
		t.Fatalf("scrobble not resolved: %+v", m)
	}
	rows, _ = st.History(ctx, store.HistoryQuery{User: "bob"})
	if len(rows) != 1 || rows[0].Source != "hulu" {
		t.Fatalf("scrobble play: %+v", rows)
	}
	if _, err := eng.HandleScrobble(ctx, ScrobbleRequest{Action: "dance", RawTitle: "x"}); err == nil {
		t.Fatal("bad action accepted")
	}
}

func TestJellyfinAndEmbyWebhooks(t *testing.T) {
	tm := fakeTMDB(t)
	defer tm.Close()
	eng, st, _ := testEngine(t, newFakePlex("srvA"), &config.Config{TMDBKey: "tk", TMDBURL: tm.URL})
	ctx := context.Background()
	jf := `{"NotificationType":"PlaybackStop","ItemType":"Episode","Name":"Chapter One: The Vanishing of Will Byers",
		"SeriesName":"Stranger Things","SeasonNumber":1,"EpisodeNumber":1,"NotificationUsername":"alice",
		"PlaybackPositionTicks":28000000000,"RunTimeTicks":29000000000,"PlayedToCompletion":"true","DeviceName":"Shield"}`
	if err := eng.HandleMediaServerWebhook(ctx, []byte(jf)); err != nil {
		t.Fatal(err)
	}
	emby := `{"Event":"item.markplayed","User":{"Name":"bob"},"Item":{"Name":"Glass Onion: A Knives Out Mystery",
		"Type":"Movie","ProductionYear":2022,"ProviderIds":{"Imdb":"tt11564570"}}}`
	if err := eng.HandleMediaServerWebhook(ctx, []byte(emby)); err != nil {
		t.Fatal(err)
	}
	a, _ := st.History(ctx, store.HistoryQuery{User: "alice"})
	b, _ := st.History(ctx, store.HistoryQuery{User: "bob"})
	if len(a) != 1 || a[0].Media.Episode != 1 || a[0].Source != "jellyfin" || !strings.Contains(a[0].Player, "Shield") {
		t.Fatalf("jellyfin: %+v", a)
	}
	if len(b) != 1 || b[0].Media.IDs.IMDB != "tt11564570" || b[0].Source != "emby" {
		t.Fatalf("emby: %+v", b)
	}
	// progress events and music are ignored
	if err := eng.HandleMediaServerWebhook(ctx, []byte(`{"NotificationType":"PlaybackProgress","ItemType":"Movie","Name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := eng.HandleMediaServerWebhook(ctx, []byte(`{"NotificationType":"PlaybackStop","ItemType":"Audio","Name":"song"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestWhereToWatchAndOnlyMine(t *testing.T) {
	tm := fakeTMDB(t)
	defer tm.Close()
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "text",
			"text": `[{"kind":"movie","title":"Glass Onion: A Knives Out Mystery","year":2022,"reason":"twisty"},
			          {"kind":"movie","title":"Some Unknown Doc","year":2001,"reason":"?"}]`}}})
	}))
	defer ai.Close()
	cfg := &config.Config{TMDBKey: "tk", TMDBURL: tm.URL, StreamingRegion: "US", MyServices: []string{"netflix"},
		AnthropicKey: "k", AnthropicURL: ai.URL, AnthropicModel: "m"}
	eng, st, _ := testEngine(t, newFakePlex("srvA"), cfg)
	ctx := context.Background()
	if err := eng.Recommend(ctx, RecRequest{Provider: "claude", Count: 5, OnlyMine: true}, noReport); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.RecRuns(ctx, 1)
	if len(runs) != 1 || len(runs[0].Recs) != 1 {
		t.Fatalf("only-mine filter: %+v", runs[0].Recs)
	}
	r := runs[0].Recs[0]
	if r.Title != "Glass Onion: A Knives Out Mystery" || len(r.Providers) != 1 || r.Providers[0] != "Netflix" || r.IMDB != "tt11564570" {
		t.Fatalf("providers not attached: %+v", r)
	}
}

func TestCompressedBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	name, err := st.Backup(context.Background(), 2)
	if err != nil || !strings.HasSuffix(name, ".db.gz") {
		t.Fatalf("backup: %s %v", name, err)
	}
	for i := 0; i < 2; i++ {
		time.Sleep(1100 * time.Millisecond)
		if _, err := st.Backup(context.Background(), 2); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "backups", "*"))
	if len(files) != 2 {
		t.Fatalf("keep=2 should leave 2 backups and no partial files, got %v", files)
	}
	if fi, _ := os.Stat(files[0]); fi.Size() == 0 {
		t.Fatal("empty backup")
	}
}
