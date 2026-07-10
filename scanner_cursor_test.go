package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// buildCursorFixture creates a synthetic state.vscdb at dir/state.vscdb with
// the real cursorDiskKV(key TEXT UNIQUE, value BLOB) schema, and inserts the
// given key->value rows. Values are stored as BLOBs to mirror the real DB.
func buildCursorFixture(t *testing.T, dir string, rows map[string]string) string {
	t.Helper()
	dbPath := filepath.Join(dir, "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, []byte(v)); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture db: %v", err)
	}
	return dbPath
}

// indexByKey maps SessionKey -> record for convenient assertions.
func indexByKey(recs []SessionRecord) map[string]SessionRecord {
	m := make(map[string]SessionRecord, len(recs))
	for _, r := range recs {
		m[r.SessionKey] = r
	}
	return m
}

func TestScanCursorStore_MixedRecords(t *testing.T) {
	// createdAt / lastUpdatedAt are epoch ms. Pick fixed values.
	const (
		createdMs = int64(1781000000000) // ~2026-06-09
		updatedMs = int64(1781200000000)
	)

	rows := map[string]string{
		// 1. Complete record: name, subtitle, timestamps, headers, repoPath.
		"composerData:11111111-1111-1111-1111-111111111111": `{
			"_v": 16,
			"composerId": "11111111-1111-1111-1111-111111111111",
			"name": "Fix flaky retry test",
			"subtitle": "Edited retry_test.go, retry.go",
			"createdAt": 1781000000000,
			"lastUpdatedAt": 1781200000000,
			"unifiedMode": "agent",
			"fullConversationHeadersOnly": [{"bubbleId":"a"},{"bubbleId":"b"},{"bubbleId":"c"}],
			"trackedGitRepos": [{"repoPath": "/home/ubuntu/co/backend"}]
		}`,

		// 2. Minimal record: only composerId + lastUpdatedAt (and a message
		//    so it is not treated as a draft).
		"composerData:22222222-2222-2222-2222-222222222222": `{
			"composerId": "22222222-2222-2222-2222-222222222222",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": [{"bubbleId":"x"}]
		}`,

		// 3. Corrupt JSON: must be skipped, must not error the whole scan.
		"composerData:33333333-3333-3333-3333-333333333333": `{ this is not valid json `,

		// 4. Draft: empty name AND zero messages -> skipped.
		"composerData:44444444-4444-4444-4444-444444444444": `{
			"composerId": "44444444-4444-4444-4444-444444444444",
			"name": "",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": []
		}`,

		// 5. Giant unrelated key prefix: ignored by the LIKE filter. Make the
		//    value large to prove we never read non-composerData blobs.
		"bubbleData:99999999": `{"blob": "` + strings.Repeat("x", 200000) + `"}`,

		// 6. lastUpdatedAt missing -> falls back to createdAt. Also exercises
		//    workspaceIdentifier configPath fallback for ProjectDir.
		"composerData:66666666-6666-6666-6666-666666666666": `{
			"composerId": "66666666-6666-6666-6666-666666666666",
			"name": "Plan AII-3229",
			"createdAt": 1781000000000,
			"fullConversationHeadersOnly": [{"bubbleId":"p"},{"bubbleId":"q"}],
			"workspaceIdentifier": {"configPath": {"path": "/home/ubuntu/co/backend/backend.code-workspace", "scheme": "file"}}
		}`,
	}

	dbPath := buildCursorFixture(t, t.TempDir(), rows)

	recs, err := ScanCursorStore(dbPath, "local", time.Time{})
	if err != nil {
		t.Fatalf("ScanCursorStore: %v", err)
	}

	byKey := indexByKey(recs)

	// Three valid records survive (1, 2, 6); the corrupt-JSON record, the
	// draft, and the giant non-composerData key are all excluded.
	if len(recs) != 3 {
		var keys []string
		for _, r := range recs {
			keys = append(keys, r.SessionKey)
		}
		sort.Strings(keys)
		t.Fatalf("expected 3 records, got %d: %v", len(recs), keys)
	}
	if _, ok := byKey["33333333-3333-3333-3333-333333333333"]; ok {
		t.Error("corrupt-JSON record should be skipped")
	}
	if _, ok := byKey["44444444-4444-4444-4444-444444444444"]; ok {
		t.Error("draft record (empty name, 0 messages) should be skipped")
	}
	if _, ok := byKey["99999999"]; ok {
		t.Error("non-composerData key should never be scanned")
	}

	// 1. Complete record fully mapped.
	c := byKey["11111111-1111-1111-1111-111111111111"]
	if c.Agent != "cursor" {
		t.Errorf("Agent = %q, want cursor", c.Agent)
	}
	if c.Machine != "local" {
		t.Errorf("Machine = %q, want local", c.Machine)
	}
	if c.Title != "Fix flaky retry test" {
		t.Errorf("Title = %q", c.Title)
	}
	if c.Subtitle != "Edited retry_test.go, retry.go" {
		t.Errorf("Subtitle = %q", c.Subtitle)
	}
	if c.ProjectDir != "/home/ubuntu/co/backend" {
		t.Errorf("ProjectDir = %q, want /home/ubuntu/co/backend", c.ProjectDir)
	}
	if c.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3", c.MessageCount)
	}
	if c.ResumeCmd != "" {
		t.Errorf("ResumeCmd = %q, want empty (GUI-only)", c.ResumeCmd)
	}
	if !c.StartedAt.Equal(time.UnixMilli(createdMs).UTC()) {
		t.Errorf("StartedAt = %v, want %v", c.StartedAt, time.UnixMilli(createdMs).UTC())
	}
	if !c.LastActive.Equal(time.UnixMilli(updatedMs).UTC()) {
		t.Errorf("LastActive = %v, want %v", c.LastActive, time.UnixMilli(updatedMs).UTC())
	}

	// 2. Minimal record: title falls back to the composerId; StartedAt zero.
	m := byKey["22222222-2222-2222-2222-222222222222"]
	if m.Title != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("minimal Title = %q, want composerId fallback", m.Title)
	}
	if !m.StartedAt.IsZero() {
		t.Errorf("minimal StartedAt = %v, want zero", m.StartedAt)
	}
	if !m.LastActive.Equal(time.UnixMilli(updatedMs).UTC()) {
		t.Errorf("minimal LastActive = %v", m.LastActive)
	}
	if m.ProjectDir != "" {
		t.Errorf("minimal ProjectDir = %q, want empty", m.ProjectDir)
	}

	// 6. lastUpdatedAt fallback to createdAt; workspaceIdentifier ProjectDir.
	f := byKey["66666666-6666-6666-6666-666666666666"]
	if !f.LastActive.Equal(time.UnixMilli(createdMs).UTC()) {
		t.Errorf("fallback LastActive = %v, want createdAt", f.LastActive)
	}
	if f.ProjectDir != "/home/ubuntu/co/backend/backend.code-workspace" {
		t.Errorf("fallback ProjectDir = %q", f.ProjectDir)
	}
}

func TestScanCursorStore_SinceFilter(t *testing.T) {
	rows := map[string]string{
		"composerData:old": `{
			"composerId": "old",
			"name": "Old session",
			"lastUpdatedAt": 1781000000000,
			"fullConversationHeadersOnly": [{"b":1}]
		}`,
		"composerData:new": `{
			"composerId": "new",
			"name": "New session",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": [{"b":1}]
		}`,
	}
	dbPath := buildCursorFixture(t, t.TempDir(), rows)

	// since sits between the two lastUpdatedAt values.
	since := time.UnixMilli(1781100000000).UTC()
	recs, err := ScanCursorStore(dbPath, "local", since)
	if err != nil {
		t.Fatalf("ScanCursorStore: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record after since-filter, got %d", len(recs))
	}
	if recs[0].SessionKey != "new" {
		t.Errorf("kept %q, want the newer session", recs[0].SessionKey)
	}
}

func TestScanCursorStore_SinceUsesFallbackTimestamp(t *testing.T) {
	// A record with no lastUpdatedAt must be filtered on createdAt, not kept
	// by accident.
	rows := map[string]string{
		"composerData:onlycreated": `{
			"composerId": "onlycreated",
			"name": "Created only",
			"createdAt": 1781000000000,
			"fullConversationHeadersOnly": [{"b":1}]
		}`,
	}
	dbPath := buildCursorFixture(t, t.TempDir(), rows)

	since := time.UnixMilli(1781100000000).UTC() // after createdAt
	recs, err := ScanCursorStore(dbPath, "local", since)
	if err != nil {
		t.Fatalf("ScanCursorStore: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected 0 records (createdAt before since), got %d", len(recs))
	}
}

func TestScanCursorStore_MissingDB(t *testing.T) {
	// A nonexistent path must return (nil, nil) — never an error that could
	// break the overall scan.
	recs, err := ScanCursorStore(filepath.Join(t.TempDir(), "nope.vscdb"), "local", time.Time{})
	if err != nil {
		t.Fatalf("missing DB returned error: %v", err)
	}
	if recs != nil {
		t.Fatalf("missing DB returned %d records, want nil", len(recs))
	}
}

func TestScanCursorStore_EmptyPath(t *testing.T) {
	recs, err := ScanCursorStore("", "local", time.Time{})
	if err != nil {
		t.Fatalf("empty path returned error: %v", err)
	}
	if recs != nil {
		t.Fatalf("empty path returned %d records, want nil", len(recs))
	}
}

func TestScanCursorStore_NotASQLiteDB(t *testing.T) {
	// A file that exists but is not a usable SQLite DB (e.g. truncated /
	// garbage) must degrade to (nil, nil), not error.
	dir := t.TempDir()
	bad := filepath.Join(dir, "state.vscdb")
	if err := os.WriteFile(bad, []byte("not a database at all"), 0o644); err != nil {
		t.Fatalf("write bad file: %v", err)
	}
	recs, err := ScanCursorStore(bad, "local", time.Time{})
	if err != nil {
		t.Fatalf("bad DB returned error: %v", err)
	}
	if recs != nil {
		t.Fatalf("bad DB returned %d records, want nil", len(recs))
	}
}

func TestScanCursorStore_VersionDrift(t *testing.T) {
	// Unknown / very different _v values still parse best-effort.
	rows := map[string]string{
		"composerData:future": `{
			"_v": 999,
			"composerId": "future",
			"name": "Future schema",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": [{"b":1},{"b":2}],
			"someBrandNewField": {"nested": true}
		}`,
		"composerData:novfield": `{
			"composerId": "novfield",
			"subtitle": "no name, has subtitle",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": [{"b":1}]
		}`,
	}
	dbPath := buildCursorFixture(t, t.TempDir(), rows)

	recs, err := ScanCursorStore(dbPath, "local", time.Time{})
	if err != nil {
		t.Fatalf("ScanCursorStore: %v", err)
	}
	byKey := indexByKey(recs)
	if len(recs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(recs))
	}
	if byKey["future"].MessageCount != 2 {
		t.Errorf("future record MessageCount = %d, want 2", byKey["future"].MessageCount)
	}
	// Title falls back to subtitle when name is absent.
	if got := byKey["novfield"].Title; got != "no name, has subtitle" {
		t.Errorf("novfield Title = %q, want subtitle fallback", got)
	}
}

func TestScanCursorStore_SessionKeyFallsBackToKeySuffix(t *testing.T) {
	// composerId missing -> key suffix used as SessionKey.
	rows := map[string]string{
		"composerData:abc-def": `{
			"name": "No composerId field",
			"lastUpdatedAt": 1781200000000,
			"fullConversationHeadersOnly": [{"b":1}]
		}`,
	}
	dbPath := buildCursorFixture(t, t.TempDir(), rows)
	recs, err := ScanCursorStore(dbPath, "local", time.Time{})
	if err != nil {
		t.Fatalf("ScanCursorStore: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].SessionKey != "abc-def" {
		t.Errorf("SessionKey = %q, want key-suffix fallback abc-def", recs[0].SessionKey)
	}
}

// TestScanCursorStore_SkipsSubcomposers: best-of-N subcomposers are
// spawned children of a parent composer, not user sessions.
func TestScanCursorStore_SkipsSubcomposers(t *testing.T) {
	dbPath := buildCursorFixture(t, t.TempDir(), map[string]string{
		"composerData:parent": `{"composerId":"parent","name":"Real session","createdAt":1781270000000,"lastUpdatedAt":1781270046084,"fullConversationHeadersOnly":[{},{}]}`,
		"composerData:subbed": `{"composerId":"subbed","name":"BoN child","createdAt":1781270000000,"lastUpdatedAt":1781270046084,"isBestOfNSubcomposer":true,"fullConversationHeadersOnly":[{},{}]}`,
	})
	recs, err := ScanCursorStore(dbPath, "local", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].SessionKey != "parent" {
		t.Errorf("want only parent, got %+v", recs)
	}
}
