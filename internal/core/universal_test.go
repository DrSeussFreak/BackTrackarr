package core

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func uniFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func uniZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "export.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for n, b := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(b))
	}
	zw.Close()
	f.Close()
	return p
}

func uniCount(t *testing.T, st *store.Store, q string) int { return shelfCount(t, st, q) }

func TestImportTraktExportZip(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	p := uniZip(t, map[string]string{
		"watched/history.json": `[
		 {"id":1,"watched_at":"2024-01-02T20:00:00.000Z","type":"movie","movie":{"title":"Heat","year":1995,"ids":{"trakt":1,"imdb":"tt0113277","tmdb":949}}},
		 {"id":2,"watched_at":"2025-03-04T20:00:00.000Z","type":"movie","movie":{"title":"Heat","year":1995,"ids":{"trakt":1,"imdb":"tt0113277","tmdb":949}}},
		 {"id":3,"watched_at":"2024-02-02T20:00:00.000Z","type":"episode","episode":{"season":1,"number":2,"title":"Two","ids":{"tvdb":22}},"show":{"title":"Andor","year":2022,"ids":{"tvdb":393189,"tmdb":83867}}}]`,
		"ratings/ratings-movies.json": `[{"rated_at":"2024-01-03T00:00:00.000Z","rating":9,"type":"movie","movie":{"title":"Heat","year":1995,"ids":{"trakt":1,"imdb":"tt0113277","tmdb":949}}}]`,
		"ratings/ratings-shows.json":  `[{"rated_at":"2024-01-03T00:00:00.000Z","rating":8,"type":"show","show":{"title":"Andor","year":2022,"ids":{"tvdb":393189}}}]`,
		"lists/watchlist.json":        `[{"listed_at":"2024-01-01T00:00:00.000Z","type":"movie","movie":{"title":"Ran","year":1985,"ids":{"trakt":9}}}]`,
		"__MACOSX/._junk.json":        `not json`,
		"notes.bin":                   "ignored",
	})
	res, err := e.ImportAnything(ctx, p, "trakt.zip", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plays != 3 || res.Ratings != 1 || res.Kept != 2 {
		t.Fatalf("%+v", res)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays p JOIN media m ON m.id=p.media_id WHERE m.title='Heat'`); n != 2 {
		t.Errorf("each rewatch is its own play, got %d", n)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM media WHERE title='Ran'`); n != 0 {
		t.Error("a watchlist item must not count as watched")
	}
	again, err := e.ImportAnything(ctx, p, "trakt.zip", "", nil)
	if err != nil || again.Plays != 0 || again.Kept != 0 || again.Duplicates != 3 {
		t.Fatalf("re-import: %+v %v", again, err)
	}
}

func TestImportSimklBackup(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	p := uniFile(t, "SimklBackup.json", `{"movies":[
	  {"status":"completed","last_watched_at":"2023-05-01T10:00:00Z","user_rating":8,"movie":{"title":"Dune","year":2021,"ids":{"simkl":5,"imdb":"tt1160419","tmdb":"438631"}}},
	  {"status":"plantowatch","movie":{"title":"Later","year":2020,"ids":{"simkl":6}}}],
	 "shows":[{"status":"watching","show":{"title":"Severance","year":2022,"ids":{"simkl":7,"tvdb":"371980"}},
	  "seasons":[{"number":1,"episodes":[{"number":1,"watched_at":"2023-06-01T10:00:00Z"},{"number":2,"watched_at":"2023-06-02T10:00:00Z"},{"number":3}]}]}],
	 "anime":[{"status":"completed","anime":{"title":"Cowboy Bebop","year":1998,"ids":{"simkl":8}},"seasons":[{"number":1,"episodes":[{"number":1,"watched_at":"2022-01-01T00:00:00Z"}]}]}]}`)
	res, err := e.ImportAnything(context.Background(), p, "SimklBackup.json", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plays != 4 || res.Ratings != 1 || res.Kept != 1 {
		t.Fatalf("%+v", res)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays WHERE source='simkl-import'`); n != 4 {
		t.Errorf("source: %d", n)
	}
}

func TestImportLetterboxdIMDbAndGeneric(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	lb := uniZip(t, map[string]string{
		"diary.csv":     "Date,Name,Year,Letterboxd URI,Rating,Rewatch,Tags,Watched Date\n2024-01-05,Alien,1979,https://boxd.it/a,4.5,,,2024-01-04\n2025-01-05,Alien,1979,https://boxd.it/b,5,Yes,,2025-01-04\n",
		"ratings.csv":   "Date,Name,Year,Letterboxd URI,Rating\n2024-02-01,Brazil,1985,https://boxd.it/c,4\n",
		"watchlist.csv": "Date,Name,Year,Letterboxd URI\n2024-02-01,Ikiru,1952,https://boxd.it/d\n",
	})
	res, err := e.ImportAnything(ctx, lb, "letterboxd.zip", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plays != 2 || res.Ratings != 3 || res.Kept != 1 {
		t.Fatalf("letterboxd: %+v", res)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM ratings r JOIN media m ON m.id=r.media_id WHERE m.title='Brazil' AND r.rating=8`); n != 1 {
		t.Error("Letterboxd's 4 of 5 should be 8 of 10")
	}
	imdb := uniFile(t, "ratings.csv", "Const,Your Rating,Date Rated,Title,URL,Title Type,IMDb Rating,Runtime (mins),Year\ntt0133093,10,2020-01-01,The Matrix,u,Movie,8.7,136,1999\ntt0903747,9,2020-01-02,Breaking Bad,u,TV Series,9.5,45,2008\n")
	res, err = e.ImportAnything(ctx, imdb, "ratings.csv", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plays != 0 || res.Ratings != 1 || res.Kept != 1 {
		t.Fatalf("imdb: %+v", res)
	}
	gen := uniFile(t, "tracker.csv", "Show,Season,Episode,Episode Title,Watched At,Rating\nThe Wire,1,1,The Target,2021-03-04 20:00:00,9\n")
	res, err = e.ImportAnything(ctx, gen, "tracker.csv", "", nil)
	if err != nil || res.Plays != 1 || res.Ratings != 1 {
		t.Fatalf("generic: %+v %v", res, err)
	}
	netflix := uniFile(t, "NetflixViewingHistory.csv", "Title,Date\n\"Stranger Things: Season 1: Chapter One\",1/2/24\nHeat,3/4/24\n")
	res, err = e.ImportAnything(ctx, netflix, "NetflixViewingHistory.csv", "", nil)
	if err != nil || res.Plays != 2 {
		t.Fatalf("netflix: %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(res.Formats, ","), "Netflix") {
		t.Errorf("formats: %v", res.Formats)
	}
	if _, err := e.ImportAnything(ctx, uniFile(t, "x.csv", "a,b\n"), "x.csv", "", nil); err == nil {
		t.Error("a file with nothing readable should say so")
	}
}

func TestImportBrowserHistory(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	res, err := e.ImportBrowserHistory(context.Background(), "Netflix", "me", []BrowserHistoryItem{{Title: "Heat", Date: "1/2/24"}, {Title: "", Date: "1/2/24"}})
	if err != nil || res.Added != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays WHERE source='netflix-import'`); n != 1 {
		t.Errorf("plays: %d", n)
	}
}

func TestImportRoutesBooksAndGames(t *testing.T) {
	e, st := shelfEngine(t, config.ShelfConfig{User: "me"})
	ctx := context.Background()
	gr := uniFile(t, "goodreads_library_export.csv", "Book Id,Title,Author,Date Read,Exclusive Shelf,Read Count\n1,Dune,Frank Herbert,2023/04/05,read,1\n")
	res, err := e.ImportAnything(ctx, gr, "goodreads_library_export.csv", "", nil)
	if err != nil || res.Shelf != 1 {
		t.Fatalf("books: %+v %v", res, err)
	}
	pn := uniFile(t, "playnite.json", `[{"Name":"Hades","GameId":"1","PluginId":"p","Playtime":3600}]`)
	res, err = e.ImportAnything(ctx, pn, "playnite.json", "", nil)
	if err != nil || res.Shelf != 1 {
		t.Fatalf("games: %+v %v", res, err)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM shelf_items`); n != 2 {
		t.Errorf("items: %d", n)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays`); n != 0 {
		t.Errorf("books and games must not become movie plays: %d", n)
	}
}
