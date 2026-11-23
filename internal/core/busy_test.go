package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/plex"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
	"github.com/ncruces/go-sqlite3"
)

func TestRetryBusy(t *testing.T) {
	old := busyRetryWait
	busyRetryWait = time.Millisecond
	defer func() { busyRetryWait = old }()
	calls := 0
	err := retryBusy(context.Background(), func() error {
		if calls++; calls < 3 {
			return sqlite3.BUSY
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls %d err %v", calls, err)
	}
	calls = 0
	boom := errors.New("boom")
	if err := retryBusy(context.Background(), func() error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("other errors must not retry: %d %v", calls, err)
	}
	calls = 0
	if err := retryBusy(context.Background(), func() error { calls++; return sqlite3.BUSY }); !errors.Is(err, sqlite3.BUSY) || calls != 4 {
		t.Fatalf("must give up: %d %v", calls, err)
	}
}

func TestImportPlexWaitsForWriterAndResumes(t *testing.T) {
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "accounts") {
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"Account": []map[string]any{{"id": 1, "name": "owner"}}}})
			return
		}
		if !strings.Contains(r.URL.Path, "history") {
			json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{}})
			return
		}
		items := []map[string]any{}
		if r.Header.Get("X-Plex-Container-Start") == "0" || r.URL.Query().Get("X-Plex-Container-Start") == "0" {
			for i := 1; i <= 3; i++ {
				items = append(items, map[string]any{"ratingKey": strconv.Itoa(i), "type": "movie", "title": "Movie " + strconv.Itoa(i),
					"guid": "plex://movie/" + strconv.Itoa(i), "viewedAt": 1700000000 + i, "accountID": 1})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"size": len(items), "Metadata": items}})
	}))
	defer srv.Close()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := New(context.Background(), &config.Config{}, st, plex.NewClient(srv.URL, "tok"), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	holder, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		holder.Rollback()
	}()
	for pass := 0; pass < 2; pass++ {
		if err := e.ImportPlex(ctx, func(string) {}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM plays`).Scan(&n); err != nil || n != 3 {
			t.Fatalf("pass %d: %d plays, err %v", pass, n, err)
		}
	}
}
