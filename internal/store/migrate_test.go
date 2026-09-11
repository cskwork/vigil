package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"vigil/internal/model"
)

// oldRunsSchema is the runs table as shipped before deploy_marker / environment existed.
const oldRunsSchema = `CREATE TABLE runs (
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
  evidence_dir TEXT NOT NULL DEFAULT ''
);`

func TestOpenMigratesOldRunsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldRunsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs(project_id, scenario_id, browser, outcome, started_at, finished_at, duration_ms) VALUES('p','old','lightpanda','PASS',1,2,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ { // idempotent: a second open must not fail on existing columns
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		have, err := tableColumns(s.DB(), "runs")
		if err != nil {
			t.Fatal(err)
		}
		if !have["deploy_marker"] || !have["environment"] {
			t.Fatalf("columns after migration: %v", have)
		}
		s.Close()
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	runs, err := s.ListRuns(ctx, "p", "old", 10)
	if err != nil || len(runs) != 1 || runs[0].Environment != "" {
		t.Fatalf("old row after migration: %v %+v", err, runs)
	}
	now := time.Now()
	r := &model.Run{ProjectID: "p", ScenarioID: "old", Browser: model.BrowserLightpanda, Outcome: model.OutcomePass, StartedAt: now, FinishedAt: now, Environment: "prod"}
	if _, err := s.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.ListRuns(ctx, "p", "old", 10)
	if len(runs) != 2 || runs[0].Environment != "prod" {
		t.Fatalf("environment round-trip: %+v", runs)
	}
}

// oldFeatureAndScenarioSchema are the tables as shipped before the queue sources
// (kind/ref/details, source_ref/source_kind/reproduction/cadence/approved_at) existed.
const oldFeatureAndScenarioSchema = `CREATE TABLE features (
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
CREATE TABLE scenarios (
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
);`

func TestOpenMigratesOldFeatureAndScenarioTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldFeatureAndScenarioSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO features(id, project_id, status, latest_shipped_sha, shipped_at, created_at, updated_at) VALUES('f1','p','shipped','abc',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO scenarios(id, project_id, state, fingerprint, created_at, updated_at) VALUES('s1','p','ACTIVE','fp',1,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		feat, _ := tableColumns(s.DB(), "features")
		for _, c := range []string{"kind", "ref", "details"} {
			if !feat[c] {
				t.Fatalf("features.%s missing after migration: %v", c, feat)
			}
		}
		sc, _ := tableColumns(s.DB(), "scenarios")
		for _, c := range []string{"source_ref", "source_kind", "reproduction", "cadence", "approved_at"} {
			if !sc[c] {
				t.Fatalf("scenarios.%s missing after migration: %v", c, sc)
			}
		}
		s.Close()
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.GetFeature(ctx, "p", "f1")
	if err != nil || f.Kind != model.FeatureKindShip || f.Ref != "" {
		t.Fatalf("old feature row: %v %+v", err, f)
	}
	m, err := s.GetScenario(ctx, "p", "s1")
	if err != nil || m.State != model.StateActive || m.SourceRef != "" || m.ApprovedAt != nil {
		t.Fatalf("old scenario row: %v %+v", err, m)
	}
	// New columns round-trip through the normal API.
	if _, err := s.UpsertFeature(ctx, "p", model.FeatureEvent{FeatureID: "PROJ-123", Status: "shipped", ShippedSHA: "jira:PROJ-123:abcd1234", ShippedAt: time.Now(), Kind: model.FeatureKindIssue, Ref: "PROJ-123", Details: "steps"}); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.GetFeature(ctx, "p", "PROJ-123"); f == nil || f.Kind != model.FeatureKindIssue || f.Ref != "PROJ-123" || f.Details != "steps" {
		t.Fatalf("issue feature round-trip: %+v", f)
	}
	if err := s.SetScenarioPendingApproval(ctx, "p", "s1", `{"reproduced":true}`); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetScenario(ctx, "p", "s1")
	if m.State != model.StatePendingApproval || m.Reproduction != `{"reproduced":true}` || m.NextDueAt != nil {
		t.Fatalf("pending approval: %+v", m)
	}
	if due, _ := s.ListDueScenarios(ctx, "p", time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatalf("PENDING_APPROVAL must never be due: %+v", due)
	}
}
