package core

import (
	"context"
	"fmt"
	"html"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Audible has no public API, and BackTrackarr never stores an Amazon login.
// Two file routes are read here:
//
//   - Audible Library Extractor CSV (browser extension). Its "Raw data",
//     "Google Sheets" and "Goodreads" modes use the column names in
//     audibleColumns, taken from the extension's source.
//   - Amazon's "Request My Data" Audible files. Amazon doesn't document these,
//     so columns are matched by name and every original column is kept.
//
// A library file says whether a title is finished but not when, so a finish
// from it is stored as one undated read. Only listening files have dates.

const audibleSource = "audible"

var audibleColumns = map[string][]string{
	"title":     {"title", "producttitle", "productname", "booktitle", "name"},
	"author":    {"authors", "author", "authorname"},
	"narrators": {"narrators", "narrator"},
	"series":    {"series"},
	"asin":      {"asin", "productasin", "audibleasin"},
	"isbn13":    {"isbn13"},
	"isbn10":    {"isbn10", "isbn"},
	"length":    {"length", "runtime", "booklength"},
	"progress":  {"progress"},
	"myrating":  {"myrating"},
	"year":      {"releasedate", "yearpublished", "publicationdate"},
	"shelves":   {"bookshelves"},
	"date":      {"startdate", "eventdate", "listeningdate", "listendate", "starttime", "eventstarttime", "eventtimestamp", "timestamp", "date"},
	"duration": {"eventdurationmilliseconds", "durationmilliseconds", "listeningdurationmilliseconds", "listeningtimemilliseconds",
		"listentimemilliseconds", "durationms", "minuteslistened", "durationminutes", "listeningtimeminutes", "durationseconds",
		"listeningtime", "listentime", "eventduration", "duration"},
}

// audibleKind says whether a file is an Audible library or listening file.
// name is the file's name or path; "" means the user chose the Books page.
func audibleKind(header []string, name string) string {
	has := map[string]bool{}
	for _, h := range header {
		has[normHeader(h)] = true
	}
	if has["exclusiveshelf"] || has["bookid"] || has["readstatus"] {
		return "" // a real Goodreads or StoryGraph export
	}
	if has["narrators"] && (has["progress"] || has["asin"]) {
		return "library"
	}
	if has["bookshelves"] && has["asin"] && has["yearpublished"] && has["averagerating"] {
		return "library" // the extension's Goodreads mode
	}
	col := shelfColumns(header, audibleColumns)
	_, asin := col["asin"]
	_, title := col["title"]
	_, date := col["date"]
	_, dur := col["duration"]
	lower := strings.ToLower(name)
	if name == "" {
		if asin && date && dur {
			return "listening"
		}
		return ""
	}
	if !strings.Contains(lower, "audible") {
		return ""
	}
	for _, skip := range []string{"purchase", "order", "credit", "membership", "billing", "payment", "customer", "return"} {
		if strings.Contains(lower, skip) {
			return "" // owning or buying something isn't listening to it
		}
	}
	if (asin || title) && date && dur {
		return "listening"
	}
	if asin && title {
		return "library"
	}
	return ""
}

var (
	audibleHours = regexp.MustCompile(`(?i)(\d+)\s*h(?:rs?|ours?)?\b`)
	audibleMins  = regexp.MustCompile(`(?i)(\d+)\s*m(?:ins?|inutes?)?\b`)
	audibleLink  = regexp.MustCompile(`(?is)^=HYPERLINK\(\s*".*?"\s*[,;]\s*"(.*)"\s*\)$`)
)

// audibleSeconds reads "33 hrs and 45 mins", "9h 10m left" or "<1 min left".
func audibleSeconds(s string) int64 {
	s = html.UnescapeString(s)
	var total int64
	if m := audibleHours.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		total += n * 3600
	}
	if m := audibleMins.FindStringSubmatch(s); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		total += n * 60
	}
	return total
}

// audibleProgress returns a status and a 0-1 progress from the Progress column.
func audibleProgress(p string, length int64) (string, float64) {
	p = strings.TrimSpace(html.UnescapeString(p))
	switch low := strings.ToLower(p); {
	case low == "" || low == "0":
		return "to-read", 0
	case strings.Contains(low, "finished"):
		return "read", 1
	default:
		if length > 0 {
			left := audibleSeconds(p)
			if strings.Contains(low, "<") {
				left = 30
			}
			if left > 0 {
				return "reading", math.Max(0, math.Min(1, 1-float64(left)/float64(length)))
			}
		}
		return "reading", 0
	}
}

func audibleShelfStatus(shelves string) string {
	for _, s := range strings.Fields(strings.ToLower(shelves)) {
		switch s {
		case "read":
			return "read"
		case "currently-reading":
			return "reading"
		case "to-read":
			return "to-read"
		}
	}
	return ""
}

func audibleDuration(v, header string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	h := normHeader(header)
	if f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64); err == nil {
		switch {
		case strings.Contains(h, "millisecond") || strings.HasSuffix(h, "ms"):
			return int64(f / 1000)
		case strings.Contains(h, "minute"):
			return int64(f * 60)
		case strings.Contains(h, "second"):
			return int64(f)
		}
		return int64(f) // no unit in the name: seconds
	}
	if d := parseDuration(v, h); d > 0 {
		return int64(d.Seconds())
	}
	return audibleSeconds(v)
}

func audibleCell(s string) string {
	s = strings.TrimSpace(s)
	if m := audibleLink.FindStringSubmatch(s); m != nil { // Google Sheets mode
		s = strings.ReplaceAll(m[1], `""`, `"`)
	}
	return strings.TrimSpace(html.UnescapeString(s))
}

// importAudibleRows stores an Audible library or listening file. Re-importing
// is safe. A title that is being listened to again keeps its earlier finish.
func (e *Engine) importAudibleRows(ctx context.Context, header []string, next func() ([]string, error), kind string) (*ShelfImportResult, error) {
	col := shelfColumns(header, audibleColumns)
	if _, ok := col["title"]; !ok {
		if _, ok := col["asin"]; !ok {
			return nil, fmt.Errorf("no Title or ASIN column found (columns: %s)", strings.Join(header, ", "))
		}
	}
	user := e.shelfUser(ctx)
	res := &ShelfImportResult{Source: audibleSource}
	durHeader := ""
	if i, ok := col["duration"]; ok {
		durHeader = header[i]
	}
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
						return audibleCell(rec[i])
					}
					return ""
				}
				title, asin := get("title"), strings.ToUpper(get("asin"))
				if title == "" && asin == "" {
					continue
				}
				res.Rows++
				raw := map[string]string{}
				for i, h := range header {
					if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
						raw[strings.TrimSpace(h)] = rec[i]
					}
				}
				isbn := strings.TrimLeft(cleanISBN(get("isbn13")), "'")
				if isbn == "" {
					isbn = strings.TrimLeft(cleanISBN(get("isbn10")), "'")
				}
				key := "asin:" + asin
				if asin == "" {
					key = hashKey(title, get("author"))
					if isbn != "" {
						key = "isbn:" + isbn
					}
				}
				if title == "" {
					// A listening row with only an ASIN must not rename a title
					// that a library import already stored.
					if title, err = e.st.ShelfTitle(ctx, audibleSource, key); err != nil {
						return err
					}
					if title == "" {
						title = "Audible " + asin
					}
				}
				it := &store.ShelfItem{MediaType: "audiobook", Source: audibleSource, ExtKey: key, Title: title, Creator: get("author"),
					Series: get("series"), ISBN: isbn, ASIN: asin, Year: shelfYear(get("year")), Raw: shelfRaw(raw)}
				it.DurationS = audibleSeconds(get("length"))
				it.Rating, _ = strconv.ParseFloat(get("myrating"), 64)

				var at, listened int64
				if kind == "listening" {
					at = uDate(get("date"))
					listened = audibleDuration(get("duration"), durHeader)
					it.LastActivity = at
				} else {
					_, hasProgress := col["progress"]
					switch {
					case hasProgress:
						it.Status, it.Progress = audibleProgress(get("progress"), it.DurationS)
					default:
						it.Status = audibleShelfStatus(get("shelves"))
						if it.Status == "read" {
							it.Progress = 1
						}
					}
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
				if kind == "listening" {
					if at > 0 && listened > 0 {
						ev := &store.ShelfEvent{ItemID: it.ID, User: user, Kind: "session", At: at, DurationS: listened, Source: audibleSource,
							ExtID: fmt.Sprintf("l:%d:%d", at, listened), Raw: shelfRaw(raw)}
						if err := e.addEvent(ctx, ev, &res.Events); err != nil {
							return err
						}
					}
					continue
				}
				// Only ever add a finish: "9h left" on a finished book is a re-listen.
				if it.Status == "read" {
					if err := e.st.SetUndatedFinishes(ctx, it.ID, user, audibleSource, 1); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return res, err
		}
	}
	return res, nil
}
