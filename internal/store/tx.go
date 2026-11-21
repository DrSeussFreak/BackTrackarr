package store

import (
	"context"
	"database/sql"
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	return tx.Commit()
}
