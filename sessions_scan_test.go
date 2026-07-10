package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// sessionsTestConfig is the fully-enabled sessions config the orchestration
// tests use. DevboxDiscovery is off so tests control the remote set explicitly
// via the remotes slice passed to scanSessionsFrom; discovery itself is
// covered in sessions_remote_test.go.
func sessionsTestConfig() Config {
	return Config{
		Sessions: SessionsConfig{
			Enabled:         true,
			DevboxDiscovery: false,
			ScanClaude:      true,
			ScanCodex:       true,
			ScanCursor:      true,
		},
	}
}

// buildLocalFixtures creates minimal claude + codex stores under a temp home
// and returns the sourcePaths pointing at them. Cursor points at a missing
// path (no Cursor install in the test sandbox), which the cursor scanner
// tolerates as (nil, nil). The claude session title carries "PLAT-422" to
// exercise ticket extraction.
func buildLocalFixtures(t *testing.T) sourcePaths {
	t.Helper()
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude", "projects")
	codexRoot := filepath.Join(root, "codex", "sessions")

	recent := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Local claude session — title mentions PLAT-422.
	writeClaudeFixture(t, claudeRoot, "-Users-foo-git-backend", "local-claude-1", []string{
		`{"type":"user","timestamp":"2026-06-01T10:00:00Z","message":{"role":"user","content":"Fix PLAT-422 flaky retry test"}}`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:05Z","message":{"role":"assistant","content":"on it"}}`,
	}, recent)

	// Local codex rollout.
	codexID := "codex-local-0001"
	writeCodexFixture(t, codexRoot, "2026/06/01", "2026-06-01T10-00-00", codexID, []string{
		`{"type":"session_meta","payload":{"id":"` + codexID + `","timestamp":"2026-06-01T10:00:00Z","cwd":"/Users/foo/git/backend"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"build the codex parser"}}`,
	}, recent)

	return sourcePaths{
		claudeRoot: claudeRoot,
		codexRoot:  codexRoot,
		cursorDB:   filepath.Join(root, "does-not-exist", "state.vscdb"),
	}
}

func openSessionsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countSessions(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	q := "SELECT COUNT(*) FROM sessions"
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestE2E_ScanSessions drives the orchestrator end-to-end through the
// unexported helper: local claude + codex fixtures plus one stubbed remote
// (devbox alias) scanned over a stubbed ssh. It asserts rows land across
// machines, tickets are extracted and stored, and per-source high-water marks
// advance.
func TestE2E_ScanSessions(t *testing.T) {
	dir := t.TempDir()
	// The remote ssh stub serves the claude fixture for both stores; the codex
	// scan parses it as having no valid session_meta line 1 and returns no
	// records, which is fine — claude rows prove the remote path works. To get
	// codex rows too we route by inspecting the script on stdin.
	writeRoutingSSHStub(t, dir, claudeRemoteFixture(), codexRemoteFixture())
	prependPATH(t, dir)

	db := openSessionsDB(t)
	paths := buildLocalFixtures(t)
	remotes := []RemoteConfig{{Name: "devbox", Host: "devbox"}}

	if err := scanSessionsFrom(context.Background(), db, sessionsTestConfig(), paths, remotes); err != nil {
		t.Fatalf("scanSessionsFrom: %v", err)
	}

	// Rows across machines: 1 local claude + 1 local codex + 2 remote claude +
	// 1 remote codex = 5.
	if got := countSessions(t, db, ""); got != 5 {
		t.Errorf("total sessions = %d, want 5", got)
	}
	if got := countSessions(t, db, "machine='local'"); got != 2 {
		t.Errorf("local sessions = %d, want 2", got)
	}
	if got := countSessions(t, db, "machine='devbox'"); got != 3 {
		t.Errorf("devbox sessions = %d, want 3", got)
	}
	if got := countSessions(t, db, "agent='claude'"); got != 3 {
		t.Errorf("claude sessions = %d, want 3", got)
	}
	if got := countSessions(t, db, "agent='codex'"); got != 2 {
		t.Errorf("codex sessions = %d, want 2", got)
	}

	// Tickets extracted and stored. The local claude title has PLAT-422; the
	// remote claude title has PLAT-422; the remote codex title has DEVBOX-12.
	assertTicketStored(t, db, "PLAT-422")
	assertTicketStored(t, db, "DEVBOX-12")

	// High-water marks advanced for every attempted source.
	for _, src := range []string{"local:claude", "local:codex", "devbox:claude", "devbox:codex"} {
		hw, err := GetScanHighWater(context.Background(), db, src)
		if err != nil {
			t.Fatalf("GetScanHighWater(%s): %v", src, err)
		}
		if hw.IsZero() {
			t.Errorf("high water for %s never advanced", src)
		}
	}
	// local:cursor pointed at a missing DB -> no records -> no high water.
	hw, _ := GetScanHighWater(context.Background(), db, "local:cursor")
	if !hw.IsZero() {
		t.Errorf("local:cursor high water = %v, want zero (no records)", hw)
	}
}

// TestE2E_ScanSessions_FailingSourceIsolated: one host's ssh exits non-zero
// for every store, but local sources and a second healthy host still persist.
// The failing source does not advance its high-water mark.
func TestE2E_ScanSessions_FailingSourceIsolated(t *testing.T) {
	dir := t.TempDir()
	// ssh stub fails for host "deadbox", succeeds (claude fixture) otherwise.
	// The ssh argv places the host before "bash -s"; we grep for it.
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  if [ "$a" = "deadbox" ]; then
    echo "unreachable" >&2
    exit 255
  fi
done
cat <<'SSH_EOF'
%sSSH_EOF
`, claudeRemoteFixture())
	writeStub(t, dir, "ssh", script)
	prependPATH(t, dir)

	db := openSessionsDB(t)
	paths := buildLocalFixtures(t)
	remotes := []RemoteConfig{
		{Name: "deadbox", Host: "deadbox"},
		{Name: "goodbox", Host: "goodbox"},
	}

	if err := scanSessionsFrom(context.Background(), db, sessionsTestConfig(), paths, remotes); err != nil {
		t.Fatalf("scanSessionsFrom should not fail when some sources succeed: %v", err)
	}

	// Local (2) + goodbox claude (2) survive; goodbox codex parses the claude
	// fixture as no-meta -> 0 records. deadbox contributes nothing.
	if got := countSessions(t, db, "machine='local'"); got != 2 {
		t.Errorf("local = %d, want 2", got)
	}
	if got := countSessions(t, db, "machine='goodbox'"); got != 2 {
		t.Errorf("goodbox = %d, want 2", got)
	}
	if got := countSessions(t, db, "machine='deadbox'"); got != 0 {
		t.Errorf("deadbox = %d, want 0 (ssh failed)", got)
	}

	// deadbox high-water marks must NOT have advanced (scan errored).
	for _, src := range []string{"deadbox:claude", "deadbox:codex"} {
		hw, _ := GetScanHighWater(context.Background(), db, src)
		if !hw.IsZero() {
			t.Errorf("%s high water advanced despite ssh failure: %v", src, hw)
		}
	}
	// goodbox:claude did advance.
	hw, _ := GetScanHighWater(context.Background(), db, "goodbox:claude")
	if hw.IsZero() {
		t.Error("goodbox:claude high water should have advanced")
	}
}

// TestE2E_ScanSessions_Idempotent: a second scan over unchanged fixtures
// upserts no duplicate rows (the UNIQUE key holds) and counts stay stable.
func TestE2E_ScanSessions_Idempotent(t *testing.T) {
	dir := t.TempDir()
	writeRoutingSSHStub(t, dir, claudeRemoteFixture(), codexRemoteFixture())
	prependPATH(t, dir)

	db := openSessionsDB(t)
	paths := buildLocalFixtures(t)
	remotes := []RemoteConfig{{Name: "devbox", Host: "devbox"}}
	cfg := sessionsTestConfig()
	ctx := context.Background()

	if err := scanSessionsFrom(ctx, db, cfg, paths, remotes); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	first := countSessions(t, db, "")
	firstTickets := countTickets(t, db)

	if err := scanSessionsFrom(ctx, db, cfg, paths, remotes); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	second := countSessions(t, db, "")
	secondTickets := countTickets(t, db)

	if first != second {
		t.Errorf("session count changed across scans: %d -> %d", first, second)
	}
	if firstTickets != secondTickets {
		t.Errorf("ticket count changed across scans: %d -> %d", firstTickets, secondTickets)
	}
}

// TestScanSessions_Disabled: when the feature is disabled, ScanSessions is a
// no-op and never touches the DB.
func TestScanSessions_Disabled(t *testing.T) {
	db := openSessionsDB(t)
	cfg := Config{Sessions: SessionsConfig{Enabled: false}}
	if err := ScanSessions(context.Background(), db, cfg); err != nil {
		t.Fatalf("disabled scan should be a no-op, got %v", err)
	}
	if got := countSessions(t, db, ""); got != 0 {
		t.Errorf("disabled scan wrote %d rows", got)
	}
}

// TestGatherRemotes_DedupeConfiguredWins: a configured remote and a discovered
// devbox share a host; the configured entry (its name) must win, and hosts
// dedupe by Host.
func TestGatherRemotes_DedupeConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	// devbox discovery returns a host "devbox" with name "main".
	script := fmt.Sprintf("#!/bin/sh\ncat <<'DEVBOX_EOF'\n%sDEVBOX_EOF\n", devboxFixture)
	writeStub(t, dir, "devbox", script)
	prependPATH(t, dir)

	cfg := Config{Sessions: SessionsConfig{
		DevboxDiscovery: true,
		Remotes: []RemoteConfig{
			{Name: "my-devbox", Host: "devbox"}, // collides with discovered host
			{Name: "extra", Host: "extra.example.com"},
		},
	}}

	remotes := gatherRemotes(context.Background(), cfg)
	byHost := map[string]RemoteConfig{}
	for _, r := range remotes {
		byHost[r.Host] = r
	}
	if len(remotes) != 2 {
		t.Fatalf("want 2 deduped remotes, got %d: %+v", len(remotes), remotes)
	}
	if byHost["devbox"].Name != "my-devbox" {
		t.Errorf("configured remote should win on name: got %q, want my-devbox", byHost["devbox"].Name)
	}
	if _, ok := byHost["extra.example.com"]; !ok {
		t.Error("configured non-devbox remote missing")
	}
}

// writeRoutingSSHStub writes an ssh stub that inspects the script piped on
// stdin and returns the claude fixture when it targets ~/.claude, the codex
// fixture when it targets ~/.codex. This lets one stub serve both stores in a
// single end-to-end run.
func writeRoutingSSHStub(t *testing.T, dir, claudeFixture, codexFixture string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
input=$(cat)
case "$input" in
  *.claude/projects*)
    cat <<'CLAUDE_EOF'
%sCLAUDE_EOF
    ;;
  *.codex/sessions*)
    cat <<'CODEX_EOF'
%sCODEX_EOF
    ;;
esac
`, claudeFixture, codexFixture)
	writeStub(t, dir, "ssh", script)
}

func countTickets(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_tickets").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertTicketStored(t *testing.T, db *sql.DB, ticket string) {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_tickets WHERE ticket=?", ticket).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Errorf("ticket %q was not extracted/stored", ticket)
	}
}
