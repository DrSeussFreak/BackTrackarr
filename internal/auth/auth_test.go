package auth

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B test vectors (SHA-1, 8 digits there; the last 6
// digits match a 6-digit code).
func TestTOTPVectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		got, err := TOTPCode(secret, c.unix/30)
		if err != nil || got != c.want {
			t.Errorf("t=%d: got %s want %s (%v)", c.unix, got, c.want, err)
		}
	}
}

func TestVerifyTOTPWindow(t *testing.T) {
	s := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	prev, _ := TOTPCode(s, Step(now)-1)
	if step, ok := VerifyTOTP(s, prev, now); !ok || step != Step(now)-1 {
		t.Fatal("previous step should be accepted for clock drift")
	}
	old, _ := TOTPCode(s, Step(now)-3)
	if _, ok := VerifyTOTP(s, old, now); ok {
		t.Fatal("a 90-second-old code must be rejected")
	}
	if _, ok := VerifyTOTP(s, "12345", now); ok {
		t.Fatal("short code accepted")
	}
	if !strings.HasPrefix(OTPAuthURI("BackTrackarr", "admin", s), "otpauth://totp/BackTrackarr:admin?") {
		t.Fatal("otpauth uri")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes(10)
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 14 || seen[c] || NormalizeRecoveryCode(strings.ToUpper(strings.ReplaceAll(c, "-", " "))) != c {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if b, _ := l.Blocked("ip"); b {
			t.Fatal("blocked too early")
		}
		l.Fail("ip")
	}
	if b, wait := l.Blocked("ip"); !b || wait <= 0 {
		t.Fatal("should be blocked after 3 failures")
	}
	if b, _ := l.Blocked("other"); b {
		t.Fatal("other addresses must not be blocked")
	}
	l.Reset("ip")
	if b, _ := l.Blocked("ip"); b {
		t.Fatal("reset")
	}
}
