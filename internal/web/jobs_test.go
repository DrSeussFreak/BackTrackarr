package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// A template error stops a page partway, which drops its scripts. Every page
// with a Jobs panel must render fully with a multi-file job listed.
func TestPagesRenderWithMultiFileJob(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: dir}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := core.New(context.Background(), cfg, st, nil, nil, log)
	h := New(cfg, st, eng, log).Handler()
	src := t.TempDir()
	var files []core.ImportFile
	for _, n := range []string{"a.json", "b.json"} {
		p := filepath.Join(src, n)
		body := `[{"watched_at":"2024-01-02T00:00:00.000Z","type":"movie","movie":{"title":"` + n + `","year":2000,"ids":{"tmdb":` + map[string]string{"a.json": "1", "b.json": "2"}[n] + `}}}]`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, core.ImportFile{Path: p, Name: n})
	}
	if err := eng.StartFileImport("import ← a.json and 1 more", files, core.FileImport{Mode: "import", User: "me"}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); eng.Jobs.AnyRunning(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("job didn't finish")
		}
	}
	for _, p := range []string{"/jobs", "/import", "/comics", "/books", "/games", "/streaming", "/sync", "/library", "/recs", "/autodelete", "/encryption"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d", p, w.Code)
			continue
		}
		body := w.Body.String()
		if !strings.Contains(body, "dropzone.js") || !strings.Contains(body, "refresh.js") {
			t.Errorf("%s stopped rendering before its scripts", p)
		}
		if p == "/jobs" && !strings.Contains(body, "2 of 2 files") {
			t.Error("the job's file count is missing")
		}
	}
}
