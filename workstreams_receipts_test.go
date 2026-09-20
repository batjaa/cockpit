package main

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMapCommands_OperationIdentityBindsSubmittedContent(t *testing.T) {
	h, store := newCommandHTTP(t)
	command := WorkstreamCommand{OperationID: "receipt-content", Action: "create_workstream", Workstream: WorkstreamInput{Name: "Credits", Outcome: "Ship credits"}}
	code, first := commandHTTP(t, h, command)
	if code != http.StatusOK {
		t.Fatalf("initial create: %d", code)
	}
	changed := command
	changed.Workstream.Name = "Different submitted draft"
	if code, _ := commandHTTP(t, h, changed); code != http.StatusUnprocessableEntity {
		t.Fatalf("reused identity with changed content: %d, want 422", code)
	}
	changed = command
	changed.Action = "archive_workstream"
	changed.WorkstreamID = first.WorkstreamID
	changed.ExpectedRevision = first.Revision
	changed.Confirm = true
	if code, _ := commandHTTP(t, h, changed); code != http.StatusUnprocessableEntity {
		t.Fatalf("reused identity with different action: %d, want 422", code)
	}
	// Receipt validation survives a new store instance; genuine retries still
	// return the original result without another mutation or history event.
	reopened := NewWorkstreamStore(store.db, time.Now, time.UTC)
	again, err := reopened.Execute(context.Background(), command)
	if err != nil || !again.Idempotent || again.WorkstreamID != first.WorkstreamID {
		t.Fatalf("original retry: %+v, %v", again, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM workstreams`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("workstream count %d, %v", count, err)
	}
}

func TestMapMigrationAddsRetryAndReceiptColumnsBeforeIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "early-map.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	early := strings.NewReplacer(
		"  attempt_count INTEGER NOT NULL DEFAULT 0,\n", "",
		"  next_attempt_at DATETIME,\n", "",
		"  last_operational_state TEXT NOT NULL DEFAULT '',\n", "",
		"  request_hash TEXT NOT NULL DEFAULT '',\n", "",
		"CREATE INDEX IF NOT EXISTS idx_mirror_pending ON mirror_states(status, next_attempt_at, entity_type, entity_id);", "",
	).Replace(schemaSQL)
	if _, err := db.Exec(early); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < 2; i++ {
		db, err = OpenDB(path)
		if err != nil {
			t.Fatalf("upgrade attempt %d: %v", i, err)
		}
		if _, err := db.Exec(`SELECT attempt_count,next_attempt_at,last_operational_state FROM mirror_states; SELECT request_hash FROM operation_receipts`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}
