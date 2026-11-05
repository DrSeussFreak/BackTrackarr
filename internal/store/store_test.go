package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMediaMergeAndPlayDedupe(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	m := model.Media{Kind: model.Episode, Title: "Pilot", ShowTitle: "Some Show", Season: 1, Episode: 1,
		IDs: model.IDs{TVDB: 349232}, PlexGUID: "plex://episode/1"}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	// The same episode arriving later with more IDs must merge into the same row.
	m2 := model.Media{Kind: model.Episode, Title: "Pilot", IDs: model.IDs{TVDB: 349232, IMDB: "tt0959621"}}
	if err := st.UpsertMedia(ctx, &m2); err != nil {
		t.Fatal(err)
	}
	if m2.ID != m.ID || m2.IDs.IMDB != "tt0959621" || m2.ShowTitle != "Some Show" {
		t.Fatalf("merge failed: %+v vs %+v", m2, m)
	}

	at := time.Date(2020, 5, 1, 20, 0, 0, 0, time.UTC)
	if ok, err := st.RecordPlay(ctx, m.ID, "alice", at, "test", "", false); err != nil || !ok {
		t.Fatalf("first play: %v %v", ok, err)
	}
	if ok, _ := st.RecordPlay(ctx, m.ID, "alice", at.Add(2*time.Hour), "test", "", false); ok {
		t.Fatal("play inside the dedupe window was recorded")
	}
	if ok, _ := st.RecordPlay(ctx, m.ID, "bob", at, "test", "", false); !ok {
		t.Fatal("another user's play was treated as a duplicate")
	}
	if ok, _ := st.RecordPlay(ctx, m.ID, "alice", at.Add(24*time.Hour), "test", "", false); !ok {
		t.Fatal("a rewatch the next day was treated as a duplicate")
	}
}

func TestStatsCacheInvalidation(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	m := model.Media{Kind: model.Movie, Title: "Heat", Year: 1995, DurationS: 10200, IDs: model.IDs{IMDB: "tt0113277"}}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	s0, err := st.Stats(ctx, "")
	if err != nil || s0.Plays != 0 {
		t.Fatalf("empty stats: %+v %v", s0, err)
	}
	if again, _ := st.Stats(ctx, ""); again != s0 {
		t.Fatal("unchanged stats were recomputed instead of cached")
	}
	if _, err := st.RecordPlay(ctx, m.ID, "alice", time.Now(), "test", "", false); err != nil {
		t.Fatal(err)
	}
	s1, _ := st.Stats(ctx, "")
	if s1.Plays != 1 || s1.Movies != 1 || s1.ThisYear != 1 || len(s1.TopMovies) != 1 {
		t.Fatalf("stats after a play: %+v", s1)
	}
	rows, _ := st.History(ctx, HistoryQuery{})
	if err := st.DeletePlay(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if s2, _ := st.Stats(ctx, ""); s2.Plays != 0 {
		t.Fatalf("stats after delete: %+v", s2)
	}
}

func TestHistoryKeysetPaging(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	m := model.Media{Kind: model.Movie, Title: "Heat", IDs: model.IDs{IMDB: "tt0113277"}}
	if err := st.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 25
	for i := 0; i < n; i++ {
		if ok, err := st.RecordPlay(ctx, m.ID, "alice", base.Add(time.Duration(i)*24*time.Hour), "test", "", false); !ok || err != nil {
			t.Fatalf("play %d: %v %v", i, ok, err)
		}
	}
	seen := map[int64]bool{}
	q := HistoryQuery{Limit: 10}
	last := int64(1 << 62)
	for page := 0; ; page++ {
		rows, err := st.History(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("play %d returned twice", r.ID)
			}
			if r.WatchedAt > last {
				t.Fatal("history is not newest first")
			}
			seen[r.ID], last = true, r.WatchedAt
		}
		end := rows[len(rows)-1]
		q.BeforeTS, q.BeforeID = end.WatchedAt, end.ID
		if page > n {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != n {
		t.Fatalf("saw %d plays, want %d", len(seen), n)
	}
}

func TestOldDatabaseNameIsMigrated(t *testing.T) {
	for _, oldName := range oldDBNames {
		t.Run(oldName, func(t *testing.T) { testOldName(t, oldName) })
	}
}

func testOldName(t *testing.T, oldName string) {
	dir := t.TempDir()
	old, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m := model.Media{Kind: model.Movie, Title: "Heat", IDs: model.IDs{IMDB: "tt0113277"}}
	if err := old.UpsertMedia(ctx, &m); err != nil {
		t.Fatal(err)
	}
	old.Close()
	// pretend it was written by the earlier version
	if err := os.Rename(filepath.Join(dir, "backtrackarr.db"), filepath.Join(dir, oldName)); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(filepath.Join(dir, oldName)); err == nil {
		t.Fatal("old file still there")
	}
	if got, _, _ := st.FindMedia(ctx, model.Media{Kind: model.Movie, IDs: model.IDs{IMDB: "tt0113277"}}); got.Title != "Heat" {
		t.Fatalf("data lost in migration: %+v", got)
	}
}

// A database from the first release (no music columns, no private flag)
// must open and keep every row.
func TestMigrateFromV1(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "backtrackarr.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE media (id INTEGER PRIMARY KEY, kind TEXT NOT NULL CHECK (kind IN ('movie','episode')), title TEXT NOT NULL,
			year INTEGER, show_title TEXT, season INTEGER, episode INTEGER, duration_s INTEGER, imdb TEXT, tmdb INTEGER, tvdb INTEGER,
			trakt INTEGER, show_imdb TEXT, show_tmdb INTEGER, show_tvdb INTEGER, show_trakt INTEGER, plex_guid TEXT, plex_key TEXT,
			updated_at INTEGER NOT NULL)`,
		`CREATE TABLE plays (id INTEGER PRIMARY KEY, media_id INTEGER NOT NULL REFERENCES media(id), user TEXT NOT NULL DEFAULT '',
			watched_at INTEGER NOT NULL, source TEXT NOT NULL, player TEXT, UNIQUE (media_id, user, watched_at))`,
		`INSERT INTO media(id,kind,title,year,imdb,updated_at) VALUES (7,'movie','Heat',1995,'tt0113277',0)`,
		`INSERT INTO plays(media_id,user,watched_at,source) VALUES (7,'alice',1000,'old')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	rows, err := st.History(ctx, HistoryQuery{})
	if err != nil || len(rows) != 1 || rows[0].Media.Title != "Heat" || rows[0].Media.ID != 7 || rows[0].Private {
		t.Fatalf("history after migration: %+v %v", rows, err)
	}
	tr := model.Media{Kind: model.Track, Title: "Song", ShowTitle: "Artist", Album: "Album"}
	if err := st.UpsertMedia(ctx, &tr); err != nil {
		t.Fatalf("tracks should be allowed after migration: %v", err)
	}
	var v int
	st.DB.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != schemaVersion {
		t.Fatalf("user_version = %d", v)
	}
}
