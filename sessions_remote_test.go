package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prependPATH puts dir at the front of PATH for the test so stub binaries
// shadow the real ssh / devbox.
func prependPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// writeStub writes an executable shell script named name into dir.
func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// devboxFixture is real-shaped `devbox list` output with ANSI color codes and
// an OSC-8 hyperlink wrapping the dashboard URL, to prove the parser strips
// both. Two devboxes: "main" (running) and "devbox-4" (stopped).
const devboxFixture = "Using profile: sso-devbox-user\n" +
	"\n" +
	"\x1b[1m🦉 Your Devboxes\x1b[0m\n" +
	"\n" +
	"╭─┬─────┬──────────┬───────────┬─────────┬────────────┬─────┬──────────┬──────────────────────────╮\n" +
	"│ │ IDX │ NAME     │ REGION    │ STATE   │ TUNNEL     │ SSH │ ALIAS    │ DASHBOARD                │\n" +
	"├─┼─────┼──────────┼───────────┼─────────┼────────────┼─────┼──────────┼──────────────────────────┤\n" +
	"│\x1b[32m*\x1b[0m│ 0   │ main     │ us-west-2 │ \x1b[32mrunning\x1b[0m │ background │ ok  │ devbox   │ \x1b]8;;https://localhost:3000/\x1b\\https://localhost:3000/\x1b]8;;\x1b\\  │\n" +
	"│ │ 4   │ devbox-4 │ us-west-2 │ stopped │ ·          │ ·   │ devbox-4 │ ·                        │\n" +
	"╰─┴─────┴──────────┴───────────┴─────────┴────────────┴─────┴──────────┴──────────────────────────╯\n"

func TestStripTerminalSequences(t *testing.T) {
	in := "\x1b[1mbold\x1b[0m \x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\ end"
	got := stripTerminalSequences(in)
	want := "bold link end"
	if got != want {
		t.Errorf("stripTerminalSequences = %q, want %q", got, want)
	}
}

func TestParseDevboxList(t *testing.T) {
	remotes := parseDevboxList(devboxFixture)
	if len(remotes) != 1 {
		t.Fatalf("want 1 running devbox, got %d: %+v", len(remotes), remotes)
	}
	if remotes[0].Name != "main" {
		t.Errorf("Name = %q, want main", remotes[0].Name)
	}
	if remotes[0].Host != "devbox" {
		t.Errorf("Host = %q, want devbox (ALIAS column)", remotes[0].Host)
	}
}

func TestDiscoverDevboxes(t *testing.T) {
	dir := t.TempDir()
	// Stub devbox: ignore args, emit the fixture verbatim. printf %b expands
	// the embedded escapes so the ANSI/OSC bytes reach the parser.
	script := fmt.Sprintf("#!/bin/sh\ncat <<'DEVBOX_EOF'\n%sDEVBOX_EOF\n", devboxFixture)
	writeStub(t, dir, "devbox", script)
	prependPATH(t, dir)

	remotes, err := DiscoverDevboxes(context.Background())
	if err != nil {
		t.Fatalf("DiscoverDevboxes: %v", err)
	}
	if len(remotes) != 1 || remotes[0].Name != "main" || remotes[0].Host != "devbox" {
		t.Fatalf("remotes = %+v, want [{main devbox}]", remotes)
	}
}

func TestDiscoverDevboxes_CLIFailure(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "devbox", "#!/bin/sh\necho 'sso expired' >&2\nexit 1\n")
	prependPATH(t, dir)

	if _, err := DiscoverDevboxes(context.Background()); err == nil {
		t.Fatal("expected error on devbox CLI failure")
	}
}

// claudeRemoteFixture is a ===FILE stream for two claude transcripts as the
// remote shell would emit it: sentinel line then head bytes. Session A has a
// real prompt and project slug; session B's only user text is a command
// wrapper, so its title falls back to the session key.
func claudeRemoteFixture() string {
	headA := `{"type":"user","timestamp":"2026-06-01T10:00:00Z","message":{"role":"user","content":"Fix PLAT-422 retry flake"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-06-01T10:00:05Z","message":{"role":"assistant","content":"on it"}}` + "\n" +
		`{"type":"user","timestamp":"2026-06-01T10:02:00Z","message":{"role":"user","content":"and ship it"}}`
	headB := `{"type":"user","timestamp":"2026-05-30T08:00:00Z","message":{"role":"user","content":"<command-name>/model</command-name>"}}`

	return remoteRecordSep + "/home/ubuntu/.claude/projects/-home-ubuntu-co-backend/aaaa-0001.jsonl\t1748772000\t47\n" +
		headA + "\n" +
		remoteRecordSep + "/home/ubuntu/.claude/projects/-home-ubuntu-co-frontend/bbbb-0002.jsonl\t1748592000\t3\n" +
		headB + "\n"
}

// codexRemoteFixture is a ===FILE stream for one codex rollout: session_meta on
// line 1, an injected AGENTS.md block (skipped), then the real prompt.
func codexRemoteFixture() string {
	id := "019e6ce8-0cf2-7fd0-ab28-ed74b13ed7b0"
	head := `{"type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-05-28T04:46:47Z","cwd":"/home/ubuntu/co/backend"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n<INSTRUCTIONS>x</INSTRUCTIONS>"}]}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"wire up DEVBOX-12 discovery"}}`

	return remoteRecordSep + "/home/ubuntu/.codex/sessions/2026/05/28/rollout-2026-05-28T12-46-47-" + id + ".jsonl\t1748443607\t12\n" +
		head + "\n"
}

// writeSSHStub writes an ssh stub that ignores all args and emits fixture on
// stdout. The real scanners call ssh once per store; the stub is store-agnostic
// because each test points it at a single fixture.
func writeSSHStub(t *testing.T, dir, fixture string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'SSH_EOF'\n%sSSH_EOF\n", fixture)
	writeStub(t, dir, "ssh", script)
}

func TestScanRemoteClaude(t *testing.T) {
	dir := t.TempDir()
	writeSSHStub(t, dir, claudeRemoteFixture())
	prependPATH(t, dir)

	recs, err := ScanRemoteClaude(context.Background(), "devbox", "main", time.Time{})
	if err != nil {
		t.Fatalf("ScanRemoteClaude: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}

	byKey := map[string]SessionRecord{}
	for _, r := range recs {
		byKey[r.SessionKey] = r
	}

	a, ok := byKey["aaaa-0001"]
	if !ok {
		t.Fatalf("record A missing: %+v", recs)
	}
	if a.Agent != "claude" || a.Machine != "main" {
		t.Errorf("A agent/machine = %q/%q", a.Agent, a.Machine)
	}
	if a.ProjectDir != "/home/ubuntu/co/backend" {
		t.Errorf("A.ProjectDir = %q, want /home/ubuntu/co/backend", a.ProjectDir)
	}
	if a.Title != "Fix PLAT-422 retry flake" {
		t.Errorf("A.Title = %q", a.Title)
	}
	if a.Subtitle != "and ship it" {
		t.Errorf("A.Subtitle = %q, want last user text", a.Subtitle)
	}
	wantStarted := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if !a.StartedAt.Equal(wantStarted) {
		t.Errorf("A.StartedAt = %v, want %v", a.StartedAt, wantStarted)
	}
	wantMtime := time.Unix(1748772000, 0).UTC()
	if !a.LastActive.Equal(wantMtime) {
		t.Errorf("A.LastActive = %v, want %v (mtime epoch)", a.LastActive, wantMtime)
	}
	if a.MessageCount != 47 {
		t.Errorf("A.MessageCount = %d, want 47 (wc -l)", a.MessageCount)
	}
	wantResume := "ssh -t devbox 'cd /home/ubuntu/co/backend && claude --resume aaaa-0001'"
	if a.ResumeCmd != wantResume {
		t.Errorf("A.ResumeCmd = %q, want %q", a.ResumeCmd, wantResume)
	}

	// Session B: command-wrapper-only -> title falls back to session key.
	b, ok := byKey["bbbb-0002"]
	if !ok {
		t.Fatalf("record B missing")
	}
	if b.Title != "bbbb-0002" {
		t.Errorf("B.Title = %q, want fallback to session key", b.Title)
	}
	if b.ProjectDir != "/home/ubuntu/co/frontend" {
		t.Errorf("B.ProjectDir = %q", b.ProjectDir)
	}
}

func TestScanRemoteCodex(t *testing.T) {
	dir := t.TempDir()
	writeSSHStub(t, dir, codexRemoteFixture())
	prependPATH(t, dir)

	recs, err := ScanRemoteCodex(context.Background(), "user@box", "otherbox", time.Time{})
	if err != nil {
		t.Fatalf("ScanRemoteCodex: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	id := "019e6ce8-0cf2-7fd0-ab28-ed74b13ed7b0"
	if r.Agent != "codex" || r.Machine != "otherbox" {
		t.Errorf("agent/machine = %q/%q", r.Agent, r.Machine)
	}
	if r.SessionKey != id {
		t.Errorf("SessionKey = %q, want %q", r.SessionKey, id)
	}
	if r.ProjectDir != "/home/ubuntu/co/backend" {
		t.Errorf("ProjectDir = %q", r.ProjectDir)
	}
	// Injected AGENTS.md user block skipped; the real prompt wins.
	if r.Title != "wire up DEVBOX-12 discovery" {
		t.Errorf("Title = %q", r.Title)
	}
	wantStarted := time.Date(2026, 5, 28, 4, 46, 47, 0, time.UTC)
	if !r.StartedAt.Equal(wantStarted) {
		t.Errorf("StartedAt = %v, want %v", r.StartedAt, wantStarted)
	}
	wantMtime := time.Unix(1748443607, 0).UTC()
	if !r.LastActive.Equal(wantMtime) {
		t.Errorf("LastActive = %v, want %v", r.LastActive, wantMtime)
	}
	if r.MessageCount != 12 {
		t.Errorf("MessageCount = %d, want 12", r.MessageCount)
	}
	if r.ResumeCmd != "ssh -t user@box 'codex resume "+id+"'" {
		t.Errorf("ResumeCmd = %q", r.ResumeCmd)
	}
}

func TestScanRemoteClaude_SSHFailure(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "ssh", "#!/bin/sh\necho 'host unreachable' >&2\nexit 255\n")
	prependPATH(t, dir)

	if _, err := ScanRemoteClaude(context.Background(), "devbox", "main", time.Time{}); err == nil {
		t.Fatal("expected error when ssh exits non-zero")
	}
}

func TestScanRemoteCodex_SSHFailure(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "ssh", "#!/bin/sh\nexit 1\n")
	prependPATH(t, dir)

	if _, err := ScanRemoteCodex(context.Background(), "devbox", "main", time.Time{}); err == nil {
		t.Fatal("expected error when ssh exits non-zero")
	}
}

// TestScanRemoteClaude_EmptyStore: a host with no claude store (ssh exits 0,
// empty output) yields (nil, nil), not an error.
func TestScanRemoteClaude_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	writeStub(t, dir, "ssh", "#!/bin/sh\nexit 0\n")
	prependPATH(t, dir)

	recs, err := ScanRemoteClaude(context.Background(), "devbox", "main", time.Time{})
	if err != nil {
		t.Fatalf("empty store should not error: %v", err)
	}
	if recs != nil {
		t.Fatalf("empty store should return nil, got %+v", recs)
	}
}

// TestRemoteScripts_HonorSince verifies the generated find predicate includes
// the epoch when since is set and omits it when zero.
func TestRemoteScripts_HonorSince(t *testing.T) {
	since := time.Unix(1700000000, 0)
	withSince := remoteClaudeScript(since)
	if !strings.Contains(withSince, "-newermt @1700000000") {
		t.Errorf("claude script missing newermt predicate: %q", withSince)
	}
	zero := remoteClaudeScript(time.Time{})
	if strings.Contains(zero, "-newermt") {
		t.Errorf("zero since should omit newermt: %q", zero)
	}
}

// TestRemoteClaudeScript_ExcludesSubagents: the remote find pipeline must
// not pull agent-*.jsonl subagent transcripts.
func TestRemoteClaudeScript_ExcludesSubagents(t *testing.T) {
	script := remoteClaudeScript(time.Time{})
	if !strings.Contains(script, "! -name 'agent-*'") {
		t.Errorf("remote claude script missing subagent exclusion:\n%s", script)
	}
}
