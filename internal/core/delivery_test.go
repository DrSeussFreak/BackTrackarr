package core

import (
	"context"
	"encoding/json"
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
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func deliveryEngine(t *testing.T, cfg *config.Config) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(context.Background(), cfg, st, nil, sinks.Build(cfg, st), slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func drain(t *testing.T, e *Engine, s sinks.Sink) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if !e.processOne(context.Background(), s) {
			return
		}
	}
	t.Fatal("queue did not drain")
}

func TestFloppyHistorySkipsTitlesWithoutIDs(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		if b["title"] == "Rejected" {
			http.Error(w, `{"detail":"Could not resolve media."}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	e, st := deliveryEngine(t, &config.Config{WatchedPercent: 90, FloppyURL: srv.URL, FloppyToken: "t"})
	ctx := context.Background()
	at := time.Unix(1700000000, 0)
	evs := []model.Event{
		{Action: model.ActWatched, At: at, Media: model.Media{Kind: model.Movie, Title: "No IDs"}},
		{Action: model.ActWatched, At: at, Media: model.Media{Kind: model.Episode, Title: "Ep", ShowTitle: "Bad Show", Season: 1, Episode: 2, IDs: model.IDs{TVDB: 55}}},
		{Action: model.ActWatched, At: at, Media: model.Media{Kind: model.Movie, Title: "Rejected", IDs: model.IDs{TMDB: 2}}},
		{Action: model.ActWatched, At: at, Media: model.Media{Kind: model.Episode, Title: "Ep", ShowTitle: "Good Show", Season: 1, Episode: 3, ShowIDs: model.IDs{TVDB: 77}}},
	}
	for i := 0; i < 2; i++ { // pushing twice must not count skips twice
		if err := st.Enqueue(ctx, "floppy", "history", evs); err != nil {
			t.Fatal(err)
		}
	}
	drain(t, e, e.Sink("floppy"))
	if len(got) != 4 {
		t.Fatalf("sent %d requests, want the 2 identifiable titles twice: %+v", len(got), got)
	}
	if got[1]["series_title"] != "Good Show" {
		t.Fatalf("episode body: %+v", got[1])
	}
	stats, err := st.OutboxStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := stats["floppy"]; s.Skipped != 2 || s.Failed != 2 || strings.Contains(s.LastError, sinks.NoIDReason) {
		t.Fatalf("stats: %+v", s)
	}
}

func TestRelayBadAddressHoldsAndHidesIt(t *testing.T) {
	const secret = "not-a-url-secret-value"
	e, st := deliveryEngine(t, &config.Config{WatchedPercent: 90, RelayURLs: []string{secret}})
	ctx := context.Background()
	s := e.Sink("relay-1")
	if s.Ready(ctx) || strings.Contains(s.Label(), secret) {
		t.Fatalf("ready %v label %q", s.Ready(ctx), s.Label())
	}
	p := s.(sinks.Troubled).Problem()
	if p == "" || strings.Contains(p, secret) {
		t.Fatalf("problem %q", p)
	}
	if err := st.Enqueue(ctx, s.Name(), model.ActWatched, model.Event{Action: model.ActWatched}); err != nil {
		t.Fatal(err)
	}
	if e.processOne(ctx, s) {
		t.Fatal("a bad address must not be attempted")
	}
}

func TestRelayDropsOldWebhooksAndHidesTokenInErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	e, st := deliveryEngine(t, &config.Config{WatchedPercent: 90, RelayURLs: []string{srv.URL + "/hook?token=secret-token"}})
	ctx := context.Background()
	s := e.Sink("relay-1")
	if err := st.Enqueue(ctx, s.Name(), model.ActWatched, model.Event{Action: model.ActWatched}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE outbox SET created_at=created_at-7200`); err != nil {
		t.Fatal(err)
	}
	drain(t, e, s)
	if calls != 0 {
		t.Fatal("an old webhook would look like a play happening now")
	}
	srv.Close()
	err := s.Send(ctx, model.Event{Action: model.ActWatched})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error must not quote the address: %v", err)
	}
}

func TestRelaySettingRejectsBareToken(t *testing.T) {
	var c config.Setting
	for _, s := range config.Settings() {
		if s.Key == "RELAY_URLS" {
			c = s
		}
	}
	_, err := config.Validate(c, "bare-token-value")
	if err == nil || strings.Contains(err.Error(), "bare-token-value") {
		t.Fatalf("got %v", err)
	}
	if _, err := config.Validate(c, "http://localhost:8787/webhook/plexwatcher?token=x, https://example.test/hook"); err != nil {
		t.Fatal(err)
	}
}
