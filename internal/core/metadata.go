package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Streaming services only give titles ("Stranger Things: Season 1: Chapter
// One"), so plays from them are matched to ids here: TMDB first
// (TMDB_API_KEY), then Trakt's public search (TRAKT_CLIENT_ID). Results,
// including misses, are cached in the kv table.

// StreamItem is a play as a streaming service describes it.
type StreamItem struct {
	Kind         string // movie | episode | "" (unknown)
	Title        string // movie title, or the raw title when unparsed
	Year         int
	Show         string
	Season       int
	Episode      int
	EpisodeTitle string
	IDs          model.IDs
	ShowIDs      model.IDs
}

type metaHit struct {
	Title string    `json:"title"`
	Year  int       `json:"year"`
	IDs   model.IDs `json:"ids"`
	Miss  bool      `json:"miss,omitempty"`
}

var tmdbHTTP = &http.Client{Timeout: 20 * time.Second}

func (e *Engine) tmdb(ctx context.Context, p string, q url.Values, out any) error {
	if e.cfg.TMDBKey == "" {
		return fmt.Errorf("TMDB_API_KEY is not set")
	}
	if q == nil {
		q = url.Values{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	if strings.HasPrefix(e.cfg.TMDBKey, "eyJ") { // v4 read access token
		req.Header.Set("Authorization", "Bearer "+e.cfg.TMDBKey)
	} else {
		q.Set("api_key", e.cfg.TMDBKey)
	}
	req.URL, err = url.Parse(e.cfg.TMDBURL + "/3" + p + "?" + q.Encode())
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := tmdbHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb %s: %s", p, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (e *Engine) trakt() *sinks.Trakt {
	t, _ := e.Sink("trakt").(*sinks.Trakt)
	if t == nil && e.cfg.TraktClientID != "" {
		t = &sinks.Trakt{ClientID: e.cfg.TraktClientID}
	}
	return t
}

// CanResolve reports whether titles can be turned into ids.
func (e *Engine) CanResolve() bool { return e.cfg.TMDBKey != "" || e.cfg.TraktClientID != "" }

func (e *Engine) cached(ctx context.Context, key string, fetch func() (*metaHit, error)) (*metaHit, error) {
	if v, _ := e.st.GetKV(ctx, key); v != "" {
		var h metaHit
		if json.Unmarshal([]byte(v), &h) == nil {
			if h.Miss {
				return nil, nil
			}
			return &h, nil
		}
	}
	h, err := fetch()
	if err != nil {
		return nil, err // don't cache errors (network, rate limits)
	}
	if h == nil {
		h = &metaHit{Miss: true}
	}
	b, _ := json.Marshal(h)
	_ = e.st.SetKV(ctx, key, string(b))
	if h.Miss {
		return nil, nil
	}
	return h, nil
}

func pickYear[T any](items []T, year func(T) int, want int) (T, bool) {
	var zero T
	if len(items) == 0 {
		return zero, false
	}
	if want > 0 {
		for _, it := range items {
			if y := year(it); y != 0 && absInt(y-want) <= 1 {
				return it, true
			}
		}
	}
	return items[0], true
}

func yearOf(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

// lookup finds a movie or show ("movie" | "show") by title.
func (e *Engine) lookup(ctx context.Context, kind, title string, year int) (*metaHit, error) {
	key := fmt.Sprintf("meta|%s|%s|%d", kind, store.NormTitle(title), year)
	return e.cached(ctx, key, func() (*metaHit, error) {
		if e.cfg.TMDBKey != "" {
			return e.tmdbLookup(ctx, kind, title, year)
		}
		if t := e.trakt(); t != nil {
			hits, err := t.Search(ctx, kind, title, year)
			if err != nil {
				return nil, err
			}
			if h, ok := pickYear(hits, func(h sinks.TraktHit) int { return h.Year }, year); ok {
				return &metaHit{Title: h.Title, Year: h.Year, IDs: h.IDs}, nil
			}
			return nil, nil
		}
		return nil, fmt.Errorf("set TMDB_API_KEY or TRAKT_CLIENT_ID to match streaming titles")
	})
}

func (e *Engine) tmdbLookup(ctx context.Context, kind, title string, year int) (*metaHit, error) {
	type result struct {
		ID           int    `json:"id"`
		Title        string `json:"title"`
		Name         string `json:"name"`
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
	}
	var res struct {
		Results []result `json:"results"`
	}
	q := url.Values{"query": {title}}
	p := "/search/movie"
	if kind == "show" {
		p = "/search/tv"
	}
	if err := e.tmdb(ctx, p, q, &res); err != nil {
		return nil, err
	}
	r, ok := pickYear(res.Results, func(r result) int { return yearOf(r.ReleaseDate + r.FirstAirDate) }, year)
	if !ok {
		return nil, nil
	}
	h := &metaHit{Title: r.Title + r.Name, Year: yearOf(r.ReleaseDate + r.FirstAirDate), IDs: model.IDs{TMDB: r.ID}}
	var ext struct {
		IMDB string `json:"imdb_id"`
		TVDB int    `json:"tvdb_id"`
	}
	ep := "/movie/" + strconv.Itoa(r.ID) + "/external_ids"
	if kind == "show" {
		ep = "/tv/" + strconv.Itoa(r.ID) + "/external_ids"
	}
	if err := e.tmdb(ctx, ep, nil, &ext); err == nil {
		h.IDs.IMDB, h.IDs.TVDB = ext.IMDB, ext.TVDB
	}
	return h, nil
}

// episodeNumber finds an episode's number from its title within a season.
func (e *Engine) episodeNumber(ctx context.Context, show model.IDs, season int, title string) (int, error) {
	want := store.NormTitle(title)
	if want == "" || season <= 0 {
		return 0, nil
	}
	key := fmt.Sprintf("meta-ep|%d|%d|%d|%s", show.TMDB, show.Trakt, season, want)
	h, err := e.cached(ctx, key, func() (*metaHit, error) {
		type ep struct {
			n     int
			title string
		}
		var eps []ep
		switch {
		case e.cfg.TMDBKey != "" && show.TMDB != 0:
			var res struct {
				Episodes []struct {
					Number int    `json:"episode_number"`
					Name   string `json:"name"`
				} `json:"episodes"`
			}
			if err := e.tmdb(ctx, fmt.Sprintf("/tv/%d/season/%d", show.TMDB, season), nil, &res); err != nil {
				return nil, err
			}
			for _, x := range res.Episodes {
				eps = append(eps, ep{x.Number, x.Name})
			}
		case e.trakt() != nil && show.Trakt != 0:
			list, err := e.trakt().SeasonEpisodes(ctx, strconv.Itoa(show.Trakt), season)
			if err != nil {
				return nil, err
			}
			for _, x := range list {
				eps = append(eps, ep{x.Number, x.Title})
			}
		default:
			return nil, nil
		}
		for _, x := range eps { // exact match first
			if store.NormTitle(x.title) == want {
				return &metaHit{Year: x.n}, nil
			}
		}
		for _, x := range eps { // then "Chapter One" vs "Chapter One: The Vanishing of Will Byers"
			n := store.NormTitle(x.title)
			if n != "" && (strings.HasPrefix(n, want) || strings.HasPrefix(want, n)) {
				return &metaHit{Year: x.n}, nil
			}
		}
		return nil, nil
	})
	if err != nil || h == nil {
		return 0, err
	}
	return h.Year, nil // the episode number rides in Year
}

// ResolveStream turns a streaming play into a stored Media. Without a
// metadata source it still stores the play by title, so nothing is lost and
// ids can be filled in later by re-importing.
func (e *Engine) ResolveStream(ctx context.Context, it StreamItem) (model.Media, error) {
	if it.Kind == "" && it.Show == "" {
		it = ParseStreamTitle(it.Title, it)
	}
	var m model.Media
	if it.Show != "" || it.Kind == model.Episode {
		m = model.Media{Kind: model.Episode, ShowTitle: it.Show, Season: it.Season, Episode: it.Episode,
			Title: it.EpisodeTitle, IDs: it.IDs, ShowIDs: it.ShowIDs}
		if m.Title == "" {
			m.Title = fmt.Sprintf("Episode %d", it.Episode)
			if it.Episode == 0 {
				m.Title = it.Title
			}
		}
		if m.ShowIDs.Empty() && e.CanResolve() {
			if h, err := e.lookup(ctx, "show", it.Show, it.Year); err != nil {
				e.log.Debug("show lookup", "title", it.Show, "err", err)
			} else if h != nil {
				m.ShowIDs = h.IDs
				m.ShowTitle = h.Title
			}
		}
		if m.Episode == 0 && m.Season > 0 && it.EpisodeTitle != "" && !m.ShowIDs.Empty() {
			if n, err := e.episodeNumber(ctx, m.ShowIDs, m.Season, it.EpisodeTitle); err == nil && n > 0 {
				m.Episode = n
			}
		}
	} else {
		m = model.Media{Kind: model.Movie, Title: it.Title, Year: it.Year, IDs: it.IDs}
		if m.IDs.Empty() && e.CanResolve() {
			if h, err := e.lookup(ctx, "movie", it.Title, it.Year); err != nil {
				e.log.Debug("movie lookup", "title", it.Title, "err", err)
			} else if h != nil {
				m.IDs, m.Title = h.IDs, h.Title
				if m.Year == 0 {
					m.Year = h.Year
				}
			}
		}
	}
	if strings.TrimSpace(m.Title) == "" {
		return m, fmt.Errorf("no title")
	}
	err := e.st.UpsertMedia(ctx, &m)
	return m, err
}

// ---------- where to watch ----------

// WatchProviders returns the streaming services (subscription, free or with
// ads) carrying a movie or show in STREAMING_REGION, via TMDB.
func (e *Engine) WatchProviders(ctx context.Context, kind string, tmdbID int) ([]string, error) {
	if e.cfg.TMDBKey == "" || tmdbID == 0 {
		return nil, nil
	}
	typ := "movie"
	if kind == "show" {
		typ = "tv"
	}
	key := fmt.Sprintf("providers|%s|%d|%s|%s", typ, tmdbID, e.cfg.StreamingRegion, time.Now().Format("2006-01-02"))
	if v, _ := e.st.GetKV(ctx, key); v != "" {
		var out []string
		_ = json.Unmarshal([]byte(v), &out)
		return out, nil
	}
	var res struct {
		Results map[string]struct {
			Flatrate []struct {
				Name string `json:"provider_name"`
			} `json:"flatrate"`
			Free []struct {
				Name string `json:"provider_name"`
			} `json:"free"`
			Ads []struct {
				Name string `json:"provider_name"`
			} `json:"ads"`
		} `json:"results"`
	}
	if err := e.tmdb(ctx, fmt.Sprintf("/%s/%d/watch/providers", typ, tmdbID), nil, &res); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	r := res.Results[e.cfg.StreamingRegion]
	for _, group := range [][]struct {
		Name string `json:"provider_name"`
	}{r.Flatrate, r.Free, r.Ads} {
		for _, p := range group {
			if !seen[p.Name] {
				seen[p.Name] = true
				out = append(out, p.Name)
			}
		}
	}
	b, _ := json.Marshal(out)
	_ = e.st.SetKV(ctx, key, string(b))
	return out, nil
}

// IsMyService reports whether a provider name matches MY_STREAMING_SERVICES
// (loosely: "Netflix" matches "Netflix basic with Ads").
func (e *Engine) IsMyService(name string) bool {
	n := store.NormTitle(name)
	for _, s := range e.cfg.MyServices {
		if m := store.NormTitle(s); m != "" && strings.Contains(n, m) {
			return true
		}
	}
	return false
}
