package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BacklotTracker/internal/store"
)

// AI recommendations: BacklotTracker sends a taste profile built from your ratings,
// rewatches and recent plays to Claude and/or Gemini (with your own API
// keys) and keeps every run. Results you've already watched are dropped;
// titles already in Plex are flagged.

type RecRequest struct {
	User         string
	Provider     string // claude | gemini | both
	Kind         string // any | movie | show
	Count        int
	IncludeAdult bool
	Note         string // optional: "something light", "80s sci-fi"...
}

func (e *Engine) AIProviders() []string {
	var out []string
	if e.cfg.AnthropicKey != "" {
		out = append(out, "claude")
	}
	if e.cfg.GeminiKey != "" {
		out = append(out, "gemini")
	}
	return out
}

var aiHTTP = &http.Client{Timeout: 3 * time.Minute}

const recSystem = `You are a film and TV recommendation engine. You receive a viewer's taste profile and reply with JSON only: an array of objects with keys "kind" ("movie" or "show"), "title", "year" (first release year, integer), "imdb_id" (if you know it, else ""), "tmdb_id" (integer or 0), "tvdb_id" (integer or 0, shows only), and "reason" (one sentence tying it to the viewer's taste). Recommend only real, released titles the viewer has not already watched. No prose outside the JSON.`

func fmtRated(rs []store.Rated, withRating bool) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString("- ")
		b.WriteString(r.Title)
		if r.Year > 0 {
			fmt.Fprintf(&b, " (%d)", r.Year)
		}
		fmt.Fprintf(&b, " [%s]", r.Kind)
		if withRating && r.Rating > 0 {
			fmt.Fprintf(&b, " rated %.1f/10", r.Rating)
		}
		if !withRating && r.Plays > 1 {
			fmt.Fprintf(&b, " ×%d", r.Plays)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (e *Engine) buildPrompt(ctx context.Context, req RecRequest) (string, error) {
	loved, err := e.st.RatedTitles(ctx, req.User, true, 40)
	if err != nil {
		return "", err
	}
	archiveRated, err := e.st.ArchiveRated(ctx, true, 40)
	if err != nil {
		return "", err
	}
	disliked, err := e.st.RatedTitles(ctx, req.User, false, 15)
	if err != nil {
		return "", err
	}
	most, err := e.st.MostPlayed(ctx, req.User, req.IncludeAdult, 30)
	if err != nil {
		return "", err
	}
	recent, err := e.st.RecentTitles(ctx, req.User, req.IncludeAdult, 30)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Recommend %d titles", req.Count)
	switch req.Kind {
	case "movie":
		b.WriteString(" (movies only)")
	case "show":
		b.WriteString(" (TV shows only)")
	default:
		b.WriteString(" (a mix of movies and TV shows)")
	}
	b.WriteString(".\n")
	if req.IncludeAdult {
		b.WriteString("Adult titles are allowed if they fit the profile.\n")
	} else {
		b.WriteString("Do not recommend pornographic titles.\n")
	}
	if n := strings.TrimSpace(req.Note); n != "" {
		fmt.Fprintf(&b, "The viewer's request for this batch: %s\n", n)
	}
	if len(loved)+len(archiveRated) > 0 {
		b.WriteString("\nHighest rated by the viewer:\n" + fmtRated(append(loved, archiveRated...), true))
	}
	var low []store.Rated
	for _, r := range disliked {
		if r.Rating > 0 && r.Rating <= 5 {
			low = append(low, r)
		}
	}
	if len(low) > 0 {
		b.WriteString("\nRated low (avoid things like these):\n" + fmtRated(low, true))
	}
	if len(most) > 0 {
		b.WriteString("\nMost played and rewatched:\n" + fmtRated(most, false))
	}
	if len(recent) > 0 {
		b.WriteString("\nWatched recently:\n" + fmtRated(recent, false))
	}
	b.WriteString("\nEverything listed above has already been watched; don't recommend it.\n")
	return b.String(), nil
}

type aiRec struct {
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Year   any    `json:"year"`
	IMDB   string `json:"imdb_id"`
	TMDB   any    `json:"tmdb_id"`
	TVDB   any    `json:"tvdb_id"`
	Reason string `json:"reason"`
}

func anyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

// parseRecs pulls the JSON array out of a model reply (tolerating code fences
// or a wrapping object).
func parseRecs(text string) ([]aiRec, error) {
	s := strings.TrimSpace(text)
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON array in the reply")
	}
	var out []aiRec
	if err := json.Unmarshal([]byte(s[i:j+1]), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Recommend runs one request per provider and saves each run.
func (e *Engine) Recommend(ctx context.Context, req RecRequest, report func(string)) error {
	if req.Count <= 0 || req.Count > 50 {
		req.Count = 20
	}
	providers := []string{req.Provider}
	if req.Provider == "both" || req.Provider == "" {
		providers = e.AIProviders()
	}
	if len(providers) == 0 {
		return fmt.Errorf("set ANTHROPIC_API_KEY and/or GEMINI_API_KEY")
	}
	prompt, err := e.buildPrompt(ctx, req)
	if err != nil {
		return err
	}
	seen, err := e.st.SeenSet(ctx, req.User)
	if err != nil {
		return err
	}
	ask := req
	ask.Count = req.Count + req.Count/2 // ask for extra; some get filtered as already seen
	if ask.Count != req.Count {
		prompt = strings.Replace(prompt, "Recommend "+strconv.Itoa(req.Count), "Recommend "+strconv.Itoa(ask.Count), 1)
	}
	var errs []string
	for _, p := range providers {
		report("asking " + p + "…")
		run := &store.RecRun{User: req.User, Provider: p, Prompt: prompt}
		var text string
		switch p {
		case "claude":
			run.Model = e.cfg.AnthropicModel
			text, err = e.askClaude(ctx, prompt)
		case "gemini":
			run.Model = e.cfg.GeminiModel
			text, err = e.askGemini(ctx, prompt)
		default:
			err = fmt.Errorf("unknown provider %q", p)
		}
		run.Response = text
		if err == nil {
			var recs []aiRec
			if recs, err = parseRecs(text); err == nil {
				run.Recs = e.filterRecs(ctx, recs, seen, req)
			}
		}
		if err != nil {
			run.Error = err.Error()
			errs = append(errs, p+": "+err.Error())
		}
		if serr := e.st.SaveRecRun(ctx, run); serr != nil {
			return serr
		}
		report(fmt.Sprintf("%s: %d recommendations", p, len(run.Recs)))
	}
	if len(errs) == len(providers) {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (e *Engine) filterRecs(ctx context.Context, in []aiRec, seen map[string]bool, req RecRequest) []store.Rec {
	var out []store.Rec
	dup := map[string]bool{}
	for _, r := range in {
		kind := strings.ToLower(strings.TrimSpace(r.Kind))
		if kind == "tv" || kind == "series" || kind == "tv show" {
			kind = "show"
		}
		if kind != "movie" && kind != "show" || strings.TrimSpace(r.Title) == "" {
			continue
		}
		if req.Kind == "movie" && kind != "movie" || req.Kind == "show" && kind != "show" {
			continue
		}
		year := anyInt(r.Year)
		key := kind + "|" + store.NormTitle(r.Title) + "|"
		if dup[key] || seen[key+strconv.Itoa(year)] || (kind == "show" && seen[key]) {
			continue
		}
		dup[key] = true
		inPlex, _ := e.st.InPlex(ctx, kind, r.Title, year)
		rec := store.Rec{Kind: kind, Title: strings.TrimSpace(r.Title), Year: year, Reason: r.Reason,
			IMDB: strings.TrimSpace(r.IMDB), TMDB: anyInt(r.TMDB), TVDB: anyInt(r.TVDB), InPlex: inPlex}
		if !strings.HasPrefix(rec.IMDB, "tt") {
			rec.IMDB = ""
		}
		out = append(out, rec)
		if len(out) >= req.Count {
			break
		}
	}
	return out
}

func postJSON(ctx context.Context, url string, hdr map[string]string, in, out any) error {
	b, _ := json.Marshal(in)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	resp, err := aiHTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, truncate(string(body), 400))
	}
	return json.Unmarshal(body, out)
}

func (e *Engine) askClaude(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	err := postJSON(ctx, e.cfg.AnthropicURL+"/v1/messages",
		map[string]string{"x-api-key": e.cfg.AnthropicKey, "anthropic-version": "2023-06-01"},
		map[string]any{"model": e.cfg.AnthropicModel, "max_tokens": 8000, "system": recSystem,
			"messages": []map[string]any{{"role": "user", "content": prompt}}}, &out)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("empty reply (stop reason %q)", out.StopReason)
	}
	return b.String(), nil
}

func (e *Engine) askGemini(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	err := postJSON(ctx, e.cfg.GeminiURL+"/v1beta/models/"+e.cfg.GeminiModel+":generateContent",
		map[string]string{"x-goog-api-key": e.cfg.GeminiKey},
		map[string]any{
			"systemInstruction": map[string]any{"parts": []map[string]string{{"text": recSystem}}},
			"contents":          []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}},
			"generationConfig":  map[string]any{"responseMimeType": "application/json", "temperature": 0.8},
		}, &out)
	if err != nil {
		return "", err
	}
	if out.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("blocked by Gemini: %s", out.PromptFeedback.BlockReason)
	}
	var b strings.Builder
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			b.WriteString(p.Text)
		}
		if b.Len() > 0 {
			break
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("empty reply from Gemini")
	}
	return b.String(), nil
}
