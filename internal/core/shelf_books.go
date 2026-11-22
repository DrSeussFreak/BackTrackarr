package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// ---------- Audiobookshelf ----------

type absMeta struct {
	Title         string `json:"title"`
	AuthorName    string `json:"authorName"`
	SeriesName    string `json:"seriesName"`
	ISBN          string `json:"isbn"`
	ASIN          string `json:"asin"`
	PublishedYear string `json:"publishedYear"`
}

type absSession struct {
	ID            string  `json:"id"`
	LibraryItemID string  `json:"libraryItemId"`
	MediaType     string  `json:"mediaType"`
	DisplayTitle  string  `json:"displayTitle"`
	DisplayAuthor string  `json:"displayAuthor"`
	Duration      float64 `json:"duration"`
	TimeListening float64 `json:"timeListening"`
	CurrentTime   float64 `json:"currentTime"`
	StartedAt     float64 `json:"startedAt"`
	UpdatedAt     float64 `json:"updatedAt"`
	MediaMetadata absMeta `json:"mediaMetadata"`
	DeviceInfo    struct {
		ClientName  string `json:"clientName"`
		Model       string `json:"model"`
		BrowserName string `json:"browserName"`
	} `json:"deviceInfo"`
}

type absProgress struct {
	LibraryItemID string  `json:"libraryItemId"`
	MediaItemType string  `json:"mediaItemType"`
	Progress      float64 `json:"progress"`
	EbookProgress float64 `json:"ebookProgress"`
	IsFinished    bool    `json:"isFinished"`
	LastUpdate    float64 `json:"lastUpdate"`
	StartedAt     float64 `json:"startedAt"`
	FinishedAt    float64 `json:"finishedAt"`
}

type absItem struct {
	ID        string `json:"id"`
	MediaType string `json:"mediaType"`
	Media     struct {
		Duration   float64           `json:"duration"`
		AudioFiles []json.RawMessage `json:"audioFiles"`
		Metadata   absMeta           `json:"metadata"`
	} `json:"media"`
}

func (e *Engine) syncAudiobookshelf(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	hdr := map[string]string{"Authorization": "Bearer " + c.AudiobookshelfToken}
	user := e.shelfUser(ctx)
	added := 0
	walked := e.shelfWalked(ctx, "audiobookshelf")

	// Sessions come newest first, so a page with nothing new ends the walk.
	for page := 0; ; page++ {
		var res struct {
			NumPages int               `json:"numPages"`
			Sessions []json.RawMessage `json:"sessions"`
		}
		u := fmt.Sprintf("%s/api/me/listening-sessions?itemsPerPage=200&page=%d", c.AudiobookshelfURL, page)
		if err := shelfGetJSON(ctx, u, hdr, &res); err != nil {
			return added, fmt.Errorf("listening sessions: %w", err)
		}
		fresh := 0
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, raw := range res.Sessions {
				var s absSession
				if err := json.Unmarshal(raw, &s); err != nil || s.ID == "" || s.LibraryItemID == "" || s.MediaType != "book" {
					continue // podcasts aren't tracked here
				}
				title := s.DisplayTitle
				if title == "" {
					title = s.MediaMetadata.Title
				}
				if title == "" {
					continue
				}
				it := &store.ShelfItem{MediaType: "audiobook", Source: "audiobookshelf", ExtKey: "item:" + s.LibraryItemID, Title: title,
					Creator: s.DisplayAuthor, ISBN: s.MediaMetadata.ISBN, ASIN: s.MediaMetadata.ASIN, Year: shelfYear(s.MediaMetadata.PublishedYear),
					DurationS: int64(s.Duration), LastActivity: int64(s.UpdatedAt / 1000)}
				if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
					return err
				}
				device := s.DeviceInfo.ClientName
				if device == "" {
					device = s.DeviceInfo.Model
				}
				if device == "" {
					device = s.DeviceInfo.BrowserName
				}
				if int64(s.TimeListening) <= 0 {
					continue
				}
				ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "session", ExtID: "s:" + s.ID, At: int64(s.UpdatedAt / 1000),
					StartedAt: int64(s.StartedAt / 1000), DurationS: int64(s.TimeListening), Progress: shelfFraction(s.CurrentTime, s.Duration),
					Device: device, Source: "audiobookshelf", Raw: raw}
				before := added
				if err := e.addEvent(ctx, ev, &added); err != nil {
					return err
				}
				fresh += added - before
			}
			return nil
		}); err != nil {
			return added, err
		}
		report(fmt.Sprintf("Audiobookshelf: page %d of %d", page+1, max(res.NumPages, 1)))
		if page+1 >= res.NumPages || len(res.Sessions) == 0 {
			e.markShelfWalked(ctx, "audiobookshelf")
			break
		}
		if walked && fresh == 0 {
			break
		}
	}

	// Progress holds finishes, including ebooks that have no listening sessions.
	var me struct {
		MediaProgress []absProgress `json:"mediaProgress"`
	}
	if err := shelfGetJSON(ctx, c.AudiobookshelfURL+"/api/me", hdr, &me); err != nil {
		return added, fmt.Errorf("progress: %w", err)
	}
	for _, p := range me.MediaProgress {
		if p.LibraryItemID == "" || p.MediaItemType != "book" {
			continue
		}
		key := "item:" + p.LibraryItemID
		id, err := e.st.ShelfItemID(ctx, "audiobookshelf", key)
		if err != nil {
			return added, err
		}
		if id == 0 {
			var item absItem
			err := shelfGetJSON(ctx, c.AudiobookshelfURL+"/api/items/"+url.PathEscape(p.LibraryItemID)+"?expanded=1", hdr, &item)
			if shelfStatusIs(err, 404) {
				continue // removed from the library
			}
			if err != nil {
				return added, fmt.Errorf("item: %w", err)
			}
			if item.Media.Metadata.Title == "" {
				continue
			}
			mt := "book"
			if item.Media.Duration > 0 || len(item.Media.AudioFiles) > 0 {
				mt = "audiobook"
			}
			m := item.Media.Metadata
			it := &store.ShelfItem{MediaType: mt, Source: "audiobookshelf", ExtKey: key, Title: m.Title, Creator: m.AuthorName, Series: m.SeriesName,
				ISBN: m.ISBN, ASIN: m.ASIN, Year: shelfYear(m.PublishedYear), DurationS: int64(item.Media.Duration), Raw: shelfRaw(item)}
			if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
				return added, err
			}
			id = it.ID
		}
		progress, status := p.Progress, "in-progress"
		if progress == 0 {
			progress = p.EbookProgress
		}
		if p.IsFinished {
			status, progress = "finished", 1
		} else if e.pastFinish(absKind(p.Progress, p.EbookProgress), progress) {
			status = "finished"
		}
		if err := e.st.TouchShelfItem(ctx, id, status, progress, int64(p.LastUpdate/1000)); err != nil {
			return added, err
		}
		if !p.IsFinished {
			if err := e.progressFinish(ctx, id, user, "audiobookshelf", absKind(p.Progress, p.EbookProgress), progress, int64(p.LastUpdate/1000), &added); err != nil {
				return added, err
			}
		} else {
			if err := e.st.DropProgressFinishes(ctx, id); err != nil {
				return added, err
			}
			at := p.FinishedAt
			if at == 0 {
				at = p.LastUpdate
			}
			ev := &store.ShelfEvent{ItemID: id, User: user, Kind: "finish", ExtID: "f:" + strconv.FormatInt(int64(at), 10), At: int64(at / 1000),
				StartedAt: int64(p.StartedAt / 1000), Progress: 1, Source: "audiobookshelf", Raw: shelfRaw(p)}
			if err := e.addEvent(ctx, ev, &added); err != nil {
				return added, err
			}
		}
	}
	return added, nil
}

// shelfComic files a comic from a reading server on the Comics page, so it
// sits with the CLZ collection. A new read date is a reread.
func (e *Engine) shelfComic(ctx context.Context, c *store.Comic, readAt int64, source string, n *int) error {
	if _, err := e.st.UpsertComic(ctx, c); err != nil {
		return err
	}
	if readAt <= 0 {
		return nil
	}
	ok, err := e.st.AddComicRead(ctx, c.ID, time.Unix(readAt, 0), source)
	if ok {
		*n++
	}
	return err
}

// ---------- Komga ----------

type komgaBook struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SeriesTitle string `json:"seriesTitle"`
	Metadata    struct {
		Title       string `json:"title"`
		Number      string `json:"number"`
		ReleaseDate string `json:"releaseDate"`
		ISBN        string `json:"isbn"`
		Authors     []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"authors"`
	} `json:"metadata"`
	Media struct {
		MediaProfile string `json:"mediaProfile"`
		PagesCount   int64  `json:"pagesCount"`
	} `json:"media"`
	ReadProgress *struct {
		Page       int64  `json:"page"`
		Completed  bool   `json:"completed"`
		ReadDate   string `json:"readDate"`
		DeviceName string `json:"deviceName"`
	} `json:"readProgress"`
}

func (e *Engine) syncKomga(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	hdr := map[string]string{"X-API-Key": c.KomgaKey}
	body := map[string]any{"condition": map[string]any{"readStatus": map[string]string{"operator": "isNot", "value": "UNREAD"}}}
	user := e.shelfUser(ctx)
	added := 0
	for page := 0; ; page++ {
		var res struct {
			Content []json.RawMessage `json:"content"`
			Last    bool              `json:"last"`
		}
		u := fmt.Sprintf("%s/api/v1/books/list?page=%d&size=200", c.KomgaURL, page)
		if err := shelfSearchJSON(ctx, u, hdr, body, &res); err != nil {
			return added, err
		}
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, raw := range res.Content {
				var b komgaBook
				if err := json.Unmarshal(raw, &b); err != nil || b.ID == "" || b.ReadProgress == nil {
					continue
				}
				title := b.Metadata.Title
				if title == "" {
					title = b.Name
				}
				at := shelfTime(b.ReadProgress.ReadDate)
				if b.Media.MediaProfile == "DIVINA" {
					cm := &store.Comic{ExtKey: "komga:" + b.ID, Series: b.SeriesTitle, Issue: b.Metadata.Number, Title: title,
						ReleaseDate: b.Metadata.ReleaseDate, Barcode: b.Metadata.ISBN, Format: "digital",
						Raw: map[string]string{"komga": string(raw)}}
					fraction := shelfFraction(float64(b.ReadProgress.Page), float64(b.Media.PagesCount))
					if err := e.shelfComicProgress(ctx, cm, b.ReadProgress.Completed, fraction, at, "komga", &added); err != nil {
						return err
					}
					continue
				}
				creator := ""
				for _, a := range b.Metadata.Authors {
					if r := strings.ToLower(a.Role); r == "author" || r == "writer" {
						creator = a.Name
						break
					}
				}
				status, progress := "in-progress", shelfFraction(float64(b.ReadProgress.Page), float64(b.Media.PagesCount))
				if b.ReadProgress.Completed {
					status, progress = "finished", 1
				} else if e.pastFinish("book", progress) {
					status = "finished"
				}
				it := &store.ShelfItem{MediaType: "book", Source: "komga", ExtKey: "book:" + b.ID, Title: title, Creator: creator, Series: b.SeriesTitle,
					ISBN: b.Metadata.ISBN, Year: shelfYear(b.Metadata.ReleaseDate), Pages: b.Media.PagesCount, Status: status, Progress: progress,
					LastActivity: at, Raw: raw}
				if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
					return err
				}
				if !b.ReadProgress.Completed {
					if err := e.progressFinish(ctx, it.ID, user, "komga", "book", progress, at, &added); err != nil {
						return err
					}
				} else {
					if err := e.st.DropProgressFinishes(ctx, it.ID); err != nil {
						return err
					}
					ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "finish", ExtID: "f:" + b.ReadProgress.ReadDate, At: at, Progress: 1,
						Device: b.ReadProgress.DeviceName, Source: "komga"}
					if err := e.addEvent(ctx, ev, &added); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return added, err
		}
		report(fmt.Sprintf("Komga: page %d", page+1))
		if res.Last || len(res.Content) == 0 {
			return added, nil
		}
	}
}

// ---------- Kavita ----------

type kavitaHistory struct {
	SeriesID     int64           `json:"seriesId"`
	SeriesName   string          `json:"seriesName"`
	SeriesFormat json.RawMessage `json:"seriesFormat"`
	LibraryName  string          `json:"libraryName"`
	Chapters     []struct {
		ChapterID       int64  `json:"chapterId"`
		Label           string `json:"label"`
		StartTimeUtc    string `json:"startTimeUtc"`
		EndTimeUtc      string `json:"endTimeUtc"`
		DurationSeconds int64  `json:"durationSeconds"`
		EndPage         int64  `json:"endPage"`
		TotalPages      int64  `json:"totalPages"`
		Completed       bool   `json:"completed"`
	} `json:"chapters"`
}

// kavitaIsBook: Kavita's formats are 0 Image, 1 Archive, 2 Unknown, 3 EPUB,
// 4 PDF. EPUB and PDF are books, the rest are comics or manga.
func kavitaIsBook(f json.RawMessage) bool {
	switch strings.ToLower(strings.Trim(string(f), `" `)) {
	case "3", "4", "epub", "pdf":
		return true
	}
	return false
}

func (e *Engine) syncKavita(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	hdr := map[string]string{"x-api-key": c.KavitaKey}
	user := e.shelfUser(ctx)
	added := 0

	// After one full import, read only since the last good sync, with a
	// week's overlap for sessions that were still open.
	from := ""
	if e.shelfWalked(ctx, "kavita") {
		v, _ := e.st.GetKV(ctx, "shelf|kavita|at")
		if last, _ := strconv.ParseInt(v, 10, 64); last > 0 {
			from = time.Unix(last, 0).UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
		}
	}
	for page := 1; page <= 5000; page++ {
		q := url.Values{"PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}}
		if from != "" {
			q.Set("StartDate", from)
		}
		var res []kavitaHistory
		if err := shelfGetJSON(ctx, c.KavitaURL+"/api/Stats/reading-history?"+q.Encode(), hdr, &res); err != nil {
			return added, fmt.Errorf("reading history: %w", err)
		}
		if len(res) == 0 {
			e.markShelfWalked(ctx, "kavita")
			return added, nil
		}
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, h := range res {
				isBook := kavitaIsBook(h.SeriesFormat)
				for _, ch := range h.Chapters {
					start, end := shelfTime(ch.StartTimeUtc), shelfTime(ch.EndTimeUtc)
					if end == 0 {
						end = start
					}
					if ch.ChapterID == 0 || end == 0 {
						continue
					}
					chID := strconv.FormatInt(ch.ChapterID, 10)
					if !isBook {
						cm := &store.Comic{ExtKey: "kavita:" + chID, Series: h.SeriesName, Issue: ch.Label, Format: "digital",
							Raw: map[string]string{"kavita": string(shelfRaw(h))}}
						fraction := shelfFraction(float64(ch.EndPage), float64(ch.TotalPages))
						if err := e.shelfComicProgress(ctx, cm, ch.Completed, fraction, end, "kavita", &added); err != nil {
							return err
						}
						continue
					}
					title := h.SeriesName
					if ch.Label != "" && !strings.EqualFold(ch.Label, h.SeriesName) {
						title += " · " + ch.Label
					}
					status, progress := "in-progress", shelfFraction(float64(ch.EndPage), float64(ch.TotalPages))
					if ch.Completed {
						status, progress = "finished", 1
					} else if e.pastFinish("book", progress) {
						status = "finished"
					}
					it := &store.ShelfItem{MediaType: "book", Source: "kavita", ExtKey: "chapter:" + chID, Title: title, Series: h.SeriesName,
						Pages: ch.TotalPages, Status: status, Progress: progress, LastActivity: end}
					if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
						return err
					}
					ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "session", ExtID: "s:" + ch.StartTimeUtc, At: end, StartedAt: start,
						DurationS: ch.DurationSeconds, Progress: progress, Source: "kavita", Raw: shelfRaw(ch)}
					if err := e.addEvent(ctx, ev, &added); err != nil {
						return err
					}
					if !ch.Completed {
						if err := e.progressFinish(ctx, it.ID, user, "kavita", "book", progress, end, &added); err != nil {
							return err
						}
					} else {
						if err := e.st.DropProgressFinishes(ctx, it.ID); err != nil {
							return err
						}
						f := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "finish", ExtID: "f:" + ch.EndTimeUtc, At: end, Progress: 1, Source: "kavita"}
						if err := e.addEvent(ctx, f, &added); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}); err != nil {
			return added, err
		}
		report(fmt.Sprintf("Kavita: page %d", page))
	}
	return added, nil
}

// ---------- Hardcover ----------

const hardcoverQuery = `query BackTrackarrLibrary($limit: Int!, $offset: Int!) {
  me {
    user_books(limit: $limit, offset: $offset, order_by: {id: asc}) {
      id status_id rating read_count updated_at
      book { id title release_year pages audio_seconds cached_contributors }
      edition { isbn_13 asin pages audio_seconds }
      user_book_reads(order_by: {id: asc}) {
        id started_at finished_at progress_pages progress_seconds
        edition { pages audio_seconds }
      }
    }
  }
}`

type hardcoverRead struct {
	ID              int64   `json:"id"`
	StartedAt       string  `json:"started_at"`
	FinishedAt      string  `json:"finished_at"`
	ProgressPages   float64 `json:"progress_pages"`
	ProgressSeconds float64 `json:"progress_seconds"`
	Edition         *struct {
		Pages        float64 `json:"pages"`
		AudioSeconds float64 `json:"audio_seconds"`
	} `json:"edition"`
}

type hardcoverBook struct {
	ID        int64    `json:"id"`
	StatusID  int      `json:"status_id"`
	Rating    *float64 `json:"rating"`
	ReadCount int64    `json:"read_count"`
	UpdatedAt string   `json:"updated_at"`
	Book      struct {
		Title        string          `json:"title"`
		ReleaseYear  int64           `json:"release_year"`
		Pages        int64           `json:"pages"`
		AudioSeconds int64           `json:"audio_seconds"`
		Contributors json.RawMessage `json:"cached_contributors"`
	} `json:"book"`
	Edition *struct {
		ISBN13       string `json:"isbn_13"`
		ASIN         string `json:"asin"`
		Pages        int64  `json:"pages"`
		AudioSeconds int64  `json:"audio_seconds"`
	} `json:"edition"`
	Reads []hardcoverRead `json:"user_book_reads"`
}

var hardcoverStatus = map[int]string{1: "want-to-read", 2: "reading", 3: "read", 4: "paused", 5: "did-not-finish", 6: "ignored"}

func hardcoverAuthor(raw json.RawMessage) string {
	var list []struct {
		Author struct {
			Name string `json:"name"`
		} `json:"author"`
	}
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		return ""
	}
	return list[0].Author.Name
}

func (e *Engine) syncHardcover(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	hdr := map[string]string{"Authorization": "Bearer " + c.HardcoverToken}
	user := e.shelfUser(ctx)
	added := 0
	const size = 100
	for offset := 0; ; offset += size {
		var res struct {
			Data struct {
				Me []struct {
					UserBooks []json.RawMessage `json:"user_books"`
				} `json:"me"`
			} `json:"data"`
			Errors json.RawMessage `json:"errors"`
		}
		body := map[string]any{"query": hardcoverQuery, "variables": map[string]int{"limit": size, "offset": offset}}
		if err := shelfSearchJSON(ctx, c.HardcoverURL+"/v1/graphql", hdr, body, &res); err != nil {
			return added, err
		}
		if len(res.Errors) > 0 && string(res.Errors) != "null" {
			msg := string(res.Errors)
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return added, fmt.Errorf("GraphQL: %s", msg)
		}
		if len(res.Data.Me) == 0 {
			return added, fmt.Errorf("the token didn't return an account")
		}
		books := res.Data.Me[0].UserBooks
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for _, raw := range books {
				var b hardcoverBook
				if err := json.Unmarshal(raw, &b); err != nil || b.ID == 0 || b.Book.Title == "" {
					continue
				}
				it := &store.ShelfItem{MediaType: "book", Source: "hardcover", ExtKey: "user_book:" + strconv.FormatInt(b.ID, 10), Title: b.Book.Title,
					Creator: hardcoverAuthor(b.Book.Contributors), Year: b.Book.ReleaseYear, Pages: b.Book.Pages, Status: hardcoverStatus[b.StatusID], Raw: raw}
				if b.Rating != nil {
					it.Rating = *b.Rating
				}
				if b.Edition != nil {
					it.ISBN, it.ASIN = b.Edition.ISBN13, b.Edition.ASIN
					if b.Edition.Pages > 0 {
						it.Pages = b.Edition.Pages
					}
					// Only an audio edition has a running time and no page count.
					if b.Edition.AudioSeconds > 0 && b.Edition.Pages == 0 {
						it.MediaType, it.DurationS = "audiobook", b.Edition.AudioSeconds
					}
				}
				for _, r := range b.Reads {
					at := max(shelfTime(r.FinishedAt), shelfTime(r.StartedAt))
					it.LastActivity = max(it.LastActivity, at)
				}
				if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
					return err
				}
				for _, r := range b.Reads {
					started, finished := shelfTime(r.StartedAt), shelfTime(r.FinishedAt)
					ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "session", ExtID: "r:" + strconv.FormatInt(r.ID, 10), At: started,
						StartedAt: started, DurationS: int64(r.ProgressSeconds), Source: "hardcover", Raw: shelfRaw(r)}
					if r.Edition != nil {
						ev.Progress = max(shelfFraction(r.ProgressPages, r.Edition.Pages), shelfFraction(r.ProgressSeconds, r.Edition.AudioSeconds))
					}
					if finished > 0 {
						ev.Kind, ev.At, ev.Progress = "finish", finished, 1
						if err := e.st.DropProgressFinishes(ctx, it.ID); err != nil {
							return err
						}
					}
					if ev.Kind == "session" && ev.At == 0 {
						continue // an undated, unfinished read holds nothing to chart
					}
					if err := e.addEvent(ctx, ev, &added); err != nil {
						return err
					}
					if ev.Kind == "session" {
						if err := e.progressFinish(ctx, it.ID, user, "hardcover", it.MediaType, ev.Progress, ev.At, &added); err != nil {
							return err
						}
					}
				}
				count := b.ReadCount
				if b.StatusID == 3 && count == 0 {
					count = 1
				}
				if err := e.st.SetUndatedFinishes(ctx, it.ID, user, "hardcover", count); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return added, err
		}
		report(fmt.Sprintf("Hardcover: %d titles", offset+len(books)))
		if len(books) < size {
			return added, nil
		}
	}
}

// ---------- Open Library ----------

func (e *Engine) syncOpenLibrary(ctx context.Context, report func(string)) (int, error) {
	c := e.cfg.Shelf
	user := e.shelfUser(ctx)
	added := 0
	for _, shelf := range []struct{ path, status string }{{"already-read", "read"}, {"currently-reading", "reading"}} {
		const size = 100
		for page := 1; ; page++ {
			var res struct {
				NumFound int               `json:"numFound"`
				Entries  []json.RawMessage `json:"reading_log_entries"`
			}
			u := fmt.Sprintf("%s/people/%s/books/%s.json?page=%d&limit=%d", c.OpenLibraryURL, url.PathEscape(c.OpenLibraryUser), shelf.path, page, size)
			if err := shelfGetJSON(ctx, u, nil, &res); err != nil {
				return added, fmt.Errorf("%s: %w", shelf.path, err)
			}
			if err := e.st.InTx(ctx, func(ctx context.Context) error {
				for _, raw := range res.Entries {
					var en struct {
						Work struct {
							Title       string   `json:"title"`
							Key         string   `json:"key"`
							AuthorNames []string `json:"author_names"`
							Year        int64    `json:"first_publish_year"`
						} `json:"work"`
						LoggedDate string `json:"logged_date"`
					}
					if err := json.Unmarshal(raw, &en); err != nil || en.Work.Key == "" || en.Work.Title == "" {
						continue
					}
					at := shelfTime(en.LoggedDate)
					it := &store.ShelfItem{MediaType: "book", Source: "openlibrary", ExtKey: en.Work.Key, Title: en.Work.Title,
						Creator: strings.Join(en.Work.AuthorNames, ", "), Year: en.Work.Year, Status: shelf.status, LastActivity: at, Raw: raw}
					if _, err := e.st.UpsertShelfItem(ctx, it); err != nil {
						return err
					}
					if shelf.status == "read" && at > 0 {
						ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "finish", ExtID: "f:" + en.LoggedDate, At: at, Progress: 1, Source: "openlibrary"}
						if err := e.addEvent(ctx, ev, &added); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				return added, err
			}
			report(fmt.Sprintf("Open Library: %s page %d", shelf.path, page))
			if len(res.Entries) == 0 || page*size >= res.NumFound {
				break
			}
		}
	}
	return added, nil
}

// ---------- Goodreads, StoryGraph and other CSV exports ----------

var bookColumns = map[string][]string{
	"id":        {"bookid"},
	"title":     {"title", "booktitle", "name"},
	"author":    {"author", "authors", "authorname", "narrator"},
	"isbn13":    {"isbn13"},
	"isbn":      {"isbn", "isbnuid"},
	"asin":      {"asin"},
	"rating":    {"myrating", "starrating", "rating"},
	"format":    {"binding", "format"},
	"pages":     {"numberofpages", "pages"},
	"year":      {"originalpublicationyear", "yearpublished", "year", "releasedate"},
	"status":    {"exclusiveshelf", "readstatus", "status"},
	"dates":     {"datesread"},
	"dateread":  {"dateread", "lastdateread", "datefinished", "finished", "finishdate"},
	"started":   {"datestarted", "started", "startdate"},
	"readcount": {"readcount"},
	"series":    {"series"},
}

var isoDate = regexp.MustCompile(`\d{4}[/-]\d{1,2}[/-]\d{1,2}`)

// bookReads returns each dated read as start and finish times. StoryGraph
// writes "2023/01/02-2023/01/10, 2024/03/01-2024/03/05".
func bookReads(dates, single, started string) [][2]int64 {
	var out [][2]int64
	seen := map[int64]bool{}
	add := func(s, f int64) {
		if f > 0 && !seen[f] {
			seen[f] = true
			out = append(out, [2]int64{s, f})
		}
	}
	day := func(v string) int64 {
		if t, ok := parseDate(strings.ReplaceAll(v, "/", "-")); ok {
			return t.Unix()
		}
		return 0
	}
	for _, part := range strings.Split(dates, ",") {
		m := isoDate.FindAllString(part, -1)
		if len(m) >= 2 {
			add(day(m[0]), day(m[len(m)-1]))
		} else if len(m) == 1 {
			add(0, day(m[0]))
		}
	}
	if m := isoDate.FindString(single); m != "" {
		add(day(isoDate.FindString(started)), day(m))
	} else if t, ok := parseDate(single); ok {
		add(0, t.Unix())
	}
	return out
}

func shelfColumns(header []string, names map[string][]string) map[string]int {
	col := map[string]int{}
	for i, h := range header {
		n := normHeader(h)
		for field, opts := range names {
			if _, done := col[field]; done {
				continue
			}
			for _, o := range opts {
				if n == o {
					col[field] = i
				}
			}
		}
	}
	return col
}

func hashKey(parts ...string) string {
	h := sha1.Sum([]byte(strings.ToLower(strings.Join(parts, "|"))))
	return "h:" + hex.EncodeToString(h[:10])
}

// cleanISBN undoes Goodreads' ="0123456789" spreadsheet quoting.
func cleanISBN(s string) string {
	return strings.Trim(strings.TrimSpace(s), `="`)
}

// shelfRows reads a CSV one row at a time, so a large export never sits in
// memory. A JSON list is small enough to read whole.
func shelfRows(r io.Reader) ([]string, func() ([]string, error), error) {
	br := bufio.NewReader(r)
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	head, _ := br.Peek(4096)
	if t := bytes.TrimSpace(head); len(t) > 0 && (t[0] == '[' || t[0] == '{') {
		header, rows, err := jsonRows(br)
		if err != nil {
			return nil, nil, err
		}
		i := 0
		return header, func() ([]string, error) {
			if i >= len(rows) {
				return nil, io.EOF
			}
			i++
			return rows[i-1], nil
		}, nil
	}
	cr := csv.NewReader(br)
	cr.Comma = detectComma(head)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = false
	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("reading header: %w", err)
	}
	return header, cr.Read, nil
}

// ImportBooks reads a Goodreads, StoryGraph or similar CSV export. mediaType
// forces book or audiobook, otherwise the Format/Binding column decides.
// Re-importing is safe: a new read date is a new row, and a read count higher
// than the dates given is kept as undated reads.
func (e *Engine) ImportBooks(ctx context.Context, r io.Reader, mediaType string) (*ShelfImportResult, error) {
	header, next, err := shelfRows(r)
	if err != nil {
		return nil, err
	}
	if kind := audibleKind(header, ""); kind != "" {
		return e.importAudibleRows(ctx, header, next, kind)
	}
	col := shelfColumns(header, bookColumns)
	if _, ok := col["title"]; !ok {
		return nil, fmt.Errorf("no Title column found (columns: %s)", strings.Join(header, ", "))
	}
	source := "csv"
	if _, ok := col["id"]; ok {
		if i, ok := col["status"]; ok && normHeader(header[i]) == "exclusiveshelf" {
			source = "goodreads"
		}
	}
	if i, ok := col["status"]; ok && normHeader(header[i]) == "readstatus" {
		source = "storygraph"
	}
	user := e.shelfUser(ctx)
	res := &ShelfImportResult{Source: source}
	// Commit every 500 rows: fast, and other writers are never held up for long.
	for done := false; !done; {
		if err := e.st.InTx(ctx, func(ctx context.Context) error {
			for n := 0; n < 500; n++ {
				rec, err := next()
				if err == io.EOF {
					done = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("row %d: %w", res.Rows+2, err)
				}
				get := func(f string) string {
					if i, ok := col[f]; ok && i < len(rec) {
						return strings.TrimSpace(rec[i])
					}
					return ""
				}
				title := get("title")
				if title == "" {
					continue
				}
				res.Rows++
				raw := map[string]string{}
				for i, h := range header {
					if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
						raw[strings.TrimSpace(h)] = rec[i]
					}
				}
				isbn := cleanISBN(get("isbn13"))
				if isbn == "" {
					isbn = cleanISBN(get("isbn"))
				}
				key := ""
				switch {
				case get("id") != "":
					key = "id:" + get("id")
				case isbn != "":
					key = "isbn:" + isbn
				case get("asin") != "":
					key = "asin:" + get("asin")
				default:
					key = hashKey(title, get("author"))
				}
				mt := mediaType
				if mt != "book" && mt != "audiobook" {
					mt = "book"
					if strings.Contains(strings.ToLower(get("format")), "audio") {
						mt = "audiobook"
					}
				}
				status := strings.ToLower(get("status"))
				reads := bookReads(get("dates"), get("dateread"), get("started"))
				it := &store.ShelfItem{MediaType: mt, Source: source, ExtKey: key, Title: title, Creator: get("author"), Series: get("series"),
					ISBN: isbn, ASIN: get("asin"), Year: shelfYear(get("year")), Status: status, Raw: shelfRaw(raw)}
				it.Pages, _ = strconv.ParseInt(get("pages"), 10, 64)
				it.Rating, _ = strconv.ParseFloat(get("rating"), 64)
				for _, rd := range reads {
					it.LastActivity = max(it.LastActivity, rd[1])
				}
				prev, err := e.st.UpsertShelfItem(ctx, it)
				if err != nil {
					return err
				}
				if prev.New {
					res.New++
				} else {
					res.Updated++
				}
				for _, rd := range reads {
					ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "finish", At: rd[1], StartedAt: rd[0], Progress: 1, Source: source,
						ExtID: "f:" + time.Unix(rd[1], 0).Format("2006-01-02")}
					if err := e.addEvent(ctx, ev, &res.Events); err != nil {
						return err
					}
				}
				count, _ := strconv.ParseInt(get("readcount"), 10, 64)
				if count == 0 && status == "read" {
					count = 1
				}
				if err := e.st.SetUndatedFinishes(ctx, it.ID, user, source, count); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ---------- the "counts as finished" setting ----------

func (e *Engine) pastFinish(kind string, fraction float64) bool {
	return fraction*100 >= e.cfg.FinishPercent(kind)
}

// progressFinish records one finish when progress passes the setting. A title
// that already has a finish is left alone, so a re-read isn't counted early.
func (e *Engine) progressFinish(ctx context.Context, id int64, user, source, kind string, fraction float64, at int64, n *int) error {
	if !e.pastFinish(kind, fraction) {
		return nil
	}
	done, err := e.st.HasShelfFinish(ctx, id)
	if err != nil || done {
		return err
	}
	return e.addEvent(ctx, &store.ShelfEvent{ItemID: id, User: user, Kind: "finish", ExtID: "t:progress", At: at,
		Progress: fraction, Source: source}, n)
}

// shelfComicProgress stores a comic and its read. A comic past the setting
// counts once, and that read is replaced when the source reports it finished.
func (e *Engine) shelfComicProgress(ctx context.Context, c *store.Comic, completed bool, fraction float64, at int64, source string, n *int) error {
	past := !completed && e.pastFinish("comic", fraction)
	c.Read = completed || past
	if err := e.shelfComic(ctx, c, 0, source, n); err != nil {
		return err
	}
	if completed {
		if err := e.st.DropProgressReads(ctx, c.ID, source+"-progress"); err != nil {
			return err
		}
		return e.shelfComic(ctx, c, at, source, n)
	}
	if !past || at <= 0 {
		return nil
	}
	reads, err := e.st.ComicReadCount(ctx, c.ID)
	if err != nil || reads > 0 {
		return err
	}
	ok, err := e.st.AddComicRead(ctx, c.ID, time.Unix(at, 0), source+"-progress")
	if ok {
		*n++
	}
	return err
}

// absKind tells listening from reading: Audiobookshelf keeps ebook progress
// in its own field.
func absKind(audio, ebook float64) string {
	if audio == 0 && ebook > 0 {
		return "book"
	}
	return "audiobook"
}
