package store

import (
	"context"
	"testing"
	"time"
)

// A long write elsewhere must delay a transaction, not fail it.
func TestInTxWaitsPastBusyTimeout(t *testing.T) {
	old := busyTimeoutMS
	busyTimeoutMS = 100
	defer func() { busyTimeoutMS = old }()
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
		time.Sleep(700 * time.Millisecond)
		holder.Rollback()
	}()
	ran := false
	if err := s.InTx(ctx, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran %v, err %v", ran, err)
	}
}
