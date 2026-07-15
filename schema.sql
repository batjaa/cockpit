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
  summary          TEXT,
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
