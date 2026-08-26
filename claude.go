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
	PR            StructuredReviewPR        `json:"pr"`
	ReviewBrief   StructuredReviewBrief     `json:"review_brief"`
	AuthorMessage string                    `json:"author_message"`
	Verdict       string                    `json:"verdict"`
	Findings      []StructuredReviewFinding `json:"findings"`
	Positives     []string                  `json:"positives"`

	// Summary is the v1 author-facing field. New reviews use ReviewBrief and
	// AuthorMessage; keeping this parser field lets custom skills migrate
	// without ever treating a private brief as public text.
	Summary string `json:"summary,omitempty"`

	// Followups is populated only on re-reviews (when a --previous
	// context file was passed): one entry per prior posted finding.
	Followups []StructuredReviewFollowup `json:"followups,omitempty"`

	// Error is populated when the skill emits the error schema instead.
	Error string `json:"error,omitempty"`
}

type StructuredReviewBrief struct {
	Change            StructuredReviewChange        `json:"change"`
	RiskLevel         string                        `json:"risk_level"`
	RiskRationale     string                        `json:"risk_rationale"`
	ComplexAreas      []StructuredReviewComplexArea `json:"complex_areas"`
	BoundaryChanges   []StructuredReviewBoundary    `json:"boundary_changes"`
	BlastRadius       string                        `json:"blast_radius"`
	Validation        StructuredReviewValidation    `json:"validation"`
	Uncertainties     []string                      `json:"uncertainties"`
	Recommendation    string                        `json:"recommendation"`
	HighLevelConcerns []StructuredReviewConcern     `json:"high_level_concerns"`
}

type StructuredReviewChange struct {
	Intent    string `json:"intent"`
	Mechanism string `json:"mechanism"`
}

type StructuredReviewComplexArea struct {
	Area string `json:"area"`
	Why  string `json:"why"`
}

type StructuredReviewBoundary struct {
	Boundary string `json:"boundary"`
	Impact   string `json:"impact"`
}

type StructuredReviewValidation struct {
	Coverage string   `json:"coverage"`
	Gaps     []string `json:"gaps"`
}

type StructuredReviewConcern struct {
	Concern string `json:"concern"`
	Why     string `json:"why"`
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
	if err := validateStructuredReview(jsonBytes, &sr); err != nil {
		return nil, out, err
	}
	return &sr, out, nil
}

// validateStructuredReview enforces the v2 private/public boundary while
// allowing the v1 summary-only contract during the custom-skill migration
// window. Required JSON keys are checked separately from Go zero values so an
// omitted risk dimension cannot silently look like an intentionally empty
// list.
func validateStructuredReview(data []byte, sr *StructuredReview) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("inspect structured review: %w", err)
	}
	briefRaw, hasBrief := top["review_brief"]
	if !hasBrief || string(briefRaw) == "null" {
		if strings.TrimSpace(sr.Summary) != "" {
			return nil // v1 custom-skill output
		}
		return errors.New("structured review missing review_brief (or legacy summary)")
	}
	if _, ok := top["author_message"]; !ok {
		return errors.New("structured review missing author_message")
	}

	if err := requireJSONFields(briefRaw, "review_brief",
		"change", "risk_level", "risk_rationale", "complex_areas",
		"boundary_changes", "blast_radius", "validation", "uncertainties",
		"recommendation", "high_level_concerns"); err != nil {
		return err
	}
	var briefFields map[string]json.RawMessage
	_ = json.Unmarshal(briefRaw, &briefFields)
	if err := requireJSONFields(briefFields["change"], "review_brief.change", "intent", "mechanism"); err != nil {
		return err
	}
	if err := requireJSONFields(briefFields["validation"], "review_brief.validation", "coverage", "gaps"); err != nil {
		return err
	}

	requiredText := map[string]string{
		"review_brief.change.intent":       sr.ReviewBrief.Change.Intent,
		"review_brief.change.mechanism":    sr.ReviewBrief.Change.Mechanism,
		"review_brief.risk_rationale":      sr.ReviewBrief.RiskRationale,
		"review_brief.blast_radius":        sr.ReviewBrief.BlastRadius,
		"review_brief.validation.coverage": sr.ReviewBrief.Validation.Coverage,
		"review_brief.recommendation":      sr.ReviewBrief.Recommendation,
	}
	for field, value := range requiredText {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("structured review %s is empty", field)
		}
	}
	if sr.ReviewBrief.RiskLevel != "low" && sr.ReviewBrief.RiskLevel != "medium" && sr.ReviewBrief.RiskLevel != "high" {
		return fmt.Errorf("structured review review_brief.risk_level %q is invalid", sr.ReviewBrief.RiskLevel)
	}
	for i, area := range sr.ReviewBrief.ComplexAreas {
		if strings.TrimSpace(area.Area) == "" || strings.TrimSpace(area.Why) == "" {
			return fmt.Errorf("structured review review_brief.complex_areas[%d] is incomplete", i)
		}
	}
	for i, boundary := range sr.ReviewBrief.BoundaryChanges {
		if strings.TrimSpace(boundary.Boundary) == "" || strings.TrimSpace(boundary.Impact) == "" {
			return fmt.Errorf("structured review review_brief.boundary_changes[%d] is incomplete", i)
		}
	}
	for i, concern := range sr.ReviewBrief.HighLevelConcerns {
		if strings.TrimSpace(concern.Concern) == "" || strings.TrimSpace(concern.Why) == "" {
			return fmt.Errorf("structured review review_brief.high_level_concerns[%d] is incomplete", i)
		}
	}
	message := strings.TrimSpace(sr.AuthorMessage)
	if len(sr.ReviewBrief.HighLevelConcerns) == 0 && message != "" {
		return errors.New("structured review author_message requires a high-level concern")
	}
	if len(sr.ReviewBrief.HighLevelConcerns) > 0 && message == "" {
		return errors.New("structured review high-level concerns require author_message")
	}
	return nil
}

func requireJSONFields(data json.RawMessage, object string, fields ...string) error {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("structured review %s must be an object", object)
	}
	for _, field := range fields {
		if _, ok := values[field]; !ok {
			return fmt.Errorf("structured review %s missing %s", object, field)
		}
	}
	return nil
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
