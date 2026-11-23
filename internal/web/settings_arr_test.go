package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestSettingsArrOptions(t *testing.T) {
	var keyed atomic.Int32
	arr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "saved-key" {
			keyed.Add(1)
		}
		if k := r.Header.Get("X-Api-Key"); k != "saved-key" && k != "typed-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/rootfolder":
			json.NewEncoder(w).Encode([]map[string]string{{"path": "/tv"}, {"path": "/anime"}})
		case "/api/v3/qualityprofile":
			json.NewEncoder(w).Encode([]map[string]string{{"name": "HD-1080p"}, {"name": "Ultra-HD"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer arr.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "saved-key" {
			t.Error("the saved key was sent to a different server")
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer other.Close()
	t.Setenv("SONARR_URL", arr.URL)
	t.Setenv("SONARR_API_KEY", "saved-key")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log)
	post := func(prefix, addr, key string) (int, map[string]any) {
		body := url.Values{"prefix": {prefix}, "url": {addr}, "key": {key}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/settings/arr-options", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.settingsArrOptions(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if _, out := post("SONARR", arr.URL+"/", ""); len(out["roots"].([]any)) != 2 || out["profiles"].([]any)[1] != "Ultra-HD" {
		t.Fatalf("saved key: %+v", out)
	}
	if keyed.Load() == 0 {
		t.Fatal("the saved key wasn't used")
	}
	if _, out := post("SONARR", other.URL, ""); out["error"] == nil {
		t.Fatalf("a new address used the saved key: %+v", out)
	}
	if _, out := post("RADARR_2", arr.URL, "typed-key"); len(out["roots"].([]any)) != 2 {
		t.Fatalf("typed key: %+v", out)
	}
	if _, out := post("RADARR", arr.URL, "wrong"); out["error"] == nil || strings.Contains(out["error"].(string), "wrong") {
		t.Fatalf("bad key: %+v", out)
	}
	if code, _ := post("PLEX", arr.URL, "typed-key"); code != http.StatusBadRequest {
		t.Fatalf("unknown app: HTTP %d", code)
	}
}
