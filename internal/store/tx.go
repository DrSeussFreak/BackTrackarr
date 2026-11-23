package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"modernc.org/sqlite"
)

// querier is what *sql.DB and *sql.Tx have in common.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{}

// q returns the transaction carried by ctx, or the database.
func (s *Store) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return tx
	}
	return s.DB
}

// InTx runs fn in one transaction, committed if fn returns nil. Bulk imports
// use it because one commit per batch is far faster than one per row. Keep
// network calls out of fn, since it holds SQLite's single write lock.
func (s *Store) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, nested := ctx.Value(txKey{}).(*sql.Tx); nested {
		return fn(ctx)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// How long a transaction keeps waiting for its turn to write.
const beginWait = 2 * time.Minute

// begin takes the write lock, waiting through another writer. Nothing has run
// yet when this fails, so trying again is always safe.
func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	deadline := time.Now().Add(beginWait)
	for {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return tx, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == 5 // SQLITE_BUSY and its variants
}
