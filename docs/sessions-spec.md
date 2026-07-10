# Cockpit Sessions — Agent Session Tracking

Extends cockpit into an operational dashboard. First capability: a
unified view of coding-agent chat sessions (Claude Code, Codex, Cursor)
across local and remote machines, so "I know I was working on that
ticket somewhere" becomes a filter instead of an archaeology dig.

## Problem

Work happens in many agent sessions across tools and machines. Sessions
get abandoned mid-ticket and forgotten; finding them later means
grepping `~/.claude`, `~/.codex`, and Cursor's UI history by hand, per
machine.

## Goals

- One list: every session, every agent, every machine.
- Find by ticket: sessions grouped/filterable by JIRA key / GitHub ref.
- Stale-session radar: surface sessions that went quiet mid-work.
- Resume in one copy-paste: the exact command (or pointer) per agent.
- Metadata only: no transcript bodies leave their machines or enter the
  DB — titles, timestamps, counts, references.

## Non-goals

- Reading/searching full transcript content (phase 3 maybe).
- Writing to any agent's store. Strictly read-only.
- Real-time updates. Scanner cadence is the existing scheduler + a
  manual refresh button.

## Data sources (verified formats)

### Claude Code — local + remote

`~/.claude/projects/<project-slug>/<session-uuid>.jsonl`

- Project dir is the de-slugged directory name (`-Users-foo-git-bar` →
  `/Users/foo/git/bar`).
- Session id = filename. Last activity = file mtime.
- Title: first `"type":"user"` line's message content, skipping
  `<command-*>` wrappers and meta lines; truncated.
- Message count ≈ line count (cheap), refined lazily if needed.
- Resume: `cd <project> && claude --resume <session-id>`.

### Codex — local + remote

`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl`

- First line is `{"type":"session_meta","payload":{id, timestamp, cwd,
  cli_version, ...}}` — one `head -1` gives everything but the title.
- Title: first user message line. Last activity = file mtime.
- Resume: `codex resume <id>`.

### Cursor — local only

`~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`,
table `cursorDiskKV`, keys `composerData:<uuid>` (JSON values):

- `name` (human title Cursor already generated), `createdAt` /
  `lastUpdatedAt` (epoch ms), `subtitle` (last-message excerpt),
  `len(fullConversationHeadersOnly)` (message count).
- Open the DB read-only (`mode=ro&immutable=0`) on a copy if locked —
  Cursor holds it open; copy-to-temp on scan is the safe default.
- Cursor is a GUI app; sessions live where the GUI runs → no remote
  scan. No CLI resume either: the row shows "open in Cursor: <name>".
- **Fragility flag**: undocumented schema (`_v` currently 16). Parser
  must tolerate missing fields and version drift; failures degrade to
  skipping Cursor with a logged warning, never breaking the scan.

## Remote machines — devbox auto-discovery + SSH pull

An internal `devbox` CLI (not a public tool) manages the user's remote
machines and maintains SSH config aliases for each (`devbox`, `devbox-1`, …).
Cockpit leverages this instead of hand-maintained host lists:

1. **Discovery**: run `devbox list --no-probe`, parse the table (strip
   ANSI, split box-drawing columns) for NAME / STATE / ALIAS. Only
   `running` devboxes are scanned. Discovery failure (CLI missing, SSO
   expired) logs a warning and falls back to configured `remotes`.
2. **Transport**: plain `ssh <alias>` — the alias resolves through the
   CLI-managed SSH config, tunnels included. (`devbox exec` is NOT used:
   it passes the command as a single unintepreted argv, so pipelines
   break; plain ssh handles them fine.)
3. Devboxes are Linux, so GNU `find -printf` works; the `stat -f`
   fallback only matters for manually configured macOS remotes.
4. Future source, not MVP: `devbox agent sessions --json` returns
   receipts for agent sessions launched via `devbox agent` — zero-ssh
   metadata. Currently empty for this user; revisit when populated.

Manually configured `remotes` in config are scanned in addition (for
non-devbox machines). **Nothing installed remotely.** Claude and Codex
stores only.

One `ssh` invocation per host per store, batching extraction into a
single remote shell pipeline:

```bash
# claude: list files + first 4KB of each (title extraction happens locally)
ssh <host> 'find ~/.claude/projects -name "*.jsonl" -newer <stamp> \
  -printf "%p\t%T@\t%s\n" 2>/dev/null'
ssh <host> 'head -c 4096 <file1> <file2> ...'   # chunked into batches

# codex: session_meta is line 1
ssh <host> 'find ~/.codex/sessions -name "rollout-*.jsonl" \
  -printf "%p\t%T@\n" -exec head -1 {} \;'
```

- macOS remotes lack GNU `-printf`: fall back to `stat -f`. Detect once
  per host, cache.
- Host failures (down, no route) log a warning and skip — never block
  the scan or other hosts.
- Incremental: per (host, store) high-water mtime stored in DB; only
  files newer than it are fetched.

## Schema

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id            INTEGER PRIMARY KEY,
  agent         TEXT NOT NULL CHECK(agent IN ('claude','codex','cursor')),
  machine       TEXT NOT NULL,             -- 'local' or remote host name
  session_key   TEXT NOT NULL,             -- agent-native id
  project_dir   TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL DEFAULT '',
  subtitle      TEXT NOT NULL DEFAULT '',  -- last-message excerpt where available
  started_at    DATETIME,
  last_active   DATETIME NOT NULL,
  message_count INTEGER NOT NULL DEFAULT 0,
  resume_cmd    TEXT NOT NULL DEFAULT '',
  archived      INTEGER NOT NULL DEFAULT 0, -- user hid it from the list
  UNIQUE(agent, machine, session_key)
);
CREATE INDEX IF NOT EXISTS idx_sessions_active ON sessions(last_active DESC);

CREATE TABLE IF NOT EXISTS session_tickets (
  session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ticket     TEXT NOT NULL,                -- 'PLAT-422' | 'org/repo#123'
  UNIQUE(session_id, ticket)
);

CREATE TABLE IF NOT EXISTS scan_state (
  source     TEXT PRIMARY KEY,             -- 'local:claude', 'devbox1:codex', ...
  high_water DATETIME NOT NULL
);
```

## Ticket extraction

From title + the first ~4KB of the transcript:

- JIRA-style keys: `\b[A-Z][A-Z0-9]{1,9}-\d+\b`
- GitHub refs: `github.com/<o>/<r>/(pull|issues)/\d+` → `o/r#N`
- Branch-style refs in the cwd's git branch if cheaply available
  (local only, `git -C <dir> branch --show-current`); skipped on remote.

Stored normalized in `session_tickets`; a session can have several.

## Staleness

`stale` = `last_active` between 24h and 14d ago AND `message_count >= 10`
(enough activity to be real work) AND not archived. Computed at query
time — no stored state. Tunable later via config if the heuristic
annoys.

## Config

```json
"sessions": {
  "enabled": true,
  "devbox_discovery": true,
  "remotes": [
    {"name": "other-box", "host": "user@host.example.com"}
  ],
  "scan_claude": true,
  "scan_codex": true,
  "scan_cursor": true
}
```

`devbox_discovery` runs `devbox list` each scan and unions the running
devboxes with the static `remotes` list.

`applyDefaults` backfills `enabled: true` with empty remotes for
existing config files.

## Scanner orchestration

- `ScanSessions(ctx, db, cfg)` runs: local claude, local codex, local
  cursor, then each remote × {claude, codex} — each source independent;
  one failing logs and continues.
- Triggered by: the existing scheduler (after each discovery run), a
  `POST /sessions/scan` endpoint (refresh button), and `--scan-sessions`
  CLI flag for cron/manual use.
- Shares nothing with the review pipeline; no claude/LLM involvement.

## UI

New page `GET /sessions` (nav link next to Dashboard):

- **Stale section** on top: "🕳 N sessions went quiet mid-work" with
  agent icon, machine chip, title, ticket chips, idle duration,
  resume command (click-to-copy), archive button.
- **Main list**: all non-archived sessions, newest activity first.
  Filter chips: agent, machine, project; text search over title +
  ticket. Ticket chips link to a filtered view (`?ticket=PLAT-422`).
- **Row anatomy**:
  `[claude] [devbox] PLAT-422 · "Fix flaky retry test" · ~/git/backend · 2h ago · 47 msgs · [copy resume] [archive]`
- Archive = hide (sets `archived=1`); the underlying agent store is
  never touched.
- Dashboard header gains a small link: "N stale sessions →" when any.

## Endpoints

| Method | Path                    | Purpose                          |
|--------|-------------------------|----------------------------------|
| GET    | `/sessions`             | The page (filters via query)     |
| POST   | `/sessions/scan`        | Kick a scan (202; reuses run-progress pattern, lighter) |
| PATCH  | `/sessions/{id}`        | `{archived: bool}`               |

## Implementation order

1. Schema + config + `applyDefaults`.
2. Local claude scanner + tests (fixture jsonl dirs).
3. Local codex scanner + tests.
4. Local cursor scanner (copy-to-temp, tolerant parser) + tests
   (fixture sqlite built in test).
5. Ticket extraction + tests.
6. `/sessions` page + filters + archive + copy-resume.
7. SSH pull (claude + codex) + tests (stub `ssh` binary on PATH, same
   pattern as the gh/claude stubs).
8. Scheduler + refresh wiring; stale link on dashboard.

## Risks

- **Cursor schema drift**: `_v` bumps may rename fields. Parser treats
  every field as optional; a parse failure skips Cursor and logs.
- **SSH latency**: a slow host stalls its own goroutine only; per-host
  timeout (15s) and parallel host scans.
- **Claude title extraction**: first-line heuristics on jsonl vary by
  version; tolerate unknown line types, fall back to filename.
- **state.vscdb size** (>100MB with agentKv blobs): query only
  `composerData:` keys; never SELECT *.
