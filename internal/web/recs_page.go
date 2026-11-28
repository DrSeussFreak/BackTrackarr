package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func (s *Server) routesRecs(admin *http.ServeMux) {
	admin.HandleFunc("GET /recs", s.recsPage)
	admin.HandleFunc("POST /recs", s.recsRun)
	admin.HandleFunc("POST /recs/{id}/arr", s.recToArr)
	admin.HandleFunc("POST /recs/{id}/ignore", s.recIgnore)
	admin.HandleFunc("POST /recs/unignore", s.recUnignore)
}

// A person's page keeps at least this many titles; ignoring below it asks for more.
const recTopUpBelow = 5

type recSourceGroup struct {
	Name    string
	Sources []core.RecSource
}

// recsUser is whose recommendations the page shows.
func (s *Server) recsUser(r *http.Request, users []store.NameCount) string {
	if r.URL.Query().Has("user") {
		return r.URL.Query().Get("user")
	}
	return s.defaultUser(users)
}

func recsURL(user string) string {
	return "/recs?user=" + url.QueryEscape(user)
}

func (s *Server) recsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, _ := s.st.Users(ctx)
	user := s.recsUser(r, users)
	all, err := s.st.RecRuns(ctx, 30)
	if err != nil {
		s.fail(w, err)
		return
	}
	ignored, err := s.st.IgnoredRecs(ctx, user)
	if err != nil {
		s.fail(w, err)
		return
	}
	hide := map[string]bool{}
	for _, g := range ignored {
		hide[g.Kind+"|"+g.Norm] = true
	}
	// This person's runs, newest first. A failed run is shown only while it's
	// the newest from that provider, so the last good list stays on screen.
	var runs []*store.RecRun
	newest := map[string]bool{}
	var last int64
	for _, run := range all {
		if run.User != user {
			continue
		}
		if run.CreatedAt > last {
			last = run.CreatedAt
		}
		first := !newest[run.Provider]
		newest[run.Provider] = true
		if run.Error != "" && !first {
			continue
		}
		kept := run.Recs[:0:0]
		for _, c := range run.Recs {
			if !hide[store.RecKey(c.Kind, c.Title)] {
				kept = append(kept, c)
			}
		}
		run.Recs = kept
		if len(runs) < 8 {
			runs = append(runs, run)
		}
	}

	var groups []recSourceGroup
	checked := 0
	sources := s.eng.RecSources(ctx, user)
	for _, src := range sources {
		if len(groups) == 0 || groups[len(groups)-1].Name != src.Group {
			groups = append(groups, recSourceGroup{Name: src.Group})
		}
		g := &groups[len(groups)-1]
		g.Sources = append(g.Sources, src)
		if src.Checked {
			checked++
		}
	}

	type arrOpt struct{ Name, Kind string }
	var arr []arrOpt
	var movieArr, showArr bool
	for _, a := range s.eng.ArrInstances() {
		arr = append(arr, arrOpt{a.Name, a.Kind})
		movieArr = movieArr || a.Kind == "radarr" || a.Kind == "whisparr"
		showArr = showArr || a.Kind == "sonarr" || a.Kind == "whisparr"
	}
	gemini, switched := s.eng.GeminiModelInUse(ctx)
	every := s.eng.RecRefreshEvery()
	data := M{"Title": "Recommendations", "Nav": "recs", "Runs": runs, "Users": users, "User": user,
		"Providers": s.eng.AIProviders(), "Arr": arr, "HasSeerr": s.cfg.SeerrURL != "" && s.cfg.SeerrKey != "", "Cfg": s.cfg,
		"Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r),
		"SourceGroups": groups, "SourcesChecked": checked, "SourcesTotal": len(sources), "Ignored": ignored,
		"HasMovieArr": movieArr, "HasShowArr": showArr, "GeminiModel": gemini, "GeminiSwitched": switched, "LastRun": last, "RefreshHours": int(every.Hours())}
	if last > 0 && every > 0 {
		data["NextRun"] = time.Unix(last, 0).Add(every).Unix()
	}
	s.render(w, "recs", data)
}

func (s *Server) recsRun(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, "/recs", "Couldn't read the form.")
		return
	}
	n, _ := strconv.Atoi(r.FormValue("count"))
	user := r.FormValue("user")
	req := core.RecRequest{User: user, Provider: r.FormValue("provider"), Kind: r.FormValue("kind"), Count: n,
		IncludeAdult: r.FormValue("adult") == "1", Note: r.FormValue("note"), OnlyMine: r.FormValue("mine") == "1"}
	if r.PostForm.Get("src_shown") == "1" {
		if ids := r.PostForm["src"]; len(ids) > 0 {
			if err := s.eng.SaveRecSources(r.Context(), user, ids); err != nil {
				s.fail(w, err)
				return
			}
			req.Sources = ids
		} else {
			s.flash(w, r, recsURL(user), "Choose at least one source for recommendations.")
			return
		}
	}
	s.eng.Jobs.Start(s.eng.Context(), "recommendations", func(ctx context.Context, report func(string)) error {
		return s.eng.Recommend(ctx, req, report)
	})
	back(w, r, recsURL(user))
}

func (s *Server) recToArr(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	msg, err := s.eng.AddToArr(r.Context(), id, r.FormValue("instance"))
	if err != nil {
		msg = "Could not add: " + err.Error()
	}
	s.flash(w, r, recsURL(r.FormValue("user")), msg)
}

func (s *Server) recIgnore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rec, err := s.st.GetRec(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if rec == nil {
		http.NotFound(w, r)
		return
	}
	user, err := s.st.RecRunUser(ctx, rec.RunID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.IgnoreRec(ctx, user, rec.Kind, rec.Title, rec.Year); err != nil {
		s.fail(w, err)
		return
	}
	msg := "Ignored " + rec.Title + ". It won't be recommended again."
	if left, err := s.st.VisibleRecCount(ctx, user); err == nil && left < recTopUpBelow && s.eng.RefreshRecs(user) {
		msg += " Getting more recommendations."
	}
	s.flash(w, r, recsURL(user), msg)
}

func (s *Server) recUnignore(w http.ResponseWriter, r *http.Request) {
	user, kind, norm := r.FormValue("user"), r.FormValue("kind"), r.FormValue("norm")
	if err := s.st.UnignoreRec(r.Context(), user, kind, norm); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, recsURL(user)+"#ignored", "Restored. It can be recommended again.")
}

// ---------- where-to-watch badges ----------

type badgeSet struct {
	Show []string
	More []string
}

// splitBadges puts the person's own services first and tucks the rest away.
func (s *Server) splitBadges(providers []string) badgeSet {
	var b badgeSet
	for _, p := range providers {
		if s.eng.IsMyService(p) {
			b.Show = append(b.Show, p)
		} else {
			b.More = append(b.More, p)
		}
	}
	if len(b.Show) == 0 { // no services chosen: show the first few
		n := min(len(b.More), 4)
		b.Show, b.More = b.More[:n], b.More[n:]
	}
	return b
}

// Search pages that take the title in the address. Anything not listed opens
// JustWatch, which links on to the service.
var watchSearch = []struct{ match, url string }{
	{"youtube tv", "https://tv.youtube.com/search?q="},
	{"youtube", "https://www.youtube.com/results?search_query="},
	{"netflix", "https://www.netflix.com/search?q="},
	{"amazon", "https://www.amazon.com/s?i=instant-video&k="},
	{"prime video", "https://www.amazon.com/s?i=instant-video&k="},
	{"hulu", "https://www.hulu.com/search?q="},
	{"tubi", "https://tubitv.com/search/"},
	{"apple tv", "https://tv.apple.com/search?term="},
	{"plex", "https://watch.plex.tv/search?q="},
	{"hoopla", "https://www.hoopladigital.com/search?q="},
	{"kanopy", "https://www.kanopy.com/search?query="},
	{"crunchyroll", "https://www.crunchyroll.com/search?q="},
	{"roku", "https://therokuchannel.roku.com/search/"},
	{"fandango", "https://athome.fandango.com/content/browse/search?searchString="},
}

// watchLink is where a where-to-watch badge goes.
func (s *Server) watchLink(provider, title string) string {
	p := strings.ToLower(provider)
	for _, w := range watchSearch {
		if strings.Contains(p, w.match) {
			if strings.HasSuffix(w.url, "/") {
				return w.url + url.PathEscape(title)
			}
			return w.url + url.QueryEscape(title)
		}
	}
	region := strings.ToLower(s.cfg.StreamingRegion)
	if len(region) != 2 {
		region = "us"
	}
	return "https://www.justwatch.com/" + region + "/search?q=" + url.QueryEscape(title)
}
