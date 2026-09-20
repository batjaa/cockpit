CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS runs (
  id          INTEGER PRIMARY KEY,
  trigger     TEXT NOT NULL CHECK(trigger IN ('schedule','launch','manual')),
  started_at  DATETIME NOT NULL,
  finished_at DATETIME,
  status      TEXT NOT NULL CHECK(status IN ('running','success','partial','error')),
  error       TEXT
);

CREATE TABLE IF NOT EXISTS prs (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  repo       TEXT NOT NULL,
  number     INTEGER NOT NULL,
  url        TEXT NOT NULL,
  title      TEXT NOT NULL,
  author     TEXT NOT NULL,
  head_sha   TEXT NOT NULL,
  state      TEXT NOT NULL DEFAULT 'OPEN',
  review_action TEXT NOT NULL DEFAULT 'review', -- review | skip
  review_skip_reason TEXT NOT NULL DEFAULT '',  -- stable policy reason code
  pr_created_at DATETIME, -- PR opened time (GitHub); null until first scanned
  pr_updated_at DATETIME, -- PR last-activity time (GitHub)
  first_seen DATETIME NOT NULL,
  last_seen  DATETIME NOT NULL,
  UNIQUE(owner, repo, number)
);

CREATE TABLE IF NOT EXISTS reviews (
  id               INTEGER PRIMARY KEY,
  pr_id            INTEGER NOT NULL REFERENCES prs(id),
  run_id           INTEGER NOT NULL REFERENCES runs(id),
  head_sha         TEXT NOT NULL,
  summary          TEXT, -- legacy v1 author-facing review body
  review_brief     TEXT NOT NULL DEFAULT '', -- private structured JSON; never posted
  author_message   TEXT NOT NULL DEFAULT '', -- optional v2 author-facing review body
  raw_output       TEXT,
  state            TEXT NOT NULL CHECK(state IN ('pending','posted','dismissed','failed')),
  created_at       DATETIME NOT NULL,
  posted_at        DATETIME,
  github_review_id INTEGER
);
CREATE INDEX IF NOT EXISTS idx_reviews_pr_state ON reviews(pr_id, state);

CREATE TABLE IF NOT EXISTS comments (
  id        INTEGER PRIMARY KEY,
  review_id INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
  severity  TEXT NOT NULL CHECK(severity IN ('blocker','major','minor','nit')),
  path      TEXT NOT NULL,
  line      INTEGER NOT NULL,
  body      TEXT NOT NULL,
  diff_hunk TEXT,                          -- code area the finding refers to, captured at review time
  selected  INTEGER NOT NULL DEFAULT 0,
  posted    INTEGER NOT NULL DEFAULT 0,
  github_id INTEGER
);
CREATE INDEX IF NOT EXISTS idx_comments_review ON comments(review_id);

CREATE TABLE IF NOT EXISTS followups (
  id         INTEGER PRIMARY KEY,
  review_id  INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  line       INTEGER NOT NULL,
  status     TEXT NOT NULL CHECK(status IN ('addressed','outstanding','disputed')),
  note       TEXT,
  finding_id TEXT
);
CREATE INDEX IF NOT EXISTS idx_followups_review ON followups(review_id);

CREATE TABLE IF NOT EXISTS sessions (
  id            INTEGER PRIMARY KEY,
  agent         TEXT NOT NULL CHECK(agent IN ('claude','codex','cursor')),
  machine       TEXT NOT NULL,             -- 'local' or remote host name
  session_key   TEXT NOT NULL,             -- agent-native id
  project_dir   TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL DEFAULT '',
  subtitle      TEXT NOT NULL DEFAULT '',  -- last-message excerpt where available
  branch        TEXT NOT NULL DEFAULT '',  -- git branch the session worked on
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

-- Map/workstreams are wholly local. IDs are random opaque strings rather
-- than titles so renames, moves, and vault paths preserve identity.
CREATE TABLE IF NOT EXISTS workstreams (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  outcome TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT 'me',
  sponsor TEXT NOT NULL DEFAULT '',
  target_date TEXT NOT NULL DEFAULT '', -- YYYY-MM-DD, interpreted in configured location
  notes TEXT NOT NULL DEFAULT '',
  lifecycle TEXT NOT NULL DEFAULT 'active' CHECK(lifecycle IN ('active','completed')),
  archived INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)),
  archived_at DATETIME,
  completion_note TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workstreams_list ON workstreams(archived, lifecycle, name, id);
CREATE INDEX IF NOT EXISTS idx_workstreams_target ON workstreams(target_date);

CREATE TABLE IF NOT EXISTS sources (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  url TEXT NOT NULL,
  canonical_id TEXT NOT NULL UNIQUE,
  label TEXT NOT NULL DEFAULT '',
  pr_id INTEGER REFERENCES prs(id),
  revision INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sources_pr ON sources(pr_id);

CREATE TABLE IF NOT EXISTS workstream_items (
  id TEXT PRIMARY KEY,
  workstream_id TEXT REFERENCES workstreams(id),
  kind TEXT NOT NULL CHECK(kind IN ('task','reference','ask','signal')),
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  source_id TEXT REFERENCES sources(id),
  due_date TEXT NOT NULL DEFAULT '',
  tracking_state TEXT NOT NULL DEFAULT '' CHECK(tracking_state IN ('','open','in_progress','blocked','done','cancelled')),
  blocker_reason TEXT NOT NULL DEFAULT '',
  counterpart TEXT NOT NULL DEFAULT '',
  ask_status TEXT NOT NULL DEFAULT '' CHECK(ask_status IN ('','open','waiting','resolved','cancelled')),
  follow_up_at DATETIME,
  last_contact_at DATETIME,
  value TEXT NOT NULL DEFAULT '',
  unit TEXT NOT NULL DEFAULT '',
  observed_at DATETIME,
  assessment TEXT NOT NULL DEFAULT '' CHECK(assessment IN ('','normal','concerning','unknown')),
  review_by DATETIME,
  detached_at DATETIME,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_parent ON workstream_items(workstream_id, detached_at, kind, id);
CREATE INDEX IF NOT EXISTS idx_items_due ON workstream_items(due_date, tracking_state, detached_at);
CREATE INDEX IF NOT EXISTS idx_items_followup ON workstream_items(follow_up_at, ask_status, detached_at);
CREATE INDEX IF NOT EXISTS idx_items_source ON workstream_items(source_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_active_reference_source
  ON workstream_items(workstream_id, source_id)
  WHERE kind='reference' AND detached_at IS NULL AND source_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS decisions (
  id TEXT PRIMARY KEY,
  workstream_id TEXT NOT NULL REFERENCES workstreams(id),
  item_id TEXT REFERENCES workstream_items(id),
  source_id TEXT REFERENCES sources(id),
  note TEXT NOT NULL,
  revision INTEGER NOT NULL DEFAULT 1,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_decisions_parent ON decisions(workstream_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS timeline_events (
  id TEXT PRIMARY KEY,
  workstream_id TEXT NOT NULL REFERENCES workstreams(id),
  entity_type TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  description TEXT NOT NULL,
  revision INTEGER NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_timeline_parent ON timeline_events(workstream_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS mirror_states (
  entity_type TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  desired_revision INTEGER NOT NULL,
  last_written_revision INTEGER NOT NULL DEFAULT 0,
  last_success_revision INTEGER NOT NULL DEFAULT 0,
  last_checksum TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','synced','error','conflict')),
  last_attempt_at DATETIME,
  last_success_at DATETIME,
  error TEXT NOT NULL DEFAULT '',
  attempt_count INTEGER NOT NULL DEFAULT 0,
  next_attempt_at DATETIME,
  last_operational_state TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(entity_type, entity_id)
);
-- The retry index is installed by OpenDB after additive column migrations.

CREATE TABLE IF NOT EXISTS operation_receipts (
  operation_id TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  workstream_id TEXT NOT NULL DEFAULT '',
  item_id TEXT NOT NULL DEFAULT '',
  decision_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL
);
