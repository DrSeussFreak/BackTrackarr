package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/DrSeussFreak/BackTrackarr/internal/store"
)

// Job states.
const (
	JobRunning = "running"
	JobQueued  = "queued"
	JobPaused  = "paused"
	JobDone    = "done"
	JobFailed  = "failed"
	JobStopped = "stopped"
)

const jobHistory = 40 // finished durable jobs kept in the list

var (
	errJobPause = errors.New("paused")
	errJobStop  = errors.New("stopped")
)

// JobStatus is one background task as shown on the page.
type JobStatus struct {
	ID       string
	Name     string
	Kind     string
	State    string
	Running  bool // running or waiting its turn
	Durable  bool // survives a restart and can be paused
	Stopping bool
	Message  string
	Err      string
	Updated  time.Time
	Done     int
	Total    int
}

func (s JobStatus) CanPause() bool {
	return s.Durable && !s.Stopping && (s.State == JobRunning || s.State == JobQueued)
}
func (s JobStatus) CanResume() bool { return s.Durable && s.State == JobPaused }
func (s JobStatus) CanRetry() bool  { return s.Durable && s.State == JobFailed }
func (s JobStatus) CanStop() bool {
	return !s.Stopping && (s.State == JobRunning || s.State == JobQueued || s.State == JobPaused)
}
func (s JobStatus) CanRemove() bool {
	return s.State == JobDone || s.State == JobFailed || s.State == JobStopped
}

// JobRunner does the work of one durable job. It must be safe to run again
// from its last checkpoint.
type JobRunner func(ctx context.Context, run *JobRun) error

type job struct {
	JobStatus
	params     []byte
	checkpoint []byte
	created    int64
	cancel     context.CancelCauseFunc
}

// Jobs tracks background tasks. Durable jobs run one at a time, are saved in
// the database, and carry on from their last checkpoint after a restart.
type Jobs struct {
	mu      sync.Mutex
	m       map[string]*job
	ctx     context.Context
	st      *store.Store
	dir     string
	log     *slog.Logger
	runners map[string]JobRunner
	working bool
	wg      sync.WaitGroup
}

// NewJobs makes the job list. dir is the data folder.
func NewJobs(ctx context.Context, st *store.Store, dir string, log *slog.Logger) *Jobs {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "backtrackarr")
	}
	return &Jobs{m: map[string]*job{}, ctx: ctx, st: st, dir: dir, log: log, runners: map[string]JobRunner{}}
}

func newJobID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func active(s JobStatus) bool {
	return s.State == JobRunning || s.State == JobQueued || s.State == JobPaused
}

// callJob turns a panic into a failed job, so one bad file can't take the
// whole app down.
func (j *Jobs) callJob(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if j.log != nil {
				j.log.Error("job crashed", "job", name, "panic", r)
			}
			err = fmt.Errorf("the job hit an internal error: %v", r)
		}
	}()
	return fn()
}

// Start runs fn in the background unless a job with that name is active. These
// jobs can be stopped, but they aren't saved across a restart.
func (j *Jobs) Start(ctx context.Context, name string, fn func(ctx context.Context, report func(string)) error) bool {
	j.mu.Lock()
	for id, x := range j.m {
		if x.Name != name {
			continue
		}
		if active(x.JobStatus) {
			j.mu.Unlock()
			return false
		}
		if !x.Durable {
			delete(j.m, id)
		}
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	x := &job{JobStatus: JobStatus{ID: newJobID(), Name: name, State: JobRunning, Running: true, Message: "starting", Updated: time.Now()}, cancel: cancel}
	j.m[x.ID] = x
	j.mu.Unlock()
	go func() {
		defer cancel(nil)
		err := j.callJob(name, func() error {
			return fn(runCtx, func(msg string) { j.report(x, msg) })
		})
		j.mu.Lock()
		defer j.mu.Unlock()
		x.Running, x.Stopping = false, false
		x.Updated = time.Now()
		switch {
		case errors.Is(context.Cause(runCtx), errJobStop):
			x.State = JobStopped
		case ctx.Err() != nil:
			x.State, x.Err = JobStopped, "Stopped when the app restarted."
		case err != nil:
			x.State, x.Err = JobFailed, err.Error()
		default:
			x.State = JobDone
		}
	}()
	return true
}

func (j *Jobs) report(x *job, msg string) {
	j.mu.Lock()
	x.Message, x.Updated = msg, time.Now()
	j.mu.Unlock()
}

type durableParams struct {
	Files []string        `json:"files,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func (j *Jobs) jobDir(id string) string { return filepath.Join(j.dir, "jobs", id) }

func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(to)
		return err
	}
	return os.Remove(from)
}

// StartDurable queues a job that survives restarts. Its files move into the
// data folder and stay there until it finishes. With unique set, a job of the
// same name that's still active means nothing new starts.
func (j *Jobs) StartDurable(kind, name string, data any, files []ImportFile, unique bool) (bool, error) {
	if j.st == nil {
		return false, errors.New("job storage isn't available")
	}
	if unique {
		j.mu.Lock()
		for _, x := range j.m {
			if x.Name == name && active(x.JobStatus) {
				j.mu.Unlock()
				return false, nil
			}
		}
		j.mu.Unlock()
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return false, err
	}
	id := newJobID()
	p := durableParams{Data: raw}
	if len(files) > 0 {
		dir := j.jobDir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
		for i, f := range files {
			if err := moveFile(f.Path, filepath.Join(dir, fmt.Sprintf("%06d", i))); err != nil {
				os.RemoveAll(dir)
				return false, fmt.Errorf("couldn't keep the upload; check free disk space: %w", err)
			}
			p.Files = append(p.Files, f.Name)
		}
	}
	params, err := json.Marshal(p)
	if err != nil {
		os.RemoveAll(j.jobDir(id))
		return false, err
	}
	now := time.Now()
	x := &job{JobStatus: JobStatus{ID: id, Name: name, Kind: kind, State: JobQueued, Running: true, Durable: true,
		Message: "waiting its turn", Updated: now, Total: len(files)}, params: params, created: now.UnixNano()}
	if err := j.save(x); err != nil {
		os.RemoveAll(j.jobDir(id))
		return false, err
	}
	j.mu.Lock()
	j.m[id] = x
	j.mu.Unlock()
	j.kick()
	return true, nil
}

// save writes a durable job. It keeps working while the app shuts down.
func (j *Jobs) save(x *job) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j.mu.Lock()
	r := store.JobRecord{ID: x.ID, Kind: x.Kind, Name: x.Name, State: x.State, Message: x.Message, Err: x.Err,
		Params: x.params, Checkpoint: x.checkpoint, Created: x.created, Updated: x.Updated.Unix(), Done: x.Done, Total: x.Total}
	j.mu.Unlock()
	return j.st.SaveJob(ctx, r)
}

// Resume loads saved jobs after a start. Anything that was running or waiting
// goes back in the queue, and paused jobs stay paused.
func (j *Jobs) Resume(ctx context.Context) error {
	rows, err := j.st.ListJobs(ctx)
	if err != nil {
		return err
	}
	j.mu.Lock()
	queued := 0
	for _, r := range rows {
		x := &job{JobStatus: JobStatus{ID: r.ID, Name: r.Name, Kind: r.Kind, State: r.State, Durable: true, Message: r.Message,
			Err: r.Err, Updated: time.Unix(r.Updated, 0), Done: r.Done, Total: r.Total},
			params: r.Params, checkpoint: r.Checkpoint, created: r.Created}
		if x.State == JobRunning || x.State == JobQueued {
			x.State, x.Running, x.Message = JobQueued, true, "continuing after a restart"
			queued++
		}
		j.m[x.ID] = x
	}
	j.mu.Unlock()
	if queued > 0 {
		j.kick()
	}
	return nil
}

func (j *Jobs) kick() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.working || j.ctx.Err() != nil {
		return
	}
	j.working = true
	j.wg.Add(1)
	go j.work()
}

func (j *Jobs) work() {
	defer j.wg.Done()
	for {
		j.mu.Lock()
		var next *job
		if j.ctx.Err() == nil {
			for _, x := range j.m {
				if x.Durable && x.State == JobQueued && (next == nil || x.created < next.created) {
					next = x
				}
			}
		}
		if next == nil {
			j.working = false
			j.mu.Unlock()
			return
		}
		runCtx, cancel := context.WithCancelCause(j.ctx)
		next.State, next.cancel, next.Err = JobRunning, cancel, ""
		next.Updated = time.Now()
		j.mu.Unlock()
		_ = j.save(next)
		j.runDurable(runCtx, next)
		cancel(nil)
	}
}

func (j *Jobs) runDurable(ctx context.Context, x *job) {
	var p durableParams
	err := json.Unmarshal(x.params, &p)
	run := &JobRun{jobs: j, job: x, Data: p.Data}
	for i, name := range p.Files {
		run.Files = append(run.Files, ImportFile{Path: filepath.Join(j.jobDir(x.ID), fmt.Sprintf("%06d", i)), Name: name})
	}
	if err == nil {
		runner := j.runners[x.Kind]
		if runner == nil {
			err = fmt.Errorf("this version can't run %q jobs", x.Kind)
		} else {
			err = j.callJob(x.Name, func() error { return runner(ctx, run) })
		}
	}
	cause := context.Cause(ctx)
	shuttingDown := j.ctx.Err() != nil
	// Files go before the state changes, so "done" means they're gone.
	if errors.Is(cause, errJobStop) || (err == nil && !shuttingDown && !errors.Is(cause, errJobPause)) {
		os.RemoveAll(j.jobDir(x.ID))
	}
	j.mu.Lock()
	x.Stopping, x.cancel = false, nil
	x.Updated = time.Now()
	switch {
	case errors.Is(cause, errJobPause):
		x.State, x.Running, x.Message = JobPaused, false, pausedMessage(x)
	case errors.Is(cause, errJobStop):
		x.State, x.Running, x.Message = JobStopped, false, "stopped"
	case shuttingDown:
		// Shutting down. It stays in the queue for the next start.
		x.State, x.Message = JobQueued, "continues after the restart"
	case err != nil:
		x.State, x.Running, x.Err = JobFailed, false, err.Error()
	default:
		x.State, x.Running = JobDone, false
	}
	j.mu.Unlock()
	_ = j.save(x)
	j.prune()
}

func pausedMessage(x *job) string {
	if x.Total > 1 {
		return fmt.Sprintf("paused at %d of %d files", x.Done, x.Total)
	}
	return "paused"
}

// prune keeps the finished list short.
func (j *Jobs) prune() {
	j.mu.Lock()
	var done []*job
	for _, x := range j.m {
		if x.Durable && !active(x.JobStatus) {
			done = append(done, x)
		}
	}
	sort.Slice(done, func(a, b int) bool { return done[a].Updated.After(done[b].Updated) })
	var drop []string
	for i := jobHistory; i < len(done); i++ {
		drop = append(drop, done[i].ID)
		delete(j.m, done[i].ID)
	}
	j.mu.Unlock()
	for _, id := range drop {
		os.RemoveAll(j.jobDir(id))
		_ = j.st.DeleteJob(context.Background(), id)
	}
}

// Wait gives running jobs time to stop during shutdown.
func (j *Jobs) Wait(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		j.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

var errJobState = errors.New("that job can't do that right now")

// Pause stops a durable job at its last checkpoint.
func (j *Jobs) Pause(id string) error {
	j.mu.Lock()
	x := j.m[id]
	if x == nil || !x.CanPause() {
		j.mu.Unlock()
		return errJobState
	}
	if x.State == JobQueued {
		x.State, x.Running, x.Message, x.Updated = JobPaused, false, pausedMessage(x), time.Now()
		j.mu.Unlock()
		return j.save(x)
	}
	x.Message = "pausing…"
	cancel := x.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel(errJobPause)
	}
	return nil
}

// ResumeJob puts a paused or failed durable job back in the queue.
func (j *Jobs) ResumeJob(id string) error {
	j.mu.Lock()
	x := j.m[id]
	if x == nil || !(x.CanResume() || x.CanRetry()) {
		j.mu.Unlock()
		return errJobState
	}
	x.State, x.Running, x.Err, x.Message, x.Updated = JobQueued, true, "", "waiting its turn", time.Now()
	j.mu.Unlock()
	if err := j.save(x); err != nil {
		return err
	}
	j.kick()
	return nil
}

// Stop ends a job for good. What it already imported stays.
func (j *Jobs) Stop(id string) error {
	j.mu.Lock()
	x := j.m[id]
	if x == nil || !x.CanStop() {
		j.mu.Unlock()
		return errJobState
	}
	if x.State == JobRunning {
		x.Stopping, x.Message = true, "stopping…"
		cancel := x.cancel
		j.mu.Unlock()
		if cancel != nil {
			cancel(errJobStop)
		}
		return nil
	}
	x.State, x.Running, x.Message, x.Updated = JobStopped, false, "stopped", time.Now()
	j.mu.Unlock()
	os.RemoveAll(j.jobDir(id))
	return j.save(x)
}

// Remove takes a finished job off the list and deletes any files it kept.
func (j *Jobs) Remove(id string) error {
	j.mu.Lock()
	x := j.m[id]
	if x == nil || !x.CanRemove() {
		j.mu.Unlock()
		return errJobState
	}
	durable := x.Durable
	delete(j.m, id)
	j.mu.Unlock()
	if !durable {
		return nil
	}
	os.RemoveAll(j.jobDir(id))
	return j.st.DeleteJob(context.Background(), id)
}

// ClearFinished removes every finished job from the list.
func (j *Jobs) ClearFinished() error {
	var ids []string
	j.mu.Lock()
	for id, x := range j.m {
		if x.CanRemove() {
			ids = append(ids, id)
		}
	}
	j.mu.Unlock()
	for _, id := range ids {
		if err := j.Remove(id); err != nil && !errors.Is(err, errJobState) {
			return err
		}
	}
	return nil
}

// List returns active jobs first, then the most recently updated.
func (j *Jobs) List() []JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JobStatus, 0, len(j.m))
	for _, x := range j.m {
		out = append(out, x.JobStatus)
	}
	sort.Slice(out, func(a, b int) bool {
		if x, y := active(out[a]), active(out[b]); x != y {
			return x
		}
		return out[a].Updated.After(out[b].Updated)
	})
	return out
}

// AnyRunning reports whether something is running or waiting its turn.
func (j *Jobs) AnyRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.m {
		if x.Running {
			return true
		}
	}
	return false
}

// BlocksRestart reports a running job that can't continue after a restart.
func (j *Jobs) BlocksRestart() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, x := range j.m {
		if x.Running && !x.Durable {
			return true
		}
	}
	return false
}

// JobRun is a durable job's view of itself.
type JobRun struct {
	jobs     *Jobs
	job      *job
	Data     json.RawMessage
	Files    []ImportFile
	lastSave time.Time
}

// Report updates the job's status line.
func (r *JobRun) Report(msg string) { r.jobs.report(r.job, msg) }

// Load reads the last checkpoint into v, if there is one.
func (r *JobRun) Load(v any) error {
	r.jobs.mu.Lock()
	b := r.job.checkpoint
	r.jobs.mu.Unlock()
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// Save records progress, writing to the database at most every two seconds
// unless final is set.
func (r *JobRun) Save(v any, done int, final bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.jobs.mu.Lock()
	r.job.checkpoint, r.job.Done, r.job.Updated = b, done, time.Now()
	r.jobs.mu.Unlock()
	if !final && time.Since(r.lastSave) < 2*time.Second {
		return nil
	}
	r.lastSave = time.Now()
	return r.jobs.save(r.job)
}

// Context returns the engine's app-lifetime context for background jobs.
func (e *Engine) Context() context.Context { return e.ctx }
