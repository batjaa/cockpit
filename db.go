package main

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// dbTimeFormat is the canonical on-disk format for timestamps. Recognized
// by SQLite's date/time functions and round-trippable to time.Time via
// modernc.org/sqlite's Scan path.
const dbTimeFormat = "2006-01-02 15:04:05.000"

// dbTime normalizes a time.Time for DB binding. The driver's default
// time.Time binding uses time.Time.String() which embeds monotonic clock
// state and breaks SQLite's date/time functions.
func dbTime(t time.Time) string {
	return t.UTC().Format(dbTimeFormat)
}

//go:embed schema.sql
var schemaSQL string

// OpenDB opens (or creates) the SQLite database at path and applies the
// embedded schema. Safe to call repeatedly — the schema is idempotent.
func OpenDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Column additions can't be expressed in the idempotent CREATE IF NOT
	// EXISTS schema — apply them here, tolerating "duplicate column".
	migrations := []string{
		`ALTER TABLE prs ADD COLUMN state TEXT NOT NULL DEFAULT 'OPEN'`,
		`ALTER TABLE sessions ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE prs ADD COLUMN pr_created_at DATETIME`,
		`ALTER TABLE prs ADD COLUMN pr_updated_at DATETIME`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				db.Close()
				return nil, fmt.Errorf("migrate (%s): %w", m, err)
			}
		}
	}
	return db, nil
}
