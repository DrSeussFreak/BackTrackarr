package core

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
)

// Each person chooses which of their data feeds recommendations. Adult
// sources start unchecked, and nothing adult is recommended unless one is
// chosen or "include adult" is ticked.

// RecSource is one thing recommendations can draw on.
type RecSource struct {
	ID      string
	Label   string
	Group   string
	Adult   bool
	Checked bool
}

const (
	recSrcRatings     = "ratings"
	recSrcPlexRatings = "plex-ratings"
	recSrcPrivate     = "private"
	recSrcPlays       = "plays:"
	recSrcArr         = "arr:"
	recSourcesKey     = "rec_sources|"
	recLastKey        = "rec_last|"
)

var playFamilyNames = map[string]string{
	"plex": "Plex", "trakt": "Trakt", "simkl": "Simkl", "tautulli": "Tautulli", "crosswatch": "CrossWatch",
	"plekt": "Plekt", "floppy": "Floppy", "jellyfin": "Jellyfin", "emby": "Emby", "netflix": "Netflix",
	"disneyplus": "Disney+", "hulu": "Hulu", "primevideo": "Prime Video", "max": "Max", "manual": "Marked by hand",
	"letterboxd": "Letterboxd", "imdb": "IMDb", "import": "Imported files",
}

func playFamilyLabel(f string) string {
	if n := playFamilyNames[f]; n != "" {
		return n
	}
	if f == "" {
		return "Other"
	}
	return strings.ToUpper(f[:1]) + f[1:]
}

// RecSources lists every available source with this person's saved choice.
func (e *Engine) RecSources(ctx context.Context, user string) []RecSource {
	out := []RecSource{
		{ID: recSrcRatings, Label: "Your ratings here (including Trakt and Simkl imports)", Group: "Ratings"},
		{ID: recSrcPlexRatings, Label: "Ratings in Plex", Group: "Ratings"},
	}
	fams, _ := e.st.PlaySourceFamilies(ctx)
	for _, f := range fams {
		out = append(out, RecSource{ID: recSrcPlays + f.Name, Label: playFamilyLabel(f.Name), Group: "Watch history from"})
	}
	if len(e.cfg.PrivateLibraries) > 0 {
		out = append(out, RecSource{ID: recSrcPrivate, Label: "Plays from private libraries", Group: "Watch history from", Adult: true})
	}
	arr := append([]config.ArrInstance(nil), e.cfg.Arr...)
	sort.SliceStable(arr, func(a, b int) bool { return arr[a].Name < arr[b].Name })
	for _, in := range arr {
		out = append(out, RecSource{ID: recSrcArr + in.Name, Label: in.Name + " (" + in.Kind + ")", Group: "Your collections", Adult: in.Adult})
	}
	saved := e.SavedRecSources(ctx, user)
	chosen := map[string]bool{}
	for _, id := range saved {
		chosen[id] = true
	}
	for i := range out {
		if saved == nil {
			out[i].Checked = !out[i].Adult // a new source starts on, unless it's adult
		} else {
			out[i].Checked = chosen[out[i].ID]
		}
	}
	return out
}

// SavedRecSources returns the ids a person chose, or nil if they never chose.
func (e *Engine) SavedRecSources(ctx context.Context, user string) []string {
	v, _ := e.st.GetKV(ctx, recSourcesKey+user)
	if v == "" {
		return nil
	}
	var ids []string
	if json.Unmarshal([]byte(v), &ids) != nil || len(ids) == 0 {
		return nil
	}
	return ids
}

func (e *Engine) SaveRecSources(ctx context.Context, user string, ids []string) error {
	b, _ := json.Marshal(ids)
	return e.st.SetKV(ctx, recSourcesKey+user, string(b))
}

type recSelection struct {
	ratings, plexRatings bool
	families             string // "" = every source, "none", or ",plex,trakt,"
	private              bool
	arr                  []config.ArrInstance
	adultNames           []string
}

func (s recSelection) arrNames() []string {
	out := make([]string, len(s.arr))
	for i, a := range s.arr {
		out[i] = a.Name
	}
	return out
}

func (e *Engine) resolveRecSources(ctx context.Context, req RecRequest) recSelection {
	chosen := map[string]bool{}
	if req.Sources == nil {
		for _, s := range e.RecSources(ctx, req.User) {
			chosen[s.ID] = s.Checked
		}
	} else {
		for _, id := range req.Sources {
			chosen[id] = true
		}
	}
	sel := recSelection{ratings: chosen[recSrcRatings], plexRatings: chosen[recSrcPlexRatings], private: req.IncludeAdult}
	if chosen[recSrcPrivate] {
		sel.private = true
		sel.adultNames = append(sel.adultNames, "private libraries")
	}
	fams, _ := e.st.PlaySourceFamilies(ctx)
	var picked []string
	for _, f := range fams {
		if chosen[recSrcPlays+f.Name] {
			picked = append(picked, f.Name)
		}
	}
	switch {
	case len(picked) == 0:
		sel.families = "none"
	case len(picked) < len(fams):
		sel.families = "," + strings.Join(picked, ",") + ","
	}
	for _, in := range e.cfg.Arr {
		if chosen[recSrcArr+in.Name] {
			sel.arr = append(sel.arr, in)
			if in.Adult {
				sel.adultNames = append(sel.adultNames, in.Name)
			}
		}
	}
	return sel
}

// ---------- keeping the page filled ----------

// saveLastRecRequest remembers a person's choices so the daily refresh asks
// the same way. A one-off note isn't repeated.
func (e *Engine) saveLastRecRequest(ctx context.Context, req RecRequest) {
	req.Note, req.Sources = "", nil
	if b, err := json.Marshal(req); err == nil {
		_ = e.st.SetKV(ctx, recLastKey+req.User, string(b))
	}
}

func (e *Engine) lastRecRequest(ctx context.Context, user string) (RecRequest, bool) {
	v, _ := e.st.GetKV(ctx, recLastKey+user)
	var req RecRequest
	if v == "" || json.Unmarshal([]byte(v), &req) != nil {
		return RecRequest{}, false
	}
	req.User = user
	return req, true
}

// RefreshRecs starts a new run for one person with their last choices.
func (e *Engine) RefreshRecs(user string) bool {
	if len(e.AIProviders()) == 0 {
		return false
	}
	req, ok := e.lastRecRequest(e.ctx, user)
	if !ok {
		return false
	}
	return e.Jobs.Start(e.ctx, "recommendations", func(ctx context.Context, report func(string)) error {
		return e.Recommend(ctx, req, report)
	})
}

// RunRecRefresh keeps recommendations current: once every REC_REFRESH_HOURS
// for each person who has asked for them before.
func (e *Engine) RunRecRefresh(ctx context.Context) {
	every := e.cfg.RecRefresh
	if every <= 0 {
		return
	}
	t := time.NewTicker(20 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if len(e.AIProviders()) == 0 || e.Jobs.AnyRunning() {
			continue
		}
		users, err := e.st.RecUsers(ctx)
		if err != nil {
			continue
		}
		for _, u := range users {
			last, err := e.st.LastRecRunAt(ctx, u)
			if err != nil || time.Since(time.Unix(last, 0)) < every {
				continue
			}
			if e.RefreshRecs(u) {
				break // one person at a time; the next tick takes the next
			}
		}
	}
}

// RecRefreshEvery is how often recommendations refresh (0 = off).
func (e *Engine) RecRefreshEvery() time.Duration { return e.cfg.RecRefresh }
