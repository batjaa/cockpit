package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCodexFixture writes lines to
// <root>/YYYY/MM/DD/rollout-<stamp>-<id>.jsonl and stamps the mtime so
// LastActive is deterministic.
func writeCodexFixture(t *testing.T, root, datePath, stamp, id string, lines []string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, datePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	name := "rollout-" + stamp + "-" + id + ".jsonl"
	path := filepath.Join(dir, name)
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

func TestScanCodexStore_MissingRoot(t *testing.T) {
	recs, err := ScanCodexStore(filepath.Join(t.TempDir(), "nope"), "local", time.Time{})
	if err != nil {
		t.Fatalf("missing root should not error, got %v", err)
	}
	if recs != nil {
		t.Fatalf("missing root should return nil, got %v", recs)
	}
}

func TestScanCodexStore(t *testing.T) {
	root := t.TempDir()
	mtimeRecent := time.Date(2026, 5, 28, 13, 0, 0, 0, time.UTC)
	mtimeOld := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	id := "019e6ce8-0cf2-7fd0-ab28-ed74b13ed7b0"
	// Valid rollout: meta + an injected AGENTS.md user block (skipped) + the
	// real user prompt, plus an assistant message and a malformed line.
	writeCodexFixture(t, root, "2026/05/28", "2026-05-28T12-46-47", id, []string{
		`{"timestamp":"2026-05-28T13:00:00.000Z","type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-05-28T04:46:47.594Z","cwd":"/Users/foo/co/backend","cli_version":"0.134.0"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>...</permissions instructions>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /Users/foo/co/backend\n<INSTRUCTIONS>do x</INSTRUCTIONS>"}]}}`,
		`{not valid json}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"check what the devbox cli can do"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"check what the devbox cli can do","images":[]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"on it"}]}}`,
	}, mtimeRecent)

	// Rollout missing meta: first line isn't session_meta -> skipped.
	writeCodexFixture(t, root, "2026/05/28", "2026-05-28T14-00-00", "ffffffff-0000-0000-0000-000000000099", []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"no meta here"}]}}`,
	}, mtimeRecent)

	// Old rollout: excluded by since filtering.
	oldID := "11111111-0000-0000-0000-000000000001"
	writeCodexFixture(t, root, "2026/01/01", "2026-01-01T00-00-00", oldID, []string{
		`{"type":"session_meta","payload":{"id":"` + oldID + `","timestamp":"2026-01-01T00:00:00Z","cwd":"/old"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"old prompt"}}`,
	}, mtimeOld)

	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	recs, err := ScanCodexStore(root, "local", since)
	if err != nil {
		t.Fatalf("ScanCodexStore: %v", err)
	}

	// Only the valid recent rollout survives: missing-meta skipped, old
	// filtered by since.
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", r.Agent)
	}
	if r.Machine != "local" {
		t.Errorf("Machine = %q, want local", r.Machine)
	}
	if r.SessionKey != id {
		t.Errorf("SessionKey = %q, want %q", r.SessionKey, id)
	}
	if r.ProjectDir != "/Users/foo/co/backend" {
		t.Errorf("ProjectDir = %q, want /Users/foo/co/backend", r.ProjectDir)
	}
	// Title: the injected AGENTS.md user block is skipped; the real prompt wins.
	if r.Title != "check what the devbox cli can do" {
		t.Errorf("Title = %q, want %q", r.Title, "check what the devbox cli can do")
	}
	wantStarted := time.Date(2026, 5, 28, 4, 46, 47, 594000000, time.UTC)
	if !r.StartedAt.Equal(wantStarted) {
		t.Errorf("StartedAt = %v, want %v (payload.timestamp)", r.StartedAt, wantStarted)
	}
	if !r.LastActive.Equal(mtimeRecent) {
		t.Errorf("LastActive = %v, want %v (file mtime)", r.LastActive, mtimeRecent)
	}
	// MessageCount = total line count (7 lines written).
	if r.MessageCount != 7 {
		t.Errorf("MessageCount = %d, want 7 (total lines)", r.MessageCount)
	}
	if r.ResumeCmd != "codex resume "+id {
		t.Errorf("ResumeCmd = %q, want %q", r.ResumeCmd, "codex resume "+id)
	}
}

func TestScanCodexStore_FallbackTitleAndUserMessageEvent(t *testing.T) {
	root := t.TempDir()
	mtime := time.Date(2026, 5, 28, 13, 0, 0, 0, time.UTC)

	// A rollout where the only user-bearing line is an event_msg/user_message.
	idA := "22222222-0000-0000-0000-00000000000a"
	writeCodexFixture(t, root, "2026/05/28", "2026-05-28T15-00-00", idA, []string{
		`{"type":"session_meta","payload":{"id":"` + idA + `","timestamp":"2026-05-28T15:00:00Z","cwd":"/proj/a"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"build the parser"}}`,
	}, mtime)

	// A rollout with no usable user message -> title falls back to id.
	idB := "33333333-0000-0000-0000-00000000000b"
	writeCodexFixture(t, root, "2026/05/28", "2026-05-28T16-00-00", idB, []string{
		`{"type":"session_meta","payload":{"id":"` + idB + `","timestamp":"2026-05-28T16:00:00Z","cwd":"/proj/b"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","total":42}}`,
		`{"type":"response_item","payload":{"type":"reasoning","text":"thinking"}}`,
	}, mtime)

	recs, err := ScanCodexStore(root, "local", time.Time{})
	if err != nil {
		t.Fatalf("ScanCodexStore: %v", err)
	}
	byKey := map[string]SessionRecord{}
	for _, r := range recs {
		byKey[r.SessionKey] = r
	}

	a, ok := byKey[idA]
	if !ok {
		t.Fatalf("record A missing")
	}
	if a.Title != "build the parser" {
		t.Errorf("A.Title = %q, want %q", a.Title, "build the parser")
	}

	b, ok := byKey[idB]
	if !ok {
		t.Fatalf("record B missing")
	}
	if b.Title != idB {
		t.Errorf("B.Title = %q, want fallback to id %q", b.Title, idB)
	}
}
