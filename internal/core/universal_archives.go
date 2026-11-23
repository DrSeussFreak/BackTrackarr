package core

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/nwaples/rardecode/v2"
)

// Exports arrive as ZIP, RAR, TAR or gzip, sometimes one inside another.
// Each is read as a stream with limits, so a hostile file can't fill the disk.

const uniMaxDepth = 3

var errUniLimit = errors.New("that archive is too large or holds too many files")

func uniIsArchive(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".zip", ".rar", ".tar", ".tgz", ".gz":
		return true
	}
	return false
}

func uniIsData(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".csv", ".tsv", ".txt", ".xml":
		return true
	}
	return false
}

// readPath works out what a saved file is from its first bytes.
func (u *uniRun) readPath(ctx context.Context, p, name string, depth int) error {
	if depth > uniMaxDepth {
		return errUniLimit
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return u.readZip(ctx, p, depth)
	case bytes.HasPrefix(head, []byte("Rar!\x1a\x07")):
		return u.readRar(ctx, f, depth)
	case len(head) > 262 && string(head[257:262]) == "ustar":
		return u.readTar(ctx, f, depth)
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("couldn't open the gzip file: %w", err)
		}
		defer gz.Close()
		inner := strings.TrimSuffix(name, ".gz")
		if strings.HasSuffix(strings.ToLower(name), ".tgz") {
			inner = strings.TrimSuffix(name, path.Ext(name)) + ".tar"
		}
		tmp, err := u.spool(gz)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		return u.readPath(ctx, tmp, inner, depth+1)
	}
	u.res.Files++
	return u.readFile(ctx, f, name)
}

// spool saves a decompressed stream to a temp file, within the size limits.
func (u *uniRun) spool(r io.Reader) (string, error) {
	tmp, err := os.CreateTemp("", "backtrackarr-archive-*")
	if err != nil {
		return "", err
	}
	limit := min(int64(uniMaxEntry), uniMaxTotal-u.total)
	n, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	tmp.Close()
	u.total += min(n, limit)
	if err == nil && n > limit {
		err = errUniLimit
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

type uniCounter struct {
	r io.Reader
	n *int64
}

func (c uniCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// entry reads one file from an archive. One bad file doesn't stop the rest.
func (u *uniRun) entry(ctx context.Context, r io.Reader, name string, size int64, depth int) error {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	if strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, ".") || (!uniIsData(base) && !uniIsArchive(base)) {
		return nil
	}
	if u.res.Files >= uniMaxFiles || size > uniMaxEntry || u.total+max(size, 0) > uniMaxTotal {
		return errUniLimit
	}
	var err error
	if uniIsArchive(base) {
		var tmp string
		if tmp, err = u.spool(r); err == nil {
			err = u.readPath(ctx, tmp, name, depth+1)
			os.Remove(tmp)
		}
	} else {
		u.res.Files++
		limit := min(int64(uniMaxEntry), uniMaxTotal-u.total)
		err = u.readFile(ctx, uniCounter{io.LimitReader(r, limit), &u.total}, name)
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errUniLimit):
		return err
	case err != nil:
		u.res.Skipped++
	}
	return nil
}

func (u *uniRun) readZip(ctx context.Context, p string, depth int) error {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return fmt.Errorf("couldn't open the ZIP: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > uniMaxEntry {
			return errUniLimit
		}
		rc, err := f.Open()
		if err != nil {
			u.res.Skipped++
			continue
		}
		err = u.entry(ctx, rc, f.Name, int64(f.UncompressedSize64), depth)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *uniRun) readRar(ctx context.Context, r io.Reader, depth int) error {
	rr, err := rardecode.NewReader(r)
	if err != nil {
		return fmt.Errorf("couldn't open the RAR: %w", err)
	}
	for {
		h, err := rr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if errors.Is(err, rardecode.ErrArchiveEncrypted) || errors.Is(err, rardecode.ErrArchivedFileEncrypted) {
				return errors.New("that RAR is password protected; extract it first")
			}
			return fmt.Errorf("couldn't read the RAR: %w", err)
		}
		if h.IsDir {
			continue
		}
		size := h.UnPackedSize
		if h.UnKnownSize {
			size = -1
		}
		if err := u.entry(ctx, rr, h.Name, size, depth); err != nil {
			return err
		}
	}
}

func (u *uniRun) readTar(ctx context.Context, r io.Reader, depth int) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("couldn't read the TAR: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := u.entry(ctx, tr, h.Name, h.Size, depth); err != nil {
			return err
		}
	}
}
