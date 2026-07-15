package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

type reviewOutcome int

const (
	outcomeReviewed reviewOutcome = iota
	outcomeSkipped
	outcomeFailed
)

// Discover lists PRs from `gh pr list --search` and reviews each one that
// needs a fresh review, fanning work across cfg.Claude.Concurrency
// workers. The whole pass is wrapped in a single run row. prog may be nil
// (CLI runs); when present it receives live per-PR state for the
// dashboard.
//
// ctx cancellation (server shutdown) kills in-flight claude processes;
// DB writes go through a non-cancellable context so the run row is
// always finalized — as 'error: interrupted' when cancelled. PRs that
// were still queued at cancellation are left untouched so the next run
// picks them up.
func Discover(ctx context.Context, db *sql.DB, cfg Config, trigger string, now time.Time, prog *RunProgress) error {
	if cfg.Search == "" {
		return fmt.Errorf("config.search is empty — set it in the config file before running discover")
	}
	dbCtx := context.WithoutCancel(ctx)

	runID, err := InsertRun(dbCtx, db, trigger, now)
	if err != nil {
		return err
	}
	slog.Info("discover starting", "trigger", trigger, "search", cfg.Search)

	prs, err := ListPRs(ctx, cfg.Search, 50)
	if err != nil {
		_ = FinishRun(dbCtx, db, runID, "error", err.Error(), time.Now())
		return fmt.Errorf("list prs: %w", err)
	}
	slog.Info("discovered prs", "count", len(prs))
	if prog != nil {
		prog.SetQueued(prs)
	}

	// Resolve the gh user once per run: PRs whose latest review by this
	// user is APPROVED are not re-reviewed (the approval stands on GitHub
	// even after new pushes). Failure just disables the suppression.
	login, err := CurrentLogin(ctx)
	if err != nil {
		slog.Warn("resolve gh login; approved-PR suppression disabled this run", "err", err)
		login = ""
	}

	concurrency := cfg.Claude.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var reviewed, skipped, failed int

	for _, p := range prs {
		wg.Add(1)
		go func(p GHPR) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return // interrupted while queued; next run retries
			}
			if ctx.Err() != nil {
				return
			}

			prID, err := UpsertPR(dbCtx, db, p, now)
			if err != nil {
				slog.Error("upsert pr", "url", p.URL, "err", err)
				mu.Lock()
				failed++
				mu.Unlock()
				if prog != nil {
					prog.MarkFailed(p.URL)
				}
				return
			}

			// Already approved by the user: no fresh review, and any
			// pending cockpit reviews are dead weight — dismiss them.
			if login != "" && p.MyLatestReviewState(login) == "APPROVED" {
				if n, err := DismissAllPendingReviewsForPR(dbCtx, db, prID); err != nil {
					slog.Error("dismiss reviews for approved pr", "url", p.URL, "err", err)
				} else if n > 0 {
					slog.Info("dismissed pending reviews; pr already approved by user", "url", p.URL, "count", n)
				}
				slog.Info("skip review; already approved by user", "pr", p.URL)
				mu.Lock()
				skipped++
				mu.Unlock()
				if prog != nil {
					prog.MarkSkipped(p.URL)
				}
				return
			}

			if prog != nil {
				prog.MarkReviewing(p.URL)
			}
			outcome, findings, err := reviewIfNeeded(ctx, db, cfg, prID, p, runID, now)
			if err != nil {
				slog.Error("review", "url", p.URL, "err", err)
			}
			mu.Lock()
			switch outcome {
			case outcomeReviewed:
				reviewed++
			case outcomeSkipped:
				skipped++
			case outcomeFailed:
				failed++
			}
			mu.Unlock()
			if prog != nil {
				switch outcome {
				case outcomeReviewed:
					prog.MarkDone(p.URL, findings)
				case outcomeSkipped:
					prog.MarkSkipped(p.URL)
				case outcomeFailed:
					prog.MarkFailed(p.URL)
				}
			}
		}(p)
	}
	wg.Wait()

	if ctx.Err() == nil {
		seen := make(map[string]bool, len(prs))
		for _, p := range prs {
			seen[p.URL] = true
		}
		if n, err := ReconcilePending(ctx, db, login, seen, now); err != nil {
			slog.Warn("reconcile pending reviews", "err", err)
		} else if n > 0 {
			slog.Info("reconcile cleared non-actionable prs", "count", n)
		}
	}

	if ctx.Err() != nil {
		slog.Warn("discover interrupted", "reviewed", reviewed, "skipped", skipped, "failed", failed)
		return FinishRun(dbCtx, db, runID, "error", "interrupted by shutdown", time.Now())
	}
	status, errMsg := summarize(len(prs), reviewed, failed)
	slog.Info("discover done", "reviewed", reviewed, "skipped", skipped, "failed", failed, "status", status)
	return FinishRun(dbCtx, db, runID, status, errMsg, time.Now())
}

// ReviewOne reviews a single PR by URL, bypassing the search filter. Used
// by the --pr CLI flag and the POST /review endpoint. prog may be nil
// (CLI); when present the dashboard shows the PR's live state, same as a
// discover run. ctx cancellation follows the Discover rules: claude is
// killed, the run row is finalized via a non-cancellable context, and no
// failed review row is written.
//
// Same-SHA caching applies to manual reviews too: if the PR already has
// a pending/posted review at its current head, the existing review is
// the answer — no LLM run. A fresh review happens only when the head
// moved (and then a posted prior review feeds the follow-up context).
func ReviewOne(ctx context.Context, db *sql.DB, cfg Config, prURL string, now time.Time, prog *RunProgress) error {
	dbCtx := context.WithoutCancel(ctx)

	runID, err := InsertRun(dbCtx, db, "manual", now)
	if err != nil {
		return err
	}
	slog.Info("reviewing single pr", "url", prURL)

	pr, err := ViewPR(ctx, prURL)
	if err != nil {
		_ = FinishRun(dbCtx, db, runID, "error", err.Error(), time.Now())
		return fmt.Errorf("view pr: %w", err)
	}
	if prog != nil {
		prog.SetQueued([]GHPR{pr})
	}
	prID, err := UpsertPR(dbCtx, db, pr, now)
	if err != nil {
		_ = FinishRun(dbCtx, db, runID, "error", err.Error(), time.Now())
		return err
	}

	if prog != nil {
		prog.MarkReviewing(pr.URL)
	}
	outcome, findings, err := reviewIfNeeded(ctx, db, cfg, prID, pr, runID, now)
	if prog != nil {
		switch outcome {
		case outcomeReviewed:
			prog.MarkDone(pr.URL, findings)
		case outcomeSkipped:
			prog.MarkSkipped(pr.URL)
		case outcomeFailed:
			prog.MarkFailed(pr.URL)
		}
	}
	if ctx.Err() != nil {
		return FinishRun(dbCtx, db, runID, "error", "interrupted by shutdown", time.Now())
	}
	if err != nil {
		_ = FinishRun(dbCtx, db, runID, "error", err.Error(), time.Now())
		return err
	}

	status := "success"
	if outcome == outcomeFailed {
		status = "error"
	}
	return FinishRun(dbCtx, db, runID, status, "", time.Now())
}

// reviewIfNeeded applies the spec's decision rules for a single PR:
//   - skip if a pending/posted review already exists for this head_sha —
//     the existing review IS the review for this code; LLM runs are only
//     spent on SHAs that haven't been reviewed
//   - otherwise dismiss stale-SHA pending reviews and create a fresh one
//
// The int return is the number of findings when outcome is outcomeReviewed.
//
// ctx cancellation aborts the claude invocation; in that case no 'failed'
// review row is written, so the PR stays eligible for the next run.
func reviewIfNeeded(ctx context.Context, db *sql.DB, cfg Config, prID int64, p GHPR, runID int64, now time.Time) (reviewOutcome, int, error) {
	dbCtx := context.WithoutCancel(ctx)

	has, err := HasReviewForSHA(dbCtx, db, prID, p.HeadRefOid)
	if err != nil {
		return outcomeFailed, 0, err
	}
	if has {
		slog.Info("skip review; already reviewed this sha", "pr", p.URL, "sha", p.HeadRefOid)
		return outcomeSkipped, 0, nil
	}

	n, err := DismissPendingReviewsForPR(dbCtx, db, prID, p.HeadRefOid)
	if err != nil {
		return outcomeFailed, 0, err
	}
	if n > 0 {
		slog.Info("dismissed stale pending reviews", "pr", p.URL, "count", n)
	}

	// Re-review path: when a posted review exists (necessarily at an older
	// SHA — same-SHA was handled above), give the skill the prior findings
	// and their thread state so it classifies follow-ups instead of
	// re-litigating. Failures here degrade to a context-free review.
	previousPath := ""
	if prev, err := GetLatestPostedReview(dbCtx, db, prID); err != nil {
		slog.Warn("load posted review for context", "pr", p.URL, "err", err)
	} else if prev != nil && len(prev.Comments) > 0 {
		if path, err := buildPreviousContext(ctx, p.URL, prev); err != nil {
			slog.Warn("build re-review context", "pr", p.URL, "err", err)
		} else {
			previousPath = path
			defer os.Remove(path)
			slog.Info("re-review with prior context", "pr", p.URL, "prior_comments", len(prev.Comments))
		}
	}

	timeout := time.Duration(cfg.Claude.TimeoutSeconds) * time.Second
	sr, raw, err := RunStructuredReview(ctx, cfg.Claude.Binary, cfg.Claude.Skill, p.URL, previousPath, timeout)
	if err != nil {
		if ctx.Err() != nil {
			slog.Warn("review interrupted by shutdown", "pr", p.URL)
			return outcomeFailed, 0, ctx.Err()
		}
		slog.Error("review failed", "pr", p.URL, "err", err)
		if _, perr := PersistFailedReview(dbCtx, db, prID, runID, p.HeadRefOid, string(raw), now); perr != nil {
			slog.Error("persist failed review", "err", perr)
		}
		return outcomeFailed, 0, nil
	}
	resolveFindingLines(ctx, sr, p.URL)

	reviewID, err := PersistReview(dbCtx, db, prID, runID, sr, string(raw), now)
	if err != nil {
		return outcomeFailed, 0, err
	}
	slog.Info("review complete", "pr", p.URL, "review_id", reviewID, "findings", len(sr.Findings), "verdict", sr.Verdict)
	return outcomeReviewed, len(sr.Findings), nil
}

// resolveFindingLines snaps each finding's line into the PR's diff hunks
// before persisting — the skill's own line resolution drifts. Degrades to
// a no-op when the diff can't be fetched or parsed (submit-time snapping
// is the second line of defense).
func resolveFindingLines(ctx context.Context, sr *StructuredReview, prURL string) {
	owner, repo, number, err := ParseRepo(prURL)
	if err != nil {
		return
	}
	diff, err := FetchPRDiff(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("fetch diff for line resolution", "pr", prURL, "err", err)
		return
	}
	hunks := parseDiffHunks(diff)
	if len(hunks) == 0 {
		slog.Warn("diff parsed to zero files; skipping line resolution", "pr", prURL)
		return
	}
	for i := range sr.Findings {
		f := &sr.Findings[i]
		if f.Line < 1 {
			continue // already marked unresolvable
		}
		resolved, ok := resolveLine(hunks, f.Path, f.Line)
		if !ok {
			slog.Warn("finding targets file outside diff", "pr", prURL, "path", f.Path, "line", f.Line)
			f.Line = -1
			continue
		}
		if resolved != f.Line {
			slog.Info("snapped finding line into diff hunk", "pr", prURL, "path", f.Path, "from", f.Line, "to", resolved)
			f.Line = resolved
		}
		// Capture the diff hunk against the snapped (postable) line so the UI
		// can render it like GitHub's diff_hunk.
		f.DiffHunk = extractDiffSnippet(diff, f.Path, resolved)
	}
}

// reconcileVanishedPRs refreshes the state of PRs that have pending
// reviews but no longer appear in the search results — usually because
// they merged or closed (the filter has is:open). Without this, their
// pending reviews sit on the dashboard looking actionable with no hint
// of why scheduled runs never touch them.
// ReconcilePending re-checks the live GitHub state of PRs that still have a
// pending review and dismisses the ones no longer worth acting on: merged,
// closed, or already approved by the current user (login). Discovery searches
// is:open, so merged/closed PRs otherwise drop out of results and their
// pending reviews linger forever. `seen` holds URLs already handled inline
// this run (they're open) so they're skipped; pass nil to re-check every
// pending PR (the manual sweep). Returns the number of PRs cleared.
// Best-effort: per-PR failures are logged and skipped, not fatal.
func ReconcilePending(ctx context.Context, db *sql.DB, login string, seen map[string]bool, now time.Time) (int, error) {
	dbCtx := context.WithoutCancel(ctx)
	rows, err := db.QueryContext(dbCtx, `
		SELECT DISTINCT p.id, p.url FROM prs p
		JOIN reviews r ON r.pr_id = p.id
		WHERE r.state='pending'
	`)
	if err != nil {
		return 0, fmt.Errorf("list pending prs: %w", err)
	}
	type pendingPR struct {
		id  int64
		url string
	}
	var candidates []pendingPR
	for rows.Next() {
		var pr pendingPR
		if err := rows.Scan(&pr.id, &pr.url); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan pending pr: %w", err)
		}
		if !seen[pr.url] { // nil map reads as false — checks everything
			candidates = append(candidates, pr)
		}
	}
	rows.Close()

	cleared := 0
	for _, pr := range candidates {
		if ctx.Err() != nil {
			break
		}
		live, err := ViewPR(ctx, pr.url)
		if err != nil {
			slog.Warn("reconcile: view pr", "url", pr.url, "err", err)
			continue
		}
		// Refresh stored state either way so the dashboard badge is accurate.
		if _, err := UpsertPR(dbCtx, db, live, now); err != nil {
			slog.Warn("reconcile: upsert pr", "url", pr.url, "err", err)
		}
		done := live.State == "MERGED" || live.State == "CLOSED" ||
			(login != "" && live.MyLatestReviewState(login) == "APPROVED")
		if !done {
			continue
		}
		n, err := DismissAllPendingReviewsForPR(dbCtx, db, pr.id)
		if err != nil {
			slog.Warn("reconcile: dismiss pr", "url", pr.url, "err", err)
			continue
		}
		if n > 0 {
			cleared++
			slog.Info("reconcile: cleared non-actionable pr", "url", pr.url, "state", live.State, "dismissed", n)
		}
	}
	return cleared, nil
}

// previousContext is the JSON shape written for the skill's --previous
// input. Mirrors the contract documented in pr-review-structured SKILL.md.
type previousContext struct {
	PreviousReview struct {
		GithubReviewID int64                    `json:"github_review_id"`
		HeadSHA        string                   `json:"head_sha"`
		Findings       []previousContextFinding `json:"findings"`
	} `json:"previous_review"`
}

type previousContextFinding struct {
	Path     string        `json:"path"`
	Line     int           `json:"line"`
	Severity string        `json:"severity"`
	Body     string        `json:"body"`
	Resolved bool          `json:"resolved"`
	Outdated bool          `json:"outdated"`
	Replies  []ThreadReply `json:"replies"`
}

// buildPreviousContext fetches the PR's review threads, matches them to
// the previously posted comments (exact body match first, path+line
// fallback), and writes the context file for the skill. Caller removes
// the returned file.
func buildPreviousContext(ctx context.Context, prURL string, prev *PostedReviewInfo) (string, error) {
	owner, repo, number, err := ParseRepo(prURL)
	if err != nil {
		return "", err
	}
	threads, err := FetchReviewThreads(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}

	var pc previousContext
	pc.PreviousReview.GithubReviewID = prev.GithubReviewID
	pc.PreviousReview.HeadSHA = prev.HeadSHA
	for _, c := range prev.Comments {
		f := previousContextFinding{
			Path: c.Path, Line: c.Line, Severity: c.Severity, Body: c.Body,
			Replies: []ThreadReply{},
		}
		if t := matchThread(threads, c); t != nil {
			f.Resolved = t.IsResolved
			f.Outdated = t.IsOutdated
			f.Replies = t.Replies
		}
		pc.PreviousReview.Findings = append(pc.PreviousReview.Findings, f)
	}

	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "cockpit-previous-*.json")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// matchThread finds the GitHub thread for a posted comment. Bodies are
// posted verbatim so exact match is the primary key; (path, line) is the
// fallback for threads GitHub re-anchored.
func matchThread(threads []ReviewThread, c PostedComment) *ReviewThread {
	for i := range threads {
		if threads[i].RootBody == c.Body {
			return &threads[i]
		}
	}
	for i := range threads {
		if threads[i].Path == c.Path && threads[i].Line == c.Line {
			return &threads[i]
		}
	}
	return nil
}

func summarize(total, reviewed, failed int) (status, errMsg string) {
	switch {
	case failed == 0:
		return "success", ""
	case reviewed == 0:
		return "error", fmt.Sprintf("%d/%d failed", failed, total)
	default:
		return "partial", fmt.Sprintf("%d/%d failed", failed, total)
	}
}
