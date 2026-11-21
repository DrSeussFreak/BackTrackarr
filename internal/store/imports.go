package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// SaveImportExtra keeps anything from an export that has no place in plays or
// ratings (watchlists, lists, comments, show ratings), so nothing is lost. The
// same record imported twice is stored once.
func (s *Store) SaveImportExtra(ctx context.Context, source, kind, title string, raw []byte) (bool, error) {
	h := sha256.Sum256(append([]byte(source+"\x00"+kind+"\x00"), raw...))
	res, err := s.q(ctx).ExecContext(ctx, `INSERT OR IGNORE INTO import_extras(hash,source,kind,title,raw,imported_at) VALUES(?,?,?,?,?,?)`,
		hex.EncodeToString(h[:16]), source, kind, ns(title), pack(raw), time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ImportExtraCounts summarizes kept records by source and kind.
func (s *Store) ImportExtraCounts(ctx context.Context) ([]NameCount, error) {
	return s.nameCounts(ctx, `SELECT source || ': ' || kind, COUNT(*) c FROM import_extras GROUP BY source, kind ORDER BY c DESC LIMIT 30`)
}
