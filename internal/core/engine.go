// Package core turns Plex activity into stored plays and queued deliveries.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
	"github.com/DrSeussFreak/BackTrackarr/internal/throttle"
)

type Engine struct {
	ctx   context.Context // app lifetime, for background jobs
	cfg   *config.Config
	st    *store.Store
	plex  *plex.Client
	sinks []sinks.Sink
	log   *slog.Logger
	wake  chan struct{}
	Jobs  *Jobs

	secMu    sync.Mutex
	sections map[string]string // library section id -> title, for private-library checks
	machine  string
	users    []PlexUser
	usersAt  time.Time

	drive driveState
}

func New(ctx context.Context, cfg *config.Config, st *store.Store, px *plex.Client, all []sinks.Sink, log *slog.Logger) *Engine {
	return &Engine{ctx: ctx, cfg: cfg, st: st, plex: px, sinks: all, log: log, wake: make(chan struct{}, 1), Jobs: NewJobs(),
		sections: map[string]string{}}
}

func (e *Engine) Sinks() []sinks.Sink { return e.sinks }

func (e *Engine) Sink(name string) sinks.Sink {
	for _, s := range e.sinks {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func (e *Engine) HasPlex() bool { return e.plex != nil }

func (e *Engine) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// allowed reports whether this user's activity goes to external services.
func (e *Engine) allowed(ev model.Event) bool {
	if ev.Private {
		return false
	}
	if len(e.cfg.ScrobbleUsers) == 0 {
		return ev.Owner
	}
	for _, u := range e.cfg.ScrobbleUsers {
		if strings.EqualFold(u, ev.User) {
			return true
		}
	}
	return false
}

func needsIDs(m model.Media) bool {
	if m.Kind == model.Track {
		return false // tracks are matched by Plex guid, MusicBrainz id or artist/album/title
	}
	if m.Kind == model.Movie {
		return m.IDs.Empty()
	}
	return m.ShowIDs.Empty()
}

// Resolve converts Plex metadata into a stored Media, asking the Plex server
// for external ids only when we don't already know them.
func (e *Engine) Resolve(ctx context.Context, md *plex.Metadata) (model.Media, bool, error) {
	m, ok := plex.ToMedia(*md)
	if !ok {
		return m, false, nil
	}
	if ex, found, _ := e.st.FindMedia(ctx, m); found && !needsIDs(ex) {
		m.IDs.Merge(ex.IDs)
		m.ShowIDs.Merge(ex.ShowIDs)
	} else if needsIDs(m) || m.IDs.Empty() {
		e.plex.Enrich(ctx, &m, md)
	}
	if err := e.st.UpsertMedia(ctx, &m); err != nil {
		return m, true, err
	}
	return m, true, nil
}

var webhookActions = map[string]string{
	"media.play": model.ActStart, "media.resume": model.ActStart, "media.pause": model.ActPause,
	"media.stop": model.ActStop, "media.scrobble": model.ActWatched, "media.rate": model.ActRate,
}

// HandlePlexWebhook processes the JSON from a Plex webhook's "payload" field.
func (e *Engine) HandlePlexWebhook(ctx context.Context, raw []byte) error {
	var p plex.WebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("bad payload: %w", err)
	}
	act, ok := webhookActions[p.Event]
	if !ok {
		return nil
	}
	ev := model.Event{Action: act, User: p.Account.Title, Owner: p.Owner, Player: p.Player.Title,
		At: time.Now(), Source: "plex-webhook", Raw: raw, Progress: plex.Progress(&p.Metadata)}
	ev.Private = e.IsPrivateSection(p.Metadata.LibrarySectionTitle, p.Metadata.LibrarySectionID.String())
	e.log.Debug("plex webhook", "event", p.Event, "user", ev.User, "title", p.Metadata.Title)

	// Relays get every webhook verbatim (before dedupe), so receivers that
	// keep their own session logic see exactly what Plex sent.
	if e.allowed(ev) {
		for _, s := range e.sinks {
			if s.Kind() == "relay" && s.Ready(ctx) {
				if err := e.st.Enqueue(ctx, s.Name(), ev.Action, ev); err != nil {
					return err
				}
			}
		}
		e.Wake()
	}
	ev.Raw = nil

	m, ok, err := e.Resolve(ctx, &p.Metadata)
	if !ok || err != nil {
		return err
	}
	ev.Media = m
	if act == model.ActWatched && ev.Progress > 0 && ev.Progress < e.watchedAt(m.Kind) {
		return nil // Plex counted it earlier than your setting; the live check will count it
	}
	if act == model.ActRate {
		ev.Rating = p.Rating
		if ev.Rating == 0 {
			ev.Rating = p.Metadata.UserRating
		}
	}
	return e.Ingest(ctx, ev, true)
}

func sessionKey(ev model.Event) string {
	return fmt.Sprintf("%s|%d|%s", ev.User, ev.Media.ID, ev.Player)
}

// Ingest applies one event: tracks the playback session, records completed
// plays (deduplicated), and queues delivery to each ready sink.
// skipRelays is true when relays were already fed the raw webhook.
func (e *Engine) Ingest(ctx context.Context, ev model.Event, skipRelays bool) error {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if ev.Media.ID == 0 {
		if err := e.st.UpsertMedia(ctx, &ev.Media); err != nil {
			return err
		}
	}
	key := sessionKey(ev)
	sess, err := e.st.GetSession(ctx, key)
	if err != nil {
		return err
	}
	// A session that already counted as watched suppresses further live
	// events (credits, pause/resume after the scrobble point).
	done := sess != nil && sess.Scrobbled && time.Since(sess.UpdatedAt) < 4*time.Hour

	switch ev.Action {
	case model.ActStart, model.ActPause:
		if err := e.st.PutSession(ctx, store.Session{Key: key, MediaID: ev.Media.ID, User: ev.User,
			State: ev.Action, Progress: ev.Progress, Scrobbled: done, Player: ev.Player, Source: ev.Source}); err != nil {
			return err
		}
		if ev.Action == model.ActStart && ev.Player != "" && strings.HasPrefix(ev.Source, "plex") {
			if err := e.st.EndOtherPlexSessions(ctx, ev.User, ev.Player, key); err != nil {
				return err
			}
		}
		if done {
			return nil
		}
	case model.ActStop:
		if err := e.st.DeleteSession(ctx, key); err != nil {
			return err
		}
		if done {
			return nil
		}
		if ev.Progress >= e.watchedAt(ev.Media.Kind) {
			ev.Action = model.ActWatched
		}
	case model.ActWatched:
		if done {
			return nil
		}
		// A library scan records a play. Nothing is playing because of it.
		if ev.Source != plexMarkSource {
			if err := e.st.PutSession(ctx, store.Session{Key: key, MediaID: ev.Media.ID, User: ev.User,
				State: ev.Action, Progress: ev.Progress, Scrobbled: true, Player: ev.Player, Source: ev.Source}); err != nil {
				return err
			}
		}
	case model.ActRate:
		if err := e.st.SetRating(ctx, ev.Media.ID, ev.User, ev.Rating, ev.At); err != nil {
			return err
		}
	}

	if ev.Action == model.ActWatched {
		inserted, err := e.st.RecordPlay(ctx, ev.Media.ID, ev.User, ev.At, ev.Source, ev.Player, ev.Private)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		e.log.Info("watched", "user", ev.User, "title", ev.Media.Display())
	}

	if !e.allowed(ev) {
		return nil
	}
	for _, s := range e.sinks {
		if skipRelays && s.Kind() == "relay" {
			continue
		}
		if !accepts(s, ev.Media.Kind) {
			continue
		}
		if !s.Ready(ctx) {
			continue
		}
		if err := e.st.Enqueue(ctx, s.Name(), ev.Action, ev); err != nil {
			return err
		}
	}
	e.Wake()
	return nil
}

// ---------- delivery worker ----------

const maxAttempts = 12

// RunWorker drains the outbox. Delivery is strictly ordered per sink and
// paced at about one request per second per sink (Trakt's write limit).
// When idle it wakes every 10s, so CPU use is close to zero.
func (e *Engine) RunWorker(ctx context.Context) {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	lastMaint := time.Now()
	if err := e.st.ScrubOutboxErrors(ctx); err != nil {
		e.log.Warn("outbox", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.wake:
		}
		busy := false
		for _, s := range e.sinks {
			if e.processOne(ctx, s) {
				busy = true
			}
		}
		if time.Since(lastMaint) > 6*time.Hour {
			if err := e.st.Maintain(ctx); err != nil {
				e.log.Warn("maintenance", "err", err)
			}
			lastMaint = time.Now()
		}
		d := 10 * time.Second
		if busy {
			d = 1100 * time.Millisecond
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d)
	}
}

func (e *Engine) processOne(ctx context.Context, s sinks.Sink) bool {
	row, err := e.st.NextOutbox(ctx, s.Name())
	if err != nil {
		e.log.Error("outbox", "err", err)
		return false
	}
	if row == nil || row.NextAt > time.Now().Unix() || !s.Ready(ctx) {
		return false
	}
	age := time.Since(time.Unix(row.CreatedAt, 0))
	limit := 60 * time.Second
	if row.Action == "history" {
		limit = 5 * time.Minute // a batch is up to 100 separate requests
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	filter, _ := s.(sinks.MediaFilter)

	var sendErr error
	if row.Action == "history" {
		var evs []model.Event
		if err := json.Unmarshal(row.Payload, &evs); err != nil {
			_ = e.st.FinishOutbox(ctx, row.ID, "failed", err.Error())
			return true
		}
		if filter != nil {
			keep := evs[:0]
			for _, ev := range evs {
				e.refreshIDs(ctx, &ev)
				if filter.CanSend(ev.Media) {
					keep = append(keep, ev)
				} else if err := e.st.AddSkipped(ctx, s.Name(), ev, sinks.NoIDReason); err != nil {
					e.log.Error("outbox", "err", err)
					return false
				}
			}
			evs = keep
			if len(evs) == 0 {
				_ = e.st.FinishOutbox(ctx, row.ID, "dropped", sinks.NoIDReason)
				return true
			}
		}
		sendErr = s.SendHistory(cctx, evs)
	} else {
		var ev model.Event
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			_ = e.st.FinishOutbox(ctx, row.ID, "failed", err.Error())
			return true
		}
		live := ev.Action == model.ActStart || ev.Action == model.ActPause || ev.Action == model.ActStop
		if live && age > 15*time.Minute {
			_ = e.st.FinishOutbox(ctx, row.ID, "dropped", "stale live event")
			return true
		}
		// A Plex webhook has no date, so a late one would look like a play
		// happening now.
		if s.Kind() == "relay" && age > relayMaxAge {
			_ = e.st.FinishOutbox(ctx, row.ID, "dropped", "too old to relay")
			return true
		}
		if filter != nil {
			e.refreshIDs(ctx, &ev)
			if !filter.CanSend(ev.Media) {
				status := "dropped" // a live update isn't a play
				if ev.Action == model.ActWatched {
					status = "skipped"
				}
				_ = e.st.FinishOutbox(ctx, row.ID, status, sinks.NoIDReason)
				return true
			}
		}
		sendErr = s.Send(cctx, ev)
	}

	until, cooling := throttle.Cooling(sendErr)
	switch {
	case sendErr == nil:
		_ = e.st.FinishOutbox(ctx, row.ID, "ok", "")
	case cooling:
		// Nothing was sent, so this doesn't use up a retry.
		_ = e.st.RetryOutbox(ctx, row.ID, row.Attempts, until.Add(time.Second), sendErr.Error())
	case sinks.IsPermanent(sendErr) || row.Attempts+1 >= maxAttempts:
		e.log.Warn("delivery failed", "sink", s.Name(), "action", row.Action, "err", sendErr)
		_ = e.st.FinishOutbox(ctx, row.ID, "failed", sendErr.Error())
	default:
		backoff := time.Duration(30<<min(row.Attempts, 10)) * time.Second
		if backoff > 6*time.Hour {
			backoff = 6 * time.Hour
		}
		e.log.Info("delivery retry", "sink", s.Name(), "in", backoff, "err", sendErr)
		_ = e.st.RetryOutbox(ctx, row.ID, row.Attempts+1, time.Now().Add(backoff), sendErr.Error())
	}
	return true
}

// relayMaxAge is how late a Plex webhook may still be forwarded.
const relayMaxAge = time.Hour

// refreshIDs picks up ids found since the event was queued.
func (e *Engine) refreshIDs(ctx context.Context, ev *model.Event) {
	if ev.Media.ID == 0 {
		return
	}
	if m, err := e.st.GetMedia(ctx, ev.Media.ID); err == nil {
		ev.Media.IDs.Merge(m.IDs)
		ev.Media.ShowIDs.Merge(m.ShowIDs)
	}
}

// ---------- bulk operations ----------

// PushHistory queues a user's entire local history to one sink in batches.
func (e *Engine) PushHistory(ctx context.Context, sinkName, user string, report func(string)) error {
	s := e.Sink(sinkName)
	if s == nil {
		return fmt.Errorf("unknown sink %q", sinkName)
	}
	var after int64
	total := 0
	for {
		rows, err := e.st.PlaysAfter(ctx, user, after, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		evs := make([]model.Event, 0, len(rows))
		for _, r := range rows {
			if r.Private || !accepts(s, r.Media.Kind) {
				continue
			}
			evs = append(evs, model.Event{Action: model.ActWatched, Media: r.Media, User: r.User,
				At: time.Unix(r.WatchedAt, 0), Source: r.Source, Progress: 100})
		}
		if len(evs) > 0 {
			if err := e.st.Enqueue(ctx, s.Name(), "history", evs); err != nil {
				return err
			}
		}
		after = rows[len(rows)-1].ID
		total += len(rows)
		report(fmt.Sprintf("queued %d plays for %s", total, s.Label()))
	}
	e.Wake()
	return nil
}

// ImportPlex reads the Plex server's full watch history into the local DB.
func (e *Engine) ImportPlex(ctx context.Context, report func(string)) error {
	if e.plex == nil {
		return fmt.Errorf("PLEX_URL and PLEX_TOKEN are not set")
	}
	accounts, err := e.plex.Accounts(ctx)
	if err != nil {
		return err
	}
	cache := map[string]int64{} // ratingKey -> media id, saves Plex lookups
	start, added := 0, 0
	for {
		items, err := e.plex.History(ctx, start, 200)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		// Plex is asked outside the transaction, so a slow lookup never holds
		// the database's write lock.
		type plexPlay struct {
			id      int64
			user    string
			at      time.Time
			private bool
		}
		page := make([]plexPlay, 0, len(items))
		for i := range items {
			md := &items[i]
			if md.ViewedAt == 0 {
				continue
			}
			id, ok := cache[md.RatingKey]
			if !ok || md.RatingKey == "" {
				var m model.Media
				var ok2 bool
				err := retryBusy(ctx, func() (err error) {
					m, ok2, err = e.Resolve(ctx, md)
					return err
				})
				if store.IsBusy(err) || ctx.Err() != nil {
					return fmt.Errorf("stopped at entry %d, import again to continue: %w", start+i, err)
				}
				if err != nil {
					e.log.Warn("import item", "title", md.Title, "err", err)
					continue
				}
				if !ok2 {
					continue
				}
				id = m.ID
				if md.RatingKey != "" {
					cache[md.RatingKey] = id
				}
			}
			user := accounts[md.AccountID.String()]
			if user == "" {
				user = "account-" + md.AccountID.String()
			}
			page = append(page, plexPlay{id, user, time.Unix(md.ViewedAt, 0),
				e.IsPrivateSection(md.LibrarySectionTitle, md.LibrarySectionID.String())})
		}
		saved := 0
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			saved = 0
			for _, p := range page {
				ins, err := e.st.RecordPlay(ctx, p.id, p.user, p.at, "plex-import", "", p.private)
				if err != nil {
					return err
				}
				if ins {
					saved++
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("stopped at entry %d, import again to continue: %w", start, err)
		}
		added += saved
		start += len(items)
		report(fmt.Sprintf("read %d history entries, %d new plays", start, added))
	}
	report(fmt.Sprintf("done: read %d history entries, %d new plays", start, added))
	return nil
}

var busyRetryWait = 2 * time.Second

// retryBusy repeats a write that lost out to another writer for up to about
// two minutes. fn must be safe to run again.
func retryBusy(ctx context.Context, fn func() error) error {
	var err error
	for try := 0; try < 4; try++ {
		if err = fn(); !store.IsBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(busyRetryWait):
		}
	}
	return err
}

// ImportTrakt copies the connected Trakt account's history into the local DB
// under the given local user name.
func (e *Engine) ImportTrakt(ctx context.Context, user string, report func(string)) error {
	t, ok := e.Sink("trakt").(*sinks.Trakt)
	if !ok || !t.Ready(ctx) {
		return fmt.Errorf("trakt is not connected")
	}
	added := 0
	err := t.ImportHistory(ctx, time.Time{}, func(m model.Media, at time.Time) error {
		if err := e.st.UpsertMedia(ctx, &m); err != nil {
			return err
		}
		ins, err := e.st.RecordPlay(ctx, m.ID, user, at, "trakt-import", "", false)
		if ins {
			added++
		}
		return err
	}, func(msg string) { report(fmt.Sprintf("%s, %d new", msg, added)) })
	if err == nil {
		report(fmt.Sprintf("done: %d new plays", added))
	}
	return err
}

// RunBackups writes a database snapshot shortly after start, then daily.
func (e *Engine) RunBackups(ctx context.Context) {
	t := time.NewTimer(e.nextBackup(ctx, time.Now()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		wait := time.Duration(0)
		if err := e.BackupNow(ctx, func(msg string) { e.log.Info("backup", "status", msg) }); err != nil {
			e.log.Error("backup", "err", err)
			if ctx.Err() == nil && !e.backupSaved(ctx) {
				wait = min(time.Hour, e.cfg.BackupInterval()) // nothing was saved: try again soon
			}
		}
		if wait == 0 {
			wait = e.nextBackup(ctx, time.Now())
		}
		t.Reset(wait)
	}
}

const backupLastKey = "backup|last"

// backupSaved reports whether a backup was written within the last minute.
func (e *Engine) backupSaved(ctx context.Context) bool {
	v, _ := e.st.GetKV(ctx, backupLastKey)
	at, _ := strconv.ParseInt(v, 10, 64)
	return time.Now().Unix()-at < 60
}

// nextBackup counts from the last backup, so restarts and saved settings
// neither add extra backups nor put the next one off. One that is due waits
// a few minutes after start, while the app settles.
func (e *Engine) nextBackup(ctx context.Context, now time.Time) time.Duration {
	v, _ := e.st.GetKV(ctx, backupLastKey)
	at, _ := strconv.ParseInt(v, 10, 64)
	if at == 0 {
		return 5 * time.Minute
	}
	return max(time.Unix(at, 0).Add(e.cfg.BackupInterval()).Sub(now), 5*time.Minute)
}

// ---------- private libraries and per-kind rules ----------

// IsPrivateSection reports whether a library (by title, or by id when the
// title isn't in the payload) is listed in PRIVATE_LIBRARIES.
func (e *Engine) IsPrivateSection(title, id string) bool {
	if len(e.cfg.PrivateLibraries) == 0 {
		return false
	}
	if title == "" && id != "" {
		e.secMu.Lock()
		title = e.sections[id]
		e.secMu.Unlock()
		if title == "" && e.plex != nil {
			e.refreshSections(e.ctx)
			e.secMu.Lock()
			title = e.sections[id]
			e.secMu.Unlock()
		}
	}
	for _, p := range e.cfg.PrivateLibraries {
		if title != "" && strings.EqualFold(p, title) {
			return true
		}
	}
	return false
}

func (e *Engine) refreshSections(ctx context.Context) {
	secs, err := e.plex.Sections(ctx)
	if err != nil {
		return
	}
	e.secMu.Lock()
	for _, s := range secs {
		e.sections[s.Key] = s.Title
	}
	e.secMu.Unlock()
}

// watchedAt returns the progress at which something counts as finished, from
// the setting for its type.
func (e *Engine) watchedAt(kind string) float64 {
	return e.cfg.FinishPercent(kind)
}

// FinishPercent is the same value, for the web pages.
func (e *Engine) FinishPercent(kind string) float64 { return e.cfg.FinishPercent(kind) }

// accepts reports whether a sink takes this kind of media. Trakt, Simkl and
// Floppy are video-only; relays and generic webhooks take everything.
func accepts(s sinks.Sink, kind string) bool {
	if kind != model.Track {
		return true
	}
	return s.Kind() == "relay" || s.Kind() == "webhook"
}
