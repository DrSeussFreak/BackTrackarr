package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

func sendChunk(t *testing.T, s *Server, batch string, index int, name string, offset, total int, body []byte) int {
	t.Helper()
	q := url.Values{"batch": {batch}, "index": {strconv.Itoa(index)}, "name": {name},
		"offset": {strconv.Itoa(offset)}, "total": {strconv.Itoa(total)}}
	r := httptest.NewRequest(http.MethodPost, "/upload/chunk?"+q.Encode(), bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.uploadChunk(w, r)
	return w.Code
}

func TestChunkedUploadManyFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{}
	batch := strings.Repeat("a", 32)
	const count = 1500 // more than one multipart form may hold
	for i := 0; i < count; i++ {
		body := []byte("file " + strconv.Itoa(i))
		if code := sendChunk(t, s, batch, i, "f"+strconv.Itoa(i)+".csv", 0, len(body), body); code != http.StatusNoContent {
			t.Fatalf("file %d: HTTP %d", i, code)
		}
	}
	files, done, err := s.finishUpload(batch, count)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(files) != count {
		t.Fatalf("%d files", len(files))
	}
	got, err := os.ReadFile(files[1234].Path)
	if err != nil || string(got) != "file 1234" || files[1234].Name != "f1234.csv" {
		t.Fatalf("%q %s %v", got, files[1234].Name, err)
	}
	if _, _, err := s.finishUpload(batch, count); err == nil {
		t.Fatal("a batch was handed over twice")
	}
}

func TestChunkedUploadPiecesAndRetry(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{}
	batch := strings.Repeat("b", 32)
	if c := sendChunk(t, s, batch, 0, "big.xml", 0, 10, []byte("01234")); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if c := sendChunk(t, s, batch, 0, "big.xml", 7, 10, []byte("789")); c != http.StatusConflict {
		t.Fatalf("a gap was accepted: %d", c)
	}
	if _, _, err := s.finishUpload(batch, 1); err == nil {
		t.Fatal("a partial file was accepted")
	}
	// The same piece sent again must not duplicate data.
	for range 2 {
		if c := sendChunk(t, s, batch, 0, "big.xml", 5, 10, []byte("56789")); c != http.StatusNoContent {
			t.Fatal(c)
		}
	}
	files, done, err := s.finishUpload(batch, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if got, _ := os.ReadFile(files[0].Path); string(got) != "0123456789" {
		t.Fatalf("%q", got)
	}
}

func TestChunkedUploadRejectsBadInput(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{}
	good := strings.Repeat("c", 32)
	for _, c := range []struct {
		batch  string
		index  int
		offset int
		total  int
	}{{"../../etc", 0, 0, 1}, {good, -1, 0, 1}, {good, uploadMaxFiles, 0, 1}, {good, 0, 5, 1}} {
		if code := sendChunk(t, s, c.batch, c.index, "x.csv", c.offset, c.total, []byte("x")); code != http.StatusBadRequest {
			t.Fatalf("%+v: HTTP %d", c, code)
		}
	}
	// A name with a path can only ever be stored by its base name.
	if code := sendChunk(t, s, good, 0, "../../evil.csv", 0, 1, []byte("x")); code != http.StatusNoContent {
		t.Fatal(code)
	}
	files, done, err := s.finishUpload(good, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if files[0].Name != "evil.csv" {
		t.Fatalf("name %q", files[0].Name)
	}
}
