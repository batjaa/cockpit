package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOpenDBAddsStructuredReviewColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", fmt.Sprintf("file:%s", path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE reviews (
			id INTEGER PRIMARY KEY,
			pr_id INTEGER NOT NULL,
			run_id INTEGER NOT NULL,
			head_sha TEXT NOT NULL,
			summary TEXT,
			raw_output TEXT,
			state TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			posted_at DATETIME,
			github_review_id INTEGER
		);
		INSERT INTO reviews (id, pr_id, run_id, head_sha, summary, state, created_at)
		VALUES (1, 1, 1, 'old-sha', 'legacy message', 'pending', CURRENT_TIMESTAMP);
	`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var summary, brief, message string
	if err := db.QueryRow(`
		SELECT summary, review_brief, author_message FROM reviews WHERE id=1
	`).Scan(&summary, &brief, &message); err != nil {
		t.Fatal(err)
	}
	if summary != "legacy message" || brief != "" || message != "" {
		t.Errorf("migrated row summary=%q brief=%q message=%q", summary, brief, message)
	}
}
