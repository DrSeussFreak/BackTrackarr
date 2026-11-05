package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "settings.env")
	os.WriteFile(f, []byte("# comment\r\nPLEX_URL=http://plex:32400\r\nexport ADMIN_USER=\"bob\"\nWATCHED_PERCENT=70\nRADARR_2_URL=http://r4k\nRADARR_2_API_KEY=k\nWHISPARR_URL=http://w\n"), 0o600)
	t.Setenv("ENV_FILE", f)
	t.Setenv("WATCHED_PERCENT", "90") // the environment wins over the file
	for _, k := range []string{"PLEX_URL", "ADMIN_USER", "RADARR_2_URL", "RADARR_2_API_KEY", "WHISPARR_URL"} {
		t.Setenv(k, "") // registers cleanup; empty means "not set" to Load
		os.Unsetenv(k)
	}
	c := Load()
	if c.EnvFile != f || c.PlexURL != "http://plex:32400" || c.AdminUser != "bob" || c.WatchedPercent != 90 {
		t.Fatalf("settings file not applied: file=%q plex=%q user=%q pct=%v", c.EnvFile, c.PlexURL, c.AdminUser, c.WatchedPercent)
	}
	if len(c.Arr) != 2 || c.Arr[0].Name != "radarr-2" || c.Arr[0].APIKey != "k" || !c.Arr[1].Adult {
		t.Fatalf("numbered instances: %+v", c.Arr)
	}
}
