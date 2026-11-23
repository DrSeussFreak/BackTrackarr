package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestSettingsAutosave(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	defer config.SetOverrides(nil)
	cfg := &config.Config{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log)
	ctx := context.Background()

	type answer struct {
		Saved  map[string]autosavedField `json:"saved"`
		Errors map[string]string         `json:"errors"`
	}
	post := func(v url.Values) (answer, string) {
		req := httptest.NewRequest(http.MethodPost, "/settings/autosave", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.settingsAutosave(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d", w.Code)
		}
		var a answer
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a, w.Body.String()
	}
	saved := func() map[string]string {
		m, err := st.Settings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	a, _ := post(url.Values{"GDRIVE_CLIENT_ID": {"id.apps.googleusercontent.com"}, "BACKUP_KEEP": {"not-a-number"}})
	if saved()["GDRIVE_CLIENT_ID"] != "id.apps.googleusercontent.com" || a.Saved["GDRIVE_CLIENT_ID"].Source != "saved" {
		t.Fatalf("good field: %+v", a)
	}
	if a.Errors["BACKUP_KEEP"] == "" || saved()["BACKUP_KEEP"] != "" {
		t.Fatalf("a bad field must be reported and not stored: %+v", a)
	}

	a, body := post(url.Values{"GDRIVE_CLIENT_SECRET": {"typed-secret-value"}})
	if saved()["GDRIVE_CLIENT_SECRET"] != "typed-secret-value" || !a.Saved["GDRIVE_CLIENT_SECRET"].IsSet {
		t.Fatalf("secret: %+v", a)
	}
	if strings.Contains(body, "typed-secret-value") {
		t.Fatal("a secret was sent back to the browser")
	}
	post(url.Values{"GDRIVE_CLIENT_SECRET": {""}})
	if saved()["GDRIVE_CLIENT_SECRET"] != "typed-secret-value" {
		t.Fatal("a blank field must keep the stored secret")
	}

	for k, v := range map[string]string{"PLEX_ALLOW_WRITES": "true", "ADMIN_USER": "someone", "TRUST_PROXY": "true", "BASE_URL": "https://example.test", "WEBHOOK_TOKEN": "new"} {
		a, _ := post(url.Values{k: {v}})
		if a.Errors[k] == "" {
			t.Errorf("%s must need a deliberate save", k)
		}
		if _, stored := saved()[k]; stored {
			t.Errorf("%s changed through auto-save", k)
		}
	}

	post(url.Values{"reset_GDRIVE_CLIENT_ID": {"1"}})
	if _, ok := saved()["GDRIVE_CLIENT_ID"]; ok {
		t.Fatal("undo didn't remove the saved value")
	}
	post(url.Values{"clear_GDRIVE_CLIENT_SECRET": {"1"}})
	if saved()["GDRIVE_CLIENT_SECRET"] != "" {
		t.Fatal("clear didn't empty the secret")
	}
}
