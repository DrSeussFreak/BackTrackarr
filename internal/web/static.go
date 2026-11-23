package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// Embedded files have no modified time, so browsers can't tell when an update
// changed them. Each file gets a hash of its contents: pages link to it, and
// responses carry it, so an update always reaches the browser.

var (
	assetHashMu sync.Mutex
	assetHashes = map[string]string{}
)

func assetHash(name string) string {
	assetHashMu.Lock()
	defer assetHashMu.Unlock()
	if h, ok := assetHashes[name]; ok {
		return h
	}
	h := ""
	if b, err := assets.ReadFile("static/" + name); err == nil {
		sum := sha256.Sum256(b)
		h = hex.EncodeToString(sum[:])[:12]
	}
	assetHashes[name] = h
	return h
}

// assetURL is the versioned link to a file in static/.
func assetURL(name string) string {
	if h := assetHash(name); h != "" {
		return "/static/" + name + "?v=" + h
	}
	return "/static/" + name
}

func staticFiles() http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := assetHash(strings.TrimPrefix(r.URL.Path, "/static/")); h != "" {
			w.Header().Set("ETag", `"`+h+`"`)
			w.Header().Set("Cache-Control", "no-cache") // reuse only after checking the ETag
		}
		files.ServeHTTP(w, r)
	})
}
