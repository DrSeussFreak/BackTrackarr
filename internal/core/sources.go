package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// Native history imports from other trackers. All of them only read.

// HistorySource is a connected app whose history can be pulled in.
type HistorySource struct {
	Name, Label string
	Last        int64
	Err         string
}

func (e *Engine) historyPullers() map[string]func(context.Context, *uniRun) error {
	c := e.cfg
	out := map[string]func(context.Context, *uniRun) error{}
	if c.TautulliURL != "" && c.TautulliKey != "" {
		out["tautulli"] = e.pullTautulli
	}
	if c.FloppyURL != "" && c.FloppyToken != "" {
		out["floppy"] = e.pullFloppy
	}
	if c.CrossWatchURL != "" && c.CrossWatchToken != "" {
		out["crosswatch"] = e.pullCrossWatch
	}
	if c.PlektURL != "" && c.PlektKey != "" {
		out["plekt"] = e.pullPlekt
	}
	return out
}

var historyLabels = map[string]string{"tautulli": "Tautulli", "floppy": "Floppy", "crosswatch": "CrossWatch", "plekt": "Plekt"}

// HistorySources lists the connected apps.
func (e *Engine) HistorySources(ctx context.Context) []HistorySource {
	var out []HistorySource
	for name := range e.historyPullers() {
		s := HistorySource{Name: name, Label: historyLabels[name]}
		v, _ := e.st.GetKV(ctx, "source|"+name+"|at")
		s.Last, _ = strconv.ParseInt(v, 10, 64)
		s.Err, _ = e.st.GetKV(ctx, "source|"+name+"|err")
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Label < out[b].Label })
	return out
}

// PullHistory imports from one connected app, or all of them when name is
// empty. Re-importing is safe. One failure doesn't stop the others.
func (e *Engine) PullHistory(ctx context.Context, name string, report func(string)) error {
	pullers := e.historyPullers()
	names := make([]string, 0, len(pullers))
	for n := range pullers {
		if name == "" || n == name {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return errors.New("that app isn't connected")
	}
	user := e.defaultUser(ctx)
	var failed, done []string
	for _, n := range names {
		report("reading " + historyLabels[n])
		u := &uniRun{e: e, user: user, res: &UniversalResult{}, formats: map[string]bool{}, report: report, source: n + "-import"}
		err := pullers[n](ctx, u)
		if ferr := u.flush(ctx); err == nil {
			err = ferr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := ""
		if err != nil {
			msg = err.Error()
			failed = append(failed, historyLabels[n]+": "+msg)
		} else {
			_ = e.st.SetKV(ctx, "source|"+n+"|at", strconv.FormatInt(time.Now().Unix(), 10))
			done = append(done, historyLabels[n]+": "+u.res.String())
		}
		_ = e.st.SetKV(ctx, "source|"+n+"|err", msg)
	}
	if len(done) > 0 {
		report(strings.Join(done, "; "))
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// RunHistorySources refreshes connected apps once a day.
func (e *Engine) RunHistorySources(ctx context.Context) {
	t := time.NewTimer(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := e.PullHistory(ctx, "", func(string) {}); err != nil && ctx.Err() == nil {
			e.log.Warn("history sources", "err", err)
		}
		t.Reset(24 * time.Hour)
	}
}

// ---------- Tautulli ----------

func (e *Engine) pullTautulli(ctx context.Context, u *uniRun) error {
	const size = 1000
	for start := 0; ; start += size {
		q := url.Values{"apikey": {e.cfg.TautulliKey}, "cmd": {"get_history"}, "start": {strconv.Itoa(start)}, "length": {strconv.Itoa(size)},
			"order_column": {"date"}, "order_dir": {"asc"}, "grouping": {"0"}}
		var res struct {
			Response struct {
				Result  string `json:"result"`
				Message string `json:"message"`
				Data    struct {
					RecordsFiltered int              `json:"recordsFiltered"`
					Data            []map[string]any `json:"data"`
				} `json:"data"`
			} `json:"response"`
		}
		if err := shelfGetJSON(ctx, e.cfg.TautulliURL+"/api/v2?"+q.Encode(), nil, &res); err != nil {
			return err
		}
		if res.Response.Result != "success" {
			return errors.New("Tautulli refused the request: check the API key")
		}
		for _, row := range res.Response.Data.Data {
			if err := u.tautulliRow(ctx, row); err != nil {
				return err
			}
		}
		if len(res.Response.Data.Data) < size {
			return nil
		}
	}
}

func (u *uniRun) tautulliRow(ctx context.Context, row map[string]any) error {
	at := int64(uNum(row, "started", "date"))
	if at <= 0 {
		return nil
	}
	typ := uStr(row, "media_type")
	kind := model.Movie
	switch typ {
	case "movie":
	case "episode":
		kind = model.Episode
	case "track":
		kind = model.Track
	default:
		return nil
	}
	if uNum(row, "watched_status") < 1 && uNum(row, "percent_complete") < u.e.watchedAt(kind) {
		return nil // started, but not finished
	}
	year := int(uNum(row, "year"))
	m := model.Media{Kind: kind, Title: uStr(row, "title"), Year: year}
	switch kind {
	case model.Episode:
		m.ShowTitle, m.Season, m.Episode = uStr(row, "grandparent_title"), int(uNum(row, "parent_media_index")), int(uNum(row, "media_index"))
		if m.ShowTitle == "" {
			return nil
		}
	case model.Track:
		m.ShowTitle, m.Album = uStr(row, "grandparent_title"), uStr(row, "parent_title")
	}
	if m.Title == "" {
		return nil
	}
	return u.emit(ctx, uniRec{media: &m, at: at, source: u.source, user: uStr(row, "user"),
		private: u.e.IsPrivateSection(uStr(row, "library_name"), "")})
}

// ---------- Floppy ----------

func (e *Engine) pullFloppy(ctx context.Context, u *uniRun) error {
	tmp, err := os.CreateTemp("", "backtrackarr-floppy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.FloppyURL+"/api/v1/export/csv", nil)
	if err != nil {
		return errors.New("bad Floppy address")
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.FloppyToken)
	c := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: shelfHTTP.CheckRedirect}
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("couldn't reach Floppy")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return shelfStatusError{resp.StatusCode}
	}
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, uniMaxEntry)); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return u.readFile(ctx, tmp, "floppy.csv")
}

// ---------- CrossWatch ----------

func (e *Engine) pullCrossWatch(ctx context.Context, u *uniRun) error {
	hdr := map[string]string{"Authorization": "Bearer " + e.cfg.CrossWatchToken}
	var list struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := shelfGetJSON(ctx, e.cfg.CrossWatchURL+"/api/snapshots/list", hdr, &list); err != nil {
		return fmt.Errorf("snapshots: %w", err)
	}
	// Snapshot paths sort by time, so keep the newest per provider and feature.
	newest := map[string]string{}
	for _, s := range list.Snapshots {
		p, feature := uStr(s, "path"), strings.ToLower(uStr(s, "feature"))
		if p == "" || (feature != "history" && feature != "ratings" && feature != "all") {
			continue
		}
		k := strings.ToLower(uStr(s, "provider")) + "|" + uStr(s, "instance") + "|" + feature
		if p > newest[k] {
			newest[k] = p
		}
	}
	if len(newest) == 0 {
		return errors.New("CrossWatch has no history snapshots yet: create one in CrossWatch first")
	}
	read := func(p string) (map[string]any, error) {
		var res struct {
			Snapshot map[string]any `json:"snapshot"`
		}
		err := shelfGetJSON(ctx, e.cfg.CrossWatchURL+"/api/snapshots/read?"+url.Values{"path": {p}}.Encode(), hdr, &res)
		return res.Snapshot, err
	}
	seen := map[string]bool{}
	var walk func(p string) error
	walk = func(p string) error {
		if seen[p] {
			return nil
		}
		seen[p] = true
		snap, err := read(p)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		if kids, ok := snap["children"].([]any); ok {
			for _, k := range kids {
				if km, ok := k.(map[string]any); ok {
					if f := strings.ToLower(uStr(km, "feature")); uStr(km, "path") != "" && (f == "history" || f == "ratings") {
						if err := walk(uStr(km, "path")); err != nil {
							return err
						}
					}
				}
			}
		}
		hint := "crosswatch/" + strings.ToLower(uStr(snap, "feature"))
		switch items := snap["items"].(type) {
		case map[string]any:
			for _, v := range items {
				if err := u.jsonItem(ctx, v, hint); err != nil {
					return err
				}
			}
		case []any:
			for _, v := range items {
				if err := u.jsonItem(ctx, v, hint); err != nil {
					return err
				}
			}
		}
		return nil
	}
	paths := make([]string, 0, len(newest))
	for _, p := range newest {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := walk(p); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Plekt ----------

func (e *Engine) pullPlekt(ctx context.Context, u *uniRun) error {
	hdr := map[string]string{"X-Api-Key": e.cfg.PlektKey}
	cursor := ""
	for page := 0; page < 100000; page++ {
		q := url.Values{"collection": {"watchHistory"}, "limit": {"500"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var res struct {
			Documents []struct {
				Data map[string]any `json:"data"`
			} `json:"documents"`
			NextCursor string `json:"nextCursor"`
			HasMore    bool   `json:"hasMore"`
		}
		if err := shelfGetJSON(ctx, e.cfg.PlektURL+"/api/backup/export?"+q.Encode(), hdr, &res); err != nil {
			return err
		}
		for _, d := range res.Documents {
			if err := u.jsonItem(ctx, d.Data, "plekt/watchhistory"); err != nil {
				return err
			}
		}
		if !res.HasMore || res.NextCursor == "" || res.NextCursor == cursor {
			return nil
		}
		cursor = res.NextCursor
	}
	return nil
}
