package throttle

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeBase struct {
	calls  int
	status int
	header http.Header
}

func (f *fakeBase) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	h := f.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: f.status, Header: h, Body: http.NoBody}, nil
}

// clocked runs a Transport on a fake clock: sleeping just moves time on.
func clocked(base http.RoundTripper) (*Transport, *time.Time, *time.Duration) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var slept time.Duration
	tr := New(base)
	tr.now = func() time.Time { return now }
	tr.sleep = func(_ *http.Request, d time.Duration) error { slept += d; now = now.Add(d); return nil }
	return tr, &now, &slept
}

func call(t *testing.T, tr *Transport, method, url string) error {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.RoundTrip(req)
	return err
}

func TestPacesPerHostAndWrites(t *testing.T) {
	b := &fakeBase{status: 200}
	tr, _, slept := clocked(b)
	for range 3 {
		call(t, tr, http.MethodGet, "https://openlibrary.org/x")
	}
	if want := 2200 * time.Millisecond; *slept != want {
		t.Fatalf("slept %v, want %v", *slept, want)
	}
	*slept = 0
	call(t, tr, http.MethodGet, "https://api.steampowered.com/x") // another host has its own pace
	if *slept != 0 {
		t.Fatalf("hosts must not delay each other: %v", *slept)
	}
	call(t, tr, http.MethodPost, "https://api.trakt.tv/a")
	*slept = 0
	call(t, tr, http.MethodPost, "https://api.trakt.tv/b")
	if *slept != 1100*time.Millisecond {
		t.Fatalf("Trakt writes need a second apart: %v", *slept)
	}
}

func TestOwnServersNotPaced(t *testing.T) {
	b := &fakeBase{status: 200}
	tr, _, slept := clocked(b)
	tr.SetExempt([]string{"https://plex.example.com:32400/"})
	for _, u := range []string{"http://192.168.1.10:32400/a", "http://radarr:7878/a", "http://nas.local/a", "https://plex.example.com:32400/a", "http://[fd00::1]:8080/a"} {
		for range 5 {
			call(t, tr, http.MethodGet, u)
		}
	}
	if *slept != 0 {
		t.Fatalf("own servers were paced: %v", *slept)
	}
}

func TestRetryAfterStopsCalls(t *testing.T) {
	b := &fakeBase{status: 429, header: http.Header{"Retry-After": {"600"}}}
	tr, now, _ := clocked(b)
	var saved map[string]int64
	tr.OnChange(func(m map[string]int64) { saved = m })
	call(t, tr, http.MethodGet, "https://api.hardcover.app/v1/graphql")
	if b.calls != 1 || saved["api.hardcover.app"] == 0 {
		t.Fatalf("calls=%d saved=%v", b.calls, saved)
	}
	err := call(t, tr, http.MethodGet, "https://api.hardcover.app/v1/graphql")
	if _, ok := Cooling(err); !ok || b.calls != 1 {
		t.Fatalf("must not call during the penalty: err=%v calls=%d", err, b.calls)
	}
	if strings.Contains(err.Error(), "graphql") {
		t.Fatalf("error should name only the host: %v", err)
	}
	if p := tr.Paused(); len(p) != 1 || p[0].Host != "api.hardcover.app" {
		t.Fatalf("paused: %+v", p)
	}
	*now = now.Add(601 * time.Second)
	b.status, b.header = 200, nil
	if err := call(t, tr, http.MethodGet, "https://api.hardcover.app/v1/graphql"); err != nil || b.calls != 2 {
		t.Fatalf("should resume: %v calls=%d", err, b.calls)
	}
}

func TestBackoffGrowsWithoutRetryAfter(t *testing.T) {
	b := &fakeBase{status: 429}
	tr, now, _ := clocked(b)
	var gaps []time.Duration
	for range 3 {
		call(t, tr, http.MethodGet, "https://api.simkl.com/x")
		p := tr.Paused()
		if len(p) != 1 {
			t.Fatal("expected a pause")
		}
		gaps = append(gaps, p[0].Until.Sub(*now))
		*now = p[0].Until.Add(time.Second)
	}
	if gaps[0] != 30*time.Second || gaps[1] != time.Minute || gaps[2] != 2*time.Minute {
		t.Fatalf("back-off: %v", gaps)
	}
}

func TestRepeatedAuthFailuresPause(t *testing.T) {
	b := &fakeBase{status: 401}
	tr, _, _ := clocked(b)
	for range 3 {
		if err := call(t, tr, http.MethodGet, "https://api.steampowered.com/x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(t, tr, http.MethodGet, "https://api.steampowered.com/x"); err == nil || b.calls != 3 {
		t.Fatalf("a bad key must stop being retried: err=%v calls=%d", err, b.calls)
	}
	tr.Clear("api.steampowered.com")
	b.status = 200
	if err := call(t, tr, http.MethodGet, "https://api.steampowered.com/x"); err != nil {
		t.Fatal(err)
	}

	// plex.tv refuses some shared users' tokens in normal use.
	p := &fakeBase{status: 401}
	tp, _, _ := clocked(p)
	for range 6 {
		if err := call(t, tp, http.MethodGet, "https://plex.tv/x"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoreSurvivesRestart(t *testing.T) {
	b := &fakeBase{status: 200}
	tr, now, _ := clocked(b)
	tr.Restore(map[string]int64{"api.trakt.tv": now.Add(time.Hour).Unix(), "old.example.com": now.Add(-time.Hour).Unix()})
	err := call(t, tr, http.MethodGet, "https://api.trakt.tv/x")
	var ce *CoolingError
	if !errors.As(err, &ce) || b.calls != 0 {
		t.Fatalf("restored pause ignored: %v", err)
	}
	if len(tr.Paused()) != 1 {
		t.Fatalf("expired pause restored: %+v", tr.Paused())
	}
}
