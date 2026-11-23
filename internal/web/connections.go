package web

import (
	"encoding/json"
	"net/http"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
)

func (s *Server) routesConnections(admin *http.ServeMux) {
	admin.HandleFunc("GET /connections", s.connectionsPage)
	admin.HandleFunc("POST /connections/test", s.connectionTest)
}

func (s *Server) connectionsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	states := s.eng.ConnStates(r.Context())
	type group struct {
		Name string
		Rows []core.ConnResult
	}
	var groups []group
	for _, st := range states {
		if len(groups) == 0 || groups[len(groups)-1].Name != st.Group {
			groups = append(groups, group{Name: st.Group})
		}
		g := &groups[len(groups)-1]
		g.Rows = append(g.Rows, st)
	}
	s.render(w, "connections", M{"Title": "Connections", "Nav": "connections", "Groups": groups, "Flash": takeFlash(w, r)})
}

// connectionTest runs one read-only test. Unsaved values may come from the
// Settings page. The answer never contains a key or a full address.
func (s *Server) connectionTest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	posted := map[string]string{}
	for k, v := range r.PostForm {
		if k != "id" && len(v) > 0 {
			posted[k] = v[0]
		}
	}
	res, ok := s.eng.TestConnection(r.Context(), r.PostForm.Get("id"), posted)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, res)
}

// connProbesJSON tells the Settings page which fields each test uses.
func (s *Server) connProbesJSON() string {
	b, err := json.Marshal(s.eng.ConnProbes())
	if err != nil {
		return "[]"
	}
	return string(b)
}
