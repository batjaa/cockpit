package main

import (
	"context"
	"database/sql"
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

// submitStubDiff covers a.go line 10 (hunk 5-19) and b.go line 20 (hunk
// 15-24). c.go is absent — its line=-1 comment is never postable.
const submitStubDiff = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -5,10 +5,15 @@ func foo() {
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -15,5 +15,10 @@ func bar() {
`

// writeSubmitStubGH writes a gh stub that handles the subcommands the
// submit flow uses:
//   - `gh pr view ...`  -> returns a PR with viewSHA as head
//   - `gh pr diff ...`  -> returns submitStubDiff
//   - `gh api ...`      -> captures stdin to captureFile, returns a fake
//     posted-review response
//
// The capture file's existence doubles as proof the API call happened.
func writeSubmitStubGH(t *testing.T, dir, viewSHA, captureFile string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "pr view")
    cat <<JSON_EOF
{"number":42,"title":"Add foo","url":"https://github.com/octo/repo/pull/42","headRefOid":"%s","author":{"login":"octocat"}}
JSON_EOF
    ;;
  "pr diff")
    cat <<'DIFF_EOF'
%s
DIFF_EOF
    ;;
  api*)
    cat > %s
    cat <<'JSON_EOF'
{"id":777,"html_url":"https://github.com/octo/repo/pull/42#pullrequestreview-777","state":"COMMENTED"}
JSON_EOF
    ;;
  *)
    echo "unexpected gh invocation: $@" >&2
    exit 1
    ;;
esac
`, viewSHA, submitStubDiff, captureFile)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// seedSubmitReview creates a pr + review (head sha "abc1234") with three
// comments: two selectable findings and one with line=-1 (unresolvable).
func seedSubmitReview(t *testing.T, db *sql.DB) (reviewID int64, commentIDs []int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	runID, err := InsertRun(ctx, db, "manual", now)
	if err != nil {
		t.Fatal(err)
	}
	prID, err := UpsertPR(ctx, db, GHPR{
		Number: 42, Title: "Add foo", URL: "https://github.com/octo/repo/pull/42",
		HeadRefOid: "abc1234", Author: GHAuthor{Login: "octocat"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	sr := &StructuredReview{
		PR:      StructuredReviewPR{HeadSHA: "abc1234"},
		Summary: "Looks solid overall.",
		Findings: []StructuredReviewFinding{
			{ID: "M1", Severity: "major", Path: "a.go", Line: 10, Body: "**issue (blocking):** off-by-one"},
			{ID: "m1", Severity: "minor", Path: "b.go", Line: 20, Body: "**suggestion:** rename"},
			{ID: "m2", Severity: "minor", Path: "c.go", Line: -1, Body: "**suggestion:** unresolvable line"},
		},
	}
	reviewID, err = PersistReview(ctx, db, prID, runID, sr, "raw", now)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id FROM comments WHERE review_id=? ORDER BY id`, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		commentIDs = append(commentIDs, id)
	}
	return reviewID, commentIDs
}

func newSubmitServer(t *testing.T) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)
	return db, mux
}

func postJSON(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// TestE2E_SubmitReview is the full happy path: select 2 of 3 comments
// (one selected comment has line=-1 and must be excluded), submit as
// COMMENT, verify the gh api payload and the DB state transitions.
func TestE2E_SubmitReview(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	writeSubmitStubGH(t, dir, "abc1234", captureFile) // same SHA -> not stale
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, commentIDs := seedSubmitReview(t, db)

	// Select the major (line 10) and the unresolvable one (line -1).
	for _, id := range []int64{commentIDs[0], commentIDs[2]} {
		if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Response shape
	var resp struct {
		GithubURL         string `json:"github_url"`
		PostedComments    int    `json:"posted_comments"`
		SkippedUnresolved int    `json:"skipped_unresolved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PostedComments != 1 || resp.SkippedUnresolved != 1 {
		t.Errorf("posted=%d skipped=%d; want 1/1", resp.PostedComments, resp.SkippedUnresolved)
	}
	if !strings.Contains(resp.GithubURL, "pullrequestreview-777") {
		t.Errorf("github_url=%q", resp.GithubURL)
	}

	// Captured gh api payload
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("gh api was never called: %v", err)
	}
	var payload ReviewPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CommitID != "abc1234" {
		t.Errorf("commit_id=%q", payload.CommitID)
	}
	if payload.Event != "COMMENT" {
		t.Errorf("event=%q", payload.Event)
	}
	if payload.Body != "Looks solid overall." {
		t.Errorf("body=%q", payload.Body)
	}
	if len(payload.Comments) != 1 {
		t.Fatalf("payload comments=%d want 1 (unselected + unresolvable excluded)", len(payload.Comments))
	}
	c := payload.Comments[0]
	if c.Path != "a.go" || c.Line != 10 || c.Side != "RIGHT" || !strings.Contains(c.Body, "off-by-one") {
		t.Errorf("payload comment: %+v", c)
	}

	// DB state
	var state string
	var ghID int64
	if err := db.QueryRow(`SELECT state, github_review_id FROM reviews WHERE id=?`, reviewID).Scan(&state, &ghID); err != nil {
		t.Fatal(err)
	}
	if state != "posted" || ghID != 777 {
		t.Errorf("review state=%q gh_id=%d; want posted/777", state, ghID)
	}
	var postedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM comments WHERE review_id=? AND posted=1`, reviewID).Scan(&postedCount); err != nil {
		t.Fatal(err)
	}
	// Both selected comments flagged posted (the line=-1 one was skipped at
	// GitHub but is still part of the user's selection record).
	if postedCount != 2 {
		t.Errorf("posted comments=%d want 2", postedCount)
	}

	// Submitting again must now 409 (state no longer pending).
	w = postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("resubmit status=%d want 409", w.Code)
	}
}

// TestE2E_SubmitStaleSHA: gh pr view reports a different head than the
// review was generated against -> 409, nothing posted, state unchanged.
func TestE2E_SubmitStaleSHA(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	writeSubmitStubGH(t, dir, "ffff999", captureFile) // different SHA -> stale
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, commentIDs := seedSubmitReview(t, db)
	if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE id=?`, commentIDs[0]); err != nil {
		t.Fatal(err)
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "re-review") {
		t.Errorf("stale message should suggest re-review: %s", w.Body.String())
	}
	if _, err := os.Stat(captureFile); !os.IsNotExist(err) {
		t.Error("gh api was called despite stale SHA")
	}
	var state string
	db.QueryRow(`SELECT state FROM reviews WHERE id=?`, reviewID).Scan(&state)
	if state != "pending" {
		t.Errorf("state=%q want pending (unchanged)", state)
	}
}

// TestE2E_SubmitOutOfHunkLine: a selected comment whose line falls
// outside the PR's diff hunks is snapped to the nearest in-hunk line —
// the skill's line resolution drifts, and GitHub's own 422 is uselessly
// vague. In-hunk comments stay untouched.
func TestE2E_SubmitOutOfHunkLine(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	// Stub diff covers a.go only up to line 19 — comment at b.go:20 will
	// be invalid because we shrink b.go's hunk to 5 lines (15-19... use a
	// custom stub: b.go hunk @@ +15,3 @@ covers 15-17, so line 20 is out.
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "pr view")
    cat <<'JSON_EOF'
{"number":42,"title":"Add foo","url":"https://github.com/octo/repo/pull/42","headRefOid":"abc1234","author":{"login":"octocat"}}
JSON_EOF
    ;;
  "pr diff")
    cat <<'DIFF_EOF'
diff --git a/a.go b/a.go
+++ b/a.go
@@ -5,10 +5,15 @@
diff --git a/b.go b/b.go
+++ b/b.go
@@ -15,3 +15,3 @@
DIFF_EOF
    ;;
  api*)
    cat > %s
    echo '{"id":1,"html_url":"x","state":"COMMENTED"}'
    ;;
esac
`, captureFile)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, commentIDs := seedSubmitReview(t, db)
	// Select a.go:10 (in hunk) AND b.go:20 (out of hunk).
	for _, id := range []int64{commentIDs[0], commentIDs[1]} {
		if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		SnappedLines int `json:"snapped_lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SnappedLines != 1 {
		t.Errorf("snapped_lines=%d want 1", resp.SnappedLines)
	}

	// The captured payload must carry the SNAPPED line (b.go hunk is
	// 15-17, so 20 snaps to 17); a.go:10 stays untouched.
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("gh api never called: %v", err)
	}
	var payload ReviewPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	lines := map[string]int{}
	for _, c := range payload.Comments {
		lines[c.Path] = c.Line
	}
	if lines["a.go"] != 10 || lines["b.go"] != 17 {
		t.Errorf("payload lines: %v; want a.go:10, b.go snapped to 17", lines)
	}
}

// TestE2E_SubmitUnanchorableFile: a selected comment on a file absent
// from the diff cannot be snapped — refuse with specifics, post nothing.
func TestE2E_SubmitUnanchorableFile(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	writeSubmitStubGH(t, dir, "abc1234", captureFile) // stub diff has a.go, b.go only
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, _ := seedSubmitReview(t, db)
	// Insert + select a comment on a file the diff doesn't touch.
	if _, err := db.Exec(
		`INSERT INTO comments (review_id, severity, path, line, body, selected) VALUES (?, 'minor', 'd.go', 5, 'x', 1)`,
		reviewID); err != nil {
		t.Fatal(err)
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "d.go:5") {
		t.Errorf("error should name the unanchorable comment: %s", w.Body.String())
	}
	if _, err := os.Stat(captureFile); !os.IsNotExist(err) {
		t.Error("gh api was called despite unanchorable file")
	}
	var state string
	db.QueryRow(`SELECT state FROM reviews WHERE id=?`, reviewID).Scan(&state)
	if state != "pending" {
		t.Errorf("state=%q want pending", state)
	}
}

func TestParseDiffHunks(t *testing.T) {
	hunks := parseDiffHunks(submitStubDiff)
	cases := []struct {
		path string
		line int
		want bool
	}{
		{"a.go", 5, true},
		{"a.go", 10, true},
		{"a.go", 19, true},
		{"a.go", 20, false},
		{"a.go", 4, false},
		{"b.go", 15, true},
		{"b.go", 24, true},
		{"b.go", 25, false},
		{"c.go", 1, false}, // file not in diff
	}
	for _, c := range cases {
		if got := lineInHunks(hunks, c.path, c.line); got != c.want {
			t.Errorf("lineInHunks(%s:%d)=%v want %v", c.path, c.line, got, c.want)
		}
	}
}

// TestE2E_SubmitMergedPR: posting to a merged PR is refused with an
// explicit message, even when the SHA still matches.
func TestE2E_SubmitMergedPR(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	// Same SHA as the review, but state MERGED.
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  pr)
    cat <<'JSON_EOF'
{"number":42,"title":"Add foo","url":"https://github.com/octo/repo/pull/42","headRefOid":"abc1234","author":{"login":"octocat"},"state":"MERGED"}
JSON_EOF
    ;;
  api)
    cat > %s
    echo '{"id":1,"html_url":"x","state":"COMMENTED"}'
    ;;
esac
`, captureFile)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, commentIDs := seedSubmitReview(t, db)
	if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE id=?`, commentIDs[0]); err != nil {
		t.Fatal(err)
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "MERGED") {
		t.Errorf("message should name the PR state: %s", w.Body.String())
	}
	if _, err := os.Stat(captureFile); !os.IsNotExist(err) {
		t.Error("gh api was called despite merged PR")
	}
	var state string
	db.QueryRow(`SELECT state FROM reviews WHERE id=?`, reviewID).Scan(&state)
	if state != "pending" {
		t.Errorf("review state=%q want pending", state)
	}
}

// TestE2E_SubmitValidation: bad event, empty selection, missing review.
func TestE2E_SubmitValidation(t *testing.T) {
	dir := t.TempDir()
	writeSubmitStubGH(t, dir, "abc1234", filepath.Join(t.TempDir(), "cap.json"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, _ := seedSubmitReview(t, db)

	// Unknown event
	if w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"MERGE"}`); w.Code != 400 {
		t.Errorf("bad event: status=%d want 400", w.Code)
	}
	// COMMENT with nothing selected
	if w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`); w.Code != 400 {
		t.Errorf("empty selection: status=%d want 400", w.Code)
	}
	// Nonexistent review
	if w := postJSON(mux, "/pr/9999/submit", `{"event":"COMMENT"}`); w.Code != 404 {
		t.Errorf("missing review: status=%d want 404", w.Code)
	}
}

// TestE2E_SubmitApproveWithoutComments: APPROVE is valid with zero
// selected comments (approval itself is the action) and without a
// summary — GitHub allows a bare approval, so the body field must be
// omitted entirely when empty.
func TestE2E_SubmitApproveWithoutComments(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	writeSubmitStubGH(t, dir, "abc1234", captureFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, _ := seedSubmitReview(t, db)
	// User cleared the summary before approving.
	if _, err := db.Exec(`UPDATE reviews SET summary='' WHERE id=?`, reviewID); err != nil {
		t.Fatal(err)
	}

	w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"APPROVE"}`)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatal(err)
	}
	var payload ReviewPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "APPROVE" || len(payload.Comments) != 0 {
		t.Errorf("payload: event=%q comments=%d", payload.Event, len(payload.Comments))
	}
	// Check the RAW serialization, not the round-tripped struct: GitHub
	// 422s on "comments": null, which unmarshals indistinguishably from [].
	if strings.Contains(string(data), `"comments":null`) {
		t.Error(`payload serialized "comments":null — real GitHub rejects this with HTTP 422`)
	}
	if !strings.Contains(string(data), `"comments":[]`) {
		t.Errorf("payload should contain an empty comments array: %s", data)
	}
	// Empty summary must omit the body field, not send "body": "".
	if strings.Contains(string(data), `"body"`) {
		t.Errorf("empty summary should omit body entirely: %s", data)
	}
}

// TestE2E_EditSummaryThenSubmit: PATCH the summary, then submit — the
// gh api payload must carry the edited text, not the original.
func TestE2E_EditSummaryThenSubmit(t *testing.T) {
	dir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "payload.json")
	writeSubmitStubGH(t, dir, "abc1234", captureFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, mux := newSubmitServer(t)
	reviewID, commentIDs := seedSubmitReview(t, db)
	if _, err := db.Exec(`UPDATE comments SET selected=1 WHERE id=?`, commentIDs[0]); err != nil {
		t.Fatal(err)
	}

	edited := "Thanks for the cleanup — one inline suggestion on the loop guard."
	req := httptest.NewRequest("PATCH", fmt.Sprintf("/reviews/%d", reviewID),
		strings.NewReader(fmt.Sprintf(`{"summary": %q}`, edited)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH summary status=%d body=%s", w.Code, w.Body.String())
	}

	// Edit persisted
	var stored string
	if err := db.QueryRow(`SELECT summary FROM reviews WHERE id=?`, reviewID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != edited {
		t.Errorf("stored summary=%q", stored)
	}

	// Submit posts the edited text
	if w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`); w.Code != 200 {
		t.Fatalf("submit status=%d body=%s", w.Code, w.Body.String())
	}
	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatal(err)
	}
	var payload ReviewPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Body != edited {
		t.Errorf("posted body=%q want edited summary", payload.Body)
	}

	// Posted review is no longer editable
	req = httptest.NewRequest("PATCH", fmt.Sprintf("/reviews/%d", reviewID),
		strings.NewReader(`{"summary": "too late"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("edit after post: status=%d want 409", w.Code)
	}
	db.QueryRow(`SELECT summary FROM reviews WHERE id=?`, reviewID).Scan(&stored)
	if stored != edited {
		t.Errorf("posted summary mutated: %q", stored)
	}
}

// TestE2E_ManualReviewEndpoint: POST /review kicks off an async single-PR
// review (stub gh + claude). Asserts 202, then waits for the pipeline to
// land a pending review with trigger='manual', and checks the progress
// tracker ends in done.
func TestE2E_ManualReviewEndpoint(t *testing.T) {
	dir := t.TempDir()
	prJSON := `{"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"sha1","author":{"login":"alice"}}`
	ghScript := fmt.Sprintf("#!/bin/sh\ncat <<'JSON_EOF'\n%s\nJSON_EOF\n", prJSON)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeJSON := `{"pr":{"url":"https://github.com/o/r/pull/100","owner":"o","repo":"r","number":100,"title":"P1","author":"alice","head_sha":"sha1"},"summary":"ok","verdict":"approve","findings":[{"id":"n1","severity":"nit","perfect":"T","path":"a.go","line":1,"original_line":1,"body":"nit"}],"positives":["good"]}`
	claudeScript := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeJSON)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, cfg: Config{Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)

	// URL with query string + /files suffix must normalize cleanly.
	w := postJSON(mux, "/review", `{"url": "https://github.com/o/r/pull/100/files?diff=split"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Async pipeline: wait for the review row (claude stub is instant, so
	// this resolves in well under a second).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE state='pending'`).Scan(&n)
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("review never landed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var trigger, status string
	if err := db.QueryRow(`SELECT trigger, status FROM runs ORDER BY id DESC LIMIT 1`).Scan(&trigger, &status); err != nil {
		t.Fatal(err)
	}
	if trigger != "manual" {
		t.Errorf("trigger=%q want manual", trigger)
	}

	// Progress tracker reflects the single PR ending in done.
	deadline = time.Now().Add(5 * time.Second)
	for {
		items := s.progress.Load().Snapshot()
		if len(items) == 1 && items[0].State == prDone && items[0].Findings == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never reached done: %+v", s.progress.Load().Snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2E_ManualReviewValidation: malformed URLs are rejected before a
// run slot is consumed; a review submitted while busy is queued, not rejected.
func TestE2E_ManualReviewValidation(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)

	for _, body := range []string{
		`{"url": "https://github.com/o/r/issues/5"}`, // not a PR
		`{"url": "https://gitlab.com/o/r/pull/5"}`,   // not github
		`{"url": "https://github.com/o/r/pull/abc"}`, // bad number
		`{"url": ""}`, // empty
		`not json`,    // bad body
	} {
		if w := postJSON(mux, "/review", body); w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status=%d want 400", body, w.Code)
		}
	}
	// No run rows consumed by rejected requests
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	if n != 0 {
		t.Errorf("runs=%d want 0", n)
	}

	// Busy server -> the review queues behind the in-flight run, not rejected.
	s.runMu.Lock()
	s.active = true
	s.current = &runJob{kind: runDiscover, trigger: "test"}
	s.runMu.Unlock()
	w := postJSON(mux, "/review", `{"url": "https://github.com/o/r/pull/5"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("busy: status=%d want 202", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Errorf("busy: want status:queued, got %s", w.Body.String())
	}
}

func TestE2E_DismissReview(t *testing.T) {
	db, mux := newSubmitServer(t)
	reviewID, _ := seedSubmitReview(t, db)

	w := postJSON(mux, fmt.Sprintf("/pr/%d/dismiss", reviewID), ``)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}
	var state string
	db.QueryRow(`SELECT state FROM reviews WHERE id=?`, reviewID).Scan(&state)
	if state != "dismissed" {
		t.Errorf("state=%q want dismissed", state)
	}
	// Dismissing again -> 409
	if w := postJSON(mux, fmt.Sprintf("/pr/%d/dismiss", reviewID), ``); w.Code != http.StatusConflict {
		t.Errorf("re-dismiss status=%d want 409", w.Code)
	}
	// Submitting a dismissed review -> 409
	if w := postJSON(mux, fmt.Sprintf("/pr/%d/submit", reviewID), `{"event":"COMMENT"}`); w.Code != http.StatusConflict {
		t.Errorf("submit dismissed: status=%d want 409", w.Code)
	}
}
