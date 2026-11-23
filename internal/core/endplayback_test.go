package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

type fakeStop struct {
	mu         sync.Mutex
	items      []map[string]any
	terminated []string
	status     int
}

func stream(rating, player, user, id string) map[string]any {
	return map[string]any{"ratingKey": rating, "type": "episode", "duration": 1000, "viewOffset": 100,
		"Player": map[string]any{"title": player, "state": "playing"}, "User": map[string]any{"id": "2", "title": user},
		"Session": map[string]any{"id": id}}
}

func stopEngine(t *testing.T) (*Engine, *store.Store, *fakeStop) {
	t.Helper()
	f := &fakeStop{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/status/sessions":
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"size": len(f.items), "Metadata": f.items}})
		case "/status/sessions/terminate":
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			f.terminated = append(f.terminated, r.URL.Query().Get("sessionId"))
		default:
			http.NotFound(w, r)
		}
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

func TestEndPlaybackStopsOnlyThatStream(t *testing.T) {
	e, st, f := stopEngine(t)
	ctx := context.Background()
	startEp(t, e, "7", "Target", "Den", "plex-webhook", model.ActStart)
	row := showing(t, st)["Target"]
	f.items = []map[string]any{stream("7", "Office", "Alex", "other-player"), stream("7", "Den", "Sam", "other-user"), stream("7", "Den", "Alex", "right-one")}

	res, err := e.EndPlayback(ctx, row.Key, "")
	if err != nil || !res.Stopped {
		t.Fatalf("result %+v err %v", res, err)
	}
	if len(f.terminated) != 1 || f.terminated[0] != "right-one" {
		t.Fatalf("stopped %v", f.terminated)
	}
	if _, ok := showing(t, st)["Target"]; ok {
		t.Fatal("a stopped stream should leave Now Playing")
	}
}

func TestEndPlaybackKeepsRowWhenPlexRefuses(t *testing.T) {
	e, st, f := stopEngine(t)
	startEp(t, e, "7", "Target", "Den", "plex-webhook", model.ActStart)
	row := showing(t, st)["Target"]
	f.items = []map[string]any{stream("7", "Den", "Alex", "right-one")}
	f.status = http.StatusForbidden

	_, err := e.EndPlayback(context.Background(), row.Key, "")
	var shown EndPlaybackError
	if !errors.As(err, &shown) {
		t.Fatalf("expected a message for the user, got %v", err)
	}
	if _, ok := showing(t, st)["Target"]; !ok {
		t.Fatal("if Plex didn't stop it, it must stay on screen")
	}
}

func TestEndPlaybackRefusesAmbiguousStream(t *testing.T) {
	e, st, f := stopEngine(t)
	startEp(t, e, "7", "Target", "Den", "plex-webhook", model.ActStart)
	row := showing(t, st)["Target"]
	f.items = []map[string]any{stream("7", "Den", "Alex", "one"), stream("7", "Den", "Alex", "two")}
	if _, err := e.EndPlayback(context.Background(), row.Key, ""); err == nil {
		t.Fatal("two matching streams must not be guessed between")
	}
	if len(f.terminated) != 0 {
		t.Fatalf("stopped %v", f.terminated)
	}
}

func TestEndPlaybackNonPlexOnlyClearsRow(t *testing.T) {
	e, st, f := stopEngine(t)
	startEp(t, e, "8", "Browser", "Netflix", "netflix", model.ActStart)
	row := showing(t, st)["Browser"]
	if e.CanStopPlayback(row.Source) {
		t.Fatal("non-Plex sources can't be stopped")
	}
	res, err := e.EndPlayback(context.Background(), row.Key, "")
	if err != nil || !res.Found || res.Stopped || len(f.terminated) != 0 {
		t.Fatalf("result %+v err %v stopped %v", res, err, f.terminated)
	}
}
