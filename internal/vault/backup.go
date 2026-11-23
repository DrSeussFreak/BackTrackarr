package vault

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// A backup file carries its own wrapped data key, so it can be restored on a
// new server with only the key file or the recovery key. Its contents use
// AES-256-GCM in numbered chunks: changing, reordering or cutting off any part
// makes the restore fail instead of producing a damaged database.

const (
	backupMagic = "BTBK1\n"
	chunkSize   = 1 << 20
	backupInfo  = "backtrackarr backup v1"
	maxHeader   = 64 << 10
)

type backupHeader struct {
	Vault  Vault  `json:"vault"`
	Salt   []byte `json:"salt"`
	Prefix []byte `json:"prefix"`
}

func chunkAEAD(data, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, data, salt, backupInfo, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(prefix []byte, n uint32, final bool) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix[:7])
	binary.BigEndian.PutUint32(nonce[7:], n)
	if final {
		nonce[11] = 1
	}
	return nonce
}

// SealFile writes src to dst as an encrypted backup.
func SealFile(src, dst string, v *Vault, data []byte) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	h := backupHeader{Vault: *v, Salt: make([]byte, 32), Prefix: make([]byte, 7)}
	if _, err := rand.Read(h.Salt); err != nil {
		return err
	}
	if _, err := rand.Read(h.Prefix); err != nil {
		return err
	}
	head, err := json.Marshal(h)
	if err != nil {
		return err
	}
	aead, err := chunkAEAD(data, h.Salt)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(out, 256<<10)
	if _, err := w.WriteString(backupMagic); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(head))); err != nil {
		return err
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	r := bufio.NewReaderSize(in, 64<<10)
	plain := make([]byte, chunkSize)
	ct := make([]byte, 0, chunkSize+aead.Overhead())
	for n := uint32(0); ; n++ {
		if n == 0xFFFFFFFF {
			return errors.New("backup is too large")
		}
		got, err := io.ReadFull(r, plain)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		_, peek := r.Peek(1)
		final := err != nil || peek == io.EOF
		if peek != nil && peek != io.EOF {
			return peek
		}
		ct = aead.Seal(ct[:0], chunkNonce(h.Prefix, n, final), plain[:got], head)
		if _, err := w.Write(ct); err != nil {
			return err
		}
		if final {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Sync()
}

// ReadBackupVault returns the vault stored in a backup file.
func ReadBackupVault(path string) (*Vault, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h, _, err := readHeader(bufio.NewReader(f))
	if err != nil {
		return nil, err
	}
	return &h.Vault, nil
}

func readHeader(r io.Reader) (*backupHeader, []byte, error) {
	magic := make([]byte, len(backupMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != backupMagic {
		return nil, nil, errors.New("this isn't a BackTrackarr encrypted backup")
	}
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil || size == 0 || size > maxHeader {
		return nil, nil, errors.New("this backup's header is damaged")
	}
	head := make([]byte, size)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, nil, errors.New("this backup's header is damaged")
	}
	var h backupHeader
	if err := json.Unmarshal(head, &h); err != nil || len(h.Prefix) != 7 || len(h.Salt) != 32 {
		return nil, nil, errors.New("this backup's header is damaged")
	}
	return &h, head, nil
}

// OpenFile decrypts a backup to dst, which holds the still page-encrypted
// database. dst is removed if any part fails its check.
func OpenFile(src, dst string, data []byte) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r := bufio.NewReaderSize(in, 256<<10)
	h, head, err := readHeader(r)
	if err != nil {
		return err
	}
	aead, err := chunkAEAD(data, h.Salt)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	w := bufio.NewWriterSize(out, 256<<10)
	ct := make([]byte, chunkSize+aead.Overhead())
	plain := make([]byte, 0, chunkSize)
	for n := uint32(0); ; n++ {
		got, err := io.ReadFull(r, ct)
		if err != nil && err != io.ErrUnexpectedEOF {
			return errors.New("this backup is incomplete or damaged")
		}
		_, peek := r.Peek(1)
		final := err != nil || peek == io.EOF
		plain, err = aead.Open(plain[:0], chunkNonce(h.Prefix, n, final), ct[:got], head)
		if err != nil {
			return fmt.Errorf("this backup is damaged or the key is wrong: %w", ErrWrongKey)
		}
		if _, err := w.Write(plain); err != nil {
			return err
		}
		if final {
			break
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Sync()
}
