package store

import (
	"context"
	"testing"
	"time"
)

func TestShelfStatsNowDoesNotWaitWhenLarge(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if got, updating := st.ShelfStatsNow(ctx, ""); updating || got == nil {
		t.Fatalf("small shelf should be computed in the request: %v %v", got, updating)
	}

	st.shelfCost.Store(int64(statsBudget))
	st.shelfCache = nil
	if got, updating := st.ShelfStatsNow(ctx, ""); got != nil || !updating {
		t.Fatalf("large shelf with no result yet: %v %v", got, updating)
	}
	for end := time.Now().Add(10 * time.Second); st.shelfBusy.Load(); {
		if time.Now().After(end) {
			t.Fatal("background rebuild never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	st.shelfCost.Store(int64(statsBudget))
	if got, updating := st.ShelfStatsNow(ctx, "game"); got == nil || updating {
		t.Fatalf("after rebuild: %v %v", got, updating)
	}

	old, _ := st.ShelfStatsNow(ctx, "")
	st.shelfGen.Add(1)
	if got, updating := st.ShelfStatsNow(ctx, ""); got != old || !updating {
		t.Fatalf("a change should keep showing the last totals: same=%v updating=%v", got == old, updating)
	}
	for end := time.Now().Add(10 * time.Second); st.shelfBusy.Load(); {
		if time.Now().After(end) {
			t.Fatal("background rebuild never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
