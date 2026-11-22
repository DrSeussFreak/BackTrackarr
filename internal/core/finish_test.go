package core

import (
	"context"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

func TestMarkedFinished(t *testing.T) {
	for _, c := range []struct {
		count  int
		offset int64
		want   bool
	}{{0, 0, false}, {0, 5000, false}, {1, 5000, false}, {1, 0, true}, {3, 0, true}} {
		if got := markedFinished(c.count, c.offset); got != c.want {
			t.Errorf("count %d offset %d: got %v", c.count, c.offset, got)
		}
	}
}

func TestFinishPercentDefaults(t *testing.T) {
	c := &config.Config{WatchedPercent: 85, ComicPercent: 70}
	for kind, want := range map[string]float64{"movie": 85, "episode": 85, "comic": 70, "track": 90, "book": 90, "audiobook": 90} {
		if got := c.FinishPercent(kind); got != want {
			t.Errorf("%s: got %v want %v", kind, got, want)
		}
	}
}

func TestMarkScanIsNotPlaying(t *testing.T) {
	e, st, _ := nowPlayingEngine(t)
	ctx := context.Background()
	m := model.Media{Kind: model.Movie, Title: "Scan", PlexKey: "9"}
	if err := e.Ingest(ctx, model.Event{Action: model.ActWatched, Media: m, User: "u", Progress: 100, Source: plexMarkSource}, false); err != nil {
		t.Fatal(err)
	}
	rows, err := st.NowPlaying(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("a scan must not show as playing: %v %+v", err, rows)
	}
}

func TestLiveCountsOnceAtSetting(t *testing.T) {
	e, st, f := nowPlayingEngine(t)
	e.cfg.WatchedPercent = 90
	ctx := context.Background()
	m := model.Media{Kind: model.Episode, ShowTitle: "Show", Season: 1, Episode: 1, PlexKey: "7"}
	if err := e.Ingest(ctx, model.Event{Action: model.ActStart, Media: m, User: "u", Player: "TV", Source: "plex-webhook"}, true); err != nil {
		t.Fatal(err)
	}
	plays := func() int {
		var n int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM plays`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, step := range []struct {
		offset int64
		want   int
	}{{500000, 0}, {899000, 0}, {905000, 1}, {950000, 1}} {
		f.set(false, playing("7", "TV", "playing", step.offset))
		if err := e.SyncNowPlaying(ctx); err != nil {
			t.Fatal(err)
		}
		if got := plays(); got != step.want {
			t.Fatalf("at %d: %d plays, want %d", step.offset, got, step.want)
		}
	}
	rows, _ := st.NowPlaying(ctx)
	if len(rows) != 1 || !rows[0].Counted {
		t.Fatalf("row should stay and show Counted: %+v", rows)
	}
}
