package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Which AI model is used: the newest one each service offers this key, unless
// a specific model is chosen. The lists come from the services themselves, so
// a new model is picked up without an update.

// ModelAuto means "use the newest model".
const ModelAuto = "auto"

// Used only until the first model list arrives, or if it can't be fetched.
const (
	claudeModelFallback = "claude-opus-5-5"
	geminiModelFallback = "gemini-3.1-pro-preview"
	aiModelsKey         = "ai_models|"
	aiModelChoiceKey    = "ai_model_choice|"
	aiModelsMaxAge      = 12 * time.Hour
)

// AIModel is one model a key can use.
type AIModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type aiModelCache struct {
	At     int64     `json:"at"`
	Models []AIModel `json:"models"`
}

func (e *Engine) cachedAIModels(ctx context.Context, provider string) (aiModelCache, bool) {
	var c aiModelCache
	v, _ := e.st.GetKV(ctx, aiModelsKey+provider)
	if v == "" || json.Unmarshal([]byte(v), &c) != nil {
		return c, false
	}
	return c, true
}

// AIModels lists a service's models, best choice first, from the saved list.
// A stale or missing list is refreshed in the background, so a page never
// waits on the service.
func (e *Engine) AIModels(ctx context.Context, provider string) []AIModel {
	c, ok := e.cachedAIModels(ctx, provider)
	if !ok || time.Since(time.Unix(c.At, 0)) > aiModelsMaxAge {
		if e.aiRefreshing.CompareAndSwap(false, true) {
			go func() {
				defer e.aiRefreshing.Store(false)
				rctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
				defer cancel()
				for _, p := range e.AIProviders() {
					_, _ = e.refreshAIModels(rctx, p)
				}
			}()
		}
	}
	return c.Models
}

// freshAIModels is AIModels for a request that is about to call the service
// anyway: it fetches the list when there isn't a recent one.
func (e *Engine) freshAIModels(ctx context.Context, provider string) []AIModel {
	c, ok := e.cachedAIModels(ctx, provider)
	if ok && time.Since(time.Unix(c.At, 0)) <= aiModelsMaxAge && len(c.Models) > 0 {
		return c.Models
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if fresh, err := e.refreshAIModels(cctx, provider); err == nil {
		return fresh
	}
	return c.Models
}

func (e *Engine) refreshAIModels(ctx context.Context, provider string) ([]AIModel, error) {
	var list []AIModel
	var err error
	switch provider {
	case "claude":
		list, err = e.fetchClaudeModels(ctx)
	case "gemini":
		list, err = e.fetchGeminiModels(ctx)
	}
	if err != nil || len(list) == 0 {
		return nil, err
	}
	b, _ := json.Marshal(aiModelCache{At: time.Now().Unix(), Models: list})
	_ = e.st.SetKV(ctx, aiModelsKey+provider, string(b))
	return list, nil
}

func aiGetJSON(ctx context.Context, url string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := aiHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &aiStatusError{resp.Status}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

type aiStatusError struct{ status string }

func (e *aiStatusError) Error() string { return "listing models: " + e.status }

// fetchClaudeModels returns Claude models newest first. Small "haiku" models
// go last, and restricted "mythos" models are left out, so the automatic
// choice is the newest full-size model.
func (e *Engine) fetchClaudeModels(ctx context.Context) ([]AIModel, error) {
	var res struct {
		Data []struct {
			ID        string `json:"id"`
			Name      string `json:"display_name"`
			CreatedAt string `json:"created_at"`
		} `json:"data"`
	}
	err := aiGetJSON(ctx, e.cfg.AnthropicURL+"/v1/models?limit=1000",
		map[string]string{"x-api-key": e.cfg.AnthropicKey, "anthropic-version": "2023-06-01"}, &res)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(res.Data, func(a, b int) bool { return res.Data[a].CreatedAt > res.Data[b].CreatedAt })
	var main, small []AIModel
	for _, m := range res.Data {
		id := strings.ToLower(m.ID)
		if m.ID == "" || strings.Contains(id, "mythos") {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		if strings.Contains(id, "haiku") {
			small = append(small, AIModel{m.ID, name})
		} else {
			main = append(main, AIModel{m.ID, name})
		}
	}
	return append(main, small...), nil
}

// fetchGeminiModels returns Gemini text models: Pro models newest first, then
// Flash models, which have their own (often larger) free allowance.
func (e *Engine) fetchGeminiModels(ctx context.Context) ([]AIModel, error) {
	var res struct {
		Models []struct {
			Name    string   `json:"name"`
			Display string   `json:"displayName"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	err := aiGetJSON(ctx, e.cfg.GeminiURL+"/v1beta/models?pageSize=1000", map[string]string{"x-goog-api-key": e.cfg.GeminiKey}, &res)
	if err != nil {
		return nil, err
	}
	type scored struct {
		m     AIModel
		score float64
	}
	var pro, flash []scored
	for _, m := range res.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		ok := false
		for _, x := range m.Methods {
			ok = ok || x == "generateContent"
		}
		for _, w := range []string{"tts", "image", "vision", "embedding", "audio", "live", "exp", "lite", "computer", "robotics"} {
			ok = ok && !strings.Contains(id, w)
		}
		if !ok {
			continue
		}
		v, _ := strconv.ParseFloat(strings.SplitN(strings.TrimPrefix(id, "gemini-"), "-", 2)[0], 64)
		score := v * 100 // the newest version first
		if !strings.Contains(id, "preview") {
			score++ // a stable release beats a preview of the same version
		}
		name := m.Display
		if name == "" {
			name = id
		}
		switch {
		case strings.Contains(id, "-pro"):
			pro = append(pro, scored{AIModel{id, name}, score})
		case strings.Contains(id, "-flash"):
			flash = append(flash, scored{AIModel{id, name}, score})
		}
	}
	var out []AIModel
	for _, group := range [][]scored{pro, flash} {
		sort.SliceStable(group, func(a, b int) bool { return group[a].score > group[b].score })
		for _, s := range group {
			out = append(out, s.m)
		}
	}
	return out, nil
}

// AIModelChoice is the model picked on the Recommendations page ("" = the
// setting, which defaults to the newest).
func (e *Engine) AIModelChoice(ctx context.Context, provider string) string {
	v, _ := e.st.GetKV(ctx, aiModelChoiceKey+provider)
	return v
}

func (e *Engine) SetAIModelChoice(ctx context.Context, provider, model string) error {
	return e.st.SetKV(ctx, aiModelChoiceKey+provider, strings.TrimSpace(model))
}

func (e *Engine) configuredModel(provider string) (setting, fallback string) {
	if provider == "gemini" {
		return e.cfg.GeminiModel, geminiModelFallback
	}
	return e.cfg.AnthropicModel, claudeModelFallback
}

// pickModel returns the model to use and whether it was chosen automatically.
func pickModel(choice, setting, fallback string, list []AIModel) (string, bool) {
	for _, v := range []string{choice, setting} {
		if v = strings.TrimSpace(v); v != "" && v != ModelAuto {
			return v, false
		}
	}
	if len(list) > 0 {
		return list[0].ID, true
	}
	return fallback, true
}

// AIModelInUse reports which model a service would use right now, from the
// saved list, and whether that was picked automatically as the newest.
func (e *Engine) AIModelInUse(ctx context.Context, provider string) (string, bool) {
	setting, fallback := e.configuredModel(provider)
	return pickModel(e.AIModelChoice(ctx, provider), setting, fallback, e.AIModels(ctx, provider))
}

// modelChain is the model to ask, followed by one to fall back to.
func (e *Engine) modelChain(ctx context.Context, provider, choice string) []string {
	list := e.freshAIModels(ctx, provider)
	setting, fallback := e.configuredModel(provider)
	first, _ := pickModel(choice, setting, fallback, list)
	chain := []string{first}
	for _, m := range list {
		if m.ID != first {
			return append(chain, m.ID)
		}
	}
	return chain
}
