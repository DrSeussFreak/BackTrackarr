package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestImportEverythingKeepsRewatchesAndRatings(t *testing.T) {
	item := func(key, typ, title string, rating float64, ratedAt int64) map[string]any {
		return map[string]any{"ratingKey": key, "type": typ, "title": title, "guid": "plex://" + typ + "/" + key,
			"userRating": rating, "lastRatedAt": ratedAt, "Guid": []map[string]string{{"id": "tmdb://" + key}}}
	}
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		send := func(mc map[string]any) { json.NewEncoder(w).Encode(map[string]any{"MediaContainer": mc}) }
		start := r.URL.Query().Get("X-Plex-Container-Start")
		switch {
		case r.URL.Path == "/accounts":
			send(map[string]any{"Account": []map[string]any{{"id": 1, "name": "owner"}}})
		case r.URL.Path == "/identity":
			send(map[string]any{"machineIdentifier": "m1"})
		case r.URL.Path == "/library/sections":
			send(map[string]any{"Directory": []map[string]any{{"key": "1", "type": "movie", "title": "Movies"}, {"key": "2", "type": "show", "title": "TV"}}})
		case strings.HasSuffix(r.URL.Path, "history/all"):
			items := []map[string]any{}
			if start == "" || start == "0" {
				for i, at := range []int{1700000000, 1710000000} { // the same movie watched twice
					it := item("11", "movie", "Rewatched", 0, 0)
					it["viewedAt"], it["accountID"], it["historyKey"] = at, 1, strconv.Itoa(i)
					items = append(items, it)
				}
			}
			send(map[string]any{"size": len(items), "Metadata": items})
		case strings.HasPrefix(r.URL.Path, "/library/sections/"):
			if start != "" && start != "0" {
				send(map[string]any{"size": 0, "Metadata": []any{}})
				return
			}
			typ := r.URL.Query().Get("type")
			items := []map[string]any{}
			switch {
			case strings.Contains(r.URL.Path, "/1/") && typ == "1":
				items = append(items, item("11", "movie", "Rewatched", 8, 1720000000), item("12", "movie", "Unrated", 0, 0))
			case strings.Contains(r.URL.Path, "/2/") && typ == "2":
				items = append(items, item("20", "show", "A Whole Show", 9, 1720000000))
			}
			send(map[string]any{"size": len(items), "totalSize": len(items), "Metadata": items})
		case strings.HasPrefix(r.URL.Path, "/library/metadata/"):
			key := strings.TrimPrefix(r.URL.Path, "/library/metadata/")
			send(map[string]any{"Metadata": []map[string]any{item(key, "movie", "Rewatched", 8, 1720000000)}})
		default:
			send(map[string]any{})
		}
	}))
	defer srv.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	e := New(context.Background(), cfg, st, plex.NewClient(srv.URL, "tok"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	count := func(q string) int {
		var n int
		if err := st.DB.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for pass := 0; pass < 2; pass++ {
		if err := e.ImportEverything(ctx, func(string) {}); err != nil {
			t.Fatal(err)
		}
		if n := count(`SELECT COUNT(*) FROM plays`); n != 2 {
			t.Fatalf("pass %d: %d plays, want both watches", pass, n)
		}
		if n := count(`SELECT COUNT(*) FROM ratings WHERE rating=8`); n != 1 {
			t.Fatalf("pass %d: %d movie ratings", pass, n)
		}
		if n := count(`SELECT COUNT(*) FROM ratings`); n != 1 {
			t.Fatalf("pass %d: an unrated title was rated", pass)
		}
		if n := count(`SELECT COUNT(*) FROM import_extras WHERE kind='rating-show'`); n != 1 {
			t.Fatalf("pass %d: show ratings kept: %d", pass, n)
		}
	}
	if writes != 0 {
		t.Fatalf("Plex must only be read, saw %d writes", writes)
	}
}

func TestRatingKeepsNewest(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.DB.Exec(`INSERT INTO media(id,kind,title,updated_at) VALUES (1,'movie','M',0)`); err != nil {
		t.Fatal(err)
	}
	get := func() float64 {
		var r float64
		if err := st.DB.QueryRow(`SELECT rating FROM ratings WHERE media_id=1`).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		rating float64
		at     int64
		want   float64
	}{{6, 100, 6}, {9, 200, 9}, {3, 150, 9}, {2, 0, 9}} {
		if err := st.SetRatingKeepNewest(ctx, 1, "me", c.rating, c.at); err != nil {
			t.Fatal(err)
		}
		if got := get(); got != c.want {
			t.Fatalf("after %v at %d: got %v, want %v", c.rating, c.at, got, c.want)
		}
	}
}
