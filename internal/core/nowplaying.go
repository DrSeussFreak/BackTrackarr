package core

import (
	"context"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

const (
	nowPlayingEvery = 12 * time.Second
	// A webhook can arrive just before Plex lists the session, so a brand-new
	// row isn't removed for being missing from one check.
	nowPlayingGrace = 15 * time.Second
)

// RunNowPlaying keeps Now Playing true to Plex, because a stop webhook can be
// missed. It only asks Plex while something is showing.
func (e *Engine) RunNowPlaying(ctx context.Context) {
	t := time.NewTicker(nowPlayingEvery)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := e.SyncNowPlaying(cctx)
		cancel()
		if err != nil && !failing && ctx.Err() == nil {
			e.log.Warn("now playing: couldn't check Plex, keeping the current list", "err", err)
		}
		failing = err != nil
	}
}

func samePlexSession(r store.PlexSession, md *plex.Metadata) bool {
	if r.Player != "" && md.Player != nil && !strings.EqualFold(r.Player, md.Player.Title) {
		return false
	}
	return (r.PlexKey != "" && r.PlexKey == md.RatingKey) || (r.PlexGUID != "" && r.PlexGUID == md.GUID)
}

// SyncNowPlaying compares rows with Plex's current sessions. If Plex can't be
// reached nothing is removed, and the usual timeouts still apply.
func (e *Engine) SyncNowPlaying(ctx context.Context) error {
	if e.plex == nil {
		return nil
	}
	rows, err := e.st.PlexSessions(ctx)
	if err != nil {
		return err
	}
	active := false
	for _, r := range rows {
		active = active || r.Active()
	}
	if !active {
		return nil
	}
	live, err := e.plex.Sessions(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range rows {
		var found *plex.Metadata
		for i := range live {
			if samePlexSession(r, &live[i]) {
				found = &live[i]
				break
			}
		}
		switch {
		case found != nil:
			state := model.ActStart
			if found.Player != nil && strings.EqualFold(found.Player.State, "paused") {
				state = model.ActPause
			}
			err = e.st.UpdateLiveSession(ctx, r.Key, state, plex.Progress(found))
		case r.Active() && (r.PlexKey != "" || r.PlexGUID != "") && now.Sub(time.Unix(r.UpdatedAt, 0)) >= nowPlayingGrace:
			err = e.st.EndSession(ctx, r.Key)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
