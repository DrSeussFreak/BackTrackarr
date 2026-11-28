package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// AI recommendations: BackTrackarr sends a taste profile built from your ratings,
// rewatches and recent plays to Claude and/or Gemini (with your own API
// keys) and keeps every run. Results you've already watched, already have or
// chose to ignore are dropped; titles already in Plex are flagged.

type RecRequest struct {
	User         string
	Provider     string // claude | gemini | both
	Kind         string // any | movie | show
	Count        int
	IncludeAdult bool
	Note         string   // optional: "something light", "80s sci-fi"...
	OnlyMine     bool     // keep only titles in Plex or on MY_STREAMING_SERVICES
	Sources      []string // RecSource ids; nil means the saved or default choice
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

func (e *Engine) buildPrompt(ctx context.Context, req RecRequest, sel recSelection) (string, error) {
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
	switch {
	case len(sel.adultNames) > 0:
		fmt.Fprintf(&b, "The viewer chose adult sources (%s). Recommend titles in the same style as what's in those sources; adult titles are allowed.\n",
			strings.Join(sel.adultNames, ", "))
	case req.IncludeAdult:
		b.WriteString("Adult titles are allowed if they fit the profile.\n")
	default:
		b.WriteString("Do not recommend pornographic or adult titles.\n")
	}
	if n := strings.TrimSpace(req.Note); n != "" {
		fmt.Fprintf(&b, "The viewer's request for this batch: %s\n", n)
	}

	var loved, low []store.Rated
	if sel.ratings {
		top, err := e.st.RatedTitles(ctx, req.User, true, 40)
		if err != nil {
			return "", err
		}
		loved = append(loved, top...)
		bottom, err := e.st.RatedTitles(ctx, req.User, false, 15)
		if err != nil {
			return "", err
		}
		for _, r := range bottom {
			if r.Rating > 0 && r.Rating <= 5 {
				low = append(low, r)
			}
		}
	}
	if sel.plexRatings {
		top, err := e.st.ArchiveRated(ctx, true, 40)
		if err != nil {
			return "", err
		}
		loved = append(loved, top...)
	}
	if len(loved) > 0 {
		b.WriteString("\nHighest rated by the viewer:\n" + fmtRated(loved, true))
	}
	if len(low) > 0 {
		b.WriteString("\nRated low (avoid things like these):\n" + fmtRated(low, true))
	}
	if sel.families != "none" {
		most, err := e.st.MostPlayedFrom(ctx, req.User, sel.private, sel.families, 30)
		if err != nil {
			return "", err
		}
		recent, err := e.st.RecentTitlesFrom(ctx, req.User, sel.private, sel.families, 30)
		if err != nil {
			return "", err
		}
		if len(most) > 0 {
			b.WriteString("\nMost played and rewatched:\n" + fmtRated(most, false))
		}
		if len(recent) > 0 {
			b.WriteString("\nWatched recently:\n" + fmtRated(recent, false))
		}
	}
	for _, in := range sel.arr {
		lib, err := e.st.ArrLibrary(ctx, in.Name, 40)
		if err != nil {
			return "", err
		}
		if len(lib) > 0 {
			label := in.Name
			if in.Adult {
				label += ", adult"
			}
			fmt.Fprintf(&b, "\nIn the viewer's %s collection (%s), already owned:\n%s", in.Kind, label, fmtRated(lib, false))
		}
	}
	if ign, err := e.st.IgnoredRecs(ctx, req.User); err == nil && len(ign) > 0 {
		b.WriteString("\nThe viewer said they're not interested in these; don't suggest them or anything too similar:\n")
		for i, r := range ign {
			if i == 60 {
				break
			}
			fmt.Fprintf(&b, "- %s [%s]\n", r.Title, r.Kind)
		}
	}
	b.WriteString("\nEverything listed above has already been watched or is owned; don't recommend it.\n")
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

var errUnreadableReply = errors.New("no JSON array in the reply")

// parseRecs reads a model reply: a JSON array, an object holding one under
// "recommendations", or either one wrapped in code fences or text.
func parseRecs(text string) ([]aiRec, error) {
	s := strings.TrimSpace(text)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```")
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	var list []aiRec
	if json.Unmarshal([]byte(s), &list) == nil {
		return list, nil
	}
	var wrapped struct {
		Recommendations []aiRec `json:"recommendations"`
	}
	if json.Unmarshal([]byte(s), &wrapped) == nil && wrapped.Recommendations != nil {
		return wrapped.Recommendations, nil
	}
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil, errUnreadableReply
	}
	if err := json.Unmarshal([]byte(s[i:j+1]), &list); err != nil {
		return nil, errUnreadableReply
	}
	return list, nil
}

// Recommend runs one request per provider and saves each run. One provider
// failing never hides the other's results.
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
	if req.Sources == nil {
		req.Sources = e.SavedRecSources(ctx, req.User)
	}
	e.saveLastRecRequest(ctx, req)
	sel := e.resolveRecSources(ctx, req)
	ask := req
	ask.Count = req.Count + req.Count/2 // ask for extra; some get filtered as already seen
	prompt, err := e.buildPrompt(ctx, ask, sel)
	if err != nil {
		return err
	}
	skip, err := e.st.SeenSet(ctx, req.User)
	if err != nil {
		return err
	}
	ignored, err := e.st.IgnoredSet(ctx, req.User)
	if err != nil {
		return err
	}
	have, err := e.st.ArrHaveSet(ctx, sel.arrNames())
	if err != nil {
		return err
	}
	var errs []string
	for _, p := range providers {
		report("asking " + p + "…")
		run := &store.RecRun{User: req.User, Provider: p, Prompt: prompt}
		text, model, err := e.askProvider(ctx, p, prompt, report)
		run.Model = model
		run.Response = text
		var recs []aiRec
		if err == nil {
			if recs, err = parseRecs(text); errors.Is(err, errUnreadableReply) {
				report(p + ": reply couldn't be read, asking again…")
				var retry string
				if retry, _, err = e.askProvider(ctx, p, prompt+"\n\nReply with only the JSON array, nothing else.", report); err == nil {
					run.Response = retry
					recs, err = parseRecs(retry)
				}
			}
		}
		if err == nil {
			run.Recs = e.filterRecs(ctx, recs, recFilter{seen: skip, ignored: ignored, have: have}, req)
		} else {
			run.Error = friendlyAIError(p, err)
			run.ErrorDetail = err.Error()
			if errors.Is(err, errUnreadableReply) {
				run.ErrorDetail += "\n\nStart of the reply:\n" + truncate(run.Response, 600)
			}
			errs = append(errs, run.Error)
		}
		if serr := e.st.SaveRecRun(ctx, run); serr != nil {
			return serr
		}
		if err == nil {
			report(fmt.Sprintf("%s: %d recommendations", p, len(run.Recs)))
		}
	}
	if len(errs) == len(providers) {
		return errors.New(strings.Join(errs, " "))
	}
	if len(errs) > 0 {
		report("done, with a problem: " + strings.Join(errs, " "))
	}
	return nil
}

func (e *Engine) askProvider(ctx context.Context, p, prompt string, report func(string)) (text, model string, err error) {
	switch p {
	case "claude":
		text, err = e.askClaude(ctx, prompt)
		return text, e.cfg.AnthropicModel, err
	case "gemini":
		model = e.geminiModel(ctx)
		text, err = e.askGemini(ctx, model, prompt)
		if err != nil && isRetiredModel(err) {
			if next, perr := e.pickGeminiModel(ctx, model); perr == nil && next != model {
				report(fmt.Sprintf("gemini: %s isn't available, switching to %s…", model, next))
				e.log.Warn("gemini model unavailable, switched automatically", "from", model, "to", next)
				_ = e.st.SetKV(ctx, geminiFallbackKey+e.cfg.GeminiModel, next)
				model = next
				text, err = e.askGemini(ctx, model, prompt)
			}
		}
		return text, model, err
	}
	return "", "", fmt.Errorf("unknown provider %q", p)
}

// friendlyAIError turns a provider failure into one plain sentence.
func friendlyAIError(p string, err error) string {
	name := map[string]string{"claude": "Claude", "gemini": "Gemini"}[p]
	if name == "" {
		name = p
	}
	s := strings.ToLower(err.Error())
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case errors.Is(err, errUnreadableReply):
		return name + "'s reply couldn't be read, even after asking again."
	case has("401", "403", "invalid x-api-key", "api key not valid", "permission"):
		return name + " rejected the API key. Check it in Settings."
	case isRetiredModel(err):
		return name + "'s model isn't available. Choose another model in Settings."
	case has("429", "quota", "credit", "rate limit"):
		return name + " is rate limiting requests, or the account is out of credit. Try again later."
	case has("529", "503", "overloaded", "unavailable"):
		return name + " is busy right now. Try again in a few minutes."
	case has("deadline", "timeout"):
		return name + " didn't answer in time."
	case has("blocked", "safety", "refus"):
		return name + " declined this request."
	}
	return name + "'s request failed."
}

func isRetiredModel(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") && (strings.Contains(s, "model") || strings.Contains(s, "not_found"))
}

type recFilter struct {
	seen, ignored, have map[string]bool
}

func (e *Engine) filterRecs(ctx context.Context, in []aiRec, f recFilter, req RecRequest) []store.Rec {
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
		rk := store.RecKey(kind, r.Title)
		if dup[key] || f.seen[key+strconv.Itoa(year)] || (kind == "show" && f.seen[key]) || f.ignored[rk] || f.have[rk] {
			continue
		}
		dup[key] = true
		inPlex, _ := e.st.InPlex(ctx, kind, r.Title, year)
		rec := store.Rec{Kind: kind, Title: strings.TrimSpace(r.Title), Year: year, Reason: r.Reason,
			IMDB: strings.TrimSpace(r.IMDB), TMDB: anyInt(r.TMDB), TVDB: anyInt(r.TVDB), InPlex: inPlex}
		if !strings.HasPrefix(rec.IMDB, "tt") {
			rec.IMDB = ""
		}
		if e.cfg.TMDBKey != "" {
			id := rec.TMDB
			if id == 0 {
				if h, err := e.lookup(ctx, kind, rec.Title, rec.Year); err == nil && h != nil {
					id = h.IDs.TMDB
					rec.TMDB = id
					if rec.IMDB == "" {
						rec.IMDB = h.IDs.IMDB
					}
				}
			}
			rec.Providers, _ = e.WatchProviders(ctx, kind, id)
		}
		if req.OnlyMine && len(e.cfg.MyServices) > 0 && !rec.InPlex {
			mine := false
			for _, p := range rec.Providers {
				mine = mine || e.IsMyService(p)
			}
			if !mine {
				continue
			}
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

// recTool makes Claude return the list as structured data instead of prose.
var recTool = map[string]any{
	"name":        "save_recommendations",
	"description": "Save the recommendations for the viewer.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"recommendations": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":    map[string]any{"type": "string", "enum": []string{"movie", "show"}},
						"title":   map[string]any{"type": "string"},
						"year":    map[string]any{"type": "integer"},
						"imdb_id": map[string]any{"type": "string"},
						"tmdb_id": map[string]any{"type": "integer"},
						"tvdb_id": map[string]any{"type": "integer"},
						"reason":  map[string]any{"type": "string"},
					},
					"required": []string{"kind", "title", "year", "reason"},
				},
			},
		},
		"required": []string{"recommendations"},
	},
}

func (e *Engine) askClaude(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	err := postJSON(ctx, e.cfg.AnthropicURL+"/v1/messages",
		map[string]string{"x-api-key": e.cfg.AnthropicKey, "anthropic-version": "2023-06-01"},
		map[string]any{"model": e.cfg.AnthropicModel, "max_tokens": 8000, "system": recSystem,
			"tools": []any{recTool}, "tool_choice": map[string]string{"type": "tool", "name": "save_recommendations"},
			"messages": []map[string]any{{"role": "user", "content": prompt}}}, &out)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range out.Content {
		switch c.Type {
		case "tool_use":
			if len(c.Input) > 0 {
				return string(c.Input), nil // parseRecs reads the "recommendations" field
			}
		case "text":
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("empty reply (stop reason %q)", out.StopReason)
	}
	return b.String(), nil
}

const geminiFallbackKey = "gemini_model_fallback|"

// geminiModel is the configured model, or the replacement picked
// automatically after Google retired it.
func (e *Engine) geminiModel(ctx context.Context) string {
	if v, _ := e.st.GetKV(ctx, geminiFallbackKey+e.cfg.GeminiModel); v != "" {
		return v
	}
	return e.cfg.GeminiModel
}

// pickGeminiModel asks Google which models this key can use and picks the
// newest Pro text model, preferring a stable release over a preview of the
// same version.
func (e *Engine) pickGeminiModel(ctx context.Context, current string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.GeminiURL+"/v1beta/models?pageSize=1000", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-goog-api-key", e.cfg.GeminiKey)
	resp, err := aiHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("listing models: %s", resp.Status)
	}
	var list struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return "", err
	}
	best, bestScore := "", -1.0
	for _, m := range list.Models {
		name := strings.TrimPrefix(m.Name, "models/")
		if name == current || !strings.HasPrefix(name, "gemini-") || !strings.Contains(name, "-pro") {
			continue
		}
		generates := false
		for _, x := range m.Methods {
			generates = generates || x == "generateContent"
		}
		skip := !generates
		for _, w := range []string{"tts", "image", "vision", "embedding", "audio", "live", "exp"} {
			skip = skip || strings.Contains(name, w)
		}
		if skip {
			continue
		}
		v, _ := strconv.ParseFloat(strings.SplitN(strings.TrimPrefix(name, "gemini-"), "-", 2)[0], 64)
		score := v * 100 // the newest version wins
		if !strings.Contains(name, "preview") {
			score++ // and a stable release beats a preview of the same version
		}
		if score > bestScore {
			best, bestScore = name, score
		}
	}
	if best == "" {
		return "", errors.New("no Gemini Pro model is available to this key")
	}
	return best, nil
}

func (e *Engine) askGemini(ctx context.Context, model, prompt string) (string, error) {
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
	err := postJSON(ctx, e.cfg.GeminiURL+"/v1beta/models/"+model+":generateContent",
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
