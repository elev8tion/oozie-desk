package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// Pragmas ride the DSN so every pooled connection gets them: WAL lets
	// readers coexist with the background writers (publish jobs, pi event
	// sinks, the beacon endpoint, the fairy), busy_timeout absorbs writer
	// contention instead of surfacing SQLITE_BUSY, and foreign_keys is
	// per-connection in SQLite so setting it once on one connection is not
	// enough.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	return database, nil
}
