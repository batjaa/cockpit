package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// writeClaudeFixture writes lines to <root>/<slug>/<key>.jsonl and stamps the
// file's mtime so LastActive assertions are deterministic.
func writeClaudeFixture(t *testing.T, root, slug, key string, lines []string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, key+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func findRecord(recs []SessionRecord, key string) (SessionRecord, bool) {
	for _, r := range recs {
		if r.SessionKey == key {
			return r, true
		}
	}
	return SessionRecord{}, false
}

func TestScanClaudeStore_MissingRoot(t *testing.T) {
	recs, err := ScanClaudeStore(filepath.Join(t.TempDir(), "does-not-exist"), "local", time.Time{})
	if err != nil {
		t.Fatalf("missing root should not error, got %v", err)
	}
	if recs != nil {
		t.Fatalf("missing root should return nil, got %v", recs)
	}
}

func TestScanClaudeStore(t *testing.T) {
	root := t.TempDir()
	slug := "-Users-foo-git-bar"
	wantProjectDir := "/Users/foo/git/bar"

	mtimeRecent := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mtimeOld := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Session A: string content, multiple user/assistant lines, a meta line
	// to skip, a malformed line to tolerate.
	sessA := "aaaaaaaa-0000-0000-0000-000000000001"
	writeClaudeFixture(t, root, slug, sessA, []string{
		`{"type":"summary","summary":"ignored"}`,
		`{"type":"user","timestamp":"2026-06-01T10:00:00Z","message":{"role":"user","content":"Fix the flaky retry test"}}`,
		`this is not json at all`,
		`{"type":"assistant","timestamp":"2026-06-01T10:00:05Z","message":{"role":"assistant","content":"Sure, on it"}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-06-01T10:00:06Z","message":{"role":"user","content":"<system-reminder>meta</system-reminder>"}}`,
		`{"type":"user","timestamp":"2026-06-01T10:01:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"output"}]}}`,
		`{"type":"user","timestamp":"2026-06-01T10:02:00Z","message":{"role":"user","content":"and now ship it"}}`,
	}, mtimeRecent)

	// Session B: array-of-blocks content, command-wrapper + local-command
	// lines to skip before the real prompt.
	sessB := "bbbbbbbb-0000-0000-0000-000000000002"
	writeClaudeFixture(t, root, slug, sessB, []string{
		`{"type":"user","timestamp":"2026-05-30T08:00:00Z","message":{"role":"user","content":[{"type":"text","text":"<command-name>/model</command-name>\n<command-message>model</command-message>"}]}}`,
		`{"type":"user","timestamp":"2026-05-30T08:00:01Z","message":{"role":"user","content":[{"type":"text","text":"<local-command-stdout>set model</local-command-stdout>"}]}}`,
		`{"type":"user","timestamp":"2026-05-30T08:01:00Z","message":{"role":"user","content":[{"type":"text","text":"Add a   dark mode\ntoggle"}]}}`,
		`{"type":"assistant","timestamp":"2026-05-30T08:01:05Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}, mtimeRecent)

	// Session C (old): excluded by since filtering.
	sessC := "cccccccc-0000-0000-0000-000000000003"
	writeClaudeFixture(t, root, slug, sessC, []string{
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"old session"}}`,
	}, mtimeOld)

	// Session D: no usable user text -> title falls back to session key.
	sessD := "dddddddd-0000-0000-0000-000000000004"
	writeClaudeFixture(t, root, slug, sessD, []string{
		`{"type":"system","timestamp":"2026-06-01T09:00:00Z","subtype":"init"}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-06-01T09:00:01Z","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
	}, mtimeRecent)

	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	recs, err := ScanClaudeStore(root, "local", since)
	if err != nil {
		t.Fatalf("ScanClaudeStore: %v", err)
	}

	// since filtering: C excluded, A/B/D included.
	if len(recs) != 3 {
		keys := make([]string, len(recs))
		for i, r := range recs {
			keys[i] = r.SessionKey
		}
		sort.Strings(keys)
		t.Fatalf("want 3 records (C filtered out), got %d: %v", len(recs), keys)
	}
	if _, ok := findRecord(recs, sessC); ok {
		t.Fatalf("session C should have been excluded by since filtering")
	}

	// Session A assertions: all fields.
	a, ok := findRecord(recs, sessA)
	if !ok {
		t.Fatalf("session A not found")
	}
	if a.Agent != "claude" {
		t.Errorf("A.Agent = %q, want claude", a.Agent)
	}
	if a.Machine != "local" {
		t.Errorf("A.Machine = %q, want local", a.Machine)
	}
	if a.ProjectDir != wantProjectDir {
		t.Errorf("A.ProjectDir = %q, want %q", a.ProjectDir, wantProjectDir)
	}
	if a.Title != "Fix the flaky retry test" {
		t.Errorf("A.Title = %q, want %q", a.Title, "Fix the flaky retry test")
	}
	// Subtitle = last usable user message text.
	if a.Subtitle != "and now ship it" {
		t.Errorf("A.Subtitle = %q, want %q", a.Subtitle, "and now ship it")
	}
	wantStarted := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if !a.StartedAt.Equal(wantStarted) {
		t.Errorf("A.StartedAt = %v, want %v", a.StartedAt, wantStarted)
	}
	if !a.LastActive.Equal(mtimeRecent) {
		t.Errorf("A.LastActive = %v, want %v", a.LastActive, mtimeRecent)
	}
	// Counts lines with type user/assistant: 4 user lines (prompt, meta,
	// tool_result, ship) + 1 assistant = 5. The summary line is excluded;
	// the malformed line fails to parse and is excluded.
	if a.MessageCount != 5 {
		t.Errorf("A.MessageCount = %d, want 5 user/assistant lines", a.MessageCount)
	}
	wantResume := "cd /Users/foo/git/bar && claude --resume " + sessA
	if a.ResumeCmd != wantResume {
		t.Errorf("A.ResumeCmd = %q, want %q", a.ResumeCmd, wantResume)
	}

	// Session B assertions: array content, command wrappers skipped,
	// newlines collapsed, whitespace runs collapsed.
	b, ok := findRecord(recs, sessB)
	if !ok {
		t.Fatalf("session B not found")
	}
	if b.Title != "Add a dark mode toggle" {
		t.Errorf("B.Title = %q, want %q", b.Title, "Add a dark mode toggle")
	}
	wantBStarted := time.Date(2026, 5, 30, 8, 0, 0, 0, time.UTC)
	if !b.StartedAt.Equal(wantBStarted) {
		t.Errorf("B.StartedAt = %v, want %v (first line with a timestamp)", b.StartedAt, wantBStarted)
	}

	// Session D assertions: fallback title = session key.
	d, ok := findRecord(recs, sessD)
	if !ok {
		t.Fatalf("session D not found")
	}
	if d.Title != sessD {
		t.Errorf("D.Title = %q, want fallback to session key %q", d.Title, sessD)
	}
}

func TestScanClaudeStore_ZeroSinceScansAll(t *testing.T) {
	root := t.TempDir()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	writeClaudeFixture(t, root, "-tmp-proj", "key1", []string{
		`{"type":"user","timestamp":"2020-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`,
	}, old)

	recs, err := ScanClaudeStore(root, "local", time.Time{})
	if err != nil {
		t.Fatalf("ScanClaudeStore: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("zero since should scan all, got %d records", len(recs))
	}
	if recs[0].ProjectDir != "/tmp/proj" {
		t.Errorf("ProjectDir = %q, want /tmp/proj", recs[0].ProjectDir)
	}
}

func TestDeslugClaudeProject(t *testing.T) {
	tests := []struct {
		slug string
		want string
	}{
		{"-Users-foo-git-bar", "/Users/foo/git/bar"},
		{"-tmp", "/tmp"},
		{"", ""},
		{"relative-path", "relative/path"},
	}
	for _, tt := range tests {
		if got := deslugClaudeProject(tt.slug); got != tt.want {
			t.Errorf("deslugClaudeProject(%q) = %q, want %q", tt.slug, got, tt.want)
		}
	}
}

func TestTruncateTitle(t *testing.T) {
	if got := truncateTitle("short", 120); got != "short" {
		t.Errorf("truncateTitle short = %q", got)
	}
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := truncateTitle(string(long), 120)
	if len([]rune(got)) != 120 {
		t.Errorf("truncateTitle long len = %d, want 120", len([]rune(got)))
	}
}

// TestScanClaudeStore_SkipsSubagentTranscripts: agent-*.jsonl files are
// subagent transcripts, not resumable sessions.
func TestScanClaudeStore_SkipsSubagentTranscripts(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-Users-x-git-repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-06-01T10:00:00Z","message":{"role":"user","content":"real session"}}` + "\n"
	for _, name := range []string{"abc-123.jsonl", "agent-deadbeef.jsonl"} {
		if err := os.WriteFile(filepath.Join(proj, name), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := ScanClaudeStore(root, "local", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].SessionKey != "abc-123" {
		t.Errorf("want only abc-123, got %+v", recs)
	}
}
