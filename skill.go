package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// prReviewSkill is the vendored pr-review-structured skill, embedded so a
// `go install`ed or brew-installed binary can lay it down without the repo
// present. Keep skills/pr-review-structured/SKILL.md in sync with the skill
// cockpit actually runs.
//
//go:embed skills/pr-review-structured/SKILL.md
var prReviewSkill string

// skillInstallPath is where `claude` looks up the review skill.
func skillInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", "pr-review-structured", "SKILL.md"), nil
}

// installSkill writes the embedded skill into ~/.claude/skills so that
// `claude -p "/pr-review-structured ..."` can find and run it.
func installSkill() (string, error) {
	path, err := skillInstallPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create skill dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(prReviewSkill), 0o644); err != nil {
		return "", fmt.Errorf("write skill: %w", err)
	}
	return path, nil
}
