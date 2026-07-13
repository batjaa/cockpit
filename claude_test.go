package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeStubClaude writes an executable `claude` script that prints
// stdoutBody to stdout and exits with exitCode.
func writeStubClaude(t *testing.T, stdoutBody string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\nexit %d\n", stdoutBody, exitCode)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestExtractLastJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "clean json",
			input: `{"pr": {"number": 1}, "verdict": "approve"}`,
			want:  `{"pr": {"number": 1}, "verdict": "approve"}`,
		},
		{
			name: "prose then json",
			input: `Here is the review for the PR:
{"pr": {"number": 2}, "verdict": "request-changes"}`,
			want: `{"pr": {"number": 2}, "verdict": "request-changes"}`,
		},
		{
			name:  "json with nested objects",
			input: `Reasoning... {"pr": {"owner": "foo", "repo": "bar"}, "findings": [{"id": "B1"}]}`,
			want:  `{"pr": {"owner": "foo", "repo": "bar"}, "findings": [{"id": "B1"}]}`,
		},
		{
			name: "multiple json objects keeps last",
			input: `Example: {"a": 1}
Actual review:
{"pr": {"number": 3}}`,
			want: `{"pr": {"number": 3}}`,
		},
		{
			name:    "no json",
			input:   "just prose, no json here",
			wantErr: true,
		},
		{
			name:  "error schema",
			input: `{"error": "PR not found"}`,
			want:  `{"error": "PR not found"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractLastJSON(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.TrimSpace(string(got)) != strings.TrimSpace(c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

const sampleStructuredJSON = `{
  "pr": {
    "url": "https://github.com/owner/repo/pull/123",
    "owner": "owner",
    "repo": "backend",
    "number": 123,
    "title": "Add foo",
    "author": "alice",
    "head_sha": "sha-abc"
  },
  "summary": "Adds foo. Looks reasonable overall.",
  "verdict": "approve-with-suggestions",
  "findings": [
    {
      "id": "M1",
      "severity": "major",
      "perfect": "E",
      "path": "internal/foo/foo.go",
      "line": 42,
      "original_line": 42,
      "body": "**issue (blocking):** Nil deref on empty input.\n\n**suggestion:** Guard the loop."
    },
    {
      "id": "m1",
      "severity": "minor",
      "perfect": "C",
      "path": "internal/foo/foo.go",
      "line": 80,
      "original_line": 78,
      "body": "**suggestion:** Rename for clarity."
    }
  ],
  "positives": ["Test coverage is solid."]
}`

func TestE2E_RunStructuredReview(t *testing.T) {
	dir := writeStubClaude(t, sampleStructuredJSON, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sr, raw, err := RunStructuredReview(context.Background(), "claude", "",
		"https://github.com/owner/repo/pull/123", "", 30*time.Second)
	if err != nil {
		t.Fatalf("RunStructuredReview: %v", err)
	}
	if len(raw) == 0 {
		t.Error("raw output empty")
	}
	if sr.PR.Number != 123 || sr.PR.Owner != "owner" || sr.PR.HeadSHA != "sha-abc" {
		t.Errorf("pr fields: %+v", sr.PR)
	}
	if sr.Verdict != "approve-with-suggestions" {
		t.Errorf("verdict: %q", sr.Verdict)
	}
	if len(sr.Findings) != 2 {
		t.Fatalf("findings: want 2, got %d", len(sr.Findings))
	}
	if sr.Findings[0].Severity != "major" || sr.Findings[0].ID != "M1" {
		t.Errorf("findings[0]: %+v", sr.Findings[0])
	}
	if sr.Findings[1].OriginalLine != 78 || sr.Findings[1].Line != 80 {
		t.Errorf("findings[1] line shift: %+v", sr.Findings[1])
	}
	if len(sr.Positives) != 1 {
		t.Errorf("positives: %+v", sr.Positives)
	}
}

func TestE2E_StructuredReviewError(t *testing.T) {
	dir := writeStubClaude(t, `{"error": "PR not found"}`, 0)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	sr, _, err := RunStructuredReview(context.Background(), "claude", "",
		"https://github.com/owner/repo/pull/999", "", 30*time.Second)
	if err == nil {
		t.Fatal("want error")
	}
	if sr == nil || sr.Error != "PR not found" {
		t.Errorf("expected Error field populated, got %+v", sr)
	}
}

// TestE2E_DiscoverReviewPersist walks the full pipeline through step 3:
// stubbed gh discovery -> upsert PR -> stubbed claude review -> persist
// review + comments. Verifies the DB ends in the expected state.
func TestE2E_DiscoverReviewPersist(t *testing.T) {
	// Stub both gh and claude in the same dir on PATH.
	dir := t.TempDir()
	ghFixture := `[{"number":123,"title":"Add foo","url":"https://github.com/owner/repo/pull/123","headRefOid":"sha-abc","author":{"login":"alice"}}]`
	ghScript := "#!/bin/sh\ncat <<'JSON_EOF'\n" + ghFixture + "\nJSON_EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeScript := "#!/bin/sh\ncat <<'CLAUDE_EOF'\n" + sampleStructuredJSON + "\nCLAUDE_EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	runID, err := InsertRun(ctx, db, "manual", now)
	if err != nil {
		t.Fatal(err)
	}

	prs, err := ListPRs(ctx, "repo:owner/repo", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 pr, got %d", len(prs))
	}
	prID, err := UpsertPR(ctx, db, prs[0], now)
	if err != nil {
		t.Fatal(err)
	}

	sr, raw, err := RunStructuredReview(ctx, "claude", "", prs[0].URL, "", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reviewID, err := PersistReview(ctx, db, prID, runID, sr, string(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := FinishRun(ctx, db, runID, "success", "", now); err != nil {
		t.Fatal(err)
	}

	// Assertions
	var state, headSHA, summary string
	if err := db.QueryRow(`SELECT state, head_sha, summary FROM reviews WHERE id=?`, reviewID).
		Scan(&state, &headSHA, &summary); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Errorf("state: %q want pending", state)
	}
	if headSHA != "sha-abc" {
		t.Errorf("head_sha: %q", headSHA)
	}
	if !strings.Contains(summary, "Adds foo") {
		t.Errorf("summary lost: %q", summary)
	}

	var cCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM comments WHERE review_id=?`, reviewID).Scan(&cCount); err != nil {
		t.Fatal(err)
	}
	if cCount != 2 {
		t.Errorf("comments: want 2, got %d", cCount)
	}

	var sev, body string
	var line int
	var selected, posted int
	if err := db.QueryRow(
		`SELECT severity, line, body, selected, posted FROM comments WHERE review_id=? AND severity='major'`,
		reviewID).Scan(&sev, &line, &body, &selected, &posted); err != nil {
		t.Fatal(err)
	}
	if sev != "major" || line != 42 || selected != 0 || posted != 0 {
		t.Errorf("major comment row mismatch: sev=%q line=%d selected=%d posted=%d", sev, line, selected, posted)
	}
	if !strings.Contains(body, "issue (blocking)") {
		t.Errorf("body lost conventional comments label: %q", body)
	}

	// Run status
	var runStatus string
	var runErr sql.NullString
	if err := db.QueryRow(`SELECT status, error FROM runs WHERE id=?`, runID).Scan(&runStatus, &runErr); err != nil {
		t.Fatal(err)
	}
	if runStatus != "success" {
		t.Errorf("run status: %q", runStatus)
	}
	if runErr.Valid {
		t.Errorf("run error should be null, got %q", runErr.String)
	}
}
