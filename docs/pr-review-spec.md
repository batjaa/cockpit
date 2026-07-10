# Cockpit — Local PR Review Pipeline

A single-binary Go app that periodically runs the `pr-review-comment` Claude
skill against PRs returned by a GitHub search filter, stores findings in
SQLite, and serves a web UI for selecting which comments to post and how to
submit the review.

Runs entirely locally. No external dependencies beyond `gh` and `claude` CLIs
already on the user's machine.

---

## Goals

- Unattended review of PRs on a configurable schedule.
- Human-in-the-loop posting: nothing goes to GitHub without explicit selection.
- Manual on-demand review of any PR URL.
- Minimal: one binary, one DB file, one config file.

## Non-goals

- Multi-user. Single-user local tool.
- Auth. Server binds to `127.0.0.1` only.
- Webhook ingestion. Polling is fine.
- Reviewing diffs outside GitHub.

---

## Stack

- Go (stdlib `net/http`, `html/template`, `database/sql`, `log/slog`).
- `modernc.org/sqlite` (pure-Go SQLite, no CGO).
- Tailwind CDN for styling (no build step).
- Shells out to `gh` and `claude`.

---

## Skill architecture

`pr-review-structured` is the **canonical review primitive**: it owns the
PERFECT framework, line resolution, and Conventional Comments formatting.
The other two PR-review skills are presentation wrappers on top.

```
              pr-review-structured        ← review engine + JSON output
                       │
       ┌───────────────┼───────────────────────┐
       │               │                       │
       ▼               ▼                       ▼
   pr-review     pr-review-comment           cockpit
   (chat md)     (Ask + post)             (web UI + post)
```

| Skill | Role | Consumers |
|---|---|---|
| `pr-review-structured` | PERFECT pass + line resolution + Conventional Comments bodies → JSON. No prompts. No posting. | cockpit, `pr-review`, `pr-review-comment` |
| `pr-review` | Call structured, render JSON to chat markdown. | Humans in chat |
| `pr-review-comment` | Call structured, render, `AskUserQuestion`, post via `gh api`. | Humans in chat |

All consumers post identical comment bodies — cockpit's web UI and the
interactive skill are just different selection front-ends over the same
structured output.

The skill is live at `~/.claude/skills/pr-review-structured/SKILL.md`.
Refactoring `pr-review` and `pr-review-comment` to delegate to it is a
separate workstream; cockpit only depends on `pr-review-structured`.

### Output schema

The skill's final message must be a JSON object only (no surrounding prose):

```json
{
  "pr": {
    "url": "https://github.com/owner/repo/pull/123",
    "owner": "owner",
    "repo": "repo",
    "number": 123,
    "title": "...",
    "author": "...",
    "head_sha": "abc123..."
  },
  "summary": "Author-facing paragraph; posted verbatim as the review body.",
  "verdict": "approve" | "approve-with-suggestions" | "request-changes",
  "findings": [
    {
      "id": "B1",
      "severity": "blocker" | "major" | "minor" | "nit",
      "perfect": "P" | "E" | "R" | "F" | "E2" | "C" | "T",
      "path": "path/from/repo/root.go",
      "line": 123,
      "original_line": 99,
      "body": "**issue (blocking):** Short subject.\n\nContext paragraph.\n\n**suggestion:** Proposed fix."
    }
  ],
  "positives": ["..."]
}
```

Notes:

- `summary` is written TO the PR author (it becomes the top-level body
  of the posted GitHub review), not as reviewer notes. Tone guidance
  lives in the skill's field rules.
- `body` is already in Conventional Comments format — consumers post it
  verbatim.
- `line` is the **resolved, in-hunk** line. `original_line` is what the
  reviewer originally flagged before line resolution shifted it; equal to
  `line` when no shift was needed.
- `id`s (`B1`, `M2`, `m3`, `n1`) are stable within a single run so the
  interactive skill can reference them in `AskUserQuestion` labels.
- On unrecoverable error (PR not found, gh auth missing), emit
  `{"error": "<message>"}` and exit non-zero.

### `pr-review-comment` refactor

Becomes a thin wrapper:

1. Invoke `pr-review-structured` against the PR URL.
2. Parse the JSON.
3. Render findings to chat using the existing template (Step 2 in the
   current SKILL.md).
4. Build `AskUserQuestion` from `findings` (existing Step 3 logic).
5. Compose and post the review (existing Steps 5–7), reusing `body` and
   `line` straight from the JSON — no re-formatting, no re-resolving.

This refactor is **out of scope for cockpit** but blocks it: cockpit can't
run reliably until `pr-review-structured` exists. Track as a separate task
in `~/.claude/skills/`.

---

## Config

File: `~/.cockpit/config.json` (auto-created on first run with defaults +
prompted values).

```json
{
  "search": "repo:owner/repo is:pr is:open -is:draft author:alice author:bob team-review-requested:org/team",
  "schedule": {
    "start_hour": 6,
    "end_hour": 18,
    "interval_hours": 4,
    "run_on_launch": true
  },
  "claude": {
    "binary": "claude",
    "timeout_seconds": 600,
    "concurrency": 3
  },
  "http": {
    "addr": "127.0.0.1:8765"
  }
}
```

Notes:

- `search` is passed verbatim to `gh pr list --search "<search>"`. Same syntax
  as `gh-dash` and the GitHub search bar.
- `start_hour` / `end_hour` are inclusive lower / exclusive upper bounds in
  local time. `interval_hours` is the gap between ticks within the window.
- `run_on_launch=true` triggers one run immediately at startup regardless of
  the window.
- CLI flags `--config <path>` and `--addr <host:port>` override.

---

## Data model (SQLite)

```sql
CREATE TABLE runs (
  id           INTEGER PRIMARY KEY,
  trigger      TEXT NOT NULL CHECK(trigger IN ('schedule','launch','manual')),
  started_at   DATETIME NOT NULL,
  finished_at  DATETIME,
  status       TEXT NOT NULL CHECK(status IN ('running','success','partial','error')),
  error        TEXT
);

CREATE TABLE prs (
  id          INTEGER PRIMARY KEY,
  owner       TEXT NOT NULL,
  repo        TEXT NOT NULL,
  number      INTEGER NOT NULL,
  url         TEXT NOT NULL,
  title       TEXT NOT NULL,
  author      TEXT NOT NULL,
  head_sha    TEXT NOT NULL,
  first_seen  DATETIME NOT NULL,
  last_seen   DATETIME NOT NULL,
  UNIQUE(owner, repo, number)
);

CREATE TABLE reviews (
  id          INTEGER PRIMARY KEY,
  pr_id       INTEGER NOT NULL REFERENCES prs(id),
  run_id      INTEGER NOT NULL REFERENCES runs(id),
  head_sha    TEXT NOT NULL,
  summary     TEXT,
  raw_output  TEXT,
  state       TEXT NOT NULL CHECK(state IN ('pending','posted','dismissed','failed')),
  created_at  DATETIME NOT NULL,
  posted_at   DATETIME,
  github_review_id INTEGER
);
CREATE INDEX idx_reviews_pr_state ON reviews(pr_id, state);

CREATE TABLE comments (
  id          INTEGER PRIMARY KEY,
  review_id   INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
  severity    TEXT NOT NULL CHECK(severity IN ('blocker','major','minor','nit')),
  path        TEXT NOT NULL,
  line        INTEGER NOT NULL,
  body        TEXT NOT NULL,
  selected    INTEGER NOT NULL DEFAULT 0,
  posted      INTEGER NOT NULL DEFAULT 0,
  github_id   INTEGER
);
CREATE INDEX idx_comments_review ON comments(review_id);

CREATE TABLE followups (
  id         INTEGER PRIMARY KEY,
  review_id  INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  line       INTEGER NOT NULL,
  status     TEXT NOT NULL CHECK(status IN ('addressed','outstanding','disputed')),
  note       TEXT,
  finding_id TEXT
);
CREATE INDEX idx_followups_review ON followups(review_id);
```

Schema is embedded via `//go:embed schema.sql` and applied with a version
table on startup.

## Re-review follow-ups

When a PR with a previously **posted** review receives new commits, the
fresh review must account for what was already said instead of
re-litigating it.

Pipeline (inside `reviewIfNeeded`, only on the re-review path — i.e. a
posted review exists for this PR at a different SHA):

1. Load the latest posted review + its posted comments from the DB.
2. Fetch the PR's review threads via GitHub GraphQL (`reviewThreads`:
   `isResolved`, `isOutdated`, comments with author/body/path/line).
3. Match each posted comment to its thread — exact body match first
   (bodies are posted verbatim), then `(path, line)` fallback.
4. Write a context file: prior findings + resolution state + author
   replies. Pass to the skill as
   `/pr-review-structured <url> --previous <file>`.
5. Thread-fetch failures degrade gracefully: log, review without context.

The skill classifies each prior finding and emits a `followups` array
(see skill SKILL.md for the contract):

- `addressed` — fixed by a code change or the thread is resolved
- `outstanding` — still applies; re-raised as a new finding whose body
  references the prior thread instead of duplicating it cold
- `disputed` — the author pushed back in replies; not re-raised unless
  blocker-severity

Cockpit persists followups with the review and renders them on the
detail page as a "Previous review" section above the findings.

---

## Review pipeline

### 1. Discover

```bash
gh pr list --search "<config.search>" --limit 50 \
  --json number,title,url,author,headRefOid,repository
```

Map each result to a `prs` row (upsert by `(owner, repo, number)`). Refresh
`head_sha` and `last_seen`.

### 2. Decide which to review

For each PR in the result set:

- If no existing review for `(pr_id, head_sha)`, queue it.
- If a `pending` review exists for an older `head_sha`, mark it `dismissed`
  (force-push invalidated the line anchors) and queue a fresh review.
- If a `posted` review already exists for the current SHA, skip.

### 3. Invoke Claude

```bash
claude -p --output-format text \
  --permission-mode acceptEdits \
  --max-turns 30 \
  "/pr-review-structured <PR_URL>"
```

The skill (see [Skill architecture](#skill-architecture)) emits a JSON object
as its final message, matching the documented schema. Parser scans stdout
from the end for the last `{ ... }` block and `json.Unmarshal`s into the
schema struct.

Parse failure or `{"error": "..."}` payload → mark the review `failed`,
store raw output for debugging, continue to the next PR.

Per-PR timeout from config. PRs reviewed by a worker pool of
`claude.concurrency` (default 3) within a run.

### 4. Persist

Insert one `reviews` row + N `comments` rows in a single transaction.
`comments.selected` defaults to `0`; user opts in from the UI.

---

## Scheduler

In-process. On startup:

1. If `run_on_launch`, fire one `trigger='launch'` run immediately in a
   goroutine.
2. Compute next tick: `time.AfterFunc(until_next_slot, tick)`.
3. `tick()` checks if current local time is within the window; if so, fires
   a `trigger='schedule'` run, then schedules the next tick at
   `now + interval_hours`. If outside the window, schedules the next tick at
   the next window opening.

Only one run executes at a time — guarded by a `sync.Mutex`. If a tick fires
while a run is in progress, it logs and skips.

---

## HTTP server

Binds `127.0.0.1` (config). Routes:

| Method | Path                  | Purpose                                            |
|--------|-----------------------|----------------------------------------------------|
| GET    | `/`                   | Dashboard: PRs with pending reviews + age + counts |
| GET    | `/pr/{id}`            | Latest review for PR: comments + submit form       |
| POST   | `/pr/{id}/submit`     | Post selected comments as a GitHub review          |
| POST   | `/review`             | Manual review: body `{url: "..."}` → kick off run  |
| GET    | `/runs`               | Run history + errors                               |
| POST   | `/run-now`            | Trigger a `trigger='schedule'` run immediately     |
| GET    | `/healthz`            | `200 ok`                                           |

### `POST /review` (manual review endpoint)

Body: `{"url": "https://github.com/owner/repo/pull/123"}`.

Behavior:

1. Normalize (strip query/fragment/suffix paths) and parse
   owner/repo/number. 400 if malformed — before a run slot is consumed.
2. Reserve the shared single-run guard (409 if a run is in progress).
3. Kick off ReviewOne in a goroutine (gh pr view → upsert → review
   pipeline), return `202 Accepted`. Same-SHA caching applies: an
   existing pending/posted review at the current head is served from
   the DB instead of spending an LLM run.
4. UI: URL input + "Review" button on `/`, next to Run now. Works even
   when `config.search` is empty. Progress appears in the same
   "Current run" dashboard section as discover runs (instead of the
   originally-specced redirect-and-poll detail page), and the page
   reloads when the run finishes.

Submit endpoint: body `{event: "APPROVE"|"REQUEST_CHANGES"|"COMMENT", selected: [comment_id, ...]}`.

Translates to:

```bash
gh api -X POST repos/{owner}/{repo}/pulls/{number}/reviews \
  -f event=COMMENT \
  -f body="<review.summary>" \
  --input - <<EOF
{
  "comments": [
    {"path": "foo.go", "line": 42, "body": "..."},
    ...
  ]
}
EOF
```

On success: mark `reviews.state='posted'`, store `github_review_id`, mark
selected comments `posted=1` with their returned IDs.

---

## UI

Server-rendered. Two templates: `dashboard.tmpl`, `pr_detail.tmpl`,
`runs.tmpl`. Tailwind via CDN.

### `/` dashboard

- Top bar: "Last run: 2h ago · success · 3 PRs reviewed" + `Run now` button
  + "Review a PR" URL input
- Table of PRs with `pending` reviews:
  `Title | Author | 🔴 / 🟠 / 🟡 / nit counts | Age | [Review →]`
- Below: collapsed "Recently posted" and "Dismissed" sections

### `/pr/{id}`

- Header: PR title, author, link, head SHA
- Summary block from review
- Comments grouped by severity. Each:
  - Checkbox (bound to `comments.selected`)
  - `path:line` with link to GitHub blob
  - Rendered markdown body
- Sticky footer:
  - Radio: Approve / Request changes / Comment
  - "Submit review" button → `POST /pr/{id}/submit`
  - "Dismiss this review" link

Selection state is debounced-saved via `PATCH /comments/{id}` so reloads
don't lose checkbox state.

### `/runs`

Table: trigger · started · duration · status · # PRs · error (if any).

---

## File layout

```
cockpit/
  go.mod
  main.go              # flag parsing, config load, wiring
  config.go            # config struct + load/save
  db.go                # open, migrate, query helpers
  schema.sql
  gh.go                # gh pr list / view / api wrappers
  claude.go            # claude shellout + JSON parse
  reviewer.go          # pipeline: discover → decide → invoke → persist
  scheduler.go         # ticker + window logic
  submitter.go         # build review payload, POST via gh api
  server.go            # http handlers + routing
  templates/
    layout.tmpl
    dashboard.tmpl
    pr_detail.tmpl
    runs.tmpl
  static/              # (optional) any local CSS overrides
```

Templates and schema.sql are `//go:embed`-ed.

---

## Build & run

```bash
go build -o cockpit ./...
./cockpit              # uses ~/.cockpit/config.json, creates if missing
./cockpit --addr :8765
```

Version: `cockpit version` → injected via ldflags, defaults to `dev`.

---

## Implementation order

Each step = one commit.

1. `go mod init` · embedded schema · `db.go` open + migrate · `config.go`
2. `gh.go` wrappers + tests with recorded fixtures
3. `claude.go` shellout + JSON parser + table-driven parse tests
4. `reviewer.go` pipeline orchestration (no scheduler yet) — verifiable via
   a `--run-once` CLI flag
5. `scheduler.go` ticker + window logic + `run_on_launch`
6. `server.go` dashboard route + templates
7. `/pr/{id}` detail + checkbox persistence
8. `submitter.go` + `POST /pr/{id}/submit`
9. `POST /review` manual endpoint + form on dashboard
10. `/runs` page + `POST /run-now`
11. `version` subcommand + GoReleaser config (optional)

---

## Open risks

- **`pr-review-structured` must exist first**: cockpit is blocked on the
  skill split. Build the skill and verify it emits clean JSON on a couple of
  real PRs before starting on cockpit step 3.
- **Output drift**: even with a dedicated skill, Claude may emit prose before
  the JSON object. Parser scans from the end for `{ ... }`. If drift is
  chronic, fall back to the Claude API directly with a tool-use response
  schema.
- **Line number staleness**: between the review and submission the PR may
  receive new commits. The submit handler re-checks `head_sha` against the
  review's stored SHA and refuses (with a "re-review now" link) if changed.
- **Search filter scope**: `gh` defaults to 30 results, we use `--limit 50`.
  If the filter regularly returns more, paginate.
- **`gh` auth**: detect missing auth at startup with `gh auth status` and
  fail loud.
