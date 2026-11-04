package sinks

import (
	"context"
	"errors"
	"math"
	"net/url"
	"strings"

	"github.com/DrSeussFreak/BacklotTracker/internal/model"
	"github.com/DrSeussFreak/BacklotTracker/internal/store"
)

const simklAPI = "https://api.simkl.com"

// Simkl implements live scrobbling, history sync and ratings against the
// Simkl API (https://api.simkl.org). Create an app at https://simkl.com/settings/developer/.
type Simkl struct {
	ClientID string
	st       *store.Store
}

func (s *Simkl) Name() string     { return "simkl" }
func (s *Simkl) Label() string    { return "Simkl" }
func (s *Simkl) Kind() string     { return "simkl" }
func (s *Simkl) Configured() bool { return s.ClientID != "" }

func (s *Simkl) Ready(ctx context.Context) bool {
	if !s.Configured() {
		return false
	}
	tok, err := s.st.GetToken(ctx, s.Name())
	return err == nil && tok != nil
}

func (s *Simkl) url(p string) string {
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	return simklAPI + p + sep + "client_id=" + url.QueryEscape(s.ClientID) +
		"&app-name=backlottracker&app-version=" + url.QueryEscape(model.Version)
}

func (s *Simkl) StartAuth(ctx context.Context) (*DeviceCode, error) {
	var dc DeviceCode
	if err := doJSON(ctx, "GET", s.url("/oauth/pin"), nil, nil, &dc); err != nil {
		return nil, err
	}
	if dc.VerificationURL == "" {
		dc.VerificationURL = "https://simkl.com/pin/"
	}
	if dc.ExpiresIn == 0 {
		dc.ExpiresIn = 900
	}
	return &dc, nil
}

func (s *Simkl) PollAuth(ctx context.Context, dc *DeviceCode) (bool, error) {
	var out struct {
		Result      string `json:"result"`
		AccessToken string `json:"access_token"`
		Message     string `json:"message"`
	}
	if err := doJSON(ctx, "GET", s.url("/oauth/pin/"+url.PathEscape(dc.UserCode)), nil, nil, &out); err != nil {
		if IsPermanent(err) {
			return false, err
		}
		return false, nil
	}
	if out.Result == "OK" && out.AccessToken != "" {
		return true, s.st.SetToken(ctx, s.Name(), store.Token{Access: out.AccessToken})
	}
	return false, nil
}

func (s *Simkl) Disconnect(ctx context.Context) error { return s.st.DeleteToken(ctx, s.Name()) }

func (s *Simkl) headers(ctx context.Context) (map[string]string, error) {
	tok, err := s.st.GetToken(ctx, s.Name())
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, errors.New("simkl is not connected")
	}
	return map[string]string{"Authorization": "Bearer " + tok.Access, "simkl-api-key": s.ClientID}, nil
}

func simklIDs(i model.IDs) M {
	m := M{}
	if i.IMDB != "" {
		m["imdb"] = i.IMDB
	}
	if i.TMDB != 0 {
		m["tmdb"] = i.TMDB
	}
	if i.TVDB != 0 {
		m["tvdb"] = i.TVDB
	}
	return m
}

func simklItem(m model.Media) M {
	if m.Kind == model.Movie {
		return M{"movie": M{"title": m.Title, "year": m.Year, "ids": simklIDs(m.IDs)}}
	}
	return M{"show": M{"title": m.ShowTitle, "ids": simklIDs(m.ShowIDs)},
		"episode": M{"season": m.Season, "number": m.Episode}}
}

func (s *Simkl) scrobble(ctx context.Context, h map[string]string, action string, m model.Media, progress float64) error {
	body := simklItem(m)
	body["progress"] = progress
	err := doJSON(ctx, "POST", s.url("/scrobble/"+action), h, body, nil)
	if StatusOf(err) == 409 { // session already finalized
		return nil
	}
	return err
}

func (s *Simkl) Send(ctx context.Context, ev model.Event) error {
	h, err := s.headers(ctx)
	if err != nil {
		return err
	}
	switch ev.Action {
	case model.ActStart, model.ActPause, model.ActStop:
		return s.scrobble(ctx, h, ev.Action, ev.Media, clamp(ev.Progress))
	case model.ActWatched:
		if fresh(ev) {
			return s.scrobble(ctx, h, "stop", ev.Media, watchedProgress(ev.Progress))
		}
		return s.SendHistory(ctx, []model.Event{ev})
	case model.ActRate:
		r := int(math.Round(ev.Rating))
		if r < 1 || ev.Media.Kind != model.Movie { // Simkl rates movies and shows, not episodes
			return nil
		}
		if r > 10 {
			r = 10
		}
		return doJSON(ctx, "POST", s.url("/sync/ratings"), h,
			M{"movies": []M{{"title": ev.Media.Title, "year": ev.Media.Year, "ids": simklIDs(ev.Media.IDs), "rating": r}}}, nil)
	}
	return nil
}

func (s *Simkl) SendHistory(ctx context.Context, evs []model.Event) error {
	h, err := s.headers(ctx)
	if err != nil {
		return err
	}
	movies, shows := []M{}, []M{}
	for _, ev := range evs {
		m, at := ev.Media, rfc3339(ev.At)
		switch {
		case m.Kind == model.Movie && (!m.IDs.Empty() || m.Title != ""):
			movies = append(movies, M{"title": m.Title, "year": m.Year, "ids": simklIDs(m.IDs), "watched_at": at})
		case m.Kind == model.Episode && (!m.ShowIDs.Empty() || m.ShowTitle != ""):
			shows = append(shows, M{"title": m.ShowTitle, "ids": simklIDs(m.ShowIDs),
				"seasons": []M{{"number": m.Season, "episodes": []M{{"number": m.Episode, "watched_at": at}}}}})
		}
	}
	if len(movies)+len(shows) == 0 {
		return nil
	}
	return doJSON(ctx, "POST", s.url("/sync/history"), h, M{"movies": movies, "shows": shows}, nil)
}
