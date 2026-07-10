package main

import (
	"regexp"
	"sort"
)

// jiraExcludePrefixes lists all-caps "project" prefixes that look like
// JIRA keys when followed by -<number> but are almost always something
// else in code/text contexts (encodings, hashes, standards, protocols,
// model names). Kept as a package-level map so it's easy to extend.
//
// CVE is intentionally NOT excluded: "CVE-2024-1234" is a useful
// reference worth keeping.
var jiraExcludePrefixes = map[string]bool{
	"UTF":  true,
	"SHA":  true,
	"RFC":  true,
	"ISO":  true,
	"GPT":  true,
	"HTTP": true,
}

var (
	// jiraRe matches JIRA-style keys: a 2-10 char uppercase/digit
	// project prefix (starting with a letter) followed by -<number>.
	//
	// The trailing (?:-\d+)* extends the spec's base pattern so that
	// multi-segment numeric references (notably "CVE-2024-1234") are
	// captured whole rather than truncated at the first number group.
	// Single-segment JIRA keys like "PLAT-422" are unaffected.
	jiraRe = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d+(?:-\d+)*\b`)

	// jiraPrefixRe pulls the project portion (before the first dash)
	// out of a matched JIRA key so it can be checked against the
	// exclude list. The key always contains a dash, so FindString
	// never returns "".
	jiraPrefixRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}`)

	// ghURLRe matches GitHub pull/issue URLs, with or without scheme,
	// capturing owner, repo and number.
	ghURLRe = regexp.MustCompile(`(?:https?://)?github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(?:pull|issues)/(\d+)`)

	// shortRefRe matches short cross-repo references like
	// "owner/repo#123" appearing literally in text.
	shortRefRe = regexp.MustCompile(`\b([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#(\d+)\b`)
)

// ExtractTickets returns normalized, deduplicated ticket references
// found in s, in first-appearance order.
func ExtractTickets(s string) []string {
	type match struct {
		start int
		value string
	}
	var matches []match

	// JIRA-style keys, kept verbatim (minus excluded prefixes).
	for _, loc := range jiraRe.FindAllStringIndex(s, -1) {
		key := s[loc[0]:loc[1]]
		prefix := jiraPrefixRe.FindString(key)
		if jiraExcludePrefixes[prefix] {
			continue
		}
		matches = append(matches, match{start: loc[0], value: key})
	}

	// GitHub PR/issue URLs -> "owner/repo#n".
	for _, loc := range ghURLRe.FindAllStringSubmatchIndex(s, -1) {
		owner := s[loc[2]:loc[3]]
		repo := s[loc[4]:loc[5]]
		num := s[loc[6]:loc[7]]
		matches = append(matches, match{start: loc[0], value: owner + "/" + repo + "#" + num})
	}

	// Short cross-repo refs "owner/repo#123", kept normalized.
	for _, loc := range shortRefRe.FindAllStringSubmatchIndex(s, -1) {
		owner := s[loc[2]:loc[3]]
		repo := s[loc[4]:loc[5]]
		num := s[loc[6]:loc[7]]
		matches = append(matches, match{start: loc[0], value: owner + "/" + repo + "#" + num})
	}

	if len(matches) == 0 {
		return nil
	}

	// Order by first appearance in the input string. A stable sort by
	// start offset preserves the per-pattern append order for ties
	// (which only occur between equal values, since the patterns don't
	// overlap at the same offset with different values).
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	var out []string
	seen := make(map[string]bool)
	for _, m := range matches {
		if seen[m.value] {
			continue
		}
		seen[m.value] = true
		out = append(out, m.value)
	}
	return out
}
