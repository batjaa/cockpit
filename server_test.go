package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedReview inserts a complete pr + run + review + 2 comments and
// returns the review id. Used by the server smoke tests.
func seedReview(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	runID, err := InsertRun(ctx, db, "manual", now)
	if err != nil {
		t.Fatal(err)
	}

	prID, err := UpsertPR(ctx, db, GHPR{
		Number:     42,
		Title:      "Add foo to bar",
		URL:        "https://github.com/octo/repo/pull/42",
		HeadRefOid: "abc1234",
		Author:     GHAuthor{Login: "octocat"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	sr := &StructuredReview{
		PR:      StructuredReviewPR{HeadSHA: "abc1234"},
		Summary: "Generally fine.",
		Verdict: "approve-with-suggestions",
		Findings: []StructuredReviewFinding{
			{ID: "M1", Severity: "major", Path: "a.go", Line: 10, Body: "**issue (blocking):** Off-by-one."},
			{ID: "n1", Severity: "nit", Path: "b.go", Line: 20, Body: "**nitpick (non-blocking):** name shadowing."},
		},
	}
	reviewID, err := PersistReview(ctx, db, prID, runID, sr, "raw", now)
	if err != nil {
		t.Fatal(err)
	}
	_ = FinishRun(ctx, db, runID, "success", "", now)
	return reviewID
}

func TestServer_DashboardEmpty(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No pending reviews") {
		t.Errorf("missing empty-state copy:\n%s", w.Body.String())
	}
}

func TestServer_DashboardWithReview(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReview(t, db)

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"octo/repo #42", "Add foo to bar", "octocat", "🟠 1", "nit 1",
		// PR number links to GitHub in a new tab
		`href="https://github.com/octo/repo/pull/42" target="_blank"`,
		// quick actions: dismiss button + approve popover with editable summary
		`class="dismiss-cta`, `class="approve-confirm`, `class="approve-summary`, `data-pr="#42"`,
		// severity badges open per-severity popovers with selectable rows
		`class="comment-toggle`, "a.go:10", "Off-by-one",
		"Major — check to include", "Nit — check to include",
		`group-hover/sev:block`,
		// all-time stats line
		"🏆 1 PRs reviewed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard body", want)
		}
	}
}

func TestServer_DetailPage(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reviewID := seedReview(t, db)

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	path := "/pr/" + intToStr(reviewID)
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"Add foo to bar", "a.go:10", "Off-by-one", "b.go:20", "Generally fine", "abc1234"[:7]} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in detail body", want)
		}
	}
}

// TestServer_DetailPageRendersMarkdown seeds a review whose comment body
// uses Conventional Comments markdown and asserts the detail page renders
// real HTML — and that raw HTML in the body is NOT passed through (review
// bodies can quote attacker-controlled PR content).
func TestServer_DetailPageRendersMarkdown(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now()
	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, _ := UpsertPR(ctx, db, GHPR{
		Number: 1, Title: "T", URL: "https://github.com/o/r/pull/1",
		HeadRefOid: "s", Author: GHAuthor{Login: "a"},
	}, now)
	sr := &StructuredReview{
		PR:      StructuredReviewPR{HeadSHA: "s"},
		Summary: "Fine.",
		Findings: []StructuredReviewFinding{
			{
				ID: "M1", Severity: "major", Path: "a.go", Line: 5,
				Body: "**issue (blocking):** Nil deref.\n\nUse `items[0]` guard:\n\n```go\nif len(items) == 0 {\n\treturn nil\n}\n```\n\n<script>alert('xss')</script>",
			},
		},
	}
	reviewID, err := PersistReview(ctx, db, prID, runID, sr, "raw", now)
	if err != nil {
		t.Fatal(err)
	}

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)
	req := httptest.NewRequest("GET", "/pr/"+intToStr(reviewID), nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<strong>issue (blocking):</strong>", // ** -> strong
		"<code>items[0]</code>",              // backticks -> code
		"<pre",                               // fence -> pre block
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in rendered body", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("raw HTML passed through markdown rendering — XSS")
	}
}

// TestServer_DiffHunkRendering seeds a review with one comment carrying a
// diff hunk whose code text is attacker-controlled, and one comment with no
// hunk. Both comment surfaces — the dashboard severity popover and the
// detail page card — must render the hunk as an escaped, colored block, and
// must render no container at all for the hunk-less comment.
func TestServer_DiffHunkRendering(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now()
	runID, _ := InsertRun(ctx, db, "manual", now)
	prID, _ := UpsertPR(ctx, db, GHPR{
		Number: 9, Title: "Hunk PR", URL: "https://github.com/o/r/pull/9",
		HeadRefOid: "s", Author: GHAuthor{Login: "a"},
	}, now)
	hunk := "@@ -1,2 +1,3 @@\n func main() {\n-\told()\n+\tinject := \"<script>alert(1)</script>\"\n }"
	sr := &StructuredReview{
		PR:      StructuredReviewPR{HeadSHA: "s"},
		Summary: "Fine.",
		Findings: []StructuredReviewFinding{
			{ID: "M1", Severity: "major", Path: "a.go", Line: 4, Body: "**issue (blocking):** Injected.", DiffHunk: hunk},
			{ID: "n1", Severity: "nit", Path: "b.go", Line: 8, Body: "**nitpick (non-blocking):** meh."}, // no hunk
		},
	}
	reviewID, err := PersistReview(ctx, db, prID, runID, sr, "raw", now)
	if err != nil {
		t.Fatal(err)
	}

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	for _, path := range []string{"/", "/pr/" + intToStr(reviewID)} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET %s status=%d body=%s", path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{
			// hunk header line, zinc-styled
			`text-zinc-500 bg-zinc-50">@@ -1,2 +1,3 @@</span>`,
			// added line got the green classes; its code text is escaped
			`bg-green-50 text-green-800">+`,
			// removed line got the red classes
			`bg-red-50 text-red-800">-`,
			// arbitrary code is HTML-escaped, never raw
			"&lt;script&gt;alert(1)&lt;/script&gt;",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s: missing %q in body", path, want)
			}
		}
		if strings.Contains(body, "<script>alert") {
			t.Errorf("GET %s: hunk code passed through unescaped — XSS", path)
		}
		// Exactly one comment carries a hunk; the empty-hunk comment must
		// render no diff container at all.
		if got := strings.Count(body, "data-diff-hunk"); got != 1 {
			t.Errorf("GET %s: data-diff-hunk containers = %d, want 1 (empty hunk must render nothing)", path, got)
		}
	}
}

func TestServer_CommentToggle(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedReview(t, db)

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	// Comment 1 should start unselected
	var sel int
	db.QueryRow("SELECT selected FROM comments WHERE id=1").Scan(&sel)
	if sel != 0 {
		t.Fatalf("initial selected=%d want 0", sel)
	}

	// Toggle on
	req := httptest.NewRequest("PATCH", "/comments/1", strings.NewReader(`{"selected": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH status=%d body=%s", w.Code, w.Body.String())
	}
	db.QueryRow("SELECT selected FROM comments WHERE id=1").Scan(&sel)
	if sel != 1 {
		t.Errorf("after toggle on: selected=%d want 1", sel)
	}

	// Toggle off
	req = httptest.NewRequest("PATCH", "/comments/1", strings.NewReader(`{"selected": false}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PATCH off status=%d", w.Code)
	}
	db.QueryRow("SELECT selected FROM comments WHERE id=1").Scan(&sel)
	if sel != 0 {
		t.Errorf("after toggle off: selected=%d want 0", sel)
	}
}

func TestServer_RunNow_MissingSearch(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	s := &server{db: db, cfg: Config{}} // empty Search
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("POST", "/run-now", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("status=%d body=%s want 412", w.Code, w.Body.String())
	}
}

func TestServer_RunNow_CoalescesWhileDiscovering(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	s := &server{db: db, cfg: Config{Search: "x"}, baseCtx: context.Background()}
	// Simulate an in-flight discovery run holding the worker.
	s.runMu.Lock()
	s.active = true
	s.current = &runJob{kind: runDiscover, trigger: "test"}
	s.runMu.Unlock()
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("POST", "/run-now", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	// A second discovery coalesces with the active one — accepted, not run.
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"duplicate"`) {
		t.Errorf("want status:duplicate, got %s", w.Body.String())
	}
}

func TestServer_RunStatus(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("GET", "/run-status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"running":false`) {
		t.Errorf("expected running:false, got %s", w.Body.String())
	}

	s.runMu.Lock()
	s.active = true
	s.runMu.Unlock()
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"running":true`) {
		t.Errorf("expected running:true, got %s", w.Body.String())
	}
}

func TestServer_RunStatusIncludesProgressItems(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	s := &server{db: db}

	prog := NewRunProgress()
	prog.SetQueued([]GHPR{
		{Number: 7, Title: "Some PR", URL: "https://github.com/o/r/pull/7", HeadRefOid: "s", Author: GHAuthor{Login: "a"}},
	})
	prog.MarkReviewing("https://github.com/o/r/pull/7")
	s.progress.Store(prog)
	s.runMu.Lock()
	s.active = true
	s.runMu.Unlock()

	mux := http.NewServeMux()
	s.routes(mux)
	req := httptest.NewRequest("GET", "/run-status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{`"running":true`, `"state":"reviewing"`, `"number":7`, `"title":"Some PR"`, `"started_at"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in run-status body: %s", want, body)
		}
	}
}

func TestServer_DashboardShowsLastRun(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	defer db.Close()
	seedReview(t, db) // creates a run + review (status=success)

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "Run now") {
		t.Errorf("missing Run now button")
	}
	if !strings.Contains(body, "last run") {
		t.Errorf("missing last run summary, body: %s", body[:1000])
	}
	if !strings.Contains(body, "1 reviewed") {
		t.Errorf("missing reviewed count")
	}
}

func TestServer_StaticAsset(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	req := httptest.NewRequest("GET", "/static/tailwind.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if w.Body.Len() < 10000 {
		t.Errorf("tailwind.js suspiciously small: %d bytes", w.Body.Len())
	}
}

func intToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
