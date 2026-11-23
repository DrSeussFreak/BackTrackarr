package vault

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVaultKeys(t *testing.T) {
	dir := t.TempDir()
	kf := testKey(t)
	v, data, recovery, err := Create(kf)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := got.OpenWithKeyFile(kf); err != nil || !bytes.Equal(k, data) {
		t.Fatalf("key file: %v", err)
	}
	if k, err := got.OpenWithRecovery(" " + recovery + " "); err != nil || !bytes.Equal(k, data) {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := got.OpenWithKeyFile(testKey(t)); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key file: %v", err)
	}
	if _, err := got.OpenWithRecovery("AAAA-BBBB"); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong recovery key: %v", err)
	}
	next := testKey(t)
	if err := got.Rewrap(data, next); err != nil {
		t.Fatal(err)
	}
	if _, err := got.OpenWithKeyFile(kf); !errors.Is(err, ErrWrongKey) {
		t.Fatal("old key file still works after rewrap")
	}
	if k, err := got.OpenWithKeyFile(next); err != nil || !bytes.Equal(k, data) {
		t.Fatalf("new key file: %v", err)
	}
}

func TestKeyFileMustBeOutsideData(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckKeyFilePath(filepath.Join(data, "key"), data); err == nil {
		t.Fatal("a key inside the data folder was allowed")
	}
	if err := CheckKeyFilePath(filepath.Join(dir, "secrets", "key"), data); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRoundTripAndTamper(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		dir := t.TempDir()
		plain := make([]byte, size)
		rand.Read(plain)
		src, sealed, out := filepath.Join(dir, "src"), filepath.Join(dir, "b.btbak"), filepath.Join(dir, "out")
		if err := os.WriteFile(src, plain, 0o600); err != nil {
			t.Fatal(err)
		}
		kf := testKey(t)
		v, data, recovery, err := Create(kf)
		if err != nil {
			t.Fatal(err)
		}
		if err := SealFile(src, sealed, v, data); err != nil {
			t.Fatal(err)
		}
		bv, err := ReadBackupVault(sealed)
		if err != nil {
			t.Fatal(err)
		}
		k, err := bv.OpenWithRecovery(recovery)
		if err != nil {
			t.Fatal(err)
		}
		if err := OpenFile(sealed, out, k); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got, _ := os.ReadFile(out); !bytes.Equal(got, plain) {
			t.Fatalf("size %d: contents changed", size)
		}
		os.Remove(out)
		b, _ := os.ReadFile(sealed)

		flipped := append([]byte(nil), b...)
		flipped[len(flipped)-1] ^= 1
		bad := filepath.Join(dir, "flipped")
		os.WriteFile(bad, flipped, 0o600)
		if err := OpenFile(bad, out, k); err == nil {
			t.Fatalf("size %d: a changed byte was accepted", size)
		}
		if _, err := os.Stat(out); err == nil {
			t.Fatal("a failed restore left a file behind")
		}
		if size > chunkSize {
			cut := filepath.Join(dir, "cut")
			os.WriteFile(cut, b[:len(b)-(chunkSize+16)], 0o600)
			if size%chunkSize != 0 {
				os.WriteFile(cut, b[:len(b)-(size%chunkSize+16)], 0o600)
			}
			if err := OpenFile(cut, out, k); err == nil {
				t.Fatalf("size %d: a truncated backup was accepted", size)
			}
		}
		if err := OpenFile(sealed, out, testKey(t)); err == nil {
			t.Fatal("a wrong key was accepted")
		}
	}
}
