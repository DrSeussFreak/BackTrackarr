package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// The archiver keeps a read-only copy of the Plex library: every movie,
// show, season, episode, artist, album and track, with the hand-edited
// fields (sort title, original title, edition, labels, collections, locks),
// plus every user's playlists and every library's collections. It never
// changes anything in Plex.

var errStop = errors.New("stop")

// PlexUser is a Plex account BackTrackarr can read as.
type PlexUser struct {
	Name   string
	Owner  bool
	Client *plex.Client
}

// MachineID returns (and caches) the Plex server's machine identifier.
func (e *Engine) MachineID(ctx context.Context) (string, error) {
	if e.plex == nil {
		return "", fmt.Errorf("PLEX_URL and PLEX_TOKEN are not set")
	}
	e.secMu.Lock()
	id := e.machine
	e.secMu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := e.plex.Identity(ctx)
	if err != nil {
		return "", err
	}
	e.secMu.Lock()
	e.machine = id
	e.secMu.Unlock()
	return id, nil
}

// PlexUsers returns the owner plus (with PLEX_ALL_USERS) every shared and
// managed user, each with a client that reads as that user.
func (e *Engine) PlexUsers(ctx context.Context) ([]PlexUser, error) {
	e.secMu.Lock()
	if e.users != nil && time.Since(e.usersAt) < time.Hour {
		u := e.users
		e.secMu.Unlock()
		return u, nil
	}
	e.secMu.Unlock()
	users, err := e.loadPlexUsers(ctx)
	if err == nil {
		e.secMu.Lock()
		e.users, e.usersAt = users, time.Now()
		e.secMu.Unlock()
	}
	return users, err
}

func (e *Engine) loadPlexUsers(ctx context.Context) ([]PlexUser, error) {
	machine, err := e.MachineID(ctx)
	if err != nil {
		return nil, err
	}
	owner := "owner"
	if acc, err := e.plex.Accounts(ctx); err == nil && acc["1"] != "" {
		owner = acc["1"]
	}
	users := []PlexUser{{Name: owner, Owner: true, Client: e.plex}}
	if !e.cfg.PlexAllUsers {
		return users, nil
	}
	shared, err := e.plex.SharedUsers(ctx, machine)
	if err != nil {
		e.log.Warn("plex.tv shared users (only the owner will be covered)", "err", err)
		return users, nil
	}
	for _, s := range shared {
		users = append(users, PlexUser{Name: s.Username, Client: e.plex.As(s.Token)})
	}
	return users, nil
}

// RunArchiver archives Plex incrementally every PLEX_ARCHIVE_MINUTES, with a
// full sweep (which also notices deletions) once a day.
func (e *Engine) RunArchiver(ctx context.Context) {
	every := e.cfg.PlexArchiveEvery
	e.log.Info("plex archiver started", "every", every)
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		last, _ := e.st.GetKV(ctx, "plex_archive_full_at")
		ts, _ := strconv.ParseInt(last, 10, 64)
		full := time.Since(time.Unix(ts, 0)) > 24*time.Hour
		name := "archive ← plex"
		e.Jobs.Start(ctx, name, func(ctx context.Context, report func(string)) error {
			return e.ArchivePlex(ctx, full, report)
		})
		t.Reset(every)
	}
}

type archiveCounts struct{ seen, changed, lists, listsChanged, deleted int64 }

// ArchivePlex copies the library, playlists and collections into the
// archive. A full run lists everything and marks what disappeared; an
// incremental run reads items newest-updated first and stops at the first
// page with no changes.
func (e *Engine) ArchivePlex(ctx context.Context, full bool, report func(string)) error {
	machine, err := e.MachineID(ctx)
	if err != nil {
		return err
	}
	secs, err := e.plex.Sections(ctx)
	if err != nil {
		return err
	}
	e.secMu.Lock()
	for _, s := range secs {
		e.sections[s.Key] = s.Title
	}
	e.secMu.Unlock()

	var n archiveCounts
	progress := func(what string) {
		report(fmt.Sprintf("%s: %d items checked, %d new or changed, %d lists (%d changed)", what, n.seen, n.changed, n.lists, n.listsChanged))
	}
	for _, sec := range secs {
		types := plex.SectionTypes(sec.Type)
		if len(types) == 0 {
			continue
		}
		start := time.Now().Unix()
		sigs, err := e.st.ItemSigs(ctx, machine, sec.Key)
		if err != nil {
			return err
		}
		for _, typ := range types {
			sortBy := ""
			if !full {
				sortBy = "updatedAt:desc"
			}
			err := e.plex.SectionItems(ctx, sec.Key, typ, sortBy, 200, func(page []json.RawMessage) error {
				var fetch, same []string
				for _, raw := range page {
					it, err := plex.ParseLibItem(raw)
					if err != nil || it.RatingKey == "" {
						continue
					}
					n.seen++
					if old, ok := sigs[it.RatingKey]; ok && old == it.Sig() {
						same = append(same, it.RatingKey)
					} else {
						fetch = append(fetch, it.RatingKey)
					}
				}
				if err := e.st.TouchItems(ctx, machine, same); err != nil {
					return err
				}
				if err := e.archiveItems(ctx, machine, sec, fetch, &n); err != nil {
					return err
				}
				progress(sec.Title)
				if !full && len(fetch) == 0 {
					return errStop
				}
				return nil
			})
			if err != nil && !errors.Is(err, errStop) {
				return fmt.Errorf("%s: %w", sec.Title, err)
			}
		}
		if full {
			d, err := e.st.MarkItemsDeleted(ctx, machine, sec.Key, start)
			if err != nil {
				return err
			}
			n.deleted += d
		}
		// Collections belong to the library, not to a user.
		if err := e.archiveCollections(ctx, machine, sec, &n); err != nil {
			e.log.Warn("archive collections", "section", sec.Title, "err", err)
		}
	}

	users, err := e.PlexUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := e.archivePlaylists(ctx, machine, u, full, &n); err != nil {
			e.log.Warn("archive playlists", "user", u.Name, "err", err)
		}
		progress("playlists for " + u.Name)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	_ = e.st.SetKV(ctx, "plex_archive_at", now)
	if full {
		_ = e.st.SetKV(ctx, "plex_archive_full_at", now)
	}
	report(fmt.Sprintf("done: %d items checked, %d new or changed, %d marked deleted, %d lists (%d changed)",
		n.seen, n.changed, n.deleted, n.lists, n.listsChanged))
	return nil
}

func (e *Engine) archiveItems(ctx context.Context, machine string, sec plex.Section, keys []string, n *archiveCounts) error {
	for len(keys) > 0 {
		batch := keys
		if len(batch) > 20 {
			batch = keys[:20]
		}
		keys = keys[len(batch):]
		raws, err := e.plex.MetadataRaw(ctx, batch)
		if err != nil {
			return err
		}
		for _, raw := range raws {
			it, err := plex.ParseLibItem(raw)
			if err != nil {
				continue
			}
			pi := toArchived(machine, sec, it, raw)
			changed, err := e.st.SaveItem(ctx, pi)
			if err != nil {
				return err
			}
			if changed {
				n.changed++
			}
		}
	}
	return nil
}

func toArchived(machine string, sec plex.Section, it *plex.LibItem, raw json.RawMessage) *store.PlexItem {
	secID, secTitle := it.LibrarySectionID.String(), it.LibrarySectionTitle
	if secID == "" {
		secID, secTitle = sec.Key, sec.Title
	}
	return &store.PlexItem{
		ServerID: machine, RatingKey: it.RatingKey, SectionID: secID, SectionTitle: secTitle, Type: it.Type,
		GUID: it.GUID, Title: it.Title, TitleSort: it.TitleSort, OriginalTitle: it.OriginalTitle, Edition: it.EditionTitle,
		Year: it.Year, ParentKey: it.ParentRatingKey, ParentTitle: it.ParentTitle, GrandparentKey: it.GrandparentRatingKey,
		GrandparentTitle: it.GrandparentTitle, Index: it.Index, ParentIndex: it.ParentIndex, UserRating: it.UserRating,
		Labels: it.Labels(), Collections: it.Collections(), Locked: it.Locked(), File: it.File(),
		AddedAt: it.AddedAt, UpdatedAt: it.UpdatedAt, Sig: it.Sig(), Data: plex.StableJSON(raw),
	}
}

func itemTitle(it *plex.LibItem) string {
	m, ok := plex.ToMedia(it.Metadata())
	if ok {
		return m.Display()
	}
	if it.GrandparentTitle != "" {
		return it.GrandparentTitle + " · " + it.Title
	}
	return it.Title
}

func (e *Engine) listItems(ctx context.Context, c *plex.Client, kind, key string) ([]store.ListItem, error) {
	items, err := c.ListItems(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	out := make([]store.ListItem, 0, len(items))
	for i, it := range items {
		out = append(out, store.ListItem{Position: i, RatingKey: it.RatingKey, GUID: it.GUID, Type: it.Type,
			Title: itemTitle(it), File: it.File()})
	}
	return out, nil
}

func (e *Engine) archivePlaylists(ctx context.Context, machine string, u PlexUser, full bool, n *archiveCounts) error {
	start := time.Now().Unix()
	raws, err := u.Client.Playlists(ctx)
	if err != nil {
		return err
	}
	for _, raw := range raws {
		it, err := plex.ParseLibItem(raw)
		if err != nil || it.RatingKey == "" {
			continue
		}
		items, err := e.listItems(ctx, u.Client, "playlist", it.RatingKey)
		if err != nil {
			return fmt.Errorf("playlist %q: %w", it.Title, err)
		}
		l := &store.PlexList{ServerID: machine, Kind: "playlist", Owner: u.Name, RatingKey: it.RatingKey, Title: it.Title,
			ListType: it.PlaylistType, Smart: it.IsSmart(), Content: it.Content, Summary: it.Summary,
			Data: plex.StableJSON(raw), Items: items}
		changed, err := e.st.SaveList(ctx, l)
		if err != nil {
			return err
		}
		n.lists++
		if changed {
			n.listsChanged++
		}
	}
	_, err = e.st.MarkListsDeleted(ctx, machine, "playlist", u.Name, start)
	return err
}

func (e *Engine) archiveCollections(ctx context.Context, machine string, sec plex.Section, n *archiveCounts) error {
	start := time.Now().Unix()
	raws, err := e.plex.Collections(ctx, sec.Key)
	if err != nil {
		return err
	}
	for _, raw := range raws {
		it, err := plex.ParseLibItem(raw)
		if err != nil || it.RatingKey == "" {
			continue
		}
		items, err := e.listItems(ctx, e.plex, "collection", it.RatingKey)
		if err != nil {
			return fmt.Errorf("collection %q: %w", it.Title, err)
		}
		l := &store.PlexList{ServerID: machine, Kind: "collection", Owner: "", RatingKey: it.RatingKey, SectionID: sec.Key,
			Title: it.Title, ListType: it.Subtype, Smart: it.IsSmart(), Content: it.Content, Summary: it.Summary,
			Data: plex.StableJSON(raw), Items: items}
		changed, err := e.st.SaveList(ctx, l)
		if err != nil {
			return err
		}
		n.lists++
		if changed {
			n.listsChanged++
		}
	}
	_, err = e.st.MarkCollectionsDeleted(ctx, machine, sec.Key, start)
	return err
}

// ---------- "mark as watched" detection ----------

// RunWatchScan looks for items whose lastViewedAt moved without a playback
// session: that is what pressing "Mark as watched" in Plex does. The play is
// recorded at lastViewedAt, which Plex sets to the moment of the press.
func (e *Engine) RunWatchScan(ctx context.Context) {
	every := e.cfg.PlexWatchScan
	e.log.Info("plex mark-as-watched scan started", "every", every)
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := e.ScanWatched(ctx); err != nil {
			e.log.Warn("mark-as-watched scan", "err", err)
		}
		t.Reset(every)
	}
}

func (e *Engine) ScanWatched(ctx context.Context) error {
	secs, err := e.plex.Sections(ctx)
	if err != nil {
		return err
	}
	users, err := e.PlexUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		for _, sec := range secs {
			typ := 0
			switch sec.Type {
			case "movie":
				typ = plex.TypeMovie
			case "show":
				typ = plex.TypeEpisode
			default:
				continue
			}
			if err := e.scanSection(ctx, u, sec, typ); err != nil {
				e.log.Warn("mark-as-watched scan", "user", u.Name, "section", sec.Title, "err", err)
			}
		}
	}
	return nil
}

func (e *Engine) scanSection(ctx context.Context, u PlexUser, sec plex.Section, typ int) error {
	key := "watchscan|" + u.Name + "|" + sec.Key
	v, _ := e.st.GetKV(ctx, key)
	checkpoint, _ := strconv.ParseInt(v, 10, 64)
	first := v == ""
	newest := checkpoint
	private := e.IsPrivateSection(sec.Title, sec.Key)
	err := u.Client.SectionItems(ctx, sec.Key, typ, "lastViewedAt:desc", 50, func(page []json.RawMessage) error {
		for _, raw := range page {
			it, err := plex.ParseLibItem(raw)
			if err != nil {
				continue
			}
			if it.LastViewedAt <= checkpoint {
				return errStop
			}
			if it.LastViewedAt > newest {
				newest = it.LastViewedAt
			}
			if first {
				// First run only sets the checkpoint; past views come from the
				// history import instead.
				return errStop
			}
			md := it.Metadata()
			m, ok, err := e.Resolve(ctx, &md)
			if err != nil || !ok {
				continue
			}
			ev := model.Event{Action: model.ActWatched, Media: m, User: u.Name, Owner: u.Owner, Progress: 100,
				At: time.Unix(it.LastViewedAt, 0), Source: "plex-mark", Private: private}
			if err := e.Ingest(ctx, ev, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return err
	}
	if first && newest < time.Now().Unix() {
		newest = time.Now().Unix()
	}
	if newest != checkpoint || first {
		return e.st.SetKV(ctx, key, strconv.FormatInt(newest, 10))
	}
	return nil
}
