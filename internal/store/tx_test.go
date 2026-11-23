package store

import (
	"context"
	"testing"
	"time"
)

// A long write elsewhere must delay a transaction, not fail it.
func TestInTxWaitsPastBusyTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 10s busy timeout")
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	holder, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(11 * time.Second)
		holder.Rollback()
	}()
	ran := false
	if err := s.InTx(ctx, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran %v, err %v", ran, err)
	}
}
