package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

type fakeSessions struct {
	mu    sync.Mutex
	items []map[string]any
	fail  bool
	calls atomic.Int32
}

func (f *fakeSessions) set(fail bool, items ...map[string]any) {
	f.mu.Lock()
	f.fail, f.items = fail, items
	f.mu.Unlock()
}

func playing(key, player, state string, offset int64) map[string]any {
	return map[string]any{"ratingKey": key, "type": "episode", "duration": 1000000, "viewOffset": offset,
		"Player": map[string]any{"title": player, "state": state}}
}

func nowPlayingEngine(t *testing.T) (*Engine, *store.Store, *fakeSessions) {
	t.Helper()
	f := &fakeSessions{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"size": len(f.items), "Metadata": f.items}})
	}))
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{WatchedPercent: 80}
	return New(context.Background(), cfg, st, plex.NewClient(srv.URL, "tok"), nil, slog.New(slog.NewTextHandler(io.Discard, nil))), st, f
}

func startEp(t *testing.T, e *Engine, key, title, player, source string, action string) model.Media {
	t.Helper()
	m := model.Media{Kind: model.Episode, ShowTitle: "Show", Season: 1, Episode: len(key) + int(key[len(key)-1]), Title: title, DurationS: 1000, PlexKey: key}
	if err := e.Ingest(context.Background(), model.Event{Action: action, Media: m, User: "Alex", Owner: true, Player: player, Source: source}, false); err != nil {
		t.Fatal(err)
	}
	return m
}

func age(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB.Exec(`UPDATE sessions SET updated_at=?`, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
}

func showing(t *testing.T, st *store.Store) map[string]store.NowPlayingRow {
	t.Helper()
	rows, err := st.NowPlaying(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.NowPlayingRow{}
	for _, r := range rows {
		out[r.Media.Title] = r
	}
	return out
}

func TestNowPlayingFollowsPlex(t *testing.T) {
	e, st, f := nowPlayingEngine(t)
	ctx := context.Background()
	startEp(t, e, "1", "Gone", "Office TV", "plex-webhook", model.ActStart)
	startEp(t, e, "2", "Gone paused", "Den", "plex-webhook", model.ActPause)
	startEp(t, e, "3", "Still on", "Family Room", "plex-webhook", model.ActStart)
	startEp(t, e, "4", "Browser", "Netflix", "netflix", model.ActStart)
	age(t, st)

	// While Plex is unreachable nothing disappears.
	f.set(true)
	if err := e.SyncNowPlaying(ctx); err == nil {
		t.Fatal("expected an error from the failed check")
	}
	if n := len(showing(t, st)); n != 4 {
		t.Fatalf("an outage removed items: %d left", n)
	}

	f.set(false, playing("3", "Family Room", "paused", 500000))
	if err := e.SyncNowPlaying(ctx); err != nil {
		t.Fatal(err)
	}
	got := showing(t, st)
	if _, ok := got["Gone"]; ok {
		t.Error("stopped item still showing")
	}
	if _, ok := got["Gone paused"]; ok {
		t.Error("stopped paused item still showing")
	}
	if r, ok := got["Still on"]; !ok || r.State != model.ActPause || r.Progress != 50 {
		t.Errorf("live item not updated: %+v", r)
	}
	if _, ok := got["Browser"]; !ok {
		t.Error("a non-Plex source must not be removed by the Plex check")
	}
}

func TestNowPlayingGraceAndIdle(t *testing.T) {
	e, st, f := nowPlayingEngine(t)
	ctx := context.Background()
	if err := e.SyncNowPlaying(ctx); err != nil || f.calls.Load() != 0 {
		t.Fatalf("must not call Plex with nothing showing: %d %v", f.calls.Load(), err)
	}
	startEp(t, e, "1", "Just started", "TV", "plex-webhook", model.ActStart)
	f.set(false)
	if err := e.SyncNowPlaying(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := showing(t, st)["Just started"]; !ok {
		t.Fatal("a brand-new item was removed before Plex listed it")
	}
}

func TestNewItemReplacesOldOnSamePlayer(t *testing.T) {
	e, st, _ := nowPlayingEngine(t)
	startEp(t, e, "1", "Episode one", "Family Room", "plex-webhook", model.ActStart)
	startEp(t, e, "2", "Other room", "Office TV", "plex-webhook", model.ActStart)
	startEp(t, e, "3", "Episode two", "Family Room", "plex-webhook", model.ActStart)
	got := showing(t, st)
	if _, ok := got["Episode one"]; ok {
		t.Error("old item still showing on the same player")
	}
	if len(got) != 2 {
		t.Errorf("showing %d, want 2", len(got))
	}
}

func TestEndedSessionCantCountTwice(t *testing.T) {
	e, st, f := nowPlayingEngine(t)
	ctx := context.Background()
	m := startEp(t, e, "1", "Finale", "TV", "plex-webhook", model.ActWatched)
	age(t, st)
	f.set(false)
	if err := e.SyncNowPlaying(ctx); err != nil {
		t.Fatal(err)
	}
	if len(showing(t, st)) != 0 {
		t.Fatal("finished item still showing")
	}
	if err := e.Ingest(ctx, model.Event{Action: model.ActStop, Media: m, User: "Alex", Owner: true, Player: "TV", Source: "plex-webhook", Progress: 99}, false); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.History(ctx, store.HistoryQuery{User: "Alex"})
	if len(rows) != 1 {
		t.Fatalf("late stop changed the play count: %d", len(rows))
	}
}
