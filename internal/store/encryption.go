package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/DrSeussFreak/BackTrackarr/internal/vault"
)

// Encryption at rest: every database page, WAL frame and SQLite temp file is
// AES-256-XTS encrypted with the vault's data key. XTS can't detect tampering
// on the live file, so backups add AES-256-GCM on top.

const (
	recoveryPendingKey = "vault|recovery_pending"
	encryptingSuffix   = ".encrypting"
	unencryptedSuffix  = ".unencrypted"
	dbFileName         = "backtrackarr.db"
	sqliteMagic        = "SQLite format 3\x00"
)

// Encryption is an unlocked vault.
type Encryption struct {
	Vault *vault.Vault
	Key   []byte
}

// sqliteDSN builds a URI, escaped so unusual paths work. It holds the key, so
// it must never be logged.
func sqliteDSN(path string, key []byte, params string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: abs}
	dsn := u.String() + "?" + params
	if key != nil {
		dsn += "&vfs=xts&hexkey=" + hex.EncodeToString(key)
	}
	return dsn, nil
}

func dbState(path string) (exists, plain bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	n, _ := f.Read(head)
	if n == 0 {
		return false, false, nil // an empty file holds nothing to protect
	}
	return true, string(head[:n]) == sqliteMagic, nil
}

func removeDBFiles(path string) {
	for _, s := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + s)
	}
}

// PrepareEncryption unlocks the database, encrypting it first if needed. With
// no key file and a plain database it returns nil. An encrypted database never
// silently falls back to running without its key.
func PrepareEncryption(dir, keyFile string) (*Encryption, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, dbFileName)
	migrateOldName(dir, path)
	// A crash between the two renames below leaves only the original.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(path + unencryptedSuffix); err == nil {
			if err := os.Rename(path+unencryptedSuffix, path); err != nil {
				return nil, err
			}
		}
	}
	exists, plain, err := dbState(path)
	if err != nil {
		return nil, err
	}
	if keyFile == "" {
		if exists && !plain {
			return nil, errors.New("this database is encrypted: set ENCRYPTION_KEY_FILE to its key file")
		}
		return nil, nil
	}
	if err := vault.CheckKeyFilePath(keyFile, dir); err != nil {
		return nil, err
	}
	kf, err := vault.ReadKeyFile(keyFile)
	if err != nil {
		return nil, err
	}
	if !exists || plain {
		// Any vault here belongs to an encryption run that never finished.
		return encryptDatabase(path, dir, exists, kf)
	}
	v, err := vault.Load(dir)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errors.New("this database is encrypted but vault.json is missing; restore it from a backup")
	}
	data, err := v.OpenWithKeyFile(kf)
	if err != nil {
		return nil, errors.New("the encryption key file doesn't match this database")
	}
	return &Encryption{Vault: v, Key: data}, nil
}

func encryptDatabase(path, dir string, hasData bool, kf []byte) (*Encryption, error) {
	v, data, recovery, err := vault.Create(kf)
	if err != nil {
		return nil, err
	}
	tmp := path + encryptingSuffix
	removeDBFiles(tmp)
	fail := func(err error) (*Encryption, error) {
		removeDBFiles(tmp)
		return nil, fmt.Errorf("encrypting the database (your data is unchanged): %w", err)
	}
	if hasData {
		if err := copyEncrypted(path, tmp, data); err != nil {
			return fail(err)
		}
	}
	dst, err := openDB(tmp, data)
	if err != nil {
		return fail(err)
	}
	// Saved inside the encrypted file before it goes live, so the one-time
	// key can't be lost to a crash.
	_, err = dst.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, recoveryPendingKey, recovery)
	if err == nil {
		_, err = dst.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	if err := checkSnapshotKey(context.Background(), tmp, data); err != nil {
		return fail(err)
	}
	if err := v.Save(dir); err != nil {
		return fail(err)
	}
	if hasData {
		if err := os.Rename(path, path+unencryptedSuffix); err != nil {
			return fail(err)
		}
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
	}
	if err := os.Rename(tmp, path); err != nil {
		if hasData {
			_ = os.Rename(path+unencryptedSuffix, path)
		}
		return fail(err)
	}
	os.Remove(tmp + "-wal")
	os.Remove(tmp + "-shm")
	return &Encryption{Vault: v, Key: data}, nil
}

// copyEncrypted writes an encrypted copy and proves every table came across.
func copyEncrypted(path, tmp string, key []byte) error {
	src, err := openDB(path, nil)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	uri, err := sqliteDSN(tmp, key, "")
	if err != nil {
		return err
	}
	if _, err := src.Exec(`VACUUM INTO ?`, uri); err != nil {
		return errors.New("couldn't write the encrypted copy; check free disk space")
	}
	dsn, err := sqliteDSN(tmp, key, "mode=ro")
	if err != nil {
		return err
	}
	dst, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer dst.Close()
	return sameTables(src, dst)
}

func sameTables(a, b *sql.DB) error {
	rows, err := a.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range names {
		q := `SELECT COUNT(*) FROM "` + n + `"`
		var x, y int64
		if err := a.QueryRow(q).Scan(&x); err != nil {
			return err
		}
		if err := b.QueryRow(q).Scan(&y); err != nil {
			return fmt.Errorf("table %s is missing from the encrypted copy", n)
		}
		if x != y {
			return fmt.Errorf("table %s has %d rows in the encrypted copy, expected %d", n, y, x)
		}
	}
	return nil
}

// Encrypted reports whether the database is encrypted.
func (s *Store) Encrypted() bool { return s.enc != nil }

// PendingRecoveryKey is the one-time key until the owner confirms saving it.
func (s *Store) PendingRecoveryKey(ctx context.Context) (string, error) {
	return s.GetKV(ctx, recoveryPendingKey)
}

// ClearRecoveryKey removes the only readable copy of the recovery key.
func (s *Store) ClearRecoveryKey(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM kv WHERE k=?`, recoveryPendingKey)
	return err
}

// UnencryptedCopies lists plain files left from before encryption.
func (s *Store) UnencryptedCopies() []string {
	var out []string
	p := filepath.Join(s.dir, dbFileName+unencryptedSuffix)
	if _, err := os.Stat(p); err == nil {
		out = append(out, p)
	}
	for _, g := range []string{"backtrackarr-*.db", "backtrackarr-*.db.gz"} {
		m, _ := filepath.Glob(filepath.Join(s.dir, "backups", g))
		out = append(out, m...)
	}
	return out
}

// EncryptedBackups lists encrypted backups, oldest first.
func (s *Store) EncryptedBackups() []string {
	m, _ := filepath.Glob(filepath.Join(s.dir, "backups", "backtrackarr-*.btbak"))
	return m
}

// RemoveUnencrypted deletes the plain copies. It needs an encrypted backup
// that passes its check, so this can never remove the only good copy.
func (s *Store) RemoveUnencrypted(ctx context.Context) (int, error) {
	if s.enc == nil {
		return 0, errors.New("encryption is off")
	}
	backups := s.EncryptedBackups()
	if len(backups) == 0 {
		return 0, errors.New("make an encrypted backup first")
	}
	if err := s.verifyBackup(ctx, backups[len(backups)-1]); err != nil {
		return 0, fmt.Errorf("your newest encrypted backup failed its check: %w", err)
	}
	n := 0
	for _, p := range s.UnencryptedCopies() {
		if err := os.Remove(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) verifyBackup(ctx context.Context, name string) error {
	tmp, err := os.CreateTemp("", "backtrackarr-verify-*")
	if err != nil {
		return err
	}
	p := tmp.Name()
	tmp.Close()
	os.Remove(p)
	defer removeDBFiles(p)
	if err := vault.OpenFile(name, p, s.enc.Key); err != nil {
		return err
	}
	return checkSnapshotKey(ctx, p, s.enc.Key)
}

// RestoreBackup puts an encrypted backup in place on a server with no
// database. The key file opens it, or the recovery key if the key file is new.
func RestoreBackup(ctx context.Context, dir, file, keyFile, recovery string) error {
	if keyFile == "" {
		return errors.New("set ENCRYPTION_KEY_FILE; a new key file is fine if you have the recovery key")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, dbFileName)
	if exists, _, err := dbState(path); err != nil {
		return err
	} else if exists {
		return errors.New("a database already exists here, so nothing was changed; restore into an empty data folder")
	}
	if err := vault.CheckKeyFilePath(keyFile, dir); err != nil {
		return err
	}
	kf, err := vault.ReadKeyFile(keyFile)
	if err != nil {
		return err
	}
	v, err := vault.ReadBackupVault(file)
	if err != nil {
		return err
	}
	data, err := v.OpenWithKeyFile(kf)
	if err != nil {
		if recovery == "" {
			return errors.New("the key file doesn't open this backup; also set RECOVERY_KEY")
		}
		if data, err = v.OpenWithRecovery(recovery); err != nil {
			return errors.New("neither the key file nor the recovery key opens this backup")
		}
		if err := v.Rewrap(data, kf); err != nil {
			return err
		}
	}
	tmp := path + ".restoring"
	removeDBFiles(tmp)
	if err := vault.OpenFile(file, tmp, data); err != nil {
		return err
	}
	if err := checkSnapshotKey(ctx, tmp, data); err != nil {
		removeDBFiles(tmp)
		return fmt.Errorf("the restored database failed its check: %w", err)
	}
	if err := v.Save(dir); err != nil {
		removeDBFiles(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		removeDBFiles(tmp)
		return err
	}
	return nil
}
