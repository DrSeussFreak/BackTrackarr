package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/core"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func (s *Server) routesExtra(admin *http.ServeMux) {
	admin.HandleFunc("GET /library", s.libraryPage)
	admin.HandleFunc("POST /library/archive", s.archiveNow)
	admin.HandleFunc("POST /library/arr", s.archiveArrNow)
	admin.HandleFunc("GET /library/lists/{id}", s.listPage)
	admin.HandleFunc("GET /library/lists/{id}/m3u", s.listM3U)
	admin.HandleFunc("POST /library/lists/{id}/copy", s.copyList)
	admin.HandleFunc("POST /library/lists/{id}/copy-ms", s.copyListMS)
	admin.HandleFunc("GET /library/restore", s.restorePage)
	admin.HandleFunc("POST /library/restore/playlists", s.restorePlaylists)
	admin.HandleFunc("POST /library/restore/fields", s.restoreFields)
	admin.HandleFunc("GET /api/library/export", s.apiLibraryExport)
	admin.HandleFunc("GET /comics", s.comicsPage)
	admin.HandleFunc("POST /comics/import", s.comicsImport)
	admin.HandleFunc("GET /api/comics/export", s.apiComicsExport)
	admin.HandleFunc("GET /recs", s.recsPage)
	admin.HandleFunc("POST /recs", s.recsRun)
	admin.HandleFunc("POST /recs/{id}/arr", s.recToArr)
}

func (s *Server) flash(w http.ResponseWriter, r *http.Request, to, msg string) {
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: strings.ReplaceAll(msg, ";", ","), Path: "/", MaxAge: 30, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	back(w, r, to)
}

func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie("flash")
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: "flash", Path: "/", MaxAge: -1})
	return c.Value
}

// ---------- library archive ----------

func (s *Server) libraryPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sum, err := s.st.ArchiveSummary(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	lists, err := s.st.Lists(ctx, "", "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	iq := store.ItemQuery{Search: strings.TrimSpace(q.Get("q")), Type: q.Get("type"), Offset: offset, Limit: 50}
	var items []*store.PlexItem
	if iq.Search != "" || iq.Type != "" {
		if items, err = s.st.Items(ctx, iq); err != nil {
			s.fail(w, err)
			return
		}
	}
	last, _ := s.st.GetKV(ctx, "plex_archive_at")
	lastFull, _ := s.st.GetKV(ctx, "plex_archive_full_at")
	machine := ""
	if s.eng.HasPlex() {
		machine, _ = s.eng.MachineID(ctx)
	}
	s.render(w, "library", M{"Title": "Library", "Nav": "library", "Sum": sum, "Lists": lists, "Items": items, "IQ": iq,
		"Next": offset + 50, "HasMore": len(items) == 50, "LastRun": atoi64(last), "LastFull": atoi64(lastFull),
		"HasPlex": s.eng.HasPlex(), "Machine": machine, "Arr": s.eng.ArrInstances(), "Cfg": s.cfg,
		"Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(), "Flash": takeFlash(w, r)})
}

func atoi64(v string) int64 { n, _ := strconv.ParseInt(v, 10, 64); return n }

func (s *Server) archiveNow(w http.ResponseWriter, r *http.Request) {
	if !s.eng.HasPlex() {
		s.flash(w, r, "/library", "Set PLEX_URL and PLEX_TOKEN first.")
		return
	}
	full := r.FormValue("full") == "1"
	s.eng.Jobs.Start(s.eng.Context(), "archive ← plex", func(ctx context.Context, report func(string)) error {
		return s.eng.ArchivePlex(ctx, full, report)
	})
	back(w, r, "/library")
}

func (s *Server) archiveArrNow(w http.ResponseWriter, r *http.Request) {
	s.eng.Jobs.Start(s.eng.Context(), "archive ← sonarr/radarr", s.eng.ArchiveArr)
	back(w, r, "/library")
}

func (s *Server) listPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	l, err := s.st.GetList(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if l == nil {
		http.NotFound(w, r)
		return
	}
	versions, _ := s.st.ItemVersions(ctx, l.Kind, l.ID)
	var plexUsers []string
	if s.eng.HasPlex() {
		if us, err := s.eng.PlexUsers(ctx); err == nil {
			for _, u := range us {
				plexUsers = append(plexUsers, u.Name)
			}
		}
	}
	type msRow struct {
		Name, Kind string
		Users      []string
		Err        string
	}
	var ms []msRow
	for _, m := range s.eng.MediaServers() {
		us, err := s.eng.MediaServerUsers(ctx, m.Name)
		row := msRow{Name: m.Name, Kind: m.Kind, Users: us}
		if err != nil {
			row.Err = err.Error()
		}
		ms = append(ms, row)
	}
	s.render(w, "list", M{"Title": l.Title, "Nav": "library", "L": l, "Versions": len(versions), "PlexUsers": plexUsers,
		"MediaServers": ms, "Writes": s.cfg.PlexAllowWrites, "Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(),
		"Flash": takeFlash(w, r)})
}

func (s *Server) listM3U(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	l, err := s.st.GetList(r.Context(), id)
	if err != nil || l == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.m3u8"`, safeName(l.Title)))
	fmt.Fprintln(w, "#EXTM3U")
	fmt.Fprintf(w, "#PLAYLIST:%s\n", l.Title)
	for _, it := range l.Items {
		if it.File == "" {
			continue
		}
		fmt.Fprintf(w, "#EXTINF:-1,%s\n%s\n", it.Title, it.File)
	}
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
}

func (s *Server) copyList(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	req := core.CopyRequest{ListID: id, ToUser: r.FormValue("user"), Title: r.FormValue("title"), AsStatic: r.FormValue("static") == "1"}
	s.eng.Jobs.Start(s.eng.Context(), "copy playlist → "+req.ToUser, func(ctx context.Context, report func(string)) error {
		return s.eng.CopyList(ctx, req, report)
	})
	back(w, r, "/library/lists/"+r.PathValue("id"))
}

func (s *Server) copyListMS(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	server, user, title := r.FormValue("server"), r.FormValue("user"), r.FormValue("title")
	s.eng.Jobs.Start(s.eng.Context(), "copy playlist → "+server, func(ctx context.Context, report func(string)) error {
		return s.eng.CopyToMediaServer(ctx, id, server, user, title, report)
	})
	back(w, r, "/library/lists/"+r.PathValue("id"))
}

func (s *Server) restorePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sum, err := s.st.ArchiveSummary(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	machine := ""
	if s.eng.HasPlex() {
		machine, _ = s.eng.MachineID(ctx)
	}
	from := r.URL.Query().Get("from")
	var plan []core.FieldChange
	var planErr string
	if from != "" && machine != "" {
		plan, err = s.eng.PlanFieldRestore(ctx, from)
		if err != nil {
			planErr = err.Error()
		}
	}
	shown := plan
	if len(shown) > 200 {
		shown = shown[:200]
	}
	s.render(w, "restore", M{"Title": "Restore", "Nav": "library", "Servers": sum.Servers, "Machine": machine, "From": from,
		"Plan": shown, "PlanCount": len(plan), "PlanErr": planErr, "Writes": s.cfg.PlexAllowWrites,
		"Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning()})
}

func (s *Server) restorePlaylists(w http.ResponseWriter, r *http.Request) {
	from := r.FormValue("from")
	s.eng.Jobs.Start(s.eng.Context(), "restore playlists → plex", func(ctx context.Context, report func(string)) error {
		return s.eng.RestorePlaylists(ctx, from, report)
	})
	back(w, r, "/library/restore?from="+from)
}

func (s *Server) restoreFields(w http.ResponseWriter, r *http.Request) {
	from := r.FormValue("from")
	s.eng.Jobs.Start(s.eng.Context(), "restore custom fields → plex", func(ctx context.Context, report func(string)) error {
		return s.eng.ApplyFieldRestore(ctx, from, report)
	})
	back(w, r, "/library/restore?from="+from)
}

// apiLibraryExport streams the whole archive (items with full metadata, then
// lists with their items) as NDJSON: a portable copy independent of BackTrackarr.
func (s *Server) apiLibraryExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="plex-archive-`+time.Now().Format("20060102")+`.ndjson"`)
	enc := json.NewEncoder(w)
	err := s.st.EachItem(ctx, "", true, func(it *store.PlexItem) error {
		return enc.Encode(M{"record": "item", "item": it, "metadata": json.RawMessage(nonEmpty(it.Data))})
	})
	if err != nil {
		s.log.Error("library export", "err", err)
		return
	}
	lists, err := s.st.Lists(ctx, "", "", "")
	if err != nil {
		return
	}
	for _, l := range lists {
		full, err := s.st.GetList(ctx, l.ID)
		if err != nil || full == nil {
			continue
		}
		enc.Encode(M{"record": full.Kind, "list": full, "metadata": json.RawMessage(nonEmpty(full.Data))})
	}
}

func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}

// ---------- comics ----------

func (s *Server) comicsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	cq := store.ComicQuery{Search: strings.TrimSpace(q.Get("q")), ReadOnly: q.Get("read") == "1", Offset: offset, Limit: 100}
	comics, err := s.st.Comics(ctx, cq)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := s.st.ComicStats(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "comics", M{"Title": "Comics", "Nav": "comics", "Comics": comics, "Stats": st, "Q": cq,
		"Next": offset + 100, "HasMore": len(comics) == 100, "Dir": s.cfg.ClzImportDir, "Flash": takeFlash(w, r)})
}

func (s *Server) comicsImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		s.flash(w, r, "/comics", "Choose a CLZ CSV export to upload.")
		return
	}
	defer f.Close()
	res, err := s.eng.ImportCLZ(r.Context(), f, "clz:"+hdr.Filename)
	if err != nil {
		s.flash(w, r, "/comics", "Import failed: "+err.Error())
		return
	}
	s.flash(w, r, "/comics", fmt.Sprintf("Imported %s: %d rows, %d new, %d updated, %d new reads.", hdr.Filename, res.Rows, res.New, res.Updated, res.Reads))
}

func (s *Server) apiComicsExport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="comics-`+time.Now().Format("20060102")+`.ndjson"`)
	enc := json.NewEncoder(w)
	if err := s.st.EachComic(r.Context(), func(c store.Comic, reads []int64) error {
		return enc.Encode(M{"comic": c, "reads": reads})
	}); err != nil {
		s.log.Error("comics export", "err", err)
	}
}

// ---------- recommendations ----------

func (s *Server) recsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs, err := s.st.RecRuns(ctx, 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	users, _ := s.st.Users(ctx)
	type arrOpt struct{ Name, Kind string }
	var arr []arrOpt
	for _, a := range s.eng.ArrInstances() {
		arr = append(arr, arrOpt{a.Name, a.Kind})
	}
	s.render(w, "recs", M{"Title": "Recommendations", "Nav": "recs", "Runs": runs, "Users": users, "DefaultUser": s.defaultUser(users),
		"Providers": s.eng.AIProviders(), "Arr": arr, "Cfg": s.cfg, "Jobs": s.eng.Jobs.List(), "Refresh": s.eng.Jobs.AnyRunning(),
		"Flash": takeFlash(w, r)})
}

func (s *Server) recsRun(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.FormValue("count"))
	req := core.RecRequest{User: r.FormValue("user"), Provider: r.FormValue("provider"), Kind: r.FormValue("kind"), Count: n,
		IncludeAdult: r.FormValue("adult") == "1", Note: r.FormValue("note")}
	s.eng.Jobs.Start(s.eng.Context(), "recommendations", func(ctx context.Context, report func(string)) error {
		return s.eng.Recommend(ctx, req, report)
	})
	back(w, r, "/recs")
}

func (s *Server) recToArr(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	msg, err := s.eng.AddToArr(r.Context(), id, r.FormValue("instance"))
	if err != nil {
		msg = "Could not add: " + err.Error()
	}
	s.flash(w, r, "/recs", msg)
}
