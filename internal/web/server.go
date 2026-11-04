// Package web serves the UI, the JSON API and the Plex webhook endpoint.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
	"github.com/DrSeussFreak/BackTrackarr/internal/sinks"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type M = map[string]any

type authState struct {
	Code    *sinks.DeviceCode
	Err     string
	Done    bool
	Expires time.Time
}

type Server struct {
	cfg  *config.Config
	st   *store.Store
	eng  *core.Engine
	log  *slog.Logger
	tmpl map[string]*template.Template

	mu   sync.Mutex
	auth map[string]*authState
}

var funcs = template.FuncMap{
	"ts": func(unix int64) string {
		if unix == 0 {
			return "—"
		}
		return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
	},
	"hours": func(sec int64) string {
		h := sec / 3600
		if h >= 48 {
			return fmt.Sprintf("%s h (%s days)", num(h), num(h/24))
		}
		return fmt.Sprintf("%d h", h)
	},
	"num": num,
	"pct": func(a, b int64) int64 {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	},
	"display": func(m model.Media) string { return m.Display() },
	"list":    func(v ...string) []string { return v },
	"i64":     func(n int) int64 { return int64(n) },
	"inc":     func(n int) int { return n + 1 },
	"mine":    func(string) bool { return false }, // replaced per server in New
}

func num(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func New(cfg *config.Config, st *store.Store, eng *core.Engine, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, eng: eng, log: log, tmpl: map[string]*template.Template{}, auth: map[string]*authState{}}
	fm := template.FuncMap{}
	for k, v := range funcs {
		fm[k] = v
	}
	fm["mine"] = eng.IsMyService
	for _, page := range []string{"dashboard", "history", "sync", "library", "list", "restore", "comics", "recs", "streaming"} {
		s.tmpl[page] = template.Must(template.New("").Funcs(fm).ParseFS(assets, "templates/layout.html", "templates/"+page+".html"))
	}
	return s
}

func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()
	root.HandleFunc("POST /webhook/plex/{token}", s.plexWebhook)
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.DB.PingContext(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	})

	admin := http.NewServeMux()
	admin.Handle("GET /static/", http.FileServerFS(assets))
	admin.HandleFunc("GET /{$}", s.dashboard)
	admin.HandleFunc("GET /history", s.history)
	admin.HandleFunc("POST /plays/{id}/delete", s.deletePlay)
	admin.HandleFunc("GET /sync", s.syncPage)
	admin.HandleFunc("POST /sync/{name}/connect", s.connect)
	admin.HandleFunc("POST /sync/{name}/disconnect", s.disconnect)
	admin.HandleFunc("POST /sync/{name}/push", s.push)
	admin.HandleFunc("POST /sync/{name}/retry", s.retry)
	admin.HandleFunc("POST /import/plex", s.importPlex)
	admin.HandleFunc("POST /import/trakt", s.importTrakt)
	admin.HandleFunc("POST /backup", s.backup)
	admin.HandleFunc("GET /api/stats", s.apiStats)
	admin.HandleFunc("GET /api/history", s.apiHistory)
	admin.HandleFunc("GET /api/export", s.apiExport)
	s.routesExtra(admin)
	s.routesStreaming(root, admin)
	root.Handle("/", s.guard(admin))
	return root
}

// guard applies optional basic auth and blocks cross-site form posts.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminPassword != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.AdminUser)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AdminPassword)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="backtrackarr"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, page string, data M) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl[page].ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("request", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func back(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// ---------- webhook ----------

func (s *Server) plexWebhook(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.PathValue("token")), []byte(s.cfg.WebhookToken)) != 1 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	var raw []byte
	if mr, err := r.MultipartReader(); err == nil {
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "bad multipart body", http.StatusBadRequest)
				return
			}
			if part.FormName() == "payload" {
				raw, _ = io.ReadAll(io.LimitReader(part, 1<<20))
			}
			part.Close() // skips the thumbnail without buffering it
		}
	} else {
		raw, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20)) // plain JSON (handy for testing)
	}
	if len(raw) == 0 {
		http.Error(w, "missing payload", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.eng.HandlePlexWebhook(ctx, raw); err != nil {
		s.log.Error("plex webhook", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- pages ----------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	stats, err := s.st.Stats(r.Context(), user)
	if err != nil {
		s.fail(w, err)
		return
	}
	recent, err := s.st.History(r.Context(), store.HistoryQuery{User: user, Limit: 15})
	if err != nil {
		s.fail(w, err)
		return
	}
	users, _ := s.st.Users(r.Context())
	s.render(w, "dashboard", M{"Title": "Dashboard", "Nav": "dash", "Stats": stats, "Recent": recent, "Users": users, "User": user})
}

func parseCursor(v string) (int64, int64) {
	a, b, ok := strings.Cut(v, "_")
	if !ok {
		return 0, 0
	}
	ts, _ := strconv.ParseInt(a, 10, 64)
	id, _ := strconv.ParseInt(b, 10, 64)
	return ts, id
}

func historyQuery(r *http.Request) store.HistoryQuery {
	q := r.URL.Query()
	hq := store.HistoryQuery{User: q.Get("user"), Kind: q.Get("kind"), Search: strings.TrimSpace(q.Get("q"))}
	hq.BeforeTS, hq.BeforeID = parseCursor(q.Get("before"))
	hq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if hq.Limit <= 0 {
		hq.Limit = 50
	}
	return hq
}

func nextCursor(rows []store.PlayRow, limit int) string {
	if len(rows) < limit || len(rows) == 0 {
		return ""
	}
	last := rows[len(rows)-1]
	return fmt.Sprintf("%d_%d", last.WatchedAt, last.ID)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	hq := historyQuery(r)
	rows, err := s.st.History(r.Context(), hq)
	if err != nil {
		s.fail(w, err)
		return
	}
	users, _ := s.st.Users(r.Context())
	s.render(w, "history", M{"Title": "History", "Nav": "history", "Rows": rows, "Q": hq, "Users": users,
		"Next": nextCursor(rows, hq.Limit)})
}

func (s *Server) deletePlay(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeletePlay(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	to := r.Header.Get("Referer")
	if to == "" {
		to = "/history"
	}
	back(w, r, to)
}

type sinkRow struct {
	Name, Label, Kind string
	Ready             bool
	CanAuth           bool
	Stats             *store.SinkStats
	Auth              *authState
}

func (s *Server) syncPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.st.OutboxStats(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var rows []sinkRow
	configured := map[string]bool{}
	pending := false
	for _, sk := range s.eng.Sinks() {
		configured[sk.Kind()] = true
		_, canAuth := sk.(sinks.Authorizer)
		st := stats[sk.Name()]
		if st == nil {
			st = &store.SinkStats{}
		}
		s.mu.Lock()
		a := s.auth[sk.Name()]
		if a != nil {
			snapshot := *a // the poller goroutine mutates the original
			a = &snapshot
		}
		s.mu.Unlock()
		if a != nil && !a.Done && a.Err == "" {
			pending = true
		}
		rows = append(rows, sinkRow{Name: sk.Name(), Label: sk.Label(), Kind: sk.Kind(), Ready: sk.Ready(ctx), CanAuth: canAuth, Stats: st, Auth: a})
	}
	var missing []string
	for _, k := range []string{"trakt", "simkl", "floppy", "relay"} {
		if !configured[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	users, _ := s.st.Users(ctx)
	base := s.cfg.BaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	jobs := s.eng.Jobs.List()
	s.render(w, "sync", M{"Title": "Sync", "Nav": "sync", "Sinks": rows, "Missing": missing, "Users": users,
		"DefaultUser": s.defaultUser(users), "Jobs": jobs, "HasPlex": s.eng.HasPlex(),
		"WebhookURL": base + "/webhook/plex/" + s.cfg.WebhookToken,
		"Refresh":    pending || s.eng.Jobs.AnyRunning(), "Cfg": s.cfg})
}

func (s *Server) defaultUser(users []store.NameCount) string {
	if len(s.cfg.ScrobbleUsers) > 0 {
		return s.cfg.ScrobbleUsers[0]
	}
	if len(users) > 0 {
		return users[0].Name
	}
	return ""
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, ok := s.eng.Sink(name).(sinks.Authorizer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dc, err := a.StartAuth(r.Context())
	st := &authState{Code: dc}
	if err != nil {
		st.Err = err.Error()
	} else {
		st.Expires = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
		go s.pollAuth(name, a, st)
	}
	s.mu.Lock()
	s.auth[name] = st
	s.mu.Unlock()
	back(w, r, "/sync")
}

func (s *Server) pollAuth(name string, a sinks.Authorizer, st *authState) {
	interval := time.Duration(max(st.Code.Interval, 5)) * time.Second
	ctx, cancel := context.WithDeadline(s.eng.Context(), st.Expires)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			st.Err = "code expired, try again"
			s.mu.Unlock()
			return
		case <-time.After(interval):
		}
		done, err := a.PollAuth(ctx, st.Code)
		s.mu.Lock()
		if err != nil {
			st.Err = err.Error()
		}
		st.Done = done
		s.mu.Unlock()
		if done || err != nil {
			s.log.Info("authorization finished", "sink", name, "ok", done, "err", err)
			return
		}
	}
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, ok := s.eng.Sink(name).(sinks.Authorizer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := a.Disconnect(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.mu.Lock()
	delete(s.auth, name)
	s.mu.Unlock()
	back(w, r, "/sync")
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	name, user := r.PathValue("name"), r.FormValue("user")
	if s.eng.Sink(name) == nil || user == "" {
		http.Error(w, "unknown service or missing user", http.StatusBadRequest)
		return
	}
	s.eng.Jobs.Start(s.eng.Context(), "push → "+name, func(ctx context.Context, report func(string)) error {
		return s.eng.PushHistory(ctx, name, user, report)
	})
	back(w, r, "/sync")
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.RetryFailed(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	s.eng.Wake()
	back(w, r, "/sync")
}

func (s *Server) importPlex(w http.ResponseWriter, r *http.Request) {
	s.eng.Jobs.Start(s.eng.Context(), "import ← plex", s.eng.ImportPlex)
	back(w, r, "/sync")
}

func (s *Server) importTrakt(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.FormValue("user"))
	if user == "" {
		http.Error(w, "missing user", http.StatusBadRequest)
		return
	}
	s.eng.Jobs.Start(s.eng.Context(), "import ← trakt", func(ctx context.Context, report func(string)) error {
		return s.eng.ImportTrakt(ctx, user, report)
	})
	back(w, r, "/sync")
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	s.eng.Jobs.Start(s.eng.Context(), "backup", func(ctx context.Context, report func(string)) error {
		name, err := s.st.Backup(ctx, s.cfg.BackupKeep)
		if err == nil {
			report("wrote " + name)
		}
		return err
	})
	back(w, r, "/sync")
}

// ---------- JSON API ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats(r.Context(), r.URL.Query().Get("user"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	hq := historyQuery(r)
	rows, err := s.st.History(r.Context(), hq)
	if err != nil {
		s.fail(w, err)
		return
	}
	if rows == nil {
		rows = []store.PlayRow{}
	}
	writeJSON(w, M{"items": rows, "next": nextCursor(rows, hq.Limit)})
}

// apiExport streams every play as newline-delimited JSON.
func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="backtrackarr-export-`+time.Now().Format("20060102")+`.ndjson"`)
	enc := json.NewEncoder(w)
	if err := s.st.ExportPlays(r.Context(), func(p store.PlayRow) error { return enc.Encode(p) }); err != nil {
		s.log.Error("export", "err", err)
	}
}
