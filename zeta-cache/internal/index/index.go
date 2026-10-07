// Package index opens and migrates the client-side SQLite index DB.
//
// Leaf 01 scope only: open/create the database file (modernc.org/sqlite,
// pure Go, no cgo) and stamp PRAGMA user_version = 0 as a placeholder so
// the daemon lifecycle has a real DB handle. The real schema, WAL mode,
// resources table, journal, and migrations are owned by leaf 03
// (index-journal), which will own user_version >= 1.
package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	// Pure-Go SQLite driver: the repo bans cgo repo-wide, and this import
	// is the only sanctioned way to reach SQLite.
	_ "modernc.org/sqlite"
)

// Open opens (creating if absent) the index database at path. The parent
// directory is created if missing. The returned DB is stamped with
// user_version = 0 (placeholder schema; leaf 03 migrates it).
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("index: creating %s: %w", dir, err)
		}
	}
	// _pragma settings: busy_timeout is defensive; foreign_keys is off
	// until leaf 03 defines the schema that would use it.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("index: opening %s: %w", path, err)
	}
	// modernc.org/sqlite is not safe for concurrent writes by default;
	// leaf 01 serializes all access to the single placeholder statement.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: stamping user_version: %w", err)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: reading user_version: %w", err)
	}
	if v != 0 {
		_ = db.Close()
		return nil, fmt.Errorf("index: user_version %d is not 0; leaf 03 owns migrations", v)
	}
	return db, nil
}
