package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

// ---------- Floppy (self-hosted, https://github.com/dannyvfilms/Floppy) ----------

// Floppy sends events to Floppy's REST scrobble endpoint (POST /api/v1/scrobble).
type Floppy struct {
	BaseURL string
	Token   string
}

func (f *Floppy) Name() string                   { return "floppy" }
func (f *Floppy) Label() string                  { return "Floppy" }
func (f *Floppy) Kind() string                   { return "floppy" }
func (f *Floppy) Configured() bool               { return f.BaseURL != "" && f.Token != "" }
func (f *Floppy) Ready(ctx context.Context) bool { return f.Configured() }

// floppyIDs are the ids Floppy matches on. An episode is found through its
// show, so the episode's own ids must never stand in for the show's.
func floppyIDs(m model.Media) map[string]string {
	if m.Kind == model.Episode {
		return m.ShowIDs.Strings()
	}
	return m.IDs.Strings()
}

// CanSend reports whether Floppy could identify this title.
func (f *Floppy) CanSend(m model.Media) bool { return len(floppyIDs(m)) > 0 }

func (f *Floppy) body(action string, ev model.Event) M {
	m := ev.Media
	b := M{"action": action, "title": m.Title}
	if m.Kind == model.Episode {
		b["media_type"] = "episode"
		b["title"] = m.ShowTitle
		b["series_title"] = m.ShowTitle
		b["season_number"] = m.Season
		b["episode_number"] = m.Episode
	} else {
		b["media_type"] = "movie"
	}
	b["ids"] = floppyIDs(m)
	if m.DurationS > 0 {
		b["duration_seconds"] = m.DurationS
		b["position_seconds"] = int(float64(m.DurationS) * clamp(ev.Progress) / 100)
	}
	return b
}

func (f *Floppy) post(ctx context.Context, b M) error {
	return doJSON(ctx, "POST", f.BaseURL+"/api/v1/scrobble", map[string]string{"Authorization": "Bearer " + f.Token}, b, nil)
}

func (f *Floppy) Send(ctx context.Context, ev model.Event) error {
	switch ev.Action {
	case model.ActStart, model.ActPause, model.ActStop:
		return f.post(ctx, f.body(ev.Action, ev))
	case model.ActWatched:
		b := f.body("stop", ev)
		b["completed"] = true
		b["played_at"] = rfc3339(ev.At)
		return f.post(ctx, b)
	}
	return nil // ratings: no documented endpoint yet
}

// SendHistory sends every play it can. One rejected title doesn't stop the
// rest. A retry is safe because Floppy ignores a repeat of the same play time.
func (f *Floppy) SendHistory(ctx context.Context, evs []model.Event) error {
	rejected := 0
	var first error
	for _, ev := range evs {
		ev.Action = model.ActWatched
		err := f.Send(ctx, ev)
		switch {
		case err == nil:
		case IsPermanent(err):
			rejected++
			if first == nil {
				first = err
			}
		default:
			return err
		}
	}
	if rejected > 0 {
		return Permanent(fmt.Errorf("%d of %d plays rejected, first: %w", rejected, len(evs), first))
	}
	return nil
}

// ---------- Plex webhook relay ----------

// Relay re-sends Plex webhooks to any service that accepts them: Trakt's
// official Plex scrobbler, Floppy/Yamtrack (/webhook/plex/<token>),
// CrossWatch, Scrob, etc. Original webhooks are forwarded byte-for-byte;
// events from the session poller get a synthesized Plex-style payload.
type Relay struct {
	N   int
	URL string
}

func (r *Relay) Name() string  { return fmt.Sprintf("relay-%d", r.N) }
func (r *Relay) Label() string { return "Plex webhook relay → " + safeHost(r.URL) }
func (r *Relay) Kind() string  { return "relay" }
func (r *Relay) Configured() bool {
	return r.URL != ""
}
func (r *Relay) Ready(ctx context.Context) bool { return config.ValidHTTPURL(r.URL) }

// Problem explains a saved value that can't be used.
func (r *Relay) Problem() string { return addressProblem(r.URL) }

var plexEvent = map[string]string{
	model.ActStart: "media.play", model.ActPause: "media.pause", model.ActStop: "media.stop",
	model.ActWatched: "media.scrobble", model.ActRate: "media.rate",
}

func synthesize(ev model.Event) ([]byte, error) {
	m := ev.Media
	guids := []M{}
	for k, v := range m.IDs.Strings() {
		guids = append(guids, M{"id": k + "://" + v})
	}
	md := M{"type": m.Kind, "title": m.Title, "year": m.Year, "guid": m.PlexGUID, "ratingKey": m.PlexKey,
		"duration": m.DurationS * 1000, "viewOffset": int(float64(m.DurationS*1000) * ev.Progress / 100), "Guid": guids}
	if m.Kind == model.Episode {
		md["grandparentTitle"] = m.ShowTitle
		md["parentIndex"] = m.Season
		md["index"] = m.Episode
	}
	return json.Marshal(M{"event": plexEvent[ev.Action], "user": true, "owner": ev.Owner, "rating": ev.Rating,
		"Account": M{"title": ev.User}, "Player": M{"title": ev.Player}, "Metadata": md})
}

func (r *Relay) Send(ctx context.Context, ev model.Event) error {
	payload := []byte(ev.Raw)
	if len(payload) == 0 {
		var err error
		if payload, err = synthesize(ev); err != nil {
			return Permanent(err)
		}
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("payload", string(payload)); err != nil {
		return Permanent(err)
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", r.URL, &buf)
	if err != nil {
		return Permanent(withoutURL(err))
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "PlexMediaServer") // some receivers check this
	_, err = send(req)
	return withoutURL(err)
}

// SendHistory is a no-op: Plex webhooks have no notion of back-filled history.
func (r *Relay) SendHistory(ctx context.Context, evs []model.Event) error { return nil }

// ---------- Generic JSON webhook ----------

// Webhook posts every event as JSON. Use it for services without a public
// API yet, or for automation tools (n8n, Home Assistant, Node-RED...).
type Webhook struct {
	N   int
	URL string
}

func (w *Webhook) Name() string  { return fmt.Sprintf("webhook-%d", w.N) }
func (w *Webhook) Label() string { return "JSON webhook → " + safeHost(w.URL) }
func (w *Webhook) Kind() string  { return "webhook" }
func (w *Webhook) Configured() bool {
	return w.URL != ""
}
func (w *Webhook) Ready(ctx context.Context) bool { return config.ValidHTTPURL(w.URL) }

// Problem explains a saved value that can't be used.
func (w *Webhook) Problem() string { return addressProblem(w.URL) }

func (w *Webhook) Send(ctx context.Context, ev model.Event) error {
	ev.Raw = nil
	return withoutURL(doJSON(ctx, "POST", w.URL, nil, M{"event": ev.Action, "item": ev}, nil))
}

func (w *Webhook) SendHistory(ctx context.Context, evs []model.Event) error {
	for i := range evs {
		evs[i].Raw = nil
	}
	return withoutURL(doJSON(ctx, "POST", w.URL, nil, M{"event": "history", "items": evs}, nil))
}

// ---------- addresses that can hold tokens ----------

// safeHost is the only part of a relay address that may be displayed.
func safeHost(raw string) string {
	if !config.ValidHTTPURL(raw) {
		return "invalid address"
	}
	u, _ := url.Parse(raw)
	return u.Host
}

func addressProblem(raw string) string {
	if config.ValidHTTPURL(raw) {
		return ""
	}
	return "This is not a full web address. In Settings, enter the whole URL, starting with http:// or https://."
}

// withoutURL removes the address from a network error, because Go includes
// it and relay addresses often contain a token.
func withoutURL(err error) error {
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	clean := fmt.Errorf("%s request failed: %w", ue.Op, ue.Err)
	if IsPermanent(err) {
		return Permanent(clean)
	}
	return clean
}
