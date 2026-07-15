package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeStubGH writes an executable shell script named `gh` that prints
// jsonBody to stdout, into a fresh temp dir. The dir is returned so the
// caller can prepend it to PATH.
func writeStubGH(t *testing.T, jsonBody string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'JSON_EOF'\n" + jsonBody + "\nJSON_EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func rewriteStubGH(t *testing.T, dir, jsonBody string) {
	t.Helper()
	script := "#!/bin/sh\ncat <<'JSON_EOF'\n" + jsonBody + "\nJSON_EOF\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRepoQualifier(t *testing.T) {
	cases := []struct {
		in       string
		wantRepo string
		wantRest string
		wantErr  bool
	}{
		{
			in:       "repo:owner/repo is:pr is:open -is:draft author:alice",
			wantRepo: "owner/repo",
			wantRest: "is:pr is:open -is:draft author:alice",
		},
		{
			in:       "is:pr is:open repo:o/r author:bob",
			wantRepo: "o/r",
			wantRest: "is:pr is:open author:bob",
		},
		{
			in:       "repo:o/r",
			wantRepo: "o/r",
			wantRest: "",
		},
		{
			in:      "is:pr is:open",
			wantErr: true, // no repo:
		},
		{
			in:      "repo:a/b repo:c/d",
			wantErr: true, // multiple
		},
	}
	for _, c := range cases {
		repo, rest, err := extractRepoQualifier(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("extractRepoQualifier(%q): want error, got (%q, %q)", c.in, repo, rest)
			}
			continue
		}
		if err != nil {
			t.Errorf("extractRepoQualifier(%q): unexpected error: %v", c.in, err)
			continue
		}
		if repo != c.wantRepo || rest != c.wantRest {
			t.Errorf("extractRepoQualifier(%q) = (%q, %q), want (%q, %q)",
				c.in, repo, rest, c.wantRepo, c.wantRest)
		}
	}
}

func TestExtractFanoutQualifiers(t *testing.T) {
	cases := []struct {
		in        string
		wantTerms []string
		wantRest  string
	}{
		{
			in:        "is:pr is:open author:alice author:bob",
			wantTerms: []string{"author:alice", "author:bob"},
			wantRest:  "is:pr is:open",
		},
		{
			in:        "author:alice author:alice author:bob", // dedupe
			wantTerms: []string{"author:alice", "author:bob"},
			wantRest:  "",
		},
		{
			in:        "is:pr -author:spam author:alice",
			wantTerms: []string{"author:alice"},
			wantRest:  "is:pr -author:spam", // -author: excludes stay shared
		},
		{
			// authors OR a team review request: both are fan-out terms.
			in:        "is:pr is:open author:alice team-review-requested:org/team",
			wantTerms: []string{"author:alice", "team-review-requested:org/team"},
			wantRest:  "is:pr is:open",
		},
		{
			in:        "review-requested:me team-review-requested:org/team",
			wantTerms: []string{"review-requested:me", "team-review-requested:org/team"},
			wantRest:  "",
		},
		{
			in:        "is:pr is:open",
			wantTerms: nil,
			wantRest:  "is:pr is:open",
		},
	}
	for _, c := range cases {
		got, rest := extractFanoutQualifiers(c.in)
		if rest != c.wantRest {
			t.Errorf("extractFanoutQualifiers(%q): rest=%q want %q", c.in, rest, c.wantRest)
		}
		if !equalStr(got, c.wantTerms) {
			t.Errorf("extractFanoutQualifiers(%q): terms=%v want %v", c.in, got, c.wantTerms)
		}
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListPRs_MultiAuthorFanout verifies that multiple `author:` tokens
// trigger one gh call per author, and that the union is deduped by PR
// number. Uses a stub gh that picks a fixture based on which author
// appears in its args.
func TestListPRs_MultiAuthorFanout(t *testing.T) {
	dir := t.TempDir()
	// Each author returns disjoint PRs except PR #200 which both alice
	// and bob have authored (e.g. co-author), so it should appear once.
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    *author:alice*)
      cat <<'JSON_EOF'
[
  {"number":100,"title":"Alice PR","url":"https://github.com/o/r/pull/100","headRefOid":"sha-a","author":{"login":"alice"}},
  {"number":200,"title":"Co-PR","url":"https://github.com/o/r/pull/200","headRefOid":"sha-c","author":{"login":"alice"}}
]
JSON_EOF
      exit 0
      ;;
    *author:bob*)
      cat <<'JSON_EOF'
[
  {"number":200,"title":"Co-PR","url":"https://github.com/o/r/pull/200","headRefOid":"sha-c","author":{"login":"bob"}},
  {"number":300,"title":"Bob PR","url":"https://github.com/o/r/pull/300","headRefOid":"sha-b","author":{"login":"bob"}}
]
JSON_EOF
      exit 0
      ;;
  esac
done
echo "[]"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prs, err := ListPRs(context.Background(), "repo:o/r is:open author:alice author:bob", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 {
		t.Fatalf("want 3 deduped PRs, got %d: %+v", len(prs), prs)
	}
	nums := map[int]bool{}
	for _, p := range prs {
		if nums[p.Number] {
			t.Errorf("PR #%d returned twice", p.Number)
		}
		nums[p.Number] = true
	}
	for _, want := range []int{100, 200, 300} {
		if !nums[want] {
			t.Errorf("missing PR #%d in union", want)
		}
	}
}

// TestListPRs_AuthorPlusTeamFanout verifies that an author: filter and a
// team-review-requested: filter fan out into separate searches whose results
// are unioned (not intersected), deduped by PR number.
func TestListPRs_AuthorPlusTeamFanout(t *testing.T) {
	dir := t.TempDir()
	// author:alice → {100, 200}; the team request → {200 (overlap), 400}.
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    *author:alice*)
      cat <<'JSON_EOF'
[
  {"number":100,"title":"Alice PR","url":"https://github.com/o/r/pull/100","headRefOid":"sha-a","author":{"login":"alice"}},
  {"number":200,"title":"Shared PR","url":"https://github.com/o/r/pull/200","headRefOid":"sha-s","author":{"login":"alice"}}
]
JSON_EOF
      exit 0
      ;;
    *team-review-requested:org/team*)
      cat <<'JSON_EOF'
[
  {"number":200,"title":"Shared PR","url":"https://github.com/o/r/pull/200","headRefOid":"sha-s","author":{"login":"alice"}},
  {"number":400,"title":"Team-requested PR","url":"https://github.com/o/r/pull/400","headRefOid":"sha-t","author":{"login":"carol"}}
]
JSON_EOF
      exit 0
      ;;
  esac
done
echo "[]"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prs, err := ListPRs(context.Background(),
		"repo:o/r is:open author:alice team-review-requested:org/team", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 {
		t.Fatalf("want 3 deduped PRs (100, 200, 400), got %d: %+v", len(prs), prs)
	}
	nums := map[int]bool{}
	for _, p := range prs {
		if nums[p.Number] {
			t.Errorf("PR #%d returned twice", p.Number)
		}
		nums[p.Number] = true
	}
	for _, want := range []int{100, 200, 400} {
		if !nums[want] {
			t.Errorf("missing PR #%d in union", want)
		}
	}
}

func TestExtractDiffSnippet(t *testing.T) {
	// Two files; a.go and b.go each carry a single hunk. New-file line
	// numbers: a.go {1: package a, 2: added-a, 3: var A}; b.go starts at 10.
	multiFile := `diff --git a/a.go b/a.go
index e69de29..1111111 100644
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package a
+// added-a
 var A = 1
diff --git a/b.go b/b.go
index e69de29..2222222 100644
--- a/b.go
+++ b/b.go
@@ -10,2 +10,3 @@
 package b
+// added-b
 var B = 2
`

	// One file, two hunks — target lands in the second so we prove the walk
	// resets per @@ and doesn't leak hunk-1 lines.
	multiHunk := `diff --git a/m.go b/m.go
--- a/m.go
+++ b/m.go
@@ -1,2 +1,2 @@
 first
 second
@@ -20,2 +20,3 @@
 twenty
+twentyone
 twentytwo
`

	// A removed line sits before the anchor; it must not shift the new-file
	// counter, so line 2 is addedNew (not keep3).
	removedBefore := `diff --git a/r.go b/r.go
--- a/r.go
+++ b/r.go
@@ -1,3 +1,3 @@
 keep1
-removed
+addedNew
 keep3
`

	// 40 added lines; the anchor at line 40 forces truncation to the last 30.
	var big strings.Builder
	big.WriteString("diff --git a/big.go b/big.go\n--- a/big.go\n+++ b/big.go\n@@ -0,0 +1,40 @@\n")
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&big, "+line%d\n", i)
	}

	cases := []struct {
		name       string
		diff       string
		path       string
		line       int
		wantEmpty  bool
		wantPrefix string
		contains   []string
		excludes   []string
		wantLines  int // 0 = don't check
	}{
		{
			name: "second file", diff: multiFile, path: "b.go", line: 11,
			wantPrefix: "@@ -10,2 +10,3 @@",
			contains:   []string{"added-b", "package b"},
			excludes:   []string{"added-a", "var B = 2"},
		},
		{
			name: "anchor at hunk first line", diff: multiFile, path: "a.go", line: 1,
			wantPrefix: "@@ -1,2 +1,3 @@",
			contains:   []string{"package a"},
			excludes:   []string{"added-a", "var A = 1"},
		},
		{
			name: "anchor at hunk last line", diff: multiFile, path: "a.go", line: 3,
			wantPrefix: "@@ -1,2 +1,3 @@",
			contains:   []string{"package a", "added-a", "var A = 1"},
		},
		{
			name: "target in second hunk", diff: multiHunk, path: "m.go", line: 21,
			wantPrefix: "@@ -20,2 +20,3 @@",
			contains:   []string{"twenty", "twentyone"},
			excludes:   []string{"first", "second", "twentytwo"},
		},
		{
			name: "removed line does not shift anchor", diff: removedBefore, path: "r.go", line: 2,
			wantPrefix: "@@ -1,3 +1,3 @@",
			contains:   []string{"keep1", "-removed", "addedNew"},
			excludes:   []string{"keep3"},
		},
		{
			name: "truncation keeps last 30 ending at anchor", diff: big.String(), path: "big.go", line: 40,
			wantPrefix: "@@ -0,0 +1,40 @@",
			contains:   []string{"+line40", "+line11"},
			excludes:   []string{"+line10\n", "+line1\n"},
			wantLines:  1 + snippetMaxLines,
		},
		{name: "line not in any hunk", diff: multiFile, path: "a.go", line: 99, wantEmpty: true},
		{name: "file missing", diff: multiFile, path: "nope.go", line: 1, wantEmpty: true},
		{name: "non-positive line", diff: multiFile, path: "a.go", line: 0, wantEmpty: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractDiffSnippet(c.diff, c.path, c.line)
			if c.wantEmpty {
				if got != "" {
					t.Fatalf("want empty, got %q", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("want snippet, got empty")
			}
			if !strings.HasPrefix(got, c.wantPrefix) {
				t.Errorf("prefix: got %q want prefix %q", got, c.wantPrefix)
			}
			for _, s := range c.contains {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in:\n%s", s, got)
				}
			}
			for _, s := range c.excludes {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in:\n%s", s, got)
				}
			}
			if c.wantLines != 0 {
				if n := len(strings.Split(got, "\n")); n != c.wantLines {
					t.Errorf("lines=%d want %d", n, c.wantLines)
				}
			}
		})
	}
}

func TestParseRepo(t *testing.T) {
	cases := []struct {
		url       string
		wantOwner string
		wantRepo  string
		wantNum   int
		wantErr   bool
	}{
		{"https://github.com/owner/repo/pull/352103", "owner", "repo", 352103, false},
		{"https://github.com/octocat/hello-world/pull/1", "octocat", "hello-world", 1, false},
		{"https://github.com/owner/repo/issues/1", "", "", 0, true},
		{"https://gitlab.com/owner/repo/pull/1", "", "", 0, true},
		{"https://github.com/owner/repo/pull/abc", "", "", 0, true},
	}
	for _, c := range cases {
		o, r, n, err := ParseRepo(c.url)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRepo(%q): want error, got nil", c.url)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRepo(%q): unexpected error: %v", c.url, err)
			continue
		}
		if o != c.wantOwner || r != c.wantRepo || n != c.wantNum {
			t.Errorf("ParseRepo(%q) = (%q, %q, %d), want (%q, %q, %d)",
				c.url, o, r, n, c.wantOwner, c.wantRepo, c.wantNum)
		}
	}
}

func TestViewPR_URLUsesExplicitRepo(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$GH_ARGS_PATH"
cat <<'JSON_EOF'
{"number":4467,"title":"P","url":"https://github.com/owner/repo/pull/4467","headRefOid":"sha","author":{"login":"alice"}}
JSON_EOF
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_ARGS_PATH", argsPath)

	pr, err := ViewPR(context.Background(), "https://github.com/owner/repo/pull/4467")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 4467 {
		t.Fatalf("number=%d want 4467", pr.Number)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	want := []string{
		"pr", "view", "4467",
		"--repo", "owner/repo",
		"--json", "number,title,url,headRefOid,author,state,createdAt,updatedAt,latestReviews",
	}
	if !equalStr(got, want) {
		t.Fatalf("gh args=%v want %v", got, want)
	}
}

func TestListPRs_StubGH(t *testing.T) {
	fixture := `[
  {"number":100,"title":"Add foo","url":"https://github.com/owner/repo/pull/100","headRefOid":"abc123","author":{"login":"alice"}},
  {"number":200,"title":"Fix bar","url":"https://github.com/owner/repo/pull/200","headRefOid":"def456","author":{"login":"bob"}}
]`
	stubDir := writeStubGH(t, fixture)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prs, err := ListPRs(context.Background(), "repo:owner/repo is:pr is:open", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 {
		t.Fatalf("want 2 PRs, got %d", len(prs))
	}
	if prs[0].Number != 100 || prs[0].Title != "Add foo" || prs[0].Author.Login != "alice" || prs[0].HeadRefOid != "abc123" {
		t.Errorf("pr[0] mismatch: %+v", prs[0])
	}
	if prs[1].Number != 200 || prs[1].Author.Login != "bob" {
		t.Errorf("pr[1] mismatch: %+v", prs[1])
	}
}

// TestE2E_DiscoverAndUpsert runs the full step-2 pipeline end to end:
// fetch via gh (stubbed), upsert into a real SQLite DB, verify rows.
// Then re-run with a modified fixture and verify update semantics —
// first_seen preserved, last_seen advanced, new row inserted.
func TestE2E_DiscoverAndUpsert(t *testing.T) {
	fixture1 := `[
  {"number":100,"title":"Add foo","url":"https://github.com/owner/repo/pull/100","headRefOid":"sha1","author":{"login":"alice"}},
  {"number":200,"title":"Fix bar","url":"https://github.com/owner/repo/pull/200","headRefOid":"sha2","author":{"login":"bob"}}
]`
	stubDir := writeStubGH(t, fixture1)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	now1 := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// Phase 1: initial discovery
	prs, err := ListPRs(ctx, "repo:owner/repo is:pr is:open", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prs {
		if _, err := UpsertPR(ctx, db, p, now1); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("phase 1: want 2 rows, got %d", count)
	}

	// Capture phase 1 timestamps for PR #100
	var fs1, ls1 time.Time
	if err := db.QueryRow(
		`SELECT first_seen, last_seen FROM prs WHERE owner='owner' AND repo='repo' AND number=100`,
	).Scan(&fs1, &ls1); err != nil {
		t.Fatal(err)
	}
	if fs1.Unix() != now1.Unix() || ls1.Unix() != now1.Unix() {
		t.Errorf("phase 1 timestamps: first_seen=%v last_seen=%v want both %v", fs1, ls1, now1)
	}

	// Phase 2: PR #100 force-pushed (new title + head_sha), PR #200 unchanged,
	// new PR #300 appears.
	fixture2 := `[
  {"number":100,"title":"Add foo (renamed)","url":"https://github.com/owner/repo/pull/100","headRefOid":"sha1-new","author":{"login":"alice"}},
  {"number":200,"title":"Fix bar","url":"https://github.com/owner/repo/pull/200","headRefOid":"sha2","author":{"login":"bob"}},
  {"number":300,"title":"Refactor baz","url":"https://github.com/owner/repo/pull/300","headRefOid":"sha3","author":{"login":"carol"}}
]`
	rewriteStubGH(t, stubDir, fixture2)
	now2 := now1.Add(4 * time.Hour)

	prs, err = ListPRs(ctx, "repo:owner/repo is:pr is:open", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prs {
		if _, err := UpsertPR(ctx, db, p, now2); err != nil {
			t.Fatalf("upsert phase 2: %v", err)
		}
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM prs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("phase 2: want 3 rows, got %d", count)
	}

	// Verify PR #100 updated in place
	var title, headSHA string
	var fs2, ls2 time.Time
	if err := db.QueryRow(
		`SELECT title, head_sha, first_seen, last_seen FROM prs WHERE number=100`,
	).Scan(&title, &headSHA, &fs2, &ls2); err != nil {
		t.Fatal(err)
	}
	if title != "Add foo (renamed)" {
		t.Errorf("title not updated: %q", title)
	}
	if headSHA != "sha1-new" {
		t.Errorf("head_sha not updated: %q", headSHA)
	}
	if fs2.Unix() != now1.Unix() {
		t.Errorf("first_seen mutated: got %v, want %v", fs2, now1)
	}
	if ls2.Unix() != now2.Unix() {
		t.Errorf("last_seen not advanced: got %v, want %v", ls2, now2)
	}

	// Verify PR #200 last_seen advanced even though nothing else changed
	var ls200 time.Time
	if err := db.QueryRow(
		`SELECT last_seen FROM prs WHERE number=200`,
	).Scan(&ls200); err != nil {
		t.Fatal(err)
	}
	if ls200.Unix() != now2.Unix() {
		t.Errorf("PR #200 last_seen not advanced: got %v, want %v", ls200, now2)
	}

	// Verify new PR #300 inserted
	var newCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM prs WHERE number=300 AND author='carol' AND head_sha='sha3'`,
	).Scan(&newCount); err != nil {
		t.Fatal(err)
	}
	if newCount != 1 {
		t.Errorf("PR #300 not inserted correctly")
	}
}
