package store

import (
	"context"
	"testing"
	"time"
)

// An episode saved without its show's name used to stop recommendations with
// "converting NULL to string is unsupported".
func TestRecommendationInputsSkipEpisodesWithoutShowName(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().Unix()
	for _, q := range []string{
		`INSERT INTO media(id,kind,title,show_title,season,episode,updated_at) VALUES(1,'episode','Pilot',NULL,1,1,1)`,
		`INSERT INTO media(id,kind,title,show_title,season,episode,updated_at) VALUES(2,'episode','Blank',' ',1,2,1)`,
		`INSERT INTO media(id,kind,title,show_title,season,episode,updated_at) VALUES(3,'episode','Real','Severance',1,1,1)`,
		`INSERT INTO media(id,kind,title,year,updated_at) VALUES(4,'movie','Alien',1979,1)`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for id := 1; id <= 4; id++ {
		if _, err := st.DB.Exec(`INSERT INTO plays(media_id,user,watched_at,source) VALUES(?,'me',?,'t')`, id, now-int64(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`INSERT INTO ratings(media_id,user,rating,rated_at) VALUES(?,'me',8,?)`, id, now); err != nil {
			t.Fatal(err)
		}
	}

	check := func(name string, rows []Rated, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, r := range rows {
			if r.Title == "" || r.Title == "Pilot" || r.Title == "Blank" {
				t.Fatalf("%s: an episode with no show name leaked in: %+v", name, rows)
			}
		}
		if len(rows) != 2 {
			t.Fatalf("%s: want Severance and Alien, got %+v", name, rows)
		}
	}
	rows, err := st.RatedTitles(ctx, "me", true, 10)
	check("rated", rows, err)
	rows, err = st.MostPlayed(ctx, "me", false, 10)
	check("most played", rows, err)
	rows, err = st.RecentTitles(ctx, "me", false, 10)
	check("recent", rows, err)
	seen, err := st.SeenSet(ctx, "me")
	if err != nil || !seen["show|"+NormTitle("Severance")+"|"] || seen["show||"] {
		t.Fatalf("seen set: %v %v", seen, err)
	}
}
