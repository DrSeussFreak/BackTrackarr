package core

import (
	"context"
	"testing"

	"github.com/DrSeussFreak/BackTrackarr/internal/model"
)

func TestEndedSessionStaysOffNowPlaying(t *testing.T) {
	e, st, f := nowPlayingEngine(t)
	ctx := context.Background()
	m := startEp(t, e, "7", "Ended", "Den", "plex-webhook", model.ActStart)
	row, ok := showing(t, st)["Ended"]
	if !ok {
		t.Fatal("session should show before it's ended")
	}
	if found, err := st.HideSession(ctx, row.Key); err != nil || !found {
		t.Fatalf("hide: found=%v err=%v", found, err)
	}
	if found, _ := st.HideSession(ctx, row.Key); found {
		t.Fatal("ending it twice should report it's already gone")
	}

	// Plex is still playing it: the check must not bring it back.
	age(t, st)
	f.set(false, playing("7", "Den", "playing", 500000))
	if err := e.SyncNowPlaying(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := showing(t, st)["Ended"]; ok {
		t.Fatal("Plex check brought an ended session back")
	}

	// A pause event doesn't bring it back either.
	startEp(t, e, "7", "Ended", "Den", "plex-webhook", model.ActPause)
	if _, ok := showing(t, st)["Ended"]; ok {
		t.Fatal("a later event brought an ended session back")
	}

	// It still counts when the real playback passes the watched point.
	if err := e.Ingest(ctx, model.Event{Action: model.ActWatched, Media: m, User: "Alex", Owner: true, Player: "Den", Progress: 90, Source: "plex-webhook"}, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.Stats(ctx, ""); s.Plays != 1 {
		t.Fatalf("ended session should still count its play: %d", s.Plays)
	}
	if _, ok := showing(t, st)["Ended"]; ok {
		t.Fatal("the watched event brought an ended session back")
	}

	// Once playback stops, the next play shows normally.
	age(t, st)
	f.set(false)
	if err := e.SyncNowPlaying(ctx); err != nil {
		t.Fatal(err)
	}
	startEp(t, e, "7", "Ended", "Den", "plex-webhook", model.ActStart)
	if _, ok := showing(t, st)["Ended"]; !ok {
		t.Fatal("a new play should show again")
	}
}
