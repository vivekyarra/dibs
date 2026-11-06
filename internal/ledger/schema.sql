-- Dibs ledger, schema version 1. Every decision Dibs makes is derived from
-- these tables, and every change is appended to `events`.
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_levels (
  handle            TEXT NOT NULL,
  skill             TEXT NOT NULL,
  level             INTEGER NOT NULL DEFAULT 0 CHECK (level BETWEEN 0 AND 4),
  clean_since_level INTEGER NOT NULL DEFAULT 0 CHECK (clean_since_level >= 0),
  updated_at        TEXT NOT NULL,
  PRIMARY KEY (handle, skill)
);

CREATE TABLE IF NOT EXISTS tickets (
  number           INTEGER PRIMARY KEY,
  title            TEXT NOT NULL,
  url              TEXT NOT NULL DEFAULT '',
  body             TEXT NOT NULL DEFAULT '',
  state            TEXT NOT NULL,
  reserved_for     TEXT NOT NULL DEFAULT '',
  skills           TEXT NOT NULL DEFAULT '[]',
  touched_paths    TEXT NOT NULL DEFAULT '[]',
  est_senior_hours REAL NOT NULL DEFAULT 0,
  est_junior_hours REAL NOT NULL DEFAULT 0,
  due_at           TEXT NOT NULL,
  score            REAL NOT NULL DEFAULT 0,
  score_breakdown  TEXT NOT NULL DEFAULT '{}',
  reason           TEXT NOT NULL DEFAULT '',
  triaged_at       TEXT NOT NULL DEFAULT '',
  proposed_at      TEXT NOT NULL DEFAULT '',
  started_at       TEXT NOT NULL DEFAULT '',
  stuck_asked_at   TEXT NOT NULL DEFAULT '',
  keep_going_until TEXT NOT NULL DEFAULT '',
  pr_number        INTEGER NOT NULL DEFAULT 0,
  pr_url           TEXT NOT NULL DEFAULT '',
  pr_author        TEXT NOT NULL DEFAULT '',
  branch           TEXT NOT NULL DEFAULT '',
  max_hint_level   INTEGER NOT NULL DEFAULT 0,
  review_rounds    INTEGER NOT NULL DEFAULT 0,
  attempts         INTEGER NOT NULL DEFAULT 0,
  senior_unlocked  INTEGER NOT NULL DEFAULT 0,
  approvals        INTEGER NOT NULL DEFAULT 0,
  senior_comments  INTEGER NOT NULL DEFAULT 0,
  teaching         INTEGER NOT NULL DEFAULT 0,
  was_released     INTEGER NOT NULL DEFAULT 0,
  released_reason  TEXT NOT NULL DEFAULT '',
  released_for     TEXT NOT NULL DEFAULT '',
  merged_at        TEXT NOT NULL DEFAULT '',
  sample        INTEGER NOT NULL DEFAULT 0,
  updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tickets_state ON tickets(state);

CREATE TABLE IF NOT EXISTS events (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  at     TEXT NOT NULL,
  ticket INTEGER NOT NULL DEFAULT 0,
  actor  TEXT NOT NULL,
  kind   TEXT NOT NULL,
  data   TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_ticket ON events(ticket, id);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind, id);
