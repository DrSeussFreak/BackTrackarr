package store

import (
	"context"
	"testing"
	"time"
)

func TestStatsNowNeverWaitsOnLargeHistory(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	addPlay := func(id int) {
		t.Helper()
		if _, err := st.DB.Exec(`INSERT INTO media(id,kind,title,updated_at) VALUES(?,'movie','Film',1)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`INSERT INTO plays(media_id,user,watched_at,source) VALUES(?,'a',1700000000,'t')`, id); err != nil {
			t.Fatal(err)
		}
		st.gen.Add(1)
	}
	waitIdle := func() {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); st.statsBusy.Load(); {
			if time.Now().After(end) {
				t.Fatal("background rebuild never finished")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Small history: exact, in the request.
	addPlay(1)
	if got, updating := st.StatsNow(ctx, ""); updating || got.Plays != 1 {
		t.Fatalf("small history: plays=%d updating=%v", got.Plays, updating)
	}

	// Large history with nothing saved: answers at once, builds behind the page.
	st.statsCost.Store(int64(statsBudget))
	st.statsCache = nil
	if got, updating := st.StatsNow(ctx, ""); !updating || got == nil || got.Plays != 0 {
		t.Fatalf("first load: plays=%d updating=%v", got.Plays, updating)
	}
	waitIdle()
	st.statsCost.Store(int64(statsBudget))
	if got, updating := st.StatsNow(ctx, ""); updating || got.Plays != 1 {
		t.Fatalf("after rebuild: plays=%d updating=%v", got.Plays, updating)
	}

	// A new play keeps showing the old totals rather than blocking.
	addPlay(2)
	if got, updating := st.StatsNow(ctx, ""); !updating || got.Plays != 1 {
		t.Fatalf("stale: plays=%d updating=%v", got.Plays, updating)
	}
	waitIdle()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Saved totals survive a restart.
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.statsCost.Store(int64(statsBudget))
	st.statsLastRun.Store(time.Now().Unix())
	got, updating := st.StatsNow(ctx, "")
	if !updating || got.Plays != 1 || got.MaxYear != 1 {
		t.Fatalf("after restart: plays=%d max=%d updating=%v", got.Plays, got.MaxYear, updating)
	}
	waitIdle()
}
