package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Auto-delete removes a file through the Sonarr, Radarr or Whisparr that owns
// it, only after every user assigned to that title has watched it. Nothing is
// deleted unless the master switch and that title's rule are both on.

const (
	deleteMasterKey = "autodelete|enabled"
	deleteScopeKey  = "autodelete|scope"
	deleteMaxPerRun = 25 // a wrong rule can only do limited damage in one run
)

// DeleteScope limits auto-delete to chosen servers and libraries. Mode is
// all, include or exclude.
type DeleteScope struct {
	ServerMode  string   `json:"server_mode"`
	Servers     []string `json:"servers"`
	LibraryMode string   `json:"library_mode"`
	Libraries   []string `json:"libraries"`
}

func scopeAllows(mode string, list []string, vals []string) bool {
	if mode != "include" && mode != "exclude" {
		return true
	}
	hit := false
	for _, v := range vals {
		for _, l := range list {
			if strings.EqualFold(v, l) {
				hit = true
			}
		}
	}
	if mode == "exclude" {
		// A title whose library is unknown can't be proven outside the list.
		return !hit && len(vals) > 0
	}
	return hit
}

// AutoDeleteOn reports the master switch, which is off until turned on.
func (e *Engine) AutoDeleteOn(ctx context.Context) bool {
	v, _ := e.st.GetKV(ctx, deleteMasterKey)
	return v == "1"
}

// SetAutoDelete turns the master switch on or off.
func (e *Engine) SetAutoDelete(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return e.st.SetKV(ctx, deleteMasterKey, v)
}

// AutoDeleteScope returns the saved server and library scope.
func (e *Engine) AutoDeleteScope(ctx context.Context) DeleteScope {
	var sc DeleteScope
	if v, _ := e.st.GetKV(ctx, deleteScopeKey); v != "" {
		_ = json.Unmarshal([]byte(v), &sc)
	}
	if sc.ServerMode == "" {
		sc.ServerMode = "all"
	}
	if sc.LibraryMode == "" {
		sc.LibraryMode = "all"
	}
	return sc
}

// SetAutoDeleteScope saves the server and library scope.
func (e *Engine) SetAutoDeleteScope(ctx context.Context, sc DeleteScope) error {
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	return e.st.SetKV(ctx, deleteScopeKey, string(b))
}

// DeleteCandidate is one file that has met its rule.
type DeleteCandidate struct {
	Instance   string
	Kind       string
	ArrID      int64
	Title      string
	Item       string // S01E02, or the movie's title
	FileID     int64
	EpisodeIDs []int64
	Users      []string
	LastWatch  int64
}

func allWatched(users []string, got map[string]int64, cutoff int64) (int64, bool) {
	var last int64
	for _, u := range users {
		at, ok := got[strings.ToLower(u)]
		if !ok || at > cutoff {
			return 0, false
		}
		last = max(last, at)
	}
	return last, len(users) > 0
}

// PreviewAutoDelete lists what would be deleted now. It changes nothing.
func (e *Engine) PreviewAutoDelete(ctx context.Context) ([]DeleteCandidate, []string, error) {
	rules, err := e.st.ActiveDeleteRules(ctx)
	if err != nil {
		return nil, nil, err
	}
	scope := e.AutoDeleteScope(ctx)
	var out []DeleteCandidate
	var problems []string
	for _, r := range rules {
		if !scopeAllows(scope.ServerMode, scope.Servers, []string{r.Instance}) {
			continue
		}
		if scope.LibraryMode == "include" || scope.LibraryMode == "exclude" {
			libs, err := e.st.TitleLibraries(ctx, r)
			if err != nil {
				return nil, nil, err
			}
			if !scopeAllows(scope.LibraryMode, scope.Libraries, libs) {
				continue
			}
		}
		a, err := e.arr(r.Instance)
		if err != nil {
			problems = append(problems, r.Title+": "+r.Instance+" is no longer configured")
			continue
		}
		if busy, err := e.st.BeingWatched(ctx, r); err != nil {
			return nil, nil, err
		} else if busy {
			continue
		}
		cutoff := time.Now().Add(-time.Duration(r.GraceDays) * 24 * time.Hour).Unix()
		var found []DeleteCandidate
		if r.Kind == "series" {
			found, err = e.seriesCandidates(ctx, a, r, cutoff)
		} else {
			found, err = e.movieCandidates(ctx, a, r, cutoff)
		}
		if err != nil {
			problems = append(problems, r.Title+": couldn't read it from "+r.Instance)
			continue
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastWatch < out[j].LastWatch })
	return out, problems, nil
}

func (e *Engine) seriesCandidates(ctx context.Context, a arrClient, r store.DeleteRule, cutoff int64) ([]DeleteCandidate, error) {
	var eps []struct {
		ID            int64 `json:"id"`
		SeasonNumber  int   `json:"seasonNumber"`
		EpisodeNumber int   `json:"episodeNumber"`
		EpisodeFileID int64 `json:"episodeFileId"`
		HasFile       bool  `json:"hasFile"`
	}
	if err := a.do(ctx, http.MethodGet, "/episode", url.Values{"seriesId": {strconv.FormatInt(r.ArrID, 10)}}, nil, &eps); err != nil {
		return nil, err
	}
	watches, err := e.st.EpisodeWatches(ctx, r)
	if err != nil {
		return nil, err
	}
	type group struct {
		ids    []int64
		labels []string
		last   int64
		ok     bool
	}
	files := map[int64]*group{}
	for _, ep := range eps {
		if !ep.HasFile || ep.EpisodeFileID == 0 {
			continue
		}
		g := files[ep.EpisodeFileID]
		if g == nil {
			g = &group{ok: true}
			files[ep.EpisodeFileID] = g
		}
		// A double episode is one file, so everyone must have seen both.
		last, ok := allWatched(r.Users, watches[[2]int{ep.SeasonNumber, ep.EpisodeNumber}], cutoff)
		g.ok = g.ok && ok
		g.last = max(g.last, last)
		g.ids = append(g.ids, ep.ID)
		g.labels = append(g.labels, fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber))
	}
	var out []DeleteCandidate
	for id, g := range files {
		if g.ok {
			out = append(out, DeleteCandidate{Instance: r.Instance, Kind: "series", ArrID: r.ArrID, Title: r.Title,
				Item: strings.Join(g.labels, "+"), FileID: id, EpisodeIDs: g.ids, Users: r.Users, LastWatch: g.last})
		}
	}
	return out, nil
}

func (e *Engine) movieCandidates(ctx context.Context, a arrClient, r store.DeleteRule, cutoff int64) ([]DeleteCandidate, error) {
	var mv struct {
		HasFile   bool `json:"hasFile"`
		MovieFile *struct {
			ID int64 `json:"id"`
		} `json:"movieFile"`
	}
	if err := a.do(ctx, http.MethodGet, "/movie/"+strconv.FormatInt(r.ArrID, 10), nil, nil, &mv); err != nil {
		return nil, err
	}
	if !mv.HasFile || mv.MovieFile == nil || mv.MovieFile.ID == 0 {
		return nil, nil
	}
	watches, err := e.st.MovieWatches(ctx, r)
	if err != nil {
		return nil, err
	}
	last, ok := allWatched(r.Users, watches, cutoff)
	if !ok {
		return nil, nil
	}
	return []DeleteCandidate{{Instance: r.Instance, Kind: "movie", ArrID: r.ArrID, Title: r.Title, Item: r.Title,
		FileID: mv.MovieFile.ID, Users: r.Users, LastWatch: last}}, nil
}

// RunAutoDeleteOnce deletes what has met its rule, if the master switch is on.
func (e *Engine) RunAutoDeleteOnce(ctx context.Context, report func(string)) error {
	if !e.AutoDeleteOn(ctx) {
		report("auto-delete is off, so nothing was deleted")
		return nil
	}
	list, problems, err := e.PreviewAutoDelete(ctx)
	if err != nil {
		return err
	}
	deleted := 0
	var failed []string
	for _, c := range list {
		if deleted >= deleteMaxPerRun {
			break
		}
		a, err := e.arr(c.Instance)
		if err != nil {
			continue
		}
		// Unmonitor first, or Sonarr or Radarr would download it again.
		// If that fails, the file is left alone.
		if c.Kind == "series" {
			err = a.do(ctx, http.MethodPut, "/episode/monitor", nil, map[string]any{"episodeIds": c.EpisodeIDs, "monitored": false}, nil)
			if err == nil {
				err = a.do(ctx, http.MethodDelete, "/episodefile/"+strconv.FormatInt(c.FileID, 10), nil, nil, nil)
			}
		} else {
			err = a.do(ctx, http.MethodPut, "/movie/editor", nil, map[string]any{"movieIds": []int64{c.ArrID}, "monitored": false}, nil)
			if err == nil {
				err = a.do(ctx, http.MethodDelete, "/moviefile/"+strconv.FormatInt(c.FileID, 10), nil, nil, nil)
			}
		}
		status := "deleted"
		if err != nil {
			status = "failed"
			failed = append(failed, c.Title+" "+c.Item)
			e.log.Warn("auto-delete", "title", c.Title, "item", c.Item, "err", err)
		} else {
			deleted++
			e.log.Info("auto-delete removed a file", "instance", c.Instance, "title", c.Title, "item", c.Item)
		}
		_ = e.st.LogDelete(ctx, store.DeleteLogRow{Instance: c.Instance, Title: c.Title, Item: c.Item, Users: strings.Join(c.Users, ", "), Status: status})
	}
	msg := fmt.Sprintf("deleted %d of %d ready", deleted, len(list))
	if len(problems) > 0 {
		msg += fmt.Sprintf(", %d titles couldn't be checked", len(problems))
	}
	report(msg)
	if len(failed) > 0 {
		return errors.New("couldn't delete: " + strings.Join(failed, ", "))
	}
	return nil
}

// RunAutoDelete checks hourly. It does nothing while the master switch is off.
func (e *Engine) RunAutoDelete(ctx context.Context) {
	t := time.NewTimer(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.AutoDeleteOn(ctx) {
			e.Jobs.Start(ctx, "auto-delete", e.RunAutoDeleteOnce)
		}
		t.Reset(time.Hour)
	}
}
