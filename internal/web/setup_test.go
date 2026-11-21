package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestSetupSignInWithPlex(t *testing.T) {
	config.SetOverrides(nil)
	t.Cleanup(func() { config.SetOverrides(nil) })
	var mu sync.Mutex
	approved := false
	var fake *httptest.Server
	fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/pins":
			io.WriteString(w, `{"id":42,"code":"abc"}`)
		case r.URL.Path == "/api/v2/pins/42":
			if approved {
				io.WriteString(w, `{"authToken":"account-token"}`)
			} else {
				io.WriteString(w, `{"authToken":null}`)
			}
		case r.URL.Path == "/api/v2/resources":
			fmt.Fprintf(w, `[{"name":"Other","provides":"player","owned":true},{"name":"Shared","provides":"server","owned":false},
			 {"name":"Home","clientIdentifier":"machine1","provides":"server","owned":true,"accessToken":"server-token",
			  "connections":[{"uri":"http://127.0.0.1:1","relay":true,"local":false},{"uri":%q,"local":true,"relay":false}]}]`, fake.URL)
		case r.URL.Path == "/identity":
			if r.Header.Get("X-Plex-Token") != "server-token" {
				http.Error(w, "auth", 401)
				return
			}
			io.WriteString(w, `{"MediaContainer":{"machineIdentifier":"machine1"}}`)
		case r.URL.Path == "/accounts":
			io.WriteString(w, `{"MediaContainer":{"Account":[{"id":0,"name":""},{"id":1,"name":"Owner"},{"id":5,"name":"Kid"}]}}`)
		case r.URL.Path == "/library/sections":
			io.WriteString(w, `{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies"},{"key":"2","type":"movie","title":"Adult"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{PlexTVURL: fake.URL, StreamingRegion: "US"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	call := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(http.MethodGet, "/setup", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Sign in with Plex") {
		t.Fatalf("setup page: %d", w.Code)
	}
	var start struct {
		ID      int64  `json:"id"`
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal(call(http.MethodPost, "/setup/plex/start", nil).Body.Bytes(), &start); err != nil || start.ID != 42 || !strings.HasPrefix(start.AuthURL, "https://app.plex.tv/auth#?") {
		t.Fatalf("start: %+v %v", start, err)
	}
	id := url.Values{"id": {"42"}}
	if b := call(http.MethodPost, "/setup/plex/status", id).Body.String(); !strings.Contains(b, "waiting") {
		t.Fatalf("before approval: %s", b)
	}
	mu.Lock()
	approved = true
	mu.Unlock()
	b := call(http.MethodPost, "/setup/plex/status", id).Body.String()
	if !strings.Contains(b, `"done"`) || strings.Contains(b, "token") {
		t.Fatalf("after approval, and tokens must stay server-side: %s", b)
	}
	m, _ := st.Settings(context.Background())
	if m["PLEX_URL"] != fake.URL || m["PLEX_TOKEN"] != "server-token" {
		t.Fatalf("saved: url ok=%v token ok=%v", m["PLEX_URL"] == fake.URL, m["PLEX_TOKEN"] == "server-token")
	}

	// With Plex connected, users and libraries become checkboxes.
	cfg.PlexURL, cfg.PlexToken = fake.URL, "server-token"
	page := call(http.MethodGet, "/setup", nil).Body.String()
	for _, want := range []string{`value="Owner" checked`, `value="Kid" >`, `value="Adult" checked`, `value="Movies" >`} {
		if !strings.Contains(page, want) {
			t.Errorf("setup page is missing %s", want)
		}
	}
	if strings.Contains(page, "server-token") {
		t.Error("token shown on the page")
	}
	call(http.MethodPost, "/setup/plex/people", url.Values{"users": {"Owner", "Kid"}, "private": {"Adult"}, "next": {"/setup"}})
	m, _ = st.Settings(context.Background())
	if m["SCROBBLE_USERS"] != "Owner,Kid" || m["PRIVATE_LIBRARIES"] != "Adult" {
		t.Fatalf("choices: %q %q", m["SCROBBLE_USERS"], m["PRIVATE_LIBRARIES"])
	}
}

func TestSetupConnectApp(t *testing.T) {
	config.SetOverrides(map[string]string{"RADARR_URL": "http://10.0.0.5:7878"})
	t.Cleanup(func() { config.SetOverrides(nil) })
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must only read: %s", r.Method)
		}
		if r.Header.Get("X-Api-Key") != "good-key" {
			http.Error(w, "auth", 401)
			return
		}
		io.WriteString(w, `{"instanceName":"Radarr 4K"}`)
	}))
	defer fake.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	post := func(key string) *httptest.ResponseRecorder {
		f := url.Values{"kind": {"radarr"}, "url": {fake.URL}, "key": {key}}
		r := httptest.NewRequest(http.MethodPost, "/setup/app", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post("bad-key"); w.Code != 400 || !strings.Contains(w.Body.String(), "rejected") {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}
	if m, _ := st.Settings(context.Background()); len(m) != 0 {
		t.Fatal("a rejected key was saved")
	}
	if w := post("good-key"); w.Code != 200 {
		t.Fatalf("good key: %d %s", w.Code, w.Body)
	}
	m, _ := st.Settings(context.Background())
	if m["RADARR_2_URL"] != fake.URL || m["RADARR_2_API_KEY"] != "good-key" || m["RADARR_2_NAME"] != "radarr-4k" {
		t.Fatalf("should take the next free slot: %v", m)
	}
}

func TestSetupSecondRadarrWithOptions(t *testing.T) {
	config.SetOverrides(map[string]string{"RADARR_URL": "http://10.0.0.5:7878"})
	t.Cleanup(func() { config.SetOverrides(nil) })
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "k" {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			io.WriteString(w, `{"instanceName":"Radarr"}`)
		case "/api/v3/qualityprofile":
			io.WriteString(w, `[{"id":1,"name":"HD"},{"id":2,"name":"4K"}]`)
		case "/api/v3/rootfolder":
			io.WriteString(w, `[{"path":"/movies"},{"path":"/adult"}]`)
		}
	}))
	defer fake.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Arr: []config.ArrInstance{{Kind: "radarr", Name: "radarr", URL: "http://10.0.0.5:7878"}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	post := func(path string, f url.Values) string {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	base := url.Values{"kind": {"radarr"}, "url": {fake.URL}, "key": {"k"}}
	if b := post("/setup/app/options", base); !strings.Contains(b, "/adult") || !strings.Contains(b, "4K") {
		t.Fatalf("options: %s", b)
	}
	base.Set("root", "/adult")
	base.Set("profile", "4K")
	base.Set("adult", "1")
	base.Set("name", "Radarr")
	post("/setup/app", base)
	m, _ := st.Settings(context.Background())
	if m["RADARR_2_URL"] != fake.URL || m["RADARR_2_ROOT_FOLDER"] != "/adult" || m["RADARR_2_QUALITY_PROFILE"] != "4K" || m["RADARR_2_ADULT"] != "true" {
		t.Fatalf("saved: %v", m)
	}
	if _, clash := m["RADARR_2_NAME"]; clash {
		t.Fatal("two instances must not share the name radarr")
	}
}
