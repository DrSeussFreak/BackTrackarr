package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/auth"
	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/throttle"
)

func (s *Server) routesSettings(admin *http.ServeMux) {
	admin.HandleFunc("GET /settings", s.settingsPage)
	admin.HandleFunc("POST /settings", s.settingsSave)
	admin.HandleFunc("POST /settings/arr-options", s.settingsArrOptions)
	admin.HandleFunc("POST /settings/autosave", s.settingsAutosave)
	admin.HandleFunc("POST /settings/adopt", s.settingsAdopt)
	admin.HandleFunc("POST /settings/password", s.settingsPassword)
	admin.HandleFunc("POST /settings/export", s.settingsExport)
	admin.HandleFunc("POST /settings/import", s.settingsImport)
	admin.HandleFunc("POST /settings/throttle/clear", s.throttleClear)
}

type settingRow struct {
	config.Setting
	Value  string // never holds a secret
	Source string
	IsSet  bool
	Info   config.Info
}

type settingGroup struct {
	Name string
	Help string
	Rows []settingRow
}

// visibleSettings hides numbered servers until the one before is configured,
// so the page shows one empty slot per kind rather than nine.
func visibleSettings() []settingGroup {
	all := config.Settings()
	hasURL := map[string]bool{}
	for _, c := range all {
		if c.Instance != "" && strings.HasSuffix(c.Key, "_URL") {
			if v, _ := config.Current(c); v != "" {
				hasURL[c.Instance+"|"+string(rune('0'+c.Number))] = true
			}
		}
	}
	var out []settingGroup
	for _, c := range all {
		alwaysSecond := c.Number == 2 && (c.Instance == "SONARR" || c.Instance == "RADARR")
		if c.Instance != "" && c.Number > 1 && !alwaysSecond &&
			!hasURL[c.Instance+"|"+string(rune('0'+c.Number))] && !hasURL[c.Instance+"|"+string(rune('0'+c.Number-1))] {
			continue
		}
		v, src := config.Current(c)
		row := settingRow{Setting: c, Value: v, Source: src, IsSet: v != "", Info: config.InfoFor(c)}
		if c.Type == "secret" {
			row.Value = ""
		}
		if len(out) == 0 || out[len(out)-1].Name != c.Group {
			out = append(out, settingGroup{Name: c.Group, Help: config.GroupHelp[c.Group]})
		}
		g := &out[len(out)-1]
		g.Rows = append(g.Rows, row)
	}
	return out
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	envCount := 0
	for _, c := range config.Settings() {
		if v, src := config.Current(c); src == "env" && v != "" {
			envCount++
		}
	}
	envPassword := s.cfg.AdminPassword != "" && !auth.IsPasswordHash(s.cfg.AdminPassword)
	if envPassword {
		envCount++
	}
	var paused []throttle.Status
	if s.Throttle != nil {
		paused = s.Throttle.Paused()
	}
	s.render(w, "settings", M{"Title": "Settings", "Nav": "settings", "Groups": visibleSettings(), "EnvCount": envCount,
		"HasPassword": s.cfg.AdminPassword != "", "Paused": paused, "ConnProbes": s.connProbesJSON(), "Ignored": config.IgnoreSaved(), "Flash": takeFlash(w, r)})
}

// settingsReturn sends a form back to the page it came from. Only these two
// fixed pages are possible.
func settingsReturn(r *http.Request) string {
	if r.FormValue("next") == "/setup" {
		return "/setup"
	}
	return "/settings"
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		s.flash(w, r, settingsReturn(r), "Could not read the form.")
		return
	}
	set := map[string]string{}
	var remove []string
	for _, c := range config.Settings() {
		vals, present := r.PostForm[c.Key]
		if !present {
			continue // a hidden numbered server: leave it alone
		}
		if r.PostForm.Get("reset_"+c.Key) == "1" || r.PostForm.Get("clear_"+c.Key) == "1" {
			if r.PostForm.Get("clear_"+c.Key) == "1" {
				set[c.Key] = "" // blank, even if the environment has a value
			} else {
				remove = append(remove, c.Key)
			}
			continue
		}
		cur, _ := config.Current(c)
		posted := strings.TrimSpace(vals[0])
		// Untouched on this page: never overwrite a newer value from another tab.
		if was, ok := r.PostForm["was_"+c.Key]; ok && posted == strings.TrimSpace(was[0]) {
			continue
		}
		if c.Key == "ADMIN_USER" && posted != cur && s.mfaEnabled(r.Context()) {
			s.flash(w, r, settingsReturn(r), "Nothing was saved. Turn off two-step sign-in and passkeys before changing the username.")
			return
		}
		if c.Type == "secret" {
			if posted == "" {
				continue // blank keeps the stored secret
			}
		} else if posted == cur {
			continue // unchanged: an env value stays an env value
		}
		v, err := config.Validate(c, posted)
		if err != nil {
			s.flash(w, r, settingsReturn(r), "Nothing was saved. "+err.Error())
			return
		}
		set[c.Key] = v
	}
	if len(set) == 0 && len(remove) == 0 {
		s.flash(w, r, settingsReturn(r), "No changes to save.")
		return
	}
	if err := s.saveSettings(r.Context(), set, remove); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("settings saved", "changed", len(set)+len(remove)) // names and values stay out of the log
	s.applySettings(w, r, "Saved and applied.")
}

// saveSettings stores changes and shows them right away. Parts of the app
// that read settings at startup pick them up on the reload that follows.
func (s *Server) saveSettings(ctx context.Context, set map[string]string, remove []string) error {
	if err := s.st.SaveSettings(ctx, set, remove); err != nil {
		return err
	}
	if !config.IgnoreSaved() {
		if all, err := s.st.Settings(ctx); err == nil {
			config.SetOverrides(all)
		}
	}
	return nil
}

// requestRestart reloads the app once, after any running job finishes, so a
// save can never cut off an import. It reports whether it's waiting on one.
func (s *Server) requestRestart() bool {
	if s.Restart == nil {
		return false
	}
	waiting := s.eng.Jobs.BlocksRestart()
	s.restartOnce.Do(func() {
		go func() {
			time.Sleep(400 * time.Millisecond) // let this response finish
			for s.eng.Jobs.BlocksRestart() {   // imports continue after the restart
				select {
				case <-s.eng.Context().Done():
					return
				case <-time.After(time.Second):
				}
			}
			s.Restart()
		}()
	})
	return waiting
}

// applySettings answers the browser, then reloads the app with the new values.
func (s *Server) applySettings(w http.ResponseWriter, r *http.Request, msg string) {
	to := settingsReturn(r)
	if s.Restart == nil {
		s.flash(w, r, to, msg)
		return
	}
	if s.requestRestart() {
		s.flash(w, r, to, msg+" Some changes finish applying when the running job is done.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: msg, Path: "/", MaxAge: 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Waits for the reloaded app, then returns. Both values are app-generated.
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="12;url=`+to+`"><title>Applying settings</title><p>Applying settings…</p><script>
(async()=>{for(;;){try{const r=await fetch("/healthz",{cache:"no-store"});if(r.ok&&r.headers.get("X-Backtrackarr-Boot")!=="`+s.boot+`"){location.replace("`+to+`");return}}catch(e){}await new Promise(d=>setTimeout(d,200))}})()
</script>`)
}

// settingsAdopt copies every value still coming from the environment into the
// database, so the stack file no longer needs them.
func (s *Server) settingsAdopt(w http.ResponseWriter, r *http.Request) {
	set := map[string]string{}
	for _, c := range config.Settings() {
		if v, src := config.Current(c); src == "env" && v != "" {
			set[c.Key] = v
		}
	}
	if p := s.cfg.AdminPassword; p != "" && !auth.IsPasswordHash(p) {
		h, err := auth.HashPassword(p)
		if err != nil {
			s.fail(w, err)
			return
		}
		set[config.AdminPasswordKey] = h
	}
	if len(set) == 0 {
		s.flash(w, r, "/settings", "Everything is already stored in the app.")
		return
	}
	if err := s.saveSettings(r.Context(), set, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.applySettings(w, r, fmt.Sprintf("Moved %d values into the app. You can now remove them from your stack.", len(set)))
}

const minSecretLen = 12

func (s *Server) settingsPassword(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if blocked, _ := s.signin.limiter.Blocked(ip); blocked {
		s.flash(w, r, "/settings", "Too many failed attempts. Try again later.")
		return
	}
	if s.cfg.AdminPassword != "" && !auth.CheckPassword(s.cfg.AdminPassword, r.FormValue("current")) {
		s.signin.limiter.Fail(ip)
		s.flash(w, r, "/settings", "Password not changed: the current password is wrong.")
		return
	}
	pw := r.FormValue("new")
	if len(pw) < minSecretLen {
		s.flash(w, r, "/settings", fmt.Sprintf("Password not changed: use at least %d characters.", minSecretLen))
		return
	}
	if pw != r.FormValue("confirm") {
		s.flash(w, r, "/settings", "Password not changed: the two new passwords don't match.")
		return
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.saveSettings(r.Context(), map[string]string{config.AdminPasswordKey: h}, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("sign-in password changed")
	s.applySettings(w, r, "Password changed.")
}

// settingsBackup is the content of an encrypted settings file.
type settingsBackup struct {
	Version      int               `json:"version"`
	Created      time.Time         `json:"created"`
	Settings     map[string]string `json:"settings"`
	PasswordHash string            `json:"password_hash,omitempty"`
}

// settingsExport downloads every setting in effect, encrypted with a
// passphrase because it contains API keys.
func (s *Server) settingsExport(w http.ResponseWriter, r *http.Request) {
	pass := r.FormValue("passphrase")
	if len(pass) < minSecretLen {
		s.flash(w, r, "/settings", fmt.Sprintf("Backup not made: use a passphrase of at least %d characters.", minSecretLen))
		return
	}
	b := settingsBackup{Version: 1, Created: time.Now().UTC(), Settings: map[string]string{}}
	for _, c := range config.Settings() {
		if v, src := config.Current(c); src != "default" && v != "" {
			b.Settings[c.Key] = v
		}
	}
	if b.Settings["WEBHOOK_TOKEN"] == "" && s.cfg.WebhookToken != "" {
		b.Settings["WEBHOOK_TOKEN"] = s.cfg.WebhookToken // keeps pasted webhook URLs working
	}
	switch p := s.cfg.AdminPassword; {
	case auth.IsPasswordHash(p):
		b.PasswordHash = p
	case p != "":
		h, err := auth.HashPassword(p)
		if err != nil {
			s.fail(w, err)
			return
		}
		b.PasswordHash = h
	}
	plain, err := json.Marshal(b)
	if err != nil {
		s.fail(w, err)
		return
	}
	sealed, err := auth.Seal(plain, pass)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="backtrackarr-settings-`+time.Now().Format("20060102")+`.btsettings"`)
	w.Write(sealed)
}

func (s *Server) settingsImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		s.flash(w, r, "/settings", "Choose a settings backup file.")
		return
	}
	defer f.Close()
	file, err := io.ReadAll(f)
	if err != nil {
		s.flash(w, r, "/settings", "Could not read the file.")
		return
	}
	plain, err := auth.Unseal(file, r.FormValue("passphrase"))
	if err != nil {
		s.signin.limiter.Fail(s.clientIP(r))
		s.flash(w, r, "/settings", "Nothing was restored: "+err.Error()+".")
		return
	}
	var b settingsBackup
	if err := json.Unmarshal(plain, &b); err != nil || b.Version != 1 {
		s.flash(w, r, "/settings", "Nothing was restored: this backup's format isn't supported.")
		return
	}
	set := map[string]string{}
	for k, v := range b.Settings {
		c, ok := config.Lookup(k)
		if !ok {
			continue // only known settings, never DATA_DIR or the like
		}
		if k == "ADMIN_USER" && s.mfaEnabled(r.Context()) {
			continue // two-step sign-in and passkeys belong to the current username
		}
		val, err := config.Validate(c, v)
		if err != nil {
			s.flash(w, r, "/settings", "Nothing was restored. "+err.Error())
			return
		}
		set[k] = val
	}
	if auth.IsPasswordHash(b.PasswordHash) {
		set[config.AdminPasswordKey] = b.PasswordHash
	}
	if err := s.saveSettings(r.Context(), set, nil); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("settings restored from backup", "values", len(set))
	s.applySettings(w, r, fmt.Sprintf("Restored %d settings.", len(set)))
}

func (s *Server) throttleClear(w http.ResponseWriter, r *http.Request) {
	if s.Throttle != nil {
		s.Throttle.Clear(r.FormValue("host"))
	}
	back(w, r, "/settings")
}
