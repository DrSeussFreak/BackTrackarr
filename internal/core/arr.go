package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BacklotTracker/internal/config"
	"github.com/DrSeussFreak/BacklotTracker/internal/store"
)

// Sonarr, Radarr and Whisparr (any number of each). BacklotTracker archives their
// libraries and settings, and can add a recommended title to one of them.
// Settings that hold credentials (download clients, indexers, notifications,
// import lists) are deliberately not archived.

var arrHTTP = &http.Client{Timeout: 60 * time.Second}

type arrClient struct{ inst config.ArrInstance }

func (a arrClient) do(ctx context.Context, method, p string, q url.Values, in, out any) error {
	u := a.inst.URL + "/api/v3" + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", a.inst.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := arrHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", a.inst.Name, p, resp.Status, truncate(string(b), 300))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

var errNotFound = errors.New("not found")

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// libraryPath is "/series" for Sonarr (and Sonarr-based Whisparr v2) and
// "/movie" for Radarr (and Radarr-based Whisparr v3).
func (a arrClient) libraryPath(ctx context.Context) (string, string, error) {
	switch a.inst.Kind {
	case "sonarr":
		return "/series", "series", nil
	case "radarr":
		return "/movie", "movie", nil
	}
	var probe []json.RawMessage
	if err := a.do(ctx, http.MethodGet, "/movie", url.Values{"pageSize": {"1"}}, nil, &probe); err == nil {
		return "/movie", "movie", nil
	}
	return "/series", "series", nil
}

func (e *Engine) ArrInstances() []config.ArrInstance { return e.cfg.Arr }

func (e *Engine) arr(name string) (arrClient, error) {
	for _, in := range e.cfg.Arr {
		if in.Name == name {
			return arrClient{in}, nil
		}
	}
	return arrClient{}, fmt.Errorf("no *arr instance named %q", name)
}

// RunArrArchiver snapshots every instance every ARR_ARCHIVE_HOURS.
func (e *Engine) RunArrArchiver(ctx context.Context) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.Jobs.Start(ctx, "archive ← sonarr/radarr", e.ArchiveArr)
		t.Reset(e.cfg.ArrArchiveEvery)
	}
}

var arrVolatile = []string{"statistics", "ratings", "popularity", "sizeOnDisk", "lastSearchTime"}

var arrSettings = []string{"/qualityprofile", "/customformat", "/rootfolder", "/tag", "/delayprofile", "/releaseprofile",
	"/languageprofile", "/config/naming", "/config/mediamanagement", "/qualitydefinition"}

func (e *Engine) ArchiveArr(ctx context.Context, report func(string)) error {
	var errs []string
	for _, in := range e.cfg.Arr {
		a := arrClient{in}
		start := time.Now().Unix()
		p, kind, err := a.libraryPath(ctx)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var items []map[string]json.RawMessage
		if err := a.do(ctx, http.MethodGet, p, nil, nil, &items); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		changed := 0
		for _, it := range items {
			for _, k := range arrVolatile {
				delete(it, k)
			}
			data, _ := json.Marshal(it)
			var f struct {
				ID        int64  `json:"id"`
				Title     string `json:"title"`
				Year      int    `json:"year"`
				TVDB      int    `json:"tvdbId"`
				TMDB      int    `json:"tmdbId"`
				IMDB      string `json:"imdbId"`
				Monitored bool   `json:"monitored"`
				Path      string `json:"path"`
			}
			_ = json.Unmarshal(data, &f)
			c, err := e.st.SaveArrItem(ctx, store.ArrItem{Instance: in.Name, ArrID: f.ID, Kind: kind, Title: f.Title, Year: f.Year,
				TVDB: f.TVDB, TMDB: f.TMDB, IMDB: f.IMDB, Monitored: f.Monitored, Path: f.Path, Data: data})
			if err != nil {
				return err
			}
			if c {
				changed++
			}
		}
		if _, err := e.st.MarkArrDeleted(ctx, in.Name, start); err != nil {
			return err
		}
		for _, sp := range arrSettings {
			var doc json.RawMessage
			if err := a.do(ctx, http.MethodGet, sp, nil, nil, &doc); err != nil {
				continue // not every app/version has every endpoint
			}
			if _, err := e.st.SaveArrConfig(ctx, in.Name, strings.TrimPrefix(sp, "/"), doc); err != nil {
				return err
			}
		}
		report(fmt.Sprintf("%s: %d %s, %d new or changed", in.Name, len(items), kind, changed))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// AddToArr adds a recommendation to an instance: looks it up by id (or
// title), picks the configured (or first) quality profile and root folder,
// and asks the instance to search for it.
func (e *Engine) AddToArr(ctx context.Context, recID int64, instance string) (string, error) {
	rec, err := e.st.GetRec(ctx, recID)
	if err != nil || rec == nil {
		return "", fmt.Errorf("recommendation %d not found", recID)
	}
	a, err := e.arr(instance)
	if err != nil {
		return "", err
	}
	p, kind, err := a.libraryPath(ctx)
	if err != nil {
		return "", err
	}
	if (rec.Kind == "show") != (kind == "series") {
		return "", fmt.Errorf("%s handles %s, but %q is a %s", instance, kind, rec.Title, rec.Kind)
	}
	terms := []string{}
	switch {
	case kind == "movie" && rec.TMDB != 0:
		terms = append(terms, "tmdb:"+strconv.Itoa(rec.TMDB))
	case kind == "movie" && rec.IMDB != "":
		terms = append(terms, "imdb:"+rec.IMDB)
	case kind == "series" && rec.TVDB != 0:
		terms = append(terms, "tvdb:"+strconv.Itoa(rec.TVDB))
	}
	title := rec.Title
	if rec.Year > 0 {
		title += " " + strconv.Itoa(rec.Year)
	}
	terms = append(terms, title)
	var found map[string]any
	for _, term := range terms {
		var res []map[string]any
		if err := a.do(ctx, http.MethodGet, p+"/lookup", url.Values{"term": {term}}, nil, &res); err != nil {
			return "", err
		}
		for _, r := range res {
			y, _ := r["year"].(float64)
			if rec.Year == 0 || int(y) == 0 || absInt(int(y)-rec.Year) <= 1 {
				found = r
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return "", fmt.Errorf("%s could not find %q", instance, rec.Title)
	}
	if id, _ := found["id"].(float64); id > 0 {
		_ = e.st.MarkRecAdded(ctx, recID, instance)
		return fmt.Sprintf("%q is already in %s", rec.Title, instance), nil
	}
	profile, root, err := a.defaults(ctx)
	if err != nil {
		return "", err
	}
	found["qualityProfileId"] = profile
	found["rootFolderPath"] = root
	found["monitored"] = true
	if kind == "movie" {
		found["minimumAvailability"] = "released"
		found["addOptions"] = map[string]any{"searchForMovie": true}
	} else {
		found["seasonFolder"] = true
		found["addOptions"] = map[string]any{"monitor": "all", "searchForMissingEpisodes": true}
		if a.inst.Kind == "sonarr" {
			found["languageProfileId"] = 1 // Sonarr v3 requires it; v4 ignores it
		}
	}
	if err := a.do(ctx, http.MethodPost, p, nil, found, nil); err != nil {
		return "", err
	}
	_ = e.st.MarkRecAdded(ctx, recID, instance)
	return fmt.Sprintf("added %q to %s", rec.Title, instance), nil
}

func (a arrClient) defaults(ctx context.Context) (int, string, error) {
	var profiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := a.do(ctx, http.MethodGet, "/qualityprofile", nil, nil, &profiles); err != nil {
		return 0, "", err
	}
	var roots []struct {
		Path string `json:"path"`
	}
	if err := a.do(ctx, http.MethodGet, "/rootfolder", nil, nil, &roots); err != nil {
		return 0, "", err
	}
	if len(profiles) == 0 || len(roots) == 0 {
		return 0, "", fmt.Errorf("%s has no quality profile or root folder set up", a.inst.Name)
	}
	pid := profiles[0].ID
	if want := a.inst.QualityProfile; want != "" {
		for _, p := range profiles {
			if strings.EqualFold(p.Name, want) || strconv.Itoa(p.ID) == want {
				pid = p.ID
			}
		}
	}
	root := roots[0].Path
	if a.inst.RootFolder != "" {
		root = a.inst.RootFolder
	}
	return pid, root, nil
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
