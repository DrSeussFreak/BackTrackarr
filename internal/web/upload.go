package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
)

// Large imports arrive as small pieces. That avoids the limit on files per
// form, request timeouts and a reverse proxy's maximum body size. Pieces wait
// on disk, so a big batch doesn't use memory.

const (
	uploadMaxChunk = 32 << 20
	uploadMaxFiles = 50000
	uploadMaxFile  = 8 << 30
	uploadMaxTotal = 40 << 30
	uploadMaxIdle  = 6 * time.Hour
)

var uploadBatchID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type uploadFile struct {
	name  string
	path  string
	total int64
	size  int64
}

type uploadBatch struct {
	dir     string
	files   map[int]*uploadFile
	bytes   int64
	touched time.Time
}

type uploads struct {
	mu      sync.Mutex
	batches map[string]*uploadBatch
}

func (u *uploads) sweep(now time.Time) {
	for id, b := range u.batches {
		if now.Sub(b.touched) > uploadMaxIdle {
			os.RemoveAll(b.dir)
			delete(u.batches, id)
		}
	}
}

func (u *uploads) batch(id string) (*uploadBatch, error) {
	if u.batches == nil {
		u.batches = map[string]*uploadBatch{}
	}
	now := time.Now()
	u.sweep(now)
	b := u.batches[id]
	if b == nil {
		dir, err := os.MkdirTemp("", "backtrackarr-upload-*")
		if err != nil {
			return nil, err
		}
		b = &uploadBatch{dir: dir, files: map[int]*uploadFile{}}
		u.batches[id] = b
	}
	b.touched = now
	return b, nil
}

// uploadChunk stores one piece of one file. Sending a piece again is safe.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, name := q.Get("batch"), filepath.Base(filepath.ToSlash(q.Get("name")))
	index, err1 := strconv.Atoi(q.Get("index"))
	offset, err2 := strconv.ParseInt(q.Get("offset"), 10, 64)
	total, err3 := strconv.ParseInt(q.Get("total"), 10, 64)
	if !uploadBatchID.MatchString(id) || err1 != nil || err2 != nil || err3 != nil || index < 0 || index >= uploadMaxFiles ||
		offset < 0 || total < 0 || offset > total || name == "." || name == string(filepath.Separator) {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	if total > uploadMaxFile {
		http.Error(w, name+" is larger than 8 GB", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxChunk)
	// Read before taking the lock, so one slow connection can't hold up others.
	tmp, err := os.CreateTemp("", "backtrackarr-chunk-*")
	if err != nil {
		s.fail(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, r.Body)
	if err != nil {
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	if offset+n > total {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		s.fail(w, err)
		return
	}

	s.uploads.mu.Lock()
	defer s.uploads.mu.Unlock()
	b, err := s.uploads.batch(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	f := b.files[index]
	if f == nil {
		if len(b.files) >= uploadMaxFiles {
			http.Error(w, "too many files", http.StatusRequestEntityTooLarge)
			return
		}
		f = &uploadFile{name: name, path: filepath.Join(b.dir, fmt.Sprintf("%06d", index)), total: total}
		b.files[index] = f
	}
	if f.name != name || f.total != total || offset > f.size {
		http.Error(w, "pieces arrived out of order", http.StatusConflict)
		return
	}
	if b.bytes-f.size+offset+n > uploadMaxTotal {
		http.Error(w, "this batch is larger than 40 GB", http.StatusRequestEntityTooLarge)
		return
	}
	out, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer out.Close()
	if err := out.Truncate(offset); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := out.Seek(offset, io.SeekStart); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := io.Copy(out, tmp); err != nil {
		_ = out.Truncate(offset)
		b.bytes -= f.size - offset
		f.size = offset
		http.Error(w, "couldn't save the upload; check free disk space", http.StatusInsufficientStorage)
		return
	}
	b.bytes += offset + n - f.size
	f.size = offset + n
	w.WriteHeader(http.StatusNoContent)
}

// finishUpload hands over a complete batch. The files then belong to the caller.
func (s *Server) finishUpload(id string, want int) ([]core.ImportFile, func(), error) {
	if !uploadBatchID.MatchString(id) {
		return nil, nil, errors.New("that upload wasn't recognized; try again")
	}
	s.uploads.mu.Lock()
	defer s.uploads.mu.Unlock()
	b := s.uploads.batches[id]
	if b == nil {
		return nil, nil, errors.New("that upload expired; try again")
	}
	if len(b.files) != want {
		return nil, nil, fmt.Errorf("only %d of %d files arrived; try again", len(b.files), want)
	}
	idx := make([]int, 0, len(b.files))
	for i, f := range b.files {
		if f.size != f.total {
			return nil, nil, fmt.Errorf("%s didn't finish uploading; try again", f.name)
		}
		if f.total == 0 { // no piece wrote it if the browser sent none
			if err := os.WriteFile(f.path, nil, 0o600); err != nil {
				return nil, nil, err
			}
		}
		idx = append(idx, i)
	}
	sort.Ints(idx)
	files := make([]core.ImportFile, 0, len(idx))
	for _, i := range idx {
		files = append(files, core.ImportFile{Path: b.files[i].path, Name: b.files[i].name})
	}
	delete(s.uploads.batches, id)
	dir := b.dir
	return files, func() { os.RemoveAll(dir) }, nil
}

// formFiles returns the files sent with a form, from pieces uploaded earlier
// or, without JavaScript, from the form itself. Call done when finished.
func (s *Server) formFiles(r *http.Request) (files []core.ImportFile, done func(), err error) {
	if id := r.FormValue("upload_batch"); id != "" {
		want, err := strconv.Atoi(r.FormValue("upload_count"))
		if err != nil || want <= 0 {
			return nil, nil, errors.New("that upload wasn't recognized; try again")
		}
		return s.finishUpload(id, want)
	}
	if r.MultipartForm == nil {
		return nil, nil, errors.New("choose a file to import")
	}
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		return nil, nil, errors.New("choose a file to import")
	}
	drop := func() {
		for _, f := range files {
			os.Remove(f.Path)
		}
	}
	for _, hdr := range headers {
		p, err := spoolUpload(hdr)
		if err != nil {
			drop()
			return nil, nil, err
		}
		files = append(files, core.ImportFile{Path: p, Name: hdr.Filename})
	}
	return files, drop, nil
}

func uploadLabel(files []core.ImportFile) string {
	if len(files) == 1 {
		return files[0].Name
	}
	return fmt.Sprintf("%s and %d more", files[0].Name, len(files)-1)
}

func spoolUpload(hdr *multipart.FileHeader) (string, error) {
	f, err := hdr.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	spool, err := os.CreateTemp("", "backtrackarr-import-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(spool, f)
	spool.Close()
	if err != nil {
		os.Remove(spool.Name())
		return "", err
	}
	return spool.Name(), nil
}

func isJSONFile(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".json")
}

// importJSON sends a JSON file to the importer that works out its format.
func (s *Server) importJSON(ctx context.Context, f *os.File, name string, report func(string)) error {
	res, err := s.eng.ImportFiles(ctx, []core.ImportFile{{Path: f.Name(), Name: name}}, "", report)
	if res != nil {
		report(res.String())
	}
	return err
}

// eachUpload runs fn on every file, so one bad file doesn't stop the batch.
func eachUpload(files []core.ImportFile, report func(string), fn func(f *os.File, name string) (string, error)) error {
	var failed []string
	last := ""
	for i, u := range files {
		if len(files) > 1 {
			report(fmt.Sprintf("file %d of %d: %s", i+1, len(files), u.Name))
		}
		f, err := os.Open(u.Path)
		if err == nil {
			last, err = fn(f, u.Name)
			f.Close()
		}
		if err != nil {
			failed = append(failed, u.Name+": "+err.Error())
		}
	}
	if len(files) > 1 {
		report(fmt.Sprintf("%d of %d files imported", len(files)-len(failed), len(files)))
	} else if last != "" {
		report(last)
	}
	if len(failed) == 0 {
		return nil
	}
	shown := failed
	more := ""
	if len(shown) > 5 {
		more = fmt.Sprintf(" and %d more", len(shown)-5)
		shown = shown[:5]
	}
	msg := ""
	for i, f := range shown {
		if i > 0 {
			msg += "; "
		}
		msg += f
	}
	return errors.New(msg + more)
}
