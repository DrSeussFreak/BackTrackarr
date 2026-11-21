package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestIdentifyApp(t *testing.T) {
	for want, pages := range map[string]map[string]string{
		"radarr":         {"/": "<html><head><title>Radarr</title></head>"},
		"komga":          {"/": "<title>Komga</title>"},
		"jellyfin":       {"/System/Info/Public": `{"ProductName":"Jellyfin Server","Id":"x","Version":"10"}`},
		"emby":           {"/System/Info/Public": `{"ServerName":"Home","Id":"x","Version":"4"}`},
		"audiobookshelf": {"/status": `{"app":"audiobookshelf","isInit":true}`},
		"":               {"/": "<title>Some router</title>"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
				t.Error("probes must not send credentials")
			}
			if b, ok := pages[r.URL.Path]; ok {
				io.WriteString(w, b)
				return
			}
			http.NotFound(w, r)
		}))
		if got := identifyApp(context.Background(), srv.URL); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		srv.Close()
	}
}

func TestScanSubnetsOnlyPrivate(t *testing.T) {
	got := ScanSubnets([]string{"http://10.20.30.70:32400", "http://10.20.30.82:8989", "https://plex.example.com", "http://8.8.8.8:80", "", "http://192.168.4.2:1"})
	if want := []string{"10.20.30", "192.168.4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestRequestInSeerr(t *testing.T) {
	var body map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "seerr-key" {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/search":
			query = r.URL.RawQuery
			io.WriteString(w, `{"results":[{"id":5,"mediaType":"movie","firstAirDate":""},{"id":77,"mediaType":"tv","firstAirDate":"2016-07-15"}]}`)
		case "/api/v1/request":
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()
	e, st := shelfEngine(t, config.ShelfConfig{})
	e.cfg.SeerrURL, e.cfg.SeerrKey = srv.URL, "seerr-key"
	run := &store.RecRun{User: "me", Provider: "test", Model: "m", Prompt: "p", Recs: []store.Rec{{Kind: "show", Title: "Stranger Things", Year: 2016}}}
	if err := st.SaveRecRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := st.DB.QueryRow(`SELECT id FROM recs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RequestInSeerr(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "Stranger%20Things") || body["mediaType"] != "tv" || body["mediaId"] != float64(77) || body["seasons"] != "all" {
		t.Fatalf("query %q body %v", query, body)
	}
}
