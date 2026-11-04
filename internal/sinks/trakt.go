package sinks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/DrSeussFreak/BacklotTracker/internal/model"
	"github.com/DrSeussFreak/BacklotTracker/internal/store"
)

const traktAPI = "https://api.trakt.tv"

// Trakt implements live scrobbling, history sync, ratings and history import
// against the Trakt v2 API. Create an app at https://trakt.tv/oauth/applications
// with redirect URI urn:ietf:wg:oauth:2.0:oob.
type Trakt struct {
	ClientID string
	Secret   string
	st       *store.Store
	mu       sync.Mutex
}

func (t *Trakt) Name() string     { return "trakt" }
func (t *Trakt) Label() string    { return "Trakt" }
func (t *Trakt) Kind() string     { return "trakt" }
func (t *Trakt) Configured() bool { return t.ClientID != "" && t.Secret != "" }

func (t *Trakt) Ready(ctx context.Context) bool {
	if !t.Configured() {
		return false
	}
	tok, err := t.st.GetToken(ctx, t.Name())
	return err == nil && tok != nil
}

type traktToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

func (t *Trakt) save(ctx context.Context, tk traktToken) error {
	created := time.Unix(tk.CreatedAt, 0)
	if tk.CreatedAt == 0 {
		created = time.Now()
	}
	return t.st.SetToken(ctx, t.Name(), store.Token{
		Access: tk.AccessToken, Refresh: tk.RefreshToken,
		ExpiresAt: created.Add(time.Duration(tk.ExpiresIn) * time.Second),
	})
}

func (t *Trakt) StartAuth(ctx context.Context) (*DeviceCode, error) {
	var dc DeviceCode
	err := doJSON(ctx, "POST", traktAPI+"/oauth/device/code", nil, M{"client_id": t.ClientID}, &dc)
	return &dc, err
}

func (t *Trakt) PollAuth(ctx context.Context, dc *DeviceCode) (bool, error) {
	var tk traktToken
	err := doJSON(ctx, "POST", traktAPI+"/oauth/device/token", nil,
		M{"code": dc.DeviceCode, "client_id": t.ClientID, "client_secret": t.Secret}, &tk)
	switch StatusOf(err) {
	case 0:
		if err != nil {
			return false, nil // network blip, keep polling
		}
		return true, t.save(ctx, tk)
	case 400, 429: // pending / slow down
		return false, nil
	case 404:
		return false, errors.New("invalid device code")
	case 409:
		return false, errors.New("code already used")
	case 410:
		return false, errors.New("code expired")
	case 418:
		return false, errors.New("you denied the request")
	default:
		return false, err
	}
}

func (t *Trakt) Disconnect(ctx context.Context) error {
	if tok, _ := t.st.GetToken(ctx, t.Name()); tok != nil {
		_ = doJSON(ctx, "POST", traktAPI+"/oauth/revoke", nil,
			M{"token": tok.Access, "client_id": t.ClientID, "client_secret": t.Secret}, nil)
	}
	return t.st.DeleteToken(ctx, t.Name())
}

func (t *Trakt) headers(ctx context.Context) (map[string]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok, err := t.st.GetToken(ctx, t.Name())
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, errors.New("trakt is not connected")
	}
	if tok.Refresh != "" && !tok.ExpiresAt.IsZero() && time.Until(tok.ExpiresAt) < 48*time.Hour {
		var tk traktToken
		err := doJSON(ctx, "POST", traktAPI+"/oauth/token", nil, M{
			"refresh_token": tok.Refresh, "client_id": t.ClientID, "client_secret": t.Secret,
			"redirect_uri": "urn:ietf:wg:oauth:2.0:oob", "grant_type": "refresh_token",
		}, &tk)
		if err == nil {
			if err := t.save(ctx, tk); err != nil {
				return nil, err
			}
			tok.Access = tk.AccessToken
		} else if time.Now().After(tok.ExpiresAt) {
			return nil, fmt.Errorf("trakt token expired and refresh failed: %w", err)
		}
	}
	return map[string]string{
		"trakt-api-version": "2",
		"trakt-api-key":     t.ClientID,
		"Authorization":     "Bearer " + tok.Access,
	}, nil
}

func traktItem(m model.Media) M {
	if m.Kind == model.Movie {
		return M{"movie": M{"title": m.Title, "year": m.Year, "ids": m.IDs}}
	}
	ep := M{"season": m.Season, "number": m.Episode}
	switch {
	case !m.ShowIDs.Empty():
		return M{"show": M{"title": m.ShowTitle, "ids": m.ShowIDs}, "episode": ep}
	case !m.IDs.Empty():
		return M{"episode": M{"ids": m.IDs}}
	default:
		return M{"show": M{"title": m.ShowTitle}, "episode": ep}
	}
}

func (t *Trakt) scrobble(ctx context.Context, h map[string]string, action string, m model.Media, progress float64) error {
	body := traktItem(m)
	body["progress"] = progress
	err := doJSON(ctx, "POST", traktAPI+"/scrobble/"+action, h, body, nil)
	if StatusOf(err) == 409 { // already scrobbled recently
		return nil
	}
	return err
}

func (t *Trakt) Send(ctx context.Context, ev model.Event) error {
	h, err := t.headers(ctx)
	if err != nil {
		return err
	}
	switch ev.Action {
	case model.ActStart, model.ActPause, model.ActStop:
		return t.scrobble(ctx, h, ev.Action, ev.Media, clamp(ev.Progress))
	case model.ActWatched:
		if fresh(ev) {
			return t.scrobble(ctx, h, "stop", ev.Media, watchedProgress(ev.Progress))
		}
		return t.SendHistory(ctx, []model.Event{ev})
	case model.ActRate:
		return t.rate(ctx, h, ev)
	}
	return nil
}

func (t *Trakt) SendHistory(ctx context.Context, evs []model.Event) error {
	h, err := t.headers(ctx)
	if err != nil {
		return err
	}
	movies, shows, episodes := []M{}, []M{}, []M{}
	for _, ev := range evs {
		m, at := ev.Media, rfc3339(ev.At)
		switch {
		case m.Kind == model.Movie && !m.IDs.Empty():
			movies = append(movies, M{"title": m.Title, "year": m.Year, "ids": m.IDs, "watched_at": at})
		case m.Kind == model.Episode && !m.ShowIDs.Empty():
			shows = append(shows, M{"ids": m.ShowIDs, "seasons": []M{{"number": m.Season,
				"episodes": []M{{"number": m.Episode, "watched_at": at}}}}})
		case m.Kind == model.Episode && !m.IDs.Empty():
			episodes = append(episodes, M{"ids": m.IDs, "watched_at": at})
		}
	}
	if len(movies)+len(shows)+len(episodes) == 0 {
		return nil
	}
	return doJSON(ctx, "POST", traktAPI+"/sync/history", h, M{"movies": movies, "shows": shows, "episodes": episodes}, nil)
}

func (t *Trakt) rate(ctx context.Context, h map[string]string, ev model.Event) error {
	r := int(math.Round(ev.Rating))
	if r < 1 {
		return nil
	}
	if r > 10 {
		r = 10
	}
	m := ev.Media
	var body M
	switch {
	case m.Kind == model.Movie:
		body = M{"movies": []M{{"title": m.Title, "year": m.Year, "ids": m.IDs, "rating": r}}}
	case !m.ShowIDs.Empty():
		body = M{"shows": []M{{"ids": m.ShowIDs, "seasons": []M{{"number": m.Season,
			"episodes": []M{{"number": m.Episode, "rating": r}}}}}}}
	case !m.IDs.Empty():
		body = M{"episodes": []M{{"ids": m.IDs, "rating": r}}}
	default:
		return nil
	}
	return doJSON(ctx, "POST", traktAPI+"/sync/ratings", h, body, nil)
}

type traktHistoryItem struct {
	WatchedAt time.Time `json:"watched_at"`
	Type      string    `json:"type"`
	Movie     *struct {
		Title string    `json:"title"`
		Year  int       `json:"year"`
		IDs   model.IDs `json:"ids"`
	} `json:"movie"`
	Episode *struct {
		Season  int       `json:"season"`
		Number  int       `json:"number"`
		Title   string    `json:"title"`
		Runtime int       `json:"runtime"`
		IDs     model.IDs `json:"ids"`
	} `json:"episode"`
	Show *struct {
		Title string    `json:"title"`
		Year  int       `json:"year"`
		IDs   model.IDs `json:"ids"`
	} `json:"show"`
}

// ImportHistory pages through the user's entire Trakt history.
func (t *Trakt) ImportHistory(ctx context.Context, fn func(model.Media, time.Time) error, report func(string)) error {
	h, err := t.headers(ctx)
	if err != nil {
		return err
	}
	total := 0
	for page := 1; ; page++ {
		var res *result
		for attempt := 0; ; attempt++ {
			res, err = do(ctx, "GET", fmt.Sprintf("%s/sync/history?page=%d&limit=100&extended=full", traktAPI, page), h, nil)
			if err == nil || StatusOf(err) != 429 || attempt >= 5 {
				break
			}
			wait := 10 * time.Second
			if res != nil {
				if s, e := strconv.Atoi(res.Header.Get("Retry-After")); e == nil {
					wait = time.Duration(s+1) * time.Second
				}
			}
			if err := sleep(ctx, wait); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
		var items []traktHistoryItem
		if err := json.Unmarshal(res.Body, &items); err != nil {
			return err
		}
		for _, it := range items {
			var m model.Media
			switch {
			case it.Movie != nil:
				m = model.Media{Kind: model.Movie, Title: it.Movie.Title, Year: it.Movie.Year, IDs: it.Movie.IDs}
			case it.Episode != nil && it.Show != nil:
				m = model.Media{Kind: model.Episode, Title: it.Episode.Title, ShowTitle: it.Show.Title,
					Season: it.Episode.Season, Episode: it.Episode.Number, DurationS: it.Episode.Runtime * 60,
					IDs: it.Episode.IDs, ShowIDs: it.Show.IDs}
			default:
				continue
			}
			if err := fn(m, it.WatchedAt); err != nil {
				return err
			}
			total++
		}
		pages, _ := strconv.Atoi(res.Header.Get("X-Pagination-Page-Count"))
		report(fmt.Sprintf("page %d of %d, %d plays read", page, pages, total))
		if len(items) == 0 || page >= pages {
			return nil
		}
		if err := sleep(ctx, 350*time.Millisecond); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
