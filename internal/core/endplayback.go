package core

import (
	"context"
	"errors"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// EndPlaybackError is a problem worth showing the user as written.
type EndPlaybackError string

func (e EndPlaybackError) Error() string { return string(e) }

// EndResult says what pressing End session did.
type EndResult struct {
	Found   bool // the session was showing
	Stopped bool // Plex was told to stop the stream
}

// CanStopPlayback reports whether End session can stop this source's stream.
func (e *Engine) CanStopPlayback(source string) bool {
	return e.plex != nil && strings.HasPrefix(source, "plex") && source != plexMarkSource
}

// EndPlayback stops one Plex stream and takes it off Now Playing. Other
// sources can only be removed from the list. If Plex can't stop it, the
// session stays showing so the screen never claims something that didn't
// happen.
func (e *Engine) EndPlayback(ctx context.Context, key string) (EndResult, error) {
	var row *store.PlexSession
	if e.plex != nil {
		rows, err := e.st.PlexSessions(ctx)
		if err != nil {
			return EndResult{}, err
		}
		for i := range rows {
			if rows[i].Key == key && rows[i].Active() {
				row = &rows[i]
				break
			}
		}
	}
	if row == nil {
		found, err := e.st.HideSession(ctx, key)
		return EndResult{Found: found}, err
	}

	live, err := e.plex.Sessions(ctx)
	if err != nil {
		if errors.Is(err, plex.ErrUnauthorized) {
			return EndResult{Found: true}, EndPlaybackError("Plex rejected the saved sign-in. Reconnect Plex on the Setup page.")
		}
		return EndResult{Found: true}, EndPlaybackError("Couldn't reach Plex, so the stream is still playing. Try again.")
	}
	var matches []*plex.Metadata
	for i := range live {
		md := &live[i]
		if !samePlexSession(*row, md) {
			continue
		}
		if row.User != "" && md.User != nil && md.User.Title != "" && !strings.EqualFold(row.User, md.User.Title) {
			continue // the same title on another person's stream
		}
		matches = append(matches, md)
	}
	switch {
	case len(matches) == 0: // it has already stopped in Plex
		found, err := e.st.HideSession(ctx, key)
		return EndResult{Found: found}, err
	case len(matches) > 1:
		return EndResult{Found: true}, EndPlaybackError("Plex shows more than one matching stream, so nothing was stopped.")
	case matches[0].Session == nil || matches[0].Session.ID == "":
		return EndResult{Found: true}, EndPlaybackError("Plex didn't give an ID for this stream, so it can't be stopped from here.")
	}

	if err := e.plex.TerminateSession(ctx, matches[0].Session.ID, "Stopped from BackTrackarr"); err != nil {
		e.log.Warn("couldn't stop a Plex stream", "err", err)
		if errors.Is(err, plex.ErrUnauthorized) {
			return EndResult{Found: true}, EndPlaybackError("Plex refused. Stopping streams needs Plex Pass and the server owner's sign-in.")
		}
		return EndResult{Found: true}, EndPlaybackError("Plex couldn't stop this stream. Try again.")
	}
	e.log.Info("stopped a Plex stream", "user", row.User, "player", row.Player)
	found, err := e.st.HideSession(ctx, key)
	return EndResult{Found: found, Stopped: true}, err
}
