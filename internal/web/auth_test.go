package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/auth"
	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

const testHost = "localhost:8787"

type client struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]string
	ip      string
}

func (c *client) do(method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	c.t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Host = testHost
	req.RemoteAddr = c.ip + ":5555"
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, req)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return w
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	return c.do("GET", path, nil, map[string]string{"Accept": "text/html"})
}

func (c *client) form(path string, v url.Values) *httptest.ResponseRecorder {
	return c.do("POST", path, strings.NewReader(v.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func (c *client) json(path string, v any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(v)
	return c.do("POST", path, bytes.NewReader(b), map[string]string{"Content-Type": "application/json"})
}

func newTestServer(t *testing.T, password string) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{AdminUser: "admin", AdminPassword: password, WebhookToken: "hook", SessionTTL: 24 * time.Hour, WatchedPercent: 80}
	var out io.Writer = io.Discard
	if testing.Verbose() {
		out = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(out, nil))
	eng := core.New(context.Background(), cfg, st, nil, nil, log)
	return New(cfg, st, eng, log).Handler(), st
}

func newClient(t *testing.T, h http.Handler, ip string) *client {
	return &client{t: t, h: h, cookies: map[string]string{}, ip: ip}
}

func TestOpenModeAndPasswordLogin(t *testing.T) {
	h, _ := newTestServer(t, "")
	if w := newClient(t, h, "10.0.0.1").get("/"); w.Code != 200 {
		t.Fatalf("no password set: dashboard should be open, got %d", w.Code)
	}

	h, _ = newTestServer(t, "s3cret")
	c := newClient(t, h, "10.0.0.2")
	if w := c.get("/history"); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=%2Fhistory") {
		t.Fatalf("should redirect to login, got %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/api/stats", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("API without auth: %d", w.Code)
	}
	if w := c.get("/static/app.css"); w.Code != 200 {
		t.Fatal("static files must load on the login page")
	}
	if w := c.form("/login", url.Values{"username": {"admin"}, "password": {"nope"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {"/history"}}); w.Header().Get("Location") != "/history" {
		t.Fatalf("login should redirect to next, got %s", w.Header().Get("Location"))
	}
	if w := c.get("/history"); w.Code != 200 {
		t.Fatalf("signed in: %d", w.Code)
	}
	if w := c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {"//evil.example"}}); w.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %s", w.Header().Get("Location"))
	}
	// Basic auth still works while there's no second factor.
	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.SetBasicAuth("admin", "s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("basic auth without MFA: %d", w.Code)
	}
	// Logout ends the session.
	c.form("/logout", nil)
	if w := c.get("/history"); w.Code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", w.Code)
	}
}

func TestLockoutAfterFailures(t *testing.T) {
	h, _ := newTestServer(t, "s3cret")
	c := newClient(t, h, "10.0.0.3")
	for i := 0; i < 10; i++ {
		c.form("/login", url.Values{"username": {"admin"}, "password": {"bad"}})
	}
	if w := c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("should be locked out, got %d", w.Code)
	}
	if w := newClient(t, h, "10.0.0.4").form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("another address must not be locked out: %d", w.Code)
	}
}

func TestTOTPRecoveryAndTokens(t *testing.T) {
	h, st := newTestServer(t, "s3cret")
	ctx := context.Background()
	a := newClient(t, h, "10.0.1.1")
	a.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})

	// Set up the authenticator app.
	a.form("/security/totp/start", nil)
	tt, _ := st.GetTOTP(ctx, "admin")
	if tt == nil || tt.Enabled {
		t.Fatal("pending secret not stored")
	}
	if page := a.get("/security").Body.String(); !strings.Contains(page, "data:image/png;base64,") || !strings.Contains(page, tt.Secret) {
		t.Fatal("QR code and secret should be shown")
	}
	if w := a.form("/security/totp/confirm", url.Values{"code": {"000000"}}); !strings.Contains(w.Body.String(), "didn") {
		t.Fatal("wrong confirmation code accepted")
	}
	code, _ := auth.TOTPCode(tt.Secret, auth.Step(time.Now()))
	w := a.form("/security/totp/confirm", url.Values{"code": {code}})
	codes := regexp.MustCompile(`<li>([a-z2-9]{4}-[a-z2-9]{4}-[a-z2-9]{4})</li>`).FindAllStringSubmatch(w.Body.String(), -1)
	if len(codes) != 10 {
		t.Fatalf("recovery codes not shown once enabled: %d", len(codes))
	}

	// A new browser now needs the second step.
	b := newClient(t, h, "10.0.1.2")
	if w := b.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}, "next": {"/sync"}}); !strings.HasPrefix(w.Header().Get("Location"), "/login/mfa") {
		t.Fatalf("password alone should lead to the MFA step: %s", w.Header().Get("Location"))
	}
	if w := b.get("/sync"); w.Code != http.StatusSeeOther {
		t.Fatal("half-signed-in session must not open pages")
	}
	if w := b.form("/login/mfa", url.Values{"code": {"123456"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", w.Code)
	}
	// The code used to enable TOTP can't be replayed.
	if w := b.form("/login/mfa", url.Values{"code": {code}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code accepted: %d", w.Code)
	}
	// A recovery code works once.
	if w := b.form("/login/mfa", url.Values{"code": {strings.ToUpper(codes[0][1])}, "next": {"/sync"}}); w.Header().Get("Location") != "/sync" {
		t.Fatalf("recovery code: %d %s", w.Code, w.Body.String())
	}
	if w := b.get("/sync"); w.Code != 200 {
		t.Fatalf("after MFA: %d", w.Code)
	}
	c := newClient(t, h, "10.0.1.3")
	c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	if w := c.form("/login/mfa", url.Values{"code": {codes[0][1]}}); w.Code != http.StatusUnauthorized {
		t.Fatal("a recovery code worked twice")
	}

	// Basic auth is refused now; API tokens work.
	req := httptest.NewRequest("GET", "/api/stats", nil)
	req.SetBasicAuth("admin", "s3cret")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized || !strings.Contains(rw.Body.String(), "API token") {
		t.Fatalf("basic auth must not bypass MFA: %d %s", rw.Code, rw.Body.String())
	}
	tok := regexp.MustCompile(`bt_[A-Za-z0-9_-]{40,}`).FindString(a.form("/security/tokens", url.Values{"name": {"script"}}).Body.String())
	if tok == "" {
		t.Fatal("token not shown")
	}
	api := newClient(t, h, "10.0.1.4")
	if w := api.do("GET", "/api/stats", nil, map[string]string{"Authorization": "Bearer " + tok}); w.Code != 200 {
		t.Fatalf("API token: %d", w.Code)
	}
	if w := api.do("GET", "/api/stats", nil, map[string]string{"Authorization": "Bearer bt_wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatal("bad token accepted")
	}

	// "Sign out everywhere else" ends b's session but keeps a's.
	a.form("/security/sessions/others", nil)
	if b.get("/sync").Code != http.StatusSeeOther || a.get("/sync").Code != 200 {
		t.Fatal("sign out others")
	}

	// Webhooks keep working with their own token.
	if w := api.do("POST", "/api/scrobble/hook", strings.NewReader(`{"action":"bogus"}`), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("scrobble endpoint should be reachable without sign-in: %d", w.Code)
	}
}

// ---------- a software passkey ----------

type softKey struct {
	priv  *ecdsa.PrivateKey
	id    []byte
	user  []byte
	count uint32
}

func cbor(v any) []byte {
	var b bytes.Buffer
	var enc func(any)
	head := func(major byte, n uint64) {
		switch {
		case n < 24:
			b.WriteByte(major<<5 | byte(n))
		case n < 256:
			b.WriteByte(major<<5 | 24)
			b.WriteByte(byte(n))
		default:
			b.WriteByte(major<<5 | 25)
			binary.Write(&b, binary.BigEndian, uint16(n))
		}
	}
	enc = func(v any) {
		switch x := v.(type) {
		case int:
			if x >= 0 {
				head(0, uint64(x))
			} else {
				head(1, uint64(-1-x))
			}
		case string:
			head(3, uint64(len(x)))
			b.WriteString(x)
		case []byte:
			head(2, uint64(len(x)))
			b.Write(x)
		case [][2]any: // ordered map
			head(5, uint64(len(x)))
			for _, kv := range x {
				enc(kv[0])
				enc(kv[1])
			}
		}
	}
	enc(v)
	return b.Bytes()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		b, _ = base64.StdEncoding.DecodeString(s)
	}
	return b
}

func (k *softKey) authData(rpID string, attested bool) []byte {
	var b bytes.Buffer
	h := sha256.Sum256([]byte(rpID))
	b.Write(h[:])
	flags := byte(0x01 | 0x04) // user present, user verified
	if attested {
		flags |= 0x40
	}
	b.WriteByte(flags)
	k.count++
	binary.Write(&b, binary.BigEndian, k.count)
	if attested {
		b.Write(make([]byte, 16)) // AAGUID
		binary.Write(&b, binary.BigEndian, uint16(len(k.id)))
		b.Write(k.id)
		x, y := k.priv.PublicKey.X.FillBytes(make([]byte, 32)), k.priv.PublicKey.Y.FillBytes(make([]byte, 32))
		b.Write(cbor([][2]any{{1, 2}, {3, -7}, {-1, 1}, {-2, x}, {-3, y}}))
	}
	return b.Bytes()
}

func clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": "http://" + testHost, "crossOrigin": false})
	return b
}

func (k *softKey) register(t *testing.T, c *client, name string) map[string]any {
	t.Helper()
	w := c.json("/security/passkeys/begin", nil)
	if w.Code != 200 {
		t.Fatalf("begin registration: %d %s", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	json.Unmarshal(w.Body.Bytes(), &opts)
	k.user = unb64(opts.PublicKey.User.ID)
	cd := clientData("webauthn.create", opts.PublicKey.Challenge)
	att := cbor([][2]any{{"fmt", "none"}, {"attStmt", [][2]any{}}, {"authData", k.authData(opts.PublicKey.RP.ID, true)}})
	w = c.json("/security/passkeys/finish?name="+url.QueryEscape(name), map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(cd), "attestationObject": b64(att)},
	})
	if w.Code != 200 {
		t.Fatalf("finish registration: %d %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	return res
}

func (k *softKey) login(t *testing.T, c *client) *httptest.ResponseRecorder {
	t.Helper()
	w := c.json("/login/passkey/begin", nil)
	if w.Code != 200 {
		t.Fatalf("begin login: %d %s", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
		} `json:"publicKey"`
	}
	json.Unmarshal(w.Body.Bytes(), &opts)
	cd := clientData("webauthn.get", opts.PublicKey.Challenge)
	ad := k.authData(opts.PublicKey.RPID, false)
	sum := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), sum[:]...)) // ES256 signs SHA-256(authData || SHA-256(clientData))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	return c.json("/login/passkey/finish?next=%2Fsync", map[string]any{
		"id": b64(k.id), "rawId": b64(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(cd), "authenticatorData": b64(ad), "signature": b64(sig), "userHandle": b64(k.user)},
	})
}

func newSoftKey() *softKey {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	rand.Read(id)
	return &softKey{priv: priv, id: id}
}

func TestPasskeys(t *testing.T) {
	h, st := newTestServer(t, "s3cret")
	ctx := context.Background()
	a := newClient(t, h, "10.0.2.1")
	a.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	key := newSoftKey()
	res := key.register(t, a, "Test key")
	if codes, _ := res["recovery_codes"].([]any); len(codes) != 10 {
		t.Fatalf("first passkey should create recovery codes: %v", res)
	}
	ps, _ := st.Passkeys(ctx, "admin")
	if len(ps) != 1 || ps[0].Name != "Test key" || ps[0].RPID != "localhost" {
		t.Fatalf("stored passkey: %+v", ps)
	}

	// Passkey alone signs in.
	b := newClient(t, h, "10.0.2.2")
	if w := key.login(t, b); w.Code != 200 || !strings.Contains(w.Body.String(), `"next":"/sync"`) {
		t.Fatalf("passkey sign-in: %d %s", w.Code, w.Body.String())
	}
	if w := b.get("/sync"); w.Code != 200 {
		t.Fatalf("after passkey sign-in: %d", w.Code)
	}

	// Password + passkey as the second step.
	c := newClient(t, h, "10.0.2.3")
	if w := c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}}); !strings.HasPrefix(w.Header().Get("Location"), "/login/mfa") {
		t.Fatal("with a passkey registered, the password needs a second step")
	}
	if w := key.login(t, c); w.Code != 200 {
		t.Fatalf("passkey as second step: %d %s", w.Code, w.Body.String())
	}
	if c.get("/sync").Code != 200 {
		t.Fatal("second step not completed")
	}

	// A different key (not registered) is refused.
	other := newSoftKey()
	other.user = key.user
	d := newClient(t, h, "10.0.2.4")
	if w := other.login(t, d); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown passkey accepted: %d", w.Code)
	}
	// A finish without a begin (replayed response) is refused.
	if w := d.json("/login/passkey/finish", map[string]any{"id": "x"}); w.Code != http.StatusBadRequest {
		t.Fatalf("finish without begin: %d", w.Code)
	}

	// Signature counter is stored.
	ps, _ = st.Passkeys(ctx, "admin")
	if ps[0].LastUsed == 0 || !strings.Contains(string(ps[0].Credential), `"signCount":3`) {
		t.Fatalf("credential not updated after use: %s", ps[0].Credential)
	}
}

func TestPasskeysNeedSecureAddress(t *testing.T) {
	h, _ := newTestServer(t, "s3cret")
	c := newClient(t, h, "10.0.3.1")
	c.form("/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	req := httptest.NewRequest("POST", "/security/passkeys/begin", nil)
	req.Host = "192.168.1.20:8787"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookies[sessionCookie]})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "domain name") {
		t.Fatalf("IP address should be refused for passkeys: %d %s", w.Code, w.Body.String())
	}
}

// A plain-HTTP LAN visit must work even when BASE_URL is an HTTPS domain
// (cookies must not be marked Secure there), and HTTPS through a trusted
// proxy enables passkeys for that domain.
func TestCookiesAndOriginFollowTheRequest(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	defer st.Close()
	cfg := &config.Config{AdminUser: "admin", AdminPassword: "pw", SessionTTL: time.Hour, BaseURL: "https://bt.example.com", TrustProxy: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(cfg, st, core.New(context.Background(), cfg, st, nil, nil, log), log).Handler()

	req := httptest.NewRequest("POST", "/login", strings.NewReader("username=admin&password=pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "192.168.1.10:8787"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	ck := w.Result().Cookies()
	if len(ck) == 0 || ck[0].Secure {
		t.Fatalf("LAN http login must get a non-Secure cookie: %+v", ck)
	}

	req = httptest.NewRequest("POST", "/login", strings.NewReader("username=admin&password=pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "bt.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	ck = w.Result().Cookies()
	if len(ck) == 0 || !ck[0].Secure {
		t.Fatalf("HTTPS login must get a Secure cookie: %+v", ck)
	}
	req = httptest.NewRequest("POST", "/security/passkeys/begin", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "bt.example.com"
	req.AddCookie(ck[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"bt.example.com"`) {
		t.Fatalf("passkeys over HTTPS via proxy: %d %s", w.Code, w.Body.String())
	}
}
