package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestRecsPageIgnoreRestoreAndLinks(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminUser: "admin", WebhookToken: "hook", SessionTTL: time.Hour, WatchedPercent: 80,
		AnthropicKey: "test-key", AnthropicModel: "m", GeminiModel: "g", StreamingRegion: "US", RecRefresh: 24 * time.Hour,
		Arr: []config.ArrInstance{{Kind: "radarr", Name: "radarr"}, {Kind: "radarr", Name: "radarr2", Adult: true}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	ctx := context.Background()
	run := &store.RecRun{User: "", Provider: "claude", Model: "m", Prompt: "p", Recs: []store.Rec{
		{Kind: "movie", Title: "Paths of Glory", Year: 1957, Reason: "Kubrick", Providers: []string{"YouTube TV", "Kanopy", "Magnolia Selects Amazon Channel", "Obscure Stream", "Tubi TV", "Hoopla"}},
		{Kind: "show", Title: "Red Dwarf", Year: 1988, Reason: "Sci-fi"},
	}}
	if err := st.SaveRecRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	failed := &store.RecRun{User: "", Provider: "gemini", Model: "g", Prompt: "p", Error: "Gemini is busy right now. Try again in a few minutes.", ErrorDetail: `503: {"error": "overloaded"}`}
	if err := st.SaveRecRun(ctx, failed); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, h, "10.0.4.1")
	body := c.get("/recs").Body.String()
	for _, want := range []string{
		`href="https://tv.youtube.com/search?q=Paths&#43;of&#43;Glory"`,
		`href="https://www.kanopy.com/search?query=Paths&#43;of&#43;Glory"`,
		`href="https://tubitv.com/search/Paths%20of%20Glory"`,
		`href="https://www.amazon.com/s?i=instant-video&amp;k=Paths&#43;of&#43;Glory"`,
		`href="https://www.justwatch.com/us/search?q=Paths&#43;of&#43;Glory"`,
		`+ 2 more`, `/recs/` + strconv.FormatInt(run.Recs[0].ID, 10) + `/ignore`,
		"Gemini is busy right now.", "Technical details", "Sources used: 3 of 4",
		`value="arr:radarr" checked`, `value="arr:radarr2" >`, "Refreshes on its own every 24 hours",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(body, `<p class="bad small">503`) {
		t.Error("raw error text should be tucked into details")
	}

	w := c.form("/recs/"+strconv.FormatInt(run.Recs[0].ID, 10)+"/ignore", nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ignore: %d", w.Code)
	}
	body = c.get("/recs").Body.String()
	if strings.Contains(body, "Kubrick") || !strings.Contains(body, "Red Dwarf") || !strings.Contains(body, "1 title that won't be recommended") {
		t.Fatal("an ignored title should leave the list and appear under Ignored")
	}
	w = c.form("/recs/unignore", url.Values{"user": {""}, "kind": {"movie"}, "norm": {store.NormTitle("Paths of Glory")}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("restore: %d", w.Code)
	}
	if body = c.get("/recs").Body.String(); !strings.Contains(body, "Kubrick") {
		t.Fatal("a restored title should show again")
	}

	// Each person sees only their own runs.
	if body = c.get("/recs?user=Someone").Body.String(); strings.Contains(body, "Red Dwarf") || !strings.Contains(body, "No recommendations yet") {
		t.Fatal("another person's page should be empty")
	}
	if w := c.form("/recs", url.Values{"user": {"x"}, "src_shown": {"1"}}); !strings.HasPrefix(w.Header().Get("Location"), "/recs?user=x") {
		t.Fatalf("no sources chosen should send them back: %s", w.Header().Get("Location"))
	}
}
