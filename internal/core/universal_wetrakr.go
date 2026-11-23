package core

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// WeTrakr exports one CSV per kind of data. Its tmdb_id belongs to the row's
// own item, so an episode's show is in show_tmdb_id. A watched row with no
// date stays a kept record: inventing a date would corrupt the timeline.

const weTrakrSource = "wetrakr-import"

func headerSet(header []string) map[string]int {
	out := make(map[string]int, len(header))
	for i, h := range header {
		out[normHeader(h)] = i
	}
	return out
}

func isWeTrakr(col map[string]int) bool {
	_, ok := col["wetrakrid"]
	return ok
}

// isWeTrakrProfile matches profile.csv, which holds account details such as
// an email address and has nothing to do with history. It is never stored.
func isWeTrakrProfile(col map[string]int) bool {
	for _, k := range []string{"username", "email", "joinedat", "plan"} {
		if _, ok := col[k]; !ok {
			return false
		}
	}
	return true
}

func (u *uniRun) readWeTrakr(ctx context.Context, cr *csv.Reader, header []string, col map[string]int) error {
	u.formats["wetrakr"] = true
	_, tracklog := col["trackedat"]
	_, ratings := col["ratedat"]
	_, lists := col["listname"]
	_, notes := col["text"]
	fileKind := "favorite"
	switch {
	case lists:
		fileKind = "list"
	case notes:
		fileKind = "note"
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			u.res.Skipped++
			continue
		}
		get := func(k string) string {
			if i, ok := col[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		keep := func(kind string) error {
			raw := make(map[string]string, len(header))
			for i, h := range header {
				if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
					raw[strings.TrimSpace(h)] = rec[i]
				}
			}
			b, _ := json.Marshal(raw)
			return u.emit(ctx, uniRec{source: weTrakrSource, kind: kind, title: get("title"), raw: b})
		}
		var r error
		switch {
		case tracklog:
			r = u.weTrakrTrack(ctx, get, keep)
		case ratings:
			r = u.weTrakrRating(ctx, get, keep)
		default:
			r = keep(fileKind)
		}
		if r != nil {
			return r
		}
	}
}

func (u *uniRun) weTrakrTrack(ctx context.Context, get func(string) string, keep func(string) error) error {
	typ, status := strings.ToLower(get("type")), strings.ToLower(get("status"))
	if status != "watched" {
		return keep("status-" + status)
	}
	m, ok := weTrakrMedia(get)
	if !ok {
		return keep(typ + "-status")
	}
	at := shelfTime(get("trackedat"))
	if at == 0 {
		return keep("watched-undated")
	}
	return u.emit(ctx, uniRec{media: &m, at: at, source: weTrakrSource})
}

func (u *uniRun) weTrakrRating(ctx context.Context, get func(string) string, keep func(string) error) error {
	rating, _ := strconv.ParseFloat(get("rating"), 64)
	m, ok := weTrakrMedia(get)
	if !ok || rating <= 0 {
		return keep("rating-" + strings.ToLower(get("type")))
	}
	return u.emit(ctx, uniRec{media: &m, rating: rating, ratedAt: shelfTime(get("ratedat")), source: weTrakrSource})
}

func weTrakrMedia(get func(string) string) (model.Media, bool) {
	num := func(k string) int { n, _ := strconv.Atoi(get(k)); return n }
	ids := model.IDs{TMDB: num("tmdbid"), IMDB: get("imdbid")}
	year := num("year")
	switch strings.ToLower(get("type")) {
	case "movie":
		if get("title") == "" {
			return model.Media{}, false
		}
		return model.Media{Kind: model.Movie, Title: get("title"), Year: year, IDs: ids}, true
	case "episode":
		show := get("showtitle")
		if show == "" || get("seasonnumber") == "" || get("episodenumber") == "" {
			return model.Media{}, false
		}
		title := get("title")
		if title == "" {
			title = "Episode " + get("episodenumber")
		}
		return model.Media{Kind: model.Episode, ShowTitle: show, Title: title, Season: num("seasonnumber"),
			Episode: num("episodenumber"), Year: year, IDs: ids, ShowIDs: model.IDs{TMDB: num("showtmdbid")}}, true
	}
	return model.Media{}, false
}
