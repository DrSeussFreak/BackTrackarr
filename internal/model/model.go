// Package model holds the types shared across the app.
package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const Version = "0.1.0"

const (
	Movie   = "movie"
	Episode = "episode"
	Track   = "track" // music: ShowTitle is the artist, Album the album, Season the disc, Episode the track number
)

// Event actions.
const (
	ActStart   = "start"
	ActPause   = "pause"
	ActStop    = "stop"
	ActWatched = "watched"
	ActRate    = "rate"
)

type IDs struct {
	IMDB  string `json:"imdb,omitempty"`
	TMDB  int    `json:"tmdb,omitempty"`
	TVDB  int    `json:"tvdb,omitempty"`
	Trakt int    `json:"trakt,omitempty"`
	MBID  string `json:"mbid,omitempty"` // MusicBrainz, for tracks
}

func (i IDs) Empty() bool {
	return i.IMDB == "" && i.TMDB == 0 && i.TVDB == 0 && i.Trakt == 0 && i.MBID == ""
}

// Merge fills fields of i that are empty with values from o.
func (i *IDs) Merge(o IDs) {
	if i.IMDB == "" {
		i.IMDB = o.IMDB
	}
	if i.TMDB == 0 {
		i.TMDB = o.TMDB
	}
	if i.TVDB == 0 {
		i.TVDB = o.TVDB
	}
	if i.Trakt == 0 {
		i.Trakt = o.Trakt
	}
	if i.MBID == "" {
		i.MBID = o.MBID
	}
}

// Strings returns the external (non-trakt) ids as strings.
func (i IDs) Strings() map[string]string {
	m := map[string]string{}
	if i.IMDB != "" {
		m["imdb"] = i.IMDB
	}
	if i.TMDB != 0 {
		m["tmdb"] = strconv.Itoa(i.TMDB)
	}
	if i.TVDB != 0 {
		m["tvdb"] = strconv.Itoa(i.TVDB)
	}
	return m
}

// Media is a movie, a single episode, or a music track.
type Media struct {
	ID        int64  `json:"id,omitempty"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Year      int    `json:"year,omitempty"`
	ShowTitle string `json:"show_title,omitempty"`
	Album     string `json:"album,omitempty"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	DurationS int    `json:"duration_s,omitempty"`
	IDs       IDs    `json:"ids"`
	ShowIDs   IDs    `json:"show_ids"`
	PlexGUID  string `json:"plex_guid,omitempty"`
	PlexKey   string `json:"plex_key,omitempty"`
}

func (m Media) Display() string {
	if m.Kind == Track {
		if m.Album != "" {
			return fmt.Sprintf("%s · %s · %s", m.ShowTitle, m.Album, m.Title)
		}
		return fmt.Sprintf("%s · %s", m.ShowTitle, m.Title)
	}
	if m.Kind == Episode {
		return fmt.Sprintf("%s · S%02dE%02d · %s", m.ShowTitle, m.Season, m.Episode, m.Title)
	}
	if m.Year > 0 {
		return fmt.Sprintf("%s (%d)", m.Title, m.Year)
	}
	return m.Title
}

// Event is one normalized playback event.
type Event struct {
	Action   string          `json:"action"`
	Media    Media           `json:"media"`
	User     string          `json:"user"`
	Owner    bool            `json:"owner"`
	Player   string          `json:"player,omitempty"`
	Progress float64         `json:"progress"`
	Rating   float64         `json:"rating,omitempty"`
	At       time.Time       `json:"at"`
	Source   string          `json:"source"`
	Private  bool            `json:"private,omitempty"` // from a private library: stored, never sent
	Raw      json.RawMessage `json:"raw,omitempty"`     // original Plex webhook JSON, used only by relay sinks
}
