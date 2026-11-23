package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
)

func (s *Server) routesStreaming(root, admin *http.ServeMux) {
	// token-protected, no admin login (called by Jellyfin/Emby, the userscript, scripts)
	root.HandleFunc("POST /webhook/jellyfin/{token}", s.mediaServerWebhook)
	root.HandleFunc("POST /webhook/emby/{token}", s.mediaServerWebhook)
	root.HandleFunc("POST /api/scrobble/{token}", s.apiScrobble)
	root.HandleFunc("OPTIONS /api/scrobble/{token}", s.apiScrobble)

	admin.HandleFunc("GET /streaming", s.streamingPage)
	admin.HandleFunc("POST /streaming/import", s.streamingImport)
	admin.HandleFunc("POST /streaming/pull", s.streamingPull)
	admin.HandleFunc("GET /streaming/backtrackarr.user.js", s.userscript)
}

func (s *Server) tokenOK(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.PathValue("token")), []byte(s.cfg.WebhookToken)) == 1
}

func (s *Server) apiScrobble(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.tokenOK(r) {
		http.NotFound(w, r)
		return
	}
	var req core.ScrobbleRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := s.eng.HandleScrobble(ctx, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, M{"ok": true, "media": m})
}

func (s *Server) mediaServerWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if mr, err := multipartPayload(r, raw); err == nil && len(mr) > 0 {
		raw = mr // Emby can send multipart with a "data" field
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.eng.HandleMediaServerWebhook(ctx, raw); err != nil {
		s.log.Warn("media server webhook", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// multipartPayload extracts the JSON from a multipart body ("data" or
// "payload" field) when the request is multipart.
func multipartPayload(r *http.Request, raw []byte) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		return nil, nil
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(strings.NewReader(string(raw)))
	if err := r2.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	for _, k := range []string{"data", "payload"} {
		if v := r2.FormValue(k); v != "" {
			return []byte(v), nil
		}
	}
	return nil, nil
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) streamingPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sources, err := s.st.SourceCounts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	users, _ := s.st.Users(ctx)
	traktPull, _ := s.st.GetKV(ctx, "pull|trakt")
	simklPull, _ := s.st.GetKV(ctx, "pull|simkl")
	base := s.baseURL(r)
	s.render(w, "streaming", M{"Title": "Streaming", "Nav": "streaming", "Services": core.Services, "Sources": sources,
		"Users": users, "Cfg": s.cfg, "CanResolve": s.eng.CanResolve(), "Base": base,
		"ScrobbleURL": base + "/api/scrobble/" + s.cfg.WebhookToken,
		"JellyfinURL": base + "/webhook/jellyfin/" + s.cfg.WebhookToken,
		"EmbyURL":     base + "/webhook/emby/" + s.cfg.WebhookToken,
		"TraktPull":   atoi64(traktPull), "SimklPull": atoi64(simklPull),
		"Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func (s *Server) streamingImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	// Spool to disk: the import runs after this request, and a large export
	// shouldn't sit in memory.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.flash(w, r, "/streaming", "Choose an export file to upload.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	files, done, err := s.formFiles(r)
	if err != nil {
		s.flash(w, r, "/streaming", "Upload failed: "+err.Error())
		return
	}
	opt := core.StreamImport{Service: r.FormValue("service"), User: strings.TrimSpace(r.FormValue("user")),
		Profile: strings.TrimSpace(r.FormValue("profile"))}
	if opt.Service == "auto" {
		opt.Service = ""
	}
	defer done()
	if err := s.eng.StartFileImport("import ← "+uploadLabel(files), files, core.FileImport{Mode: "streaming", Stream: opt}); err != nil {
		s.flash(w, r, "/streaming", "Upload failed: "+err.Error())
		return
	}
	back(w, r, "/streaming")
}

func (s *Server) streamingPull(w http.ResponseWriter, r *http.Request) {
	s.eng.Jobs.Start(s.eng.Context(), "pull ← trakt/simkl", s.eng.PullTrackers)
	back(w, r, "/streaming")
}

func (s *Server) userscript(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("static/userscript.js")
	if err != nil {
		s.fail(w, err)
		return
	}
	js := strings.NewReplacer("{{SERVER}}", jsString(s.baseURL(r)), "{{TOKEN}}", jsString(s.cfg.WebhookToken),
		"{{USER}}", jsString(r.URL.Query().Get("user"))).Replace(string(b))
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	io.WriteString(w, js)
}

// jsString escapes a value for use inside a double-quoted JS string.
func jsString(v string) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}
