package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func connEngine(t *testing.T) *Engine {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(context.Background(), &config.Config{}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func connFakeArr(t *testing.T, app string, accessible bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a test must only read, got %s", r.Method)
		}
		if r.Header.Get("X-Api-Key") != "good-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			json.NewEncoder(w).Encode(map[string]string{"appName": app, "instanceName": app, "version": "4.0.1"})
		case "/api/v3/rootfolder":
			json.NewEncoder(w).Encode([]map[string]any{{"path": "/tv", "accessible": accessible, "freeSpace": int64(2) << 40}})
		case "/api/v3/qualityprofile":
			json.NewEncoder(w).Encode([]map[string]any{{"name": "HD"}, {"name": "UHD"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectionResults(t *testing.T) {
	e := connEngine(t)
	ctx := context.Background()
	good := connFakeArr(t, "Sonarr", true)
	wrongApp := connFakeArr(t, "Radarr", true)
	badRoot := connFakeArr(t, "Sonarr", false)
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>Sign in</html>") }))
	defer html.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) }))
	defer redirect.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	l.Close()

	for _, c := range []struct {
		name, addr, key, status, want string
	}{
		{"works", good.URL, "good-key", ConnOK, "Connected"},
		{"bad key", good.URL, "secret-wrong-key", ConnFail, "rejected the key"},
		{"wrong app", wrongApp.URL, "good-key", ConnFail, "is Radarr"},
		{"bad root", badRoot.URL, "good-key", ConnWarn, "root folder is unavailable"},
		{"login page", html.URL, "good-key", ConnFail, "web page"},
		{"redirect", redirect.URL, "good-key", ConnFail, "redirected"},
		{"nothing there", closed, "good-key", ConnFail, "Nothing is answering"},
		{"bad address", "not-a-url", "good-key", ConnFail, "isn't complete"},
	} {
		r, ok := e.TestConnection(ctx, "sonarr", map[string]string{"SONARR_URL": c.addr, "SONARR_API_KEY": c.key})
		if !ok || r.Status != c.status || !strings.Contains(r.Headline, c.want) {
			t.Errorf("%s: %+v", c.name, r)
		}
		if c.status != ConnOK && r.Fix == "" {
			t.Errorf("%s: no advice on how to fix it", c.name)
		}
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "secret-wrong-key") || strings.Contains(string(b), c.addr) && c.addr != "not-a-url" {
			t.Errorf("%s leaked a key or address: %s", c.name, b)
		}
	}
	r, _ := e.TestConnection(ctx, "sonarr", map[string]string{"SONARR_URL": closed, "SONARR_API_KEY": "good-key"})
	if !strings.Contains(r.Fix, "localhost") {
		t.Errorf("a localhost address should get the Docker hint: %+v", r)
	}
	if _, ok := e.TestConnection(ctx, "nope", nil); ok {
		t.Error("an unknown app was tested")
	}
}

func TestConnectionNeverSendsSavedKeyToNewAddress(t *testing.T) {
	saved := connFakeArr(t, "Sonarr", true)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached the new address with key %q", r.Header.Get("X-Api-Key"))
	}))
	defer other.Close()
	t.Setenv("SONARR_URL", saved.URL)
	t.Setenv("SONARR_API_KEY", "good-key")
	e := connEngine(t)
	r, _ := e.TestConnection(context.Background(), "sonarr", map[string]string{"SONARR_URL": other.URL})
	if r.Status != ConnFail || !strings.Contains(r.Headline, "new address") {
		t.Fatalf("%+v", r)
	}
	r, _ = e.TestConnection(context.Background(), "sonarr", nil)
	if r.Status != ConnOK {
		t.Fatalf("saved values: %+v", r)
	}
	var found bool
	for _, s := range e.ConnStates(context.Background()) {
		found = found || (s.ID == "sonarr" && s.Status == ConnOK)
	}
	if !found {
		t.Fatal("the last result wasn't kept")
	}
}

func TestEverySettingsAppHasATest(t *testing.T) {
	e := connEngine(t)
	ids := map[string]bool{}
	for _, p := range e.ConnProbes() {
		if ids[p.ID] {
			t.Errorf("duplicate test %s", p.ID)
		}
		ids[p.ID] = true
	}
	for _, id := range []string{"plex", "trakt", "simkl", "floppy", "relays", "webhooks", "tmdb", "claude", "gemini", "tautulli", "crosswatch", "plekt", "seerr",
		"audiobookshelf", "komga", "kavita", "hardcover", "openlibrary", "steam", "retroachievements", "romm", "gdrive", "sonarr", "radarr", "whisparr", "jellyfin", "emby"} {
		if !ids[id] {
			t.Errorf("no connection test for %s", id)
		}
	}
}

func TestRelayTestSendsNoEvent(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			posts++
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	e := connEngine(t)
	r, _ := e.TestConnection(context.Background(), "relays", map[string]string{"RELAY_URLS": srv.URL + "/hook?token=abc123"})
	if r.Status != ConnOK || posts != 0 {
		t.Fatalf("%+v posts %d", r, posts)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "abc123") {
		t.Fatalf("token leaked: %s", b)
	}
}
