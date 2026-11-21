package web

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
)

// The Setup page fills in everything it can find, so the user mostly clicks
// and only pastes the few keys an app won't hand over.

type plexSignin struct {
	pin     *plex.Pin
	created time.Time
	token   string
	servers []plex.Server
	done    bool
	err     string
}

type setupState struct {
	mu   sync.Mutex
	pins map[int64]*plexSignin
}

func (s *Server) routesSetup(admin *http.ServeMux) {
	admin.HandleFunc("GET /setup", s.setupPage)
	admin.HandleFunc("POST /setup/plex/start", s.setupPlexStart)
	admin.HandleFunc("POST /setup/plex/status", s.setupPlexStatus)
	admin.HandleFunc("POST /setup/plex/choose", s.setupPlexChoose)
	admin.HandleFunc("POST /setup/plex/people", s.setupPlexPeople)
	admin.HandleFunc("POST /setup/scan", s.setupScan)
	admin.HandleFunc("POST /setup/app/options", s.setupAppOptions)
	admin.HandleFunc("POST /setup/app", s.setupApp)
	admin.HandleFunc("POST /setup/streaming", s.setupStreaming)
	admin.HandleFunc("POST /recs/{id}/seerr", s.recToSeerr)
}

func jsonFail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(M{"error": msg})
}

func (s *Server) restartSoon() { s.requestRestart() }

type setupPerson struct {
	Name    string
	Checked bool
	Owner   bool
}

type setupLibrary struct {
	Title, Type string
	Private     bool
}

type setupConnected struct {
	Label, URL string
}

var adultName = regexp.MustCompile(`(?i)\b(adult|xxx|nsfw|porn|private)\b`)

// plexPeople reads users and libraries from the connected server. A saved
// choice is kept. Before one exists, the owner is selected, and libraries that
// look adult are marked private, the safer default.
func (s *Server) plexPeople(ctx context.Context) ([]setupPerson, []setupLibrary, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	c := plex.NewClient(s.cfg.PlexURL, s.cfg.PlexToken)
	accounts, err := c.Accounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, nil, err
	}
	chosen := map[string]bool{}
	for _, u := range s.cfg.ScrobbleUsers {
		chosen[strings.ToLower(u)] = true
	}
	var people []setupPerson
	for id, name := range accounts {
		if name == "" || id == "0" {
			continue
		}
		owner := id == "1"
		people = append(people, setupPerson{Name: name, Owner: owner, Checked: chosen[strings.ToLower(name)] || (len(chosen) == 0 && owner)})
	}
	sort.Slice(people, func(a, b int) bool {
		if people[a].Owner != people[b].Owner {
			return people[a].Owner
		}
		return strings.ToLower(people[a].Name) < strings.ToLower(people[b].Name)
	})
	private := map[string]bool{}
	for _, l := range s.cfg.PrivateLibraries {
		private[strings.ToLower(l)] = true
	}
	_, savedChoice := config.Lookup("PRIVATE_LIBRARIES")
	if c, ok := config.Lookup("PRIVATE_LIBRARIES"); ok {
		_, src := config.Current(c)
		savedChoice = src != "default"
	}
	var libs []setupLibrary
	for _, sec := range sections {
		libs = append(libs, setupLibrary{Title: sec.Title, Type: sec.Type,
			Private: private[strings.ToLower(sec.Title)] || (!savedChoice && adultName.MatchString(sec.Title))})
	}
	return people, libs, nil
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := M{"Title": "Setup", "Nav": "setup", "Flash": takeFlash(w, r), "PlexConnected": s.cfg.PlexURL != "" && s.cfg.PlexToken != "",
		"PlexURL": s.cfg.PlexURL, "BaseURL": s.cfg.BaseURL, "TZ": s.cfg.TimeZone, "Region": s.cfg.StreamingRegion}
	if s.cfg.BaseURL == "" {
		data["BaseURL"] = s.baseURL(r)
	}
	behindProxy := s.cfg.TrustProxy || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != ""
	data["BehindProxy"] = behindProxy
	if data["PlexConnected"] == true {
		people, libs, err := s.plexPeople(ctx)
		if err != nil {
			data["PlexErr"] = "Can't reach your Plex server right now."
		}
		data["People"], data["Libraries"] = people, libs
	}
	var apps []setupConnected
	for _, a := range s.cfg.Arr {
		apps = append(apps, setupConnected{Label: a.Name, URL: a.URL})
	}
	for _, m := range s.cfg.MediaServers {
		apps = append(apps, setupConnected{Label: m.Name, URL: m.URL})
	}
	c := s.cfg.Shelf
	for _, a := range []setupConnected{{"Jellyseerr / Overseerr", s.cfg.SeerrURL}, {"Tautulli", s.cfg.TautulliURL}, {"Floppy", s.cfg.FloppyURL}, {"CrossWatch", s.cfg.CrossWatchURL}, {"Plekt", s.cfg.PlektURL}, {"Audiobookshelf", c.AudiobookshelfURL}, {"Komga", c.KomgaURL}, {"Kavita", c.KavitaURL}, {"RomM", c.RommURL}} {
		if a.URL != "" {
			apps = append(apps, a)
		}
	}
	data["Apps"] = apps
	data["AppKinds"] = core.AppKinds
	mine := map[string]bool{}
	for _, v := range s.cfg.MyServices {
		mine[strings.ToLower(v)] = true
	}
	type serviceRow struct {
		Name    string
		Checked bool
	}
	var services []serviceRow
	for _, n := range streamingChoices {
		services = append(services, serviceRow{n, mine[strings.ToLower(n)]})
	}
	data["Services"] = services
	keys := []struct{ Key, Label, Why, Link string }{
		{"TMDB_API_KEY", "TMDB", "Matches streaming titles and shows where to watch", "https://www.themoviedb.org/settings/api"},
		{"STEAM_API_KEY", "Steam key", "Tracks your Steam games", "https://steamcommunity.com/dev/apikey"},
		{"HARDCOVER_TOKEN", "Hardcover", "Tracks your reading", "https://hardcover.app/account/api"},
		{"RETROACHIEVEMENTS_API_KEY", "RetroAchievements", "Tracks retro games", "https://retroachievements.org/settings"},
		{"ANTHROPIC_API_KEY", "Claude", "AI recommendations", "https://console.anthropic.com/settings/keys"},
		{"GEMINI_API_KEY", "Gemini", "AI recommendations", "https://aistudio.google.com/apikey"},
	}
	type keyRow struct {
		Key, Label, Why, Link string
		IsSet                 bool
	}
	var rows []keyRow
	for _, k := range keys {
		st, _ := config.Lookup(k.Key)
		v, _ := config.Current(st)
		rows = append(rows, keyRow{k.Key, k.Label, k.Why, k.Link, v != ""})
	}
	data["Keys"] = rows
	s.render(w, "setup", data)
}

func (s *Server) plexClientID(ctx context.Context) (string, error) {
	return s.st.EnsureSecret(ctx, "plex_client_id")
}

func (s *Server) setupPlexStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := s.plexClientID(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	pin, err := plex.CreatePin(ctx, s.cfg.PlexTVURL, id)
	if err != nil {
		jsonFail(w, http.StatusBadGateway, "Couldn't reach Plex. Try again in a moment.")
		return
	}
	s.setup.mu.Lock()
	for k, p := range s.setup.pins {
		if time.Since(p.created) > 20*time.Minute {
			delete(s.setup.pins, k)
		}
	}
	s.setup.pins[pin.ID] = &plexSignin{pin: pin, created: time.Now()}
	s.setup.mu.Unlock()
	writeJSON(w, M{"id": pin.ID, "auth_url": plex.AuthURL(id, pin.Code)})
}

func (s *Server) savePlexServer(ctx context.Context, srv plex.Server) (string, error) {
	addr, ok := plex.BestAddress(ctx, srv)
	if !ok {
		return "", errUnreachable
	}
	if err := s.saveSettings(ctx, map[string]string{"PLEX_URL": addr, "PLEX_TOKEN": srv.AccessToken}, nil); err != nil {
		return "", err
	}
	s.log.Info("plex connected from setup", "server", srv.Name)
	s.restartSoon()
	return addr, nil
}

var errUnreachable = jsonError2("BackTrackarr can't reach that Plex server from where it's running.")

type jsonError2 string

func (e jsonError2) Error() string { return string(e) }

func (s *Server) setupPlexStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pinID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	s.setup.mu.Lock()
	p := s.setup.pins[pinID]
	s.setup.mu.Unlock()
	if p == nil {
		jsonFail(w, http.StatusNotFound, "That sign-in expired. Start again.")
		return
	}
	clientID, err := s.plexClientID(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	token, err := plex.CheckPin(ctx, s.cfg.PlexTVURL, clientID, p.pin)
	if err != nil {
		jsonFail(w, http.StatusBadGateway, "Couldn't reach Plex. Try again in a moment.")
		return
	}
	if token == "" {
		writeJSON(w, M{"state": "waiting"})
		return
	}
	servers, err := plex.OwnedServers(ctx, s.cfg.PlexTVURL, clientID, token)
	if err != nil {
		jsonFail(w, http.StatusBadGateway, "Signed in, but couldn't list your servers. Try again.")
		return
	}
	switch len(servers) {
	case 0:
		jsonFail(w, http.StatusNotFound, "This Plex account doesn't own a server.")
	case 1:
		if _, err := s.savePlexServer(ctx, servers[0]); err != nil {
			jsonFail(w, http.StatusBadGateway, err.Error())
			return
		}
		s.setup.mu.Lock()
		delete(s.setup.pins, pinID)
		s.setup.mu.Unlock()
		writeJSON(w, M{"state": "done", "server": servers[0].Name})
	default:
		s.setup.mu.Lock()
		p.token, p.servers = token, servers
		s.setup.mu.Unlock()
		names := make([]M, len(servers))
		for i, v := range servers {
			names[i] = M{"index": i, "name": v.Name}
		}
		writeJSON(w, M{"state": "choose", "servers": names})
	}
}

func (s *Server) setupPlexChoose(w http.ResponseWriter, r *http.Request) {
	pinID, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	idx, err := strconv.Atoi(r.FormValue("index"))
	s.setup.mu.Lock()
	p := s.setup.pins[pinID]
	s.setup.mu.Unlock()
	if p == nil || err != nil || idx < 0 || idx >= len(p.servers) {
		jsonFail(w, http.StatusBadRequest, "That sign-in expired. Start again.")
		return
	}
	if _, err := s.savePlexServer(r.Context(), p.servers[idx]); err != nil {
		jsonFail(w, http.StatusBadGateway, err.Error())
		return
	}
	s.setup.mu.Lock()
	delete(s.setup.pins, pinID)
	s.setup.mu.Unlock()
	writeJSON(w, M{"state": "done", "server": p.servers[idx].Name})
}

func (s *Server) setupPlexPeople(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "/setup", "Could not read the form.")
		return
	}
	set := map[string]string{"SCROBBLE_USERS": strings.Join(r.PostForm["users"], ","),
		"PRIVATE_LIBRARIES": strings.Join(r.PostForm["private"], ",")}
	if err := s.saveSettings(r.Context(), set, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.applySettings(w, r, "Saved.")
}

func (s *Server) setupScan(w http.ResponseWriter, r *http.Request) {
	urls := []string{s.cfg.PlexURL, s.cfg.FloppyURL}
	known := map[string]bool{}
	add := func(u string) {
		urls = append(urls, u)
		known[strings.TrimRight(strings.ToLower(u), "/")] = true
	}
	for _, a := range s.cfg.Arr {
		add(a.URL)
	}
	for _, m := range s.cfg.MediaServers {
		add(m.URL)
	}
	c := s.cfg.Shelf
	for _, u := range []string{s.cfg.SeerrURL, s.cfg.TautulliURL, s.cfg.CrossWatchURL, s.cfg.PlektURL, c.AudiobookshelfURL, c.KomgaURL, c.KavitaURL, c.RommURL} {
		if u != "" {
			add(u)
		}
	}
	subnets := core.ScanSubnets(urls)
	if len(subnets) == 0 {
		jsonFail(w, http.StatusBadRequest, "Connect Plex first, so BackTrackarr knows which network to look on.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out := []core.FoundApp{}
	for _, f := range core.FindApps(ctx, subnets) {
		if !known[strings.ToLower(f.URL)] {
			out = append(out, f)
		}
	}
	writeJSON(w, M{"apps": out})
}

// appKeys returns the settings for a new app. Sonarr, Radarr, Whisparr,
// Jellyfin and Emby can have several servers, so it takes the next free slot.
func appKeys(kind, addr string) (urlKey, keyKey, nameKey string, ok bool) {
	switch kind {
	case "audiobookshelf":
		return "AUDIOBOOKSHELF_URL", "AUDIOBOOKSHELF_TOKEN", "", true
	case "komga":
		return "KOMGA_URL", "KOMGA_API_KEY", "", true
	case "kavita":
		return "KAVITA_URL", "KAVITA_API_KEY", "", true
	case "romm":
		return "ROMM_URL", "ROMM_TOKEN", "", true
	case "seerr":
		return "SEERR_URL", "SEERR_API_KEY", "", true
	case "tautulli":
		return "TAUTULLI_URL", "TAUTULLI_API_KEY", "", true
	case "floppy":
		return "FLOPPY_URL", "FLOPPY_TOKEN", "", true
	case "crosswatch":
		return "CROSSWATCH_URL", "CROSSWATCH_TOKEN", "", true
	case "plekt":
		return "PLEKT_URL", "PLEKT_API_KEY", "", true
	case "sonarr", "radarr", "whisparr", "jellyfin", "emby":
	default:
		return "", "", "", false
	}
	base := strings.ToUpper(kind)
	for n := 1; n <= 9; n++ {
		p := base
		if n > 1 {
			p += "_" + strconv.Itoa(n)
		}
		st, found := config.Lookup(p + "_URL")
		if !found {
			continue
		}
		cur, _ := config.Current(st)
		if cur == "" || strings.EqualFold(strings.TrimRight(cur, "/"), addr) {
			return p + "_URL", p + "_API_KEY", p + "_NAME", true
		}
	}
	return "", "", "", false
}

func (s *Server) setupApp(w http.ResponseWriter, r *http.Request) {
	kind, key := r.FormValue("kind"), strings.TrimSpace(r.FormValue("key"))
	addr := strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/")
	if key == "" {
		jsonFail(w, http.StatusBadRequest, "Paste the key first.")
		return
	}
	name, err := core.TestApp(r.Context(), kind, addr, key)
	if err != nil {
		jsonFail(w, http.StatusBadRequest, "Not saved: "+err.Error()+".")
		return
	}
	urlKey, keyKey, nameKey, ok := appKeys(kind, addr)
	if !ok {
		jsonFail(w, http.StatusBadRequest, "No free slot for another one of these.")
		return
	}
	set := map[string]string{urlKey: addr, keyKey: key}
	if v := strings.TrimSpace(r.FormValue("name")); v != "" {
		name = v
	}
	if nameKey != "" && name != "" {
		if clean := strings.ToLower(strings.ReplaceAll(name, " ", "-")); s.instanceNameFree(clean, kind, urlKey, addr) {
			set[nameKey] = clean
		}
	}
	if isArr(kind) {
		p := strings.TrimSuffix(urlKey, "_URL")
		for form, suffix := range map[string]string{"root": "_ROOT_FOLDER", "profile": "_QUALITY_PROFILE"} {
			if v := strings.TrimSpace(r.FormValue(form)); v != "" {
				set[p+suffix] = v
			}
		}
		if _, ok := config.Lookup(p + "_ADULT"); ok {
			set[p+"_ADULT"] = strconv.FormatBool(r.FormValue("adult") == "1")
		}
	}
	if err := s.saveSettings(r.Context(), set, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("app connected from setup", "kind", kind)
	s.restartSoon()
	writeJSON(w, M{"ok": true})
}

func isArr(kind string) bool { return kind == "sonarr" || kind == "radarr" || kind == "whisparr" }

// instanceNameFree: names identify instances in the archive, so two servers
// must never share one. If taken, the app's numbered default is used instead.
func (s *Server) instanceNameFree(name, kind, urlKey, addr string) bool {
	if name == kind && urlKey != strings.ToUpper(kind)+"_URL" {
		return false
	}
	for _, a := range s.cfg.Arr {
		if a.Name == name && !strings.EqualFold(strings.TrimRight(a.URL, "/"), addr) {
			return false
		}
	}
	for _, m := range s.cfg.MediaServers {
		if m.Name == name && !strings.EqualFold(strings.TrimRight(m.URL, "/"), addr) {
			return false
		}
	}
	return true
}

// setupAppOptions checks a key and returns the choices for that app.
func (s *Server) setupAppOptions(w http.ResponseWriter, r *http.Request) {
	kind, key := r.FormValue("kind"), strings.TrimSpace(r.FormValue("key"))
	addr := strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/")
	if key == "" {
		jsonFail(w, http.StatusBadRequest, "Paste the key first.")
		return
	}
	name, err := core.TestApp(r.Context(), kind, addr, key)
	if err != nil {
		jsonFail(w, http.StatusBadRequest, "Not saved: "+err.Error()+".")
		return
	}
	if !isArr(kind) {
		writeJSON(w, M{"is_arr": false})
		return
	}
	roots, profiles, err := core.ArrOptions(r.Context(), kind, addr, key)
	if err != nil {
		jsonFail(w, http.StatusBadGateway, "Connected, but "+err.Error()+".")
		return
	}
	writeJSON(w, M{"is_arr": true, "name": name, "roots": roots, "profiles": profiles, "always_adult": kind == "whisparr"})
}

// streamingChoices are written the way TMDB names them.
var streamingChoices = []string{"Netflix", "Disney Plus", "Hulu", "Amazon Prime Video", "Max", "Paramount Plus",
	"Peacock", "Apple TV Plus", "Crunchyroll", "Starz", "AMC+", "Shudder", "BritBox", "Criterion Channel", "Mubi", "Tubi", "Pluto TV"}

func (s *Server) setupStreaming(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "/setup", "Could not read the form.")
		return
	}
	if err := s.saveSettings(r.Context(), map[string]string{"MY_STREAMING_SERVICES": strings.Join(r.PostForm["services"], ",")}, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.applySettings(w, r, "Saved.")
}

func (s *Server) recToSeerr(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	msg, err := s.eng.RequestInSeerr(r.Context(), id)
	if err != nil {
		msg = "Could not request: " + err.Error()
	}
	s.flash(w, r, "/recs", msg)
}
