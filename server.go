package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

var (
	dashboardTmpl = template.Must(template.ParseFS(tmplFS, "templates/layout.tmpl", "templates/dashboard.tmpl"))
	detailTmpl    = template.Must(template.ParseFS(tmplFS, "templates/layout.tmpl", "templates/pr_detail.tmpl"))
)

type server struct {
	db      *sql.DB
	cfg     Config
	baseCtx context.Context // cancelled on shutdown; runs inherit it

	// Run worker: a single goroutine drains the queue serially so heavy
	// claude reviews never overlap. runMu guards queue/active/current.
	// progress holds the live state of the job currently executing; runWG
	// tracks the worker for graceful shutdown.
	runMu    sync.Mutex
	queue    []runJob
	active   bool                        // worker goroutine is draining the queue
	current  *runJob                     // job currently executing (nil when idle)
	progress atomic.Pointer[RunProgress] // per-PR state for the active (or last) run
	runWG    sync.WaitGroup
}

// Serve starts the HTTP server. Blocks until ctx is cancelled, then
// gracefully shuts down: in-flight claude reviews are cancelled via
// baseCtx and the run goroutine is given a grace period to finalize its
// run row before the process exits.
func Serve(ctx context.Context, db *sql.DB, cfg Config) error {
	s := &server{db: db, cfg: cfg, baseCtx: ctx}
	scanSessionsFn = ScanSessions // wire the sessions scanner (see sessions_server.go)
	mux := http.NewServeMux()
	s.routes(mux)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind before touching shared state: the port acts as a single-
	// instance lock. A second cockpit (or a failed start while another
	// server is mid-run) must not reach the stale-run cleanup below, or
	// it would mark the other process's active run as interrupted.
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTP.Addr, err)
	}
	if n, err := CleanupStaleRuns(ctx, db, time.Now()); err != nil {
		slog.Warn("cleanup stale runs", "err", err)
	} else if n > 0 {
		slog.Info("marked stale runs as interrupted", "count", n)
	}

	go s.runScheduler()
	go s.runSessionTicker()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("serving", "addr", "http://"+cfg.HTTP.Addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutCtx)

		// ctx cancellation has already killed any in-flight claude
		// process; give the run goroutine a moment to write its final
		// 'interrupted' run row. Startup cleanup is the backstop if
		// this window is missed.
		done := make(chan struct{})
		go func() { s.runWG.Wait(); scanWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			slog.Warn("run goroutine did not finish within shutdown grace period")
		}
		return err
	case err := <-errCh:
		return err
	}
}

func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /pr/{id}", s.handlePRDetail)
	mux.HandleFunc("POST /pr/{id}/submit", s.handleSubmit)
	mux.HandleFunc("POST /pr/{id}/dismiss", s.handleDismiss)
	mux.HandleFunc("PATCH /comments/{id}", s.handleCommentToggle)
	mux.HandleFunc("PATCH /reviews/{id}", s.handleSummaryEdit)
	mux.HandleFunc("POST /run-now", s.handleRunNow)
	mux.HandleFunc("POST /review", s.handleManualReview)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	mux.HandleFunc("GET /run-status", s.handleRunStatus)

	// Sessions UI (see sessions_server.go).
	mux.HandleFunc("GET /sessions", s.handleSessions)
	mux.HandleFunc("POST /sessions/scan", s.handleSessionScan)
	mux.HandleFunc("GET /sessions/status", s.handleSessionScanStatus)
	mux.HandleFunc("PATCH /sessions/{id}", s.handleSessionArchive)

	// Strip "static/" off the embedded FS so URLs are /static/foo.js
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	reviews, err := ListPendingReviews(r.Context(), s.db)
	if err != nil {
		s.serverError(w, "list pending reviews", err)
		return
	}

	pendingComments, err := ListPendingComments(r.Context(), s.db)
	if err != nil {
		s.serverError(w, "list pending comments", err)
		return
	}
	stats, err := GetStats(r.Context(), s.db)
	if err != nil {
		s.serverError(w, "stats", err)
		return
	}

	// Decorate with human-readable age and severity-grouped comment
	// excerpts — done in Go rather than template to keep the template
	// clean. Each group renders as a badge whose hover popover lists only
	// that severity's findings.
	type commentRow struct {
		CommentDetail
		PathShort string // last two path segments; full path in title attr
		Excerpt   string
	}
	type sevGroup struct {
		Key, Icon, Label string
		Comments         []commentRow
	}
	type row struct {
		DashboardReview
		Age        string // review age — fallback when PR times are absent
		OpenedAge  string // PR opened, humanized ("3d")
		UpdatedAge string // PR last activity, humanized ("2h")
		Groups     []sevGroup
	}
	sevOrder := []sevGroup{
		{Key: "blocker", Icon: "🔴", Label: "Blockers"},
		{Key: "major", Icon: "🟠", Label: "Major"},
		{Key: "minor", Icon: "🟡", Label: "Minor"},
		{Key: "nit", Icon: "nit", Label: "Nit"},
	}
	rows := make([]row, len(reviews))
	for i, rv := range reviews {
		rows[i] = row{DashboardReview: rv, Age: humanizeAge(time.Since(rv.CreatedAt))}
		if rv.PRCreatedAt.Valid {
			rows[i].OpenedAge = humanizeAge(time.Since(rv.PRCreatedAt.Time))
		}
		if rv.PRUpdatedAt.Valid {
			rows[i].UpdatedAge = humanizeAge(time.Since(rv.PRUpdatedAt.Time))
		}
		for _, g := range sevOrder {
			group := sevGroup{Key: g.Key, Icon: g.Icon, Label: g.Label}
			for _, c := range pendingComments[rv.ReviewID] {
				if c.Severity == g.Key {
					group.Comments = append(group.Comments, commentRow{
						CommentDetail: c,
						PathShort:     shortPath(c.Path),
						Excerpt:       excerpt(c.Body, 300),
					})
				}
			}
			if len(group.Comments) > 0 {
				rows[i].Groups = append(rows[i].Groups, group)
			}
		}
	}

	type lastRunView struct {
		Status      string
		Age         string
		Trigger     string
		ReviewCount int
		Error       string
	}
	var lastRun *lastRunView
	if latest, err := LatestRun(r.Context(), s.db); err == nil && latest != nil {
		// While a run is running, finished_at is null; show "running" instead of age.
		age := "running"
		if latest.FinishedAt.Valid {
			age = humanizeAge(time.Since(latest.FinishedAt.Time))
		}
		errStr := ""
		if latest.Error.Valid {
			errStr = latest.Error.String
		}
		lastRun = &lastRunView{
			Status:      latest.Status,
			Age:         age,
			Trigger:     latest.Trigger,
			ReviewCount: latest.ReviewCount,
			Error:       errStr,
		}
	}

	nextRun := ""
	if schedulerEnabled(s.cfg.Schedule) {
		nextRun = NextRunTime(time.Now(), s.cfg.Schedule).Format("Mon 15:04")
	}

	// Surface a link to stale agent sessions in the header meta line. A
	// query failure here shouldn't blank the dashboard — log and show none.
	staleSessions := 0
	if n, err := CountStaleSessions(r.Context(), s.db); err != nil {
		slog.Warn("count stale sessions", "err", err)
	} else {
		staleSessions = n
	}

	s.render(w, dashboardTmpl, map[string]any{
		"Title":         "Pending reviews",
		"Reviews":       rows,
		"LastRun":       lastRun,
		"Running":       s.isRunning(),
		"SearchEmpty":   s.cfg.Search == "",
		"Stats":         stats,
		"NextRun":       nextRun,
		"StaleSessions": staleSessions,
	})
}

// excerpt flattens a comment body for the dashboard's compact rows:
// newlines and runs of whitespace collapse to single spaces (so the
// context paragraphs show, not just the label line), bold markers are
// stripped, and the result is truncated.
func excerpt(body string, max int) string {
	flat := strings.Join(strings.Fields(body), " ")
	flat = strings.ReplaceAll(flat, "**", "")
	if len(flat) > max {
		return flat[:max] + "…"
	}
	return flat
}

// shortPath keeps the last two path segments — enough to disambiguate
// the many index.tsx files — with the full path shown via title attr.
func shortPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// tryStartRun enqueues a discovery run if the search is configured. Shared by
// the Run now button and the scheduler. Discover jobs coalesce, so a slot that
// fires while a discovery is already active or pending is a no-op — reported
// as false so the caller logs it as skipped. Returns true when the run was
// started or queued (e.g. behind an in-flight manual review).
func (s *server) tryStartRun(trigger string) bool {
	if s.cfg.Search == "" {
		return false
	}
	status, _ := s.enqueue(runJob{kind: runDiscover, trigger: trigger})
	return status == enqStarted || status == enqQueued
}

func (s *server) handleRunNow(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Search == "" {
		http.Error(w, "config.search is empty; set it in ~/.cockpit/config.json", http.StatusPreconditionFailed)
		return
	}
	status, ahead := s.enqueue(runJob{kind: runDiscover, trigger: "manual"})
	s.writeRunAccepted(w, status, ahead, "")
}

// handleManualReview reviews a single PR by URL, independent of the
// configured search filter (works even when config.search is empty). The job
// is enqueued on the shared run worker, so if a discovery or another review is
// in flight it waits its turn rather than being rejected.
func (s *server) handleManualReview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Normalize: drop query/fragment and anything after the PR number
	// (e.g. /files), then validate the shape before spending a run slot.
	url := strings.TrimSpace(body.URL)
	url, _, _ = strings.Cut(url, "?")
	url, _, _ = strings.Cut(url, "#")
	owner, repo, number, err := ParseRepo(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	url = fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)

	status, ahead := s.enqueue(runJob{kind: runReview, url: url})
	s.writeRunAccepted(w, status, ahead, url)
}

// handleReconcile re-checks every pending PR's live GitHub state and dismisses
// the ones that are merged, closed, or already approved by the user. Runs
// synchronously — it's a handful of gh calls, no LLM — and returns the count
// cleared. The scheduled/manual discovery runs do this automatically too; this
// is the on-demand button.
func (s *server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	login, err := CurrentLogin(r.Context())
	if err != nil {
		slog.Warn("reconcile: resolve gh login; approved-PR clearing disabled", "err", err)
		login = ""
	}
	cleared, err := ReconcilePending(r.Context(), s.db, login, nil, time.Now())
	if err != nil {
		s.serverError(w, "reconcile", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Cleared int `json:"cleared"`
	}{Cleared: cleared})
}

func (s *server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	active, queued := s.runSnapshot()
	resp := struct {
		Running bool         `json:"running"`
		Items   []PRProgress `json:"items"`
	}{Running: active}
	if prog := s.progress.Load(); prog != nil {
		resp.Items = prog.Snapshot()
	}
	// Show pending jobs (queued reviews, a queued discovery) after the
	// active run's items so the user sees their click landed in line.
	resp.Items = append(resp.Items, queued...)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type commentView struct {
	CommentDetail
	BodyHTML template.HTML
}

type severityBlock struct {
	Key, Icon, Label string
	Comments         []commentView
}

func (s *server) handlePRDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	d, err := GetReviewDetail(r.Context(), s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "review not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, "get review detail", err)
		return
	}

	severities := []severityBlock{
		{Key: "blocker", Icon: "🔴", Label: "Blockers"},
		{Key: "major", Icon: "🟠", Label: "Major"},
		{Key: "minor", Icon: "🟡", Label: "Minor"},
		{Key: "nit", Icon: "·", Label: "Nit"},
	}
	for _, c := range d.Comments {
		for i := range severities {
			if severities[i].Key == c.Severity {
				severities[i].Comments = append(severities[i].Comments, commentView{
					CommentDetail: c,
					BodyHTML:      RenderMarkdown(c.Body),
				})
				break
			}
		}
	}

	headShort := d.PR.HeadSHA
	if len(headShort) > 7 {
		headShort = headShort[:7]
	}

	s.render(w, detailTmpl, map[string]any{
		"Title":          fmt.Sprintf("%s/%s#%d", d.PR.Owner, d.PR.Repo, d.PR.Number),
		"ReviewID":       d.ReviewID,
		"State":          d.State,
		"PR":             d.PR,
		"HeadShort":      headShort,
		"Summary":        d.Summary,
		"Severities":     severities,
		"HasAnyComments": len(d.Comments) > 0,
		"Followups":      d.Followups,
	})
}

var validEvents = map[string]bool{"APPROVE": true, "REQUEST_CHANGES": true, "COMMENT": true}

// handleSubmit posts the review to GitHub: one API call containing the
// summary and every selected comment.
//
// Refusal cases, in check order:
//   - 400 unknown event / bad body
//   - 409 review not in 'pending' state (already posted or dismissed)
//   - 400 COMMENT event with zero selected comments (empty review)
//   - 409 PR head moved since the review was generated (stale line anchors)
func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Event string `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validEvents[body.Event] {
		http.Error(w, "event must be APPROVE, REQUEST_CHANGES, or COMMENT", http.StatusBadRequest)
		return
	}

	d, err := GetReviewDetail(r.Context(), s.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "review not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, "get review detail", err)
		return
	}
	if d.State != "pending" {
		http.Error(w, "review is "+d.State+", not pending", http.StatusConflict)
		return
	}

	var comments []ReviewPayloadComment
	skippedUnresolved := 0
	for _, c := range d.Comments {
		if !c.Selected {
			continue
		}
		if c.Line < 1 {
			// Line resolution failed at review time; GitHub would 422.
			skippedUnresolved++
			continue
		}
		comments = append(comments, ReviewPayloadComment{
			Path: c.Path, Line: c.Line, Side: "RIGHT", Body: c.Body,
		})
	}
	if body.Event == "COMMENT" && len(comments) == 0 {
		http.Error(w, "no comments selected; refusing to post an empty review", http.StatusBadRequest)
		return
	}

	// Stale check: if the PR head moved since the review was generated,
	// line anchors may be invalid. Refuse — re-review instead.
	current, err := ViewPR(r.Context(), d.PR.URL)
	if err != nil {
		s.serverError(w, "stale check", err)
		return
	}
	if current.State != "" && current.State != "OPEN" {
		http.Error(w,
			fmt.Sprintf("PR is %s — reviews can no longer be posted from cockpit; dismiss this review", current.State),
			http.StatusConflict)
		return
	}
	if current.HeadRefOid != d.HeadSHA {
		http.Error(w,
			fmt.Sprintf("PR head moved (%.7s -> %.7s) since this review; re-review before posting",
				d.HeadSHA, current.HeadRefOid),
			http.StatusConflict)
		return
	}

	// Resolve selected lines against the live diff. The skill's line
	// resolution drifts (e.g. line 295 in a 154-line file), and GitHub's
	// 422 for an out-of-hunk line is uselessly vague — snap to the nearest
	// in-hunk line instead, refusing only when the file isn't in the diff
	// at all.
	snapped := 0
	if len(comments) > 0 {
		diff, err := FetchPRDiff(r.Context(), d.PR.Owner, d.PR.Repo, d.PR.Number)
		if err != nil {
			s.serverError(w, "fetch diff for line validation", err)
			return
		}
		hunks := parseDiffHunks(diff)
		var unanchorable []string
		for i := range comments {
			resolved, ok := resolveLine(hunks, comments[i].Path, comments[i].Line)
			if !ok {
				unanchorable = append(unanchorable, fmt.Sprintf("%s:%d", comments[i].Path, comments[i].Line))
				continue
			}
			if resolved != comments[i].Line {
				slog.Info("snapped comment line into diff hunk",
					"review_id", id, "path", comments[i].Path, "from", comments[i].Line, "to", resolved)
				comments[i].Line = resolved
				snapped++
			}
		}
		if len(unanchorable) > 0 {
			http.Error(w,
				"selected comment(s) target files outside the PR diff — GitHub cannot anchor them: "+
					strings.Join(unanchorable, ", ")+". Deselect them or re-review.",
				http.StatusBadRequest)
			return
		}
	}

	posted, err := PostReview(r.Context(), d.PR.Owner, d.PR.Repo, d.PR.Number, ReviewPayload{
		CommitID: d.HeadSHA,
		Event:    body.Event,
		Body:     d.Summary,
		Comments: comments,
	})
	if err != nil {
		s.serverError(w, "post review", err)
		return
	}
	if err := MarkReviewPosted(r.Context(), s.db, id, posted.ID, time.Now()); err != nil {
		// The review IS on GitHub; surface loudly rather than retrying.
		s.serverError(w, "mark posted (review WAS submitted to github)", err)
		return
	}

	slog.Info("review posted", "review_id", id, "github_id", posted.ID, "comments", len(comments), "event", body.Event)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"github_url":         posted.HTMLURL,
		"posted_comments":    len(comments),
		"skipped_unresolved": skippedUnresolved,
		"snapped_lines":      snapped,
	})
}

func (s *server) handleDismiss(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := MarkReviewDismissed(r.Context(), s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "review not found or not pending", http.StatusConflict)
			return
		}
		s.serverError(w, "dismiss review", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSummaryEdit saves an edited review summary. Only pending reviews
// are editable — the summary is what posts as the review body.
func (s *server) handleSummaryEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := UpdateReviewSummary(r.Context(), s.db, id, body.Summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "review not found or not pending", http.StatusConflict)
			return
		}
		s.serverError(w, "update summary", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleCommentToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var body struct {
		Selected bool `json:"selected"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := SetCommentSelected(r.Context(), s.db, id, body.Selected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "comment not found", http.StatusNotFound)
			return
		}
		s.serverError(w, "set comment selected", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// These pages carry their behavior in inline <script>. Never let the
	// browser replay a cached copy, or JS fixes silently won't reach the user.
	w.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("render", "err", err)
	}
}

func (s *server) serverError(w http.ResponseWriter, stage string, err error) {
	slog.Error(stage, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// humanizeAge converts a duration to a short relative string ("3m", "2h", "5d").
func humanizeAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
