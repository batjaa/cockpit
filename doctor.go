package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// checkResult is one dependency-check line.
type checkResult struct {
	name string
	ok   bool
	hint string // shown when !ok
}

// runChecks verifies the external tools cockpit shells out to. Cheap and
// read-only, so it backs both `cockpit doctor` and the server preflight.
func runChecks(ctx context.Context, cfg Config) []checkResult {
	var out []checkResult

	if _, err := exec.LookPath("gh"); err != nil {
		out = append(out, checkResult{"gh CLI", false, "install the GitHub CLI: https://cli.github.com"})
	} else {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := exec.CommandContext(cctx, "gh", "auth", "status").Run()
		cancel()
		out = append(out, checkResult{"gh authenticated", err == nil, "run: gh auth login"})
	}

	bin := cfg.Claude.Binary
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		out = append(out, checkResult{"claude CLI", false, "install Claude Code, or set claude.binary in the config"})
	} else {
		out = append(out, checkResult{"claude CLI", true, ""})
	}

	skill := cfg.Claude.Skill
	if skill == "" {
		skill = defaultSkillName
	}
	if path, err := skillInstallPath(skill); err == nil {
		_, statErr := os.Stat(path)
		hint := "run: cockpit install-skill"
		if skill != defaultSkillName {
			hint = fmt.Sprintf("install with: cockpit install-skill --as %s  (or edit claude.skill in config)", skill)
		}
		out = append(out, checkResult{fmt.Sprintf("%s skill", skill), statErr == nil, hint})
	}

	return out
}

// doctor prints the checks as a checklist. Returns true when everything
// passed, so the caller can exit non-zero otherwise.
func doctor(ctx context.Context, cfg Config) bool {
	allOK := true
	fmt.Println("cockpit doctor — checking dependencies")
	fmt.Println()
	for _, c := range runChecks(ctx, cfg) {
		mark := "✓"
		if !c.ok {
			mark = "✗"
			allOK = false
		}
		fmt.Printf("  %s  %s\n", mark, c.name)
		if !c.ok && c.hint != "" {
			fmt.Printf("       → %s\n", c.hint)
		}
	}
	fmt.Println()
	if allOK {
		fmt.Println("All good. Set `search` in your config, then run `cockpit`.")
	} else {
		fmt.Println("Some checks failed — see the hints above.")
	}
	return allOK
}

// preflight logs a warning per failed check at server startup but never
// blocks: the web UI and local session scan work without gh/claude, and
// review attempts surface their own errors.
func preflight(ctx context.Context, cfg Config) {
	for _, c := range runChecks(ctx, cfg) {
		if !c.ok {
			slog.Warn("preflight: dependency not ready", "check", c.name, "fix", c.hint)
		}
	}
}
