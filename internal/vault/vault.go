// Package vault holds the data key that encrypts the database and backups.
//
// One random data key does the encrypting. It is stored only in wrapped form:
// once under a key file kept outside the data volume, and once under a
// recovery key the owner saves. AES-256-GCM protects each wrap, so a wrong key
// or a changed vault is detected rather than producing garbage.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DataKeyLen is two AES-256 keys, as AES-XTS uses.
	DataKeyLen = 64
	// FileName is the vault's name inside the data folder.
	FileName = "vault.json"

	minKeyFileBytes = 32
	recoveryIter    = 600_000
	keyFileInfo     = "backtrackarr key file v1"
)

// ErrWrongKey means a key didn't open the vault.
var ErrWrongKey = errors.New("that key doesn't open this vault")

// Wrap is the data key encrypted under one other key.
type Wrap struct {
	Salt  []byte `json:"salt"`
	Iter  int    `json:"iter,omitempty"`
	Nonce []byte `json:"nonce"`
	Data  []byte `json:"data"`
}

// Vault is what's saved on disk. It holds no usable key by itself.
type Vault struct {
	Version  int  `json:"version"`
	KeyFile  Wrap `json:"key_file"`
	Recovery Wrap `json:"recovery"`
}

// ReadKeyFile loads a key file: at least 32 random bytes written as hex.
func ReadKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("can't read the encryption key file: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < minKeyFileBytes {
		return nil, errors.New("the encryption key file must hold at least 32 random bytes as hex; create one with: openssl rand -hex 32")
	}
	return key, nil
}

// CheckKeyFilePath refuses a key file stored on the data volume, where it
// would be stolen or backed up along with what it protects.
func CheckKeyFilePath(keyFile, dataDir string) error {
	k, err := resolvePath(keyFile)
	if err != nil {
		return err
	}
	d, err := resolvePath(dataDir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(d, k); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("the encryption key file must not be inside the data folder")
	}
	return nil
}

// resolvePath follows symlinks through the part of the path that exists, so a
// file that hasn't been created yet still compares correctly.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest), nil
		}
		if dir == filepath.Dir(dir) {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

func seal(kek, plain, aad []byte) (nonce, ct []byte, err error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plain, aad), nil
}

func unseal(kek []byte, w Wrap, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(w.Nonce) != gcm.NonceSize() {
		return nil, ErrWrongKey
	}
	plain, err := gcm.Open(nil, w.Nonce, w.Data, aad)
	if err != nil || len(plain) != DataKeyLen {
		return nil, ErrWrongKey
	}
	return plain, nil
}

func keyFileKEK(key, salt []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, key, salt, keyFileInfo, 32)
}

func recoveryKEK(recovery string, salt []byte, iter int) ([]byte, error) {
	if iter < recoveryIter {
		return nil, ErrWrongKey // a changed vault must not weaken this
	}
	return pbkdf2.Key(sha256.New, normalizeRecovery(recovery), salt, iter, 32)
}

func normalizeRecovery(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
}

func newRecovery() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	var parts []string
	for len(s) > 0 {
		n := min(4, len(s))
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return strings.Join(parts, "-"), nil
}

func wrapKeyFile(data, key []byte) (Wrap, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Wrap{}, err
	}
	kek, err := keyFileKEK(key, salt)
	if err != nil {
		return Wrap{}, err
	}
	nonce, ct, err := seal(kek, data, []byte("key_file"))
	return Wrap{Salt: salt, Nonce: nonce, Data: ct}, err
}

// Create makes a new data key and returns it with the vault and the one-time
// recovery key.
func Create(keyFile []byte) (*Vault, []byte, string, error) {
	data := make([]byte, DataKeyLen)
	if _, err := rand.Read(data); err != nil {
		return nil, nil, "", err
	}
	kf, err := wrapKeyFile(data, keyFile)
	if err != nil {
		return nil, nil, "", err
	}
	recovery, err := newRecovery()
	if err != nil {
		return nil, nil, "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, "", err
	}
	kek, err := recoveryKEK(recovery, salt, recoveryIter)
	if err != nil {
		return nil, nil, "", err
	}
	nonce, ct, err := seal(kek, data, []byte("recovery"))
	if err != nil {
		return nil, nil, "", err
	}
	return &Vault{Version: 1, KeyFile: kf, Recovery: Wrap{Salt: salt, Iter: recoveryIter, Nonce: nonce, Data: ct}}, data, recovery, nil
}

// OpenWithKeyFile returns the data key.
func (v *Vault) OpenWithKeyFile(key []byte) ([]byte, error) {
	kek, err := keyFileKEK(key, v.KeyFile.Salt)
	if err != nil {
		return nil, err
	}
	return unseal(kek, v.KeyFile, []byte("key_file"))
}

// OpenWithRecovery returns the data key.
func (v *Vault) OpenWithRecovery(recovery string) ([]byte, error) {
	kek, err := recoveryKEK(recovery, v.Recovery.Salt, v.Recovery.Iter)
	if err != nil {
		return nil, err
	}
	return unseal(kek, v.Recovery, []byte("recovery"))
}

// Rewrap points the vault at a new key file.
func (v *Vault) Rewrap(data, keyFile []byte) error {
	w, err := wrapKeyFile(data, keyFile)
	if err != nil {
		return err
	}
	v.KeyFile = w
	return nil
}

// Load reads a vault. It returns nil, nil if there is none.
func Load(dataDir string) (*Vault, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v Vault
	if err := json.Unmarshal(b, &v); err != nil || v.Version != 1 {
		return nil, errors.New("the encryption vault file is damaged")
	}
	return &v, nil
}

// Save writes the vault atomically and flushes it to disk.
func (v *Vault) Save(dataDir string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, ".vault-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dataDir, FileName))
}
