package store

import (
	"context"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// PlexSession is a playback row from a Plex source, with what's needed to
// compare it against Plex's own list of current sessions.
type PlexSession struct {
	Key       string
	State     string
	Player    string
	PlexKey   string
	PlexGUID  string
	UpdatedAt int64
}

// Active reports whether this row currently shows in Now Playing.
func (p PlexSession) Active() bool {
	return p.State == model.ActStart || p.State == model.ActPause || p.State == model.ActWatched
}

// PlexSessions lists recent rows from Plex webhooks and the Plex poller.
func (s *Store) PlexSessions(ctx context.Context) ([]PlexSession, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT x.key, x.state, COALESCE(x.player,''), COALESCE(m.plex_key,''), COALESCE(m.plex_guid,''), x.updated_at
		FROM sessions x JOIN media m ON m.id=x.media_id WHERE x.source LIKE 'plex%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlexSession
	for rows.Next() {
		var p PlexSession
		if err := rows.Scan(&p.Key, &p.State, &p.Player, &p.PlexKey, &p.PlexGUID, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateLiveSession records what Plex says is happening right now.
func (s *Store) UpdateLiveSession(ctx context.Context, key, state string, progress float64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET state=?, progress=?, updated_at=? WHERE key=?`,
		state, progress, time.Now().Unix(), key)
	return err
}

// EndSession takes a row out of Now Playing. The row itself stays until
// routine cleanup, so a late event can't count a finished play twice.
func (s *Store) EndSession(ctx context.Context, key string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET state=? WHERE key=?`, model.ActStop, key)
	return err
}

// EndOtherPlexSessions clears anything else showing for this user on this
// player: a player can only play one thing at a time.
func (s *Store) EndOtherPlexSessions(ctx context.Context, user, player, keep string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET state=? WHERE user=? AND player=? AND key<>? AND source LIKE 'plex%' AND state IN (?,?,?)`,
		model.ActStop, user, player, keep, model.ActStart, model.ActPause, model.ActWatched)
	return err
}
