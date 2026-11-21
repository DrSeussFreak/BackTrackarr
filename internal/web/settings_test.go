package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/auth"
	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestSettingsPage(t *testing.T) {
	t.Setenv("PLEX_TOKEN", "env-secret-value")
	t.Setenv("BACKUP_KEEP", "3")
	config.SetOverrides(nil)
	t.Cleanup(func() { config.SetOverrides(nil) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()

	call := func(method string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	saved := func() map[string]string {
		m, err := st.Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		config.SetOverrides(m)
		return m
	}

	page := call(http.MethodGet, nil)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "env-secret-value") {
		t.Fatalf("page must load and never show a secret (status %d)", page.Code)
	}

	// Unchanged values and a blank secret save nothing.
	call(http.MethodPost, url.Values{"PLEX_TOKEN": {""}, "BACKUP_KEEP": {"3"}})
	if m := saved(); len(m) != 0 {
		t.Fatalf("nothing should be saved: %v", m)
	}

	// A bad value rejects the whole form.
	call(http.MethodPost, url.Values{"SESSION_DAYS": {"9"}, "PLEX_URL": {"not a url"}})
	if m := saved(); len(m) != 0 {
		t.Fatalf("invalid form must save nothing: %v", m)
	}

	// Saved values beat the environment, and only known keys are accepted.
	call(http.MethodPost, url.Values{"BACKUP_KEEP": {"5"}, "PLEX_TOKEN": {"new-secret"}, "DATA_DIR": {"/tmp/evil"}, "ADMIN_PASSWORD": {"x"}})
	m := saved()
	if m["BACKUP_KEEP"] != "5" || m["PLEX_TOKEN"] != "new-secret" || len(m) != 2 {
		t.Fatalf("saved: %v", m)
	}
	if c := config.Load(); c.BackupKeep != 5 || c.PlexToken != "new-secret" {
		t.Fatalf("override not applied: keep=%d", c.BackupKeep)
	}
	if strings.Contains(call(http.MethodGet, nil).Body.String(), "new-secret") {
		t.Fatal("saved secret was shown")
	}

	// Undo returns to the environment value; clear blanks a secret.
	call(http.MethodPost, url.Values{"BACKUP_KEEP": {"5"}, "reset_BACKUP_KEEP": {"1"}, "PLEX_TOKEN": {""}, "clear_PLEX_TOKEN": {"1"}})
	saved()
	if c := config.Load(); c.BackupKeep != 3 || c.PlexToken != "" {
		t.Fatalf("after undo and clear: keep=%d token set=%v", c.BackupKeep, c.PlexToken != "")
	}
}

func TestSettingsAdoptPasswordAndBackup(t *testing.T) {
	t.Setenv("TMDB_API_KEY", "env-tmdb-key")
	t.Setenv("BACKUP_KEEP", "4")
	config.SetOverrides(nil)
	t.Cleanup(func() { config.SetOverrides(nil) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{AdminUser: "admin", WebhookToken: "hook-token"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	ctx := context.Background()
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Env values move into the database.
	post("/settings/adopt", nil)
	m, _ := st.Settings(ctx)
	if m["TMDB_API_KEY"] != "env-tmdb-key" || m["BACKUP_KEEP"] != "4" {
		t.Fatalf("adopt: %v", m)
	}
	config.SetOverrides(m)

	// A password is stored only as a hash.
	post("/settings/password", url.Values{"new": {"short"}, "confirm": {"short"}})
	if m, _ := st.Settings(ctx); m[config.AdminPasswordKey] != "" {
		t.Fatal("a short password was accepted")
	}
	post("/settings/password", url.Values{"new": {"correct horse battery"}, "confirm": {"correct horse battery"}})
	m, _ = st.Settings(ctx)
	hash := m[config.AdminPasswordKey]
	if !auth.IsPasswordHash(hash) || !auth.CheckPassword(hash, "correct horse battery") || auth.CheckPassword(hash, "wrong password!") {
		t.Fatalf("bad password hash: %q", hash)
	}
	cfg.AdminPassword = hash
	post("/settings/password", url.Values{"current": {"wrong password!"}, "new": {"another long password"}, "confirm": {"another long password"}})
	if m, _ := st.Settings(ctx); m[config.AdminPasswordKey] != hash {
		t.Fatal("changed the password without the current one")
	}

	// Sign in, since a password now protects the page.
	cfg.AdminPassword = ""
	if w := post("/settings/export", url.Values{"passphrase": {"short"}}); w.Header().Get("Content-Disposition") != "" {
		t.Fatal("exported with a weak passphrase")
	}
	cfg.AdminPassword = hash
	req := func(path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth("admin", "correct horse battery")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	exp := req("/settings/export", url.Values{"passphrase": {"backup passphrase"}})
	if exp.Code != http.StatusOK || strings.Contains(exp.Body.String(), "env-tmdb-key") {
		t.Fatalf("export must be encrypted (HTTP %d)", exp.Code)
	}

	// Restore onto an empty database.
	if err := st.SaveSettings(ctx, nil, []string{"TMDB_API_KEY", "BACKUP_KEEP", config.AdminPasswordKey}); err != nil {
		t.Fatal(err)
	}
	restore := func(pass string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, _ := mw.CreateFormFile("file", "s.btsettings")
		fw.Write(exp.Body.Bytes())
		mw.WriteField("passphrase", pass)
		mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/settings/import", &b)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.SetBasicAuth("admin", "correct horse battery")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	restore("the wrong passphrase")
	if m, _ := st.Settings(ctx); m["TMDB_API_KEY"] != "" {
		t.Fatal("restored with a wrong passphrase")
	}
	restore("backup passphrase")
	m, _ = st.Settings(ctx)
	if m["TMDB_API_KEY"] != "env-tmdb-key" || m["BACKUP_KEEP"] != "4" || m["WEBHOOK_TOKEN"] != "hook-token" || m[config.AdminPasswordKey] != hash {
		t.Fatalf("restore lost values: %d keys", len(m))
	}
}

func TestSettingsSaveIsImmediateAndIgnoresStaleTabs(t *testing.T) {
	config.SetOverrides(nil)
	t.Cleanup(func() { config.SetOverrides(nil) })
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()
	post := func(f url.Values) {
		r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	post(url.Values{"BACKUP_KEEP": {"5"}, "was_BACKUP_KEEP": {""}})
	keep, _ := config.Lookup("BACKUP_KEEP")
	if v, src := config.Current(keep); v != "5" || src != "saved" {
		t.Fatalf("a save must show right away: %q %q", v, src)
	}
	// An older tab still holds the old blank value and changes something else.
	post(url.Values{"BACKUP_KEEP": {""}, "was_BACKUP_KEEP": {""}, "SESSION_DAYS": {"9"}, "was_SESSION_DAYS": {""}})
	m, _ := st.Settings(context.Background())
	if m["BACKUP_KEEP"] != "5" || m["SESSION_DAYS"] != "9" {
		t.Fatalf("stale tab changed a newer value: %v", m)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, want := range []string{"Needed for:", "Essential", "novalidate", `name="was_BACKUP_KEEP" value="5"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("settings page is missing %q", want)
		}
	}
}
