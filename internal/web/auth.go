package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"rsc.io/qr"

	"github.com/DrSeussFreak/BackTrackarr/internal/auth"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Sign-in: a password (ADMIN_USER / ADMIN_PASSWORD), then a second factor
// when one is set up (authenticator code, passkey or recovery code), or a
// passkey alone. Scripts use API tokens. Plain HTTP basic auth still works
// until a second factor exists, so existing setups keep working.

const (
	sessionCookie  = "bt_session"
	ceremonyCookie = "bt_webauthn"
	pendingTTL     = 10 * time.Minute
	issuer         = "BackTrackarr"
)

type authKey struct{}

// authInfo is attached to the request context of signed-in requests.
type authInfo struct {
	User      string
	SessionID string // hash of the current session ("" for API tokens / basic auth)
}

func currentAuth(r *http.Request) authInfo {
	a, _ := r.Context().Value(authKey{}).(authInfo)
	return a
}

type authState2 struct {
	limiter *auth.Limiter

	mu         sync.Mutex
	ceremonies map[string]ceremony
	rps        map[string]*webauthn.WebAuthn
}

type ceremony struct {
	kind    string // register | login
	user    string
	data    webauthn.SessionData
	expires time.Time
}

func newAuthState() *authState2 {
	return &authState2{limiter: auth.NewLimiter(10, 15*time.Minute), ceremonies: map[string]ceremony{}, rps: map[string]*webauthn.WebAuthn{}}
}

// ---------- helpers ----------

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return strings.TrimSpace(v)
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			first, _, _ := strings.Cut(v, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isHTTPS reports whether the browser reached us over HTTPS: directly, via a
// trusted proxy, or at the HTTPS BASE_URL (same host). It decides the
// cookie Secure flag and the passkey origin, so it must describe this
// request, not the configured address in general.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	if u, err := url.Parse(s.cfg.BaseURL); err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, r.Host) {
		return true // a TLS-terminating proxy that forwards the Host header
	}
	return false
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r)}
	if ttl > 0 {
		c.MaxAge = int(ttl.Seconds())
	} else {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func (s *Server) hasPasskeys(ctx context.Context) bool {
	ps, _ := s.st.Passkeys(ctx, s.cfg.AdminUser)
	return len(ps) > 0
}

func (s *Server) totpEnabled(ctx context.Context) bool {
	t, _ := s.st.GetTOTP(ctx, s.cfg.AdminUser)
	return t != nil && t.Enabled
}

// authEnabled: sign-in is required once a password or a passkey exists.
func (s *Server) authEnabled(ctx context.Context) bool {
	return s.cfg.AdminPassword != "" || s.hasPasskeys(ctx)
}

func (s *Server) mfaEnabled(ctx context.Context) bool {
	return s.totpEnabled(ctx) || s.hasPasskeys(ctx)
}

func (s *Server) sessionFromCookie(r *http.Request) (*store.AuthSession, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, ""
	}
	h := store.HashSecret(c.Value)
	sess, err := s.st.GetAuthSession(r.Context(), h)
	if err != nil || sess == nil || sess.User != s.cfg.AdminUser {
		return nil, ""
	}
	return sess, h
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, mfaDone bool) error {
	// Always a fresh id after any step of sign-in (no session fixation).
	if _, old := s.sessionFromCookie(r); old != "" {
		_ = s.st.DeleteAuthSession(r.Context(), old)
	}
	id := auth.Token(32)
	ttl := s.cfg.SessionTTL
	if !mfaDone {
		ttl = pendingTTL
	}
	agent := r.UserAgent()
	if len(agent) > 200 {
		agent = agent[:200]
	}
	if err := s.st.CreateAuthSession(r.Context(), store.HashSecret(id), s.cfg.AdminUser, mfaDone, ttl, s.clientIP(r), agent); err != nil {
		return err
	}
	s.setCookie(w, r, sessionCookie, id, ttl)
	return nil
}

// safeNext only allows redirects back into this site.
func safeNext(v string) string {
	if v == "" || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") {
		return "/"
	}
	return v
}

func wantsHTML(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ---------- middleware ----------

// guard protects every admin page and API route.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		ctx := r.Context()
		if !s.authEnabled(ctx) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, authKey{}, authInfo{User: s.cfg.AdminUser})))
			return
		}
		// API tokens (scripts, dashboards).
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			if user, _ := s.st.CheckAPIToken(ctx, store.HashSecret(strings.TrimPrefix(h, "Bearer "))); user == s.cfg.AdminUser && user != "" {
				next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, authKey{}, authInfo{User: user})))
				return
			}
			http.Error(w, "invalid API token", http.StatusUnauthorized)
			return
		}
		// Basic auth, only while no second factor is set up.
		if u, p, ok := r.BasicAuth(); ok {
			ip := s.clientIP(r)
			if blocked, _ := s.signin.limiter.Blocked(ip); blocked {
				http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
				return
			}
			if s.cfg.AdminPassword != "" && subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.AdminUser)) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AdminPassword)) == 1 {
				if s.mfaEnabled(ctx) {
					http.Error(w, "two-step sign-in is on: sign in on the login page, or use an API token (Security page)", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, authKey{}, authInfo{User: u})))
				return
			}
			s.signin.limiter.Fail(ip)
		}
		if sess, h := s.sessionFromCookie(r); sess != nil && sess.MFADone {
			if time.Since(time.Unix(sess.SeenAt, 0)) > 5*time.Minute {
				_ = s.st.TouchAuthSession(ctx, h, s.cfg.SessionTTL)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, authKey{}, authInfo{User: sess.User, SessionID: h})))
			return
		}
		if wantsHTML(r) {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "sign in required", http.StatusUnauthorized)
	})
}

// ---------- login pages ----------

func (s *Server) routesAuth(root *http.ServeMux) {
	root.HandleFunc("GET /login", s.loginPage)
	root.HandleFunc("POST /login", s.loginPassword)
	root.HandleFunc("GET /login/mfa", s.mfaPage)
	root.HandleFunc("POST /login/mfa", s.mfaCode)
	root.HandleFunc("POST /login/passkey/begin", s.passkeyLoginBegin)
	root.HandleFunc("POST /login/passkey/finish", s.passkeyLoginFinish)
	root.HandleFunc("POST /logout", s.logout)
}

func (s *Server) loginData(r *http.Request, msg string) M {
	_, rpErr := s.relyingParty(r)
	return M{"Title": "Sign in", "Bare": true, "Next": safeNext(r.FormValue("next")), "Error": msg,
		"HasPassword": s.cfg.AdminPassword != "", "HasPasskeys": s.hasPasskeys(r.Context()), "PasskeyErr": errString(rpErr)}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled(r.Context()) {
		back(w, r, safeNext(r.FormValue("next")))
		return
	}
	if sess, _ := s.sessionFromCookie(r); sess != nil && sess.MFADone {
		back(w, r, safeNext(r.FormValue("next")))
		return
	}
	s.render(w, "login", s.loginData(r, ""))
}

func (s *Server) tooMany(w http.ResponseWriter, r *http.Request, page string, data M) bool {
	if blocked, wait := s.signin.limiter.Blocked(s.clientIP(r)); blocked {
		data["Error"] = fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1)
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, page, data)
		return true
	}
	return false
}

func (s *Server) loginPassword(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	data := s.loginData(r, "")
	if s.tooMany(w, r, "login", data) {
		return
	}
	u, p := r.FormValue("username"), r.FormValue("password")
	ok := s.cfg.AdminPassword != "" && subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.AdminUser)) == 1 &&
		subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.AdminPassword)) == 1
	if !ok {
		s.signin.limiter.Fail(s.clientIP(r))
		s.log.Warn("failed sign-in", "ip", s.clientIP(r))
		data["Error"] = "Wrong username or password."
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", data)
		return
	}
	mfa := s.mfaEnabled(r.Context())
	if err := s.startSession(w, r, !mfa); err != nil {
		s.fail(w, err)
		return
	}
	if mfa {
		back(w, r, "/login/mfa?next="+url.QueryEscape(safeNext(r.FormValue("next"))))
		return
	}
	s.signin.limiter.Reset(s.clientIP(r))
	back(w, r, safeNext(r.FormValue("next")))
}

func (s *Server) mfaData(r *http.Request, msg string) M {
	ctx := r.Context()
	left, _ := s.st.RecoveryCodesLeft(ctx, s.cfg.AdminUser)
	_, rpErr := s.relyingParty(r)
	return M{"Title": "Two-step sign-in", "Bare": true, "Next": safeNext(r.FormValue("next")), "Error": msg,
		"TOTP": s.totpEnabled(ctx), "HasPasskeys": s.hasPasskeys(ctx), "RecoveryLeft": left, "PasskeyErr": errString(rpErr)}
}

func (s *Server) mfaPage(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.sessionFromCookie(r)
	if sess == nil {
		back(w, r, "/login?next="+url.QueryEscape(safeNext(r.FormValue("next"))))
		return
	}
	if sess.MFADone {
		back(w, r, safeNext(r.FormValue("next")))
		return
	}
	s.render(w, "mfa", s.mfaData(r, ""))
}

// checkSecondFactor accepts a current authenticator code or an unused
// recovery code.
func (s *Server) checkSecondFactor(ctx context.Context, code string) (bool, string, error) {
	code = strings.TrimSpace(code)
	digits := strings.ReplaceAll(code, " ", "")
	if len(digits) == 6 && strings.Trim(digits, "0123456789") == "" {
		t, err := s.st.GetTOTP(ctx, s.cfg.AdminUser)
		if err != nil || t == nil || !t.Enabled {
			return false, "", err
		}
		step, ok := auth.VerifyTOTP(t.Secret, digits, time.Now())
		if !ok {
			return false, "", nil
		}
		fresh, err := s.st.UseTOTPStep(ctx, s.cfg.AdminUser, step)
		if !fresh {
			return false, "That code was already used. Wait for the next one.", err
		}
		return true, "", err
	}
	ok, err := s.st.UseRecoveryCode(ctx, s.cfg.AdminUser, store.HashSecret(auth.NormalizeRecoveryCode(code)))
	return ok, "", err
}

func (s *Server) mfaCode(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	sess, _ := s.sessionFromCookie(r)
	if sess == nil {
		back(w, r, "/login")
		return
	}
	data := s.mfaData(r, "")
	if s.tooMany(w, r, "mfa", data) {
		return
	}
	ok, why, err := s.checkSecondFactor(r.Context(), r.FormValue("code"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		s.signin.limiter.Fail(s.clientIP(r))
		if why == "" {
			why = "That code didn't work."
		}
		data["Error"] = why
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "mfa", data)
		return
	}
	if err := s.startSession(w, r, true); err != nil {
		s.fail(w, err)
		return
	}
	s.signin.limiter.Reset(s.clientIP(r))
	back(w, r, safeNext(r.FormValue("next")))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, h := s.sessionFromCookie(r); h != "" {
		_ = s.st.DeleteAuthSession(r.Context(), h)
	}
	s.setCookie(w, r, sessionCookie, "", 0)
	back(w, r, "/login")
}

// ---------- passkeys (WebAuthn) ----------

type waUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.id }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.name + " (BackTrackarr)" }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// relyingParty returns the WebAuthn configuration for the address the
// browser is using. Browsers only allow passkeys on HTTPS (or localhost),
// and on a domain name rather than an IP address.
func (s *Server) relyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	// The origin is the address in the browser's address bar.
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("can't work out this site's address")
	}
	host := u.Hostname()
	local := host == "localhost" || strings.HasSuffix(host, ".localhost")
	if net.ParseIP(host) != nil {
		return nil, fmt.Errorf("passkeys need a domain name (e.g. https://backtrackarr.example.com), not an IP address")
	}
	if u.Scheme != "https" && !local {
		return nil, fmt.Errorf("passkeys need HTTPS (put BackTrackarr behind a reverse proxy with a certificate and set BASE_URL)")
	}
	key := u.Scheme + "://" + u.Host
	s.signin.mu.Lock()
	defer s.signin.mu.Unlock()
	if rp, ok := s.signin.rps[key]; ok {
		return rp, nil
	}
	rp, err := webauthn.New(&webauthn.Config{RPID: host, RPDisplayName: issuer, RPOrigins: []string{key}})
	if err != nil {
		return nil, err
	}
	s.signin.rps[key] = rp
	return rp, nil
}

func (s *Server) waUser(ctx context.Context, rpID string) (*waUser, error) {
	idHex, err := s.st.EnsureSecret(ctx, "webauthn_user_handle|"+s.cfg.AdminUser)
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(idHex)
	if err != nil {
		return nil, err
	}
	u := &waUser{id: id, name: s.cfg.AdminUser}
	ps, err := s.st.Passkeys(ctx, s.cfg.AdminUser)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if rpID != "" && p.RPID != rpID {
			continue // a passkey only works on the domain it was made for
		}
		var c webauthn.Credential
		if json.Unmarshal(p.Credential, &c) == nil {
			u.creds = append(u.creds, c)
		}
	}
	return u, nil
}

func (s *Server) saveCeremony(w http.ResponseWriter, r *http.Request, c ceremony) {
	id := auth.Token(24)
	c.expires = time.Now().Add(5 * time.Minute)
	s.signin.mu.Lock()
	for k, v := range s.signin.ceremonies {
		if time.Now().After(v.expires) {
			delete(s.signin.ceremonies, k)
		}
	}
	s.signin.ceremonies[id] = c
	s.signin.mu.Unlock()
	s.setCookie(w, r, ceremonyCookie, id, 5*time.Minute)
}

func (s *Server) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) (ceremony, bool) {
	ck, err := r.Cookie(ceremonyCookie)
	if err != nil {
		return ceremony{}, false
	}
	s.setCookie(w, r, ceremonyCookie, "", 0)
	s.signin.mu.Lock()
	defer s.signin.mu.Unlock()
	c, ok := s.signin.ceremonies[ck.Value]
	delete(s.signin.ceremonies, ck.Value) // one use only
	if !ok || c.kind != kind || time.Now().After(c.expires) {
		return ceremony{}, false
	}
	return c, true
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(M{"error": msg})
}

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if blocked, _ := s.signin.limiter.Blocked(s.clientIP(r)); blocked {
		jsonError(w, http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.waUser(r.Context(), rp.Config.RPID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(u.creds) == 0 {
		jsonError(w, http.StatusBadRequest, "No passkey is registered for this address.")
		return
	}
	var opts *protocol.CredentialAssertion
	var data *webauthn.SessionData
	if sess, _ := s.sessionFromCookie(r); sess != nil && !sess.MFADone {
		// Second step after the password: this account's passkeys only.
		opts, data, err = rp.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationPreferred))
	} else {
		// Passkey instead of password: the passkey itself proves two factors
		// (the device and its PIN/biometric), so user verification is required.
		opts, data, err = rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.saveCeremony(w, r, ceremony{kind: "login", user: u.name, data: *data})
	writeJSON(w, opts)
}

func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	c, ok := s.takeCeremony(w, r, "login")
	if !ok {
		jsonError(w, http.StatusBadRequest, "The sign-in request expired. Try again.")
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.waUser(r.Context(), rp.Config.RPID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var cred *webauthn.Credential
	if c.data.UserID != nil && len(c.data.AllowedCredentialIDs) > 0 {
		cred, err = rp.FinishLogin(u, c.data, r)
	} else {
		_, cred, err = rp.FinishPasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			if subtle.ConstantTimeCompare(userHandle, u.id) != 1 {
				return nil, errors.New("unknown passkey")
			}
			return u, nil
		}, c.data, r)
	}
	if err != nil {
		s.signin.limiter.Fail(ip)
		s.log.Warn("failed passkey sign-in", "ip", ip, "err", err)
		jsonError(w, http.StatusUnauthorized, "That passkey wasn't accepted.")
		return
	}
	if b, err := json.Marshal(cred); err == nil {
		_ = s.st.UpdatePasskeyUse(r.Context(), base64.RawURLEncoding.EncodeToString(cred.ID), b)
	}
	if cred.Authenticator.CloneWarning {
		s.log.Warn("passkey signature counter went backwards: the authenticator may have been cloned", "credential", base64.RawURLEncoding.EncodeToString(cred.ID))
	}
	if err := s.startSession(w, r, true); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.signin.limiter.Reset(ip)
	writeJSON(w, M{"ok": true, "next": safeNext(r.URL.Query().Get("next"))})
}

// ---------- Security page ----------

func (s *Server) routesSecurity(admin *http.ServeMux) {
	admin.HandleFunc("GET /security", s.securityPage)
	admin.HandleFunc("POST /security/totp/start", s.totpStart)
	admin.HandleFunc("POST /security/totp/confirm", s.totpConfirm)
	admin.HandleFunc("POST /security/totp/disable", s.totpDisable)
	admin.HandleFunc("POST /security/recovery", s.recoveryRegenerate)
	admin.HandleFunc("POST /security/passkeys/begin", s.passkeyRegisterBegin)
	admin.HandleFunc("POST /security/passkeys/finish", s.passkeyRegisterFinish)
	admin.HandleFunc("POST /security/passkeys/{id}/delete", s.passkeyDelete)
	admin.HandleFunc("POST /security/tokens", s.tokenCreate)
	admin.HandleFunc("POST /security/tokens/{hash}/delete", s.tokenDelete)
	admin.HandleFunc("POST /security/sessions/others", s.sessionsRevokeOthers)
	admin.HandleFunc("POST /security/sessions/{hash}/delete", s.sessionRevoke)
}

func qrDataURI(text string) string {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	c.Scale = 5
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG())
}

func (s *Server) securityData(r *http.Request, extra M) (M, error) {
	ctx := r.Context()
	user := s.cfg.AdminUser
	t, err := s.st.GetTOTP(ctx, user)
	if err != nil {
		return nil, err
	}
	passkeys, err := s.st.Passkeys(ctx, user)
	if err != nil {
		return nil, err
	}
	tokens, err := s.st.APITokens(ctx, user)
	if err != nil {
		return nil, err
	}
	sessions, err := s.st.ListAuthSessions(ctx, user)
	if err != nil {
		return nil, err
	}
	left, _ := s.st.RecoveryCodesLeft(ctx, user)
	rp, rpErr := s.relyingParty(r)
	rpID := ""
	if rp != nil {
		rpID = rp.Config.RPID
	}
	data := M{"Title": "Security", "Nav": "security", "User": user, "HasPassword": s.cfg.AdminPassword != "",
		"AuthEnabled": s.authEnabled(ctx), "TOTPOn": t != nil && t.Enabled, "Passkeys": passkeys, "RPID": rpID,
		"PasskeyErr": errString(rpErr), "Tokens": tokens, "Sessions": sessions, "Current": currentAuth(r).SessionID,
		"RecoveryLeft": left, "SessionDays": int(s.cfg.SessionTTL.Hours() / 24)}
	if t != nil && !t.Enabled {
		uri := auth.OTPAuthURI(issuer, user, t.Secret)
		data["PendingSecret"] = t.Secret
		data["PendingQR"] = template.URL(qrDataURI(uri))
	}
	for k, v := range extra {
		data[k] = v
	}
	return data, nil
}

func (s *Server) renderSecurity(w http.ResponseWriter, r *http.Request, extra M) {
	data, err := s.securityData(r, extra)
	if err != nil {
		s.fail(w, err)
		return
	}
	if f := takeFlash(w, r); f != "" {
		data["Flash"] = f
	}
	s.render(w, "security", data)
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request) { s.renderSecurity(w, r, nil) }

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminPassword == "" {
		s.flash(w, r, "/security", "Set ADMIN_PASSWORD first: an authenticator code is a second step after the password.")
		return
	}
	if err := s.st.SetPendingTOTP(r.Context(), s.cfg.AdminUser, auth.NewTOTPSecret()); err != nil {
		s.fail(w, err)
		return
	}
	back(w, r, "/security#totp")
}

// ensureRecoveryCodes creates codes the first time a second factor is set up.
func (s *Server) ensureRecoveryCodes(ctx context.Context) ([]string, error) {
	if left, err := s.st.RecoveryCodesLeft(ctx, s.cfg.AdminUser); err != nil || left > 0 {
		return nil, err
	}
	return s.newRecoveryCodes(ctx)
}

func (s *Server) newRecoveryCodes(ctx context.Context) ([]string, error) {
	codes := auth.NewRecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = store.HashSecret(c)
	}
	return codes, s.st.SetRecoveryCodes(ctx, s.cfg.AdminUser, hashes)
}

func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.st.GetTOTP(ctx, s.cfg.AdminUser)
	if err != nil || t == nil || t.Enabled {
		back(w, r, "/security")
		return
	}
	step, ok := auth.VerifyTOTP(t.Secret, r.FormValue("code"), time.Now())
	if !ok {
		s.renderSecurity(w, r, M{"TOTPError": "That code didn't match. Check the time on your phone and try the next code."})
		return
	}
	if err := s.st.EnableTOTP(ctx, s.cfg.AdminUser, step); err != nil {
		s.fail(w, err)
		return
	}
	codes, err := s.ensureRecoveryCodes(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("authenticator app enabled", "user", s.cfg.AdminUser)
	s.renderSecurity(w, r, M{"NewCodes": codes, "Flash": "Authenticator app is on. Next sign-in will ask for a code."})
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	ok, why, err := s.checkSecondFactor(r.Context(), r.FormValue("code"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		if why == "" {
			why = "Enter a current authenticator code (or a recovery code) to turn it off."
		}
		s.renderSecurity(w, r, M{"TOTPError": why})
		return
	}
	if err := s.st.DeleteTOTP(r.Context(), s.cfg.AdminUser); err != nil {
		s.fail(w, err)
		return
	}
	if !s.hasPasskeys(r.Context()) {
		_ = s.st.SetRecoveryCodes(r.Context(), s.cfg.AdminUser, nil)
	}
	s.log.Info("authenticator app disabled", "user", s.cfg.AdminUser)
	s.flash(w, r, "/security", "Authenticator app turned off.")
}

func (s *Server) recoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	if !s.mfaEnabled(r.Context()) {
		back(w, r, "/security")
		return
	}
	codes, err := s.newRecoveryCodes(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSecurity(w, r, M{"NewCodes": codes, "Flash": "New recovery codes made; the old ones no longer work."})
}

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	rp, err := s.relyingParty(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.waUser(r.Context(), rp.Config.RPID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for i := range u.creds {
		exclude = append(exclude, u.creds[i].Descriptor())
	}
	opts, data, err := rp.BeginRegistration(u,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}),
		webauthn.WithExclusions(exclude))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.saveCeremony(w, r, ceremony{kind: "register", user: u.name, data: *data})
	writeJSON(w, opts)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	c, ok := s.takeCeremony(w, r, "register")
	if !ok {
		jsonError(w, http.StatusBadRequest, "The request expired. Try again.")
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.waUser(r.Context(), rp.Config.RPID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cred, err := rp.FinishRegistration(u, c.data, r)
	if err != nil {
		s.log.Warn("passkey registration failed", "err", err)
		jsonError(w, http.StatusBadRequest, "The passkey couldn't be saved: "+err.Error())
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey " + time.Now().Format("2006-01-02")
	}
	if len(name) > 60 {
		name = name[:60]
	}
	b, _ := json.Marshal(cred)
	ctx := r.Context()
	if err := s.st.AddPasskey(ctx, store.Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), User: u.name,
		Name: name, RPID: rp.Config.RPID, Credential: b}); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	codes, err := s.ensureRecoveryCodes(ctx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("passkey added", "user", u.name, "name", name)
	// Adding a passkey turns sign-in on even without a password; keep this
	// browser signed in.
	if currentAuth(r).SessionID == "" {
		_ = s.startSession(w, r, true)
	}
	writeJSON(w, M{"ok": true, "recovery_codes": codes})
}

func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ps, _ := s.st.Passkeys(ctx, s.cfg.AdminUser)
	if len(ps) == 1 && s.cfg.AdminPassword == "" {
		s.flash(w, r, "/security", "That's your only way to sign in. Set ADMIN_PASSWORD or add another passkey first.")
		return
	}
	if err := s.st.DeletePasskey(ctx, s.cfg.AdminUser, r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	if !s.mfaEnabled(ctx) {
		_ = s.st.SetRecoveryCodes(ctx, s.cfg.AdminUser, nil)
	}
	s.flash(w, r, "/security", "Passkey removed.")
}

func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "API token"
	}
	tok := "bt_" + auth.Token(32)
	if err := s.st.AddAPIToken(r.Context(), store.HashSecret(tok), s.cfg.AdminUser, name); err != nil {
		s.fail(w, err)
		return
	}
	s.renderSecurity(w, r, M{"NewToken": tok, "NewTokenName": name})
}

func (s *Server) tokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteAPIToken(r.Context(), s.cfg.AdminUser, r.PathValue("hash")); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, "/security", "Token revoked.")
}

func (s *Server) sessionsRevokeOthers(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.DeleteAuthSessionsExcept(r.Context(), s.cfg.AdminUser, currentAuth(r).SessionID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, "/security", fmt.Sprintf("Signed out %d other sessions.", n))
}

func (s *Server) sessionRevoke(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteAuthSession(r.Context(), r.PathValue("hash")); err != nil {
		s.fail(w, err)
		return
	}
	s.flash(w, r, "/security", "Session signed out.")
}
