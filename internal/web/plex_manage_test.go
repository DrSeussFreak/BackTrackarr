package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// fakePMS answers like a Plex server that accepts one token.
func fakePMS(t *testing.T, machine, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity":
			json.NewEncoder(w).Encode(M{"MediaContainer": M{"machineIdentifier": machine}})
		case "/accounts":
			if r.Header.Get("X-Plex-Token") != token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(M{"MediaContainer": M{"Account": []M{{"id": 1, "name": "owner"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeTV(t *testing.T, machine, accepted, serverToken, uri string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != accepted {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]M{{"name": "Home", "clientIdentifier": machine, "provides": "server", "owned": true, "accessToken": serverToken,
			"connections": []M{{"protocol": "http", "uri": uri, "local": false, "relay": false}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func plexManagerServer(t *testing.T, url, token, tv string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminUser: "admin", WebhookToken: "hook", SessionTTL: time.Hour, WatchedPercent: 80, PlexURL: url, PlexToken: token, PlexTVURL: tv}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log), st
}

func TestPlexManagerHealthy(t *testing.T) {
	pms := fakePMS(t, "m1", "good")
	s, st := plexManagerServer(t, pms.URL, "good", "")
	if h := s.checkPlex(context.Background()); h.State != plexStateOK {
		t.Fatalf("state %s", h.State)
	}
	if id, _ := st.GetKV(context.Background(), plexMachineKey); id != "m1" {
		t.Fatalf("machine id %q", id)
	}
}

func TestPlexManagerFollowsNewAddress(t *testing.T) {
	ctx := context.Background()
	moved := fakePMS(t, "m1", "good")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	tv := fakeTV(t, "m1", "good", "good", moved.URL)
	s, st := plexManagerServer(t, deadURL, "good", tv.URL)
	if err := st.SetKV(ctx, plexMachineKey, "m1"); err != nil {
		t.Fatal(err)
	}
	if h := s.checkPlex(ctx); h.State != plexStateOffline {
		t.Fatalf("one failed check must not change anything: %s", h.State)
	}
	if h := s.checkPlex(ctx); h.State != plexStateRepaired {
		t.Fatalf("second check: %s", h.State)
	}
	all, err := st.Settings(ctx)
	if err != nil || all["PLEX_URL"] != moved.URL {
		t.Fatalf("saved address wrong (err %v)", err)
	}
}

func TestPlexManagerAsksForSignInWhenKeyIsDead(t *testing.T) {
	ctx := context.Background()
	pms := fakePMS(t, "m1", "good")
	tv := fakeTV(t, "m1", "other", "good", pms.URL)
	s, st := plexManagerServer(t, pms.URL, "revoked", tv.URL)
	if h := s.checkPlex(ctx); h.State != plexStateSignIn {
		t.Fatalf("state %s", h.State)
	}
	if all, _ := st.Settings(ctx); all["PLEX_TOKEN"] != "" {
		t.Fatal("nothing should be saved")
	}
	c := newClient(t, s.Handler(), "10.0.3.1")
	if body := c.get("/").Body.String(); !strings.Contains(body, "Plex needs you to sign in again") {
		t.Fatal("home page should prompt to reconnect")
	}
}

func TestPlexManagerRefusesWrongServer(t *testing.T) {
	ctx := context.Background()
	other := fakePMS(t, "someone-else", "good")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	tv := fakeTV(t, "m1", "good", "good", other.URL)
	s, st := plexManagerServer(t, deadURL, "good", tv.URL)
	_ = st.SetKV(ctx, plexMachineKey, "m1")
	s.checkPlex(ctx)
	if h := s.checkPlex(ctx); h.State != plexStateOffline {
		t.Fatalf("state %s", h.State)
	}
	if all, _ := st.Settings(ctx); all["PLEX_URL"] != "" {
		t.Fatal("an address that isn't this server must never be saved")
	}
}
