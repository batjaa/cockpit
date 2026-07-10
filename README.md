# cockpit

Local PR review pipeline. Periodically (or on demand) invokes the
`pr-review-structured` Claude skill against PRs matching a `gh` search
filter, stores findings in SQLite, and serves a web UI for picking which
comments to post.

Single Go binary. SQLite (pure-Go, no CGO). Tailwind vendored as a single
file. Shells out to `gh` and `claude`.

See [docs/pr-review-spec.md](./docs/pr-review-spec.md) for full design and roadmap.

## Quickstart

```bash
git clone https://github.com/batjaa/cockpit && cd cockpit
make install                     # build + install `cockpit` into $GOBIN
cockpit install-skill            # vendor the review skill into ~/.claude/skills
cockpit doctor                   # verify gh, claude, and the skill are ready
$EDITOR ~/.cockpit/config.json   # set "search" (created on first run; see Configure)
cockpit                          # serve the dashboard at http://127.0.0.1:8765
```

Ensure `$GOBIN` (usually `~/go/bin`) is on your `PATH`. That's it — everything
else (config, SQLite db) is created under `~/.cockpit` on first run.

## Prerequisites

| Tool | Purpose | Check |
|---|---|---|
| `go` 1.22+ | build | `go version` |
| `gh` | PR discovery + posting | `gh auth status` |
| `claude` CLI | invoke the review skill | `which claude` |
| `pr-review-structured` skill | review engine | `cockpit install-skill` (vendored in this repo) |

Run **`cockpit doctor`** to check all of the above at once, with fix-it hints
for anything missing.

## Build

```bash
make build      # -> ./cockpit, version stamped from `git describe`
make install    # -> $GOBIN/cockpit
make test       # go test ./...
make help       # list targets
```

Or without make: `go install github.com/batjaa/cockpit@latest` (version
shows as `dev` unless built with `-ldflags "-X main.version=..."`, which the
Makefile does for you).

The test suite stubs `gh` and `claude` via `PATH` injection, so it doesn't
hit the network.

## Configure

First run creates `~/.cockpit/config.json` with defaults:

```json
{
  "search": "",
  "schedule": { "start_hour": 6, "end_hour": 18, "interval_hours": 4, "run_on_launch": true },
  "claude":   { "binary": "claude", "timeout_seconds": 600, "concurrency": 3 },
  "http":     { "addr": "127.0.0.1:8765" },
  "sessions": {
    "enabled": true,
    "devbox_discovery": false,
    "remotes": [],
    "scan_claude": true, "scan_codex": true, "scan_cursor": true,
    "scan_interval_minutes": 20
  }
}
```

The scheduler fires discovery runs at local-time slots: every
`interval_hours` starting at `start_hour`, up to but excluding
`end_hour` — e.g. `start 6, end 18, interval 6` runs at 06:00 and
12:00 daily. `run_on_launch` additionally triggers one run at server
start. The scheduler only runs while the server is up; a slot that
fires mid-run is skipped (the next slot catches up, since reviews are
cached by SHA). The dashboard header shows the next armed slot.

Set `search` to a GitHub PR search query (same syntax as gh-dash, the
GitHub search bar, and `gh pr list --search`). Example:

```
repo:owner/repo is:pr is:open -is:draft author:alice author:bob
```

Requirements:

- Must include exactly one `repo:owner/name` qualifier. cockpit lifts it
  out and passes it as `--repo` to gh, because `gh pr list --search`
  refuses to run without a current-repo context when invoked outside a
  git checkout.
- Don't include a leading `filters: ` — that's a gh-dash TOML key, not
  part of the search query.

`author:`, `review-requested:`, and `team-review-requested:` are treated
as OR'd "fan-out" qualifiers: cockpit runs one search per term and unions
the results, so `author:alice author:bob team-review-requested:owner/team`
reviews Alice's PRs **and** Bob's PRs **and** the team's review requests
(GitHub would otherwise AND them into near-nothing). Everything else —
`is:open`, `-is:draft`, `-author:app/dependabot`, `label:`… — is shared
across every fan-out branch.

## Run

```bash
# Default: serve the web UI on http://127.0.0.1:8765
cockpit

# Review one PR by URL (skips the search filter)
cockpit --pr https://github.com/owner/repo/pull/123

# Discover via the configured search and review all matching PRs once
cockpit --run-once

# Scan agent sessions once, then exit
cockpit --scan-sessions
```

Setup / maintenance subcommands:

```bash
cockpit install-skill   # write the vendored review skill into ~/.claude/skills
cockpit doctor          # check gh / claude / skill are ready, with fix hints
cockpit version         # print the build version
```

Override the config path: `--config /some/path.json`.

The server also accepts a "Run now" button on the dashboard that triggers
the same flow as `--run-once`, with status polling and an in-memory guard
against concurrent runs. Next to it, a URL input reviews any single PR on
demand (`POST /review`) — independent of the search filter, so it works
even with an empty `search` config.

Reviews are cached by head SHA: if a PR already has a pending or posted
review at its current head, both discovery and manual reviews serve the
existing review instead of spending an LLM run. A fresh review happens
only when the head moves — and then a previously posted review feeds
the re-review follow-up context.

Discovery also skips PRs whose latest review by *you* (the gh user) is
APPROVED — the approval stands on GitHub even after new pushes, so
cockpit stops generating reviews for them and auto-dismisses any
pending ones. CHANGES_REQUESTED / COMMENTED keep the normal re-review
flow, since there you're waiting on the author and follow-ups matter.

## How a review happens

Per matching PR, in the order documented in
[docs/pr-review-spec.md](./docs/pr-review-spec.md):

1. `gh pr list --search <config.search>` (or `gh pr view <url>` for `--pr`).
2. Upsert into `prs` keyed on `(owner, repo, number)`.
3. Decision: skip if a pending/posted review already exists for this
   `head_sha`; otherwise dismiss any stale pending reviews (force-push)
   and proceed.
4. `claude -p "/pr-review-structured <url>"` — parses the trailing JSON
   from stdout, persists `reviews` row + N `comments` rows in a
   transaction with `state='pending'`.
5. Findings appear in the dashboard. On the detail page, check the
   comments to include, pick Comment / Approve / Request changes, and
   Submit — cockpit re-checks the PR head SHA first and refuses if the
   branch moved since the review (re-review instead). Everything posts
   as ONE GitHub review via `gh api`. Dismiss removes a review from the
   pending list without posting.

Reviews take 2–5 minutes per PR and run `claude.concurrency` at a time
(default 3).

Restart behavior: completed reviews are durable (one SQLite transaction
per PR). Ctrl-C kills in-flight claude processes and marks the run
`interrupted by shutdown`; PRs that didn't finish are picked up by the
next run, since the SHA-skip logic only honors pending/posted reviews.
Stale `running` runs from a hard kill are marked interrupted at startup.

## Sessions

`/sessions` indexes your coding-agent chat sessions (Claude Code, Codex,
Cursor) across this machine. It can also scan remote hosts over plain ssh
aliases (nothing installed remotely) — list them under `sessions.remotes`,
or set `sessions.devbox_discovery: true` to auto-discover hosts via
`devbox list` (off by default; requires an internal `devbox` CLI — not a
public tool).
Metadata only: titles, timestamps, message counts, extracted
ticket refs. Filter by agent/machine/ticket/search, copy a resume
command, archive what's done. Stale sessions (10+ messages, idle 1-14
days) surface on top and as a dashboard link. Scans run on their own
ticker (`sessions.scan_interval_minutes`, default 20), after every
discovery run, via the Scan button, or `cockpit --scan-sessions` —
incremental everywhere, including reading only changed rows from
Cursor's live WAL database.
Design: [docs/sessions-spec.md](./docs/sessions-spec.md).

## Inspect state

DB lives at `~/.cockpit/cockpit.db`:

```bash
sqlite3 ~/.cockpit/cockpit.db 'SELECT id, status, started_at FROM runs ORDER BY id DESC LIMIT 5;'
sqlite3 ~/.cockpit/cockpit.db 'SELECT id, pr_id, state, head_sha FROM reviews;'
sqlite3 ~/.cockpit/cockpit.db 'SELECT severity, path, line FROM comments WHERE review_id = 1;'
```

Tail the run log when the server is running with output redirection:

```bash
/tmp/cockpit > /tmp/cockpit.log 2>&1 &
tail -f /tmp/cockpit.log
```

## Stop the server

```bash
lsof -ti:8765 | xargs kill
```

Or `Ctrl-C` in the foreground terminal — `SIGINT`/`SIGTERM` triggers a
graceful shutdown.

## Layout

```
cockpit/
  main.go              flag parsing + subcommand dispatch
  server.go            HTTP server, handlers
  reviewer.go          Discover / ReviewOne / reconcile orchestration
  runqueue.go          single-worker run queue (discovery + manual reviews)
  gh.go                gh CLI wrappers (search fan-out, view, post)
  claude.go            claude shellout + JSON extraction
  skill.go             embedded review skill + `install-skill`
  doctor.go            dependency checks (`doctor` + startup preflight)
  queries.go           all SQL
  db.go                schema open + migrations + time format helper
  schema.sql           embedded
  templates/           html/template files
  static/tailwind.js   vendored Tailwind Play CDN
  skills/              vendored pr-review-structured skill (embedded)
  docs/                specs: pr-review (design + roadmap), sessions
  Makefile             build / install / test
  LICENSE              MIT
```
