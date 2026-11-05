// Package auth holds the pieces of sign-in that don't depend on HTTP:
// TOTP (RFC 6238) codes, recovery codes, random tokens, and a failed-attempt
// limiter.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ---------- random tokens ----------

// Token returns n random bytes, base64url encoded.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source failing is not recoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---------- TOTP (authenticator apps) ----------

const (
	totpPeriod = 30
	totpDigits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a 160-bit secret in the base32 form authenticator
// apps expect.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPCode computes the code for one 30-second time step.
func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("bad TOTP secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000), nil
}

// Step is the TOTP time step for t.
func Step(t time.Time) int64 { return t.Unix() / totpPeriod }

// VerifyTOTP checks a code against the current step and one step either
// side (for clock drift), returning the matching step. Callers must reject
// a step that was already used, so a code can't be replayed.
func VerifyTOTP(secret, code string, now time.Time) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := Step(now)
	for _, step := range []int64{cur, cur - 1, cur + 1} {
		want, err := TOTPCode(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURI is what the QR code holds for authenticator apps.
func OTPAuthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// ---------- recovery codes ----------

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i

// NewRecoveryCodes returns n codes like "k7wq-m3xz-p9re" (~70 bits each).
func NewRecoveryCodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var s strings.Builder
		for j, c := range b {
			if j > 0 && j%4 == 0 {
				s.WriteByte('-')
			}
			s.WriteByte(recoveryAlphabet[int(c)%len(recoveryAlphabet)])
		}
		out[i] = s.String()
	}
	return out
}

// NormalizeRecoveryCode makes typed codes comparable: lowercase, no spaces
// or dashes, then regrouped.
func NormalizeRecoveryCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	raw := b.String()
	var out strings.Builder
	for i, r := range raw {
		if i > 0 && i%4 == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// ---------- failed-attempt limiter ----------

// Limiter blocks a key (an IP address) after Max failures within Window.
type Limiter struct {
	Max    int
	Window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, fails: map[string][]time.Time{}}
}

func (l *Limiter) recent(key string, now time.Time) []time.Time {
	list := l.fails[key]
	i := 0
	for i < len(list) && now.Sub(list[i]) > l.Window {
		i++
	}
	list = list[i:]
	if len(list) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = list
	}
	return list
}

// Blocked reports whether key is locked out and for how long.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	list := l.recent(key, now)
	if len(list) < l.Max {
		return false, 0
	}
	return true, l.Window - now.Sub(list[0])
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.fails[key] = append(l.recent(key, now), now)
	if len(l.fails) > 10000 { // bound memory under a flood of addresses
		for k := range l.fails {
			delete(l.fails, k)
			if len(l.fails) < 5000 {
				break
			}
		}
	}
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}
