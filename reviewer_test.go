package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeStubs writes both gh and claude stubs into dir, each returning
// the given fixture. Either may be empty to leave the existing stub
// intact (useful when rewriting between phases).
func writeStubs(t *testing.T, dir, ghFixture, claudeFixture string) {
	t.Helper()
	if ghFixture != "" {
		script := fmt.Sprintf("#!/bin/sh\ncat <<'JSON_EOF'\n%s\nJSON_EOF\n", ghFixture)
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if claudeFixture != "" {
		script := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeFixture)
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func countReviews(t *testing.T, db *sql.DB, state string) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM reviews`
	var args []any
	if state != "" {
		q += ` WHERE state=?`
		args = append(args, state)
	}
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func claudeFixtureFor(sha string) string {
	return fmt.Sprintf(`{
  "pr": {"url":"https://github.com/o/r/pull/100","owner":"o","repo":"r","number":100,"title":"P1","author":"alice","head_sha":"%s"},
  "summary": "ok",
  "verdict": "approve",
  "findings": [{"id":"n1","severity":"nit","perfect":"T","path":"a.go","line":1,"original_line":1,"body":"nitpick"}],
  "positives": ["good"]
}`, sha)
}

func ghFixtureFor(sha string) string {
	return fmt.Sprintf(
		`[{"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"%s","author":{"login":"alice"}}]`,
		sha,
	)
}

// TestE2E_DiscoverDecisionLogic exercises the full reviewer pipeline
// across three phases:
//
//	Phase 1: fresh PR with sha1            -> 1 pending review
//	Phase 2: same PR, same sha1            -> idempotent (still 1 review)
//	Phase 3: same PR, new sha2             -> old dismissed, new pending
func TestE2E_DiscoverDecisionLogic(t *testing.T) {
	dir := t.TempDir()
	writeStubs(t, dir, ghFixtureFor("sha1"), claudeFixtureFor("sha1"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{
		Search: "repo:o/r is:pr",
		Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30},
	}
	ctx := context.Background()

	// Phase 1: fresh review
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("phase 1: %v", err)
	}
	if got := countReviews(t, db, "pending"); got != 1 {
		t.Errorf("phase 1: pending=%d want 1", got)
	}
	if got := countReviews(t, db, ""); got != 1 {
		t.Errorf("phase 1: total=%d want 1", got)
	}

	// Phase 2: same SHA -> idempotent, no new review
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("phase 2: %v", err)
	}
	if got := countReviews(t, db, ""); got != 1 {
		t.Errorf("phase 2: total=%d want 1 (idempotent)", got)
	}

	// Phase 3: force-push, new SHA -> dismiss old, create new
	writeStubs(t, dir, ghFixtureFor("sha2"), claudeFixtureFor("sha2"))
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("phase 3: %v", err)
	}
	if got := countReviews(t, db, "pending"); got != 1 {
		t.Errorf("phase 3: pending=%d want 1", got)
	}
	if got := countReviews(t, db, "dismissed"); got != 1 {
		t.Errorf("phase 3: dismissed=%d want 1", got)
	}
	if got := countReviews(t, db, ""); got != 2 {
		t.Errorf("phase 3: total=%d want 2", got)
	}

	// Verify the pending review is the new sha
	var pendingSHA string
	if err := db.QueryRow(`SELECT head_sha FROM reviews WHERE state='pending'`).Scan(&pendingSHA); err != nil {
		t.Fatal(err)
	}
	if pendingSHA != "sha2" {
		t.Errorf("pending review has sha=%q want sha2", pendingSHA)
	}

	// Verify the dismissed review is the old sha
	var dismissedSHA string
	if err := db.QueryRow(`SELECT head_sha FROM reviews WHERE state='dismissed'`).Scan(&dismissedSHA); err != nil {
		t.Fatal(err)
	}
	if dismissedSHA != "sha1" {
		t.Errorf("dismissed review has sha=%q want sha1", dismissedSHA)
	}

	// Three runs recorded, all success
	var runCount, successCount int
	db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runCount)
	db.QueryRow(`SELECT COUNT(*) FROM runs WHERE status='success'`).Scan(&successCount)
	if runCount != 3 || successCount != 3 {
		t.Errorf("runs: total=%d success=%d want 3/3", runCount, successCount)
	}
}

// TestE2E_DismissedReviewNotResurrected: once the user dismisses a review, a
// later discovery run at the SAME sha must not re-review the PR — otherwise
// the dismissed review reappears (and burns another LLM run). A new commit
// (different sha) is still reviewed.
func TestE2E_DismissedReviewNotResurrected(t *testing.T) {
	dir := t.TempDir()
	writeStubs(t, dir, ghFixtureFor("sha1"), claudeFixtureFor("sha1"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{Search: "repo:o/r is:pr", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	ctx := context.Background()

	// Review once, then the user dismisses it.
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("initial discover: %v", err)
	}
	var reviewID int64
	if err := db.QueryRow(`SELECT id FROM reviews WHERE state='pending'`).Scan(&reviewID); err != nil {
		t.Fatal(err)
	}
	if err := MarkReviewDismissed(ctx, db, reviewID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// Re-run discovery at the SAME sha: the dismissed review must block it.
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("second discover: %v", err)
	}
	if got := countReviews(t, db, "pending"); got != 0 {
		t.Errorf("dismissed sha re-reviewed: pending=%d want 0", got)
	}
	if got := countReviews(t, db, ""); got != 1 {
		t.Errorf("dismissed sha re-reviewed: total=%d want 1 (no new LLM run)", got)
	}

	// A new commit (sha2) SHOULD still be reviewed.
	writeStubs(t, dir, ghFixtureFor("sha2"), claudeFixtureFor("sha2"))
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("third discover: %v", err)
	}
	if got := countReviews(t, db, "pending"); got != 1 {
		t.Errorf("new sha not reviewed: pending=%d want 1", got)
	}
}

// TestE2E_DiscoverProgress verifies the live progress tracker transitions
// through the run: queued items appear after gh discovery, and each ends
// in done (with findings count) after the review completes. A second run
// against the same SHA ends in skipped.
func TestE2E_DiscoverProgress(t *testing.T) {
	dir := t.TempDir()
	writeStubs(t, dir, ghFixtureFor("sha1"), claudeFixtureFor("sha1"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{
		Search: "repo:o/r is:pr",
		Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30},
	}
	ctx := context.Background()

	prog := NewRunProgress()
	if err := Discover(ctx, db, cfg, "manual", time.Now(), prog); err != nil {
		t.Fatal(err)
	}
	items := prog.Snapshot()
	if len(items) != 1 {
		t.Fatalf("items=%d want 1", len(items))
	}
	it := items[0]
	if it.State != prDone {
		t.Errorf("state=%q want done", it.State)
	}
	if it.Findings != 1 {
		t.Errorf("findings=%d want 1", it.Findings)
	}
	if it.Owner != "o" || it.Repo != "r" || it.Number != 100 || it.Title != "P1" {
		t.Errorf("item fields: %+v", it)
	}
	if it.StartedAt == nil {
		t.Error("StartedAt not set; reviewing state was never entered")
	}

	// Second run, same SHA -> skipped
	prog2 := NewRunProgress()
	if err := Discover(ctx, db, cfg, "manual", time.Now(), prog2); err != nil {
		t.Fatal(err)
	}
	items = prog2.Snapshot()
	if len(items) != 1 || items[0].State != prSkipped {
		t.Errorf("second run: %+v, want state=skipped", items)
	}
}

// TestE2E_ConcurrentReviews verifies the worker pool actually runs
// reviews in parallel: 4 PRs, each claude stub sleeping 1s, concurrency
// 4 — wall clock must come in well under the 4s a sequential run takes.
func TestE2E_ConcurrentReviews(t *testing.T) {
	dir := t.TempDir()
	ghFixture := `[
  {"number":1,"title":"P1","url":"https://github.com/o/r/pull/1","headRefOid":"s1","author":{"login":"a"}},
  {"number":2,"title":"P2","url":"https://github.com/o/r/pull/2","headRefOid":"s2","author":{"login":"a"}},
  {"number":3,"title":"P3","url":"https://github.com/o/r/pull/3","headRefOid":"s3","author":{"login":"a"}},
  {"number":4,"title":"P4","url":"https://github.com/o/r/pull/4","headRefOid":"s4","author":{"login":"a"}}
]`
	ghScript := fmt.Sprintf("#!/bin/sh\ncat <<'JSON_EOF'\n%s\nJSON_EOF\n", ghFixture)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeScript := fmt.Sprintf("#!/bin/sh\nsleep 1\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeFixtureFor("s1"))
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{
		Search: "repo:o/r",
		Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30, Concurrency: 4},
	}
	start := time.Now()
	if err := Discover(context.Background(), db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if got := countReviews(t, db, "pending"); got != 4 {
		t.Errorf("pending=%d want 4", got)
	}
	// Sequential would take >= 4s (4 x 1s sleeps). Allow generous slack
	// for process spawn overhead while still proving parallelism.
	if elapsed > 3*time.Second {
		t.Errorf("elapsed=%v; reviews did not run concurrently", elapsed)
	}
}

// TestE2E_InterruptedRun cancels the context mid-review and verifies:
// run row finalized as 'error: interrupted by shutdown', and no
// 'failed' review rows persisted (so the next run retries the PR).
func TestE2E_InterruptedRun(t *testing.T) {
	dir := t.TempDir()
	writeStubs(t, dir, ghFixtureFor("sha1"), "")
	// claude sleeps long enough that cancellation wins
	slowScript := "#!/bin/sh\nsleep 30\necho '{}'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(slowScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{Search: "repo:o/r", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 60, Concurrency: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond) // let the review start
		cancel()
	}()

	start := time.Now()
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("Discover returned err (should finalize the run instead): %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation did not kill claude promptly; elapsed=%v", elapsed)
	}

	var status, errMsg string
	if err := db.QueryRow(`SELECT status, COALESCE(error,'') FROM runs`).Scan(&status, &errMsg); err != nil {
		t.Fatal(err)
	}
	if status != "error" || errMsg != "interrupted by shutdown" {
		t.Errorf("run status=%q error=%q; want error/interrupted by shutdown", status, errMsg)
	}
	if got := countReviews(t, db, ""); got != 0 {
		t.Errorf("reviews=%d want 0 — interrupted review must not persist a failed row", got)
	}
}

func TestCleanupStaleRuns(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	// One zombie, one legitimately finished
	if _, err := InsertRun(ctx, db, "manual", time.Now()); err != nil {
		t.Fatal(err)
	}
	finishedID, err := InsertRun(ctx, db, "manual", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := FinishRun(ctx, db, finishedID, "success", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	n, err := CleanupStaleRuns(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleaned %d runs, want 1", n)
	}
	var status, errMsg string
	if err := db.QueryRow(`SELECT status, error FROM runs WHERE id=1`).Scan(&status, &errMsg); err != nil {
		t.Fatal(err)
	}
	if status != "error" || errMsg != "interrupted by restart" {
		t.Errorf("zombie run: status=%q error=%q", status, errMsg)
	}
	// The finished run must be untouched
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=2`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "success" {
		t.Errorf("finished run mutated: status=%q", status)
	}
}

// TestE2E_ReviewOne covers the --pr code path: ViewPR -> UpsertPR ->
// reviewIfNeeded. Uses a stub gh that handles `pr view` by ignoring args
// and returning a single object (which is what gh pr view --json emits).
func TestE2E_ReviewOne(t *testing.T) {
	dir := t.TempDir()
	prJSON := `{"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"sha1","author":{"login":"alice"}}`
	ghScript := fmt.Sprintf("#!/bin/sh\ncat <<'JSON_EOF'\n%s\nJSON_EOF\n", prJSON)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeScript := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeFixtureFor("sha1"))
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := ReviewOne(context.Background(), db, cfg, "https://github.com/o/r/pull/100", time.Now(), nil); err != nil {
		t.Fatalf("ReviewOne: %v", err)
	}
	if got := countReviews(t, db, "pending"); got != 1 {
		t.Errorf("pending=%d want 1", got)
	}
	var runTrigger string
	if err := db.QueryRow(`SELECT trigger FROM runs`).Scan(&runTrigger); err != nil {
		t.Fatal(err)
	}
	if runTrigger != "manual" {
		t.Errorf("trigger=%q want manual", runTrigger)
	}
}

// TestE2E_ManualReviewServesCache: repeating a manual review at an
// unchanged head SHA must not spend an LLM run — the existing review is
// the answer. Only a head move triggers a fresh review.
func TestE2E_ManualReviewServesCache(t *testing.T) {
	dir := t.TempDir()
	prJSON := `{"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"sha1","author":{"login":"alice"}}`
	ghScript := fmt.Sprintf("#!/bin/sh\ncat <<'JSON_EOF'\n%s\nJSON_EOF\n", prJSON)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// claude stub counts invocations so we can prove the cache hit.
	invocations := filepath.Join(t.TempDir(), "claude-invocations")
	claudeScript := fmt.Sprintf("#!/bin/sh\necho run >> %s\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n",
		invocations, claudeFixtureFor("sha1"))
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := Config{Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	ctx := context.Background()

	// First manual review runs claude and creates a pending review.
	if err := ReviewOne(ctx, db, cfg, "https://github.com/o/r/pull/100", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	// Second manual review at the same SHA: served from the DB.
	prog := NewRunProgress()
	if err := ReviewOne(ctx, db, cfg, "https://github.com/o/r/pull/100", time.Now(), prog); err != nil {
		t.Fatal(err)
	}

	if got := countReviews(t, db, "pending"); got != 1 {
		t.Errorf("pending=%d want 1 (no duplicate)", got)
	}
	if got := countReviews(t, db, ""); got != 1 {
		t.Errorf("total=%d want 1 (no LLM re-review at same SHA)", got)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "run"); n != 1 {
		t.Errorf("claude invoked %d times, want 1", n)
	}
	items := prog.Snapshot()
	if len(items) != 1 || items[0].State != prSkipped {
		t.Errorf("second run progress: %+v, want skipped", items)
	}
	// Both runs succeed — a cache hit is not an error.
	var successes int
	db.QueryRow(`SELECT COUNT(*) FROM runs WHERE status='success'`).Scan(&successes)
	if successes != 2 {
		t.Errorf("successful runs=%d want 2", successes)
	}
}

// TestE2E_ReconcileVanishedPRs: a PR with a pending review that merged and
// dropped out of the search gets reconciled during discover — its stored
// state is refreshed AND its now-pointless pending review is dismissed, so it
// leaves the pending list instead of lingering.
func TestE2E_ReconcileVanishedPRs(t *testing.T) {
	dir := t.TempDir()
	// pr list returns ONLY pr/200; pr view (called for vanished pr/100)
	// reports it MERGED.
	ghScript := `#!/bin/sh
if [ "$2" = "list" ]; then
  cat <<'JSON_EOF'
[{"number":200,"title":"P2","url":"https://github.com/o/r/pull/200","headRefOid":"s2","author":{"login":"a"},"state":"OPEN"}]
JSON_EOF
else
  cat <<'JSON_EOF'
{"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"s1b","author":{"login":"a"},"state":"MERGED"}
JSON_EOF
fi
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeScript := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeFixtureFor("s2"))
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

	// Seed: pr/100 open with a pending review (from an earlier run).
	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, _ := UpsertPR(ctx, db, GHPR{
		Number: 100, Title: "P1", URL: "https://github.com/o/r/pull/100",
		HeadRefOid: "s1", Author: GHAuthor{Login: "a"}, State: "OPEN",
	}, now)
	sr := &StructuredReview{PR: StructuredReviewPR{HeadSHA: "s1"}}
	if _, err := PersistReview(ctx, db, prID, runID, sr, "raw", now); err != nil {
		t.Fatal(err)
	}

	// Discover: pr/100 is absent from results; reconcile must mark it MERGED.
	cfg := Config{Search: "repo:o/r", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	var state, headSHA string
	if err := db.QueryRow(`SELECT state, head_sha FROM prs WHERE number=100`).Scan(&state, &headSHA); err != nil {
		t.Fatal(err)
	}
	if state != "MERGED" {
		t.Errorf("vanished pr state=%q want MERGED", state)
	}
	if headSHA != "s1b" {
		t.Errorf("vanished pr head not refreshed: %q", headSHA)
	}

	// The merged PR's pending review is dismissed — no longer actionable.
	var pending, dismissed int
	db.QueryRow(`SELECT COUNT(*) FROM reviews r JOIN prs p ON p.id=r.pr_id WHERE p.number=100 AND r.state='pending'`).Scan(&pending)
	db.QueryRow(`SELECT COUNT(*) FROM reviews r JOIN prs p ON p.id=r.pr_id WHERE p.number=100 AND r.state='dismissed'`).Scan(&dismissed)
	if pending != 0 || dismissed != 1 {
		t.Errorf("merged pr reviews: pending=%d dismissed=%d want 0/1", pending, dismissed)
	}

	// And it's gone from the dashboard's pending list.
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "/pull/100") {
		t.Error("merged PR still shown in pending list after reconcile")
	}
}

// TestE2E_ReconcileKeepsOpenPR: a still-open, unapproved PR that dropped out
// of the search keeps its pending review — reconcile clears only merged,
// closed, or approved PRs, never actionable ones.
func TestE2E_ReconcileKeepsOpenPR(t *testing.T) {
	dir := t.TempDir()
	// list returns nothing; pr view reports pr/101 still OPEN, not approved.
	ghScript := `#!/bin/sh
if [ "$2" = "list" ]; then
  echo "[]"
else
  cat <<'JSON_EOF'
{"number":101,"title":"P101","url":"https://github.com/o/r/pull/101","headRefOid":"s1","author":{"login":"a"},"state":"OPEN","latestReviews":[]}
JSON_EOF
fi
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
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
		Number: 101, Title: "P101", URL: "https://github.com/o/r/pull/101",
		HeadRefOid: "s1", Author: GHAuthor{Login: "a"}, State: "OPEN",
	}, now)
	sr := &StructuredReview{PR: StructuredReviewPR{HeadSHA: "s1"}}
	if _, err := PersistReview(ctx, db, prID, runID, sr, "raw", now); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Search: "repo:o/r", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := Discover(ctx, db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatal(err)
	}

	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM reviews r JOIN prs p ON p.id=r.pr_id WHERE p.number=101 AND r.state='pending'`).Scan(&pending)
	if pending != 1 {
		t.Errorf("open unapproved pr: pending=%d want 1 (must stay in the list)", pending)
	}
}

// TestE2E_SkipApprovedByUser: a PR whose latest review by the gh user is
// APPROVED is not re-reviewed by discovery, and its pending cockpit
// reviews are auto-dismissed. Other PRs still review normally.
func TestE2E_SkipApprovedByUser(t *testing.T) {
	dir := t.TempDir()
	ghScript := `#!/bin/sh
if [ "$1" = "api" ]; then
  echo "me"
  exit 0
fi
cat <<'JSON_EOF'
[
  {"number":100,"title":"P1","url":"https://github.com/o/r/pull/100","headRefOid":"s1-new","author":{"login":"alice"},"state":"OPEN",
   "latestReviews":[{"author":{"login":"me"},"state":"APPROVED"},{"author":{"login":"bot"},"state":"COMMENTED"}]},
  {"number":200,"title":"P2","url":"https://github.com/o/r/pull/200","headRefOid":"s2","author":{"login":"alice"},"state":"OPEN",
   "latestReviews":[{"author":{"login":"me"},"state":"CHANGES_REQUESTED"}]}
]
JSON_EOF
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	claudeScript := fmt.Sprintf("#!/bin/sh\ncat <<'CLAUDE_EOF'\n%s\nCLAUDE_EOF\n", claudeFixtureFor("s2"))
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

	// Seed pr/100 with a stale pending review (older SHA) — the user has
	// since approved on GitHub.
	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, _ := UpsertPR(ctx, db, GHPR{
		Number: 100, Title: "P1", URL: "https://github.com/o/r/pull/100",
		HeadRefOid: "s1-old", Author: GHAuthor{Login: "alice"}, State: "OPEN",
	}, now)
	sr := &StructuredReview{PR: StructuredReviewPR{HeadSHA: "s1-old"}}
	if _, err := PersistReview(ctx, db, prID, runID, sr, "raw", now); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Search: "repo:o/r", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	prog := NewRunProgress()
	if err := Discover(ctx, db, cfg, "manual", time.Now(), prog); err != nil {
		t.Fatal(err)
	}

	// pr/100: pending dismissed, no new review.
	var dismissed, pendingFor100 int
	db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE pr_id=? AND state='dismissed'`, prID).Scan(&dismissed)
	db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE pr_id=? AND state='pending'`, prID).Scan(&pendingFor100)
	if dismissed != 1 || pendingFor100 != 0 {
		t.Errorf("approved pr: dismissed=%d pending=%d; want 1/0", dismissed, pendingFor100)
	}

	// pr/200 (changes requested by me): reviewed normally.
	var pendingTotal int
	db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE state='pending'`).Scan(&pendingTotal)
	if pendingTotal != 1 {
		t.Errorf("pending total=%d want 1 (pr/200 only)", pendingTotal)
	}

	// Progress: pr/100 skipped, pr/200 done.
	for _, it := range prog.Snapshot() {
		switch it.Number {
		case 100:
			if it.State != prSkipped {
				t.Errorf("pr/100 progress=%q want skipped", it.State)
			}
		case 200:
			if it.State != prDone {
				t.Errorf("pr/200 progress=%q want done", it.State)
			}
		}
	}
}

// TestE2E_FailedReviewPersists covers the case where claude exits non-zero
// or emits unparseable output. We expect a 'failed' review row with the
// raw output retained for debugging.
func TestE2E_FailedReviewPersists(t *testing.T) {
	dir := t.TempDir()
	writeStubs(t, dir, ghFixtureFor("sha1"), "")
	// claude exits 0 but emits no JSON
	failScript := "#!/bin/sh\necho 'I refuse to review this'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(failScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := Config{Search: "repo:o/r", Claude: ClaudeConfig{Binary: "claude", TimeoutSeconds: 30}}
	if err := Discover(context.Background(), db, cfg, "manual", time.Now(), nil); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := countReviews(t, db, "failed"); got != 1 {
		t.Errorf("failed reviews=%d want 1", got)
	}
	var raw string
	if err := db.QueryRow(`SELECT raw_output FROM reviews WHERE state='failed'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Error("raw_output empty; should retain failed claude output for debugging")
	}
	// Run should be marked partial or error since 1/1 failed
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs`).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "error" {
		t.Errorf("run status=%q want error (all PRs failed)", runStatus)
	}
}
