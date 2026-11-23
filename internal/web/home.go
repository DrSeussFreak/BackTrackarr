package web

import (
	"context"
	"encoding/json"
	"net/http"
)

const homeLayoutKey = "home|layout"

// HomeSection is one block on the home page.
type HomeSection struct {
	ID     string `json:"id"`
	Title  string `json:"-"`
	Hidden bool   `json:"hidden"`
	Wide   bool   `json:"wide"`
}

func homeDefaults() []HomeSection {
	return []HomeSection{
		{ID: "now", Title: "Now playing", Wide: true},
		{ID: "stats", Title: "Totals", Wide: true},
		{ID: "years", Title: "By year"},
		{ID: "shows", Title: "Top shows"},
		{ID: "movies", Title: "Most-watched movies"},
		{ID: "rewatched", Title: "Most rewatched"},
		{ID: "artists", Title: "Top artists"},
		{ID: "recent", Title: "Recently watched", Wide: true},
		{ID: "shelf", Title: "Books and games totals", Wide: true},
		{ID: "shelfrecent", Title: "Recently read and played", Wide: true},
	}
}

// homeLayout returns the saved order. Sections added in later versions appear
// at the end, and ones that no longer exist are ignored.
func (s *Server) homeLayout(ctx context.Context) []HomeSection {
	defaults := homeDefaults()
	v, _ := s.st.GetKV(ctx, homeLayoutKey)
	if v == "" {
		return defaults
	}
	var saved []HomeSection
	if json.Unmarshal([]byte(v), &saved) != nil {
		return defaults
	}
	return mergeHomeLayout(saved, defaults)
}

func mergeHomeLayout(saved, defaults []HomeSection) []HomeSection {
	known := make(map[string]HomeSection, len(defaults))
	for _, d := range defaults {
		known[d.ID] = d
	}
	out := make([]HomeSection, 0, len(defaults))
	seen := map[string]bool{}
	for _, h := range saved {
		d, ok := known[h.ID]
		if !ok || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		d.Hidden, d.Wide = h.Hidden, h.Wide
		out = append(out, d)
	}
	for _, d := range defaults {
		if !seen[d.ID] {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) homeSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var saved []HomeSection
	if err := json.Unmarshal([]byte(r.FormValue("layout")), &saved); err != nil {
		http.Error(w, "bad layout", http.StatusBadRequest)
		return
	}
	b, err := json.Marshal(mergeHomeLayout(saved, homeDefaults()))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.st.SetKV(r.Context(), homeLayoutKey, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) homeReset(w http.ResponseWriter, r *http.Request) {
	if err := s.st.SetKV(r.Context(), homeLayoutKey, ""); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, "/")
}

func (s *Server) importEverything(w http.ResponseWriter, r *http.Request) {
	if _, err := s.eng.StartImportEverything(); err != nil {
		s.flash(w, r, "/import", "Couldn't start the import: "+err.Error())
		return
	}
	back(w, r, "/import")
}
