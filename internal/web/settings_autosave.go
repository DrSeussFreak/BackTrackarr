package web

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
)

// These can lock you out, break links in other apps or allow changes to Plex,
// so they only change when Save and apply is clicked.
var manualSaveKeys = map[string]bool{
	"ADMIN_USER":        true,
	"BASE_URL":          true,
	"TRUST_PROXY":       true,
	"WEBHOOK_TOKEN":     true,
	"PLEX_ALLOW_WRITES": true,
}

const autosaveApplyDelay = 6 * time.Second

type autosaveState struct {
	mu    sync.Mutex
	timer *time.Timer
}

// applySoon reloads the app once edits have paused, so a run of changes causes
// one short reload instead of one per field.
func (s *Server) applySoon() {
	if s.Restart == nil {
		return
	}
	s.autosave.mu.Lock()
	defer s.autosave.mu.Unlock()
	if s.autosave.timer != nil {
		s.autosave.timer.Stop()
	}
	s.autosave.timer = time.AfterFunc(autosaveApplyDelay, func() { s.requestRestart() })
}

type autosavedField struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	IsSet  bool   `json:"is_set"`
}

// settingsAutosave stores the fields sent and reports each one. A field that
// isn't valid is left unchanged and doesn't stop the others.
func (s *Server) settingsAutosave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	set := map[string]string{}
	var remove []string
	errs := map[string]string{}
	touched := []config.Setting{}
	for _, c := range config.Settings() {
		vals, present := r.PostForm[c.Key]
		reset := r.PostForm.Get("reset_"+c.Key) == "1"
		clear := r.PostForm.Get("clear_"+c.Key) == "1"
		if !present && !reset && !clear {
			continue
		}
		if manualSaveKeys[c.Key] {
			errs[c.Key] = "Click Save and apply to change this one."
			continue
		}
		touched = append(touched, c)
		switch {
		case reset:
			remove = append(remove, c.Key)
		case clear:
			set[c.Key] = ""
		default:
			posted := strings.TrimSpace(vals[0])
			if c.Type == "secret" && posted == "" {
				continue // blank keeps the stored secret
			}
			v, err := config.Validate(c, posted)
			if err != nil {
				errs[c.Key] = err.Error()
				continue
			}
			set[c.Key] = v
		}
	}
	if len(set)+len(remove) > 0 {
		if err := s.saveSettings(r.Context(), set, remove); err != nil {
			s.log.Error("autosave", "err", err)
			http.Error(w, "couldn't save", http.StatusInternalServerError)
			return
		}
		s.log.Info("settings saved", "changed", len(set)+len(remove)) // names and values stay out of the log
		s.applySoon()
	}
	saved := map[string]autosavedField{}
	for _, c := range touched {
		if _, bad := errs[c.Key]; bad {
			continue
		}
		v, src := config.Current(c)
		f := autosavedField{Value: v, Source: src, IsSet: v != ""}
		if c.Type == "secret" {
			f.Value = "" // never sent back to the browser
		}
		saved[c.Key] = f
	}
	writeJSON(w, M{"saved": saved, "errors": errs})
}
