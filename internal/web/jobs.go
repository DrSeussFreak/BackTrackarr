package web

import (
	"net/http"
	"net/url"
)

func (s *Server) routesJobs(admin *http.ServeMux) {
	admin.HandleFunc("GET /jobs", s.jobsPage)
	admin.HandleFunc("POST /jobs/clear", s.jobsClear)
	admin.HandleFunc("POST /jobs/{id}/{action}", s.jobAction)
}

func (s *Server) jobsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "jobs", M{"Title": "Jobs", "Nav": "jobs", "Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

// jobsBack returns to the page the button was on.
func jobsBack(r *http.Request) string {
	if u, err := url.Parse(r.Referer()); err == nil && u.Host == r.Host {
		if p := safeNext(u.Path); p != "/" {
			return p
		}
	}
	return "/jobs"
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "pause":
		err = s.eng.Jobs.Pause(id)
	case "resume", "retry":
		err = s.eng.Jobs.ResumeJob(id)
	case "stop":
		err = s.eng.Jobs.Stop(id)
	case "remove":
		err = s.eng.Jobs.Remove(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.flash(w, r, jobsBack(r), "Couldn't do that: "+err.Error())
		return
	}
	back(w, r, jobsBack(r))
}

func (s *Server) jobsClear(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.Jobs.ClearFinished(); err != nil {
		s.flash(w, r, "/jobs", "Couldn't clear the list: "+err.Error())
		return
	}
	back(w, r, "/jobs")
}
