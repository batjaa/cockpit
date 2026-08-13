package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// StructuredReview matches the JSON schema emitted by the
// `pr-review-structured` Claude skill.
type StructuredReview struct {
	PR        StructuredReviewPR        `json:"pr"`
	Summary   string                    `json:"summary"`
	Verdict   string                    `json:"verdict"`
	Findings  []StructuredReviewFinding `json:"findings"`
	Positives []string                  `json:"positives"`

	// Followups is populated only on re-reviews (when a --previous
	// context file was passed): one entry per prior posted finding.
	Followups []StructuredReviewFollowup `json:"followups,omitempty"`

	// Error is populated when the skill emits the error schema instead.
	Error string `json:"error,omitempty"`
}

type StructuredReviewFollowup struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Status    string `json:"status"` // addressed | outstanding | disputed
	Note      string `json:"note"`
	FindingID string `json:"finding_id,omitempty"`
}

type StructuredReviewPR struct {
	URL     string `json:"url"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Author  string `json:"author"`
	HeadSHA string `json:"head_sha"`
}

type StructuredReviewFinding struct {
	ID           string `json:"id"`
	Severity     string `json:"severity"`
	Perfect      string `json:"perfect"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	Body         string `json:"body"`

	// DiffHunk is captured by cockpit at review time (see extractDiffSnippet),
	// never parsed from the skill's JSON — json:"-" keeps a buggy or malicious
	// skill from injecting it.
	DiffHunk string `json:"-"`
}

// RunStructuredReview invokes `claude -p "/<skill> <prURL>"`
// with the given timeout, then parses the last JSON object from stdout.
// previousPath, when non-empty, points at a re-review context file and is
// passed to the skill as --previous. Returns the parsed review plus the
// raw stdout (always — useful for debugging on parse failure).
func RunStructuredReview(ctx context.Context, binary, model, skill, prURL, previousPath string, timeout time.Duration) (*StructuredReview, []byte, error) {
	if model == "" {
		model = "sonnet"
	}
	if skill == "" {
		skill = defaultSkillName
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt := "/" + skill + " " + prURL
	if previousPath != "" {
		prompt += " --previous " + previousPath
	}
	// bypassPermissions is required because the review skill
	// shells out to `gh` to fetch PR metadata / diff. acceptEdits only
	// auto-approves Edit/Write tools — bash commands would prompt and the
	// skill would hang. The skill itself is read-only (its rules forbid
	// posting and prompting), so the blast radius is bounded.
	args := []string{
		"-p",
		"--model", model,
		"--output-format", "text",
		"--permission-mode", "bypassPermissions",
		"--max-turns", "30",
		prompt,
	}
	cmd := exec.CommandContext(cctx, binary, args...)
	// On cancellation CommandContext kills the claude process, but Output()
	// would still block until the stdout pipe closes — and claude's own
	// children (gh subprocesses) inherit that pipe. WaitDelay force-closes
	// the pipes shortly after the process dies so shutdown stays prompt.
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, out, fmt.Errorf("%s: %w: %s", binary, err, string(ee.Stderr))
		}
		return nil, out, fmt.Errorf("%s: %w", binary, err)
	}

	jsonBytes, err := extractLastJSON(string(out))
	if err != nil {
		return nil, out, err
	}
	var sr StructuredReview
	if err := json.Unmarshal(jsonBytes, &sr); err != nil {
		return nil, out, fmt.Errorf("unmarshal structured review: %w", err)
	}
	if sr.Error != "" {
		return &sr, out, fmt.Errorf("structured review error: %s", sr.Error)
	}
	return &sr, out, nil
}

// extractLastJSON returns the last top-level JSON object embedded in s.
// Tolerates leading prose. Useful because `claude -p` may include
// internal reasoning before the final JSON message.
func extractLastJSON(s string) ([]byte, error) {
	var last []byte
	i := 0
	for i < len(s) {
		if s[i] != '{' {
			i++
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			i++
			continue
		}
		last = []byte(v)
		i += int(dec.InputOffset())
	}
	if last == nil {
		return nil, errors.New("no JSON object found in output")
	}
	return last, nil
}
