package store

import (
	"context"
	"testing"
	"time"

	"reelog/internal/model"
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
