package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestActiveSessionRenewsCookie(t *testing.T) {
	h, st := newTestServer(t, "s3cret")
	c := newClient(t, h, "10.0.1.1")
	c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	before := c.cookies[sessionCookie]
	if before == "" {
		t.Fatal("no session cookie after sign-in")
	}
	old := time.Now().Add(-time.Hour).Unix()
	if _, err := st.DB.Exec(`UPDATE auth_sessions SET seen_at=?`, old); err != nil {
		t.Fatal(err)
	}
	w := c.get("/history")
	for _, ck := range w.Result().Cookies() {
		if ck.Name == sessionCookie {
			if ck.Value != before || ck.MaxAge != int((24*time.Hour).Seconds()) {
				t.Fatalf("renewed cookie: same value=%v max-age=%d", ck.Value == before, ck.MaxAge)
			}
			return
		}
	}
	t.Fatal("an active session must renew its cookie, or the device signs out anyway")
}

func TestAppHidesPasskeyWhenPasswordExists(t *testing.T) {
	h, st := newTestServer(t, "s3cret")
	err := st.AddPasskey(context.Background(), store.Passkey{ID: "k1", User: "admin", Name: "Test", RPID: "localhost", Credential: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, h, "10.0.1.2")
	browser := c.do("GET", "/login", nil, map[string]string{"Accept": "text/html", "User-Agent": "Mozilla/5.0 Safari/605.1.15"})
	if !strings.Contains(browser.Body.String(), "Sign in with a passkey") {
		t.Fatal("browsers must keep the passkey button")
	}
	app := c.do("GET", "/login", nil, map[string]string{"Accept": "text/html", "User-Agent": "Mozilla/5.0 BackTrackarrApp/1.0"})
	body := app.Body.String()
	if strings.Contains(body, "Sign in with a passkey") || !strings.Contains(body, "Passkeys only work in a web browser") {
		t.Fatal("the app should explain passkeys instead of showing a button that fails")
	}
}
