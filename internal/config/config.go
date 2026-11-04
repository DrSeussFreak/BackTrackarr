// Package config loads all settings from environment variables.
package config

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen        string
	DataDir       string
	BaseURL       string
	AdminUser     string
	AdminPassword string
	WebhookToken  string
	LogLevel      slog.Level

	// A play counts as "watched" once progress reaches this percentage.
	WatchedPercent float64

	PlexURL   string
	PlexToken string
	// If > 0, poll Plex /status/sessions instead of (or in addition to) webhooks.
	// Use this when you don't have Plex Pass.
	PlexPoll time.Duration

	// Plex usernames whose plays are sent to external services.
	// Empty = only the server owner.
	ScrobbleUsers []string

	TraktClientID     string
	TraktClientSecret string
	SimklClientID     string
	FloppyURL         string
	FloppyToken       string
	RelayURLs         []string
	WebhookURLs       []string

	BackupKeep int

	// Plex archive and tools. Reelog only reads from Plex unless PlexAllowWrites.
	PlexAllowWrites  bool
	PlexArchiveEvery time.Duration // incremental archive of metadata, playlists, collections
	PlexWatchScan    time.Duration // detects "mark as watched" presses
	PlexAllUsers     bool          // also archive and scan shared/managed users
	PlexTVURL        string

	// Libraries (by name) whose plays are stored but never sent anywhere,
	// e.g. adult libraries. Also left out of AI recommendations by default.
	PrivateLibraries []string

	ClzImportDir string

	Arr             []ArrInstance
	MediaServers    []MediaServer // Jellyfin / Emby targets for playlist copies
	ArrArchiveEvery time.Duration

	AnthropicKey   string
	AnthropicModel string
	AnthropicURL   string
	GeminiKey      string
	GeminiModel    string
	GeminiURL      string
	AIIncludeAdult bool
}

// MediaServer is a Jellyfin or Emby server that playlists can be copied to.
type MediaServer struct {
	Kind   string // jellyfin | emby
	Name   string
	URL    string
	APIKey string
}

// ArrInstance is one Sonarr, Radarr or Whisparr server.
type ArrInstance struct {
	Kind           string // sonarr | radarr | whisparr
	Name           string // unique, e.g. "sonarr", "radarr-4k"
	URL            string
	APIKey         string
	Adult          bool
	RootFolder     string // for adding titles; default: the first root folder
	QualityProfile string // name or id; default: the first profile
}

func Load() *Config {
	c := &Config{
		Listen:            env("LISTEN", ":8787"),
		DataDir:           env("DATA_DIR", "./data"),
		BaseURL:           strings.TrimRight(env("BASE_URL", ""), "/"),
		AdminUser:         env("ADMIN_USER", "admin"),
		AdminPassword:     env("ADMIN_PASSWORD", ""),
		WebhookToken:      env("WEBHOOK_TOKEN", ""),
		WatchedPercent:    envFloat("WATCHED_PERCENT", 80),
		PlexURL:           strings.TrimRight(env("PLEX_URL", ""), "/"),
		PlexToken:         env("PLEX_TOKEN", ""),
		PlexPoll:          time.Duration(envInt("PLEX_POLL_SECONDS", 0)) * time.Second,
		ScrobbleUsers:     list("SCROBBLE_USERS"),
		TraktClientID:     env("TRAKT_CLIENT_ID", ""),
		TraktClientSecret: env("TRAKT_CLIENT_SECRET", ""),
		SimklClientID:     env("SIMKL_CLIENT_ID", ""),
		FloppyURL:         strings.TrimRight(env("FLOPPY_URL", ""), "/"),
		FloppyToken:       env("FLOPPY_TOKEN", ""),
		RelayURLs:         list("RELAY_URLS"),
		WebhookURLs:       list("WEBHOOK_URLS"),
		BackupKeep:        envInt("BACKUP_KEEP", 7),
		PlexAllowWrites:   envBool("PLEX_ALLOW_WRITES", false),
		PlexArchiveEvery:  time.Duration(envInt("PLEX_ARCHIVE_MINUTES", 60)) * time.Minute,
		PlexWatchScan:     time.Duration(envInt("PLEX_WATCH_SCAN_SECONDS", 300)) * time.Second,
		PlexAllUsers:      envBool("PLEX_ALL_USERS", true),
		PlexTVURL:         strings.TrimRight(env("PLEXTV_URL", "https://plex.tv"), "/"),
		PrivateLibraries:  list("PRIVATE_LIBRARIES"),
		ArrArchiveEvery:   time.Duration(envInt("ARR_ARCHIVE_HOURS", 12)) * time.Hour,
		AnthropicKey:      env("ANTHROPIC_API_KEY", ""),
		AnthropicModel:    env("CLAUDE_MODEL", "claude-opus-5-5"),
		AnthropicURL:      strings.TrimRight(env("ANTHROPIC_BASE_URL", "https://api.anthropic.com"), "/"),
		GeminiKey:         env("GEMINI_API_KEY", ""),
		GeminiModel:       env("GEMINI_MODEL", "gemini-2.5-pro"),
		GeminiURL:         strings.TrimRight(env("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com"), "/"),
		AIIncludeAdult:    envBool("AI_INCLUDE_ADULT", false),
	}
	c.ClzImportDir = env("CLZ_IMPORT_DIR", c.DataDir+"/import/clz")
	c.Arr = loadArr()
	c.MediaServers = loadMediaServers()
	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		c.LogLevel = slog.LevelInfo
	}
	if c.PlexPoll > 0 && c.PlexPoll < 5*time.Second {
		c.PlexPoll = 5 * time.Second
	}
	return c
}

func envBool(k string, def bool) bool {
	switch strings.ToLower(env(k, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// loadMediaServers reads JELLYFIN_URL / JELLYFIN_API_KEY (and JELLYFIN_2_..9)
// and the same for EMBY. Optional: _NAME.
func loadMediaServers() []MediaServer {
	var out []MediaServer
	for _, kind := range []string{"jellyfin", "emby"} {
		for n := 1; n <= 9; n++ {
			p := strings.ToUpper(kind) + "_"
			name := kind
			if n > 1 {
				p += strconv.Itoa(n) + "_"
				name += "-" + strconv.Itoa(n)
			}
			if u := strings.TrimRight(env(p+"URL", ""), "/"); u != "" {
				out = append(out, MediaServer{Kind: kind, Name: strings.ToLower(env(p+"NAME", name)), URL: u, APIKey: env(p+"API_KEY", "")})
			}
		}
	}
	return out
}

// loadArr reads SONARR_URL / SONARR_API_KEY (and SONARR_2_URL ... SONARR_9_URL),
// the same for RADARR and WHISPARR. Optional per instance: _NAME, _ADULT,
// _ROOT_FOLDER, _QUALITY_PROFILE. Whisparr instances are always adult.
func loadArr() []ArrInstance {
	var out []ArrInstance
	for _, kind := range []string{"sonarr", "radarr", "whisparr"} {
		for n := 1; n <= 9; n++ {
			p := strings.ToUpper(kind) + "_"
			if n > 1 {
				p += strconv.Itoa(n) + "_"
			}
			u := strings.TrimRight(env(p+"URL", ""), "/")
			if u == "" {
				continue
			}
			name := kind
			if n > 1 {
				name += "-" + strconv.Itoa(n)
			}
			out = append(out, ArrInstance{Kind: kind, Name: strings.ToLower(env(p+"NAME", name)), URL: u,
				APIKey: env(p+"API_KEY", ""), Adult: kind == "whisparr" || envBool(p+"ADULT", false),
				RootFolder: env(p+"ROOT_FOLDER", ""), QualityProfile: env(p+"QUALITY_PROFILE", "")})
		}
	}
	return out
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil {
		return n
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if n, err := strconv.ParseFloat(env(k, ""), 64); err == nil {
		return n
	}
	return def
}

func list(k string) []string {
	var out []string
	for _, p := range strings.Split(env(k, ""), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
