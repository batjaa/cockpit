package main

import (
	"log/slog"
	"path"
	"strings"
)

// defaultExcludePaths is the shipped review.exclude_paths value: paths
// that are machine-generated or otherwise not worth review findings.
// Users override the whole list in the config file.
func defaultExcludePaths() []string {
	return []string{
		"*.pb.go",
		"*.pb.gw.go",
		"*_gen.go",
		"*.gen.go",
		"*_generated.go",
		"zz_generated*.go",
		"*_string.go",
		"mock_*.go",
		"mocks/",
		"vendor/",
		"node_modules/",
		"go.sum",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"*.min.js",
		"*.min.css",
		"__snapshots__/",
		"*.snap",
	}
}

// pathExcluded reports whether p (a slash-separated repo-relative path)
// matches any exclusion pattern. A pattern glob-matches (path.Match
// syntax) the full path or any suffix starting at a path-segment
// boundary — so "feature_flags.go" and "internal/feature_flags.go" both
// match "svc/internal/feature_flags.go". A trailing "/" makes it a
// directory pattern: "vendor/" matches any path with a vendor segment.
func pathExcluded(patterns []string, p string) bool {
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if dir, ok := strings.CutSuffix(pat, "/"); ok {
			if matchesSegment(dir, p) {
				return true
			}
			continue
		}
		if matchesSuffix(pat, p) {
			return true
		}
	}
	return false
}

// matchesSuffix reports whether pat glob-matches p or any suffix of p
// starting immediately after a "/".
func matchesSuffix(pat, p string) bool {
	if ok, _ := path.Match(pat, p); ok {
		return true
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if ok, _ := path.Match(pat, p[i+1:]); ok {
				return true
			}
		}
	}
	return false
}

// matchesSegment reports whether any directory segment of p (every
// segment except the final basename) glob-matches pat.
func matchesSegment(pat, p string) bool {
	segs := strings.Split(p, "/")
	for _, seg := range segs[:len(segs)-1] {
		if ok, _ := path.Match(pat, seg); ok {
			return true
		}
	}
	return false
}

// dropExcludedFindings removes findings on excluded paths so they never
// reach the DB, the dashboard, or GitHub. The skill is instructed to
// skip generated files itself; this is the deterministic backstop.
func dropExcludedFindings(findings []StructuredReviewFinding, patterns []string, prURL string) []StructuredReviewFinding {
	if len(patterns) == 0 {
		return findings
	}
	kept := findings[:0]
	for _, f := range findings {
		if pathExcluded(patterns, f.Path) {
			slog.Info("dropped finding on excluded path", "pr", prURL, "path", f.Path, "id", f.ID)
			continue
		}
		kept = append(kept, f)
	}
	return kept
}
