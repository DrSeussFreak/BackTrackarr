package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/DrSeussFreak/BacklotTracker/internal/model"
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

func (f *Floppy) body(action string, ev model.Event) M {
	m := ev.Media
	b := M{"action": action, "title": m.Title}
	ids := m.IDs
	if m.Kind == model.Episode {
		b["media_type"] = "episode"
		b["title"] = m.ShowTitle
		b["season_number"] = m.Season
		b["episode_number"] = m.Episode
		if !m.ShowIDs.Empty() {
			ids = m.ShowIDs
		}
	} else {
		b["media_type"] = "movie"
	}
	b["ids"] = ids.Strings()
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

func (f *Floppy) SendHistory(ctx context.Context, evs []model.Event) error {
	for _, ev := range evs {
		ev.Action = model.ActWatched
		if err := f.Send(ctx, ev); err != nil {
			return err
		}
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

func (r *Relay) Name() string { return fmt.Sprintf("relay-%d", r.N) }
func (r *Relay) Label() string {
	host := r.URL
	if u, err := url.Parse(r.URL); err == nil {
		host = u.Host
	}
	return "Plex webhook relay → " + host
}
func (r *Relay) Kind() string                   { return "relay" }
func (r *Relay) Configured() bool               { return r.URL != "" }
func (r *Relay) Ready(ctx context.Context) bool { return r.Configured() }

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
		return Permanent(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "PlexMediaServer") // some receivers check this
	_, err = send(req)
	return err
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

func (w *Webhook) Name() string { return fmt.Sprintf("webhook-%d", w.N) }
func (w *Webhook) Label() string {
	host := w.URL
	if u, err := url.Parse(w.URL); err == nil {
		host = u.Host
	}
	return "JSON webhook → " + host
}
func (w *Webhook) Kind() string                   { return "webhook" }
func (w *Webhook) Configured() bool               { return w.URL != "" }
func (w *Webhook) Ready(ctx context.Context) bool { return w.Configured() }

func (w *Webhook) Send(ctx context.Context, ev model.Event) error {
	ev.Raw = nil
	return doJSON(ctx, "POST", w.URL, nil, M{"event": ev.Action, "item": ev}, nil)
}

func (w *Webhook) SendHistory(ctx context.Context, evs []model.Event) error {
	for i := range evs {
		evs[i].Raw = nil
	}
	return doJSON(ctx, "POST", w.URL, nil, M{"event": "history", "items": evs}, nil)
}
