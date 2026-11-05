package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// ErrReadOnly is returned by every call that would change something in Plex
// while writes are disabled (the default). BackTrackarr only reads from Plex unless
// PLEX_ALLOW_WRITES=true, and even then only on an explicit button press.
var ErrReadOnly = errors.New("plex: writes are disabled (set PLEX_ALLOW_WRITES=true to allow restore and copy)")

// Plex metadata type numbers used by library queries.
const (
	TypeMovie   = 1
	TypeShow    = 2
	TypeSeason  = 3
	TypeEpisode = 4
	TypeArtist  = 8
	TypeAlbum   = 9
	TypeTrack   = 10
)

var typeNames = map[string]int{"movie": TypeMovie, "show": TypeShow, "season": TypeSeason, "episode": TypeEpisode,
	"artist": TypeArtist, "album": TypeAlbum, "track": TypeTrack}

func TypeNum(name string) int { return typeNames[name] }

// SectionTypes lists the item types to archive for a library type.
func SectionTypes(sectionType string) []int {
	switch sectionType {
	case "movie":
		return []int{TypeMovie}
	case "show":
		return []int{TypeShow, TypeSeason, TypeEpisode}
	case "artist":
		return []int{TypeArtist, TypeAlbum, TypeTrack}
	}
	return nil
}

// As returns a client for the same server acting as another user (with that
// user's server access token). It shares the HTTP client and settings.
func (c *Client) As(token string) *Client {
	return &Client{BaseURL: c.BaseURL, Token: token, HTTP: c.HTTP, AllowWrites: c.AllowWrites, PlexTV: c.PlexTV, showCache: map[string]model.IDs{}}
}

func (c *Client) request(ctx context.Context, method, p string, q url.Values) (*http.Response, error) {
	u := c.BaseURL + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	req.Header.Set("X-Plex-Client-Identifier", "backtrackarr")
	req.Header.Set("X-Plex-Product", "BackTrackarr")
	req.Header.Set("X-Plex-Version", model.Version)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("plex %s %s: %s %s", method, p, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, p string, q url.Values, out any) error {
	resp, err := c.request(ctx, http.MethodGet, p, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(out); err != nil {
		return fmt.Errorf("plex %s: decode: %w", p, err)
	}
	return nil
}

// write is the only path that changes anything on the Plex server.
func (c *Client) write(ctx context.Context, method, p string, q url.Values, out any) error {
	if !c.AllowWrites {
		return ErrReadOnly
	}
	resp, err := c.request(ctx, method, p, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// ---------- reading the library ----------

type rawContainer struct {
	MediaContainer struct {
		Size              int               `json:"size"`
		TotalSize         int               `json:"totalSize"`
		MachineIdentifier string            `json:"machineIdentifier"`
		Metadata          []json.RawMessage `json:"Metadata"`
		Directory         []json.RawMessage `json:"Directory"`
	} `json:"MediaContainer"`
}

// Identity returns the server's machine identifier.
func (c *Client) Identity(ctx context.Context) (string, error) {
	var rc rawContainer
	if err := c.getJSON(ctx, "/identity", nil, &rc); err != nil {
		return "", err
	}
	if rc.MediaContainer.MachineIdentifier == "" {
		return "", errors.New("plex: /identity returned no machineIdentifier")
	}
	return rc.MediaContainer.MachineIdentifier, nil
}

type Section struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Title string `json:"title"`
	UUID  string `json:"uuid"`
	Agent string `json:"agent"`
}

func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var out struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, "/library/sections", nil, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Directory, nil
}

// Page fetches one page of a list endpoint. Metadata and Directory entries
// are returned together, untouched.
func (c *Client) Page(ctx context.Context, p string, q url.Values, start, size int) ([]json.RawMessage, int, error) {
	q2 := url.Values{}
	for k, v := range q {
		q2[k] = v
	}
	q2.Set("X-Plex-Container-Start", strconv.Itoa(start))
	q2.Set("X-Plex-Container-Size", strconv.Itoa(size))
	var rc rawContainer
	if err := c.getJSON(ctx, p, q2, &rc); err != nil {
		return nil, 0, err
	}
	items := append(rc.MediaContainer.Metadata, rc.MediaContainer.Directory...)
	total := rc.MediaContainer.TotalSize
	if total == 0 {
		total = rc.MediaContainer.Size
	}
	return items, total, nil
}

// All pages through a list endpoint, calling fn for each page.
func (c *Client) All(ctx context.Context, p string, q url.Values, size int, fn func([]json.RawMessage) error) error {
	for start := 0; ; {
		items, total, err := c.Page(ctx, p, q, start, size)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			if err := fn(items); err != nil {
				return err
			}
		}
		start += len(items)
		if len(items) == 0 || len(items) < size || (total > 0 && start >= total) {
			return nil
		}
	}
}

// SectionItems lists one item type of a library section.
func (c *Client) SectionItems(ctx context.Context, section string, typ int, sortBy string, size int, fn func([]json.RawMessage) error) error {
	q := url.Values{"type": {strconv.Itoa(typ)}, "includeGuids": {"1"}}
	if sortBy != "" {
		q.Set("sort", sortBy)
	}
	return c.All(ctx, "/library/sections/"+url.PathEscape(section)+"/all", q, size, fn)
}

// MetadataRaw fetches full metadata for several items in one request.
func (c *Client) MetadataRaw(ctx context.Context, keys []string) ([]json.RawMessage, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var rc rawContainer
	err := c.getJSON(ctx, "/library/metadata/"+strings.Join(keys, ","), url.Values{"includeGuids": {"1"}}, &rc)
	if err != nil {
		return nil, err
	}
	return rc.MediaContainer.Metadata, nil
}

// LibItem holds the fields BackTrackarr pulls out of full Plex metadata.
type LibItem struct {
	RatingKey            string      `json:"ratingKey"`
	Key                  string      `json:"key"`
	GUID                 string      `json:"guid"`
	Type                 string      `json:"type"`
	Title                string      `json:"title"`
	TitleSort            string      `json:"titleSort"`
	OriginalTitle        string      `json:"originalTitle"`
	EditionTitle         string      `json:"editionTitle"`
	Year                 int         `json:"year"`
	ParentRatingKey      string      `json:"parentRatingKey"`
	ParentTitle          string      `json:"parentTitle"`
	GrandparentRatingKey string      `json:"grandparentRatingKey"`
	GrandparentTitle     string      `json:"grandparentTitle"`
	Index                int         `json:"index"`
	ParentIndex          int         `json:"parentIndex"`
	UserRating           float64     `json:"userRating"`
	ViewCount            int         `json:"viewCount"`
	LastViewedAt         int64       `json:"lastViewedAt"`
	AddedAt              int64       `json:"addedAt"`
	UpdatedAt            int64       `json:"updatedAt"`
	Duration             int64       `json:"duration"`
	LibrarySectionID     json.Number `json:"librarySectionID"`
	LibrarySectionTitle  string      `json:"librarySectionTitle"`
	Smart                any         `json:"smart"`
	Content              string      `json:"content"`
	Summary              string      `json:"summary"`
	PlaylistType         string      `json:"playlistType"`
	Subtype              string      `json:"subtype"`
	LeafCount            int         `json:"leafCount"`
	ChildCount           int         `json:"childCount"`
	Label                []Tag       `json:"Label"`
	Collection           []Tag       `json:"Collection"`
	Field                []struct {
		Name   string `json:"name"`
		Locked bool   `json:"locked"`
	} `json:"Field"`
	Media []struct {
		Part []struct {
			File string `json:"file"`
		} `json:"Part"`
	} `json:"Media"`
	Guids []Guid `json:"Guid"`
}

type Tag struct {
	Tag string `json:"tag"`
}

func ParseLibItem(raw json.RawMessage) (*LibItem, error) {
	var it LibItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return nil, err
	}
	return &it, nil
}

func (it *LibItem) IsSmart() bool {
	switch v := it.Smart.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v == "1" || v == "true"
	}
	return false
}

func (it *LibItem) File() string {
	for _, m := range it.Media {
		for _, p := range m.Part {
			if p.File != "" {
				return p.File
			}
		}
	}
	return ""
}

func tags(ts []Tag) []string {
	var out []string
	for _, t := range ts {
		if t.Tag != "" {
			out = append(out, t.Tag)
		}
	}
	sort.Strings(out)
	return out
}

func (it *LibItem) Labels() []string      { return tags(it.Label) }
func (it *LibItem) Collections() []string { return tags(it.Collection) }

func (it *LibItem) Locked() []string {
	var out []string
	for _, f := range it.Field {
		if f.Locked {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Sig is a cheap fingerprint from a library listing; when it changes the
// full metadata is fetched again.
func (it *LibItem) Sig() string {
	return fmt.Sprintf("%d|%d|%g|%s|%s|%d", it.UpdatedAt, it.AddedAt, it.UserRating, it.TitleSort, it.Title, it.ChildCount)
}

// Metadata converts to the playback Metadata shape used by ToMedia.
func (it *LibItem) Metadata() Metadata {
	return Metadata{RatingKey: it.RatingKey, Key: it.Key, GUID: it.GUID, Type: it.Type, Title: it.Title,
		GrandparentTitle: it.GrandparentTitle, GrandparentRatingKey: it.GrandparentRatingKey,
		ParentTitle: it.ParentTitle, ParentIndex: it.ParentIndex, Index: it.Index, Year: it.Year,
		Duration: it.Duration, UserRating: it.UserRating, Guids: it.Guids}
}

// volatile fields change on every play and would make every item look edited.
var volatile = []string{"viewCount", "lastViewedAt", "viewOffset", "skipCount", "viewedLeafCount", "lastRatedAt", "OnDeck"}

// StableJSON strips volatile fields and re-encodes with sorted keys, so the
// same metadata always produces the same bytes.
func StableJSON(raw json.RawMessage) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	for _, k := range volatile {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// ---------- playlists and collections ----------

// Playlists lists every playlist visible to this client's user.
func (c *Client) Playlists(ctx context.Context) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := c.All(ctx, "/playlists", url.Values{"includeGuids": {"1"}}, 200, func(p []json.RawMessage) error {
		out = append(out, p...)
		return nil
	})
	return out, err
}

func (c *Client) Collections(ctx context.Context, section string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := c.All(ctx, "/library/sections/"+url.PathEscape(section)+"/collections", url.Values{"includeGuids": {"1"}}, 200,
		func(p []json.RawMessage) error {
			out = append(out, p...)
			return nil
		})
	return out, err
}

// ListItems returns the items of a playlist or collection.
func (c *Client) ListItems(ctx context.Context, kind, ratingKey string) ([]*LibItem, error) {
	p := "/playlists/" + url.PathEscape(ratingKey) + "/items"
	if kind == "collection" {
		p = "/library/collections/" + url.PathEscape(ratingKey) + "/children"
	}
	var out []*LibItem
	err := c.All(ctx, p, url.Values{"includeGuids": {"1"}}, 300, func(page []json.RawMessage) error {
		for _, raw := range page {
			it, err := ParseLibItem(raw)
			if err != nil {
				return err
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// ---------- users (plex.tv) ----------

type SharedUser struct {
	Username string
	UserID   string
	Token    string // server access token for this user
}

// SharedUsers asks plex.tv for the users this server is shared with,
// including managed (home) users, and their server access tokens. This is a
// read; it needs the owner's token.
func (c *Client) SharedUsers(ctx context.Context, machineID string) ([]SharedUser, error) {
	base := c.PlexTV
	if base == "" {
		base = "https://plex.tv"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/servers/"+url.PathEscape(machineID)+"/shared_servers", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Plex-Token", c.Token)
	req.Header.Set("X-Plex-Client-Identifier", "backtrackarr")
	req.Header.Set("X-Plex-Product", "BackTrackarr")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex.tv shared_servers: %s", resp.Status)
	}
	var doc struct {
		Shared []struct {
			Username    string `xml:"username,attr"`
			UserID      string `xml:"userID,attr"`
			AccessToken string `xml:"accessToken,attr"`
		} `xml:"SharedServer"`
	}
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("plex.tv shared_servers: %w", err)
	}
	var out []SharedUser
	for _, s := range doc.Shared {
		if s.AccessToken != "" {
			out = append(out, SharedUser{Username: s.Username, UserID: s.UserID, Token: s.AccessToken})
		}
	}
	return out, nil
}

// ---------- writes (only with PLEX_ALLOW_WRITES=true) ----------

func LibraryURI(machineID string, keys []string) string {
	return "server://" + machineID + "/com.plexapp.plugins.library/library/metadata/" + strings.Join(keys, ",")
}

// SmartURI turns a smart playlist's stored "content" filter into the uri
// that recreates it on the given server. The content looks like
// "library://<section-uuid>/directory/%2Flibrary%2Fsections%2F1%2Fall%3F..." or a
// plain "/library/sections/1/all?..." path.
func SmartURI(machineID, content string) (string, error) {
	p := content
	if i := strings.Index(p, "/directory/"); i >= 0 {
		dec, err := url.PathUnescape(p[i+len("/directory/"):])
		if err != nil {
			return "", err
		}
		p = dec
	}
	if !strings.HasPrefix(p, "/library/") {
		return "", fmt.Errorf("unrecognized smart filter %q", content)
	}
	return "server://" + machineID + "/com.plexapp.plugins.library" + p, nil
}

// CreatePlaylist creates a playlist for this client's user and returns its
// rating key. uri is a LibraryURI (static) or SmartURI (smart).
func (c *Client) CreatePlaylist(ctx context.Context, title, playlistType string, smart bool, uri string) (string, error) {
	q := url.Values{"title": {title}, "type": {playlistType}, "uri": {uri}, "smart": {"0"}}
	if smart {
		q.Set("smart", "1")
	}
	var out struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.write(ctx, http.MethodPost, "/playlists", q, &out); err != nil {
		return "", err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return "", errors.New("plex did not return the new playlist")
	}
	return out.MediaContainer.Metadata[0].RatingKey, nil
}

func (c *Client) AddToPlaylist(ctx context.Context, playlistKey, uri string) error {
	return c.write(ctx, http.MethodPut, "/playlists/"+url.PathEscape(playlistKey)+"/items", url.Values{"uri": {uri}}, nil)
}

// EditItem sets fields on one item, e.g. titleSort.value=..., titleSort.locked=1,
// label[0].tag.tag=..., collection[0].tag.tag=....
func (c *Client) EditItem(ctx context.Context, section string, typ int, ratingKey string, fields url.Values) error {
	q := url.Values{"type": {strconv.Itoa(typ)}, "id": {ratingKey}}
	for k, v := range fields {
		q[k] = v
	}
	return c.write(ctx, http.MethodPut, "/library/sections/"+url.PathEscape(section)+"/all", q, nil)
}

// Rate sets this client's user's star rating (0-10) on an item.
func (c *Client) Rate(ctx context.Context, ratingKey string, rating float64) error {
	return c.write(ctx, http.MethodPut, "/:/rate", url.Values{"key": {ratingKey},
		"identifier": {"com.plexapp.plugins.library"}, "rating": {strconv.FormatFloat(rating, 'f', -1, 64)}}, nil)
}
