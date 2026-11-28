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
	ClaudeModel  string   // "" = the setting, which defaults to the newest model
	GeminiModel  string
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
	run := func(p string) string {
		report("asking " + p + "…")
		run := &store.RecRun{User: req.User, Provider: p, Prompt: prompt}
		choice := req.ClaudeModel
		if p == "gemini" {
			choice = req.GeminiModel
		}
		var text string
		var recs []aiRec
		var err error
		for i, model := range e.modelChain(ctx, p, choice) {
			if i > 0 {
				// A second model is worth trying unless the key itself is the problem.
				if isKeyError(err) {
					break
				}
				report(fmt.Sprintf("%s: %s didn't work, trying %s…", p, run.Model, model))
				e.log.Warn("AI model failed, trying another", "provider", p, "from", run.Model, "to", model, "err", friendlyAIError(p, err))
			}
			run.Model = model
			if text, err = e.askProvider(ctx, p, model, prompt); err == nil {
				if recs, err = parseRecs(text); errors.Is(err, errUnreadableReply) {
					report(p + ": reply couldn't be read, asking again…")
					var retry string
					if retry, err = e.askProvider(ctx, p, model, prompt+"\n\nReply with only the JSON, nothing else."); err == nil {
						text = retry
						recs, err = parseRecs(retry)
					}
				}
			}
			run.Response = text
			if err == nil {
				break
			}
		}
		problem := ""
		if err == nil {
			// Most suggestions can turn out to be things the viewer already has.
			// Ask again, naming what was turned down, until the list is full.
			f := recFilter{seen: skip, ignored: ignored, have: have, taken: map[string]bool{}}
			run.Recs = e.filterRecs(ctx, recs, f, req)
			offered := recs
			for round := 0; round < recTopUpRounds && len(run.Recs) < req.Count && len(recs) > 0; round++ {
				report(fmt.Sprintf("%s: %d of %d are new to you, asking for more…", p, len(run.Recs), req.Count))
				var b strings.Builder
				b.WriteString(prompt + "\n\nYou already suggested these. The viewer has seen or owns most of them, so suggest different titles, and look further from the obvious picks:\n")
				for _, r := range offered {
					fmt.Fprintf(&b, "- %s\n", r.Title)
				}
				more, merr := e.askProvider(ctx, p, run.Model, b.String())
				if merr != nil {
					break // keep what we have
				}
				if recs, merr = parseRecs(more); merr != nil {
					break
				}
				offered = append(offered, recs...)
				left := req
				left.Count = req.Count - len(run.Recs)
				run.Recs = append(run.Recs, e.filterRecs(ctx, recs, f, left)...)
			}
		} else {
			run.Error = friendlyAIError(p, err)
			run.ErrorDetail = err.Error()
			if errors.Is(err, errUnreadableReply) {
				run.ErrorDetail += "\n\nStart of the reply:\n" + truncate(run.Response, 600)
			}
			problem = run.Error
		}
		if serr := e.st.SaveRecRun(ctx, run); serr != nil {
			return serr.Error()
		}
		if err == nil {
			report(fmt.Sprintf("%s (%s): %d recommendations", p, run.Model, len(run.Recs)))
		}
		return problem
	}
	var errs []string
	for _, p := range providers {
		if problem := run(p); problem != "" {
			errs = append(errs, problem)
		}
	}
	// Asked for one service and it failed: use the other one if it's set up.
	if len(providers) == 1 && len(errs) == 1 {
		for _, other := range e.AIProviders() {
			if other == providers[0] {
				continue
			}
			report(fmt.Sprintf("%s failed, using %s instead…", providers[0], other))
			if problem := run(other); problem == "" {
				report("done with " + other + ", because " + providers[0] + " had a problem: " + errs[0])
				return nil
			} else {
				errs = append(errs, problem)
			}
		}
		return errors.New(strings.Join(errs, " "))
	}
	if len(errs) == len(providers) {
		return errors.New(strings.Join(errs, " "))
	}
	if len(errs) > 0 {
		report("done, with a problem: " + strings.Join(errs, " "))
	}
	return nil
}

func (e *Engine) askProvider(ctx context.Context, p, model, prompt string) (string, error) {
	switch p {
	case "claude":
		return e.askClaude(ctx, model, prompt)
	case "gemini":
		return e.askGemini(ctx, model, prompt)
	}
	return "", fmt.Errorf("unknown provider %q", p)
}

func isKeyError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, w := range []string{"401", "403", "invalid x-api-key", "api key not valid", "api_key_invalid", "permission"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
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
	case isKeyError(err):
		return name + " rejected the API key. Check it in Settings."
	case isRetiredModel(err):
		return name + "'s model isn't available. Choose another model in Settings."
	case has("429", "quota", "credit", "rate limit"):
		return name + " is rate limiting requests, or the account is out of credit. Try again later."
	case has("529", "503", "overloaded", "unavailable"):
		return name + " is busy right now. Try again in a few minutes."
	case has("deadline", "timeout"):
		return name + " didn't answer in time."
	case has("400"):
		return name + " didn't accept the request for this model. Choose another model."
	case has("blocked", "safety", "refus"):
		return name + " declined this request."
	}
	return name + "'s request failed."
}

func isRetiredModel(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "404") && (strings.Contains(s, "model") || strings.Contains(s, "not_found"))
}

// recTopUpRounds is how many extra times a service is asked when too few of
// its suggestions were new to the viewer.
const recTopUpRounds = 2

type recFilter struct {
	seen, ignored, have map[string]bool
	taken               map[string]bool // already kept in this run, across rounds
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
		if dup[key] || f.taken[rk] || f.seen[key+strconv.Itoa(year)] || (kind == "show" && f.seen[key]) || f.ignored[rk] || f.have[rk] {
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
		if f.taken != nil {
			f.taken[rk] = true
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

// recSchema makes Claude return the list as structured data instead of prose.
// (Forcing a tool call is rejected by current Claude models.)
var recSchema = map[string]any{
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
				"required":             []string{"kind", "title", "year", "imdb_id", "tmdb_id", "tvdb_id", "reason"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"recommendations"},
	"additionalProperties": false,
}

func (e *Engine) askClaude(ctx context.Context, model, prompt string) (string, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	body := map[string]any{"model": model, "max_tokens": 16000, "system": recSystem,
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": recSchema}},
		"messages":      []map[string]any{{"role": "user", "content": prompt}}}
	hdr := map[string]string{"x-api-key": e.cfg.AnthropicKey, "anthropic-version": "2023-06-01"}
	err := postJSON(ctx, e.cfg.AnthropicURL+"/v1/messages", hdr, body, &out)
	if err != nil && strings.HasPrefix(err.Error(), "400") {
		// The model wouldn't take the structured-output request (an older
		// model, or a rule that changed): ask plainly. The prompt already
		// requires JSON, and the reply reader copes with extra text.
		delete(body, "output_config")
		err = postJSON(ctx, e.cfg.AnthropicURL+"/v1/messages", hdr, body, &out)
	}
	if err != nil {
		return "", err
	}
	if out.StopReason == "refusal" {
		return "", errors.New("Claude refused this request")
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
	if out.StopReason == "max_tokens" {
		return "", errors.New("Claude's reply was cut off before it finished")
	}
	return b.String(), nil
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
