package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const (
	passwordPrefix = "$pbkdf2-sha256$"
	pbkdfRounds    = 600_000
)

// HashPassword returns a salted hash for storing in the database.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdfRounds, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return passwordPrefix + strconv.Itoa(pbkdfRounds) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
}

// IsPasswordHash reports whether a stored value is a hash, not plain text.
func IsPasswordHash(v string) bool { return strings.HasPrefix(v, passwordPrefix) }

// The last good check is remembered as a keyed digest, so scripts using basic
// auth don't spend a slow hash on every request.
var (
	checkMu   sync.Mutex
	checkSalt = func() []byte { b := make([]byte, 32); rand.Read(b); return b }()
	lastGood  []byte
)

func checkDigest(stored, password string) []byte {
	h := sha256.New()
	h.Write(checkSalt)
	h.Write([]byte(stored))
	h.Write([]byte{0})
	h.Write([]byte(password))
	return h.Sum(nil)
}

// CheckPassword compares against a stored hash, or against a plain value set
// through the ADMIN_PASSWORD environment variable.
func CheckPassword(stored, password string) bool {
	if stored == "" {
		return false
	}
	if !IsPasswordHash(stored) {
		a, b := sha256.Sum256([]byte(stored)), sha256.Sum256([]byte(password))
		return subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	d := checkDigest(stored, password)
	checkMu.Lock()
	hit := lastGood != nil && subtle.ConstantTimeCompare(lastGood, d) == 1
	checkMu.Unlock()
	if hit {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(stored, passwordPrefix), "$")
	if len(parts) != 3 {
		return false
	}
	rounds, err := strconv.Atoi(parts[0])
	if err != nil || rounds < 100_000 || rounds > 10_000_000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[1])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return false
	}
	checkMu.Lock()
	lastGood = d
	checkMu.Unlock()
	return true
}

type sealedFile struct {
	Format string `json:"format"`
	Rounds int    `json:"rounds"`
	Salt   []byte `json:"salt"`
	Nonce  []byte `json:"nonce"`
	Data   []byte `json:"data"`
}

const sealFormat = "backtrackarr-settings-1"

// ErrBadPassphrase means the passphrase is wrong or the file was altered.
var ErrBadPassphrase = errors.New("wrong passphrase, or the file is damaged")

// Seal encrypts data with a passphrase (AES-256-GCM, PBKDF2 key).
func Seal(data []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := sealCipher(passphrase, salt, pbkdfRounds)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	f := sealedFile{Format: sealFormat, Rounds: pbkdfRounds, Salt: salt, Nonce: nonce}
	f.Data = gcm.Seal(nil, nonce, data, []byte(sealFormat))
	return json.Marshal(f)
}

// Unseal decrypts a file made by Seal.
func Unseal(file []byte, passphrase string) ([]byte, error) {
	var f sealedFile
	if err := json.Unmarshal(file, &f); err != nil || f.Format != sealFormat {
		return nil, fmt.Errorf("this isn't a BackTrackarr settings backup")
	}
	if f.Rounds < 100_000 || f.Rounds > 10_000_000 {
		return nil, ErrBadPassphrase
	}
	gcm, err := sealCipher(passphrase, f.Salt, f.Rounds)
	if err != nil {
		return nil, err
	}
	if len(f.Nonce) != gcm.NonceSize() {
		return nil, ErrBadPassphrase
	}
	out, err := gcm.Open(nil, f.Nonce, f.Data, []byte(sealFormat))
	if err != nil {
		return nil, ErrBadPassphrase
	}
	return out, nil
}

func sealCipher(passphrase string, salt []byte, rounds int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, rounds, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
