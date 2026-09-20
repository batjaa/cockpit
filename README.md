# cockpit

Local PR review pipeline. Periodically (or on demand) invokes the
`pr-review-structured` Claude skill against PRs matching a `gh` search
filter, stores findings in SQLite, and serves a web UI for picking which
comments to post. Each review includes a private structured risk brief for the
reviewer and, only when warranted, a separate author-facing message.

The **Map** page organizes outcome-oriented workstreams with local tasks,
references, cross-team asks, manual signals, decisions, and a one-way Markdown
archive. Map works without provider or model access; the PR review pipeline
continues to use `gh` and `claude`.

Single Go binary. SQLite (pure-Go, no CGO). Server-rendered HTML and Tailwind
vendored as a single file.

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
| `go` 1.25+ | build | `go version` |
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
  "claude":   { "binary": "claude", "model": "sonnet", "timeout_seconds": 600, "concurrency": 3, "skill": "pr-review-structured" },
  "review": {
    "skip_authors": ["app/dependabot", "dependabot[bot]"],
    "exclude_paths": ["*.pb.go", "vendor/", "go.sum", "..."]
  },
  "http":     { "addr": "127.0.0.1:8765" },
  "sessions": {
    "enabled": true,
    "devbox_discovery": false,
    "remotes": [],
    "scan_claude": true, "scan_codex": true, "scan_cursor": true,
    "scan_interval_minutes": 20
  },
  "workstreams": {
    "timezone": "",
    "mirror": {}
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

`review.skip_authors` keeps matching PRs in discovery and on the dashboard,
but marks them skipped without invoking Claude. Matching is case-insensitive;
the defaults cover GitHub's Dependabot app and bot logins. Add more GitHub
logins to extend the policy to other automation, or set an explicit `[]` to
review every author.

`review.exclude_paths` keeps findings on generated code out of reviews:
any finding whose path matches a pattern is dropped before it's persisted
(the review skill is also instructed to skip generated files, but this
filter is the deterministic backstop). A pattern glob-matches the full
repo-relative path or any suffix at a path-segment boundary — so
`feature_flags.go` matches `svc/internal/feature_flags.go` — and a
trailing `/` matches a directory segment anywhere (`vendor/`). The
default list covers protobuf output, `*_gen.go`/`zz_generated*`, mocks,
vendored deps, lockfiles, minified assets, and test snapshots; setting
the field to an explicit `[]` disables exclusion.

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
cockpit install-skill              # install the default review skill
cockpit install-skill --as my-fork # fork it under a custom slug you can edit
cockpit doctor                     # check gh / claude / skill are ready
cockpit version                    # print the build version
```

Override the config path: `--config /some/path.json`.

This changes the configuration file, **not** the database location. The normal
binary always uses `~/.cockpit/cockpit.db`. Use the isolated preview described
below for browser acceptance instead of pointing a test at personal data.

## Workstreams and Map

Open **Map** in the navigation, or visit `/map`. The existing PR dashboard
remains the default page. Create a workstream with a name and desired outcome,
then add local tasks, labelled source references, asks, manual observations,
and decisions. A cached PR reference is context; a separate review/merge/ship
task represents an obligation. A source merge never completes that task for you.

The overview, static graph, timeline, and Obsidian tabs share the same records.
Attention explains blocked/overdue tasks, overdue asks, concerning signals, and
missed target dates. Missing or stale data and mirror failures are separate
warnings, not a claim about the workstream's health. Completion is an explicit
confirmed action with an outcome note. Detachment, archive, and completion keep
history and can be reversed through the appropriate restore/reopen action.

Map does not ingest Gmail, Slack, Calendar, Zoom, or other providers. Attached
URLs are saved without fetching previews. Inbound, staff-radar, calendar, and
AI-drafting surfaces are clearly unavailable in this module.

`workstreams.timezone` accepts an IANA timezone such as
`America/Los_Angeles`; empty uses the machine timezone. Task/target dates become
overdue on the following local day. Follow-up and observation times are stored
as UTC instants and displayed locally.

The mirror defaults to `~/cockpit-vault`. To select a different existing or new
directory, set an absolute path:

```json
"workstreams": {
  "timezone": "America/Los_Angeles",
  "mirror": { "enabled": true, "root": "/absolute/path/to/cockpit-vault" }
}
```

Set `"enabled": false` to disable filesystem exports while continuing to save
workstreams in SQLite. Missing `enabled` means enabled, including older configs.
The vault groups stable-ID Markdown files by entity kind; renaming a workstream
does not change its identity or filename.

**Saved in Cockpit** and **synced to vault** are different outcomes. Pending
exports survive restart; a failed export does not roll back a local save. An
externally changed file or an unexpected existing path produces a conflict.
Retry never force-overwrites it: reconcile/restore the generated file, or move
the conflicting file aside, then retry. Editing Markdown does not import changes
into Cockpit. Avoid simultaneous editing of generated files while exports run;
the local filesystem does not provide an atomic compare-and-swap protocol with
other applications. Symlink targets and unrelated notes are not managed.

The implementation keeps domain mutations, semantic history, and pending mirror
revisions in one SQLite transaction. A single background worker performs bounded
retries and atomic file writes after commit. Map reads are local and side-effect
free. The module's contracts and acceptance requirements are in
[the workstream spec](docs/specs/workstreams-map.md).

### Isolated browser preview

The opt-in test harness runs the actual embedded application with a separate
database and vault, with all discovery/review/session jobs disabled:

```bash
preview_dir=$(mktemp -d /tmp/cockpit-preview.XXXXXX)
go test -c -o "$preview_dir/cockpit-preview.test"
COCKPIT_PREVIEW_DIR="$preview_dir" "$preview_dir/cockpit-preview.test" \
  -test.run '^TestMapPreview$' -test.timeout 0 -test.v
```

It listens on `http://127.0.0.1:8766` by default; `COCKPIT_PREVIEW_ADDR` selects
another localhost port. Stop it with Ctrl-C. The directory remains available
for inspecting test data and Markdown; no real user vault is touched. Ordinary
`go test ./...` skips this opt-in long-running harness.

For the optional browser acceptance walkthrough, start the preview with
`COCKPIT_PREVIEW_SEED=1` to add local cached-PR fixtures (no provider requests).
Use a temporary Playwright installation and an existing Chrome executable:

```bash
browser_tools_dir=$(mktemp -d /tmp/cockpit-browser.XXXXXX)
npm install --prefix "$browser_tools_dir" playwright
COCKPIT_PLAYWRIGHT_MODULE="$browser_tools_dir/node_modules/playwright/index.mjs" \
  node scripts/verify-map.mjs
```

`COCKPIT_CHROME` overrides the default macOS Chrome path;
`COCKPIT_MAP_BASE_URL` selects another loopback preview and
`COCKPIT_MAP_EVIDENCE` selects the screenshot directory. Run this only against
the isolated, seeded preview: it deliberately creates acceptance fixtures.
The runner blocks external requests and checks all four item kinds, cached PR
selection, conflicts, lifecycle, movement, keyboard controls, attention
resolution, and maximum-length layouts at 375/768/1440px. Playwright is not a
production dependency.

## PR review workflow

The server also accepts a "Run now" button on the dashboard that triggers
the same flow as `--run-once`, with status polling and a serial in-memory
queue. Next to it, a URL input reviews any single PR on
demand (`POST /review`) — independent of the search filter, so it works
even with an empty `search` config. Failed review banners include a
**Retry failed** action that requeues the affected PRs through the same serial
worker; failed rows never block a same-SHA retry.

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
   from stdout, persists the private `review_brief`, optional public
   `author_message`, and N inline findings in one transaction with
   `state='pending'`.
5. The dashboard and detail page lead with the private brief: change intent,
   mechanism, risk, complex areas, boundary changes, blast radius, validation,
   uncertainties, and recommendation. This data never enters a GitHub payload.
6. Check the inline comments to include, edit or explicitly add an author
   message, choose Comment / Approve / Request changes, and Submit. Cockpit
   re-checks the PR head SHA first. If the branch moved, Cockpit removes the
   stale review before anything posts and offers a one-click re-review of the
   latest commit. Everything public posts as one GitHub review via `gh api`;
   Dismiss posts nothing.

The default skill generates `author_message` only for an actionable
cross-cutting concern. Positive feedback and localized inline findings do not
qualify. A bare approval may omit the message; GitHub requires a body for
Comment and Request changes, so Cockpit requires an author message for those
events. See the [GitHub review API](https://docs.github.com/en/rest/pulls/reviews?apiVersion=2026-03-10).

Reviews take 2–5 minutes per PR and run `claude.concurrency` at a time
(default 3).

Cockpit does not pass Claude's optional `--max-turns` flag. The old hard-coded
value of 30 was a per-invocation agent-turn ceiling—not a daily quota—and could
stop complex reviews prematurely. Reviews remain bounded by
`claude.timeout_seconds` (default 600 seconds). See Anthropic's
[`--max-turns` CLI documentation](https://docs.anthropic.com/en/docs/claude-code/cli-usage).

Cockpit defaults to Claude Sonnet for reviews, independently of the model
configured for interactive Claude Code sessions. Set `claude.model` to a
different CLI model alias or full model name when a review needs it.

Restart behavior: completed reviews are durable (one SQLite transaction
per PR). Ctrl-C kills in-flight claude processes and marks the run
`interrupted by shutdown`; PRs that didn't finish are picked up by the
next run, since the SHA-skip logic only honors pending/posted reviews.
Stale `running` runs from a hard kill are marked interrupted at startup.

## Custom review skills

The reviewer is a Claude skill, so it's swappable. Cockpit ships a default
(`pr-review-structured`, vendored under `skills/` and installed via
`cockpit install-skill`), but you can point it at any skill under
`~/.claude/skills` that follows the output contract.

Fork the template under your own slug, edit it, wire it up:

```bash
cockpit install-skill --as my-review   # writes ~/.claude/skills/my-review/SKILL.md
$EDITOR ~/.claude/skills/my-review/SKILL.md
# set in ~/.cockpit/config.json:
#   "claude": { "skill": "my-review" }
cockpit doctor                         # confirms cockpit sees your skill
```

You can change how the skill reviews (framework, tone, additional
checks, per-team house rules) freely, but you **must** preserve the JSON
output contract documented at the bottom of `SKILL.md`. Cockpit invokes
`claude -p "/<skill> <pr-url> [--previous <path>]"` and parses the
trailing JSON object for these fields:

- `pr` (owner/repo/number/title/author/head_sha)
- `review_brief` — required private analysis with change, risk, complexity,
  boundaries, blast radius, validation, uncertainty, recommendation, and
  high-level concerns; never posted
- `author_message` — string or `null`; generated only when
  `review_brief.high_level_concerns` is non-empty and the only generated
  top-level prose eligible for posting
- `verdict` — `approve` / `approve-with-suggestions` / `request-changes`
- `findings[]` — id/severity/perfect/path/line/original_line/body
- `positives[]`
- `followups[]` — only when `--previous` is passed (re-review context)

If a custom skill breaks the schema, the review fails with a parse error
and cockpit records the raw output for debugging. During the compatibility
window, custom skills that emit the old `summary` field still work; Cockpit
labels their content as legacy author-facing text and never treats it as a
private brief.

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
