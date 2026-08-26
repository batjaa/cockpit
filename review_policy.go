package main

import "strings"

type reviewAction string

const (
	reviewActionReview reviewAction = "review"
	reviewActionSkip   reviewAction = "skip"

	skipReasonExcludedAuthor = "excluded_author"
)

// reviewDecision is the persisted result of applying review policy to a PR.
// Reason is a stable machine-readable code; presentation belongs at the UI
// boundary so wording can change without rewriting stored rows.
type reviewDecision struct {
	Action reviewAction
	Reason string
}

func defaultSkipAuthors() []string {
	// GitHub currently reports app-authored Dependabot PRs as app/dependabot.
	// Keep the canonical bot login too for repositories/API paths that expose
	// it directly.
	return []string{"app/dependabot", "dependabot[bot]"}
}

// decideReview is the single extension point for deciding whether a PR should
// spend an LLM review. Add future rules here and give each a stable reason
// code; discovery, manual review, persistence, and the dashboard all consume
// the same decision.
func decideReview(p GHPR, cfg ReviewConfig) reviewDecision {
	for _, author := range cfg.SkipAuthors {
		if strings.EqualFold(strings.TrimSpace(author), p.Author.Login) {
			return reviewDecision{Action: reviewActionSkip, Reason: skipReasonExcludedAuthor}
		}
	}
	return reviewDecision{Action: reviewActionReview}
}

func skipReasonLabel(reason, author string) string {
	switch reason {
	case skipReasonExcludedAuthor:
		if strings.EqualFold(author, "app/dependabot") || strings.EqualFold(author, "dependabot[bot]") {
			return "Dependabot-authored PR"
		}
		return author + " is excluded by review policy"
	default:
		return "Excluded by review policy"
	}
}
