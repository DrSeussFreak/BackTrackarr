package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestShelfPages(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{Shelf: config.ShelfConfig{User: "me", BooksImportDir: "/data/import/books", GamesImportDir: "/data/import/games"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := core.New(context.Background(), cfg, st, nil, nil, log)
	h := New(cfg, st, eng, log).Handler()

	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", path, w.Code)
		}
		return w.Body.String()
	}
	upload := func(path, name, content string) {
		t.Helper()
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, _ := mw.CreateFormFile("file", name)
		io.WriteString(fw, content)
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, path, &b)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: HTTP %d", path, w.Code)
		}
		// Imports run as background jobs.
		for end := time.Now().Add(10 * time.Second); eng.Jobs.AnyRunning(); {
			if time.Now().After(end) {
				t.Fatal("import didn't finish")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Empty pages render.
	for _, p := range []string{"/", "/books", "/books?type=audiobook", "/games"} {
		get(p)
	}
	upload("/books/import", "goodreads.csv", "Book Id,Title,Author,Binding,Date Read,Exclusive Shelf,Read Count\n1,<Dune>,Frank Herbert,Audible Audio,2023/04/05,read,2\n")
	upload("/games/import", "playnite.json", `[{"Name":"Hades","GameId":"1","PluginId":"p","Playtime":7200,"LastActivity":"2024-05-01T20:00:00Z","Platforms":[{"Name":"PC"}]}]`)

	books := get("/books")
	if !strings.Contains(books, "&lt;Dune&gt;") || strings.Contains(books, "<Dune>") {
		t.Error("book titles must be shown and escaped")
	}
	if !strings.Contains(get("/books?type=audiobook"), "&lt;Dune&gt;") {
		t.Error("audiobook filter should include it")
	}
	if strings.Contains(get("/books?type=book"), "&lt;Dune&gt;") {
		t.Error("book filter should leave out audiobooks")
	}
	games := get("/games")
	if !strings.Contains(games, "Hades") || !strings.Contains(games, "2 h 0 m") {
		t.Error("games page should show the game and its playtime")
	}
	dash := get("/")
	if !strings.Contains(dash, "Books and audiobooks") || !strings.Contains(dash, "Time played") {
		t.Error("dashboard should show books and games")
	}
	if x := get("/api/books/export"); !strings.Contains(x, "Audible Audio") {
		t.Error("export should include raw source data")
	}
}
