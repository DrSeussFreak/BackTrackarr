package core

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Everything in this file writes to Plex. Each function refuses unless
// PLEX_ALLOW_WRITES=true, and only runs when a person presses a button.
// Nothing here deletes or overwrites a playlist: copies are always new.

func (e *Engine) writesAllowed() error {
	if e.plex == nil {
		return fmt.Errorf("PLEX_URL and PLEX_TOKEN are not set")
	}
	if !e.cfg.PlexAllowWrites {
		return plex.ErrReadOnly
	}
	return nil
}

func (e *Engine) userClient(ctx context.Context, name string) (*PlexUser, error) {
	users, err := e.PlexUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].Name, name) {
			return &users[i], nil
		}
	}
	return nil, fmt.Errorf("no Plex user named %q (shared users need PLEX_ALL_USERS=true)", name)
}

// CopyRequest copies an archived playlist or collection into a playlist
// owned by another (or the same) Plex user.
type CopyRequest struct {
	ListID   int64
	ToUser   string
	Title    string // default: the source title
	AsStatic bool   // copy a smart playlist as a fixed list of its current items
}

func playlistTypeFor(l *store.PlexList) string {
	if l.Kind == "playlist" && l.ListType != "" {
		return l.ListType
	}
	switch l.ListType {
	case "artist", "album", "track":
		return "audio"
	case "photo":
		return "photo"
	}
	// collections: check the items
	for _, it := range l.Items {
		switch it.Type {
		case "track", "album", "artist":
			return "audio"
		}
		break
	}
	return "video"
}

// CopyList creates a new playlist for req.ToUser from an archived playlist
// or collection. Smart playlists stay smart (their filter is copied), unless
// AsStatic is set. Items are matched on the current server by rating key,
// then GUID, then file path, so lists archived from an old server work too.
func (e *Engine) CopyList(ctx context.Context, req CopyRequest, report func(string)) error {
	if err := e.writesAllowed(); err != nil {
		return err
	}
	l, err := e.st.GetList(ctx, req.ListID)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("list %d not found", req.ListID)
	}
	machine, err := e.MachineID(ctx)
	if err != nil {
		return err
	}
	u, err := e.userClient(ctx, req.ToUser)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = l.Title
	}
	if err := e.ensureNoPlaylist(ctx, u, title); err != nil {
		return err
	}
	ptype := playlistTypeFor(l)

	if l.Smart && l.Kind == "playlist" && !req.AsStatic {
		uri, err := plex.SmartURI(machine, l.Content)
		if err == nil {
			if _, err := u.Client.CreatePlaylist(ctx, title, ptype, true, uri); err != nil {
				return err
			}
			report(fmt.Sprintf("created smart playlist %q for %s", title, u.Name))
			return nil
		}
		report(fmt.Sprintf("smart filter could not be copied (%v); copying its current items instead", err))
	}

	keys, missing, err := e.mapItems(ctx, machine, l)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("none of the %d items exist on this server", len(l.Items))
	}
	if err := createStatic(ctx, u.Client, machine, title, ptype, keys); err != nil {
		return err
	}
	msg := fmt.Sprintf("created playlist %q for %s with %d items", title, u.Name, len(keys))
	if missing > 0 {
		msg += fmt.Sprintf(" (%d not found on this server)", missing)
	}
	report(msg)
	return nil
}

func createStatic(ctx context.Context, c *plex.Client, machine, title, ptype string, keys []string) error {
	const chunk = 100 // keeps the request URL a safe length
	first := keys
	if len(first) > chunk {
		first = keys[:chunk]
	}
	pk, err := c.CreatePlaylist(ctx, title, ptype, false, plex.LibraryURI(machine, first))
	if err != nil {
		return err
	}
	for i := len(first); i < len(keys); i += chunk {
		end := min(i+chunk, len(keys))
		if err := c.AddToPlaylist(ctx, pk, plex.LibraryURI(machine, keys[i:end])); err != nil {
			return fmt.Errorf("adding items %d-%d: %w", i, end, err)
		}
	}
	return nil
}

func (e *Engine) ensureNoPlaylist(ctx context.Context, u *PlexUser, title string) error {
	existing, err := u.Client.Playlists(ctx)
	if err != nil {
		return err
	}
	for _, raw := range existing {
		if it, err := plex.ParseLibItem(raw); err == nil && strings.EqualFold(it.Title, title) {
			return fmt.Errorf("%s already has a playlist named %q; pick another title", u.Name, title)
		}
	}
	return nil
}

// mapItems resolves a list's items to rating keys on the current server.
func (e *Engine) mapItems(ctx context.Context, machine string, l *store.PlexList) ([]string, int, error) {
	var keys []string
	missing := 0
	for _, it := range l.Items {
		if l.ServerID == machine && it.RatingKey != "" {
			keys = append(keys, it.RatingKey)
			continue
		}
		found, err := e.st.FindItem(ctx, machine, it.GUID, it.File)
		if err != nil {
			return nil, 0, err
		}
		if found == nil {
			missing++
			continue
		}
		keys = append(keys, found.RatingKey)
	}
	return keys, missing, nil
}

// RestorePlaylists recreates every archived playlist from another server
// (e.g. one that died) on the current server, for the same users. Lists
// that already exist (same owner and title) are skipped.
func (e *Engine) RestorePlaylists(ctx context.Context, fromServer string, report func(string)) error {
	if err := e.writesAllowed(); err != nil {
		return err
	}
	lists, err := e.st.Lists(ctx, fromServer, "playlist", "")
	if err != nil {
		return err
	}
	made, skipped, failed := 0, 0, 0
	for _, l := range lists {
		if l.DeletedAt != 0 {
			continue
		}
		err := e.CopyList(ctx, CopyRequest{ListID: l.ID, ToUser: l.Owner}, func(string) {})
		switch {
		case err == nil:
			made++
		case strings.Contains(err.Error(), "already has a playlist"):
			skipped++
		default:
			failed++
			e.log.Warn("restore playlist", "title", l.Title, "owner", l.Owner, "err", err)
		}
		report(fmt.Sprintf("%d created, %d already there, %d failed", made, skipped, failed))
	}
	return nil
}

// ---------- custom field restore ----------

// FieldChange is one hand-edited value to put back.
type FieldChange struct {
	Title     string
	SectionID string
	Type      string
	RatingKey string // on the current server
	Field     string
	From, To  string
}

var restorable = []string{"titleSort", "originalTitle", "editionTitle", "label", "collection", "userRating"}

// PlanFieldRestore compares an archived server's items with the current
// server and lists the custom values that are missing. Fields locked on the
// current server are left alone (someone edited them there on purpose).
func (e *Engine) PlanFieldRestore(ctx context.Context, fromServer string) ([]FieldChange, error) {
	machine, err := e.MachineID(ctx)
	if err != nil {
		return nil, err
	}
	var out []FieldChange
	err = e.st.EachItem(ctx, fromServer, false, func(src *store.PlexItem) error {
		if src.DeletedAt != 0 {
			return nil
		}
		var dst *store.PlexItem
		var err error
		if fromServer == machine {
			return nil // same server: nothing to restore from itself
		}
		if dst, err = e.st.FindItem(ctx, machine, src.GUID, src.File); err != nil || dst == nil {
			return err
		}
		locked := map[string]bool{}
		for _, f := range dst.Locked {
			locked[f] = true
		}
		add := func(field, from, to string) {
			if to != "" && from != to && !locked[field] {
				out = append(out, FieldChange{Title: dst.Title, SectionID: dst.SectionID, Type: dst.Type,
					RatingKey: dst.RatingKey, Field: field, From: from, To: to})
			}
		}
		add("titleSort", dst.TitleSort, src.TitleSort)
		add("originalTitle", dst.OriginalTitle, src.OriginalTitle)
		if dst.Type == "movie" {
			add("editionTitle", dst.Edition, src.Edition)
		}
		if extra := missingTags(dst.Labels, src.Labels); len(extra) > 0 {
			add("label", strings.Join(dst.Labels, ", "), strings.Join(union(dst.Labels, src.Labels), ", "))
		}
		if extra := missingTags(dst.Collections, src.Collections); len(extra) > 0 {
			add("collection", strings.Join(dst.Collections, ", "), strings.Join(union(dst.Collections, src.Collections), ", "))
		}
		if src.UserRating > 0 && dst.UserRating == 0 {
			add("userRating", "", strconv.FormatFloat(src.UserRating, 'f', -1, 64))
		}
		return nil
	})
	return out, err
}

func missingTags(have, want []string) []string {
	h := map[string]bool{}
	for _, t := range have {
		h[strings.ToLower(t)] = true
	}
	var out []string
	for _, t := range want {
		if !h[strings.ToLower(t)] {
			out = append(out, t)
		}
	}
	return out
}

func union(a, b []string) []string {
	out := append([]string{}, a...)
	out = append(out, missingTags(a, b)...)
	sort.Strings(out)
	return out
}

// ApplyFieldRestore writes the planned changes to the current server.
func (e *Engine) ApplyFieldRestore(ctx context.Context, fromServer string, report func(string)) error {
	if err := e.writesAllowed(); err != nil {
		return err
	}
	plan, err := e.PlanFieldRestore(ctx, fromServer)
	if err != nil {
		return err
	}
	type key struct{ sec, typ, rk string }
	byItem := map[key][]FieldChange{}
	var order []key
	for _, c := range plan {
		k := key{c.SectionID, c.Type, c.RatingKey}
		if _, ok := byItem[k]; !ok {
			order = append(order, k)
		}
		byItem[k] = append(byItem[k], c)
	}
	done, failed := 0, 0
	for _, k := range order {
		q := url.Values{}
		var rating float64
		for _, c := range byItem[k] {
			switch c.Field {
			case "label", "collection":
				for i, t := range strings.Split(c.To, ", ") {
					q.Set(fmt.Sprintf("%s[%d].tag.tag", c.Field, i), t)
				}
				q.Set(c.Field+".locked", "1")
			case "userRating":
				rating, _ = strconv.ParseFloat(c.To, 64)
			default:
				q.Set(c.Field+".value", c.To)
				q.Set(c.Field+".locked", "1")
			}
		}
		var err error
		if len(q) > 0 {
			err = e.plex.EditItem(ctx, k.sec, plex.TypeNum(k.typ), k.rk, q)
		}
		if err == nil && rating > 0 {
			err = e.plex.Rate(ctx, k.rk, rating)
		}
		if err != nil {
			failed++
			e.log.Warn("restore fields", "rating_key", k.rk, "err", err)
		} else {
			done++
		}
		if (done+failed)%25 == 0 {
			report(fmt.Sprintf("%d of %d items updated, %d failed", done, len(order), failed))
		}
	}
	report(fmt.Sprintf("done: %d items updated, %d failed", done, failed))
	return nil
}
