package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

func writeKeyFile(t *testing.T, fill byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "backtrackarr.key")
	if err := os.WriteFile(p, []byte(hex.EncodeToString(bytes.Repeat([]byte{fill}, 32))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openEncrypted(t *testing.T, dir, key string) *Store {
	t.Helper()
	enc, err := PrepareEncryption(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenWith(dir, enc)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func addPlay(t *testing.T, st *Store, title string) {
	t.Helper()
	ctx := context.Background()
	m := model.Media{Kind: model.Movie, Title: title, IDs: model.IDs{IMDB: "tt" + title}}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordPlay(ctx, m.ID, "me", time.Unix(1700000000, 0), "test", "", false); err != nil {
		t.Fatal(err)
	}
}

func playCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM plays`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEncryptExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	key := writeKeyFile(t, 1)
	plain, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	addPlay(t, plain, "SecretTitle")
	plain.Close()

	st := openEncrypted(t, dir, key)
	if playCount(t, st) != 1 {
		t.Fatal("play lost")
	}
	rec, err := st.PendingRecoveryKey(context.Background())
	if err != nil || rec == "" {
		t.Fatalf("recovery key: %q %v", rec, err)
	}
	st.Close()

	raw, err := os.ReadFile(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte(sqliteMagic)) || bytes.Contains(raw, []byte("SecretTitle")) {
		t.Fatal("database is readable on disk")
	}
	for _, f := range []string{dbFileName + "-wal", dbFileName + encryptingSuffix} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil && bytes.Contains(b, []byte("SecretTitle")) {
			t.Fatalf("%s is readable", f)
		}
	}

	// Reopening must not create a second vault or lose data.
	st = openEncrypted(t, dir, key)
	defer st.Close()
	if playCount(t, st) != 1 {
		t.Fatal("play lost after reopen")
	}
	if got, _ := st.PendingRecoveryKey(context.Background()); got != rec {
		t.Fatal("recovery key changed")
	}
}

func TestEncryptedDatabaseRefusesBadKeys(t *testing.T) {
	dir := t.TempDir()
	st := openEncrypted(t, dir, writeKeyFile(t, 1))
	addPlay(t, st, "A")
	st.Close()
	if _, err := PrepareEncryption(dir, ""); err == nil || !strings.Contains(err.Error(), "ENCRYPTION_KEY_FILE") {
		t.Fatalf("no key: %v", err)
	}
	if _, err := PrepareEncryption(dir, writeKeyFile(t, 2)); err == nil {
		t.Fatal("wrong key accepted")
	}
	inside := filepath.Join(dir, "key")
	b, _ := os.ReadFile(writeKeyFile(t, 1))
	os.WriteFile(inside, b, 0o600)
	if _, err := PrepareEncryption(dir, inside); err == nil {
		t.Fatal("a key file on the data volume was accepted")
	}
}

// If the app dies mid-way, the original database must come back untouched.
func TestInterruptedEncryptionRecovers(t *testing.T) {
	dir := t.TempDir()
	key := writeKeyFile(t, 1)
	plain, _ := Open(dir)
	addPlay(t, plain, "Kept")
	plain.Close()
	path := filepath.Join(dir, dbFileName)
	if err := os.Rename(path, path+unencryptedSuffix); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path+encryptingSuffix, []byte("half written"), 0o600)
	st := openEncrypted(t, dir, key)
	defer st.Close()
	if playCount(t, st) != 1 {
		t.Fatal("data lost after an interrupted run")
	}
}

func TestEncryptedBackupRestoresOnNewServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := writeKeyFile(t, 1)
	plain, _ := Open(dir)
	addPlay(t, plain, "SecretTitle")
	plain.Close()
	st := openEncrypted(t, dir, key)
	rec, _ := st.PendingRecoveryKey(ctx)
	name, err := st.Backup(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, ".btbak") {
		t.Fatalf("backup name %s", name)
	}
	b, _ := os.ReadFile(name)
	if bytes.Contains(b, []byte("SecretTitle")) || bytes.Contains(b, []byte(sqliteMagic)) {
		t.Fatal("backup is readable")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backups", "*.partial*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	// New server, new key file: only the recovery key can open it.
	next := t.TempDir()
	newKey := writeKeyFile(t, 9)
	if err := RestoreBackup(ctx, next, name, newKey, ""); err == nil {
		t.Fatal("restored without a usable key")
	}
	if err := RestoreBackup(ctx, next, name, newKey, rec); err != nil {
		t.Fatal(err)
	}
	got := openEncrypted(t, next, newKey)
	if playCount(t, got) != 1 {
		t.Fatal("play missing after restore")
	}
	got.Close()
	if err := RestoreBackup(ctx, next, name, newKey, rec); err == nil {
		t.Fatal("restore overwrote an existing database")
	}

	if st.UnencryptedCopies() == nil {
		t.Fatal("the pre-encryption copy should be listed")
	}
	if n, err := st.RemoveUnencrypted(ctx); err != nil || n != 1 {
		t.Fatalf("removed %d: %v", n, err)
	}
	if playCount(t, st) != 1 {
		t.Fatal("live data changed")
	}
	st.Close()
}

func TestRemoveUnencryptedNeedsBackup(t *testing.T) {
	dir := t.TempDir()
	plain, _ := Open(dir)
	addPlay(t, plain, "A")
	plain.Close()
	st := openEncrypted(t, dir, writeKeyFile(t, 1))
	defer st.Close()
	if _, err := st.RemoveUnencrypted(context.Background()); err == nil {
		t.Fatal("removed plain copies with no encrypted backup")
	}
	if _, err := os.Stat(filepath.Join(dir, dbFileName+unencryptedSuffix)); err != nil {
		t.Fatal("the original copy is gone")
	}
}
