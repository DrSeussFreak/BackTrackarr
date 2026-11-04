package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// Public Trakt lookups (no user login needed, only the client id). Used to
// turn titles from streaming-service exports into ids.

type TraktHit struct {
	Title string
	Year  int
	IDs   model.IDs
}

func (t *Trakt) public() map[string]string {
	return map[string]string{"trakt-api-version": "2", "trakt-api-key": t.ClientID}
}

// Search finds a movie ("movie") or show ("show") by title and optional year.
func (t *Trakt) Search(ctx context.Context, kind, title string, year int) ([]TraktHit, error) {
	if t.ClientID == "" {
		return nil, fmt.Errorf("TRAKT_CLIENT_ID is not set")
	}
	q := url.Values{"query": {title}, "limit": {"5"}}
	if year > 0 {
		q.Set("years", strconv.Itoa(year))
	}
	res, err := do(ctx, "GET", traktAPI+"/search/"+kind+"?"+q.Encode(), t.public(), nil)
	if err != nil {
		return nil, err
	}
	var hits []map[string]json.RawMessage
	if err := json.Unmarshal(res.Body, &hits); err != nil {
		return nil, err
	}
	var out []TraktHit
	for _, h := range hits {
		var it struct {
			Title string    `json:"title"`
			Year  int       `json:"year"`
			IDs   model.IDs `json:"ids"`
		}
		if json.Unmarshal(h[kind], &it) == nil && it.Title != "" {
			out = append(out, TraktHit{Title: it.Title, Year: it.Year, IDs: it.IDs})
		}
	}
	return out, nil
}

type TraktEpisode struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	IDs    model.IDs `json:"ids"`
}

// SeasonEpisodes lists one season of a show (by trakt id or slug).
func (t *Trakt) SeasonEpisodes(ctx context.Context, show string, season int) ([]TraktEpisode, error) {
	res, err := do(ctx, "GET", fmt.Sprintf("%s/shows/%s/seasons/%d", traktAPI, url.PathEscape(show), season), t.public(), nil)
	if err != nil {
		return nil, err
	}
	var eps []TraktEpisode
	err = json.Unmarshal(res.Body, &eps)
	return eps, err
}
