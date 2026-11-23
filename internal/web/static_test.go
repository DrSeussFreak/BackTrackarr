package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStaticFilesRevalidate(t *testing.T) {
	h := staticFiles()
	r := httptest.NewRequest(http.MethodGet, assetURL("dropzone.js"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("HTTP %d etag %q cache %q", w.Code, etag, w.Header().Get("Cache-Control"))
	}
	if !strings.Contains(w.Body.String(), "Drop files or folders") {
		t.Fatal("wrong file served")
	}
	r = httptest.NewRequest(http.MethodGet, "/static/dropzone.js", nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified {
		t.Fatalf("unchanged file: HTTP %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/static/dropzone.js", nil)
	r.Header.Set("If-None-Match", `"old-version"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("changed file: HTTP %d", w.Code)
	}
}

// Every script and stylesheet must use a versioned link, or an update can be
// hidden by a browser's cached copy.
func TestTemplatesVersionScriptsAndStyles(t *testing.T) {
	bad := regexp.MustCompile(`(?:src|href)="/static/[^"]+\.(?:js|css)"`)
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil || len(pages) == 0 {
		t.Fatal(err)
	}
	for _, p := range pages {
		b, _ := assets.ReadFile(p)
		if m := bad.Find(b); m != nil {
			t.Errorf("%s: unversioned %s", p, m)
		}
	}
}
