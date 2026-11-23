package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// ImportEverything reads plays, rewatches and ratings from every connected
// app. Everything here only reads from those apps, and running it again is
// safe. One app failing doesn't stop the others.
func (e *Engine) ImportEverything(ctx context.Context, report func(string)) error {
	var done, failed []string
	step := func(name string, fn func(func(string)) (string, error)) {
		if ctx.Err() != nil {
			return
		}
		report(name + ": starting")
		last := ""
		msg, err := fn(func(s string) {
			last = s
			report(name + ": " + s)
		})
		if msg == "" {
			msg = last
		}
		if err != nil {
			failed = append(failed, name+": "+err.Error())
			return
		}
		done = append(done, name+": "+msg)
	}
	if e.plex != nil {
		step("Plex history", func(r func(string)) (string, error) { return "", e.ImportPlex(ctx, r) })
		step("Plex ratings", func(r func(string)) (string, error) { return e.importPlexRatings(ctx, r) })
	}
	if len(e.historyPullers()) > 0 {
		step("Connected apps", func(r func(string)) (string, error) { return "", e.PullHistory(ctx, "", r) })
	}
	user := e.defaultUser(ctx)
	if t, ok := e.Sink("trakt").(*sinks.Trakt); ok && t.Ready(ctx) {
		step("Trakt history", func(r func(string)) (string, error) { return "", e.ImportTrakt(ctx, user, r) })
		step("Trakt ratings", func(r func(string)) (string, error) {
			n := 0
			err := t.ImportRatings(ctx, func(m model.Media, rating float64, at time.Time) error {
				saved, err := e.saveRating(ctx, m, user, rating, at)
				if saved {
					n++
				}
				return err
			})
			return fmt.Sprintf("%d ratings", n), err
		})
	}
	if s, ok := e.Sink("simkl").(*sinks.Simkl); ok && s.Ready(ctx) {
		step("Simkl history", func(r func(string)) (string, error) {
			added := 0
			_, err := s.ImportHistory(ctx, time.Time{}, func(m model.Media, at time.Time) error {
				if err := e.st.UpsertMedia(ctx, &m); err != nil {
					return err
				}
				ins, err := e.st.RecordPlay(ctx, m.ID, user, at, "simkl-import", "", false)
				if ins {
					added++
				}
				return err
			})
			return fmt.Sprintf("%d new plays", added), err
		})
		step("Simkl ratings", func(r func(string)) (string, error) {
			n := 0
			err := s.ImportRatings(ctx, func(m model.Media, rating float64, at time.Time) error {
				saved, err := e.saveRating(ctx, m, user, rating, at)
				if saved {
					n++
				}
				return err
			})
			return fmt.Sprintf("%d ratings", n), err
		})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(done)+len(failed) == 0 {
		return errors.New("no apps are connected yet")
	}
	report(strings.Join(done, "; "))
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

func (e *Engine) saveRating(ctx context.Context, m model.Media, user string, rating float64, at time.Time) (bool, error) {
	if err := retryBusy(ctx, func() error { return e.st.UpsertMedia(ctx, &m) }); err != nil {
		return false, err
	}
	var ts int64
	if !at.IsZero() {
		ts = at.Unix()
	}
	err := retryBusy(ctx, func() error { return e.st.SetRatingKeepNewest(ctx, m.ID, user, min(rating, 10), ts) })
	return err == nil, err
}

type plexRating struct {
	id     int64
	user   string
	rating float64
	at     int64
	kind   string
	title  string
	raw    []byte
}

// importPlexRatings reads each Plex user's ratings. Whole-show, season, album
// and artist ratings have no single play, so they're kept as records.
func (e *Engine) importPlexRatings(ctx context.Context, report func(string)) (string, error) {
	secs, err := e.plex.Sections(ctx)
	if err != nil {
		return "", err
	}
	users, err := e.PlexUsers(ctx)
	if err != nil {
		return "", err
	}
	rated, kept := 0, 0
	var problems []string
	for _, u := range users {
		for _, sec := range secs {
			for _, typ := range plex.SectionTypes(sec.Type) {
				err := u.Client.SectionItems(ctx, sec.Key, typ, "userRating:desc", 200, func(page []json.RawMessage) error {
					batch := make([]plexRating, 0, len(page))
					finished := false
					for _, raw := range page {
						it, err := plex.ParseLibItem(raw)
						if err != nil {
							continue
						}
						if it.UserRating <= 0 {
							finished = true // sorted highest first, so the rest are unrated
							break
						}
						r := plexRating{user: u.Name, rating: min(it.UserRating, 10), at: it.LastRatedAt}
						switch it.Type {
						case "movie", "episode", "track":
							md := it.Metadata()
							var m model.Media
							var ok bool
							if err := retryBusy(ctx, func() (err error) {
								m, ok, err = e.Resolve(ctx, &md)
								return err
							}); err != nil {
								return err
							}
							if !ok {
								continue
							}
							r.id = m.ID
						default:
							r.kind, r.title = "rating-"+it.Type, it.Title
							r.raw, _ = json.Marshal(map[string]any{"user": u.Name, "type": it.Type, "title": it.Title,
								"parent": it.ParentTitle, "show": it.GrandparentTitle, "index": it.Index, "guid": it.GUID,
								"rating": it.UserRating, "rated_at": it.LastRatedAt})
						}
						batch = append(batch, r)
					}
					if err := e.st.InTx(ctx, func(ctx context.Context) error {
						return savePlexRatings(ctx, e.st, batch)
					}); err != nil {
						return err
					}
					for _, r := range batch {
						if r.id != 0 {
							rated++
						} else {
							kept++
						}
					}
					report(fmt.Sprintf("%d ratings, %d show or album ratings", rated, kept))
					if finished {
						return errStop
					}
					return nil
				})
				if err != nil && !errors.Is(err, errStop) {
					if ctx.Err() != nil {
						return "", ctx.Err()
					}
					// A shared user may not have access to every library.
					e.log.Warn("plex ratings", "user", u.Name, "section", sec.Title, "err", err)
					problems = append(problems, u.Name+"/"+sec.Title)
				}
			}
		}
	}
	msg := fmt.Sprintf("%d ratings, %d show or album ratings", rated, kept)
	if len(problems) == len(users)*len(secs) && len(problems) > 0 {
		return msg, errors.New("couldn't read ratings from any library")
	}
	if len(problems) > 0 {
		msg += fmt.Sprintf(", %d libraries couldn't be read", len(problems))
	}
	return msg, nil
}

func savePlexRatings(ctx context.Context, st *store.Store, batch []plexRating) error {
	for _, r := range batch {
		if r.id != 0 {
			if err := st.SetRatingKeepNewest(ctx, r.id, r.user, r.rating, r.at); err != nil {
				return err
			}
			continue
		}
		if _, err := st.SaveImportExtra(ctx, "plex-import", r.kind, r.title, r.raw); err != nil {
			return err
		}
	}
	return nil
}
