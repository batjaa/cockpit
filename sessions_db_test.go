package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func newSessionsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fetchOne reads a single session back by id for assertions.
func fetchOne(t *testing.T, db *sql.DB, id int64) SessionRow {
	t.Helper()
	rows, err := ListSessions(context.Background(), db, SessionFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("session id %d not found", id)
	return SessionRow{}
}

func TestUpsertSession_InsertThenUpdatePreserves(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)

	started := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	first := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	id, err := UpsertSession(ctx, db, SessionRecord{
		Agent:        "claude",
		Machine:      "local",
		SessionKey:   "sess-1",
		ProjectDir:   "/Users/foo/git/bar",
		Title:        "Initial title",
		Subtitle:     "first",
		ResumeCmd:    "cd /Users/foo/git/bar && claude --resume sess-1",
		StartedAt:    started,
		LastActive:   first,
		MessageCount: 3,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	// Archive it so we can verify the flag survives the update.
	if err := SetSessionArchived(ctx, db, id, true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	second := time.Date(2026, 6, 2, 11, 30, 0, 0, time.UTC)
	id2, err := UpsertSession(ctx, db, SessionRecord{
		Agent:        "claude",
		Machine:      "local",
		SessionKey:   "sess-1",
		ProjectDir:   "/Users/foo/git/baz", // changed
		Title:        "Updated title",      // changed
		Subtitle:     "latest",             // changed
		ResumeCmd:    "cd /Users/foo/git/baz && claude --resume sess-1",
		StartedAt:    time.Time{}, // intentionally zero; must NOT clobber stored started_at
		LastActive:   second,
		MessageCount: 12,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id2 != id {
		t.Fatalf("upsert created a new row: got id %d want %d", id2, id)
	}

	got := fetchOne(t, db, id)
	if got.Title != "Updated title" {
		t.Errorf("title not refreshed: %q", got.Title)
	}
	if got.Subtitle != "latest" {
		t.Errorf("subtitle not refreshed: %q", got.Subtitle)
	}
	if got.ProjectDir != "/Users/foo/git/baz" {
		t.Errorf("project_dir not refreshed: %q", got.ProjectDir)
	}
	if got.MessageCount != 12 {
		t.Errorf("message_count not refreshed: %d", got.MessageCount)
	}
	if !got.LastActive.Equal(second) {
		t.Errorf("last_active not refreshed: got %v want %v", got.LastActive, second)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at not preserved: got %v want %v", got.StartedAt, started)
	}
	if !got.Archived {
		t.Errorf("archived flag not preserved across update")
	}
}

func TestUpsertSession_ZeroStartedAt(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)

	id, err := UpsertSession(ctx, db, SessionRecord{
		Agent:      "codex",
		Machine:    "local",
		SessionKey: "cdx-1",
		Title:      "No start time",
		StartedAt:  time.Time{},
		LastActive: time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := fetchOne(t, db, id)
	if !got.StartedAt.IsZero() {
		t.Errorf("expected zero started_at, got %v", got.StartedAt)
	}
}

func TestReplaceSessionTickets(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)

	id, err := UpsertSession(ctx, db, SessionRecord{
		Agent:      "claude",
		Machine:    "local",
		SessionKey: "sess-t",
		Title:      "ticketed",
		LastActive: time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"initial set", []string{"PLAT-422", "org/repo#123"}, []string{"PLAT-422", "org/repo#123"}},
		{"replace with dedup", []string{"ENG-1", "ENG-1", "ENG-2"}, []string{"ENG-1", "ENG-2"}},
		{"skip empties", []string{"", "ONLY-1", ""}, []string{"ONLY-1"}},
		{"clear all", []string{}, nil},
		{"clear nil", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ReplaceSessionTickets(ctx, db, id, tc.in); err != nil {
				t.Fatalf("replace: %v", err)
			}
			got := fetchOne(t, db, id).Tickets
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if len(got) == 0 && len(want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("tickets = %v, want %v", got, want)
			}
		})
	}
}

func TestListSessions_Filters(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)

	base := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	// Three sessions with distinct agent/machine/title so filters bite.
	idA, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "a",
		Title: "Fix flaky retry test", ProjectDir: "/Users/foo/git/backend",
		LastActive: base.Add(3 * time.Hour),
	})
	idB, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "codex", Machine: "devbox", SessionKey: "b",
		Title: "Add pagination", ProjectDir: "/srv/api",
		LastActive: base.Add(2 * time.Hour),
	})
	idC, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "cursor", Machine: "local", SessionKey: "c",
		Title: "Refactor RETRY handler", ProjectDir: "/Users/foo/git/frontend",
		LastActive: base.Add(1 * time.Hour),
	})
	if err := ReplaceSessionTickets(ctx, db, idB, []string{"PLAT-422"}); err != nil {
		t.Fatalf("tickets: %v", err)
	}

	ids := func(rows []SessionRow) []int64 {
		var out []int64
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	tests := []struct {
		name string
		f    SessionFilter
		want []int64 // expected ids in result order
	}{
		{"no filter, newest first", SessionFilter{}, []int64{idA, idB, idC}},
		{"agent claude", SessionFilter{Agent: "claude"}, []int64{idA}},
		{"machine local", SessionFilter{Machine: "local"}, []int64{idA, idC}},
		{"machine devbox", SessionFilter{Machine: "devbox"}, []int64{idB}},
		{"ticket match", SessionFilter{Ticket: "PLAT-422"}, []int64{idB}},
		{"ticket miss", SessionFilter{Ticket: "NOPE-1"}, nil},
		{"search title ci", SessionFilter{Search: "retry"}, []int64{idA, idC}},
		{"search project dir", SessionFilter{Search: "backend"}, []int64{idA}},
		{"search no match", SessionFilter{Search: "zzz"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := ListSessions(ctx, db, tc.f)
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			got := ids(rows)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListSessions_SearchEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)
	now := time.Now()
	idLiteral, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "lit",
		Title: "100% done", LastActive: now,
	})
	_, _ = UpsertSession(ctx, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "other",
		Title: "nothing special", LastActive: now.Add(-time.Minute),
	})

	rows, err := ListSessions(ctx, db, SessionFilter{Search: "100%"})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != idLiteral {
		t.Fatalf("wildcard not escaped: got %d rows %v", len(rows), rows)
	}
}

func TestListSessions_ArchivedExcludedByDefault(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)
	now := time.Now()
	visible, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "vis",
		Title: "visible", LastActive: now,
	})
	hidden, _ := UpsertSession(ctx, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "hid",
		Title: "hidden", LastActive: now.Add(-time.Minute),
	})
	if err := SetSessionArchived(ctx, db, hidden, true); err != nil {
		t.Fatalf("archive: %v", err)
	}

	def, err := ListSessions(ctx, db, SessionFilter{})
	if err != nil {
		t.Fatalf("list default: %v", err)
	}
	if len(def) != 1 || def[0].ID != visible {
		t.Errorf("default listing should exclude archived: got %v", def)
	}

	all, err := ListSessions(ctx, db, SessionFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("IncludeArchived should return both: got %d", len(all))
	}
}

func TestSetSessionArchived_Missing(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)
	err := SetSessionArchived(ctx, db, 99999, true)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows for missing session, got %v", err)
	}
}

func TestStaleComputation(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)
	now := time.Now()

	tests := []struct {
		name      string
		idleAgo   time.Duration
		messages  int
		archived  bool
		wantStale bool
	}{
		{"fresh (active <24h)", 2 * time.Hour, 30, false, false},
		{"stale: 2d idle, enough msgs", 2 * 24 * time.Hour, 12, false, true},
		{"too old (>14d)", 20 * 24 * time.Hour, 30, false, false},
		{"too few messages", 2 * 24 * time.Hour, 9, false, false},
		{"exactly 10 messages at 2d", 2 * 24 * time.Hour, 10, false, true},
		{"archived never stale", 2 * 24 * time.Hour, 30, true, false},
		{"just past 24h boundary", 25 * time.Hour, 15, false, true},
		{"just under 14d boundary", 13 * 24 * time.Hour, 15, false, true},
	}

	var wantCount int
	for i, tc := range tests {
		key := "stale-" + string(rune('a'+i))
		id, err := UpsertSession(ctx, db, SessionRecord{
			Agent: "claude", Machine: "local", SessionKey: key,
			Title:      tc.name,
			LastActive: now.Add(-tc.idleAgo),
			// vary message_count; staleness gate uses >= 10
			MessageCount: tc.messages,
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", tc.name, err)
		}
		if tc.archived {
			if err := SetSessionArchived(ctx, db, id, true); err != nil {
				t.Fatalf("archive %s: %v", tc.name, err)
			}
		}
		if tc.wantStale {
			wantCount++
		}
	}

	rows, err := ListSessions(ctx, db, SessionFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byTitle := map[string]SessionRow{}
	for _, r := range rows {
		byTitle[r.Title] = r
	}
	for _, tc := range tests {
		r, ok := byTitle[tc.name]
		if !ok {
			t.Fatalf("missing row for %q", tc.name)
		}
		if r.Stale != tc.wantStale {
			t.Errorf("%s: Stale=%v want %v (idle=%s msgs=%d archived=%v)",
				tc.name, r.Stale, tc.wantStale, tc.idleAgo, tc.messages, tc.archived)
		}
	}

	// CountStaleSessions only counts non-archived; the archived row in the
	// table has wantStale=false so it doesn't contribute either way.
	gotCount, err := CountStaleSessions(ctx, db)
	if err != nil {
		t.Fatalf("count stale: %v", err)
	}
	if gotCount != wantCount {
		t.Errorf("CountStaleSessions = %d, want %d", gotCount, wantCount)
	}
}

func TestSessionsConfig_ApplyDefaults(t *testing.T) {
	tests := []struct {
		name string
		json string
		want SessionsConfig
	}{
		{
			name: "absent sessions block backfills defaults (scanners on, devbox off)",
			json: `{"search":"is:open"}`,
			want: SessionsConfig{Enabled: true, DevboxDiscovery: false, ScanClaude: true, ScanCodex: true, ScanCursor: true, ScanIntervalMinutes: 20},
		},
		{
			name: "empty sessions object treated as absent",
			json: `{"sessions":{}}`,
			want: SessionsConfig{Enabled: true, DevboxDiscovery: false, ScanClaude: true, ScanCodex: true, ScanCursor: true, ScanIntervalMinutes: 20},
		},
		{
			name: "explicit disable with another field set is respected",
			json: `{"sessions":{"enabled":false,"devbox_discovery":true}}`,
			want: SessionsConfig{Enabled: false, DevboxDiscovery: true},
		},
		{
			name: "partial config preserved (not backfilled to all-true)",
			json: `{"sessions":{"enabled":true,"scan_claude":true}}`,
			want: SessionsConfig{Enabled: true, ScanClaude: true, ScanIntervalMinutes: 20},
		},
		{
			name: "non-nil remotes keeps block from being treated as absent",
			json: `{"sessions":{"enabled":false,"remotes":[{"name":"box","host":"u@h"}]}}`,
			want: SessionsConfig{Enabled: false, Remotes: []RemoteConfig{{Name: "box", Host: "u@h"}}},
		},
		{
			name: "explicit ticker disable (-1) survives backfill",
			json: `{"sessions":{"enabled":true,"scan_interval_minutes":-1}}`,
			want: SessionsConfig{Enabled: true, ScanIntervalMinutes: -1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.json), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			cfg.applyDefaults()
			if !reflect.DeepEqual(cfg.Sessions, tc.want) {
				t.Errorf("Sessions = %#v, want %#v", cfg.Sessions, tc.want)
			}
		})
	}
}

func TestScanHighWater_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newSessionsTestDB(t)

	// Absent source returns zero time, nil error.
	got, err := GetScanHighWater(ctx, db, "local:claude")
	if err != nil {
		t.Fatalf("get absent: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("absent source should be zero time, got %v", got)
	}

	want := time.Date(2026, 6, 9, 13, 45, 30, 123000000, time.UTC)
	if err := SetScanHighWater(ctx, db, "local:claude", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = GetScanHighWater(ctx, db, "local:claude")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round trip mismatch: got %v want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", got.Location())
	}

	// Upsert: setting again overwrites.
	later := want.Add(48 * time.Hour)
	if err := SetScanHighWater(ctx, db, "local:claude", later); err != nil {
		t.Fatalf("set later: %v", err)
	}
	got, err = GetScanHighWater(ctx, db, "local:claude")
	if err != nil {
		t.Fatalf("get later: %v", err)
	}
	if !got.Equal(later) {
		t.Errorf("upsert mismatch: got %v want %v", got, later)
	}
}
