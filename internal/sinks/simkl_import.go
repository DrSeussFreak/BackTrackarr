package sinks

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// simklIDs accepts Simkl's ids, which mix numbers and strings.
func simklIDsFrom(raw map[string]any) model.IDs {
	str := func(k string) string {
		switch v := raw[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	ids := model.IDs{IMDB: str("imdb")}
	ids.TMDB, _ = strconv.Atoi(str("tmdb"))
	ids.TVDB, _ = strconv.Atoi(str("tvdb"))
	return ids
}

type simklHistItem struct {
	LastWatchedAt time.Time `json:"last_watched_at"`
	Movie         *struct {
		Title string         `json:"title"`
		Year  int            `json:"year"`
		IDs   map[string]any `json:"ids"`
	} `json:"movie"`
	Show *struct {
		Title string         `json:"title"`
		Year  int            `json:"year"`
		IDs   map[string]any `json:"ids"`
	} `json:"show"`
	Seasons []struct {
		Number   int `json:"number"`
		Episodes []struct {
			Number    int        `json:"number"`
			WatchedAt *time.Time `json:"watched_at"`
		} `json:"episodes"`
	} `json:"seasons"`
}

// ImportHistory reads everything watched on Simkl after since (all of it
// when since is zero), including anime, calling fn per play.
func (s *Simkl) ImportHistory(ctx context.Context, since time.Time, fn func(model.Media, time.Time) error) (int, error) {
	h, err := s.headers(ctx)
	if err != nil {
		return 0, err
	}
	p := "/sync/all-items/?extended=full&episode_watched_at=yes"
	if !since.IsZero() {
		p += "&date_from=" + since.UTC().Format(time.RFC3339)
	}
	res, err := do(ctx, "GET", s.url(p), h, nil)
	if err != nil {
		return 0, err
	}
	var out map[string][]simklHistItem
	if err := json.Unmarshal(res.Body, &out); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range out["movies"] {
		if it.Movie == nil || it.LastWatchedAt.IsZero() {
			continue
		}
		m := model.Media{Kind: model.Movie, Title: it.Movie.Title, Year: it.Movie.Year, IDs: simklIDsFrom(it.Movie.IDs)}
		if err := fn(m, it.LastWatchedAt); err != nil {
			return n, err
		}
		n++
	}
	for _, group := range []string{"shows", "anime"} {
		for _, it := range out[group] {
			if it.Show == nil {
				continue
			}
			ids := simklIDsFrom(it.Show.IDs)
			for _, se := range it.Seasons {
				for _, ep := range se.Episodes {
					at := it.LastWatchedAt
					if ep.WatchedAt != nil {
						at = *ep.WatchedAt
					}
					if at.IsZero() || (!since.IsZero() && at.Before(since)) {
						continue
					}
					m := model.Media{Kind: model.Episode, ShowTitle: it.Show.Title, Season: se.Number, Episode: ep.Number,
						Title: "Episode " + strconv.Itoa(ep.Number), ShowIDs: ids}
					if err := fn(m, at); err != nil {
						return n, err
					}
					n++
				}
			}
		}
	}
	return n, nil
}
