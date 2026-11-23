package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// CLZ Comics exports a CSV (or tab/semicolon separated text) with whichever
// columns you picked. Known columns are matched loosely by name; every
// column is kept in the comic's raw data regardless.

var clzColumns = map[string][]string{
	"key":       {"comicid", "coreid", "clzcomicid", "bpcomicid", "id", "itemid"},
	"copy":      {"id", "itemid"}, // one owned copy; the key above can be a catalog id
	"series":    {"series", "seriesname", "seriestitle"},
	"issue":     {"issue", "issuenr", "issueno", "issuenumber", "nr", "number"},
	"variant":   {"variant", "variantdescription", "edition"},
	"title":     {"title", "issuetitle", "storytitle", "storyarc"},
	"publisher": {"publisher"},
	"release":   {"releasedate", "publicationdate", "pubdate", "storedate", "onsaledate"},
	"cover":     {"coverdate"},
	"barcode":   {"barcode", "upc", "isbn", "ean"},
	"format":    {"format"},
	"read":      {"read", "readit", "isread", "readstatus"},
	"readdate":  {"readdate", "dateread", "lastreaddate", "readon"},
	"rating":    {"myrating", "rating", "personalrating"},
}

func normHeader(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func detectComma(head []byte) rune {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	best, n := ',', bytes.Count(line, []byte(","))
	for _, c := range []rune{'\t', ';'} {
		if k := bytes.Count(line, []byte(string(c))); k > n {
			best, n = c, k
		}
	}
	return best
}

var dateLayouts = []string{"2006-01-02", "2006/01/02", "01/02/2006", "1/2/2006", "02-01-2006", "Jan 02, 2006", "Jan 2, 2006",
	"January 2, 2006", "02 Jan 2006", "2 Jan 2006", "Jan 2006", "January 2006", "2006-01", "2006",
	"2006-01-02 15:04:05", "01/02/2006 15:04", "1/2/2006 15:04"}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			if t.Year() < 1800 || t.Year() > 2200 {
				return time.Time{}, false
			}
			if l == "2006" || l == "Jan 2006" || l == "January 2006" || l == "2006-01" {
				return t, true // month/year precision
			}
			return t.Add(12 * time.Hour), true // noon, so time zones don't move the day
		}
	}
	return time.Time{}, false
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "y", "true", "1", "read", "x", "✓":
		return true
	}
	return false
}

type ClzResult struct {
	Rows, New, Updated, Reads int
}

// ImportCLZ reads one CLZ export. Re-importing later is safe: comics are
// matched by their CLZ id (or series/issue/variant/barcode), and a new read
// date is added as another read, so rereads are kept.
func (e *Engine) ImportCLZ(ctx context.Context, r io.Reader, source string) (*ClzResult, error) {
	return e.ImportCLZProgress(ctx, r, source, func(string) {})
}

// ImportCLZProgress is ImportCLZ with progress messages for a background job.
func (e *Engine) ImportCLZProgress(ctx context.Context, r io.Reader, source string, report func(string)) (*ClzResult, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(4096)
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	if b, _ := br.Peek(3); bytes.Equal(b, []byte("\xef\xbb\xbf")) {
		br.Discard(3)
	}
	if bytes.HasPrefix(bytes.TrimSpace(head), []byte("<")) {
		return e.importCLZXML(ctx, br, source, report)
	}
	cr := csv.NewReader(br)
	cr.Comma = detectComma(head)
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		n := normHeader(h)
		for field, names := range clzColumns {
			if _, done := col[field]; done {
				continue
			}
			for _, name := range names {
				if n == name {
					col[field] = i
				}
			}
		}
	}
	if _, ok := col["series"]; !ok {
		if _, ok := col["title"]; !ok {
			return nil, fmt.Errorf("no Series or Title column found (columns: %s)", strings.Join(header, ", "))
		}
	}
	res := &ClzResult{}
	line := 1
	err = e.clzBatches(ctx, func() (*clzRow, int, error) {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil, 0, io.EOF
		}
		line++
		if err != nil {
			return nil, 0, fmt.Errorf("row %d: %w", line, err)
		}
		raw := map[string]string{}
		size := 0
		for i, h := range header {
			if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
				raw[strings.TrimSpace(h)] = rec[i]
				size += len(h) + len(rec[i])
			}
		}
		if len(raw) == 0 {
			return nil, 0, nil
		}
		get := func(f string) string {
			if i, ok := col[f]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		return &clzRow{get: get, raw: raw}, size, nil
	}, source, res, report)
	return res, err
}

const (
	clzBatchRows  = 100
	clzBatchBytes = 4 << 20
)

// clzRow is one parsed comic waiting to be saved.
type clzRow struct {
	get func(string) string
	raw map[string]string
}

// clzBatches parses a small batch with no lock held, then saves it in one
// short transaction, so a large file never blocks other writers for long.
// If the file breaks partway, comics before that point are kept.
func (e *Engine) clzBatches(ctx context.Context, next func() (*clzRow, int, error), source string, res *ClzResult, report func(string)) error {
	for {
		batch := make([]*clzRow, 0, clzBatchRows)
		size := 0
		var readErr error
		for len(batch) < clzBatchRows && size < clzBatchBytes {
			row, n, err := next()
			if err != nil {
				readErr = err
				break
			}
			if row != nil {
				batch = append(batch, row)
				size += n
			}
		}
		if len(batch) > 0 {
			if err := e.st.InTx(ctx, func(ctx context.Context) error {
				saved := *res
				for _, row := range batch {
					if err := e.clzStore(ctx, row.get, row.raw, source, res); err != nil {
						*res = saved // rolled back, so don't count it
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			report(fmt.Sprintf("%d comics saved", res.Rows))
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// clzStore saves one comic from either a CSV row or an XML record.
func (e *Engine) clzStore(ctx context.Context, get func(string) string, raw map[string]string, source string, res *ClzResult) error {
	res.Rows++
	c := &store.Comic{Series: get("series"), Issue: get("issue"), Variant: get("variant"), Title: get("title"),
		Publisher: get("publisher"), ReleaseDate: get("release"), CoverDate: get("cover"), Barcode: get("barcode"),
		Format: get("format"), Raw: raw}
	c.ExtKey = get("key")
	if c.ExtKey == "" {
		h := sha1.Sum([]byte(strings.ToLower(strings.Join([]string{c.Series, c.Issue, c.Variant, c.Barcode, c.Title}, "|"))))
		c.ExtKey = "h:" + hex.EncodeToString(h[:10])
	} else {
		base := c.ExtKey
		c.ExtKey = "clz:" + base
		if id := get("copy"); id != "" && id != base {
			key, err := e.st.ComicCopyKey(ctx, c.ExtKey, id)
			if err != nil {
				return err
			}
			c.ExtKey = key
		}
	}
	readAt, hasDate := clzDate(get("readdate"))
	c.Read = truthy(get("read")) || hasDate
	c.ReadKnown = get("read") != "" // the export says either way, so it can also clear a wrong value
	if v, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(get("rating"), "/")[0]), 64); err == nil {
		c.Rating = v
	}
	isNew, err := e.st.UpsertComic(ctx, c)
	if err != nil {
		return err
	}
	if isNew {
		res.New++
	} else {
		res.Updated++
	}
	if hasDate {
		if ok, err := e.st.AddComicRead(ctx, c.ID, readAt, source); err != nil {
			return err
		} else if ok {
			res.Reads++
		}
	}
	return nil
}

// RunCLZWatcher imports any export dropped into CLZ_IMPORT_DIR, then moves
// it to an "imported" subfolder. Checks hourly.
func (e *Engine) RunCLZWatcher(ctx context.Context) {
	dir := e.cfg.ClzImportDir
	if err := os.MkdirAll(filepath.Join(dir, "imported"), 0o755); err != nil {
		e.log.Warn("clz import folder", "err", err)
		return
	}
	t := time.NewTimer(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f))
			if ext != ".csv" && ext != ".txt" && ext != ".tsv" && ext != ".xml" {
				continue
			}
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			res, err := e.ImportCLZ(ctx, fh, "clz:"+filepath.Base(f))
			fh.Close()
			if err != nil {
				e.log.Warn("clz import", "file", f, "err", err)
				continue
			}
			e.log.Info("clz import", "file", filepath.Base(f), "rows", res.Rows, "new", res.New, "reads", res.Reads)
			dst := filepath.Join(dir, "imported", time.Now().Format("20060102-150405-")+filepath.Base(f))
			_ = os.Rename(f, dst)
		}
		t.Reset(time.Hour)
	}
}
