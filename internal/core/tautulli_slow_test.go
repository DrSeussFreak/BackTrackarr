package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func TestShelfTimeoutIsAPlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	old := shelfHTTP.Timeout
	shelfHTTP.Timeout = 50 * time.Millisecond
	defer func() { shelfHTTP.Timeout = old }()

	var out map[string]any
	err := shelfGetJSON(context.Background(), srv.URL, nil, &out)
	if !errors.Is(err, errShelfSlow) || strings.Contains(err.Error(), "deadline") {
		t.Fatalf("got %v", err)
	}

	// Stopping an import isn't "slow".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shelfGetJSON(ctx, srv.URL, nil, &out); errors.Is(err, errShelfSlow) {
		t.Fatal("a cancelled import must not be reported as a slow service")
	}
}

func TestTautulliRetriesASlowPage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond) // first try is too slow
		}
		io.WriteString(w, `{"response":{"result":"success","data":{"recordsFiltered":1,"data":[
			{"started":1700000000,"media_type":"movie","title":"Alien","year":1979,"watched_status":1}]}}}`)
	}))
	defer srv.Close()
	old := shelfHTTP.Timeout
	shelfHTTP.Timeout = 100 * time.Millisecond
	defer func() { shelfHTTP.Timeout = old }()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{WatchedPercent: 80, TautulliURL: srv.URL, TautulliKey: "k"}
	e := New(context.Background(), cfg, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u := &uniRun{e: e, user: "me", res: &UniversalResult{}, formats: map[string]bool{}, report: func(string) {}, source: "tautulli-import"}
	ctx := withShelfTimeout(context.Background(), 150*time.Millisecond) // shorter than the real limit, for speed
	if err := e.pullTautulliWith(ctx, u); err != nil {
		t.Fatalf("a slow page should be retried: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one retry, got %d calls", calls.Load())
	}
}
