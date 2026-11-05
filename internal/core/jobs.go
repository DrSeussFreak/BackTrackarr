package core

import (
	"context"
	"sort"
	"sync"
	"time"
)

type JobStatus struct {
	Name    string
	Running bool
	Message string
	Err     string
	Updated time.Time
}

// Jobs tracks long-running background tasks (imports, pushes, backups).
type Jobs struct {
	mu sync.Mutex
	m  map[string]*JobStatus
}

func NewJobs() *Jobs { return &Jobs{m: map[string]*JobStatus{}} }

// Start runs fn in the background unless a job with that name is running.
func (j *Jobs) Start(ctx context.Context, name string, fn func(ctx context.Context, report func(string)) error) bool {
	j.mu.Lock()
	if s, ok := j.m[name]; ok && s.Running {
		j.mu.Unlock()
		return false
	}
	j.m[name] = &JobStatus{Name: name, Running: true, Message: "starting", Updated: time.Now()}
	j.mu.Unlock()
	go func() {
		err := fn(ctx, func(msg string) {
			j.mu.Lock()
			j.m[name].Message = msg
			j.m[name].Updated = time.Now()
			j.mu.Unlock()
		})
		j.mu.Lock()
		s := j.m[name]
		s.Running = false
		s.Updated = time.Now()
		if err != nil {
			s.Err = err.Error()
		}
		j.mu.Unlock()
	}()
	return true
}

func (j *Jobs) List() []JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JobStatus, 0, len(j.m))
	for _, s := range j.m {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Updated.After(out[b].Updated) })
	return out
}

func (j *Jobs) AnyRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, s := range j.m {
		if s.Running {
			return true
		}
	}
	return false
}

// Context returns the engine's app-lifetime context for background jobs.
func (e *Engine) Context() context.Context { return e.ctx }
