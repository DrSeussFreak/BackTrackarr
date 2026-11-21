package store

import (
	"context"
	"time"
)

// Settings returns every value saved from the web UI.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT k, v FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings applies changes all together, or not at all. Keys in remove
// go back to their environment value or default.
func (s *Store) SaveSettings(ctx context.Context, set map[string]string, remove []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for k, v := range set {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(k,v,updated_at) VALUES(?,?,?)
			ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`, k, v, now); err != nil {
			return err
		}
	}
	for _, k := range remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE k=?`, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}
