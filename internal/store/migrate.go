package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaVersion is stored in PRAGMA user_version. Bump it and add a step to
// migrate() whenever an existing table changes; schema.sql then only needs
// to describe the latest shape (CREATE ... IF NOT EXISTS adds new tables and
// indexes on its own).
//
//	1: first release (movies and episodes)
//	2: music (media.album, media.mbid, 'track' kind) and private plays
const schemaVersion = 2

func hasTable(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0, err
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n)
	return n > 0, err
}

// migrate upgrades a database written by an older version, in place and
// without losing data. A fresh database is left for schema.sql to create.
func migrate(db *sql.DB) error {
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	exists, err := hasTable(db, "media")
	if err != nil || !exists {
		return err
	}
	if ver == 0 {
		// Version 0 means "unknown": databases made before versioning. The
		// columns tell which release wrote it.
		ver = 1
		if ok, err := hasColumn(db, "media", "mbid"); err != nil {
			return err
		} else if ok {
			ver = 2
		}
	}
	if ver < 2 {
		if err := migrateV2(db); err != nil {
			return fmt.Errorf("to version 2: %w", err)
		}
	}
	return nil
}

// migrateV2 rebuilds the media table (SQLite can't change a CHECK
// constraint in place) and adds plays.private.
func migrateV2(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx) // PRAGMA foreign_keys applies per connection
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)

	create := strings.SplitN(schema, ";\n", 2)[0]
	if !strings.HasPrefix(create, "CREATE TABLE IF NOT EXISTS media (") {
		return fmt.Errorf("schema.sql must start with the media table")
	}
	create = strings.Replace(create, "media (", "media_v2 (", 1)
	const cols = `id,kind,title,year,show_title,season,episode,duration_s,imdb,tmdb,tvdb,trakt,
		show_imdb,show_tmdb,show_tvdb,show_trakt,plex_guid,plex_key,updated_at`
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DROP TABLE IF EXISTS media_v2`,
		create,
		`INSERT INTO media_v2 (` + cols + `) SELECT ` + cols + ` FROM media`,
		`DROP TABLE media`,
		`ALTER TABLE media_v2 RENAME TO media`,
		`ALTER TABLE plays ADD COLUMN private INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%w\n%s", err, q)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	bad := rows.Next()
	rows.Close()
	if bad {
		return fmt.Errorf("foreign key check failed after rebuilding media")
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
		return err
	}
	return tx.Commit()
}
