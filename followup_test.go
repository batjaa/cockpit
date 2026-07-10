package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_ReReviewWithFollowups exercises the whole re-review path:
//
//  1. A posted review exists (2 posted comments) at sha1.
//  2. The PR moves to sha2; ReviewOne triggers a fresh review.
//  3. cockpit fetches review threads (stub GraphQL: comment A's thread
//     resolved, comment B's has an author reply), builds the --previous
//     context file, and passes it to claude.
//  4. claude (stub) returns followups + a re-raised finding.
//  5. cockpit persists followups and the detail page renders them.
func TestE2E_ReReviewWithFollowups(t *testing.T) {
	dir := t.TempDir()
	captureDir := t.TempDir()
	argsFile := filepath.Join(captureDir, "claude-args")
	prevCopy := filepath.Join(captureDir, "previous.json")

	bodyA := "**issue (blocking):** off-by-one in loop guard."
	bodyB := "**suggestion:** rename ambiguous variable."

	// gh stub: `pr view` returns the PR at sha2; `api graphql` returns the
	// thread state for the two previously posted comments.
	threadsJSON := fmt.Sprintf(`{
  "data": {"repository": {"pullRequest": {"reviewThreads": {"nodes": [
    {"isResolved": true, "isOutdated": true,
     "comments": {"nodes": [
       {"body": %q, "path": "a.go", "line": 10, "author": {"login": "cockpit-bot"}}
     ]}},
    {"isResolved": false, "isOutdated": false,
     "comments": {"nodes": [
       {"body": %q, "path": "b.go", "line": 20, "author": {"login": "cockpit-bot"}},
       {"body": "I prefer the short name here, it matches the package style.", "path": "b.go", "line": 20, "author": {"login": "octocat"}}
     ]}}
  ]}}}}
}`, bodyA, bodyB)

	ghScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  pr)
    cat <<'JSON_EOF'
{"number":42,"title":"Add foo","url":"https://github.com/octo/repo/pull/42","headRefOid":"sha2","author":{"login":"octocat"}}
JSON_EOF
    ;;
  api)
    cat <<'JSON_EOF'
%s
JSON_EOF
    ;;
esac
`, threadsJSON)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// claude stub: records argv, snapshots the --previous file (cockpit
	// deletes it after the run), emits a review with followups.
	claudeJSON := `{
  "pr": {"url":"https://github.com/octo/repo/pull/42","owner":"octo","repo":"repo","number":42,"title":"Add foo","author":"octocat","head_sha":"sha2"},
  "summary": "Thanks for the updates — the loop guard fix looks right. One earlier point still applies; details inline.",
  "verdict": "approve-with-suggestions",
  "findings": [
    {"id":"M1","severity":"major","perfect":"E","path":"b.go","line":22,"original_line":22,"body":"**suggestion (blocking):** Raised in the previous review — still applies after the rename discussion."}
  ],
  "positives": ["Guard fix is exactly what the previous review asked for."],
  "followups": [
    {"path":"a.go","line":10,"status":"addressed","note":"Guard added; thread resolved."},
    {"path":"b.go","line":20,"status":"outstanding","note":"Author prefers short name but shadowing risk remains.","finding_id":"M1"}
  ]
}`
	claudeScript := fmt.Sprintf(`#!/bin/sh
echo "$@" > %s
prev=$(echo "$@" | sed -n 's/.*--previous \([^ ]*\).*/\1/p')
[ -n "$prev" ] && cp "$prev" %s
cat <<'CLAUDE_EOF'
%s
CLAUDE_EOF
`, argsFile, prevCopy, claudeJSON)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()

	// Seed the posted review at sha1 with two posted comments.
	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, err := UpsertPR(ctx, db, GHPR{
		Number: 42, Title: "Add foo", URL: "https://github.com/octo/repo/pull/42",
		HeadRefOid: "sha1", Author: GHAuthor{Login: "octocat"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	seedSR := &StructuredReview{
		PR:      StructuredReviewPR{HeadSHA: "sha1"},
		Summary: "prior",
		Findings: []StructuredReviewFinding{
			{ID: "M1", Severity: "major", Path: "a.go", Line: 10, Body: bodyA},
			{ID: "m1", Severity: "minor", Path: "b.go", Line: 20, Body: bodyB},
		},
	}
	postedID, err := PersistReview(ctx, db, prID, runID, seedSR, "raw", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE review_id=?`, postedID); err != nil {
		t.Fatal(err)
	}
	if err := MarkReviewPosted(ctx, db, postedID, 777, now); err != nil {
		t.Fatal(err)
	}

	// Re-review at sha2.
	cfg := Config{Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := ReviewOne(ctx, db, cfg, "https://github.com/octo/repo/pull/42", time.Now(), nil); err != nil {
		t.Fatalf("ReviewOne: %v", err)
	}

	// claude received --previous with a real context file.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("claude never invoked: %v", err)
	}
	if !strings.Contains(string(args), "--previous") {
		t.Fatalf("claude args missing --previous: %s", args)
	}

	// Context file carried thread state: A resolved, B with the reply.
	prevData, err := os.ReadFile(prevCopy)
	if err != nil {
		t.Fatalf("previous context file not captured: %v", err)
	}
	var pc previousContext
	if err := json.Unmarshal(prevData, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.PreviousReview.GithubReviewID != 777 || pc.PreviousReview.HeadSHA != "sha1" {
		t.Errorf("context review meta: %+v", pc.PreviousReview)
	}
	if len(pc.PreviousReview.Findings) != 2 {
		t.Fatalf("context findings=%d want 2", len(pc.PreviousReview.Findings))
	}
	fA, fB := pc.PreviousReview.Findings[0], pc.PreviousReview.Findings[1]
	if !fA.Resolved || !fA.Outdated {
		t.Errorf("finding A thread state: %+v", fA)
	}
	if fB.Resolved || len(fB.Replies) != 1 || fB.Replies[0].Author != "octocat" {
		t.Errorf("finding B thread state: %+v", fB)
	}

	// Followups persisted on the new pending review.
	var newReviewID int64
	if err := db.QueryRow(`SELECT id FROM reviews WHERE state='pending'`).Scan(&newReviewID); err != nil {
		t.Fatal(err)
	}
	d, err := GetReviewDetail(ctx, db, newReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Followups) != 2 {
		t.Fatalf("followups=%d want 2: %+v", len(d.Followups), d.Followups)
	}
	// Outstanding sorts first.
	if d.Followups[0].Status != "outstanding" || d.Followups[0].FindingID != "M1" {
		t.Errorf("followups[0]: %+v", d.Followups[0])
	}
	if d.Followups[1].Status != "addressed" || d.Followups[1].Path != "a.go" {
		t.Errorf("followups[1]: %+v", d.Followups[1])
	}

	// Detail page renders the Previous review section.
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)
	req := httptest.NewRequest("GET", fmt.Sprintf("/pr/%d", newReviewID), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{"Previous review", "✓ addressed", "⚠ outstanding", "re-raised as M1", "thread resolved"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

// TestE2E_ReReviewThreadFetchFails: GraphQL failure must degrade to a
// context-free review, not block it.
func TestE2E_ReReviewThreadFetchFails(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "claude-args")

	ghScript := `#!/bin/sh
case "$1" in
  pr)
    cat <<'JSON_EOF'
{"number":42,"title":"Add foo","url":"https://github.com/octo/repo/pull/42","headRefOid":"sha2","author":{"login":"octocat"}}
JSON_EOF
    ;;
  api)
    echo "GraphQL: something broke" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeJSON := `{"pr":{"url":"https://github.com/octo/repo/pull/42","owner":"octo","repo":"repo","number":42,"title":"Add foo","author":"octocat","head_sha":"sha2"},"summary":"ok","verdict":"approve","findings":[],"positives":["fine"]}`
	claudeScript := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %s\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", argsFile, claudeJSON)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()

	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, _ := UpsertPR(ctx, db, GHPR{
		Number: 42, Title: "Add foo", URL: "https://github.com/octo/repo/pull/42",
		HeadRefOid: "sha1", Author: GHAuthor{Login: "octocat"},
	}, now)
	sr := &StructuredReview{
		PR:       StructuredReviewPR{HeadSHA: "sha1"},
		Findings: []StructuredReviewFinding{{ID: "M1", Severity: "major", Path: "a.go", Line: 10, Body: "x"}},
	}
	postedID, _ := PersistReview(ctx, db, prID, runID, sr, "raw", now)
	db.Exec(`UPDATE comments SET selected=1 WHERE review_id=?`, postedID)
	MarkReviewPosted(ctx, db, postedID, 1, now)

	cfg := Config{Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := ReviewOne(ctx, db, cfg, "https://github.com/octo/repo/pull/42", time.Now(), nil); err != nil {
		t.Fatalf("ReviewOne should survive thread-fetch failure: %v", err)
	}

	// Review ran without --previous.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("claude never invoked: %v", err)
	}
	if strings.Contains(string(args), "--previous") {
		t.Errorf("claude got --previous despite fetch failure: %s", args)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE state='pending'`).Scan(&n)
	if n != 1 {
		t.Errorf("pending reviews=%d want 1", n)
	}
}
