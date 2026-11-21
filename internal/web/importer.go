package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
)

func (s *Server) routesImport(root, admin *http.ServeMux) {
	admin.HandleFunc("GET /import", s.importPage)
	admin.HandleFunc("POST /import", s.importUpload)
	root.HandleFunc("POST /api/scrobble/{token}/history", s.apiBrowserHistory)
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, _ := s.st.Users(ctx)
	extras, _ := s.st.ImportExtraCounts(ctx)
	base := s.baseURL(r)
	s.render(w, "import", M{"Title": "Import", "Nav": "import", "Users": users, "DefaultUser": s.defaultUser(users), "Extras": extras,
		"PlexURL": base + "/webhook/plex/" + s.cfg.WebhookToken, "JellyfinURL": base + "/webhook/jellyfin/" + s.cfg.WebhookToken,
		"EmbyURL": base + "/webhook/emby/" + s.cfg.WebhookToken, "ScrobbleURL": base + "/api/scrobble/" + s.cfg.WebhookToken,
		"Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func (s *Server) importUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.flash(w, r, "/import", "Upload failed. Choose a file under 2 GB.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		s.flash(w, r, "/import", "Choose a file to import.")
		return
	}
	defer f.Close()
	spool, err := os.CreateTemp("", "backtrackarr-import-*")
	if err != nil {
		s.fail(w, err)
		return
	}
	_, err = io.Copy(spool, f)
	spool.Close()
	if err != nil {
		os.Remove(spool.Name())
		s.flash(w, r, "/import", "Upload failed: "+err.Error())
		return
	}
	name, user, p := hdr.Filename, strings.TrimSpace(r.FormValue("user")), spool.Name()
	started := s.eng.Jobs.Start(s.eng.Context(), "import ← "+name, func(ctx context.Context, report func(string)) error {
		defer os.Remove(p)
		res, err := s.eng.ImportAnything(ctx, p, name, user, report)
		if res != nil {
			report(res.String())
		}
		return err
	})
	if !started {
		os.Remove(p)
		s.flash(w, r, "/import", "That file is already importing.")
		return
	}
	back(w, r, "/import")
}

// apiBrowserHistory receives rows the browser script read from a signed-in
// history page. It's protected by the webhook token.
func (s *Server) apiBrowserHistory(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Service string                    `json:"service"`
		User    string                    `json:"user"`
		Items   []core.BrowserHistoryItem `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	if len(req.Items) == 0 || len(req.Items) > 100000 {
		http.Error(w, "expected 1 to 100000 items", http.StatusBadRequest)
		return
	}
	res, err := s.eng.ImportBrowserHistory(r.Context(), req.Service, req.User, req.Items)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, M{"ok": true, "message": fmt.Sprintf("%d new plays, %d already there", res.Added, res.Duplicates)})
}
