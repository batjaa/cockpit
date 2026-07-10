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

// seedSession upserts one session and (optionally) its tickets, returning the
// row id. lastActive and msgCount let callers craft a stale row.
func seedSession(t *testing.T, db *sql.DB, rec SessionRecord, tickets ...string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := UpsertSession(ctx, db, rec)
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if len(tickets) > 0 {
		if err := ReplaceSessionTickets(ctx, db, id, tickets); err != nil {
			t.Fatalf("replace tickets: %v", err)
		}
	}
	return id
}

// seedSessions populates a varied fixture: claude/local, codex/devbox-1,
// cursor/local, plus one stale claude session (idle 3 days, 20 messages).
// Returns the stale row's id.
func seedSessions(t *testing.T, db *sql.DB) (staleID int64) {
	t.Helper()
	now := time.Now()

	seedSession(t, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "c1",
		ProjectDir: "/Users/foo/git/backend", Title: "Fix flaky retry test",
		ResumeCmd:  "cd /Users/foo/git/backend && claude --resume c1",
		LastActive: now.Add(-2 * time.Hour), MessageCount: 47,
	}, "PLAT-422")

	seedSession(t, db, SessionRecord{
		Agent: "codex", Machine: "devbox-1", SessionKey: "x1",
		ProjectDir: "/home/foo/git/infra", Title: "Wire up Terraform module",
		ResumeCmd:  "codex resume x1",
		LastActive: now.Add(-5 * time.Hour), MessageCount: 12,
	}, "org/repo#123")

	seedSession(t, db, SessionRecord{
		Agent: "cursor", Machine: "local", SessionKey: "u1",
		ProjectDir: "/Users/foo/git/frontend", Title: "Polish settings page",
		ResumeCmd:  "", // cursor has no CLI resume
		LastActive: now.Add(-1 * time.Hour), MessageCount: 8,
	})

	// Stale: idle 3 days, 20 messages -> matches the heuristic.
	staleID = seedSession(t, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "stale1",
		ProjectDir: "/Users/foo/git/auth", Title: "Abandoned refactor mid-flight",
		ResumeCmd:  "cd /Users/foo/git/auth && claude --resume stale1",
		LastActive: now.Add(-3 * 24 * time.Hour), MessageCount: 20,
	}, "AUTH-99")

	return staleID
}

func newSessionsServer(t *testing.T) (*server, *http.ServeMux, *sql.DB) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &server{db: db, baseCtx: context.Background()}
	mux := http.NewServeMux()
	s.routes(mux)
	return s, mux, db
}

func getBody(t *testing.T, mux *http.ServeMux, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestSessions_PageRenders(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	seedSessions(t, db)

	code, body := getBody(t, mux, "/sessions")
	if code != 200 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	for _, want := range []string{
		"Sessions",
		"Fix flaky retry test",     // title
		"Wire up Terraform module", // title
		"Polish settings page",     // title
		"PLAT-422",                 // ticket chip
		"org/repo#123",             // ticket chip
		"devbox-1",                 // machine chip (non-local shown)
		"copy resume",              // resume button for claude
		"open in Cursor",           // cursor label instead of resume
		"codex resume x1",          // codex resume cmd is copyable
		`href="?ticket=PLAT-422"`,  // ticket chips link to filtered view
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in sessions body", want)
		}
	}
	// local machine chip must be hidden.
	if strings.Contains(body, `bg-zinc-100 text-zinc-600">local<`) {
		t.Error("local machine chip should be hidden")
	}
}

func TestSessions_EmptyState(t *testing.T) {
	_, mux, _ := newSessionsServer(t)
	code, body := getBody(t, mux, "/sessions")
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, "Scan now") {
		t.Error("missing Scan now button")
	}
	if !strings.Contains(body, "No sessions") {
		t.Errorf("missing empty-state copy:\n%s", body)
	}
}

func TestSessions_StaleSection(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	seedSessions(t, db)

	code, body := getBody(t, mux, "/sessions")
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	for _, want := range []string{
		"Went quiet mid-work",           // stale section header
		"Abandoned refactor mid-flight", // the stale session title
		"idle 3d",                       // idle badge
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q (stale section)", want)
		}
	}
}

func TestSessions_FilterByAgent(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	seedSessions(t, db)

	code, body := getBody(t, mux, "/sessions?agent=codex")
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, "Wire up Terraform module") {
		t.Error("codex session missing under agent=codex")
	}
	if strings.Contains(body, "Fix flaky retry test") {
		t.Error("claude session should be filtered out under agent=codex")
	}
}

func TestSessions_FilterByTicket(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	seedSessions(t, db)

	code, body := getBody(t, mux, "/sessions?ticket=PLAT-422")
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, "Fix flaky retry test") {
		t.Error("PLAT-422 session missing under ticket filter")
	}
	if strings.Contains(body, "Wire up Terraform module") {
		t.Error("other session should be filtered out under ticket=PLAT-422")
	}
}

func TestSessions_FilterBySearch(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	seedSessions(t, db)

	code, body := getBody(t, mux, "/sessions?q=Terraform")
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if !strings.Contains(body, "Wire up Terraform module") {
		t.Error("search q=Terraform should match the codex session")
	}
	if strings.Contains(body, "Fix flaky retry test") {
		t.Error("non-matching session should be filtered out")
	}
}

func TestSessions_ArchiveFlow(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	id := seedSession(t, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "a1",
		Title: "Archive me", LastActive: time.Now().Add(-time.Hour), MessageCount: 3,
	})

	// Present in the default view.
	_, body := getBody(t, mux, "/sessions")
	if !strings.Contains(body, "Archive me") {
		t.Fatal("session missing before archive")
	}

	// Archive via PATCH.
	req := httptest.NewRequest("PATCH", "/sessions/"+intToStr(id), strings.NewReader(`{"archived": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("archive PATCH status=%d body=%s", w.Code, w.Body.String())
	}

	// Gone from the default view...
	_, body = getBody(t, mux, "/sessions")
	if strings.Contains(body, "Archive me") {
		t.Error("archived session still shows in default view")
	}
	// ...but visible with archived=1, now with an Unarchive button.
	_, body = getBody(t, mux, "/sessions?archived=1")
	if !strings.Contains(body, "Archive me") {
		t.Error("archived session missing from archived view")
	}
	if !strings.Contains(body, "Unarchive") {
		t.Error("archived view should offer Unarchive")
	}

	// Unarchive flips it back.
	req = httptest.NewRequest("PATCH", "/sessions/"+intToStr(id), strings.NewReader(`{"archived": false}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unarchive PATCH status=%d", w.Code)
	}
	_, body = getBody(t, mux, "/sessions")
	if !strings.Contains(body, "Archive me") {
		t.Error("session should reappear in default view after unarchive")
	}
}

func TestSessions_ArchiveBadID(t *testing.T) {
	_, mux, _ := newSessionsServer(t)
	req := httptest.NewRequest("PATCH", "/sessions/notanumber", strings.NewReader(`{"archived": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad id: status=%d want 400", w.Code)
	}
}

func TestSessions_ArchiveMissing(t *testing.T) {
	_, mux, _ := newSessionsServer(t)
	req := httptest.NewRequest("PATCH", "/sessions/9999", strings.NewReader(`{"archived": true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("missing id: status=%d want 404", w.Code)
	}
}

func TestSessions_ScanUnwired(t *testing.T) {
	_, mux, _ := newSessionsServer(t)
	// Ensure the package var is nil for this test.
	saved := scanSessionsFn
	scanSessionsFn = nil
	t.Cleanup(func() { scanSessionsFn = saved })

	req := httptest.NewRequest("POST", "/sessions/scan", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired scan: status=%d want 503 body=%s", w.Code, w.Body.String())
	}
}

func TestSessions_ScanInFlight(t *testing.T) {
	_, mux, _ := newSessionsServer(t)

	// A fake scanner that blocks until released, so we can observe the
	// 409-while-scanning and the status endpoint mid-flight.
	release := make(chan struct{})
	started := make(chan struct{})
	saved := scanSessionsFn
	scanSessionsFn = func(ctx context.Context, db *sql.DB, cfg Config) error {
		close(started)
		<-release
		return nil
	}
	t.Cleanup(func() {
		scanSessionsFn = saved
		// Make sure the goroutine isn't left blocked.
		select {
		case <-release:
		default:
			close(release)
		}
		scanWG.Wait()
		sessionScanning.Store(false)
	})

	// First scan: 202.
	req := httptest.NewRequest("POST", "/sessions/scan", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first scan: status=%d want 202 body=%s", w.Code, w.Body.String())
	}

	// Wait for the fake to actually start before probing in-flight state.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fake scanner never started")
	}

	// Status endpoint reflects scanning.
	code, body := getBody(t, mux, "/sessions/status")
	if code != 200 {
		t.Fatalf("status endpoint: %d", code)
	}
	if !strings.Contains(body, `"scanning":true`) {
		t.Errorf("status should report scanning:true, got %s", body)
	}

	// Second scan while in-flight: 409.
	req = httptest.NewRequest("POST", "/sessions/scan", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("concurrent scan: status=%d want 409", w.Code)
	}

	// Release and let it drain.
	close(release)
	scanWG.Wait()

	// Status back to idle.
	code, body = getBody(t, mux, "/sessions/status")
	if code != 200 {
		t.Fatalf("status endpoint after scan: %d", code)
	}
	if !strings.Contains(body, `"scanning":false`) {
		t.Errorf("status should report scanning:false after drain, got %s", body)
	}
}

func TestDashboard_StaleSessionsLink(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedSessions(t, db) // includes one stale session

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	code, body := getBody(t, mux, "/")
	if code != 200 {
		t.Fatalf("dashboard status=%d", code)
	}
	if !strings.Contains(body, "stale session") {
		t.Errorf("dashboard missing stale sessions link")
	}
	if !strings.Contains(body, `href="/sessions"`) {
		t.Errorf("stale link should point to /sessions")
	}
}

func TestDashboard_NoStaleSessionsNoLink(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// One fresh, non-stale session.
	seedSession(t, db, SessionRecord{
		Agent: "claude", Machine: "local", SessionKey: "fresh",
		Title: "Fresh", LastActive: time.Now(), MessageCount: 30,
	})

	s := &server{db: db}
	mux := http.NewServeMux()
	s.routes(mux)

	_, body := getBody(t, mux, "/")
	if strings.Contains(body, "stale session") {
		t.Error("dashboard should not show stale link when none are stale")
	}
}

// TestSessions_BucketsTrivialAndBranch covers the UX rework: recency
// bucket headers, trivial sessions hidden behind the toggle, branch/repo
// tag filters, and the fuzzy-filter haystack attribute.
func TestSessions_BucketsTrivialAndBranch(t *testing.T) {
	_, mux, db := newSessionsServer(t)
	ctx := context.Background()
	now := time.Now()

	seed := func(key, title, dir, branch string, msgs int, active time.Time) int64 {
		id, err := UpsertSession(ctx, db, SessionRecord{
			Agent: "claude", Machine: "local", SessionKey: key,
			ProjectDir: dir, Title: title, Branch: branch,
			LastActive: active, MessageCount: msgs,
			ResumeCmd: "claude --resume " + key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	seed("s-today", "Fix retry logic", "/Users/x/git/backend", "batjaa/fix-retry", 25, now.Add(-2*time.Hour))
	seed("s-week", "Refactor exporter", "/Users/x/git/cockpit", "main", 12, now.Add(-3*24*time.Hour))
	seed("s-trivial", "quick scratch question", "/Users/x/git/scratch", "", 1, now.Add(-1*time.Hour))

	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
		return w.Body.String()
	}

	body := get("/sessions")
	for _, want := range []string{
		"Today", "This week", // bucket headers
		"⎇ batjaa/fix-retry", // branch chip
		`href="?repo=backend"`, `href="?branch=main"`,
		"1 quick session hidden", // trivial toggle
		`data-hay="`,             // fuzzy haystack
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "quick scratch question") {
		t.Error("trivial session shown by default")
	}

	// all=1 reveals trivial sessions.
	if body := get("/sessions?all=1"); !strings.Contains(body, "quick scratch question") {
		t.Error("all=1 should show trivial sessions")
	}

	// Branch and repo filters narrow the set.
	if body := get("/sessions?branch=main"); strings.Contains(body, "Fix retry logic") || !strings.Contains(body, "Refactor exporter") {
		t.Error("branch filter wrong")
	}
	if body := get("/sessions?repo=backend"); !strings.Contains(body, "Fix retry logic") || strings.Contains(body, "Refactor exporter") {
		t.Error("repo filter wrong")
	}
}
