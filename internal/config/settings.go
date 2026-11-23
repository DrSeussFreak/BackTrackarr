package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AdminPasswordKey is the settings row holding the hashed sign-in password.
// It is managed by its own form, so it isn't in the Settings list.
const AdminPasswordKey = "ADMIN_PASSWORD_HASH"

// Settings saved on the Settings page live in the database and take priority
// over environment variables. Only what is needed before the database opens,
// or to recover, stays environment-only: DATA_DIR, LISTEN, AUTH_RESET,
// ENV_FILE and SETTINGS_IGNORE. Alternate API base URLs also stay there, so a
// saved key can't be pointed at another host from the web UI.

// Setting describes one value editable in the web UI.
type Setting struct {
	Key, Label, Group, Help, Default string
	Type                             string // text, secret, number, percent, bool, url, choice, tz
	Options                          []string
	Instance                         string // e.g. RADARR_2, for numbered servers
	Number                           int
}

var (
	overrideMu sync.RWMutex
	overrides  map[string]string
)

// SetOverrides installs saved settings. Unknown keys are ignored, so a saved
// row can never change an environment-only value.
func SetOverrides(saved map[string]string) {
	known := map[string]string{}
	for k, v := range saved {
		if _, ok := settingIndex()[k]; ok {
			known[k] = v
		}
	}
	overrideMu.Lock()
	overrides = known
	overrideMu.Unlock()
}

func override(k string) (string, bool) {
	overrideMu.RLock()
	defer overrideMu.RUnlock()
	v, ok := overrides[k]
	return v, ok
}

// IgnoreSaved is the recovery switch: start with SETTINGS_IGNORE=true to run
// on environment variables alone after a saved value locks you out.
func IgnoreSaved() bool { return envBool("SETTINGS_IGNORE", false) }

// Current returns the value to show for a setting and where it came from:
// "saved", "env" or "default". Booleans are normalized to true/false.
func Current(s Setting) (value, source string) {
	if v, ok := override(s.Key); ok {
		value, source = strings.TrimSpace(v), "saved"
	} else if v := strings.TrimSpace(os.Getenv(s.Key)); v != "" {
		value, source = v, "env"
	} else {
		source = "default"
	}
	if s.Type == "bool" {
		d := s.Default == "true"
		if value == "" {
			return strconv.FormatBool(d), source
		}
		return strconv.FormatBool(envBool(s.Key, d)), source
	}
	if s.Type == "choice" && value == "" {
		return s.Default, source // what the dropdown shows, so saving it is no change
	}
	return value, source
}

// Validate checks and tidies a submitted value. Empty means "use the default".
// ValidHTTPURL reports whether v is a full http or https address.
func ValidHTTPURL(v string) bool {
	u, err := url.Parse(strings.TrimSpace(v))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func Validate(s Setting, v string) (string, error) {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "\r\n\x00") {
		return "", fmt.Errorf("%s: must be a single line", s.Label)
	}
	if v == "" {
		return "", nil
	}
	if s.Key == "RELAY_URLS" || s.Key == "WEBHOOK_URLS" {
		for _, u := range strings.Split(v, ",") {
			if u = strings.TrimSpace(u); u != "" && !ValidHTTPURL(u) {
				// The value may be a token, so it is never repeated here.
				return "", fmt.Errorf("%s: enter the whole web address, starting with http:// or https://", s.Label)
			}
		}
	}
	switch s.Type {
	case "number":
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return "", fmt.Errorf("%s: must be a whole number, 0 or more", s.Label)
		}
	case "percent":
		if n, err := strconv.ParseFloat(v, 64); err != nil || n <= 0 || n > 100 {
			return "", fmt.Errorf("%s: must be between 1 and 100", s.Label)
		}
	case "bool":
		if v != "true" && v != "false" {
			return "", fmt.Errorf("%s: must be true or false", s.Label)
		}
	case "url":
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("%s: must be a full http:// or https:// address", s.Label)
		}
		v = strings.TrimRight(v, "/")
	case "tz":
		if _, err := time.LoadLocation(v); err != nil {
			return "", fmt.Errorf("%s: unknown time zone, use a name like America/Chicago", s.Label)
		}
	case "choice":
		for _, o := range s.Options {
			if v == o {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s: must be one of %s", s.Label, strings.Join(s.Options, ", "))
	}
	return v, nil
}

var (
	settingsOnce sync.Once
	settingsList []Setting
	settingsMap  map[string]Setting
)

func settingIndex() map[string]Setting {
	settingsOnce.Do(buildSettings)
	return settingsMap
}

// FromEnv returns environment values not yet saved in the app, ready to save.
// Startup uses it to move an existing install's settings into the database.
// A value the app already holds always wins, and placeholders such as
// "<your Plex token>" or invalid values are left behind.
func FromEnv(saved map[string]string) map[string]string {
	out := map[string]string{}
	for _, s := range Settings() {
		if _, ok := saved[s.Key]; ok {
			continue
		}
		v := strings.TrimSpace(os.Getenv(s.Key))
		if v == "" || (strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">")) {
			continue
		}
		if s.Type == "bool" {
			switch strings.ToLower(v) {
			case "1", "true", "yes", "on":
				v = "true"
			case "0", "false", "no", "off":
				v = "false"
			default:
				continue
			}
		}
		if clean, err := Validate(s, v); err == nil && clean != "" {
			out[s.Key] = clean
		}
	}
	return out
}

// Lookup finds a setting by name.
func Lookup(key string) (Setting, bool) {
	s, ok := settingIndex()[key]
	return s, ok
}

// Settings lists every value editable in the web UI, in display order.
func Settings() []Setting {
	settingsOnce.Do(buildSettings)
	return settingsList
}

func buildSettings() {
	add := func(group, key, label, typ, def, help string) {
		s := Setting{Key: key, Label: label, Group: group, Type: typ, Default: def, Help: help}
		if key == "LOG_LEVEL" {
			s.Options = []string{"debug", "info", "warn", "error"}
		}
		if key == "BACKUP_EVERY" {
			s.Options = BackupEveryOptions
		}
		settingsList = append(settingsList, s)
	}
	g := "General"
	add(g, "ADMIN_USER", "Sign-in username", "text", "admin", "Can't change while two-step sign-in or passkeys are on. API tokens stop working if it changes.")
	add(g, "TZ", "Time zone", "tz", "", "A name like America/Chicago. Blank uses the system's.")
	add(g, "BASE_URL", "Address of this app", "url", "", "Used for webhook links and passkeys.")
	add(g, "TRUST_PROXY", "Behind a reverse proxy", "bool", "false", "Turn on only if every request passes through your proxy.")
	add(g, "SESSION_DAYS", "Sign-in lasts (days)", "number", "30", "")
	add(g, "WEBHOOK_TOKEN", "Webhook token", "secret", "", "Changing it breaks webhook URLs already pasted into other apps.")
	add(g, "LOG_LEVEL", "Log level", "choice", "info", "")

	g = "Plex"
	add(g, "PLEX_URL", "Server address", "url", "", "As this app sees it, e.g. http://192.168.1.10:32400.")
	add(g, "PLEX_TOKEN", "Token", "secret", "", "")
	add(g, "PLEX_POLL_SECONDS", "Poll sessions every (seconds)", "number", "0", "Use 10 without Plex Pass. 0 uses webhooks.")
	add(g, "SCROBBLE_USERS", "Users sent to other services", "text", "", "Comma-separated. Blank means the server owner.")
	add(g, "PRIVATE_LIBRARIES", "Private libraries", "text", "", "Comma-separated. Stored here, never sent anywhere.")
	add(g, "PLEX_ARCHIVE_MINUTES", "Archive every (minutes)", "number", "60", "0 turns it off.")
	add(g, "PLEX_WATCH_SCAN_SECONDS", "Check for Mark as watched (seconds)", "number", "300", "0 turns it off.")
	add(g, "PLEX_ALL_USERS", "Include shared and managed users", "bool", "true", "")
	add(g, "PLEX_ALLOW_WRITES", "Allow changes to Plex", "bool", "false", "Needed only for restores and copying playlists.")

	g = "Counts as finished"
	add(g, "WATCHED_PERCENT", "Movies and TV (%)", "percent", "90", "")
	add(g, "MUSIC_FINISHED_PERCENT", "Music (%)", "percent", "90", "")
	add(g, "BOOK_FINISHED_PERCENT", "Books (%)", "percent", "90", "")
	add(g, "AUDIOBOOK_FINISHED_PERCENT", "Audiobooks (%)", "percent", "90", "")
	add(g, "COMIC_FINISHED_PERCENT", "Comics (%)", "percent", "90", "")

	g = "Trakt and Simkl"
	add(g, "TRAKT_CLIENT_ID", "Trakt client ID", "text", "", "")
	add(g, "TRAKT_CLIENT_SECRET", "Trakt client secret", "secret", "", "")
	add(g, "SIMKL_CLIENT_ID", "Simkl client ID", "text", "", "")
	add(g, "TRACKER_PULL_MINUTES", "Pull new plays every (minutes)", "number", "60", "0 turns it off.")
	add(g, "TRACKER_PULL_USER", "Local user for pulled plays", "text", "", "Blank means the Plex owner.")

	g = "Other destinations"
	add(g, "FLOPPY_URL", "Floppy address", "url", "", "")
	add(g, "FLOPPY_TOKEN", "Floppy token", "secret", "", "")
	add(g, "RELAY_URLS", "Plex webhook relays", "secret", "", "Full URLs, comma-separated. For CrossWatch, copy its whole Plex webhook URL, ending in /webhook/plexwatcher?token=...")
	add(g, "WEBHOOK_URLS", "JSON webhooks", "secret", "", "Comma-separated URLs. Hidden because they often contain tokens.")

	g = "Streaming"
	add(g, "TMDB_API_KEY", "TMDB key", "secret", "", "")
	add(g, "STREAMING_REGION", "Region", "text", "US", "Two letters, e.g. US, CA, GB.")
	add(g, "MY_STREAMING_SERVICES", "My services", "text", "", "Comma-separated, as TMDB names them.")
	add(g, "STREAMING_IMPORT_DIR", "Import folder", "text", "", "Blank uses import/streaming in the data folder.")

	g = "AI recommendations"
	add(g, "ANTHROPIC_API_KEY", "Anthropic API key", "secret", "", "")
	add(g, "CLAUDE_MODEL", "Claude model", "text", "claude-opus-5-5", "")
	add(g, "GEMINI_API_KEY", "Gemini API key", "secret", "", "")
	add(g, "GEMINI_MODEL", "Gemini model", "text", "gemini-2.5-pro", "")
	add(g, "AI_INCLUDE_ADULT", "Include adult by default", "bool", "false", "")

	g = "Comics and backups"
	add(g, "CLZ_IMPORT_DIR", "CLZ import folder", "text", "", "Blank uses import/clz in the data folder.")
	add(g, "BACKUP_EVERY", "Back up every", "choice", DefaultBackupEvery, "")
	add(g, "BACKUP_KEEP", "Backups to keep", "number", "7", "0 turns backups off.")
	add(g, "BACKUP_COMPRESS", "Compress backups", "bool", "true", "Encrypted backups can't be compressed.")

	g = "Google Drive backups"
	add(g, "GDRIVE_CLIENT_ID", "Google client ID", "text", "", "From a TVs and Limited Input client.")
	add(g, "GDRIVE_CLIENT_SECRET", "Google client secret", "secret", "", "")
	add(g, "GDRIVE_FOLDER", "Drive folder", "text", "BackTrackarr Backups", "")
	add(g, "GDRIVE_KEEP", "Backups to keep in Drive", "number", "7", "Older ones go to Drive's trash. 0 keeps all.")

	g = "History imports"
	add(g, "TAUTULLI_URL", "Tautulli address", "url", "", "")
	add(g, "TAUTULLI_API_KEY", "Tautulli API key", "secret", "", "From Settings, Web Interface, API.")
	add(g, "CROSSWATCH_URL", "CrossWatch address", "url", "", "")
	add(g, "CROSSWATCH_TOKEN", "CrossWatch API token", "secret", "", "From CrossWatch's API tokens settings.")
	add(g, "PLEKT_URL", "Plekt address", "url", "", "")
	add(g, "PLEKT_API_KEY", "Plekt API key", "secret", "", "")

	g = "Requests"
	add(g, "SEERR_URL", "Jellyseerr or Overseerr address", "url", "", "")
	add(g, "SEERR_API_KEY", "Jellyseerr or Overseerr API key", "secret", "", "From Settings, General, API Key.")

	g = "Books and audiobooks"
	add(g, "AUDIOBOOKSHELF_URL", "Audiobookshelf address", "url", "", "")
	add(g, "AUDIOBOOKSHELF_TOKEN", "Audiobookshelf API token", "secret", "", "From Settings, Users, your user. Covers that one user.")
	add(g, "KOMGA_URL", "Komga address", "url", "", "")
	add(g, "KOMGA_API_KEY", "Komga API key", "secret", "", "From Account Settings, API Keys. Comics go to the Comics page.")
	add(g, "KAVITA_URL", "Kavita address", "url", "", "")
	add(g, "KAVITA_API_KEY", "Kavita auth key", "secret", "", "From your account's Auth Keys. Needs a Kavita version with reading history.")
	add(g, "HARDCOVER_TOKEN", "Hardcover API token", "secret", "", "From hardcover.app, Settings, API. Use a read-only token if offered.")
	add(g, "OPENLIBRARY_USER", "Open Library username", "text", "", "Your reading log must be set to public.")
	add(g, "BOOKS_IMPORT_DIR", "Book export folder", "text", "", "Blank uses import/books in the data folder.")

	g = "Games"
	add(g, "STEAM_API_KEY", "Steam Web API key", "secret", "", "From steamcommunity.com/dev/apikey.")
	add(g, "STEAM_ID", "SteamID64", "text", "", "The 17-digit id. Game details must be visible to the key.")
	add(g, "RETROACHIEVEMENTS_USER", "RetroAchievements username", "text", "", "")
	add(g, "RETROACHIEVEMENTS_API_KEY", "RetroAchievements web API key", "secret", "", "From your account's Settings, Keys.")
	add(g, "ROMM_URL", "RomM address", "url", "", "")
	add(g, "ROMM_TOKEN", "RomM client API token", "secret", "", "Needs read access to ROMs and user ROM data.")
	add(g, "GAMES_IMPORT_DIR", "Game export folder", "text", "", "Blank uses import/games in the data folder.")

	g = "Book and game sync"
	add(g, "SHELF_SYNC_MINUTES", "Read sources every (minutes)", "number", "180", "0 turns it off. The buttons on Books and Games still work.")
	add(g, "SHELF_USER", "Local user for reads and plays", "text", "", "Blank means the Plex owner.")

	add("Sonarr, Radarr, Whisparr", "ARR_ARCHIVE_HOURS", "Archive every (hours)", "number", "12", "")
	for _, k := range []struct{ prefix, name, group string }{
		{"SONARR", "Sonarr", "Sonarr, Radarr, Whisparr"}, {"RADARR", "Radarr", "Sonarr, Radarr, Whisparr"}, {"WHISPARR", "Whisparr", "Sonarr, Radarr, Whisparr"},
		{"JELLYFIN", "Jellyfin", "Jellyfin and Emby"}, {"EMBY", "Emby", "Jellyfin and Emby"},
	} {
		arr := k.group != "Jellyfin and Emby"
		for n := 1; n <= 9; n++ {
			p, title := k.prefix, k.name
			if n > 1 {
				p += "_" + strconv.Itoa(n)
				title += " " + strconv.Itoa(n)
			}
			inst := func(suffix, label, typ, help string) {
				settingsList = append(settingsList, Setting{Key: p + "_" + suffix, Label: title + " " + label, Group: k.group,
					Type: typ, Help: help, Instance: k.prefix, Number: n})
			}
			inst("URL", "address", "url", "")
			inst("API_KEY", "API key", "secret", "")
			inst("NAME", "name", "text", "Optional label, e.g. radarr-4k.")
			if arr {
				if k.prefix != "WHISPARR" {
					settingsList = append(settingsList, Setting{Key: p + "_ADULT", Label: title + " is adult", Group: k.group,
						Type: "bool", Default: "false", Instance: k.prefix, Number: n})
				}
				inst("ROOT_FOLDER", "root folder", "text", "Blank uses its first root folder.")
				inst("QUALITY_PROFILE", "quality profile", "text", "Name or id. Blank uses its first profile.")
			}
		}
	}
	settingsMap = make(map[string]Setting, len(settingsList))
	for _, s := range settingsList {
		settingsMap[s.Key] = s
	}
}
