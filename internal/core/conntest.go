package core

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
)

// Connection tests only read, and never send a play, webhook or AI prompt.
// Their messages never include a key or a full address, since those can carry
// tokens. They bypass the rate limiter so a mistyped key can't pause a service.

// Connection test states.
const (
	ConnOK   = "ok"
	ConnWarn = "warn"
	ConnFail = "fail"
	ConnOff  = "off"
)

// ConnResult is what one connection test found.
type ConnResult struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Group    string   `json:"group"`
	Status   string   `json:"status"`
	Headline string   `json:"headline"`
	Details  []string `json:"details,omitempty"`
	Fix      string   `json:"fix,omitempty"`
	Ms       int64    `json:"ms,omitempty"`
	At       int64    `json:"at,omitempty"`
}

// ConnProbe describes one testable app for the pages.
type ConnProbe struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Group string   `json:"group"`
	Keys  []string `json:"keys"`

	addr  string
	needs []string
	run   func(*connRun)
	// patience replaces the usual wait for apps that are slow to answer.
	patience time.Duration
}

// slowConnHTTP is connHTTP with more patience, for apps like Plekt that can
// take a while to start answering. It's otherwise identical.
var slowConnHTTP = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		DisableKeepAlives:     true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Replaced in tests.
var (
	connTraktAPI = "https://api.trakt.tv"
	connSimklAPI = "https://api.simkl.com"
)

var connHTTP = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 9 * time.Second,
		DisableKeepAlives:     true,
	},
	// A redirect usually means a sign-in page, and could carry a key elsewhere.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type connRun struct {
	ctx  context.Context
	e    *Engine
	p    *ConnProbe
	v    func(string) string
	res  *ConnResult
	done bool
}

func (t *connRun) fail(headline, fix string) {
	t.res.Status, t.res.Headline, t.res.Fix = ConnFail, headline, fix
	t.done = true
}

func (t *connRun) warn(headline, fix string) {
	if t.res.Status != ConnFail {
		t.res.Status, t.res.Headline, t.res.Fix = ConnWarn, headline, fix
	}
}

func (t *connRun) add(format string, a ...any) {
	s := strings.TrimSpace(fmt.Sprintf(format, a...))
	if len(s) > 160 {
		s = s[:160]
	}
	if s != "" {
		t.res.Details = append(t.res.Details, s)
	}
}

func isLocalHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0"
}

// failNetwork explains a connection error without quoting its address.
func (t *connRun) failNetwork(target string, err error) {
	name := t.p.Name
	docker := ""
	if isLocalHost(target) {
		docker = " Inside Docker, localhost means this container, so use the server's IP or the app's container name."
	}
	var dns *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var expired x509.CertificateInvalidError
	var nerr net.Error
	switch {
	case errors.As(err, &dns):
		t.fail("Can't find that server name", "Check the spelling of the address, and that this container's DNS can resolve it."+docker)
	case errors.Is(err, syscall.ECONNREFUSED):
		t.fail("Nothing is answering at that address", "Check that "+name+" is running and that the port is right."+docker)
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		t.fail("Can't reach that network", "Check the IP address, and that this container can reach that network or VLAN.")
	case errors.As(err, &unknownCA):
		t.fail("Its HTTPS certificate isn't trusted", "Use a trusted certificate, or use the app's direct http:// address on your network.")
	case errors.As(err, &hostname):
		t.fail("Its HTTPS certificate is for a different name", "Use the address the certificate was issued for.")
	case errors.As(err, &expired):
		t.fail("Its HTTPS certificate is invalid or expired", "Renew the certificate, or use the app's direct address.")
	case errors.As(err, &nerr) && nerr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		t.fail("It didn't answer in time", "Check the address and port. A firewall can also cause this, or "+name+" may be overloaded.")
	case strings.Contains(err.Error(), "server gave HTTP response to HTTPS client"):
		t.fail("That port doesn't use HTTPS", "Change the address to start with http://.")
	case strings.Contains(err.Error(), "malformed HTTP response"), strings.Contains(err.Error(), "first record does not look like a TLS"):
		t.fail("That port isn't speaking the expected protocol", "Check the port, and whether the address should be http:// or https://.")
	default:
		t.fail("Couldn't connect", "Check the address and that "+name+" is running."+docker)
	}
}

func (t *connRun) failStatus(status int, body []byte) {
	name := t.p.Name
	switch {
	case status == 401 || status == 403:
		t.fail(name+" rejected the key", "Copy a fresh API key or token from "+name+" and save it.")
	case status >= 300 && status < 400:
		t.fail("It redirected instead of answering", "This is often a login page or proxy. Use the app's direct address, including http or https correctly.")
	case status == 404:
		t.fail("Reached a server, but not "+name+"'s API", "Check the port, and add the URL base if "+name+" runs under a path such as /"+strings.ToLower(strings.Fields(name)[0])+".")
	case status == 429:
		t.warn("It's limiting requests right now", "Wait a few minutes and test again.")
		t.done = true
	case status == 502 || status == 503 || status == 504:
		t.fail("Its proxy can't reach "+name, "Check that "+name+" is running behind that proxy.")
	case status >= 500:
		t.fail(name+" had an internal error", "Check "+name+"'s own logs.")
	default:
		t.fail(fmt.Sprintf("%s answered with an unexpected error (HTTP %d)", name, status), "Check the address and key.")
	}
}

type connResp struct {
	status int
	body   []byte
}

func (t *connRun) call(method, target string, hdr map[string]string, payload []byte) *connResp {
	if t.done {
		return nil
	}
	req, err := http.NewRequestWithContext(t.ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		t.fail("That address doesn't look right", "Enter the full address, such as http://192.168.1.10:8989.")
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "BackTrackarr (self-hosted)")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	client := connHTTP
	if t.p.patience > 0 {
		client = slowConnHTTP // the run's own deadline still applies
	}
	resp, err := client.Do(req)
	if err != nil {
		t.failNetwork(target, err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return &connResp{status: resp.StatusCode, body: body}
}

// get returns a successful JSON answer in out, or records what went wrong.
func (t *connRun) get(target string, hdr map[string]string, out any) bool {
	return t.finish(t.call(http.MethodGet, target, hdr, nil), out)
}

func (t *connRun) finish(r *connResp, out any) bool {
	if r == nil {
		return false
	}
	if r.status < 200 || r.status > 299 {
		t.failStatus(r.status, r.body)
		return false
	}
	if lead := bytes.TrimSpace(r.body); len(lead) > 0 && lead[0] == '<' {
		t.fail("Got a web page instead of "+t.p.Name+"'s API", "This is usually a login page, proxy or the wrong port. Use the app's direct address.")
		return false
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			// A list cut off at the size limit still proved the connection.
			var syn *json.SyntaxError
			if !(errors.As(err, &syn) && len(r.body) >= 4<<20) {
				t.fail("Got an answer that isn't "+t.p.Name, "Check that this address and port belong to "+t.p.Name+".")
				return false
			}
		}
	}
	return true
}

func base(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }

func gbText(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.0f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MB", b>>20)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// ---------- the apps ----------

func (e *Engine) connProbes() []*ConnProbe {
	simple := func(id, name, group, addr, secret string, run func(*connRun, string, string)) *ConnProbe {
		return &ConnProbe{ID: id, Name: name, Group: group, Keys: []string{addr, secret}, addr: addr, needs: []string{addr, secret},
			run: func(t *connRun) { run(t, base(t.v(addr)), t.v(secret)) }}
	}
	patient := func(p *ConnProbe, d time.Duration) *ConnProbe {
		p.patience = d
		return p
	}
	bearer := func(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }
	const (
		media   = "Media servers"
		track   = "Trackers"
		meta    = "Metadata and AI"
		history = "History and requests"
		books   = "Books"
		games   = "Games"
		backup  = "Backups"
	)
	list := []*ConnProbe{
		simple("plex", "Plex", media, "PLEX_URL", "PLEX_TOKEN", func(t *connRun, b, k string) {
			var id struct {
				MediaContainer struct {
					Version string `json:"version"`
				} `json:"MediaContainer"`
			}
			h := map[string]string{"X-Plex-Token": k}
			if !t.get(b+"/identity", h, &id) {
				return
			}
			var libs struct {
				MediaContainer struct {
					Directory []struct {
						Type string `json:"type"`
					} `json:"Directory"`
				} `json:"MediaContainer"`
			}
			if !t.get(b+"/library/sections", h, &libs) {
				return
			}
			t.add("Plex %s", id.MediaContainer.Version)
			if n := len(libs.MediaContainer.Directory); n == 1 {
				t.add("1 library")
			} else {
				t.add("%d libraries", n)
			}
		}),
		{ID: "trakt", Name: "Trakt", Group: track, Keys: []string{"TRAKT_CLIENT_ID", "TRAKT_CLIENT_SECRET"}, needs: []string{"TRAKT_CLIENT_ID"}, run: func(t *connRun) {
			if !t.get(connTraktAPI+"/movies/trending?limit=1", map[string]string{"trakt-api-version": "2", "trakt-api-key": t.v("TRAKT_CLIENT_ID")}, nil) {
				return
			}
			t.add("Client ID accepted")
			if s := e.Sink("trakt"); s == nil || !s.Ready(t.ctx) {
				t.warn("App is valid, but your account isn't connected", "Open Sync and click Connect next to Trakt.")
			} else {
				t.add("Account connected")
			}
		}},
		{ID: "simkl", Name: "Simkl", Group: track, Keys: []string{"SIMKL_CLIENT_ID"}, needs: []string{"SIMKL_CLIENT_ID"}, run: func(t *connRun) {
			if !t.get(connSimklAPI+"/search/id?imdb=tt0111161&client_id="+url.QueryEscape(t.v("SIMKL_CLIENT_ID")), nil, nil) {
				return
			}
			t.add("Client ID accepted")
			if s := e.Sink("simkl"); s == nil || !s.Ready(t.ctx) {
				t.warn("App is valid, but your account isn't connected", "Open Sync and click Connect next to Simkl.")
			} else {
				t.add("Account connected")
			}
		}},
		simple("floppy", "Floppy", track, "FLOPPY_URL", "FLOPPY_TOKEN", func(t *connRun, b, k string) {
			if t.get(b+"/api/v1/media", bearer(k), nil) {
				t.add("Token accepted")
			}
		}),
		{ID: "relays", Name: "Plex webhook relays", Group: track, Keys: []string{"RELAY_URLS"}, needs: []string{"RELAY_URLS"}, run: func(t *connRun) { t.testURLs("RELAY_URLS", "relay") }},
		{ID: "webhooks", Name: "JSON webhooks", Group: track, Keys: []string{"WEBHOOK_URLS"}, needs: []string{"WEBHOOK_URLS"}, run: func(t *connRun) { t.testURLs("WEBHOOK_URLS", "webhook") }},

		{ID: "tmdb", Name: "TMDB", Group: meta, Keys: []string{"TMDB_API_KEY"}, needs: []string{"TMDB_API_KEY"}, run: func(t *connRun) {
			k, target := t.v("TMDB_API_KEY"), base(e.cfg.TMDBURL)
			if target == "" {
				target = "https://api.themoviedb.org"
			}
			target += "/3/configuration"
			var h map[string]string
			if strings.HasPrefix(k, "eyJ") {
				h = bearer(k)
			} else {
				target += "?api_key=" + url.QueryEscape(k)
			}
			if t.get(target, h, nil) {
				t.add("Key accepted")
			}
		}},
		{ID: "claude", Name: "Claude", Group: meta, Keys: []string{"ANTHROPIC_API_KEY", "CLAUDE_MODEL"}, needs: []string{"ANTHROPIC_API_KEY"}, run: func(t *connRun) {
			b := base(e.cfg.AnthropicURL)
			if b == "" {
				b = "https://api.anthropic.com"
			}
			var res struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if !t.get(b+"/v1/models?limit=1000", map[string]string{"x-api-key": t.v("ANTHROPIC_API_KEY"), "anthropic-version": "2023-06-01"}, &res) {
				return
			}
			t.add("Key accepted, %s available", plural(len(res.Data), "model"))
			want, found := t.v("CLAUDE_MODEL"), false
			for _, m := range res.Data {
				found = found || m.ID == want
			}
			if want != "" && !found {
				t.warn("Key works, but the chosen model isn't available", "Change Claude model in Settings to one your key can use.")
			} else if want != "" {
				t.add("Using %s", want)
			}
		}},
		{ID: "gemini", Name: "Gemini", Group: meta, Keys: []string{"GEMINI_API_KEY", "GEMINI_MODEL"}, needs: []string{"GEMINI_API_KEY"}, run: func(t *connRun) {
			b := base(e.cfg.GeminiURL)
			if b == "" {
				b = "https://generativelanguage.googleapis.com"
			}
			r := t.call(http.MethodGet, b+"/v1beta/models?pageSize=1000", map[string]string{"x-goog-api-key": t.v("GEMINI_API_KEY")}, nil)
			if r != nil && r.status == 400 && bytes.Contains(r.body, []byte("API_KEY_INVALID")) {
				t.fail("Gemini rejected the key", "Create a key in Google AI Studio and save it.")
				return
			}
			var res struct {
				Models []struct {
					Name string `json:"name"`
				} `json:"models"`
			}
			if !t.finish(r, &res) {
				return
			}
			t.add("Key accepted, %s available", plural(len(res.Models), "model"))
			want, found := t.v("GEMINI_MODEL"), false
			if want == ModelAuto {
				t.add("Using the newest model automatically")
				return
			}
			for _, m := range res.Models {
				found = found || strings.TrimPrefix(m.Name, "models/") == want
			}
			if want != "" && !found {
				t.warn("Key works, but the chosen model isn't available", "Change Gemini model in Settings to one your key can use.")
			} else if want != "" {
				t.add("Using %s", want)
			}
		}},

		simple("tautulli", "Tautulli", history, "TAUTULLI_URL", "TAUTULLI_API_KEY", func(t *connRun, b, k string) {
			var res struct {
				Response struct {
					Result string `json:"result"`
					Data   struct {
						Version string `json:"tautulli_version"`
					} `json:"data"`
				} `json:"response"`
			}
			if !t.get(b+"/api/v2?"+url.Values{"apikey": {k}, "cmd": {"get_tautulli_info"}}.Encode(), nil, &res) {
				return
			}
			if res.Response.Result != "success" {
				t.fail("Tautulli rejected the key", "Copy the key from Tautulli's Settings, Web Interface, API.")
				return
			}
			t.add("Tautulli %s", res.Response.Data.Version)
		}),
		simple("crosswatch", "CrossWatch", history, "CROSSWATCH_URL", "CROSSWATCH_TOKEN", func(t *connRun, b, k string) {
			if t.get(b+"/api/app-auth/tokens/whoami", bearer(k), nil) {
				t.add("Token accepted")
			}
		}),
		patient(simple("plekt", "Plekt", history, "PLEKT_URL", "PLEKT_API_KEY", func(t *connRun, b, k string) {
			if t.get(b+"/api/backup/export?collection=watchHistory&limit=1", map[string]string{"X-Api-Key": k}, nil) {
				t.add("Key accepted, watch history is readable")
			}
		}), plektPatience),
		simple("seerr", "Jellyseerr or Overseerr", history, "SEERR_URL", "SEERR_API_KEY", func(t *connRun, b, k string) {
			var st struct {
				Version string `json:"version"`
			}
			if !t.get(b+"/api/v1/status", nil, &st) || !t.get(b+"/api/v1/auth/me", map[string]string{"X-Api-Key": k}, nil) {
				return
			}
			t.add("Version %s", st.Version)
			t.add("Key accepted")
		}),

		simple("audiobookshelf", "Audiobookshelf", books, "AUDIOBOOKSHELF_URL", "AUDIOBOOKSHELF_TOKEN", func(t *connRun, b, k string) {
			var me struct {
				Username string `json:"username"`
			}
			if t.get(b+"/api/me", bearer(k), &me) {
				t.add("Signed in as %s", me.Username)
			}
		}),
		simple("komga", "Komga", books, "KOMGA_URL", "KOMGA_API_KEY", func(t *connRun, b, k string) {
			if t.get(b+"/api/v1/users/me", map[string]string{"X-API-Key": k}, nil) {
				t.add("Key accepted")
			}
		}),
		simple("kavita", "Kavita", books, "KAVITA_URL", "KAVITA_API_KEY", func(t *connRun, b, k string) {
			if t.get(b+"/api/plugin/authkey-expires", map[string]string{"x-api-key": k}, nil) {
				t.add("Auth key accepted")
			}
		}),
		{ID: "hardcover", Name: "Hardcover", Group: books, Keys: []string{"HARDCOVER_TOKEN"}, needs: []string{"HARDCOVER_TOKEN"}, run: func(t *connRun) {
			k := strings.TrimSpace(strings.TrimPrefix(t.v("HARDCOVER_TOKEN"), "Bearer "))
			var res struct {
				Data struct {
					Me json.RawMessage `json:"me"`
				} `json:"data"`
				Errors json.RawMessage `json:"errors"`
			}
			r := t.call(http.MethodPost, base(e.cfg.Shelf.HardcoverURL)+"/v1/graphql", bearer(k), []byte(`{"query":"query { me { username } }"}`))
			if !t.finish(r, &res) {
				return
			}
			if len(res.Errors) > 0 && string(res.Errors) != "null" {
				t.fail("Hardcover rejected the token", "Copy a fresh token from hardcover.app, Settings, API.")
				return
			}
			var me []struct {
				Username string `json:"username"`
			}
			if json.Unmarshal(res.Data.Me, &me) == nil && len(me) > 0 {
				t.add("Signed in as %s", me[0].Username)
			} else {
				t.add("Token accepted")
			}
		}},
		{ID: "openlibrary", Name: "Open Library", Group: books, Keys: []string{"OPENLIBRARY_USER"}, needs: []string{"OPENLIBRARY_USER"}, run: func(t *connRun) {
			var res struct {
				NumFound int `json:"numFound"`
			}
			r := t.call(http.MethodGet, base(e.cfg.Shelf.OpenLibraryURL)+"/people/"+url.PathEscape(t.v("OPENLIBRARY_USER"))+"/books/already-read.json?limit=1", nil, nil)
			if r != nil && r.status == 404 {
				t.fail("Can't find that username", "Check the username, and that your reading log is set to public.")
				return
			}
			if t.finish(r, &res) {
				t.add("%s in your reading log", plural(res.NumFound, "book"))
			}
		}},

		{ID: "steam", Name: "Steam", Group: games, Keys: []string{"STEAM_API_KEY", "STEAM_ID"}, needs: []string{"STEAM_API_KEY", "STEAM_ID"}, run: func(t *connRun) {
			var res struct {
				Response struct {
					Count *int `json:"game_count"`
				} `json:"response"`
			}
			q := url.Values{"key": {t.v("STEAM_API_KEY")}, "steamid": {t.v("STEAM_ID")}, "format": {"json"}}
			if !t.get(base(e.cfg.Shelf.SteamURL)+"/IPlayerService/GetOwnedGames/v1/?"+q.Encode(), nil, &res) {
				return
			}
			if res.Response.Count == nil {
				t.warn("Key works, but your games aren't visible", "Set Game details to Public in your Steam privacy settings, and check the SteamID64.")
				return
			}
			t.add("%s owned", plural(*res.Response.Count, "game"))
		}},
		{ID: "retroachievements", Name: "RetroAchievements", Group: games, Keys: []string{"RETROACHIEVEMENTS_USER", "RETROACHIEVEMENTS_API_KEY"},
			needs: []string{"RETROACHIEVEMENTS_USER", "RETROACHIEVEMENTS_API_KEY"}, run: func(t *connRun) {
				q := url.Values{"u": {t.v("RETROACHIEVEMENTS_USER")}, "y": {t.v("RETROACHIEVEMENTS_API_KEY")}}
				var res struct {
					User string `json:"User"`
				}
				if t.get(base(e.cfg.Shelf.RetroURL)+"/API/API_GetUserProfile.php?"+q.Encode(), nil, &res) {
					t.add("Signed in as %s", res.User)
				}
			}},
		simple("romm", "RomM", games, "ROMM_URL", "ROMM_TOKEN", func(t *connRun, b, k string) {
			var me struct {
				Username string `json:"username"`
			}
			if t.get(b+"/api/users/me", bearer(k), &me) {
				t.add("Signed in as %s", me.Username)
			}
		}),
		{ID: "gdrive", Name: "Google Drive", Group: backup, Keys: []string{"GDRIVE_CLIENT_ID", "GDRIVE_CLIENT_SECRET"}, needs: []string{"GDRIVE_CLIENT_ID", "GDRIVE_CLIENT_SECRET"}, run: e.testDrive},
	}
	list = append(list, e.instanceProbes()...)
	order := map[string]int{media: 0, "Sonarr, Radarr, Whisparr": 1, track: 2, history: 3, meta: 4, books: 5, games: 6, backup: 7}
	sort.SliceStable(list, func(a, b int) bool { return order[list[a].Group] < order[list[b].Group] })
	return list
}

// testURLs checks that delivery addresses answer, without sending an event.
func (t *connRun) testURLs(key, word string) {
	var bad, warned int
	urls := strings.Split(t.v(key), ",")
	n := 0
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		n++
		label := word + " " + strconv.Itoa(n)
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			label = u.Host
		}
		if !config.ValidHTTPURL(raw) {
			t.add("%s: not a full web address", label)
			bad++
			continue
		}
		t.done = false
		r := t.call(http.MethodGet, raw, nil, nil)
		switch {
		case r == nil:
			t.add("%s: %s", label, strings.ToLower(t.res.Headline))
			bad++
			t.res.Status, t.res.Headline, t.res.Fix = "", "", ""
		case r.status == 401 || r.status == 403:
			t.add("%s: answered, but rejected the token in the address", label)
			warned++
		case r.status == 404:
			t.add("%s: answered, but that path wasn't found", label)
			warned++
		case r.status >= 500:
			t.add("%s: answered with a server error", label)
			warned++
		default:
			t.add("%s: reachable", label)
		}
	}
	t.done = false
	switch {
	case bad > 0:
		t.fail(plural(bad, word)+" can't be reached", "Use the complete address the other app gives you, starting with http:// or https://.")
	case warned > 0:
		t.warn(plural(warned, word)+" need attention", "Check the path and token in the address. Copy the whole URL from the other app.")
	default:
		t.res.Headline = "Reachable"
		t.add("No test event was sent, so nothing was recorded there")
	}
}

func (e *Engine) testDrive(t *connRun) {
	if !e.st.Encrypted() {
		t.warn("Encryption is off, so backups won't upload", "Turn on encryption first, on Security, Encryption and backups.")
	}
	if e.DriveConnected(t.ctx) {
		var about struct {
			User struct {
				Name string `json:"displayName"`
			} `json:"user"`
			Quota struct {
				Limit string `json:"limit"`
				Usage string `json:"usage"`
			} `json:"storageQuota"`
		}
		err := e.driveJSON(t.ctx, http.MethodGet, driveAPI+"/about?fields=user(displayName),storageQuota(limit,usage)", nil, &about)
		if err != nil {
			t.fail("Drive sign-in isn't working", "Disconnect and reconnect Google Drive on Security, Encryption and backups.")
			return
		}
		t.add("Connected as %s", about.User.Name)
		limit, _ := strconv.ParseInt(about.Quota.Limit, 10, 64)
		used, _ := strconv.ParseInt(about.Quota.Usage, 10, 64)
		if limit > 0 {
			t.add("%s free of %s", gbText(limit-used), gbText(limit))
			if limit-used < 1<<30 {
				t.warn("Drive is almost full", "Free up space, or lower Backups to keep in Drive.")
			}
		}
		if last := e.DriveLastUpload(t.ctx); last.Err != "" {
			t.warn("The last backup upload failed", "Run Back up now and check the Jobs panel.")
		} else if last.At > 0 {
			t.add("Last upload %s", time.Unix(last.At, 0).Format("2006-01-02 15:04"))
		}
		return
	}
	var dc map[string]any
	form := url.Values{"client_id": {t.v("GDRIVE_CLIENT_ID")}, "scope": {driveScope}}
	status, code, err := oauthPost(t.ctx, "/device/code", form, &dc)
	switch {
	case err != nil:
		t.fail("Couldn't reach Google", "Check this server's internet access and DNS.")
	case status >= 300 && code == "invalid_client":
		t.fail("Google doesn't recognize that client ID", "Copy the ID from the Google Auth Platform's Clients page. New clients can take a few minutes.")
	case status >= 300:
		t.fail("Google refused this client", "Check that the client type is TVs and Limited Input devices.")
	default:
		t.add("Client ID accepted")
		t.warn("Client is valid, but Drive isn't connected", "Click Connect Google Drive on Security, Encryption and backups.")
	}
}

// instanceProbes covers each numbered Sonarr, Radarr, Whisparr, Jellyfin and
// Emby server that's set up, plus the first of each so they can be added.
func (e *Engine) instanceProbes() []*ConnProbe {
	var out []*ConnProbe
	for _, c := range config.Settings() {
		if c.Instance == "" || !strings.HasSuffix(c.Key, "_URL") {
			continue
		}
		p := strings.TrimSuffix(c.Key, "_URL")
		if v, _ := config.Current(c); c.Number > 1 && v == "" {
			continue
		}
		name := strings.TrimSuffix(c.Label, " address")
		kind := strings.ToLower(c.Instance)
		urlKey, keyKey := p+"_URL", p+"_API_KEY"
		pr := &ConnProbe{ID: strings.ToLower(p), Name: name, Keys: []string{urlKey, keyKey}, addr: urlKey, needs: []string{urlKey, keyKey}}
		if kind == "jellyfin" || kind == "emby" {
			pr.Group = "Media servers"
			pr.run = func(t *connRun) {
				var info struct {
					Name    string `json:"ServerName"`
					Version string `json:"Version"`
				}
				if t.get(base(t.v(urlKey))+"/System/Info", map[string]string{"X-Emby-Token": t.v(keyKey)}, &info) {
					t.add("%s, version %s", info.Name, info.Version)
				}
			}
		} else {
			pr.Group = "Sonarr, Radarr, Whisparr"
			pr.run = func(t *connRun) { testArr(t, kind, base(t.v(urlKey)), t.v(keyKey)) }
		}
		out = append(out, pr)
	}
	return out
}

func testArr(t *connRun, kind, b, key string) {
	h := map[string]string{"X-Api-Key": key}
	var st struct {
		App     string `json:"appName"`
		Name    string `json:"instanceName"`
		Version string `json:"version"`
	}
	if !t.get(b+"/api/v3/system/status", h, &st) {
		return
	}
	if st.App != "" && !strings.EqualFold(st.App, kind) {
		t.fail("That address is "+st.App+", not "+t.p.Name, "Use the address and API key for "+t.p.Name+".")
		return
	}
	t.add("%s %s", firstNonEmpty(st.Name, st.App), st.Version)
	var roots []struct {
		Path       string `json:"path"`
		Accessible *bool  `json:"accessible"`
		Free       int64  `json:"freeSpace"`
	}
	if !t.get(b+"/api/v3/rootfolder", h, &roots) {
		return
	}
	var profiles []json.RawMessage
	if !t.get(b+"/api/v3/qualityprofile", h, &profiles) {
		return
	}
	var bad []string
	for _, r := range roots {
		if r.Accessible != nil && !*r.Accessible {
			bad = append(bad, r.Path)
		} else {
			t.add("%s: %s free", r.Path, gbText(r.Free))
		}
	}
	t.add("%s", plural(len(profiles), "quality profile"))
	switch {
	case len(roots) == 0:
		t.warn("Connected, but it has no root folders", "Add a root folder in "+t.p.Name+"'s Media Management settings.")
	case len(bad) > 0:
		t.warn("Connected, but a root folder is unavailable: "+strings.Join(bad, ", "), "Check that folder's mount and permissions in "+t.p.Name+".")
	case len(profiles) == 0:
		t.warn("Connected, but it has no quality profiles", "Add a quality profile in "+t.p.Name+".")
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---------- running ----------

func settingNow(key string) (string, bool) {
	for _, c := range config.Settings() {
		if c.Key == key {
			v, _ := config.Current(c)
			return v, c.Type == "secret"
		}
	}
	return "", false
}

// ConnProbes lists every testable app.
func (e *Engine) ConnProbes() []ConnProbe {
	list := e.connProbes()
	out := make([]ConnProbe, len(list))
	for i, p := range list {
		out[i] = *p
	}
	return out
}

const connResultKey = "conn|"

// ConnStates returns each app with its last saved result.
func (e *Engine) ConnStates(ctx context.Context) []ConnResult {
	list := e.connProbes()
	out := make([]ConnResult, 0, len(list))
	for _, p := range list {
		r := ConnResult{ID: p.ID, Name: p.Name, Group: p.Group, Status: ConnOff, Headline: "Not set up"}
		ready := true
		for _, k := range p.needs {
			v, _ := settingNow(k)
			ready = ready && v != ""
		}
		if ready {
			r.Status, r.Headline = "", "Not tested yet"
			if v, _ := e.st.GetKV(ctx, connResultKey+p.ID); v != "" {
				var last ConnResult
				if json.Unmarshal([]byte(v), &last) == nil {
					last.Name, last.Group = p.Name, p.Group
					r = last
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// TestConnection runs one app's test. posted holds unsaved values from the
// Settings page. A saved secret is never sent to an address that isn't saved.
func (e *Engine) TestConnection(ctx context.Context, id string, posted map[string]string) (ConnResult, bool) {
	var p *ConnProbe
	for _, x := range e.connProbes() {
		if x.ID == id {
			p = x
			break
		}
	}
	if p == nil {
		return ConnResult{}, false
	}
	allowed := make(map[string]string, len(posted))
	for _, k := range p.Keys {
		if v := strings.TrimSpace(posted[k]); v != "" {
			allowed[k] = v
		}
	}
	addrChanged := false
	if p.addr != "" {
		if v, ok := allowed[p.addr]; ok {
			saved, _ := settingNow(p.addr)
			addrChanged = base(v) != base(saved)
		}
	}
	val := func(k string) string {
		if v, ok := allowed[k]; ok {
			return v
		}
		v, secret := settingNow(k)
		if secret && addrChanged {
			return ""
		}
		return v
	}
	res := ConnResult{ID: p.ID, Name: p.Name, Group: p.Group, Status: ConnOK, Headline: "Connected", At: time.Now().Unix()}
	var missing []string
	for _, k := range p.needs {
		if val(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		res.Status, res.Headline = ConnOff, "Not set up"
		res.Fix = "Fill in " + connFieldNames(missing) + "."
		if addrChanged {
			res.Status, res.Headline = ConnFail, "Enter the key for this new address"
			res.Fix = "A saved key is only sent to its saved address, to keep it safe."
		}
		return res, true
	}
	if p.addr != "" && !config.ValidHTTPURL(val(p.addr)) {
		res.Status, res.Headline, res.Fix = ConnFail, "That address isn't complete", "Start it with http:// or https://, and include the port."
		return res, true
	}
	wait := 25 * time.Second
	if p.patience > 0 {
		wait = p.patience
	}
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	start := time.Now()
	t := &connRun{ctx: cctx, e: e, p: p, v: val, res: &res}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.fail("The test hit an internal error", "Try again, and check BackTrackarr's logs if it continues.")
			}
		}()
		p.run(t)
	}()
	res.Ms = time.Since(start).Milliseconds()
	if res.Status == ConnOK && res.Headline == "" {
		res.Headline = "Connected"
	}
	if len(allowed) == 0 {
		if b, err := json.Marshal(res); err == nil {
			_ = e.st.SetKV(context.WithoutCancel(ctx), connResultKey+p.ID, string(b))
		}
	}
	return res, true
}

func connFieldNames(keys []string) string {
	var names []string
	for _, k := range keys {
		for _, c := range config.Settings() {
			if c.Key == k {
				names = append(names, c.Label)
				break
			}
		}
	}
	return strings.Join(names, " and ")
}
