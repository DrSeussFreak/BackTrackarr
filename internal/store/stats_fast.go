package store

import (
	"context"
	"encoding/json"
	"time"
)

// Rebuilding totals can take many seconds on a large history, so pages must
// never wait for it. Small libraries are still computed in the request and
// stay exact; large ones show the last saved totals while a single background
// run brings them up to date.
const (
	statsBudget     = 300 * time.Millisecond
	statsMinGap     = 2 * time.Minute // at most one rebuild this often during imports
	statsRebuildMax = 15 * time.Minute
	statsSavedKey   = "stats_saved|"
)

// StatsNow returns totals without blocking on a long rebuild. updating is true
// when the numbers shown are older than the data, or not ready yet.
func (s *Store) StatsNow(ctx context.Context, user string) (st *Stats, updating bool) {
	gen, year := s.gen.Load(), time.Now().Year()
	s.statsMu.Lock()
	c, ok := s.statsCache[user]
	s.statsMu.Unlock()
	if ok && c.gen == gen && c.year == year {
		return c.st, false
	}
	if s.statsCost.Load() < int64(statsBudget) {
		quick, cancel := context.WithTimeout(ctx, statsBudget)
		fresh, err := s.computeAndKeep(quick, user, gen, year)
		cancel()
		if err == nil {
			return fresh, false
		}
		s.statsCost.Store(int64(statsBudget)) // too big to build during a request
	}
	if !ok {
		if saved := s.loadSavedStats(ctx, user); saved != nil {
			c, ok = cachedStats{gen: -1, year: year, st: saved}, true
			s.keepStats(user, c)
		}
	}
	s.rebuildStats(user, !ok)
	if ok {
		return c.st, true
	}
	return &Stats{}, true
}

func (s *Store) keepStats(user string, c cachedStats) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.statsCache == nil || len(s.statsCache) > 64 {
		s.statsCache = map[string]cachedStats{}
	}
	s.statsCache[user] = c
}

func (s *Store) computeAndKeep(ctx context.Context, user string, gen int64, year int) (*Stats, error) {
	start := time.Now()
	st, err := s.computeStats(ctx, user)
	if err != nil {
		return nil, err
	}
	s.statsCost.Store(int64(time.Since(start)))
	s.keepStats(user, cachedStats{gen: gen, year: year, st: st})
	return st, nil
}

// rebuildStats runs one rebuild at a time, independent of any page request, so
// closing a page doesn't throw the work away.
func (s *Store) rebuildStats(user string, force bool) {
	if !force && time.Since(time.Unix(s.statsLastRun.Load(), 0)) < statsMinGap {
		return
	}
	if !s.statsBusy.CompareAndSwap(false, true) {
		return
	}
	s.statsLastRun.Store(time.Now().Unix())
	go func() {
		defer s.statsBusy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), statsRebuildMax)
		defer cancel()
		st, err := s.computeAndKeep(ctx, user, s.gen.Load(), time.Now().Year())
		if err != nil || len(st.Years) == 0 {
			return // an unknown user name has nothing worth saving
		}
		if b, err := json.Marshal(st); err == nil {
			_ = s.SetKV(ctx, statsSavedKey+user, string(b))
		}
	}()
}

func (s *Store) loadSavedStats(ctx context.Context, user string) *Stats {
	v, err := s.GetKV(ctx, statsSavedKey+user)
	if err != nil || v == "" {
		return nil
	}
	st := &Stats{}
	if json.Unmarshal([]byte(v), st) != nil {
		return nil
	}
	for _, y := range st.Years {
		if y.Plays > st.MaxYear {
			st.MaxYear = y.Plays // not part of the saved form
		}
	}
	return st
}
