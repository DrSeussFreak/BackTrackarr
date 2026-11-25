package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Books, audiobooks and games ("the shelf"). Every source is read-only: the
// only POSTs are Komga's book search and Hardcover's GraphQL query.

const shelfMaxBody = 64 << 20

var shelfHTTP = &http.Client{
	Timeout: 45 * time.Second,
	// Some sources authenticate with a custom header or a query parameter,
	// which Go would carry to another host on a redirect.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			return errors.New("refusing a redirect to another host")
		}
		return nil
	},
}

// shelfGetJSON and shelfSearchJSON are the only ways sources reach the
// network, so no source can send a PUT, PATCH or DELETE.
func shelfGetJSON(ctx context.Context, u string, hdr map[string]string, out any) error {
	return shelfDo(ctx, http.MethodGet, u, hdr, nil, out)
}

func shelfSearchJSON(ctx context.Context, u string, hdr map[string]string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return shelfDo(ctx, http.MethodPost, u, hdr, b, out)
}

type shelfTimeoutKey struct{}

// withShelfTimeout gives one slow source more time per request than the usual
// shelfHTTP limit. The import's own context still bounds the whole run.
func withShelfTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, shelfTimeoutKey{}, d)
}

func shelfDo(ctx context.Context, method, u string, hdr map[string]string, body []byte, out any) error {
	client := shelfHTTP
	if d, ok := ctx.Value(shelfTimeoutKey{}).(time.Duration); ok && d > client.Timeout {
		patient := *shelfHTTP
		patient.Timeout = d
		client = &patient
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("bad service address")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BackTrackarr (self-hosted)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if shelfTimedOut(ctx, err) {
			return errShelfSlow
		}
		// url.Error quotes the full URL, which can hold an API key.
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return shelfStatusError{resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, shelfMaxBody)).Decode(out); err != nil {
		if shelfTimedOut(ctx, err) {
			return errShelfSlow // the reply started, but didn't finish in time
		}
		return fmt.Errorf("unreadable reply: %w", err)
	}
	return nil
}

// errShelfSlow means a source took longer than its time limit. It's a plain
// sentence because it can reach the Import page as is.
var errShelfSlow = errors.New("the service didn't answer in time; try again when it's less busy")

// shelfTimedOut reports a request that ran out of time, as opposed to an
// import being stopped or paused (which cancels ctx and must not look slow).
func shelfTimedOut(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var nerr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout())
}

type shelfStatusError struct{ code int }

func (e shelfStatusError) Error() string {
	switch e.code {
	case 401, 403:
		return fmt.Sprintf("HTTP %d: check the token or key, and that the account can read this", e.code)
	case 404:
		return "HTTP 404: this address or server version doesn't offer that API"
	case 429:
		return "HTTP 429: rate limited, will try again on the next sync"
	}
	return fmt.Sprintf("HTTP %d", e.code)
}

func shelfStatusIs(err error, code int) bool {
	var se shelfStatusError
	return errors.As(err, &se) && se.code == code
}

// ShelfSource is one configured source, for the status tables.
type ShelfSource struct {
	Name, Label, Covers string
	Last                int64
	Err                 string
	run                 func(context.Context, func(string)) (int, error)
}

func (e *Engine) shelfSources(ctx context.Context) []ShelfSource {
	c := e.cfg.Shelf
	var out []ShelfSource
	add := func(on bool, name, label, covers string, run func(context.Context, func(string)) (int, error)) {
		if !on {
			return
		}
		s := ShelfSource{Name: name, Label: label, Covers: covers, run: run}
		v, _ := e.st.GetKV(ctx, "shelf|"+name+"|at")
		s.Last, _ = strconv.ParseInt(v, 10, 64)
		s.Err, _ = e.st.GetKV(ctx, "shelf|"+name+"|err")
		out = append(out, s)
	}
	add(c.AudiobookshelfURL != "" && c.AudiobookshelfToken != "", "audiobookshelf", "Audiobookshelf", "books", e.syncAudiobookshelf)
	add(c.KomgaURL != "" && c.KomgaKey != "", "komga", "Komga", "books", e.syncKomga)
	add(c.KavitaURL != "" && c.KavitaKey != "", "kavita", "Kavita", "books", e.syncKavita)
	add(c.HardcoverToken != "", "hardcover", "Hardcover", "books", e.syncHardcover)
	add(c.OpenLibraryUser != "", "openlibrary", "Open Library", "books", e.syncOpenLibrary)
	add(c.SteamKey != "" && c.SteamID != "", "steam", "Steam", "games", e.syncSteam)
	add(c.RetroUser != "" && c.RetroKey != "", "retroachievements", "RetroAchievements", "games", e.syncRetro)
	add(c.RommURL != "" && c.RommToken != "", "romm", "RomM", "games", e.syncRomm)
	return out
}

// ShelfSources lists configured sources for one page: "books" or "games".
func (e *Engine) ShelfSources(ctx context.Context, covers string) []ShelfSource {
	var out []ShelfSource
	for _, s := range e.shelfSources(ctx) {
		if s.Covers == covers {
			out = append(out, s)
		}
	}
	return out
}

// HasShelfSources reports whether any book or game source is configured.
func (e *Engine) HasShelfSources(ctx context.Context) bool {
	return len(e.shelfSources(ctx)) > 0
}

// SyncShelf reads every configured source for "books", "games" or both ("").
// One failing source never stops the others.
func (e *Engine) SyncShelf(ctx context.Context, covers string, report func(string)) error {
	var failed, done []string
	for _, s := range e.shelfSources(ctx) {
		if covers != "" && s.Covers != covers {
			continue
		}
		report("reading " + s.Label)
		n, err := s.run(ctx, report)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := ""
		if err != nil {
			msg = err.Error()
			failed = append(failed, s.Label+": "+msg)
			e.log.Warn("shelf sync", "source", s.Name, "err", msg)
		} else {
			_ = e.st.SetKV(ctx, "shelf|"+s.Name+"|at", strconv.FormatInt(time.Now().Unix(), 10))
			done = append(done, fmt.Sprintf("%s %d new", s.Label, n))
			e.log.Info("shelf sync", "source", s.Name, "new", n)
		}
		_ = e.st.SetKV(ctx, "shelf|"+s.Name+"|err", msg)
	}
	if len(done) > 0 {
		report(strings.Join(done, ", "))
	} else if len(failed) == 0 {
		report("no sources configured")
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// RunShelfSync reads all sources on a timer, one after another.
func (e *Engine) RunShelfSync(ctx context.Context) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_ = e.SyncShelf(ctx, "", func(string) {})
		t.Reset(e.cfg.Shelf.SyncEvery)
	}
}

// RunShelfWatcher imports files dropped into the books and games folders,
// then moves them to an "imported" subfolder. Checks hourly.
func (e *Engine) RunShelfWatcher(ctx context.Context) {
	dirs := map[string]string{"books": e.cfg.Shelf.BooksImportDir, "games": e.cfg.Shelf.GamesImportDir}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(d, "imported"), 0o755); err != nil {
			e.log.Warn("shelf import folder", "err", err)
		}
	}
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for kind, dir := range dirs {
			files, _ := filepath.Glob(filepath.Join(dir, "*"))
			for _, f := range files {
				switch strings.ToLower(filepath.Ext(f)) {
				case ".csv", ".tsv", ".txt", ".json":
				default:
					continue
				}
				fh, err := os.Open(f)
				if err != nil {
					continue
				}
				var res *ShelfImportResult
				if kind == "books" {
					res, err = e.ImportBooks(ctx, fh, "")
				} else {
					res, err = e.ImportGames(ctx, fh)
				}
				fh.Close()
				if err != nil {
					e.log.Warn("shelf import", "file", filepath.Base(f), "err", err)
					continue
				}
				e.log.Info("shelf import", "file", filepath.Base(f), "source", res.Source, "rows", res.Rows, "new", res.New, "events", res.Events)
				_ = os.Rename(f, filepath.Join(dir, "imported", time.Now().Format("20060102-150405-")+filepath.Base(f)))
			}
		}
		t.Reset(time.Hour)
	}
}

func (e *Engine) shelfUser(ctx context.Context) string {
	if e.cfg.Shelf.User != "" {
		return e.cfg.Shelf.User
	}
	return e.defaultUser(ctx)
}

// shelfWalked reports whether one complete page-by-page import has finished.
// Syncs stop early only after that, so an interrupted first import can't
// leave older history behind for good.
func (e *Engine) shelfWalked(ctx context.Context, source string) bool {
	v, _ := e.st.GetKV(ctx, "shelf|"+source+"|walked")
	return v == "1"
}

func (e *Engine) markShelfWalked(ctx context.Context, source string) {
	_ = e.st.SetKV(ctx, "shelf|"+source+"|walked", "1")
}

// addEvent stores an event and counts it if it was new.
func (e *Engine) addEvent(ctx context.Context, ev *store.ShelfEvent, n *int) error {
	ok, err := e.st.AddShelfEvent(ctx, ev)
	if ok {
		*n++
	}
	return err
}

// addFinishOnce records a completion. The first counts as a finish. Later
// ones (mastering a game already beaten) are kept as plays, so one playthrough
// isn't counted twice.
func (e *Engine) addFinishOnce(ctx context.Context, it *store.ShelfItem, user, extID string, at int64, n *int) error {
	done, err := e.st.HasShelfFinish(ctx, it.ID)
	if err != nil {
		return err
	}
	kind := "finish"
	if done {
		kind = "play"
	}
	return e.addEvent(ctx, &store.ShelfEvent{ItemID: it.ID, User: user, Kind: kind, ExtID: extID, At: at, Source: it.Source}, n)
}

// applyPlaytime turns a game's lifetime playtime counter into rows: the first
// sighting is a baseline, and each later increase is its own session.
func (e *Engine) applyPlaytime(ctx context.Context, it *store.ShelfItem, prev store.ShelfPrev, user string, n *int) error {
	at := it.LastActivity
	base := store.ShelfEvent{ItemID: it.ID, User: user, Source: it.Source}
	switch {
	case prev.New:
		if it.PlaytimeS <= 0 && at <= 0 {
			return nil
		}
		ev := base
		ev.Kind, ev.ExtID, ev.At, ev.DurationS = "baseline", "baseline", at, it.PlaytimeS
		return e.addEvent(ctx, &ev, n)
	case it.PlaytimeS > prev.PlaytimeS:
		if at <= prev.LastActivity {
			at = time.Now().Unix()
		}
		ev := base
		ev.Kind, ev.ExtID, ev.At, ev.DurationS = "session", "pt:"+strconv.FormatInt(it.PlaytimeS, 10), at, it.PlaytimeS-prev.PlaytimeS
		return e.addEvent(ctx, &ev, n)
	case at > prev.LastActivity:
		ev := base
		ev.Kind, ev.ExtID, ev.At = "play", "at:"+strconv.FormatInt(at, 10), at
		return e.addEvent(ctx, &ev, n)
	}
	return nil
}

// shelfTime parses the timestamps APIs return. Values without a zone are UTC.
func shelfTime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, l := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05", "2006/01/02, 15:04:05"} {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			if t.Year() < 1971 {
				return 0 // .NET and others use year 1 for "never"
			}
			return t.Unix()
		}
	}
	if t, ok := parseDate(s); ok {
		return t.Unix()
	}
	return 0
}

// ShelfImportResult summarizes one file import.
type ShelfImportResult struct {
	Source                     string
	Rows, New, Updated, Events int
}

func shelfRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func shelfYear(s string) int64 {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	y, err := strconv.ParseInt(s[:4], 10, 64)
	if err != nil || y < 1000 || y > 2200 {
		return 0
	}
	return y
}

func shelfFraction(a, b float64) float64 {
	if b <= 0 || a <= 0 {
		return 0
	}
	return min(a/b, 1)
}
