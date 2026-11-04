// Package sinks delivers playback events to external services.
package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/DrSeussFreak/BacklotTracker/internal/config"
	"github.com/DrSeussFreak/BacklotTracker/internal/model"
	"github.com/DrSeussFreak/BacklotTracker/internal/store"
)

type M = map[string]any

var UserAgent = "BacklotTracker/" + model.Version

// Sink is one destination service.
type Sink interface {
	Name() string  // stable id used in the queue
	Label() string // human readable
	Kind() string
	Configured() bool
	Ready(ctx context.Context) bool // configured and authorized
	Send(ctx context.Context, ev model.Event) error
	SendHistory(ctx context.Context, evs []model.Event) error
}

// DeviceCode is a pending device/PIN authorization.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Authorizer is implemented by sinks that use a device-code login.
type Authorizer interface {
	StartAuth(ctx context.Context) (*DeviceCode, error)
	PollAuth(ctx context.Context, dc *DeviceCode) (bool, error)
	Disconnect(ctx context.Context) error
}

// Build creates every sink enabled in the config.
func Build(cfg *config.Config, st *store.Store) []Sink {
	var out []Sink
	if cfg.TraktClientID != "" {
		out = append(out, &Trakt{ClientID: cfg.TraktClientID, Secret: cfg.TraktClientSecret, st: st})
	}
	if cfg.SimklClientID != "" {
		out = append(out, &Simkl{ClientID: cfg.SimklClientID, st: st})
	}
	if cfg.FloppyURL != "" {
		out = append(out, &Floppy{BaseURL: cfg.FloppyURL, Token: cfg.FloppyToken})
	}
	for i, u := range cfg.RelayURLs {
		out = append(out, &Relay{N: i + 1, URL: u})
	}
	for i, u := range cfg.WebhookURLs {
		out = append(out, &Webhook{N: i + 1, URL: u})
	}
	return out
}

// ---------- errors ----------

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks an error as not worth retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func StatusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// ---------- HTTP helpers ----------

var httpClient = &http.Client{Timeout: 30 * time.Second}

type result struct {
	Status int
	Header http.Header
	Body   []byte
}

func do(ctx context.Context, method, url string, hdr map[string]string, body any) (*result, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, Permanent(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, Permanent(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return send(req)
}

func send(req *http.Request) (*result, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err // network errors are retryable
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	res := &result{Status: resp.StatusCode, Header: resp.Header, Body: data}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return res, nil
	}
	he := &HTTPError{Status: resp.StatusCode, Body: truncate(string(data), 300)}
	switch {
	case resp.StatusCode == 401, resp.StatusCode == 408, resp.StatusCode == 429, resp.StatusCode >= 500:
		return res, he
	default:
		return res, Permanent(he)
	}
}

func doJSON(ctx context.Context, method, url string, hdr map[string]string, body, out any) error {
	res, err := do(ctx, method, url, hdr, body)
	if err != nil {
		return err
	}
	if out != nil && len(res.Body) > 0 {
		if err := json.Unmarshal(res.Body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// fresh reports whether an event is recent enough to send as a live scrobble
// rather than as a history entry.
func fresh(ev model.Event) bool { return time.Since(ev.At) < time.Hour }

func clamp(p float64) float64 { return math.Round(math.Max(0, math.Min(100, p))*100) / 100 }

// watchedProgress makes sure services that apply their own threshold
// (Trakt/Simkl use 80%) count the play.
func watchedProgress(p float64) float64 { return clamp(math.Max(p, 95)) }

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
