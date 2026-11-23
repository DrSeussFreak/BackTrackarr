package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
)

func (s *Server) routesAutoDelete(admin *http.ServeMux) {
	admin.HandleFunc("GET /autodelete", s.autoDeletePage)
	admin.HandleFunc("POST /autodelete/rule", s.autoDeleteRule)
	admin.HandleFunc("POST /autodelete/settings", s.autoDeleteSettings)
	admin.HandleFunc("POST /autodelete/run", s.autoDeleteRun)
	admin.HandleFunc("GET /api/now-playing", s.apiNowPlaying)
	admin.HandleFunc("POST /api/now-playing/end", s.apiEndSession)
	admin.HandleFunc("POST /import/source/{name}", s.importSource)
}

func (s *Server) apiNowPlaying(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.NowPlaying(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for i := range rows {
		rows[i].FinishAt = s.eng.FinishPercent(rows[i].Media.Kind)
		rows[i].CanStop = s.eng.CanStopPlayback(rows[i].Source)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, M{"items": rows})
}

// apiEndSession stops a Plex stream and removes it from Now Playing. Sources
// BackTrackarr can't control are only removed from the list.
func (s *Server) apiEndSession(w http.ResponseWriter, r *http.Request) {
	res, err := s.eng.EndPlayback(r.Context(), r.FormValue("key"))
	var shown core.EndPlaybackError
	if errors.As(err, &shown) {
		jsonError(w, http.StatusBadGateway, shown.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !res.Found {
		jsonError(w, http.StatusNotFound, "That session has already ended.")
		return
	}
	writeJSON(w, M{"ok": true, "stopped": res.Stopped})
}

func (s *Server) importSource(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.eng.StartSourceImport(name); err != nil {
		s.flash(w, r, "/import", "Couldn't start the import: "+err.Error())
		return
	}
	back(w, r, "/import")
}

// knownUsers combines Plex accounts with anyone who has plays here.
func (s *Server) knownUsers(ctx context.Context) []string {
	seen := map[string]string{}
	if s.cfg.PlexURL != "" && s.cfg.PlexToken != "" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if acc, err := plex.NewClient(s.cfg.PlexURL, s.cfg.PlexToken).Accounts(cctx); err == nil {
			for id, n := range acc {
				if n != "" && id != "0" {
					seen[strings.ToLower(n)] = n
				}
			}
		}
	}
	if users, err := s.st.Users(ctx); err == nil {
		for _, u := range users {
			if u.Name != "" {
				if _, ok := seen[strings.ToLower(u.Name)]; !ok {
					seen[strings.ToLower(u.Name)] = u.Name
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, n := range seen {
		out = append(out, n)
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a]) < strings.ToLower(out[b]) })
	return out
}

type pickRow struct {
	Name    string
	Checked bool
}

func picks(all, chosen []string) []pickRow {
	on := map[string]bool{}
	for _, c := range chosen {
		on[strings.ToLower(c)] = true
	}
	out := make([]pickRow, 0, len(all))
	for _, n := range all {
		out = append(out, pickRow{n, on[strings.ToLower(n)]})
	}
	return out
}

type deleteTitleRow struct {
	Instance, Kind, Title string
	ArrID                 int64
	Year, GraceDays       int
	Enabled               bool
	Users                 []pickRow
	Assigned              string
}

func (s *Server) autoDeletePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("q"))
	onlyOn := q.Get("on") == "1"
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	titles, err := s.st.DeleteTitles(ctx, search, onlyOn, offset)
	if err != nil {
		s.fail(w, err)
		return
	}
	users := s.knownUsers(ctx)
	rows := make([]deleteTitleRow, 0, len(titles))
	for _, t := range titles {
		rows = append(rows, deleteTitleRow{Instance: t.Instance, Kind: t.Kind, Title: t.Title, ArrID: t.ArrID, Year: t.Year, GraceDays: t.GraceDays,
			Enabled: t.Enabled, Users: picks(users, t.Users), Assigned: strings.Join(t.Users, ", ")})
	}
	var ready []core.DeleteCandidate
	var problems []string
	if q.Get("preview") == "1" {
		if ready, problems, err = s.eng.PreviewAutoDelete(ctx); err != nil {
			s.fail(w, err)
			return
		}
	}
	scope := s.eng.AutoDeleteScope(ctx)
	servers := make([]string, 0, len(s.cfg.Arr))
	for _, a := range s.eng.ArrInstances() {
		servers = append(servers, a.Name)
	}
	libraries, _ := s.st.PlexLibraryNames(ctx)
	log, _ := s.st.DeleteLog(ctx, 25)
	next := url.Values{"offset": {strconv.Itoa(offset + 100)}}
	if search != "" {
		next.Set("q", search)
	}
	if onlyOn {
		next.Set("on", "1")
	}
	s.render(w, "autodelete", M{"Title": "Auto-delete", "Nav": "autodelete", "On": s.eng.AutoDeleteOn(ctx), "Rows": rows, "Search": search, "OnlyOn": onlyOn,
		"HasUsers": len(users) > 0, "Previewed": q.Get("preview") == "1", "Ready": ready, "Problems": problems, "Scope": scope,
		"Servers": picks(servers, scope.Servers), "Libraries": picks(libraries, scope.Libraries), "Log": log, "NextURL": "?" + next.Encode(),
		"HasMore": len(rows) == 100, "Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func (s *Server) autoDeleteRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "/autodelete", "Could not read the form.")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("arr_id"), 10, 64)
	grace, err := strconv.Atoi(r.FormValue("grace"))
	if err != nil || grace < 0 {
		grace = 1
	}
	users := r.PostForm["users"]
	enabled := r.FormValue("enabled") == "1"
	if err := s.st.SaveDeleteRule(r.Context(), r.FormValue("instance"), id, users, enabled, grace); err != nil {
		s.flash(w, r, "/autodelete", "Not saved: "+err.Error()+".")
		return
	}
	msg := "Saved. This title won't be deleted."
	if enabled && len(users) > 0 {
		msg = "Saved. Files are removed after all " + strconv.Itoa(len(users)) + " chosen users watch them, while the master switch is on."
	} else if enabled {
		msg = "Saved but left off: choose at least one user."
	}
	to := "/autodelete"
	if q := r.FormValue("return"); strings.HasPrefix(q, "?") {
		to += q
	}
	s.flash(w, r, to, msg)
}

func scopeMode(v string) string {
	if v == "include" || v == "exclude" {
		return v
	}
	return "all"
}

func (s *Server) autoDeleteSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "/autodelete", "Could not read the form.")
		return
	}
	sc := core.DeleteScope{ServerMode: scopeMode(r.FormValue("server_mode")), Servers: r.PostForm["servers"],
		LibraryMode: scopeMode(r.FormValue("library_mode")), Libraries: r.PostForm["libraries"]}
	if err := s.eng.SetAutoDeleteScope(r.Context(), sc); err != nil {
		s.fail(w, err)
		return
	}
	on := r.FormValue("master") == "1"
	if err := s.eng.SetAutoDelete(r.Context(), on); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("auto-delete settings changed", "on", on)
	if on {
		s.flash(w, r, "/autodelete", "Auto-delete is ON for titles with a rule.")
		return
	}
	s.flash(w, r, "/autodelete", "Auto-delete is off. Nothing will be deleted.")
}

func (s *Server) autoDeleteRun(w http.ResponseWriter, r *http.Request) {
	s.eng.Jobs.Start(s.eng.Context(), "auto-delete", s.eng.RunAutoDeleteOnce)
	back(w, r, "/autodelete")
}
