// Package throttle paces every outbound HTTP request so BackTrackarr doesn't
// get rate limited or locked out by the services it talks to. It wraps the
// default transport, so all clients are covered without knowing about it.
//
// For each host it spaces requests out, obeys 429 and Retry-After, and stops
// calling for a while after repeated sign-in failures. Servers on your own
// network are not paced.
package throttle

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// A wait up to this long happens inline. Anything longer fails fast, so
	// a job isn't stuck and the service isn't called during its penalty.
	maxInlineWait = 15 * time.Second
	maxCooldown   = 24 * time.Hour
	authFailLimit = 3 // 401/403 in a row before backing off
)

// Rule is the pace for one service.
type Rule struct {
	Every      time.Duration // minimum gap between requests
	WriteEvery time.Duration // gap after a POST, PUT or DELETE, if longer
	NoAuthStop bool          // 401/403 are normal here, so don't back off
}

// Published limits where a service documents them, and a cautious pace where
// it doesn't.
var rules = map[string]Rule{
	"api.trakt.tv":                      {Every: 350 * time.Millisecond, WriteEvery: 1100 * time.Millisecond}, // 1,000 GETs per 5 min, 1 POST per second
	"api.simkl.com":                     {Every: time.Second},
	"api.themoviedb.org":                {Every: 100 * time.Millisecond},
	"openlibrary.org":                   {Every: 1100 * time.Millisecond}, // 1 per second without a contact address
	"api.hardcover.app":                 {Every: 1050 * time.Millisecond}, // 60 per minute
	"api.steampowered.com":              {Every: 1100 * time.Millisecond},
	"retroachievements.org":             {Every: time.Second},
	"plex.tv":                           {Every: 500 * time.Millisecond, NoAuthStop: true}, // shared users' tokens can be refused
	"api.anthropic.com":                 {Every: time.Second},
	"generativelanguage.googleapis.com": {Every: time.Second},
}

// defaultRule applies to any other public host, such as a webhook target.
var defaultRule = Rule{Every: 250 * time.Millisecond}

// CoolingError means the service is in a back-off period and wasn't called.
type CoolingError struct {
	Host  string
	Until time.Time
}

func (e *CoolingError) Error() string {
	return fmt.Sprintf("%s is paused until %s to avoid a lockout", e.Host, e.Until.Local().Format("15:04:05"))
}

// Cooling returns the back-off end time if err is a CoolingError.
func Cooling(err error) (time.Time, bool) {
	var ce *CoolingError
	if errors.As(err, &ce) {
		return ce.Until, true
	}
	return time.Time{}, false
}

type hostState struct {
	next      time.Time // earliest free request slot
	cooldown  time.Time
	strikes   int // rate-limit replies in a row
	authFails int
	reason    string
}

// Status describes one host currently paused.
type Status struct {
	Host   string
	Until  time.Time
	Reason string
}

// Transport is the throttling http.RoundTripper.
type Transport struct {
	Base     http.RoundTripper
	mu       sync.Mutex
	hosts    map[string]*hostState
	exempt   map[string]bool
	onChange func(map[string]int64)
	now      func() time.Time
	sleep    func(*http.Request, time.Duration) error
}

// New wraps base.
func New(base http.RoundTripper) *Transport {
	return &Transport{Base: base, hosts: map[string]*hostState{}, exempt: map[string]bool{}, now: time.Now,
		sleep: func(r *http.Request, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-r.Context().Done():
				return r.Context().Err()
			case <-t.C:
				return nil
			}
		}}
}

var (
	installOnce sync.Once
	shared      *Transport
)

// Install puts the throttle in front of http.DefaultTransport, once.
func Install() *Transport {
	installOnce.Do(func() {
		shared = New(http.DefaultTransport)
		http.DefaultTransport = shared
	})
	return shared
}

// SetExempt lists hosts of your own servers (from their configured URLs),
// which aren't paced even when they have a public-looking name.
func (t *Transport) SetExempt(urls []string) {
	m := map[string]bool{}
	for _, u := range urls {
		if h := hostOf(u); h != "" {
			m[h] = true
		}
	}
	t.mu.Lock()
	t.exempt = m
	t.mu.Unlock()
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	if h, _, err := net.SplitHostPort(u); err == nil {
		u = h
	}
	return strings.ToLower(strings.Trim(u, "[]"))
}

// OnChange is called with all active cooldowns whenever one is set, so they
// can be saved and survive a restart.
func (t *Transport) OnChange(fn func(map[string]int64)) {
	t.mu.Lock()
	t.onChange = fn
	t.mu.Unlock()
}

// Restore reloads saved cooldowns (host to Unix time).
func (t *Transport) Restore(saved map[string]int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for h, until := range saved {
		if u := time.Unix(until, 0); u.After(now) {
			st := t.state(h)
			if u.After(st.cooldown) {
				st.cooldown, st.reason = u, "paused before the last restart"
			}
		}
	}
}

// Paused lists hosts currently in a back-off period.
func (t *Transport) Paused() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []Status
	for h, st := range t.hosts {
		if st.cooldown.After(now) {
			out = append(out, Status{Host: h, Until: st.cooldown, Reason: st.reason})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Host < out[b].Host })
	return out
}

// Clear ends a host's back-off, for a fixed key or token.
func (t *Transport) Clear(host string) {
	t.mu.Lock()
	if st, ok := t.hosts[host]; ok {
		st.cooldown, st.strikes, st.authFails, st.reason = time.Time{}, 0, 0, ""
	}
	fn, snap := t.onChange, t.snapshot()
	t.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
}

func (t *Transport) state(h string) *hostState {
	st := t.hosts[h]
	if st == nil {
		st = &hostState{}
		t.hosts[h] = st
	}
	return st
}

func (t *Transport) snapshot() map[string]int64 {
	now := t.now()
	m := map[string]int64{}
	for h, st := range t.hosts {
		if st.cooldown.After(now) {
			m[h] = st.cooldown.Unix()
		}
	}
	return m
}

// isLocal is true for addresses on your own network.
func isLocal(h string) bool {
	if h == "localhost" || !strings.Contains(h, ".") && !strings.Contains(h, ":") {
		return true // Docker service names
	}
	for _, s := range []string{".local", ".lan", ".internal", ".home", ".localhost", ".home.arpa"} {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return false
}

func ruleFor(h string) Rule {
	if r, ok := rules[h]; ok {
		return r
	}
	for name, r := range rules {
		if strings.HasSuffix(h, "."+name) {
			return r
		}
	}
	return defaultRule
}

// RoundTrip waits for this host's next slot, sends, and learns from the reply.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	t.mu.Lock()
	own := t.exempt[host] || isLocal(host)
	rule := ruleFor(host)
	st := t.state(host)
	now := t.now()
	if st.cooldown.After(now) && st.cooldown.Sub(now) > maxInlineWait {
		until := st.cooldown
		t.mu.Unlock()
		closeBody(req)
		return nil, &CoolingError{Host: host, Until: until}
	}
	var wait time.Duration
	if !own {
		slot := now
		if st.next.After(slot) {
			slot = st.next
		}
		if st.cooldown.After(slot) {
			slot = st.cooldown
		}
		gap := rule.Every
		if req.Method != http.MethodGet && req.Method != http.MethodHead && rule.WriteEvery > gap {
			gap = rule.WriteEvery
		}
		st.next = slot.Add(gap)
		wait = slot.Sub(now)
	} else if st.cooldown.After(now) {
		wait = st.cooldown.Sub(now)
	}
	t.mu.Unlock()
	if wait > 0 {
		if err := t.sleep(req, wait); err != nil {
			closeBody(req)
			return nil, err
		}
	}
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	t.observe(host, own, rule, resp)
	return resp, nil
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		req.Body.Close()
	}
}

func (t *Transport) observe(host string, own bool, rule Rule, resp *http.Response) {
	t.mu.Lock()
	st := t.state(host)
	now := t.now()
	var pause time.Duration
	reason := ""
	retry, hasRetry := retryAfter(resp.Header.Get("Retry-After"), now)
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests:
		st.strikes++
		pause, reason = retry, "the service asked to slow down"
		if !hasRetry {
			pause = 30 * time.Second << min(st.strikes-1, 7) // 30 s, doubling, about 1 h at most
		}
	case code == http.StatusServiceUnavailable && hasRetry && !own:
		pause, reason = retry, "the service is temporarily unavailable"
	case (code == http.StatusUnauthorized || code == http.StatusForbidden) && !own && !rule.NoAuthStop:
		st.authFails++
		if st.authFails >= authFailLimit {
			pause = 15 * time.Minute << min(st.authFails-authFailLimit, 4) // 15 min, doubling, up to 4 h
			reason = "repeated sign-in failures: check the key or token"
		}
	case code >= 200 && code < 400:
		st.strikes, st.authFails = 0, 0
	}
	var fn func(map[string]int64)
	var snap map[string]int64
	if pause > 0 {
		if pause > maxCooldown {
			pause = maxCooldown
		}
		if until := now.Add(pause); until.After(st.cooldown) {
			st.cooldown, st.reason = until, reason
			fn, snap = t.onChange, t.snapshot()
		}
	}
	t.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
}

// retryAfter reads the header as seconds or an HTTP date.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}
