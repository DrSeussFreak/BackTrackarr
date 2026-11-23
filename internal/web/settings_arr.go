package web

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/core"
)

var arrPrefix = regexp.MustCompile(`^(SONARR|RADARR|WHISPARR)(_[2-9])?$`)

func savedSetting(key string) string {
	for _, c := range config.Settings() {
		if c.Key == key {
			v, _ := config.Current(c)
			return v
		}
	}
	return ""
}

// settingsArrOptions reads root folders and quality profiles from an app, so
// Settings can offer them as choices. A saved API key is only ever sent to the
// saved address, so changing the address can't hand the key to another server.
func (s *Server) settingsArrOptions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	prefix := r.FormValue("prefix")
	addr := strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/")
	key := strings.TrimSpace(r.FormValue("key"))
	w.Header().Set("Cache-Control", "no-store")
	if !arrPrefix.MatchString(prefix) {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	if !config.ValidHTTPURL(addr) {
		writeJSON(w, M{"error": "Enter the app's full address first."})
		return
	}
	if key == "" {
		if strings.TrimRight(savedSetting(prefix+"_URL"), "/") != addr {
			writeJSON(w, M{"error": "Enter the API key for this new address."})
			return
		}
		if key = savedSetting(prefix + "_API_KEY"); key == "" {
			writeJSON(w, M{"error": "Enter the API key first."})
			return
		}
	}
	kind, _, _ := strings.Cut(strings.ToLower(prefix), "_")
	roots, profiles, err := core.ArrOptions(r.Context(), kind, addr, key)
	if err != nil {
		writeJSON(w, M{"error": "Couldn't connect. Check the address and API key."})
		return
	}
	writeJSON(w, M{"roots": roots, "profiles": profiles})
}
