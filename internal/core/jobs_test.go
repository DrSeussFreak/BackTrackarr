package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/config"
	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

func jobEngine(t *testing.T, ctx context.Context, st *store.Store, dir string) *Engine {
	t.Helper()
	return New(ctx, &config.Config{DataDir: dir}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func jobStatus(e *Engine, id string) JobStatus {
	for _, s := range e.Jobs.List() {
		if s.ID == id {
			return s
		}
	}
	return JobStatus{}
}

func waitJob(t *testing.T, e *Engine, id string, states ...string) JobStatus {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		s := jobStatus(e, id)
		for _, want := range states {
			if s.State == want {
				return s
			}
		}
	}
	t.Fatalf("job never reached %v: %+v", states, jobStatus(e, id))
	return JobStatus{}
}

func historyFiles(t *testing.T, n int) []ImportFile {
	t.Helper()
	dir := t.TempDir()
	out := make([]ImportFile, n)
	for i := range out {
		p := filepath.Join(dir, fmt.Sprintf("up%d", i))
		body := fmt.Sprintf(`[{"watched_at":"2024-01-%02dT00:00:00.000Z","type":"movie","movie":{"title":"Job %d","year":2000,"ids":{"tmdb":%d}}}]`, i%27+1, i, 5000+i)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out[i] = ImportFile{Path: p, Name: fmt.Sprintf("history-%d.json", i)}
	}
	return out
}

func onlyJob(t *testing.T, e *Engine) string {
	t.Helper()
	l := e.Jobs.List()
	if len(l) != 1 {
		t.Fatalf("%d jobs", len(l))
	}
	return l[0].ID
}

// The important case: a new image restarts the app mid-import.
func TestJobContinuesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, stop := context.WithCancel(context.Background())
	defer stop()
	e := jobEngine(t, first, st, dir)
	const total = 120
	if err := e.StartFileImport("import ← batch", historyFiles(t, total), FileImport{Mode: "import", User: "me"}); err != nil {
		t.Fatal(err)
	}
	id := onlyJob(t, e)
	for end := time.Now().Add(15 * time.Second); jobStatus(e, id).Done < 20; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no progress")
		}
	}
	stop()
	e.Jobs.Wait(10 * time.Second)
	if s := jobStatus(e, id); s.State != JobQueued || s.Done >= total {
		t.Fatalf("should wait for the next start: %+v", s)
	}

	second := jobEngine(t, context.Background(), st, dir)
	if err := second.ResumeJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, second, id, JobDone, JobFailed)
	if s.State != JobDone || s.Done != total {
		t.Fatalf("%+v", s)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays`); n != total {
		t.Fatalf("%d plays, want %d with none lost or doubled", n, total)
	}
	if _, err := os.Stat(filepath.Join(dir, "jobs", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uploaded files were left behind")
	}
}

func TestJobPauseResumeStopRemove(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := jobEngine(t, context.Background(), st, dir)
	release := make(chan struct{})
	e.Jobs.runners["test"] = func(ctx context.Context, run *JobRun) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	if ok, err := e.Jobs.StartDurable("test", "first", nil, nil, true); !ok || err != nil {
		t.Fatal(ok, err)
	}
	id := onlyJob(t, e)
	waitJob(t, e, id, JobRunning)
	if ok, _ := e.Jobs.StartDurable("test", "first", nil, nil, true); ok {
		t.Fatal("a duplicate job started")
	}
	if err := e.Jobs.Pause(id); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, JobPaused)
	if e.Jobs.AnyRunning() {
		t.Fatal("a paused job counts as running")
	}
	// A paused job stays paused through a restart.
	other := jobEngine(t, context.Background(), st, dir)
	other.Jobs.runners["test"] = e.Jobs.runners["test"]
	if err := other.ResumeJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := jobStatus(other, id); s.State != JobPaused {
		t.Fatalf("after restart: %+v", s)
	}
	if err := e.Jobs.Remove(id); err == nil {
		t.Fatal("removed a paused job")
	}
	if err := e.Jobs.ResumeJob(id); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, JobRunning)
	if err := e.Jobs.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitJob(t, e, id, JobStopped)
	if err := e.Jobs.Remove(id); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ListJobs(context.Background()); len(rows) != 0 {
		t.Fatalf("%d saved jobs", len(rows))
	}
}

func TestJobPanicDoesNotCrashApp(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := jobEngine(t, context.Background(), st, dir)
	e.Jobs.runners["boom"] = func(context.Context, *JobRun) error { panic("bad data") }
	if _, err := e.Jobs.StartDurable("boom", "durable", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, e, onlyJob(t, e), JobFailed); !strings.Contains(s.Err, "bad data") {
		t.Fatalf("%+v", s)
	}
	e.Jobs.Start(context.Background(), "plain", func(context.Context, func(string)) error { panic("bad too") })
	for end := time.Now().Add(5 * time.Second); e.Jobs.AnyRunning() && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
	}
	for _, s := range e.Jobs.List() {
		if s.Name == "plain" && s.State != JobFailed {
			t.Fatalf("%+v", s)
		}
	}
}

func TestOneBadFileDoesNotStopBatch(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := jobEngine(t, context.Background(), st, dir)
	files := historyFiles(t, 3)
	if err := os.WriteFile(files[1].Path, []byte(`[{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.StartFileImport("import ← mixed", files, FileImport{Mode: "import", User: "me"}); err != nil {
		t.Fatal(err)
	}
	s := waitJob(t, e, onlyJob(t, e), JobDone, JobFailed)
	if s.State != JobFailed || !strings.Contains(s.Err, "history-1.json") {
		t.Fatalf("%+v", s)
	}
	if n := uniCount(t, st, `SELECT COUNT(*) FROM plays`); n != 2 {
		t.Fatalf("%d plays", n)
	}
}
