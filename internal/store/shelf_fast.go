package store

import (
	"context"
	"time"
)

// ShelfStatsNow is ShelfStats for pages that must not wait: small shelves are
// computed in the request, large ones return the last result (or nil before
// the first one exists) while a background run updates them.
func (s *Store) ShelfStatsNow(ctx context.Context, mediaType string) (st *ShelfStats, updating bool) {
	gen, year := s.shelfGen.Load(), time.Now().Year()
	s.shelfMu.Lock()
	c, ok := s.shelfCache[mediaType]
	s.shelfMu.Unlock()
	if ok && c.gen == gen && c.year == year {
		return c.st, false
	}
	if s.shelfCost.Load() < int64(statsBudget) {
		quick, cancel := context.WithTimeout(ctx, statsBudget)
		start := time.Now()
		fresh, err := s.ShelfStats(quick, mediaType)
		cancel()
		if err == nil {
			s.shelfCost.Store(int64(time.Since(start)))
			return fresh, false
		}
		s.shelfCost.Store(int64(statsBudget))
	}
	s.WarmShelfStats()
	if ok {
		return c.st, true
	}
	return nil, true
}

// WarmShelfStats rebuilds book and game totals in the background, one run at a
// time and independent of any page request.
func (s *Store) WarmShelfStats() {
	if !s.shelfBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.shelfBusy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), statsRebuildMax)
		defer cancel()
		start := time.Now()
		for _, mediaType := range []string{"", "game"} {
			if _, err := s.ShelfStats(ctx, mediaType); err != nil {
				return
			}
		}
		s.shelfCost.Store(int64(time.Since(start)))
	}()
}
