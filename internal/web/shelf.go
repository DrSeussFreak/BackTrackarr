package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func (s *Server) routesShelf(admin *http.ServeMux) {
	for _, covers := range []string{"books", "games"} {
		admin.HandleFunc("GET /"+covers, func(w http.ResponseWriter, r *http.Request) { s.shelfPage(w, r, covers) })
		admin.HandleFunc("POST /"+covers+"/import", func(w http.ResponseWriter, r *http.Request) { s.shelfImport(w, r, covers) })
		admin.HandleFunc("POST /"+covers+"/sync", func(w http.ResponseWriter, r *http.Request) { s.shelfSync(w, r, covers) })
		admin.HandleFunc("GET /api/"+covers+"/export", func(w http.ResponseWriter, r *http.Request) { s.shelfExport(w, r, covers) })
	}
}

// shelfType limits the ?type= filter to known values.
func shelfType(covers, v string) string {
	if covers == "games" {
		return "game"
	}
	if v == "book" || v == "audiobook" {
		return v
	}
	return ""
}

func (s *Server) shelfPage(w http.ResponseWriter, r *http.Request, covers string) {
	ctx := r.Context()
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	sq := store.ShelfQuery{Type: shelfType(covers, q.Get("type")), Search: strings.TrimSpace(q.Get("q")),
		Status: q.Get("status"), Offset: offset, Limit: 100}
	items, err := s.st.ShelfItems(ctx, sq)
	if err != nil {
		s.fail(w, err)
		return
	}
	stats, err := s.st.ShelfStats(ctx, sq.Type)
	if err != nil {
		s.fail(w, err)
		return
	}
	recent, err := s.st.RecentShelfEvents(ctx, sq.Type, 15)
	if err != nil {
		s.fail(w, err)
		return
	}
	statuses, _ := s.st.ShelfStatuses(ctx, sq.Type)
	next := url.Values{"offset": {strconv.Itoa(offset + 100)}}
	for k, v := range map[string]string{"q": sq.Search, "status": sq.Status, "type": q.Get("type")} {
		if v != "" {
			next.Set(k, v)
		}
	}
	title, dir := "Books", s.cfg.Shelf.BooksImportDir
	if covers == "games" {
		title, dir = "Games", s.cfg.Shelf.GamesImportDir
	}
	s.render(w, "shelf", M{"Title": title, "Nav": covers, "Path": "/" + covers, "IsGames": covers == "games", "Type": sq.Type,
		"Stats": stats, "Items": items, "Recent": recent, "Sources": s.eng.ShelfSources(ctx, covers), "Statuses": statuses, "Q": sq,
		"NextURL": "?" + next.Encode(), "HasMore": len(items) == 100, "Dir": dir, "Jobs": s.eng.Jobs.List(),
		"Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func (s *Server) shelfImport(w http.ResponseWriter, r *http.Request, covers string) {
	to := "/" + covers
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	// Keep at most 1 MB of the upload in memory, the rest in a temp file.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.flash(w, r, to, "Upload failed: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	files, done, err := s.formFiles(r)
	if err != nil {
		s.flash(w, r, to, "Upload failed: "+err.Error())
		return
	}
	mediaType := shelfType(covers, r.FormValue("type"))
	started := s.eng.Jobs.Start(s.eng.Context(), covers+" import ← "+uploadLabel(files), func(ctx context.Context, report func(string)) error {
		defer done()
		defer debug.FreeOSMemory()
		rows, added, updated, events := 0, 0, 0, 0
		err := eachUpload(files, report, func(f *os.File, name string) (string, error) {
			var res *core.ShelfImportResult
			var err error
			switch {
			case covers == "games":
				res, err = s.eng.ImportGames(ctx, f)
			case isJSONFile(name):
				return "", s.importJSON(ctx, f, name, report)
			default:
				res, err = s.eng.ImportBooks(ctx, f, mediaType)
			}
			if res != nil {
				rows += res.Rows
				added += res.New
				updated += res.Updated
				events += res.Events
			}
			return "", err
		})
		report(fmt.Sprintf("%d rows, %d new, %d updated, %d new reads or plays", rows, added, updated, events))
		return err
	})
	if !started {
		done()
		s.flash(w, r, to, "That file is already importing.")
		return
	}
	back(w, r, to)
}

func (s *Server) shelfSync(w http.ResponseWriter, r *http.Request, covers string) {
	s.eng.Jobs.Start(s.eng.Context(), "sync ← "+covers, func(ctx context.Context, report func(string)) error {
		return s.eng.SyncShelf(ctx, covers, report)
	})
	back(w, r, "/"+covers)
}

// shelfExport streams every title with its reads or plays and raw source data.
func (s *Server) shelfExport(w http.ResponseWriter, r *http.Request, covers string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+covers+`-`+time.Now().Format("20060102")+`.ndjson"`)
	enc := json.NewEncoder(w)
	if err := s.st.EachShelfItem(r.Context(), shelfType(covers, r.URL.Query().Get("type")), func(it store.ShelfItem, evs []store.ShelfEvent) error {
		return enc.Encode(M{"item": it, "events": evs})
	}); err != nil {
		s.log.Error("shelf export", "err", err)
	}
}
