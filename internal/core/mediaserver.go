package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"reelog/internal/config"
	"reelog/internal/plex"
	"reelog/internal/store"
)

// Copying Plex playlists and collections to Jellyfin or Emby, so they show
// up in clients such as Pleazy when it's connected to those servers. (Pleazy
// on Plex already sees Plex playlists directly.) Jellyfin and Emby have no
// smart playlists, so smart ones are copied as their current items.

var msHTTP = &http.Client{Timeout: 60 * time.Second}

type msClient struct{ srv config.MediaServer }

func (m msClient) do(ctx context.Context, method, p string, q url.Values, out any) error {
	u := m.srv.URL + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", m.srv.APIKey)
	req.Header.Set("Authorization", `MediaBrowser Client="Reelog", Device="Reelog", DeviceId="reelog", Version="1", Token="`+m.srv.APIKey+`"`)
	req.Header.Set("Accept", "application/json")
	resp, err := msHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", m.srv.Name, p, resp.Status, truncate(string(b), 300))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (e *Engine) MediaServers() []config.MediaServer { return e.cfg.MediaServers }

func (e *Engine) mediaServer(name string) (msClient, error) {
	for _, s := range e.cfg.MediaServers {
		if s.Name == name {
			return msClient{s}, nil
		}
	}
	return msClient{}, fmt.Errorf("no Jellyfin/Emby server named %q", name)
}

type msUser struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

func (m msClient) users(ctx context.Context) ([]msUser, error) {
	var out []msUser
	err := m.do(ctx, http.MethodGet, "/Users", nil, &out)
	return out, err
}

// MediaServerUsers lists the users of one Jellyfin/Emby server.
func (e *Engine) MediaServerUsers(ctx context.Context, name string) ([]string, error) {
	m, err := e.mediaServer(name)
	if err != nil {
		return nil, err
	}
	us, err := m.users(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range us {
		out = append(out, u.Name)
	}
	return out, nil
}

type msItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"`
	Path        string            `json:"Path"`
	ProviderIDs map[string]string `json:"ProviderIds"`
}

// msIndex maps external ids and path tails to item ids on the target server.
type msIndex struct {
	byID   map[string]string
	byPath map[string]string
}

func pathTail(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	dir, file := path.Split(p)
	return strings.ToLower(path.Base(strings.TrimSuffix(dir, "/")) + "/" + file)
}

func (m msClient) index(ctx context.Context, userID string) (*msIndex, error) {
	idx := &msIndex{byID: map[string]string{}, byPath: map[string]string{}}
	for start := 0; ; {
		var page struct {
			Items            []msItem `json:"Items"`
			TotalRecordCount int      `json:"TotalRecordCount"`
		}
		q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie,Episode,Audio,Series,MusicAlbum"},
			"Fields": {"ProviderIds,Path"}, "StartIndex": {strconv.Itoa(start)}, "Limit": {"1000"}}
		if err := m.do(ctx, http.MethodGet, "/Users/"+url.PathEscape(userID)+"/Items", q, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			for k, v := range it.ProviderIDs {
				if v != "" {
					idx.byID[strings.ToLower(it.Type)+"|"+strings.ToLower(k)+"|"+strings.ToLower(v)] = it.ID
				}
			}
			if it.Path != "" {
				idx.byPath[pathTail(it.Path)] = it.ID
			}
		}
		start += len(page.Items)
		if len(page.Items) == 0 || start >= page.TotalRecordCount {
			return idx, nil
		}
	}
}

var msTypes = map[string]string{"movie": "movie", "episode": "episode", "track": "audio", "show": "series", "album": "musicalbum"}

// match finds a Plex item on the Jellyfin/Emby server via the archived
// metadata's external ids, then the file path.
func (e *Engine) match(ctx context.Context, idx *msIndex, serverID string, it store.ListItem) string {
	typ := msTypes[it.Type]
	if a, err := e.st.GetItemByKey(ctx, serverID, it.RatingKey); err == nil && a != nil {
		if li, err := plex.ParseLibItem(a.Data); err == nil {
			for _, g := range li.Guids {
				scheme, id, ok := strings.Cut(g.ID, "://")
				if !ok {
					continue
				}
				if scheme == "mbid" {
					scheme = "musicbrainztrack"
				}
				if v := idx.byID[typ+"|"+scheme+"|"+strings.ToLower(id)]; v != "" {
					return v
				}
			}
		}
		if it.File == "" {
			it.File = a.File
		}
	}
	if it.File != "" {
		return idx.byPath[pathTail(it.File)]
	}
	return ""
}

// CopyToMediaServer creates a playlist on Jellyfin/Emby for one of its users
// from an archived Plex playlist or collection.
func (e *Engine) CopyToMediaServer(ctx context.Context, listID int64, server, toUser, title string, report func(string)) error {
	m, err := e.mediaServer(server)
	if err != nil {
		return err
	}
	l, err := e.st.GetList(ctx, listID)
	if err != nil || l == nil {
		return fmt.Errorf("list %d not found", listID)
	}
	users, err := m.users(ctx)
	if err != nil {
		return err
	}
	var uid string
	for _, u := range users {
		if strings.EqualFold(u.Name, toUser) {
			uid = u.ID
		}
	}
	if uid == "" {
		return fmt.Errorf("%s has no user %q", server, toUser)
	}
	if title = strings.TrimSpace(title); title == "" {
		title = l.Title
	}
	var existing struct{ Items []msItem }
	if err := m.do(ctx, http.MethodGet, "/Users/"+url.PathEscape(uid)+"/Items",
		url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Playlist"}}, &existing); err == nil {
		for _, it := range existing.Items {
			if strings.EqualFold(it.Name, title) {
				return fmt.Errorf("%s already has a playlist named %q for %s", server, title, toUser)
			}
		}
	}
	report("indexing " + server + "…")
	idx, err := m.index(ctx, uid)
	if err != nil {
		return err
	}
	var ids []string
	missing := 0
	for _, it := range l.Items {
		if id := e.match(ctx, idx, l.ServerID, it); id != "" {
			ids = append(ids, id)
		} else {
			missing++
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("none of the %d items were found on %s", len(l.Items), server)
	}
	mediaType := "Video"
	if playlistTypeFor(l) == "audio" {
		mediaType = "Audio"
	}
	const chunk = 100
	first := ids[:min(chunk, len(ids))]
	var created struct {
		ID string `json:"Id"`
	}
	if err := m.do(ctx, http.MethodPost, "/Playlists", url.Values{"Name": {title}, "Ids": {strings.Join(first, ",")},
		"UserId": {uid}, "MediaType": {mediaType}}, &created); err != nil {
		return err
	}
	if created.ID == "" {
		return fmt.Errorf("%s did not return the new playlist id", server)
	}
	for i := len(first); i < len(ids); i += chunk {
		end := min(i+chunk, len(ids))
		if err := m.do(ctx, http.MethodPost, "/Playlists/"+url.PathEscape(created.ID)+"/Items",
			url.Values{"Ids": {strings.Join(ids[i:end], ",")}, "UserId": {uid}}, nil); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("created %q on %s for %s with %d items", title, server, toUser, len(ids))
	if missing > 0 {
		msg += fmt.Sprintf(" (%d not found there)", missing)
	}
	if l.Smart {
		msg += "; smart playlist copied as its current items"
	}
	report(msg)
	return nil
}
