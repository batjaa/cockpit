---
name: pr-review-structured
description: >
  Canonical PR review primitive: runs the PERFECT review framework against a
  GitHub PR, resolves line numbers against diff hunks, formats each finding
  in Conventional Comments, and emits ONE JSON object as its final message.
  Does not prompt the user. Does not post to GitHub. Use this skill when
  invoked via `/pr-review-structured`, from automation (cockpit), or when
  another skill (like `pr-review` or `pr-review-comment`) needs structured
  review data to render or post. For chat-rendered review use `pr-review`.
  For interactive review-and-post use `pr-review-comment`.
---

# PR Review (Structured) — Canonical Review Primitive

This skill is the **foundational PR review engine**. It owns the review
logic. Two sibling skills are thin presentation wrappers on top:

```
                ┌───────────────────────────────────┐
                │       pr-review-structured        │  ← review engine + JSON
                │  (PERFECT pass, line-resolve,     │
                │   Conventional Comments bodies)   │
                └────────────────┬──────────────────┘
                                 │
                ┌────────────────┴──────────────────┐
                │                                   │
                ▼                                   ▼
       ┌───────────────┐                  ┌───────────────────┐
       │  pr-review    │                  │ pr-review-comment │
       │  renders JSON │                  │ render + Ask +    │
       │  to chat md   │                  │ post via gh api   │
       └───────────────┘                  └───────────────────┘

       Also consumed directly by: cockpit (claude -p), other automation
```

What this skill does — and only does:

1. Fetch PR metadata + diff with `gh`.
2. Run the PERFECT review pass (defined below — this is the canonical
   definition; siblings reference it).
3. Assign stable IDs to findings (`B1`, `M2`, `m3`, `n1`).
4. Resolve every finding's line number against the PR's diff hunks.
5. Format each finding's `body` in Conventional Comments — ready to POST
   verbatim.
6. Emit one JSON object as the final message. No prose after it.

What this skill must NOT do:

- Call `AskUserQuestion`. Ever.
- Mutate GitHub state (`gh api -X POST/PATCH/DELETE`).
- Render chat markdown — that's `pr-review`'s job.
- Drive the selection/posting flow — that's `pr-review-comment`'s job.

---

## Input

A GitHub PR URL or `<owner>/<repo>#<number>` reference. If neither is
provided (e.g. raw diff only), emit
`{"error": "PR URL required; use pr-review for raw diffs"}` and exit
non-zero.

Optionally followed by `--previous <file>`: a JSON file describing a
previously posted review on this PR — its findings and the current
GitHub thread state for each (resolved/outdated flags plus author
replies). When present, read it and apply the **follow-up rules**
below; the consumer (cockpit) generates this file automatically on
re-reviews.

```json
{
  "previous_review": {
    "github_review_id": 777,
    "head_sha": "<sha the prior review was posted against>",
    "findings": [
      {
        "path": "a.go",
        "line": 10,
        "severity": "major",
        "body": "**issue (blocking):** ...",
        "resolved": true,
        "outdated": false,
        "replies": [{"author": "octocat", "body": "fixed in abc123"}]
      }
    ]
  }
}
```

Fetch metadata, diff, and head commit:

```bash
gh pr view <num> --repo <owner>/<repo> \
  --json title,author,baseRefName,headRefName,commits,url \
  --jq '{title, author: .author.login, baseRefName, headRefName, head: .commits[-1].oid, url}'

gh pr diff <num> --repo <owner>/<repo> > /tmp/pr<num>.diff
```

---

## Step 0 — Skip generated code

Generated files are not reviewed. Emit **no findings** (and no positives)
for a file when either holds:

- Its content carries a generation marker — `// Code generated … DO NOT
  EDIT.`, `@generated`, or an equivalent "do not edit" header near the
  top of the file.
- Its path matches a well-known generated/lockfile pattern: `*.pb.go`,
  `*.pb.gw.go`, `*_gen.go`, `*.gen.go`, `*_generated.go`,
  `zz_generated*.go`, `*_string.go`, `mock_*.go`, `mocks/`, `vendor/`,
  `node_modules/`, `go.sum`, `package-lock.json`, `yarn.lock`,
  `pnpm-lock.yaml`, `*.min.*`, `__snapshots__/`, `*.snap` — or is
  otherwise clearly machine-generated (registry-generated config,
  feature-flag accessors, codegen output).

You may still *read* generated hunks for context (e.g. a new flag name
the hand-written code consumes), but they produce no findings and don't
count toward the verdict. If the entire diff is generated, emit an empty
`findings` array with a summary saying so. Consumers (cockpit) drop
findings on excluded paths as a hard filter — a finding on a generated
file is wasted work.

---

## The PERFECT framework (canonical)

Apply the seven principles top-down. Stop and flag blockers immediately —
don't soft-pedal.

### P — Purpose
*Does the code actually solve the stated task?*
- Trace the happy path end-to-end against the PR description / linked
  ticket.
- Flag if implementation diverges from the stated goal, or if the PR solves
  the wrong problem.

Severity if violated: **blocker**

### E — Edge Cases
*Are inputs, boundaries, and unexpected states handled?*
- Nil/null/undefined dereferences; off-by-one; empty collections; zero,
  negative, max values; type coercion / overflow.
- Missing enum / switch cases.
- Unhandled error returns. **Go**: every returned `error` handled;
  `interface{}` casts guarded. **TS**: no `as any` without justification;
  optional chaining where needed.
- Race conditions in concurrent code.

Severity: **blocker** (reachable nil deref / data corruption) or **major**
(unreachable but plausible).

### R — Reliability
*Performance and security regressions.*

Performance:
- N+1 queries, sequential I/O that could be batched.
- Missing pagination on potentially large datasets.
- Unbounded goroutine spawning (Go: missing `sync.WaitGroup`, no context
  cancellation); leaked `http.Response.Body`.
- Missing DB indexes introduced by this PR.

Security:
- Secrets or credentials in code / logs.
- Unvalidated user input reaching SQL / shell / filesystem.
- Auth checks missing on new endpoints.
- Overly permissive IAM (AWS); SQS/SNS payloads logged in plaintext.

Distributed systems (SQS, gRPC, Kinesis):
- Missing idempotency for SQS consumers; no DLQ consideration.
- gRPC streaming without cancellation context.
- Kinesis shard iterator not renewed.

Severity: **blocker** (security) / **major** (perf).

### F — Form
*Cohesion / coupling — design principles.*
- New responsibilities placed in the right layer / package?
- Function does one thing?
- Dependencies injected, not hardcoded?
- Does this PR violate existing patterns in the codebase?

Flag SOLID violations only when they create tangible maintenance cost —
not as style preferences.

Severity: **major** or **minor**.

### E2 — Evidence
*Tests exist and pass.*
- New code paths covered by tests?
- Tests check behavior, not implementation details?
- CI checks green? (flag if unknown)
- Mocks/stubs not so heavy that they prevent testing real behavior.
- Test names descriptive (`TestHandleEvent_WhenQueueEmpty_ReturnsNoOp`).

No tests on non-trivial logic → **major**.

### C — Clarity
*Intent obvious without reading every line.*
- Self-documenting names; non-obvious decisions explained; no dead code.
- Would a new team member understand this in 2 minutes?

Severity: **minor** (unless truly obfuscated → **major**).

### T — Taste
*Pure preferences — never block.*
- Style deviations not covered by a linter; minor naming preferences;
  equally-valid alternatives.

Severity: **nit** only.

### Severities (canonical)

| JSON value | When | Posts as (default) |
|---|---|---|
| `blocker` | Must fix before merge — security, correctness, breaks build | `**issue (blocking):**` |
| `major`   | Should fix — perf regression, missing tests, design violation | `**issue (blocking):**` or `**suggestion (blocking):**` |
| `minor`   | Nice to fix — small clarity / refactor                        | `**suggestion:**` |
| `nit`     | Pure preference                                                | `**nitpick (non-blocking):**` |

Every finding must include: **location**, **what's wrong**, **why it
matters**, **a proposed fix**.

---

## Step 1 — Assign stable IDs

After the PERFECT pass, order findings by severity, then path, then line.
Assign IDs:

- Blockers: `B1`, `B2`, …
- Major:    `M1`, `M2`, …
- Minor:    `m1`, `m2`, … (lowercase)
- Nit:      `n1`, `n2`, …

IDs are referenced by downstream consumers (`pr-review-comment`, cockpit).
Don't renumber after assignment.

---

## Step 2 — Resolve line numbers against PR hunks

GitHub returns HTTP 422 `Line could not be resolved` for inline comments
that target lines outside the diff hunks. Resolve before emitting so
consumers can POST `body` verbatim.

For each unique file in the findings:

1. From `/tmp/pr<num>.diff`, parse `@@ -old,oldN +new,newN @@` headers to
   build the set of valid new-file line ranges for that file.
2. For each finding targeting that file:
   - Inside a hunk → `line` and `original_line` are equal.
   - Outside → shift `line` to the **nearest in-hunk line** (prefer an
     added `+` line over context). Set `original_line` to the value the
     reviewer first flagged. Reference the original line in `body` (e.g.
     "The guard at line 99 above…").
3. **Never drop a finding silently.** If no in-hunk shift exists in the
   file, emit with `line: -1` and a note in `body`; consumers surface it
   as un-postable.

Sanity-check a line exists in the head commit when needed:

```bash
gh api "repos/<owner>/<repo>/contents/<path>?ref=<head-sha>" \
  --jq .content | base64 -d > /tmp/head_<basename>
```

---

## Step 3 — Format each `body` in Conventional Comments

```
**<label> (<decoration>):** <one-line subject>

<optional context — why it matters, references>

**suggestion:** <proposed fix, if not already in the label>
```

Default severity → label mapping (override per finding when content
warrants):

| Severity | Default label |
|---|---|
| `blocker` | `**issue (blocking):**` |
| `major` (correctness)   | `**issue (blocking):**` |
| `major` (design choice) | `**suggestion (blocking):**` |
| `minor` | `**suggestion:**` (no decoration) |
| `nit`   | `**nitpick (non-blocking):**` |

Other valid Conventional Comments labels (`question`, `praise`, `thought`,
`note`, `todo`, `chore`) may be used when appropriate. Reference:
<https://conventionalcomments.org/>.

The `body` field in the output is what consumers POST — no trimming, no
decoration injection happens downstream.

---

## Step 4 — Emit JSON

The **final message** of this skill must be a single JSON object — and
nothing else. No prose before or after, no code fences. Consumers parse by
scanning from the end of stdout for the last `{ ... }`, so internal
reasoning before the JSON is tolerated but a trailing prose summary
breaks parsing.

### Success schema

```json
{
  "pr": {
    "url": "https://github.com/owner/repo/pull/123",
    "owner": "owner",
    "repo": "repo",
    "number": 123,
    "title": "Add foo to bar",
    "author": "octocat",
    "head_sha": "abc123def4..."
  },
  "summary": "Nice — scoping the new variant behind the flag and keeping it data-only makes this easy to reason about. I've got one blocking question on the LD key naming, plus a couple of optional suggestions inline.",
  "verdict": "approve-with-suggestions",
  "findings": [
    {
      "id": "B1",
      "severity": "blocker",
      "perfect": "E",
      "path": "internal/foo/foo.go",
      "line": 142,
      "original_line": 142,
      "body": "**issue (blocking):** Nil deref on empty input.\n\nWhen `items` is empty the loop body still runs once because of the off-by-one on line 140, dereferencing `items[0]`.\n\n**suggestion:** Guard with `if len(items) == 0 { return nil }` before the loop."
    }
  ],
  "positives": [
    "Test coverage for the new path is thorough."
  ],
  "followups": [
    {"path": "a.go", "line": 10, "status": "addressed", "note": "Guard added in the latest push."},
    {"path": "b.go", "line": 20, "status": "outstanding", "note": "Loop still unguarded.", "finding_id": "M1"},
    {"path": "c.go", "line": 5, "status": "disputed", "note": "Author: intentional for backwards compat."}
  ]
}
```

Field rules:

- `summary`: **posted verbatim as the top-level body of the GitHub
  review, under the author's PR.** Write it TO the author in second
  person ("you"), the way a colleague comments on your PR — react to
  the change, don't narrate it back. Open with a genuine, specific
  reaction (fold the strongest positive in), then your overall take on the
  approach. Stay a level above the inline comments: every specific issue is
  already posted inline as its own comment, so the summary speaks to the
  overall approach — design, structure, testing, risk — not a recap of the
  individual findings. Four failure modes, all of which make it read as
  notes ABOUT the PR to a third party instead of a message to the person
  who wrote it:
    - **Explaining the author's own code back to them.** They wrote it;
      they know what it does. "the parameter validation on every widget
      closes off the SQL-injection surface that raw string interpolation
      would otherwise open" is you proving you understood it. Aim the
      same point at them: "nice call validating every widget's params —
      keeps the query-building injection-safe."
    - **Opening with a graded label.** "This is a clean, well-documented
      addition", "Clean, focused additive change" — a verdict announced
      to an audience. Lead with the substance instead.
    - **Reviewer-log tics.** "I noticed", "the author has", "this PR
      does X" — rephrase toward "you".
    - **Rehashing the inline findings.** Each issue is already its own
      inline comment, so re-listing it in the summary is redundant noise.
      If several findings share a root cause, name that one theme ("the two
      notebooks are drifting toward duplication") rather than enumerating
      the individual spots.
  It must read like a review comment, not an analysis. The
  machine-readable assessment lives in `verdict`, so the summary never
  needs verdict language.
- `verdict`: one of `"approve"`, `"approve-with-suggestions"`,
  `"request-changes"`. Use `"request-changes"` only when at least one
  blocker exists.
- `perfect`: one of `"P"`, `"E"`, `"R"`, `"F"`, `"E2"`, `"C"`, `"T"`.
- `severity`: one of `"blocker"`, `"major"`, `"minor"`, `"nit"`.
- `findings`: may be empty.
- `positives`: include at least one entry.
- `line`: integer. `-1` means line resolution failed; finding reported but
  not postable as-is.
- `followups`: present **only when `--previous` was given**; one entry per
  prior finding, in the same order as the input file. Omit the field
  entirely otherwise.

### Follow-up rules (when `--previous` is given)

Classify every prior finding — never drop one silently:

| `status` | When | Effect on findings |
|---|---|---|
| `addressed` | The new diff fixes it, or the thread is resolved/outdated | Do NOT re-raise. |
| `outstanding` | Still applies at the new head | Re-raise as a new finding; set `finding_id` to its ID. The new body must reference the prior discussion ("Raised in the previous review — still applies…") instead of repeating the original cold. |
| `disputed` | An author reply pushes back with reasoning | Do NOT re-raise unless it is blocker severity (correctness/security) — then re-raise and engage the author's argument in the body. |

`note` is one sentence of evidence ("guard added at line 142", "author
says intentional"). New, unrelated findings in the fresh diff are normal
findings — followups only cover the prior review's items.

### Error schema

If the PR can't be fetched (404, gh auth missing, malformed ref):

```json
{"error": "human-readable reason"}
```

…and exit non-zero. Do not partial-fill the success schema.

---

## Rules

1. **No prompts.** Never call `AskUserQuestion`.
2. **No posting.** Never call `gh api -X POST/PATCH/DELETE` on PR or
   review endpoints.
3. **Final message is JSON only.** No code fences, no trailing prose.
4. **Stable IDs.** Don't renumber after Step 1.
5. **Don't drop findings.** Unresolvable line → `line: -1`, not omission.
6. **One source of formatting truth.** Bodies emitted ready to POST.
7. Every finding: location, what's wrong, why it matters, proposed fix.
   Tag with PERFECT letter. Always include at least one positive.

---

## Quick reference

```bash
# Metadata + head SHA
gh pr view <num> --repo <owner>/<repo> \
  --json title,author,baseRefName,headRefName,commits,url \
  --jq '{title, author: .author.login, head: .commits[-1].oid, url}'

# Diff
gh pr diff <num> --repo <owner>/<repo> > /tmp/pr<num>.diff

# Hunk headers per file
grep -E "^(diff --git|@@ )" /tmp/pr<num>.diff

# File contents at head
gh api "repos/<owner>/<repo>/contents/<path>?ref=<sha>" --jq .content \
  | base64 -d > /tmp/head_file.ext
```
