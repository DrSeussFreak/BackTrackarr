package config

import "testing"

func TestFromEnv(t *testing.T) {
	t.Setenv("BACKUP_KEEP", "4")
	t.Setenv("PLEX_ALL_USERS", "yes")
	t.Setenv("TMDB_API_KEY", "<your key>")
	t.Setenv("PLEX_URL", "not a url")
	t.Setenv("STREAMING_REGION", "CA")
	t.Setenv("DATA_DIR", "/data")
	got := FromEnv(map[string]string{"STREAMING_REGION": "GB"})
	if got["BACKUP_KEEP"] != "4" || got["PLEX_ALL_USERS"] != "true" {
		t.Fatalf("values not moved: %v", got)
	}
	for _, k := range []string{"TMDB_API_KEY", "PLEX_URL", "STREAMING_REGION", "DATA_DIR"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not be moved", k)
		}
	}
}
