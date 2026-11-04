// Package plex parses Plex webhook payloads and talks to a Plex Media Server.
package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"reelog/internal/model"
)

var ErrNotFound = errors.New("plex: not found")

type Guid struct {
	ID string `json:"id"`
}

type SessionUser struct {
	ID    json.Number `json:"id"`
	Title string      `json:"title"`
}

type SessionPlayer struct {
	Title             string `json:"title"`
	State             string `json:"state"`
	MachineIdentifier string `json:"machineIdentifier"`
}

// Metadata is the subset of a Plex metadata item we use. The same shape
// appears in webhooks, /status/sessions, history, and /library/metadata.
type Metadata struct {
	RatingKey            string         `json:"ratingKey"`
	Key                  string         `json:"key"`
	GUID                 string         `json:"guid"`
	Type                 string         `json:"type"`
	Title                string         `json:"title"`
	GrandparentTitle     string         `json:"grandparentTitle"`
	GrandparentRatingKey string         `json:"grandparentRatingKey"`
	GrandparentKey       string         `json:"grandparentKey"`
	ParentTitle          string         `json:"parentTitle"`
	LibrarySectionID     json.Number    `json:"librarySectionID"`
	LibrarySectionTitle  string         `json:"librarySectionTitle"`
	LastViewedAt         int64          `json:"lastViewedAt"`
	ParentIndex          int            `json:"parentIndex"`
	Index                int            `json:"index"`
	Year                 int            `json:"year"`
	Duration             int64          `json:"duration"`
	ViewOffset           int64          `json:"viewOffset"`
	UserRating           float64        `json:"userRating"`
	ViewedAt             int64          `json:"viewedAt"`
	AccountID            json.Number    `json:"accountID"`
	SessionKey           string         `json:"sessionKey"`
	Guids                []Guid         `json:"Guid"`
	User                 *SessionUser   `json:"User,omitempty"`
	Player               *SessionPlayer `json:"Player,omitempty"`
}

// WebhookPayload is the JSON in the "payload" field of a Plex webhook.
type WebhookPayload struct {
	Event   string  `json:"event"`
	Owner   bool    `json:"owner"`
	Rating  float64 `json:"rating"`
	Account struct {
		ID    json.Number `json:"id"`
		Title string      `json:"title"`
	} `json:"Account"`
	Server struct {
		Title string `json:"title"`
		UUID  string `json:"uuid"`
	} `json:"Server"`
	Player struct {
		Title string `json:"title"`
		UUID  string `json:"uuid"`
	} `json:"Player"`
	Metadata Metadata `json:"Metadata"`
}

// applyGUID reads one Plex guid (new "imdb://tt..." style or legacy
// "com.plexapp.agents.thetvdb://123/1/2?lang=en" style) into m.
func applyGUID(g string, m *model.Media) {
	g, _, _ = strings.Cut(g, "?")
	scheme, rest, ok := strings.Cut(g, "://")
	if !ok || rest == "" {
		return
	}
	if i := strings.LastIndex(scheme, "."); i >= 0 {
		scheme = scheme[i+1:]
	}
	parts := strings.Split(rest, "/")
	target := &m.IDs
	if m.Kind == model.Episode && len(parts) >= 3 {
		// legacy agents encode show-id/season/episode
		target = &m.ShowIDs
		if m.Season == 0 && m.Episode == 0 {
			m.Season, _ = strconv.Atoi(parts[1])
			m.Episode, _ = strconv.Atoi(parts[2])
		}
	}
	id := parts[0]
	switch scheme {
	case "imdb":
		if strings.HasPrefix(id, "tt") && target.IMDB == "" {
			target.IMDB = id
		}
	case "tmdb", "themoviedb":
		if n, err := strconv.Atoi(id); err == nil && target.TMDB == 0 {
			target.TMDB = n
		}
	case "tvdb", "thetvdb":
		if n, err := strconv.Atoi(id); err == nil && target.TVDB == 0 {
			target.TVDB = n
		}
	case "mbid", "musicbrainz":
		if target.MBID == "" {
			target.MBID = id
		}
	}
}

// ToMedia converts Plex metadata to a Media. ok is false for anything that
// isn't a movie, episode or music track (clips, trailers, photos...).
func ToMedia(md Metadata) (model.Media, bool) {
	var m model.Media
	switch md.Type {
	case "movie":
		m.Kind = model.Movie
	case "episode":
		m.Kind = model.Episode
		m.ShowTitle = md.GrandparentTitle
		m.Season = md.ParentIndex
		m.Episode = md.Index
	case "track":
		m.Kind = model.Track
		m.ShowTitle = md.GrandparentTitle // artist
		m.Album = md.ParentTitle
		m.Season = md.ParentIndex // disc
		m.Episode = md.Index      // track number
	default:
		return m, false
	}
	m.Title = md.Title
	m.Year = md.Year
	m.DurationS = int(md.Duration / 1000)
	m.PlexKey = md.RatingKey
	if strings.HasPrefix(md.GUID, "plex://") {
		m.PlexGUID = md.GUID
	} else if md.GUID != "" {
		applyGUID(md.GUID, &m)
	}
	for _, g := range md.Guids {
		applyGUID(g.ID, &m)
	}
	return m, true
}

// Progress returns playback progress as a 0-100 percentage.
func Progress(md *Metadata) float64 {
	if md.Duration <= 0 {
		return 0
	}
	p := float64(md.ViewOffset) * 100 / float64(md.Duration)
	p = math.Max(0, math.Min(100, p))
	return math.Round(p*100) / 100
}

// ---------- Plex Media Server client ----------

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// AllowWrites must be true for anything that changes the server (restore,
	// copy playlists). Reading never needs it.
	AllowWrites bool
	PlexTV      string // plex.tv base URL (overridable for tests)

	mu        sync.Mutex
	showCache map[string]model.IDs
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		HTTP:      &http.Client{Timeout: 20 * time.Second},
		showCache: map[string]model.IDs{},
	}
}

type container struct {
	MediaContainer struct {
		Size      int        `json:"size"`
		TotalSize int        `json:"totalSize"`
		Metadata  []Metadata `json:"Metadata"`
		Account   []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"Account"`
	} `json:"MediaContainer"`
}

func (c *Client) get(ctx context.Context, p string, q url.Values) (*container, error) {
	u := c.BaseURL + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	req.Header.Set("X-Plex-Client-Identifier", "reelog")
	req.Header.Set("X-Plex-Product", "Reelog")
	req.Header.Set("X-Plex-Version", model.Version)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex %s: %s", p, resp.Status)
	}
	var out container
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("plex %s: decode: %w", p, err)
	}
	return &out, nil
}

func (c *Client) Metadata(ctx context.Context, ratingKey string) (*Metadata, error) {
	mc, err := c.get(ctx, "/library/metadata/"+url.PathEscape(ratingKey), url.Values{"includeGuids": {"1"}})
	if err != nil {
		return nil, err
	}
	if len(mc.MediaContainer.Metadata) == 0 {
		return nil, ErrNotFound
	}
	return &mc.MediaContainer.Metadata[0], nil
}

func (c *Client) Sessions(ctx context.Context) ([]Metadata, error) {
	mc, err := c.get(ctx, "/status/sessions", nil)
	if err != nil {
		return nil, err
	}
	return mc.MediaContainer.Metadata, nil
}

// History returns one page of the server's full watch history, oldest first.
func (c *Client) History(ctx context.Context, start, size int) ([]Metadata, error) {
	q := url.Values{}
	q.Set("sort", "viewedAt:asc")
	q.Set("X-Plex-Container-Start", strconv.Itoa(start))
	q.Set("X-Plex-Container-Size", strconv.Itoa(size))
	mc, err := c.get(ctx, "/status/sessions/history/all", q)
	if err != nil {
		return nil, err
	}
	return mc.MediaContainer.Metadata, nil
}

// Accounts maps server account ids to usernames.
func (c *Client) Accounts(ctx context.Context) (map[string]string, error) {
	mc, err := c.get(ctx, "/accounts", nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range mc.MediaContainer.Account {
		out[a.ID.String()] = a.Name
	}
	return out, nil
}

// Enrich fills in external ids that the webhook/session payload lacks by
// asking the Plex server. Safe to call on a nil client (no-op).
func (c *Client) Enrich(ctx context.Context, m *model.Media, md *Metadata) {
	if c == nil {
		return
	}
	gpKey := md.GrandparentRatingKey
	if m.IDs.Empty() && md.RatingKey != "" {
		if full, err := c.Metadata(ctx, md.RatingKey); err == nil {
			if m.PlexGUID == "" && strings.HasPrefix(full.GUID, "plex://") {
				m.PlexGUID = full.GUID
			}
			for _, g := range full.Guids {
				applyGUID(g.ID, m)
			}
			if m.DurationS == 0 {
				m.DurationS = int(full.Duration / 1000)
			}
			if m.Year == 0 {
				m.Year = full.Year
			}
			if gpKey == "" {
				gpKey = full.GrandparentRatingKey
			}
		}
	}
	if m.Kind == model.Episode && m.ShowIDs.Empty() {
		if gpKey == "" && md.GrandparentKey != "" {
			gpKey = path.Base(md.GrandparentKey)
		}
		if gpKey != "" {
			m.ShowIDs.Merge(c.showIDs(ctx, gpKey))
		}
	}
}

func (c *Client) showIDs(ctx context.Context, ratingKey string) model.IDs {
	c.mu.Lock()
	ids, ok := c.showCache[ratingKey]
	c.mu.Unlock()
	if ok {
		return ids
	}
	if show, err := c.Metadata(ctx, ratingKey); err == nil {
		tmp := model.Media{Kind: model.Movie} // so guids land in IDs
		for _, g := range show.Guids {
			applyGUID(g.ID, &tmp)
		}
		if show.GUID != "" && !strings.HasPrefix(show.GUID, "plex://") {
			applyGUID(show.GUID, &tmp)
		}
		ids = tmp.IDs
	} else if !errors.Is(err, ErrNotFound) {
		return ids // transient error: don't cache
	}
	c.mu.Lock()
	if len(c.showCache) > 5000 {
		c.showCache = map[string]model.IDs{}
	}
	c.showCache[ratingKey] = ids
	c.mu.Unlock()
	return ids
}
