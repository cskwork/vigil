package store

// Schema follows PRD §17 (minimal persistent schema). Timestamps are unix
// milliseconds (UTC) so range queries stay index-friendly in SQLite.
const schema = `
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  base_url TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS repositories (
  project_id TEXT NOT NULL,
  path TEXT NOT NULL,
  branch TEXT NOT NULL,
  last_seen_sha TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project_id, path)
);

CREATE TABLE IF NOT EXISTS features (
  id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  status TEXT NOT NULL,
  latest_shipped_sha TEXT NOT NULL,
  shipped_at INTEGER NOT NULL,
  changed_paths TEXT NOT NULL DEFAULT '[]',
  routes TEXT NOT NULL DEFAULT '[]',
  summary TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  readiness TEXT NOT NULL DEFAULT 'WAITING_FOR_DEPLOYMENT',
  ready_at INTEGER,
  last_handled_sha TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, id)
);
CREATE INDEX IF NOT EXISTS ix_features_sha ON features(project_id, latest_shipped_sha);

CREATE TABLE IF NOT EXISTS scenarios (
  id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  state TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  class TEXT NOT NULL DEFAULT 'P1',
  mutation TEXT NOT NULL DEFAULT 'read-only',
  locks TEXT NOT NULL DEFAULT '[]',
  current_version INTEGER NOT NULL DEFAULT 1,
  oracle_source TEXT NOT NULL DEFAULT '',
  oracle_feature TEXT NOT NULL DEFAULT '',
  oracle_sha TEXT NOT NULL DEFAULT '',
  soak_passes INTEGER NOT NULL DEFAULT 0,
  soak_target INTEGER NOT NULL DEFAULT 3,
  last_outcome TEXT NOT NULL DEFAULT '',
  last_run_at INTEGER,
  last_pass_at INTEGER,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  next_due_at INTEGER,
  origin TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, id)
);
CREATE INDEX IF NOT EXISTS ix_scenarios_state ON scenarios(project_id, state);
CREATE INDEX IF NOT EXISTS ix_scenarios_fp ON scenarios(fingerprint);
CREATE INDEX IF NOT EXISTS ix_scenarios_due ON scenarios(project_id, state, next_due_at);

CREATE TABLE IF NOT EXISTS scenario_versions (
  project_id TEXT NOT NULL,
  scenario_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  yaml TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  created_by TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, scenario_id, version)
);

CREATE TABLE IF NOT EXISTS scenario_variants (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  scenario_id TEXT NOT NULL,
  name TEXT NOT NULL,
  params TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS flows (
  id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  current_version INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, id)
);

CREATE TABLE IF NOT EXISTS flow_versions (
  project_id TEXT NOT NULL,
  flow_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  yaml TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project_id, flow_id, version)
);

CREATE TABLE IF NOT EXISTS coverage_links (
  project_id TEXT NOT NULL,
  scenario_id TEXT NOT NULL,
  link_type TEXT NOT NULL,
  link_value TEXT NOT NULL,
  PRIMARY KEY (project_id, scenario_id, link_type, link_value)
);
CREATE INDEX IF NOT EXISTS ix_links_value ON coverage_links(link_type, link_value);

CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  priority INTEGER NOT NULL,
  scheduled_at INTEGER NOT NULL,
  scenario_id TEXT NOT NULL DEFAULT '',
  feature_id TEXT NOT NULL DEFAULT '',
  browser TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL DEFAULT '{}',
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 2,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_expires_at INTEGER,
  last_error TEXT NOT NULL DEFAULT '',
  dedup_key TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_jobs_ready ON jobs(state, scheduled_at, priority);
CREATE INDEX IF NOT EXISTS ix_jobs_lease ON jobs(lease_expires_at);
CREATE INDEX IF NOT EXISTS ix_jobs_dedup ON jobs(project_id, dedup_key, state);

CREATE TABLE IF NOT EXISTS job_attempts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL,
  attempt INTEGER NOT NULL,
  worker_id TEXT NOT NULL,
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL DEFAULT 0,
  project_id TEXT NOT NULL,
  scenario_id TEXT NOT NULL,
  scenario_version INTEGER NOT NULL DEFAULT 0,
  feature_id TEXT NOT NULL DEFAULT '',
  shipped_sha TEXT NOT NULL DEFAULT '',
  browser TEXT NOT NULL,
  outcome TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  started_at INTEGER NOT NULL,
  finished_at INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  failed_step INTEGER NOT NULL DEFAULT 0,
  failed_action TEXT NOT NULL DEFAULT '',
  expected TEXT NOT NULL DEFAULT '',
  actual TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  evidence_dir TEXT NOT NULL DEFAULT '',
  deploy_marker TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_runs_scenario ON runs(scenario_id, started_at);
CREATE INDEX IF NOT EXISTS ix_runs_outcome ON runs(project_id, outcome, started_at);

CREATE TABLE IF NOT EXISTS run_artifacts (
  run_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  path TEXT NOT NULL,
  PRIMARY KEY (run_id, kind, path)
);

CREATE TABLE IF NOT EXISTS incidents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  scenario_id TEXT NOT NULL DEFAULT '',
  scenario_version INTEGER NOT NULL DEFAULT 0,
  feature_id TEXT NOT NULL DEFAULT '',
  shipped_sha TEXT NOT NULL DEFAULT '',
  run_id INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'OPEN',
  markdown_path TEXT NOT NULL DEFAULT '',
  json_path TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  resolved_at INTEGER
);
CREATE INDEX IF NOT EXISTS ix_incidents_open ON incidents(project_id, state, kind);

CREATE TABLE IF NOT EXISTS workers (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  last_heartbeat INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS scheduler_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS scenario_metrics (
  scenario_id TEXT PRIMARY KEY,
  runs INTEGER NOT NULL DEFAULT 0,
  passes INTEGER NOT NULL DEFAULT 0,
  failures INTEGER NOT NULL DEFAULT 0,
  flakes INTEGER NOT NULL DEFAULT 0,
  regressions_caught INTEGER NOT NULL DEFAULT 0,
  total_duration_ms INTEGER NOT NULL DEFAULT 0,
  last_verified_at INTEGER
);

CREATE TABLE IF NOT EXISTS resource_locks (
  lock_key TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS budget_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL,          -- browser | chromium | agent
  amount INTEGER NOT NULL,     -- minutes*1000 or task count
  at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_budget ON budget_events(project_id, kind, at);
`
