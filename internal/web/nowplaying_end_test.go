package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestEndSessionFromHome(t *testing.T) {
	h, st := newTestServer(t, "s3cret")
	ctx := context.Background()
	m := model.Media{Kind: model.Episode, ShowTitle: "Show", Season: 1, Episode: 2, Title: "Pilot"}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	const key = "Alex|1|Den"
	if err := st.PutSession(ctx, store.Session{Key: key, MediaID: m.ID, User: "Alex", State: model.ActStart, Player: "Den", Source: "plex-webhook"}); err != nil {
		t.Fatal(err)
	}

	anon := newClient(t, h, "10.0.2.1")
	if w := anon.form("/api/now-playing/end", url.Values{"key": {key}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("ending a session must require sign-in: %d", w.Code)
	}

	c := newClient(t, h, "10.0.2.2")
	c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	w := c.do("GET", "/api/now-playing", nil, nil)
	var list struct {
		Items []store.NowPlayingRow `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].Key != key {
		t.Fatalf("now playing should give each row's key: %s", w.Body.String())
	}
	if w := c.form("/api/now-playing/end", url.Values{"key": {key}}); w.Code != http.StatusOK {
		t.Fatalf("end: %d %s", w.Code, w.Body.String())
	}
	if rows, _ := st.NowPlaying(ctx); len(rows) != 0 {
		t.Fatalf("session should be gone: %d left", len(rows))
	}
	if w := c.form("/api/now-playing/end", url.Values{"key": {key}}); w.Code != http.StatusNotFound {
		t.Fatalf("already ended: %d", w.Code)
	}
}
