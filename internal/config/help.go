package config

import "strings"

// Info explains a setting in plain words: what it does, what needs it, and
// how much it matters, so unneeded ones are easy to skip.
type Info struct {
	What   string
	Needed string
	Level  string // Essential, Recommended, Optional or Advanced
}

const (
	essential   = "Essential"
	recommended = "Recommended"
	optional    = "Optional"
	advanced    = "Advanced"
)

var settingInfo = map[string]Info{
	"ADMIN_USER":    {"The username you sign in with.", "Signing in. The default, admin, is fine.", optional},
	"TZ":            {"Your local time zone.", "Correct times on history, logs and daily backups.", recommended},
	"BASE_URL":      {"The address you open this app at.", "Passkeys, secure sign-in and the webhook links it shows you.", essential},
	"TRUST_PROXY":   {"Tells the app it sits behind a proxy such as SWAG.", "Passkeys and login lockouts behind a proxy. Leave off if you connect directly.", essential},
	"SESSION_DAYS":  {"How long you stay signed in since your last visit.", "Nothing. Skip unless you want longer or shorter sign-ins.", optional},
	"WEBHOOK_TOKEN": {"The private part of your webhook links, like a password for them.", "Plex, Jellyfin and Emby webhooks and the browser scrobbler. Leave blank and one is made for you. Change it only if a link leaked, then update those apps.", advanced},
	"LOG_LEVEL":     {"How much detail goes in the logs.", "Troubleshooting only.", advanced},

	"PLEX_URL":                {"Your Plex server's address.", "Everything Plex: history, library archive, users. Setup fills this in.", essential},
	"PLEX_TOKEN":              {"The key that lets this app read your Plex server.", "Everything Plex. Setup fills this in when you sign in with Plex.", essential},
	"PLEX_POLL_SECONDS":       {"How often to check what's playing.", "Tracking plays without Plex Pass: use 10. With Plex Pass webhooks, leave at 0.", recommended},
	"SCROBBLE_USERS":          {"Whose plays get sent to Trakt, Simkl and others.", "Sending plays out. Blank means only you. Everyone's plays are saved here anyway.", optional},
	"PRIVATE_LIBRARIES":       {"Libraries whose plays stay here and are never sent anywhere.", "Keeping adult or personal libraries off Trakt, Simkl and AI.", recommended},
	"WATCHED_PERCENT":         {"How far through something counts as watched.", "Nothing. The default of 80% suits most people.", optional},
	"PLEX_ARCHIVE_MINUTES":    {"How often your Plex metadata, playlists and collections are backed up.", "The Library page and restoring to a new server.", optional},
	"PLEX_WATCH_SCAN_SECONDS": {"How often to look for titles marked watched by hand.", "Catching Mark as watched in Plex.", optional},
	"PLEX_ALL_USERS":          {"Includes shared and managed users.", "Other people's playlists and watch marks. Turn off if it's only you.", optional},
	"PLEX_ALLOW_WRITES":       {"Allows this app to change Plex.", "Only restoring to a new server and copying playlists. Leave off otherwise.", advanced},

	"TRAKT_CLIENT_ID":      {"Identifies this app to Trakt.", "Syncing with Trakt. Skip if you don't use it.", optional},
	"TRAKT_CLIENT_SECRET":  {"The private half of the Trakt app key.", "Syncing with Trakt. Skip if you don't use it.", optional},
	"SIMKL_CLIENT_ID":      {"Identifies this app to Simkl.", "Syncing with Simkl. Skip if you don't use it.", optional},
	"TRACKER_PULL_MINUTES": {"How often new plays are pulled from Trakt and Simkl.", "Plays their apps recorded, such as from Netflix.", optional},
	"TRACKER_PULL_USER":    {"Which local user pulled plays belong to.", "Nothing unless several people use this app.", advanced},

	"FLOPPY_URL":   {"Your Floppy server's address.", "Sending plays to Floppy. Skip if you don't use it.", optional},
	"FLOPPY_TOKEN": {"The key for your Floppy server.", "Sending plays to Floppy. Skip if you don't use it.", optional},
	"RELAY_URLS":   {"Other apps that should get a copy of each Plex webhook.", "Tools like CrossWatch. Skip if you have none.", optional},
	"WEBHOOK_URLS": {"Addresses that get each play as JSON.", "Automations like n8n or Home Assistant. Skip if you have none.", optional},

	"TMDB_API_KEY":          {"A free movie and TV database key.", "Matching streaming titles, finding episode numbers and where-to-watch.", recommended},
	"STREAMING_REGION":      {"Your two-letter country.", "Showing what streams where you live.", optional},
	"MY_STREAMING_SERVICES": {"The services you pay for.", "Highlighting and filtering recommendations to what you can watch.", optional},
	"STREAMING_IMPORT_DIR":  {"A folder watched for history files.", "Automatic imports. Skip if you upload on the page.", advanced},

	"ANTHROPIC_API_KEY": {"Your Claude API key.", "AI recommendations from Claude. Skip if you don't want them.", optional},
	"CLAUDE_MODEL":      {"Which Claude model to use.", "Nothing. Leave the default.", advanced},
	"GEMINI_API_KEY":    {"Your Gemini API key.", "AI recommendations from Gemini. Skip if you don't want them.", optional},
	"GEMINI_MODEL":      {"Which Gemini model to use.", "Nothing. Leave the default.", advanced},
	"AI_INCLUDE_ADULT":  {"Ticks Include adult by default.", "Using private-library plays in recommendations.", optional},

	"CLZ_IMPORT_DIR":  {"A folder watched for CLZ Comics exports.", "Automatic imports. Skip if you upload on the page.", advanced},
	"BACKUP_KEEP":     {"How many daily database backups to keep.", "Recovering from a mistake or failed disk. Keep this above 0.", recommended},
	"BACKUP_COMPRESS": {"Shrinks backups to save disk space.", "Nothing. Leave it on.", advanced},

	"ARR_ARCHIVE_HOURS": {"How often Sonarr, Radarr and Whisparr are backed up.", "Their library and settings archive.", optional},

	"TAUTULLI_URL":     {"Your Tautulli address.", "Importing Plex history from Tautulli. Skip if you don't use it.", optional},
	"TAUTULLI_API_KEY": {"Its API key.", "Importing Plex history from Tautulli.", optional},
	"CROSSWATCH_URL":   {"Your CrossWatch address.", "Importing history and ratings from CrossWatch. Skip if you don't use it.", optional},
	"CROSSWATCH_TOKEN": {"Its API token.", "Importing history and ratings from CrossWatch.", optional},
	"PLEKT_URL":        {"Your Plekt address.", "Importing watch history from Plekt. Skip if you don't use it.", optional},
	"PLEKT_API_KEY":    {"Its API key.", "Importing watch history from Plekt.", optional},

	"SEERR_URL":     {"Your Jellyseerr or Overseerr address.", "The Request button on recommendations. Skip if you don't use one.", optional},
	"SEERR_API_KEY": {"Its API key.", "The Request button on recommendations.", optional},

	"AUDIOBOOKSHELF_URL":   {"Your Audiobookshelf address.", "Tracking audiobooks. Skip if you don't use it.", optional},
	"AUDIOBOOKSHELF_TOKEN": {"Its API token.", "Tracking audiobooks.", optional},
	"KOMGA_URL":            {"Your Komga address.", "Tracking books and comics you read there.", optional},
	"KOMGA_API_KEY":        {"Its API key.", "Tracking books and comics in Komga.", optional},
	"KAVITA_URL":           {"Your Kavita address.", "Tracking books and comics you read there.", optional},
	"KAVITA_API_KEY":       {"Its auth key.", "Tracking books and comics in Kavita.", optional},
	"HARDCOVER_TOKEN":      {"Your Hardcover API token.", "Importing reads from Hardcover.", optional},
	"OPENLIBRARY_USER":     {"Your Open Library username.", "Importing your public reading log.", optional},
	"BOOKS_IMPORT_DIR":     {"A folder watched for book exports.", "Automatic imports. Skip if you upload on the page.", advanced},

	"STEAM_API_KEY":             {"Your Steam Web API key.", "Tracking Steam games and playtime.", optional},
	"STEAM_ID":                  {"Your 17-digit Steam account number.", "Tracking Steam games and playtime.", optional},
	"RETROACHIEVEMENTS_USER":    {"Your RetroAchievements username.", "Tracking retro games.", optional},
	"RETROACHIEVEMENTS_API_KEY": {"Its web API key.", "Tracking retro games.", optional},
	"ROMM_URL":                  {"Your RomM address.", "Tracking your ROM library's play status.", optional},
	"ROMM_TOKEN":                {"Its client API token.", "Tracking your ROM library's play status.", optional},
	"GAMES_IMPORT_DIR":          {"A folder watched for game exports.", "Automatic imports. Skip if you upload on the page.", advanced},

	"SHELF_SYNC_MINUTES": {"How often book and game sources are read.", "Keeping Books and Games up to date.", optional},
	"SHELF_USER":         {"Which local user reads and plays belong to.", "Nothing unless several people use this app.", advanced},
}

// GroupHelp says when a whole section can be skipped.
var GroupHelp = map[string]string{
	"General":                  "Check Address and Behind a reverse proxy. The rest can stay as is.",
	"Plex":                     "Use Setup's Sign in with Plex and most of this fills itself in.",
	"Trakt and Simkl":          "Skip unless you use Trakt or Simkl.",
	"Other destinations":       "Skip unless you send plays to Floppy, CrossWatch or automations.",
	"Streaming":                "Add a TMDB key if you import streaming history. The rest is optional.",
	"AI recommendations":       "Skip unless you want the For you page.",
	"Comics and backups":       "The defaults are fine.",
	"History imports":          "Skip unless you want history from Tautulli, CrossWatch or Plekt. Floppy uses its own section.",
	"Requests":                 "Skip unless you use Jellyseerr or Overseerr.",
	"Books and audiobooks":     "Fill in only the services you use.",
	"Games":                    "Fill in only the services you use.",
	"Book and game sync":       "The defaults are fine.",
	"Sonarr, Radarr, Whisparr": "Skip any you don't run. Setup can find them for you.",
	"Jellyfin and Emby":        "Only for copying Plex playlists to them. Webhooks don't need this.",
}

// InfoFor returns the explanation for a setting.
func InfoFor(s Setting) Info {
	if i, ok := settingInfo[s.Key]; ok {
		return i
	}
	if s.Instance == "" {
		return Info{}
	}
	app := s.Instance[:1] + strings.ToLower(s.Instance[1:])
	purpose := "Archiving its library and adding recommendations to it."
	if s.Instance == "JELLYFIN" || s.Instance == "EMBY" {
		purpose = "Copying Plex playlists to it."
	}
	skip := " Skip if you don't run " + app + "."
	switch {
	case strings.HasSuffix(s.Key, "_URL"):
		return Info{"This " + app + " server's address.", purpose + skip, optional}
	case strings.HasSuffix(s.Key, "_API_KEY"):
		return Info{"This " + app + " server's API key.", purpose + skip, optional}
	case strings.HasSuffix(s.Key, "_NAME"):
		return Info{"A label for this server, such as radarr-4k.", "Telling several servers apart. Blank is fine.", advanced}
	case strings.HasSuffix(s.Key, "_ADULT"):
		return Info{"Marks this server as holding adult content.", "Keeping it out of normal recommendations.", optional}
	case strings.HasSuffix(s.Key, "_ROOT_FOLDER"):
		return Info{"Where recommended titles are added.", "The Add button. Blank uses its first folder.", advanced}
	case strings.HasSuffix(s.Key, "_QUALITY_PROFILE"):
		return Info{"Which quality profile new titles use.", "The Add button. Blank uses its first profile.", advanced}
	}
	return Info{}
}
