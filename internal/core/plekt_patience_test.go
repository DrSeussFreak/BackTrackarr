package core

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Plekt can take longer than other apps to start answering.
func TestPlektConnectionTestWaitsForSlowPlekt(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the usual 9 second limit")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "plekt-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		time.Sleep(11 * time.Second) // past the usual header limit
		io.WriteString(w, `{"documents":[],"hasMore":false}`)
	}))
	defer srv.Close()
	e := connEngine(t)
	r, ok := e.TestConnection(context.Background(), "plekt", map[string]string{"PLEKT_URL": srv.URL, "PLEKT_API_KEY": "plekt-key"})
	if !ok || r.Status != ConnOK {
		t.Fatalf("slow Plekt should pass: %+v", r)
	}
}

func TestShelfTimeoutCanBeRaisedPerSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	old := shelfHTTP.Timeout
	shelfHTTP.Timeout = 100 * time.Millisecond
	defer func() { shelfHTTP.Timeout = old }()

	var out map[string]any
	if err := shelfGetJSON(context.Background(), srv.URL, nil, &out); err == nil {
		t.Fatal("the usual limit should still apply to other sources")
	}
	if err := shelfGetJSON(withShelfTimeout(context.Background(), 3*time.Second), srv.URL, nil, &out); err != nil {
		t.Fatalf("a patient source should wait: %v", err)
	}
}
