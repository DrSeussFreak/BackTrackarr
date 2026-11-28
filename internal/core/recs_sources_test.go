package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestParseRecsAcceptsEveryShape(t *testing.T) {
	item := `{"kind":"movie","title":"Thief","year":1981,"reason":"x"}`
	for name, text := range map[string]string{
		"array":       `[` + item + `]`,
		"tool input":  `{"recommendations":[` + item + `]}`,
		"code fence":  "```json\n[" + item + "]\n```",
		"with prose":  "Here you go:\n[" + item + "]\nEnjoy!",
		"fenced tool": "```\n{\"recommendations\":[" + item + "]}\n```",
	} {
		got, err := parseRecs(text)
		if err != nil || len(got) != 1 || got[0].Title != "Thief" {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if _, err := parseRecs("I'd suggest Thief and Heat."); err != errUnreadableReply {
		t.Fatalf("prose only: %v", err)
	}
}

// aiFake stands in for Claude and Gemini.
type aiFake struct {
	mu           sync.Mutex
	claudeBodies []string
	geminiModels []string
	claudeReply  func(n int) any
	deadGemini   string // this model answers 404, like a retired one
}

func (f *aiFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	recs := []map[string]any{
		{"kind": "movie", "title": "Thief", "year": 1981, "reason": "a"},
		{"kind": "movie", "title": "Ronin", "year": 1998, "reason": "b"},
		{"kind": "show", "title": "Crime Story", "year": 1986, "reason": "c"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/v1/messages":
			f.claudeBodies = append(f.claudeBodies, string(b))
			if f.claudeReply != nil {
				json.NewEncoder(w).Encode(f.claudeReply(len(f.claudeBodies)))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "save_recommendations",
				"input": map[string]any{"recommendations": recs}}}})
		case r.URL.Path == "/v1beta/models":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
				{"name": "models/gemini-9.0-pro-preview", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-8.5-pro", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-9.5-pro-image", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-9.9-flash", "supportedGenerationMethods": []string{"generateContent"}},
				{"name": "models/gemini-9.9-pro-embed", "supportedGenerationMethods": []string{"embedContent"}},
			}})
		case strings.Contains(r.URL.Path, ":generateContent"):
			m := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":generateContent")
			f.geminiModels = append(f.geminiModels, m)
			if m == f.deadGemini {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error":{"code":404,"message":"This model is no longer available","status":"NOT_FOUND"}}`)
				return
			}
			txt, _ := json.Marshal(recs)
			json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": string(txt)}}}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func recEngine(t *testing.T, f *aiFake, arr ...config.ArrInstance) (*Engine, *store.Store) {
	t.Helper()
	srv := f.server(t)
	cfg := &config.Config{AnthropicKey: "ck", AnthropicModel: "claude-test", AnthropicURL: srv.URL,
		GeminiKey: "gk", GeminiModel: "gemini-old", GeminiURL: srv.URL, Arr: arr, WatchedPercent: 80}
	eng, st, _ := testEngine(t, newFakePlex("srvA"), cfg)
	return eng, st
}

func TestClaudeIsAskedForStructuredOutput(t *testing.T) {
	f := &aiFake{}
	eng, st := recEngine(t, f)
	ctx := context.Background()
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.claudeBodies[0], `"tool_choice"`) || !strings.Contains(f.claudeBodies[0], "save_recommendations") {
		t.Fatal("Claude wasn't asked to answer through the tool")
	}
	runs, _ := st.RecRuns(ctx, 5)
	if len(runs) != 1 || runs[0].Error != "" || len(runs[0].Recs) != 3 {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestUnreadableReplyIsRetriedOnce(t *testing.T) {
	f := &aiFake{}
	f.claudeReply = func(n int) any {
		text := "I'd suggest a few films you'd enjoy."
		if n > 1 {
			text = `[{"kind":"movie","title":"Thief","year":1981,"reason":"a"}]`
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	}
	eng, st := recEngine(t, f)
	ctx := context.Background()
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	if len(f.claudeBodies) != 2 || !strings.Contains(f.claudeBodies[1], "only the JSON array") {
		t.Fatalf("expected one retry, got %d calls", len(f.claudeBodies))
	}
	runs, _ := st.RecRuns(ctx, 5)
	if runs[0].Error != "" || len(runs[0].Recs) != 1 {
		t.Fatalf("run: %+v", runs[0])
	}
}

func TestOneProviderFailingKeepsTheOther(t *testing.T) {
	f := &aiFake{}
	f.claudeReply = func(int) any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "No list today."}}}
	}
	eng, st := recEngine(t, f)
	ctx := context.Background()
	var said []string
	err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "both", Count: 5}, func(s string) { said = append(said, s) })
	if err != nil {
		t.Fatalf("one failure must not fail the run: %v", err)
	}
	runs, _ := st.RecRuns(ctx, 5)
	byProvider := map[string]*store.RecRun{}
	for _, r := range runs {
		byProvider[r.Provider] = r
	}
	c, g := byProvider["claude"], byProvider["gemini"]
	if c == nil || c.Error != "Claude's reply couldn't be read, even after asking again." || !strings.Contains(c.ErrorDetail, "No list today.") {
		t.Fatalf("claude run: %+v", c)
	}
	if strings.ContainsAny(c.Error, "{}") {
		t.Fatalf("the visible error should be one plain sentence: %q", c.Error)
	}
	if g == nil || g.Error != "" || len(g.Recs) != 3 {
		t.Fatalf("gemini run: %+v", g)
	}
	if last := said[len(said)-1]; !strings.Contains(last, "done, with a problem") {
		t.Fatalf("the job should say one provider had a problem: %q", last)
	}
}

func TestRetiredGeminiModelIsReplacedAutomatically(t *testing.T) {
	f := &aiFake{deadGemini: "gemini-old"}
	eng, st := recEngine(t, f)
	ctx := context.Background()
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "gemini", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	// The newest Pro version wins; image, flash and embedding models never qualify.
	if got := strings.Join(f.geminiModels, ","); got != "gemini-old,gemini-9.0-pro-preview" {
		t.Fatalf("models tried: %s", got)
	}
	runs, _ := st.RecRuns(ctx, 5)
	if runs[0].Error != "" || runs[0].Model != "gemini-9.0-pro-preview" || len(runs[0].Recs) != 3 {
		t.Fatalf("run: %+v", runs[0])
	}
	if m, switched := eng.GeminiModelInUse(ctx); m != "gemini-9.0-pro-preview" || !switched {
		t.Fatalf("in use: %s %v", m, switched)
	}
	// The next request goes straight to the replacement.
	f.geminiModels = nil
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "gemini", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.geminiModels, ","); got != "gemini-9.0-pro-preview" {
		t.Fatalf("second request tried: %s", got)
	}
}

func TestIgnoredTitlesNeverComeBack(t *testing.T) {
	f := &aiFake{}
	eng, st := recEngine(t, f)
	ctx := context.Background()
	if err := st.IgnoreRec(ctx, "me", "movie", "Thief", 1981); err != nil {
		t.Fatal(err)
	}
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.RecRuns(ctx, 5)
	for _, c := range runs[0].Recs {
		if c.Title == "Thief" {
			t.Fatal("an ignored title was recommended")
		}
	}
	if len(runs[0].Recs) != 2 || !strings.Contains(f.claudeBodies[0], "not interested in these") {
		t.Fatalf("recs %+v", runs[0].Recs)
	}
	// Someone else's ignore list doesn't apply.
	if err := eng.Recommend(ctx, RecRequest{User: "other", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	runs, _ = st.RecRuns(ctx, 5)
	if len(runs[0].Recs) != 3 {
		t.Fatalf("other person: %+v", runs[0].Recs)
	}
	if n, _ := st.VisibleRecCount(ctx, "me"); n != 2 {
		t.Fatalf("visible for me: %d", n)
	}
	if err := st.UnignoreRec(ctx, "me", "movie", store.NormTitle("Thief")); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.VisibleRecCount(ctx, "me"); n != 2 { // that run already left it out
		t.Fatalf("after restore: %d", n)
	}
}

func TestAdultFollowsTheChosenSources(t *testing.T) {
	f := &aiFake{}
	eng, st := recEngine(t, f,
		config.ArrInstance{Kind: "radarr", Name: "radarr"},
		config.ArrInstance{Kind: "radarr", Name: "radarr2", Adult: true})
	ctx := context.Background()
	for i, it := range []struct{ inst, title string }{{"radarr", "Paddington"}, {"radarr2", "Private Feature"}, {"radarr", "Ronin"}} {
		if _, err := st.DB.Exec(`INSERT INTO arr_items(instance,arr_id,kind,title,year,hash,seen_at) VALUES(?,?,'movie',?,2000,'h',1)`, it.inst, i+1, it.title); err != nil {
			t.Fatal(err)
		}
	}
	m := model.Media{Kind: model.Movie, Title: "Heat", Year: 1995}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	st.RecordPlay(ctx, m.ID, "me", time.Now(), "trakt-pull", "", false)

	// By default the adult instance is off, and so is anything adult.
	var adultOff, adultOn bool
	for _, s := range eng.RecSources(ctx, "me") {
		switch s.ID {
		case "arr:radarr":
			adultOff = s.Checked && !s.Adult
		case "arr:radarr2":
			adultOn = s.Checked || !s.Adult
		}
	}
	if !adultOff || adultOn {
		t.Fatal("radarr should start checked and radarr2 (adult) unchecked")
	}
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	p := f.claudeBodies[0]
	if strings.Contains(p, "Private Feature") || !strings.Contains(p, "Do not recommend pornographic or adult titles") || !strings.Contains(p, "Paddington") {
		t.Fatalf("default prompt leaked or dropped a source:\n%s", p)
	}
	runs, _ := st.RecRuns(ctx, 5)
	for _, c := range runs[0].Recs {
		if c.Title == "Ronin" {
			t.Fatal("a title already in a chosen collection was recommended")
		}
	}

	// Choosing only the adult instance makes results follow it.
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5, Sources: []string{"arr:radarr2"}}, noReport); err != nil {
		t.Fatal(err)
	}
	p = f.claudeBodies[1]
	if !strings.Contains(p, "Private Feature") || strings.Contains(p, "Paddington") || strings.Contains(p, "Heat (1995)") ||
		!strings.Contains(p, "chose adult sources (radarr2)") || strings.Contains(p, "Do not recommend pornographic") {
		t.Fatalf("adult-source prompt wrong:\n%s", p)
	}

	// A saved choice is used when a request doesn't name sources.
	if err := eng.SaveRecSources(ctx, "me", []string{"plays:trakt"}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Count: 5}, noReport); err != nil {
		t.Fatal(err)
	}
	p = f.claudeBodies[2]
	if !strings.Contains(p, "Heat (1995)") || strings.Contains(p, "Paddington") || strings.Contains(p, "Private Feature") {
		t.Fatalf("saved-source prompt wrong:\n%s", p)
	}
}

func TestRefreshUsesLastChoicesWithoutTheNote(t *testing.T) {
	f := &aiFake{}
	eng, _ := recEngine(t, f)
	ctx := context.Background()
	if eng.RefreshRecs("me") {
		t.Fatal("nobody asked yet, so nothing should refresh")
	}
	if err := eng.Recommend(ctx, RecRequest{User: "me", Provider: "claude", Kind: "movie", Count: 7, Note: "one-off mood"}, noReport); err != nil {
		t.Fatal(err)
	}
	req, ok := eng.lastRecRequest(ctx, "me")
	if !ok || req.Kind != "movie" || req.Count != 7 || req.Provider != "claude" || req.Note != "" {
		t.Fatalf("saved request: %+v %v", req, ok)
	}
	if !eng.RefreshRecs("me") {
		t.Fatal("refresh should start")
	}
	for end := time.Now().Add(10 * time.Second); eng.Jobs.AnyRunning(); {
		if time.Now().After(end) {
			t.Fatal("refresh never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claudeBodies) != 2 || strings.Contains(f.claudeBodies[1], "one-off mood") || !strings.Contains(f.claudeBodies[1], "movies only") {
		t.Fatalf("refresh request wrong (%d calls)", len(f.claudeBodies))
	}
}
