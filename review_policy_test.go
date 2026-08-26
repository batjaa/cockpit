package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestDecideReviewByAuthor(t *testing.T) {
	tests := []struct {
		name    string
		author  string
		skipped []string
		want    reviewAction
	}{
		{"app dependabot", "app/dependabot", []string{"app/dependabot"}, reviewActionSkip},
		{"canonical dependabot", "dependabot[bot]", []string{"dependabot[bot]"}, reviewActionSkip},
		{"case insensitive", "App/Dependabot", []string{"app/dependabot"}, reviewActionSkip},
		{"human author", "alice", []string{"app/dependabot"}, reviewActionReview},
		{"explicitly disabled", "app/dependabot", []string{}, reviewActionReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideReview(
				GHPR{Author: GHAuthor{Login: tt.author}},
				ReviewConfig{SkipAuthors: tt.skipped},
			)
			if got.Action != tt.want {
				t.Fatalf("action=%q want %q", got.Action, tt.want)
			}
			if got.Action == reviewActionSkip && got.Reason != skipReasonExcludedAuthor {
				t.Errorf("reason=%q want %q", got.Reason, skipReasonExcludedAuthor)
			}
		})
	}
}

func TestSkipReasonLabel(t *testing.T) {
	if got := skipReasonLabel(skipReasonExcludedAuthor, "app/dependabot"); got != "Dependabot-authored PR" {
		t.Errorf("Dependabot label=%q", got)
	}
	if got := skipReasonLabel(skipReasonExcludedAuthor, "release-bot"); got != "release-bot is excluded by review policy" {
		t.Errorf("generic author label=%q", got)
	}
}

func TestRefreshStoredReviewDecisionsDismissesNewlySkippedReview(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()
	prID, err := UpsertPR(ctx, db, GHPR{
		Number: 1, Title: "Bump dependency", URL: "https://github.com/o/r/pull/1",
		HeadRefOid: "sha", Author: GHAuthor{Login: "app/dependabot"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := InsertRun(ctx, db, "manual", now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PersistReview(ctx, db, prID, runID, &StructuredReview{
		PR: StructuredReviewPR{HeadSHA: "sha"}, Summary: "old review",
	}, "raw", now)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := RefreshStoredReviewDecisions(ctx, db, ReviewConfig{SkipAuthors: []string{"app/dependabot"}})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Errorf("changed=%d want 1", changed)
	}
	var action, reason, reviewState string
	if err := db.QueryRow(`
		SELECT p.review_action, p.review_skip_reason, r.state
		FROM prs p JOIN reviews r ON r.pr_id=p.id WHERE p.id=?
	`, prID).Scan(&action, &reason, &reviewState); err != nil {
		t.Fatal(err)
	}
	if action != "skip" || reason != skipReasonExcludedAuthor || reviewState != "dismissed" {
		t.Errorf("stored state=%q/%q review=%q", action, reason, reviewState)
	}

	// Reapplying an unchanged policy is idempotent.
	changed, err = RefreshStoredReviewDecisions(ctx, db, ReviewConfig{SkipAuthors: []string{"APP/DEPENDABOT"}})
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("idempotent refresh changed=%d want 0", changed)
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM reviews WHERE state='pending'`).Scan(&pending); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("pending reviews=%d want 0", pending)
	}
}
