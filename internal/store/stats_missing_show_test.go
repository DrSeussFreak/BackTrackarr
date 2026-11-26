package store

import (
	"context"
	"testing"
)

// An episode with no show name must not break the home page totals.
func TestStatsSurviveEpisodeWithoutShowName(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, q := range []string{
		`INSERT INTO media(id,kind,title,show_title,season,episode,updated_at) VALUES(1,'episode','Pilot',NULL,1,1,1)`,
		`INSERT INTO plays(media_id,user,watched_at,source) VALUES(1,'me',1700000000,'t')`,
		`INSERT INTO plays(media_id,user,watched_at,source) VALUES(1,'me',1700000100,'t')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s, err := st.computeStats(context.Background(), "")
	if err != nil {
		t.Fatalf("totals failed: %v", err)
	}
	if len(s.TopShows) != 0 {
		t.Fatalf("a nameless show shouldn't be listed: %+v", s.TopShows)
	}
	if len(s.Rewatched) != 1 || s.Rewatched[0].Name != "Pilot · S01E01" {
		t.Fatalf("rewatched: %+v", s.Rewatched)
	}
}
