package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UpsertPR inserts or updates a PR row keyed on (owner, repo, number).
// first_seen is preserved across updates; last_seen, title, url, author,
// head_sha are refreshed.
func UpsertPR(ctx context.Context, db *sql.DB, p GHPR, now time.Time) (int64, error) {
	owner, repo, number, err := ParseRepo(p.URL)
	if err != nil {
		return 0, err
	}
	state := p.State
	if state == "" {
		state = "OPEN"
	}
	// GitHub PR times are optional per fetch path — store NULL when absent so
	// COALESCE below never overwrites a previously captured value with zero.
	var prCreated, prUpdated any
	if !p.CreatedAt.IsZero() {
		prCreated = dbTime(p.CreatedAt)
	}
	if !p.UpdatedAt.IsZero() {
		prUpdated = dbTime(p.UpdatedAt)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO prs (owner, repo, number, url, title, author, head_sha, state, pr_created_at, pr_updated_at, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, repo, number) DO UPDATE SET
			title         = excluded.title,
			url           = excluded.url,
			author        = excluded.author,
			head_sha      = excluded.head_sha,
			state         = excluded.state,
			pr_created_at = COALESCE(excluded.pr_created_at, prs.pr_created_at),
			pr_updated_at = COALESCE(excluded.pr_updated_at, prs.pr_updated_at),
			last_seen     = excluded.last_seen
	`, owner, repo, number, p.URL, p.Title, p.Author.Login, p.HeadRefOid, state, prCreated, prUpdated, dbTime(now), dbTime(now))
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM prs WHERE owner=? AND repo=? AND number=?`,
		owner, repo, number).Scan(&id)
	return id, err
}

// InsertRun creates a runs row with status='running' and returns its id.
func InsertRun(ctx context.Context, db *sql.DB, trigger string, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO runs (trigger, started_at, status) VALUES (?, ?, 'running')`,
		trigger, dbTime(now))
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	return res.LastInsertId()
}

// FinishRun marks a run row with a final status and optional error message.
func FinishRun(ctx context.Context, db *sql.DB, runID int64, status string, errMsg string, now time.Time) error {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := db.ExecContext(ctx,
		`UPDATE runs SET status=?, finished_at=?, error=? WHERE id=?`,
		status, dbTime(now), errVal, runID)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// PostedComment is a previously posted comment, used to build re-review
// context.
type PostedComment struct {
	Severity string
	Path     string
	Line     int
	Body     string
}

// PostedReviewInfo is the latest posted review for a PR plus its posted
// comments.
type PostedReviewInfo struct {
	ReviewID       int64
	GithubReviewID int64
	HeadSHA        string
	Comments       []PostedComment
}

// GetLatestPostedReview returns the most recent posted review for a PR,
// or nil if none exists.
func GetLatestPostedReview(ctx context.Context, db *sql.DB, prID int64) (*PostedReviewInfo, error) {
	var info PostedReviewInfo
	var ghID sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT id, github_review_id, head_sha FROM reviews
		WHERE pr_id=? AND state='posted'
		ORDER BY posted_at DESC LIMIT 1
	`, prID).Scan(&info.ReviewID, &ghID, &info.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest posted review: %w", err)
	}
	info.GithubReviewID = ghID.Int64

	rows, err := db.QueryContext(ctx, `
		SELECT severity, path, line, body FROM comments
		WHERE review_id=? AND posted=1
		ORDER BY id
	`, info.ReviewID)
	if err != nil {
		return nil, fmt.Errorf("posted comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c PostedComment
		if err := rows.Scan(&c.Severity, &c.Path, &c.Line, &c.Body); err != nil {
			return nil, fmt.Errorf("scan posted comment: %w", err)
		}
		info.Comments = append(info.Comments, c)
	}
	return &info, rows.Err()
}

// CleanupStaleRuns marks any 'running' runs as errored. Called once at
// startup: a run can only be 'running' across a restart if the previous
// process died mid-run, and nothing will ever finish it.
func CleanupStaleRuns(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE runs SET status='error', error='interrupted by restart', finished_at=?
		WHERE status='running'
	`, dbTime(now))
	if err != nil {
		return 0, fmt.Errorf("cleanup stale runs: %w", err)
	}
	return res.RowsAffected()
}

// PersistReview writes a reviews row plus all of its comments rows in a
// single transaction. State is set to 'pending' — selection and posting
// happens later via the web UI. Returns the new review id.
func PersistReview(ctx context.Context, db *sql.DB, prID, runID int64, sr *StructuredReview, raw string, now time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO reviews (pr_id, run_id, head_sha, summary, raw_output, state, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)
	`, prID, runID, sr.PR.HeadSHA, sr.Summary, raw, dbTime(now))
	if err != nil {
		return 0, fmt.Errorf("insert review: %w", err)
	}
	reviewID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("review id: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO comments (review_id, severity, path, line, body, diff_hunk)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("prepare comment insert: %w", err)
	}
	defer stmt.Close()

	for _, f := range sr.Findings {
		if _, err := stmt.ExecContext(ctx, reviewID, f.Severity, f.Path, f.Line, f.Body, f.DiffHunk); err != nil {
			return 0, fmt.Errorf("insert comment: %w", err)
		}
	}
	for _, fu := range sr.Followups {
		var findingID any
		if fu.FindingID != "" {
			findingID = fu.FindingID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO followups (review_id, path, line, status, note, finding_id)
			VALUES (?, ?, ?, ?, ?, ?)
		`, reviewID, fu.Path, fu.Line, fu.Status, fu.Note, findingID); err != nil {
			return 0, fmt.Errorf("insert followup: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return reviewID, nil
}

// HasReviewForSHA reports whether a review already exists for (prID, headSHA)
// that should stop another LLM run: pending (awaiting the user), posted, or
// dismissed (the user rejected it — re-reviewing the same unchanged code would
// just resurface what they threw away). 'failed' reviews are excluded so a
// transient error is retried on the next run.
func HasReviewForSHA(ctx context.Context, db *sql.DB, prID int64, headSHA string) (bool, error) {
	var cnt int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reviews
		WHERE pr_id=? AND head_sha=? AND state IN ('pending','posted','dismissed')
	`, prID, headSHA).Scan(&cnt)
	return cnt > 0, err
}

// DismissPendingReviewsForPR marks any pending reviews for prID whose
// head_sha differs from currentSHA as 'dismissed'. Returns the number of
// rows affected. Called when a PR has been force-pushed — old line
// anchors are invalid.
func DismissPendingReviewsForPR(ctx context.Context, db *sql.DB, prID int64, currentSHA string) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE reviews SET state='dismissed'
		WHERE pr_id=? AND state='pending' AND head_sha != ?
	`, prID, currentSHA)
	if err != nil {
		return 0, fmt.Errorf("dismiss pending reviews: %w", err)
	}
	return res.RowsAffected()
}

// DismissAllPendingReviewsForPR dismisses every pending review for prID
// regardless of SHA. Used on forced manual reviews, where a fresh review
// replaces whatever is pending.
func DismissAllPendingReviewsForPR(ctx context.Context, db *sql.DB, prID int64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE reviews SET state='dismissed' WHERE pr_id=? AND state='pending'`, prID)
	if err != nil {
		return 0, fmt.Errorf("dismiss all pending reviews: %w", err)
	}
	return res.RowsAffected()
}

// DashboardReview is the joined row shape for the dashboard listing.
type DashboardReview struct {
	ReviewID    int64
	PRID        int64
	Owner       string
	Repo        string
	Number      int
	Title       string
	Author      string
	URL         string
	HeadSHA     string
	PRState     string // OPEN | MERGED | CLOSED
	Summary     string
	CreatedAt   time.Time    // review row created (when cockpit reviewed it)
	PRCreatedAt sql.NullTime // PR opened time (GitHub); null for pre-migration rows
	PRUpdatedAt sql.NullTime // PR last-activity time (GitHub)
	Blockers    int
	Majors      int
	Minors      int
	Nits        int
	Selected    int // comments currently selected for posting
}

// ListPendingReviews returns every review currently in 'pending' state
// joined with its PR row and severity counts. Sorted newest-first.
func ListPendingReviews(ctx context.Context, db *sql.DB) ([]DashboardReview, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			r.id, p.id, p.owner, p.repo, p.number, p.title, p.author, p.url,
			p.head_sha, p.state, COALESCE(r.summary, ''), r.created_at,
			p.pr_created_at, p.pr_updated_at,
			(SELECT COUNT(*) FROM comments WHERE review_id=r.id AND severity='blocker'),
			(SELECT COUNT(*) FROM comments WHERE review_id=r.id AND severity='major'),
			(SELECT COUNT(*) FROM comments WHERE review_id=r.id AND severity='minor'),
			(SELECT COUNT(*) FROM comments WHERE review_id=r.id AND severity='nit'),
			(SELECT COUNT(*) FROM comments WHERE review_id=r.id AND selected=1)
		FROM reviews r
		JOIN prs p ON p.id = r.pr_id
		WHERE r.state='pending'
		ORDER BY r.created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list pending reviews: %w", err)
	}
	defer rows.Close()

	var out []DashboardReview
	for rows.Next() {
		var d DashboardReview
		if err := rows.Scan(
			&d.ReviewID, &d.PRID, &d.Owner, &d.Repo, &d.Number, &d.Title,
			&d.Author, &d.URL, &d.HeadSHA, &d.PRState, &d.Summary, &d.CreatedAt,
			&d.PRCreatedAt, &d.PRUpdatedAt,
			&d.Blockers, &d.Majors, &d.Minors, &d.Nits, &d.Selected,
		); err != nil {
			return nil, fmt.Errorf("scan dashboard row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListPendingComments returns the comments of every pending review,
// grouped by review id — used by the dashboard's expandable cards.
func ListPendingComments(ctx context.Context, db *sql.DB) (map[int64][]CommentDetail, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.review_id, c.id, c.severity, c.path, c.line, c.body, COALESCE(c.diff_hunk,''), c.selected
		FROM comments c
		JOIN reviews r ON r.id = c.review_id
		WHERE r.state='pending'
		ORDER BY c.review_id,
			CASE c.severity
				WHEN 'blocker' THEN 1 WHEN 'major' THEN 2 WHEN 'minor' THEN 3 WHEN 'nit' THEN 4
			END,
			c.path, c.line
	`)
	if err != nil {
		return nil, fmt.Errorf("list pending comments: %w", err)
	}
	defer rows.Close()

	out := map[int64][]CommentDetail{}
	for rows.Next() {
		var reviewID int64
		var c CommentDetail
		var sel int
		if err := rows.Scan(&reviewID, &c.ID, &c.Severity, &c.Path, &c.Line, &c.Body, &c.DiffHunk, &sel); err != nil {
			return nil, fmt.Errorf("scan pending comment: %w", err)
		}
		c.Selected = sel != 0
		out[reviewID] = append(out[reviewID], c)
	}
	return out, rows.Err()
}

// CommentDetail is one row on the review detail page.
type CommentDetail struct {
	ID       int64
	Severity string
	Path     string
	Line     int
	Body     string
	DiffHunk string // code area captured at review time; "" for pre-migration rows
	Selected bool
}

// FollowupDetail is one prior-review follow-up row on the detail page.
type FollowupDetail struct {
	Path      string
	Line      int
	Status    string
	Note      string
	FindingID string
}

// ReviewDetail is the full data for the /pr/{id} page.
type ReviewDetail struct {
	ReviewID  int64
	State     string
	HeadSHA   string // SHA the review was generated against (may lag prs.head_sha)
	Summary   string
	CreatedAt time.Time
	PR        DashboardReview // reuse fields; only PR-related cells populated
	Comments  []CommentDetail
	Followups []FollowupDetail
}

// GetReviewDetail loads a single review by id, with its PR and all
// comments. Returns sql.ErrNoRows if the review is absent.
func GetReviewDetail(ctx context.Context, db *sql.DB, reviewID int64) (*ReviewDetail, error) {
	var d ReviewDetail
	err := db.QueryRowContext(ctx, `
		SELECT r.id, r.state, r.head_sha, COALESCE(r.summary, ''), r.created_at,
		       p.id, p.owner, p.repo, p.number, p.title, p.author, p.url, p.head_sha, p.state
		FROM reviews r
		JOIN prs p ON p.id = r.pr_id
		WHERE r.id = ?
	`, reviewID).Scan(
		&d.ReviewID, &d.State, &d.HeadSHA, &d.Summary, &d.CreatedAt,
		&d.PR.PRID, &d.PR.Owner, &d.PR.Repo, &d.PR.Number, &d.PR.Title,
		&d.PR.Author, &d.PR.URL, &d.PR.HeadSHA, &d.PR.PRState,
	)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, severity, path, line, body, COALESCE(diff_hunk,''), selected
		FROM comments
		WHERE review_id = ?
		ORDER BY
			CASE severity
				WHEN 'blocker' THEN 1
				WHEN 'major'   THEN 2
				WHEN 'minor'   THEN 3
				WHEN 'nit'     THEN 4
			END,
			path, line
	`, reviewID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c CommentDetail
		var sel int
		if err := rows.Scan(&c.ID, &c.Severity, &c.Path, &c.Line, &c.Body, &c.DiffHunk, &sel); err != nil {
			return nil, fmt.Errorf("scan comment: %w", err)
		}
		c.Selected = sel != 0
		d.Comments = append(d.Comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	fuRows, err := db.QueryContext(ctx, `
		SELECT path, line, status, COALESCE(note,''), COALESCE(finding_id,'')
		FROM followups WHERE review_id=? ORDER BY
			CASE status WHEN 'outstanding' THEN 1 WHEN 'disputed' THEN 2 ELSE 3 END,
			path, line
	`, reviewID)
	if err != nil {
		return nil, fmt.Errorf("list followups: %w", err)
	}
	defer fuRows.Close()
	for fuRows.Next() {
		var f FollowupDetail
		if err := fuRows.Scan(&f.Path, &f.Line, &f.Status, &f.Note, &f.FindingID); err != nil {
			return nil, fmt.Errorf("scan followup: %w", err)
		}
		d.Followups = append(d.Followups, f)
	}
	return &d, fuRows.Err()
}

// Stats are the all-time counters shown on the dashboard.
type Stats struct {
	PRsReviewed    int // distinct PRs that received at least one generated review
	ReviewsPosted  int // reviews submitted to GitHub
	CommentsPosted int // inline comments shipped
}

func GetStats(ctx context.Context, db *sql.DB) (Stats, error) {
	var s Stats
	err := db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(DISTINCT pr_id) FROM reviews WHERE state != 'failed'),
			(SELECT COUNT(*) FROM reviews WHERE state='posted'),
			(SELECT COUNT(*) FROM comments WHERE posted=1)
	`).Scan(&s.PRsReviewed, &s.ReviewsPosted, &s.CommentsPosted)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: %w", err)
	}
	return s, nil
}

// RunSummary is the joined latest-run row shown on the dashboard.
type RunSummary struct {
	ID          int64
	Trigger     string
	Status      string
	StartedAt   time.Time
	FinishedAt  sql.NullTime
	Error       sql.NullString
	ReviewCount int
}

// LatestRun returns the most recent run row, or nil if no runs exist.
func LatestRun(ctx context.Context, db *sql.DB) (*RunSummary, error) {
	var rs RunSummary
	err := db.QueryRowContext(ctx, `
		SELECT r.id, r.trigger, r.status, r.started_at, r.finished_at, r.error,
		       (SELECT COUNT(*) FROM reviews WHERE run_id=r.id) AS review_count
		FROM runs r
		ORDER BY r.id DESC
		LIMIT 1
	`).Scan(&rs.ID, &rs.Trigger, &rs.Status, &rs.StartedAt, &rs.FinishedAt, &rs.Error, &rs.ReviewCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest run: %w", err)
	}
	return &rs, nil
}

// MarkReviewPosted transitions a review to 'posted', records the GitHub
// review id, and flags every selected comment as posted. One transaction.
func MarkReviewPosted(ctx context.Context, db *sql.DB, reviewID, githubReviewID int64, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE reviews SET state='posted', posted_at=?, github_review_id=? WHERE id=?
	`, dbTime(now), githubReviewID, reviewID); err != nil {
		return fmt.Errorf("mark review posted: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE comments SET posted=1 WHERE review_id=? AND selected=1
	`, reviewID); err != nil {
		return fmt.Errorf("mark comments posted: %w", err)
	}
	return tx.Commit()
}

// MarkReviewDismissed transitions a pending review to 'dismissed'.
// Returns sql.ErrNoRows if the review isn't pending (already posted /
// dismissed reviews must not silently flip state).
func MarkReviewDismissed(ctx context.Context, db *sql.DB, reviewID int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE reviews SET state='dismissed' WHERE id=? AND state='pending'`, reviewID)
	if err != nil {
		return fmt.Errorf("dismiss review: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateReviewSummary edits the summary of a pending review (the text
// that posts as the GitHub review body). Returns sql.ErrNoRows when the
// review is absent or no longer pending — posted/dismissed summaries are
// a historical record and must not change.
func UpdateReviewSummary(ctx context.Context, db *sql.DB, reviewID int64, summary string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE reviews SET summary=? WHERE id=? AND state='pending'`, summary, reviewID)
	if err != nil {
		return fmt.Errorf("update review summary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetCommentSelected toggles a comment's selected flag.
func SetCommentSelected(ctx context.Context, db *sql.DB, commentID int64, selected bool) error {
	val := 0
	if selected {
		val = 1
	}
	res, err := db.ExecContext(ctx, `UPDATE comments SET selected=? WHERE id=?`, val, commentID)
	if err != nil {
		return fmt.Errorf("update selected: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// PersistFailedReview records a reviews row in 'failed' state, storing the
// raw output for debugging. Used when claude returns un-parseable output
// or an explicit error schema.
func PersistFailedReview(ctx context.Context, db *sql.DB, prID, runID int64, headSHA, raw string, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO reviews (pr_id, run_id, head_sha, raw_output, state, created_at)
		VALUES (?, ?, ?, ?, 'failed', ?)
	`, prID, runID, headSHA, raw, dbTime(now))
	if err != nil {
		return 0, fmt.Errorf("insert failed review: %w", err)
	}
	return res.LastInsertId()
}
