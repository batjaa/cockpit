package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// defaultSkillName is the slug cockpit ships as its default review engine
// and the folder it installs under ~/.claude/skills. Users can point
// claude.skill in the config at any other slug that emits the same JSON.
const defaultSkillName = "pr-review-structured"

// prReviewSkill is the vendored default review skill, embedded so a
// `go install`ed or brew-installed binary can lay it down without the repo
// present. Keep skills/pr-review-structured/SKILL.md in sync — it's both
// the shipped template and the documented output contract.
//
//go:embed skills/pr-review-structured/SKILL.md
var prReviewSkill string

// skillInstallPath returns ~/.claude/skills/<name>/SKILL.md — where the
// claude CLI looks up a skill by slug.
func skillInstallPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", name, "SKILL.md"), nil
}

// installSkill writes the embedded review-skill template into
// ~/.claude/skills/<name>/SKILL.md. Passing a custom name is how users
// generate a starting point they can edit; the JSON output contract at the
// bottom of SKILL.md must be preserved so cockpit can parse the result.
func installSkill(name string) (string, error) {
	if name == "" {
		name = defaultSkillName
	}
	path, err := skillInstallPath(name)
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
