package core

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// CLZ's XML layout isn't published, so it's read by meaning. Each <comic> is
// flattened to paths such as mainsection.series.displayname, every value is
// kept in the raw data, and known fields are matched by name.

const (
	clzMaxDepth  = 32
	clzMaxFields = 5000
	clzMaxValue  = 64 << 10
)

// Sections describing people or contents, whose names and titles aren't the
// comic's own.
var clzIgnored = []string{"cred", "creator", "character", "stor", "link", "cover", "image", "team", "arc", "tag"}

func xmlCharset(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
		return &latin1Reader{r: in}, nil
	}
	return nil, fmt.Errorf("unsupported text encoding %q", label)
}

// latin1Reader converts single-byte Western text to UTF-8.
type latin1Reader struct {
	r   io.Reader
	out []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	if len(l.out) == 0 {
		buf := make([]byte, 4096)
		n, err := l.r.Read(buf)
		if n == 0 {
			return 0, err
		}
		l.out = make([]byte, 0, n*2)
		for _, b := range buf[:n] {
			l.out = utf8.AppendRune(l.out, rune(b))
		}
	}
	n := copy(p, l.out)
	l.out = l.out[n:]
	return n, nil
}

// readXMLComic flattens one <comic> whose start tag was just read.
func readXMLComic(dec *xml.Decoder) (map[string]string, error) {
	out := map[string]string{}
	var path []string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(path) >= clzMaxDepth {
				return nil, errors.New("XML is nested too deeply")
			}
			path = append(path, strings.ToLower(t.Name.Local))
			text.Reset()
			for _, a := range t.Attr {
				if v := strings.TrimSpace(a.Value); v != "" && len(out) < clzMaxFields {
					out[strings.Join(path, ".")+"."+strings.ToLower(a.Name.Local)] = v
				}
			}
		case xml.CharData:
			if text.Len() < clzMaxValue {
				text.Write(t)
			}
		case xml.EndElement:
			if len(path) == 0 {
				return out, nil // the end of </comic>
			}
			if v := strings.TrimSpace(text.String()); v != "" && len(out) < clzMaxFields {
				k := strings.Join(path, ".")
				if old, ok := out[k]; ok {
					if len(old) < clzMaxValue {
						out[k] = old + " | " + v // repeated elements, e.g. several creators
					}
				} else {
					out[k] = v
				}
			}
			text.Reset()
			path = path[:len(path)-1]
		}
	}
}

// clzPick finds a field: at the top level, then as a named value inside it
// (series.displayname, readdate.date), then deeper.
func clzPick(paths []string, flat map[string]string, names []string, topOnly bool) string {
	inner := []string{"displayname", "name", "value", "date", "displaydate", "text", "timestamp"}
	isName := func(s string) bool {
		s = normHeader(s)
		for _, n := range names {
			if s == n {
				return true
			}
		}
		return false
	}
	for _, p := range paths {
		if !strings.Contains(p, ".") && isName(p) {
			return flat[p]
		}
	}
	if topOnly {
		return ""
	}
	for _, sub := range inner {
		for _, p := range paths {
			parts := strings.Split(p, ".")
			if len(parts) == 2 && isName(parts[0]) && parts[1] == sub {
				return flat[p]
			}
		}
	}
	for _, p := range paths { // paths are sorted shallowest first
		parts := strings.Split(p, ".")
		if len(parts) < 2 || clzSkipped(parts[:len(parts)-1]) {
			continue
		}
		last := parts[len(parts)-1]
		if isName(last) {
			return flat[p]
		}
		for _, sub := range inner {
			if last == sub && isName(parts[len(parts)-2]) {
				return flat[p]
			}
		}
	}
	return ""
}

func clzSkipped(parts []string) bool {
	for _, part := range parts {
		for _, bad := range clzIgnored {
			if strings.Contains(part, bad) {
				return true
			}
		}
	}
	return false
}

// clzDate also accepts Unix timestamps, which XML exports can use.
func clzDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 100000000 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(n, 0), true
	}
	return parseDate(s)
}

func (e *Engine) importCLZXML(ctx context.Context, r io.Reader, source string, report func(string)) (*ClzResult, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.CharsetReader = xmlCharset
	res := &ClzResult{}
	next := func() (map[string]string, error) {
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if st, ok := tok.(xml.StartElement); ok && strings.EqualFold(st.Name.Local, "comic") {
				return readXMLComic(dec)
			}
		}
	}
	seen := 0
	err := e.clzBatches(ctx, func() (*clzRow, int, error) {
		flat, err := next()
		if err == io.EOF {
			return nil, 0, io.EOF
		}
		seen++
		if err != nil {
			return nil, 0, fmt.Errorf("comic %d: %w", seen, err)
		}
		if len(flat) == 0 {
			return nil, 0, nil
		}
		size := 0
		paths := make([]string, 0, len(flat))
		for p, v := range flat {
			paths = append(paths, p)
			size += len(p) + len(v)
		}
		sort.Slice(paths, func(a, b int) bool {
			da, db := strings.Count(paths[a], "."), strings.Count(paths[b], ".")
			if da != db {
				return da < db
			}
			return paths[a] < paths[b]
		})
		// Worked out now, so no matching happens while the write lock is held.
		vals := make(map[string]string, len(clzColumns))
		for f, names := range clzColumns {
			vals[f] = clzPick(paths, flat, names, f == "key")
		}
		if vals["series"] == "" && vals["title"] == "" {
			return nil, 0, nil
		}
		return &clzRow{get: func(f string) string { return vals[f] }, raw: flat}, size, nil
	}, source, res, report)
	if err != nil {
		return res, err
	}
	if res.Rows == 0 {
		return res, errors.New("no comics found in this XML file")
	}
	return res, nil
}
