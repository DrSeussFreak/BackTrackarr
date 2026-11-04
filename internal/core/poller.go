package core

import (
	"context"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
)

type pollState struct {
	ev        model.Event
	state     string
	scrobbled bool
}

// RunPoller watches Plex /status/sessions. It works without Plex Pass and
// produces the same events as webhooks. Enable either this or webhooks.
func (e *Engine) RunPoller(ctx context.Context, every time.Duration) {
	e.log.Info("plex session poller started", "every", every)
	sessions := map[string]*pollState{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := e.plex.Sessions(ctx)
		if err != nil {
			e.log.Warn("plex sessions", "err", err)
			continue
		}
		seen := map[string]bool{}
		for i := range list {
			md := &list[i]
			if md.Type != "movie" && md.Type != "episode" && md.Type != "track" {
				continue
			}
			key := md.SessionKey + "|" + md.RatingKey
			seen[key] = true
			state := model.ActStart
			if md.Player != nil && strings.EqualFold(md.Player.State, "paused") {
				state = model.ActPause
			}
			ps, ok := sessions[key]
			if !ok {
				m, ok, err := e.Resolve(ctx, md)
				if err != nil || !ok {
					continue
				}
				ev := model.Event{Media: m, Source: "plex-poll",
					Private: e.IsPrivateSection(md.LibrarySectionTitle, md.LibrarySectionID.String())}
				if md.User != nil {
					ev.User = md.User.Title
					ev.Owner = md.User.ID.String() == "1"
				}
				if md.Player != nil {
					ev.Player = md.Player.Title
				}
				ps = &pollState{ev: ev}
				sessions[key] = ps
			}
			ps.ev.Progress = plex.Progress(md)
			if ps.state != state {
				ps.state = state
				e.emit(ctx, ps.ev, state)
			}
			if !ps.scrobbled && ps.ev.Progress >= e.watchedAt(ps.ev.Media.Kind) {
				ps.scrobbled = true
				e.emit(ctx, ps.ev, model.ActWatched)
			}
		}
		for key, ps := range sessions {
			if !seen[key] {
				delete(sessions, key)
				e.emit(ctx, ps.ev, model.ActStop)
			}
		}
	}
}

func (e *Engine) emit(ctx context.Context, ev model.Event, action string) {
	ev.Action = action
	ev.At = time.Now()
	if err := e.Ingest(ctx, ev, false); err != nil {
		e.log.Error("ingest", "action", action, "title", ev.Media.Display(), "err", err)
	}
}
