package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type GHPR struct {
	Number        int              `json:"number"`
	Title         string           `json:"title"`
	URL           string           `json:"url"`
	HeadRefOid    string           `json:"headRefOid"`
	Author        GHAuthor         `json:"author"`
	State         string           `json:"state"`     // OPEN | MERGED | CLOSED
	CreatedAt     time.Time        `json:"createdAt"` // PR opened time (GitHub)
	UpdatedAt     time.Time        `json:"updatedAt"` // PR last-activity time (GitHub)
	LatestReviews []GHLatestReview `json:"latestReviews"`
}

type GHAuthor struct {
	Login string `json:"login"`
}

// GHLatestReview is one reviewer's most recent review on a PR.
type GHLatestReview struct {
	Author GHAuthor `json:"author"`
	State  string   `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
}

// MyLatestReviewState returns the state of login's latest review on the
// PR, or "" if they haven't reviewed.
func (p GHPR) MyLatestReviewState(login string) string {
	for _, r := range p.LatestReviews {
		if r.Author.Login == login {
			return r.State
		}
	}
	return ""
}

// CurrentLogin returns the authenticated gh user's login.
func CurrentLogin(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh api user: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ListPRs runs `gh pr list --repo <owner/name> --search <remaining>` after
// extracting the `repo:` qualifier from the search.
//
// Two GitHub-quirk workarounds layered in here:
//
//  1. gh pr list refuses to run without a current-repo context, even when
//     --search has `repo:` — so the qualifier is lifted out and passed
//     via --repo explicitly.
//
//  2. GitHub search ANDs qualifiers like `author:` and
//     `team-review-requested:`, so `author:alice author:bob` — or
//     `author:alice team-review-requested:org/team` — returns the
//     intersection (usually zero), not the union. Match the gh-dash UX:
//     when 2+ such fan-out qualifiers are present, fan out one call per
//     qualifier and union the results, deduped by PR number. This is how
//     "my authors' PRs plus the team's review-requested PRs" is expressed.
func ListPRs(ctx context.Context, search string, limit int) ([]GHPR, error) {
	repo, rest, err := extractRepoQualifier(search)
	if err != nil {
		return nil, err
	}
	terms, base := extractFanoutQualifiers(rest)

	// 0 or 1 fan-out qualifier: a single search already expresses it (the
	// lone term, if any, stays in rest and ANDs cleanly with the base).
	if len(terms) <= 1 {
		return runGHList(ctx, repo, rest, limit)
	}

	// 2+ qualifiers GitHub would AND: run one search per term and union by
	// PR number so, e.g., a set of author: filters and a
	// team-review-requested: filter combine as OR, not AND.
	seen := make(map[int]bool, len(terms)*4)
	var all []GHPR
	for _, term := range terms {
		q := strings.TrimSpace(base + " " + term)
		prs, err := runGHList(ctx, repo, q, limit)
		if err != nil {
			return nil, err
		}
		for _, p := range prs {
			if seen[p.Number] {
				continue
			}
			seen[p.Number] = true
			all = append(all, p)
		}
	}
	return all, nil
}

func runGHList(ctx context.Context, repo, search string, limit int) ([]GHPR, error) {
	args := []string{
		"pr", "list",
		"--repo", repo,
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,url,headRefOid,author,state,createdAt,updatedAt,latestReviews",
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh pr list: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []GHPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}
	return prs, nil
}

// fanoutPrefixes are the search qualifiers GitHub ANDs together but that
// cockpit wants OR'd: each becomes its own sub-search, unioned by PR number.
// Excludes like `-author:spam` don't match these positive prefixes, so they
// stay in the shared base and filter every branch.
var fanoutPrefixes = []string{"author:", "review-requested:", "team-review-requested:"}

func isFanoutQualifier(tok string) bool {
	for _, p := range fanoutPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}

// extractFanoutQualifiers pulls every fan-out qualifier token (see
// fanoutPrefixes) out of the query, dedupes them, and returns them as full
// tokens alongside the remaining shared query. GitHub ANDs these qualifiers,
// so cockpit runs one search per token and unions the results.
func extractFanoutQualifiers(search string) (terms []string, rest string) {
	seen := map[string]bool{}
	var others []string
	for _, tok := range strings.Fields(search) {
		if isFanoutQualifier(tok) {
			if !seen[tok] {
				seen[tok] = true
				terms = append(terms, tok)
			}
			continue
		}
		others = append(others, tok)
	}
	return terms, strings.Join(others, " ")
}

// extractRepoQualifier pulls a single `repo:owner/name` token out of a
// GitHub search query and returns it alongside the remaining qualifiers.
// Cockpit requires exactly one `repo:` per search filter.
func extractRepoQualifier(search string) (repo, rest string, err error) {
	var repos, others []string
	for _, tok := range strings.Fields(search) {
		if r, ok := strings.CutPrefix(tok, "repo:"); ok {
			repos = append(repos, r)
		} else {
			others = append(others, tok)
		}
	}
	switch len(repos) {
	case 0:
		return "", "", fmt.Errorf("search must include a `repo:owner/name` qualifier (got %q)", search)
	case 1:
		return repos[0], strings.Join(others, " "), nil
	default:
		return "", "", fmt.Errorf("search has %d repo: qualifiers; cockpit supports one per filter", len(repos))
	}
}

// ViewPR fetches a single PR's metadata by URL or owner/repo#num ref.
func ViewPR(ctx context.Context, prURL string) (GHPR, error) {
	args := []string{
		"pr", "view",
	}
	if owner, repo, number, err := ParseRepo(prURL); err == nil {
		args = append(args, strconv.Itoa(number), "--repo", owner+"/"+repo)
	} else {
		args = append(args, prURL)
	}
	args = append(args,
		"--json", "number,title,url,headRefOid,author,state,createdAt,updatedAt,latestReviews",
	)
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return GHPR{}, fmt.Errorf("gh pr view: %w: %s", err, string(ee.Stderr))
		}
		return GHPR{}, fmt.Errorf("gh pr view: %w", err)
	}
	var pr GHPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return GHPR{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	return pr, nil
}

// ReviewPayload is the GitHub "create a review" request body.
// https://docs.github.com/en/rest/pulls/reviews#create-a-review-for-a-pull-request
type ReviewPayload struct {
	CommitID string                 `json:"commit_id"`
	Event    string                 `json:"event"` // APPROVE | REQUEST_CHANGES | COMMENT
	Body     string                 `json:"body,omitempty"`
	Comments []ReviewPayloadComment `json:"comments"`
}

type ReviewPayloadComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"` // always RIGHT — comments anchor to the new file
	Body string `json:"body"`
}

// PostedReview is the subset of GitHub's response we keep.
type PostedReview struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
}

// PostReview submits one review with all selected comments in a single
// API call via `gh api`. The payload goes over stdin to avoid shell
// escaping issues with comment bodies.
func PostReview(ctx context.Context, owner, repo string, number int, payload ReviewPayload) (*PostedReview, error) {
	// GitHub 422s on "comments": null ("nil is not an array") — an
	// approve-without-comments submission must serialize as [].
	if payload.Comments == nil {
		payload.Comments = []ReviewPayloadComment{}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal review payload: %w", err)
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", owner, repo, number)
	cmd := exec.CommandContext(ctx, "gh", "api", "-X", "POST", endpoint, "--input", "-")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api post review: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh api post review: %w", err)
	}
	var pr PostedReview
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("parse post review response: %w", err)
	}
	return &pr, nil
}

// FetchPRDiff returns the unified diff for a PR.
func FetchPRDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "diff",
		strconv.Itoa(number), "--repo", owner+"/"+repo)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr diff: %w: %s", err, string(ee.Stderr))
		}
		return "", fmt.Errorf("gh pr diff: %w", err)
	}
	return string(out), nil
}

// hunkRe matches `@@ -a,b +c,d @@`; groups capture the new-file start
// line and count (count absent means 1).
var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

type lineRange struct{ start, end int }

// parseDiffHunks maps each file in a unified diff to its new-file line
// ranges. GitHub only accepts inline review comments on lines that fall
// inside these ranges.
func parseDiffHunks(diff string) map[string][]lineRange {
	hunks := map[string][]lineRange{}
	var cur string
	for _, ln := range strings.Split(diff, "\n") {
		if rest, ok := strings.CutPrefix(ln, "+++ b/"); ok {
			cur = rest
			continue
		}
		if cur == "" || !strings.HasPrefix(ln, "@@") {
			continue
		}
		m := hunkRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		start, _ := strconv.Atoi(m[1])
		count := 1
		if m[2] != "" {
			count, _ = strconv.Atoi(m[2])
		}
		if count > 0 {
			hunks[cur] = append(hunks[cur], lineRange{start, start + count - 1})
		}
	}
	return hunks
}

func lineInHunks(hunks map[string][]lineRange, path string, line int) bool {
	for _, r := range hunks[path] {
		if line >= r.start && line <= r.end {
			return true
		}
	}
	return false
}

// resolveLine snaps a line to the nearest line inside the file's hunks.
// Returns (line, true) unchanged when already in a hunk, the closest
// in-hunk line otherwise, and (-1, false) when the file has no hunks at
// all. The LLM's line resolution drifts (e.g. line 295 in a 154-line new
// file), so postability is enforced deterministically here.
func resolveLine(hunks map[string][]lineRange, path string, line int) (int, bool) {
	ranges := hunks[path]
	if len(ranges) == 0 {
		return -1, false
	}
	best, bestDist := -1, int(^uint(0)>>1)
	for _, r := range ranges {
		clamped := min(max(line, r.start), r.end)
		dist := clamped - line
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist {
			best, bestDist = clamped, dist
		}
	}
	return best, true
}

// snippetMaxLines caps how many hunk content lines precede-and-include the
// anchor in a captured diff snippet. Oversized hunks are truncated to the
// last snippetMaxLines lines ending at the anchor; the @@ header numbers are
// left untouched (the snippet is for display, not re-parsing).
const snippetMaxLines = 30

// extractDiffSnippet returns the diff_hunk (GitHub semantics) for path@line:
// the hunk's @@ header followed by every hunk line from the hunk start
// through the line whose new-file number equals line — nothing after the
// anchor. Returns "" when line <= 0, the file isn't in the diff, or no hunk
// contains line.
//
// New-file line accounting mirrors parseDiffHunks: context (" ") and added
// ("+") lines advance the counter; removed ("-") lines do not. So "-" lines
// before the anchor must not shift it.
func extractDiffSnippet(diff string, path string, line int) string {
	if line <= 0 {
		return ""
	}
	var (
		curFile string
		inHunk  bool     // walking a hunk that belongs to path
		header  string   // current hunk's @@ header
		newLine int      // new-file line number of the next " "/"+" line
		body    []string // hunk content lines accumulated after the header
	)
	for _, ln := range strings.Split(diff, "\n") {
		if rest, ok := strings.CutPrefix(ln, "+++ b/"); ok {
			curFile = rest
			inHunk = false
			continue
		}
		if strings.HasPrefix(ln, "diff --git") {
			inHunk = false // new file; abandon any in-progress hunk
			continue
		}
		if strings.HasPrefix(ln, "@@") {
			m := hunkRe.FindStringSubmatch(ln)
			if m == nil {
				inHunk = false
				continue
			}
			start, _ := strconv.Atoi(m[1])
			header, newLine, body, inHunk = ln, start, body[:0], curFile == path
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "-"):
			body = append(body, ln) // removed: does not advance new-file line
		case strings.HasPrefix(ln, "+"), strings.HasPrefix(ln, " "):
			body = append(body, ln)
			if newLine == line {
				return joinSnippet(header, body)
			}
			newLine++
		case strings.HasPrefix(ln, `\`):
			body = append(body, ln) // "\ No newline at end of file" marker
		default:
			inHunk = false // unrecognized line (e.g. trailing "") ends the hunk
		}
	}
	return ""
}

func joinSnippet(header string, body []string) string {
	if len(body) > snippetMaxLines {
		body = body[len(body)-snippetMaxLines:]
	}
	return header + "\n" + strings.Join(body, "\n")
}

// ReviewThread is one inline-comment thread on a PR, flattened from the
// GraphQL response: the root comment plus any replies.
type ReviewThread struct {
	IsResolved bool
	IsOutdated bool
	Path       string
	Line       int
	RootBody   string
	Replies    []ThreadReply
}

type ThreadReply struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          isOutdated
          comments(first: 50) {
            nodes { body path line author { login } }
          }
        }
      }
    }
  }
}`

// FetchReviewThreads returns all inline review threads on a PR with
// resolution state and replies. Resolution flags only exist in the
// GraphQL API — the REST comments endpoint doesn't expose them.
func FetchReviewThreads(ctx context.Context, owner, repo string, number int) ([]ReviewThread, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql",
		"-f", "query="+reviewThreadsQuery,
		"-f", "owner="+owner,
		"-f", "repo="+repo,
		"-F", fmt.Sprintf("number=%d", number),
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api graphql review threads: %w: %s", err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("gh api graphql review threads: %w", err)
	}

	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
							IsOutdated bool `json:"isOutdated"`
							Comments   struct {
								Nodes []struct {
									Body   string `json:"body"`
									Path   string `json:"path"`
									Line   int    `json:"line"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse review threads: %w", err)
	}

	var threads []ReviewThread
	for _, n := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if len(n.Comments.Nodes) == 0 {
			continue
		}
		root := n.Comments.Nodes[0]
		t := ReviewThread{
			IsResolved: n.IsResolved,
			IsOutdated: n.IsOutdated,
			Path:       root.Path,
			Line:       root.Line,
			RootBody:   root.Body,
		}
		for _, reply := range n.Comments.Nodes[1:] {
			t.Replies = append(t.Replies, ThreadReply{Author: reply.Author.Login, Body: reply.Body})
		}
		threads = append(threads, t)
	}
	return threads, nil
}

// ParseRepo extracts (owner, repo, number) from a github PR URL like
// https://github.com/owner/repo/pull/123. The PR URL is the canonical
// source for the upstream repo coordinates — headRepository fields on
// `gh pr list` results point at the fork when the PR comes from one.
func ParseRepo(url string) (owner, repo string, number int, err error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(url, prefix) {
		return "", "", 0, fmt.Errorf("not a github URL: %s", url)
	}
	parts := strings.Split(url[len(prefix):], "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("not a PR URL: %s", url)
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid PR number %q: %w", parts[3], err)
	}
	return parts[0], parts[1], n, nil
}
