// Package store implements the control-state repository on SQLite (local mode).
// The same method set is what a PostgreSQL implementation must provide (PRD §16).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"vigil/internal/model"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers; WAL keeps readers cheap
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrateColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate columns: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateColumns adds columns introduced after the first release to existing databases.
func migrateColumns(db *sql.DB) error {
	for _, m := range migrations {
		if err := addColumnIfMissing(db, m.table, m.column, m.ddl); err != nil {
			return err
		}
	}
	// indexes on migrated columns are created here, after the column is guaranteed
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS ix_runs_deploy ON runs(project_id, deploy_marker, id)`)
	return err
}

// addColumnIfMissing is the idempotent migration helper: PRAGMA table_info,
// then ALTER TABLE ... ADD COLUMN when the column is absent.
func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	have, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	if have[column] {
		return nil
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl))
	return err
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// ---- helpers -------------------------------------------------------------

func ms(t time.Time) int64 { return t.UTC().UnixMilli() }
func msp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ms(*t)
}
func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }
func fromMsp(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}
func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func parseList(s string) []string {
	var v []string
	_ = json.Unmarshal([]byte(s), &v)
	return v
}
func now() time.Time { return time.Now().UTC() }

// ---- projects ------------------------------------------------------------

func (s *Store) UpsertProject(ctx context.Context, id, baseURL string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(id, base_url, created_at) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET base_url=excluded.base_url`, id, baseURL, ms(now()))
	return err
}

func (s *Store) GetRepoCursor(ctx context.Context, project, path string) (string, error) {
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT last_seen_sha FROM repositories WHERE project_id=? AND path=?`, project, path).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return sha, err
}

func (s *Store) SetRepoCursor(ctx context.Context, project, path, branch, sha string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repositories(project_id, path, branch, last_seen_sha) VALUES(?,?,?,?)
		ON CONFLICT(project_id, path) DO UPDATE SET branch=excluded.branch, last_seen_sha=excluded.last_seen_sha`, project, path, branch, sha)
	return err
}

// ---- features ------------------------------------------------------------

const featureCols = `id, project_id, status, latest_shipped_sha, shipped_at, changed_paths, routes, summary, source, kind, ref, details, readiness, ready_at, last_handled_sha, created_at, updated_at`

// featureKind normalises an event kind for storage: "" is a shipped change.
func featureKind(kind string) string {
	if kind == "" {
		return model.FeatureKindShip
	}
	return kind
}

func scanFeature(sc interface{ Scan(...any) error }) (*model.Feature, error) {
	var f model.Feature
	var shipped, created, updated int64
	var ready sql.NullInt64
	var paths, routes, readiness string
	if err := sc.Scan(&f.ID, &f.ProjectID, &f.Status, &f.LatestShippedSHA, &shipped, &paths, &routes, &f.Summary, &f.Source, &f.Kind, &f.Ref, &f.Details, &readiness, &ready, &f.LastHandledSHA, &created, &updated); err != nil {
		return nil, err
	}
	f.ShippedAt = fromMs(shipped)
	f.ChangedPaths = parseList(paths)
	f.Routes = parseList(routes)
	f.Readiness = model.Readiness(readiness)
	f.ReadyAt = fromMsp(ready)
	f.CreatedAt = fromMs(created)
	f.UpdatedAt = fromMs(updated)
	return &f, nil
}

// UpsertFeature records a shipped event. Returns true when the SHA is new for this feature.
func (s *Store) UpsertFeature(ctx context.Context, project string, ev model.FeatureEvent) (isNew bool, err error) {
	existing, err := s.GetFeature(ctx, project, ev.FeatureID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	t := ms(now())
	if existing == nil {
		_, err = s.db.ExecContext(ctx, `INSERT INTO features(`+featureCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ev.FeatureID, project, ev.Status, ev.ShippedSHA, ms(ev.ShippedAt), jsonList(ev.ChangedPaths), jsonList(ev.Routes), ev.Summary, ev.Source,
			featureKind(ev.Kind), ev.Ref, ev.Details, string(model.ReadinessWaiting), nil, "", t, t)
		return true, err
	}
	if existing.LatestShippedSHA == ev.ShippedSHA {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE features SET status=?, latest_shipped_sha=?, shipped_at=?, changed_paths=?, routes=?, summary=?, source=?,
		kind=?, ref=?, details=?, readiness=?, ready_at=NULL, updated_at=? WHERE project_id=? AND id=?`,
		ev.Status, ev.ShippedSHA, ms(ev.ShippedAt), jsonList(ev.ChangedPaths), jsonList(ev.Routes), ev.Summary, ev.Source,
		featureKind(ev.Kind), ev.Ref, ev.Details, string(model.ReadinessWaiting), t, project, ev.FeatureID)
	return true, err
}

func (s *Store) GetFeature(ctx context.Context, project, id string) (*model.Feature, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+featureCols+` FROM features WHERE project_id=? AND id=?`, project, id)
	f, err := scanFeature(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (s *Store) ListFeatures(ctx context.Context, project string) ([]*model.Feature, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+featureCols+` FROM features WHERE project_id=? ORDER BY shipped_at DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListUnhandledFeatures returns features whose latest SHA has not been planned yet.
func (s *Store) ListUnhandledFeatures(ctx context.Context, project string) ([]*model.Feature, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+featureCols+` FROM features WHERE project_id=? AND latest_shipped_sha<>last_handled_sha ORDER BY shipped_at`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) SetFeatureReadiness(ctx context.Context, project, id string, r model.Readiness, readyAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE features SET readiness=?, ready_at=?, updated_at=? WHERE project_id=? AND id=?`, string(r), msp(readyAt), ms(now()), project, id)
	return err
}

func (s *Store) MarkFeatureHandled(ctx context.Context, project, id, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE features SET last_handled_sha=?, updated_at=? WHERE project_id=? AND id=?`, sha, ms(now()), project, id)
	return err
}

// RegisterManualRequest atomically records a ready, already-handled manual
// feature and its Browser Agent job. A dashboard receipt must never point at a
// feature whose job failed to enter the queue.
func (s *Store) RegisterManualRequest(ctx context.Context, project string, ev model.FeatureEvent, j *model.Job, dedupKey string) (id int64, created bool, err error) {
	if j == nil {
		return 0, false, errors.New("manual request job is required")
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 2
	}
	if j.Payload == "" {
		j.Payload = "{}"
	}
	if j.ScheduledAt.IsZero() {
		j.ScheduledAt = now()
	}
	t := ms(now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	_, err = tx.ExecContext(ctx, `INSERT INTO features(`+featureCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,id) DO UPDATE SET
		status=excluded.status, latest_shipped_sha=excluded.latest_shipped_sha, shipped_at=excluded.shipped_at,
		changed_paths=excluded.changed_paths, routes=excluded.routes, summary=excluded.summary, source=excluded.source,
		kind=excluded.kind, ref=excluded.ref, details=excluded.details,
		readiness=excluded.readiness, ready_at=excluded.ready_at, last_handled_sha=excluded.last_handled_sha, updated_at=excluded.updated_at`,
		ev.FeatureID, project, ev.Status, ev.ShippedSHA, ms(ev.ShippedAt), jsonList(ev.ChangedPaths), jsonList(ev.Routes), ev.Summary, ev.Source,
		featureKind(ev.Kind), ev.Ref, ev.Details, string(model.ReadinessReady), ms(ev.ShippedAt), ev.ShippedSHA, t, t)
	if err != nil {
		return 0, false, err
	}
	if dedupKey != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE project_id=? AND dedup_key=? AND state IN ('READY','LEASED') LIMIT 1`, project, dedupKey).Scan(&id)
		if err == nil {
			if err = tx.Commit(); err != nil {
				return 0, false, err
			}
			return id, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs(project_id, kind, state, priority, scheduled_at, scenario_id, feature_id, browser, payload, attempt, max_attempts, lease_owner, lease_expires_at, last_error, dedup_key, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,0,?,'',NULL,'',?,?,?)`,
		project, string(j.Kind), string(model.JobReady), j.Priority, ms(j.ScheduledAt), j.ScenarioID, j.FeatureID, string(j.Browser), j.Payload, j.MaxAttempts, dedupKey, t, t)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ---- scenarios -----------------------------------------------------------

const scenarioCols = `id, project_id, state, fingerprint, title, class, mutation, locks, current_version, oracle_source, oracle_feature, oracle_sha,
 soak_passes, soak_target, last_outcome, last_run_at, last_pass_at, consecutive_failures, next_due_at, origin,
 source_ref, source_kind, reproduction, cadence, approved_at, created_at, updated_at`

func scanScenario(sc interface{ Scan(...any) error }) (*model.Scenario, error) {
	var m model.Scenario
	var state, mutation, locks, lastOutcome string
	var lastRun, lastPass, nextDue, approved sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&m.ID, &m.ProjectID, &state, &m.Fingerprint, &m.Title, &m.Class, &mutation, &locks, &m.CurrentVersion, &m.OracleSource, &m.OracleFeature, &m.OracleSHA,
		&m.SoakPasses, &m.SoakTarget, &lastOutcome, &lastRun, &lastPass, &m.ConsecutiveFailures, &nextDue, &m.Origin,
		&m.SourceRef, &m.SourceKind, &m.Reproduction, &m.Cadence, &approved, &created, &updated); err != nil {
		return nil, err
	}
	m.ApprovedAt = fromMsp(approved)
	m.State = model.ScenarioState(state)
	m.Mutation = model.Mutation(mutation)
	m.Locks = parseList(locks)
	m.LastOutcome = model.Outcome(lastOutcome)
	m.LastRunAt = fromMsp(lastRun)
	m.LastPassAt = fromMsp(lastPass)
	m.NextDueAt = fromMsp(nextDue)
	m.CreatedAt = fromMs(created)
	m.UpdatedAt = fromMs(updated)
	return &m, nil
}

// CreateScenario inserts the scenario row plus its first version and coverage links.
func (s *Store) CreateScenario(ctx context.Context, m *model.Scenario, v *model.ScenarioVersion, links []model.CoverageLink) error {
	t := ms(now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if m.Class == "" {
		m.Class = "P1"
	}
	if m.Mutation == "" {
		m.Mutation = model.MutationReadOnly
	}
	if m.CurrentVersion == 0 {
		m.CurrentVersion = v.Version
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO scenarios(`+scenarioCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ProjectID, string(m.State), m.Fingerprint, m.Title, m.Class, string(m.Mutation), jsonList(m.Locks), m.CurrentVersion,
		m.OracleSource, m.OracleFeature, m.OracleSHA, m.SoakPasses, m.SoakTarget, string(m.LastOutcome), msp(m.LastRunAt), msp(m.LastPassAt),
		m.ConsecutiveFailures, msp(m.NextDueAt), m.Origin, m.SourceRef, m.SourceKind, m.Reproduction, m.Cadence, msp(m.ApprovedAt), t, t)
	if err != nil {
		return fmt.Errorf("insert scenario %s: %w", m.ID, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO scenario_versions(project_id, scenario_id, version, yaml, fingerprint, created_by, reason, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		m.ProjectID, m.ID, v.Version, v.YAML, v.Fingerprint, v.CreatedBy, v.Reason, t)
	if err != nil {
		return err
	}
	for _, l := range links {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO coverage_links(project_id, scenario_id, link_type, link_value) VALUES(?,?,?,?)`, m.ProjectID, m.ID, string(l.LinkType), l.LinkValue); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddScenarioVersion appends a new version and makes it current; links are replaced.
func (s *Store) AddScenarioVersion(ctx context.Context, project string, v *model.ScenarioVersion, links []model.CoverageLink) error {
	t := ms(now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO scenario_versions(project_id, scenario_id, version, yaml, fingerprint, created_by, reason, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		project, v.ScenarioID, v.Version, v.YAML, v.Fingerprint, v.CreatedBy, v.Reason, t); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scenarios SET current_version=?, fingerprint=?, updated_at=? WHERE project_id=? AND id=?`, v.Version, v.Fingerprint, t, project, v.ScenarioID); err != nil {
		return err
	}
	if links != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM coverage_links WHERE project_id=? AND scenario_id=?`, project, v.ScenarioID); err != nil {
			return err
		}
		for _, l := range links {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO coverage_links(project_id, scenario_id, link_type, link_value) VALUES(?,?,?,?)`, project, v.ScenarioID, string(l.LinkType), l.LinkValue); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) GetScenario(ctx context.Context, project, id string) (*model.Scenario, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scenarioCols+` FROM scenarios WHERE project_id=? AND id=?`, project, id)
	m, err := scanScenario(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (s *Store) FindScenarioByFingerprint(ctx context.Context, project, fp string) (*model.Scenario, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scenarioCols+` FROM scenarios WHERE project_id=? AND fingerprint=? AND state NOT IN ('REJECTED','RETIRED','DUPLICATE') ORDER BY created_at LIMIT 1`, project, fp)
	m, err := scanScenario(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

func (s *Store) ListScenarios(ctx context.Context, project string, states ...model.ScenarioState) ([]*model.Scenario, error) {
	q := `SELECT ` + scenarioCols + ` FROM scenarios WHERE project_id=?`
	args := []any{project}
	if len(states) > 0 {
		ph := make([]string, len(states))
		for i, st := range states {
			ph[i] = "?"
			args = append(args, string(st))
		}
		q += ` AND state IN (` + strings.Join(ph, ",") + `)`
	}
	q += ` ORDER BY class, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Scenario
	for rows.Next() {
		m, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListDueScenarios returns ACTIVE/SOAK scenarios whose next_due_at <= now (or never run),
// limited so huge corpora are not loaded at once (AC-22).
func (s *Store) ListDueScenarios(ctx context.Context, project string, at time.Time, limit int) ([]*model.Scenario, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scenarioCols+` FROM scenarios WHERE project_id=? AND state IN ('ACTIVE','SOAK')
		AND (next_due_at IS NULL OR next_due_at<=?) ORDER BY CASE state WHEN 'SOAK' THEN 0 ELSE 1 END, class, COALESCE(next_due_at,0) LIMIT ?`, project, ms(at), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Scenario
	for rows.Next() {
		m, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetScenarioSource records the queue item a scenario reproduces (issue key / log signature hash).
func (s *Store) SetScenarioSource(ctx context.Context, project, id, ref, kind string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET source_ref=?, source_kind=?, updated_at=? WHERE project_id=? AND id=?`, ref, kind, ms(now()), project, id)
	return err
}

// SetScenarioPendingApproval parks a reproduce script for a human decision:
// state PENDING_APPROVAL, the reproduction verdict JSON stored, never due.
func (s *Store) SetScenarioPendingApproval(ctx context.Context, project, id, reproduction string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET state=?, reproduction=?, next_due_at=NULL, updated_at=? WHERE project_id=? AND id=?`,
		string(model.StatePendingApproval), reproduction, ms(now()), project, id)
	return err
}

// SetScenarioApproved promotes a script straight to ACTIVE on a named cadence
// (approval workflow): soak counters reset, approved_at stamped, next due set.
func (s *Store) SetScenarioApproved(ctx context.Context, project, id, cadence string, approvedAt, nextDue time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET state=?, cadence=?, approved_at=?, soak_passes=0, consecutive_failures=0, next_due_at=?, updated_at=?
		WHERE project_id=? AND id=?`, string(model.StateActive), cadence, ms(approvedAt), ms(nextDue), ms(now()), project, id)
	return err
}

// SetScenarioSoak restarts the soak path: state SOAK, counters reset, cadence
// cleared, due at the given time.
func (s *Store) SetScenarioSoak(ctx context.Context, project, id string, dueAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET state=?, cadence='', soak_passes=0, consecutive_failures=0, next_due_at=?, updated_at=?
		WHERE project_id=? AND id=?`, string(model.StateSoak), ms(dueAt), ms(now()), project, id)
	return err
}

func (s *Store) SetScenarioState(ctx context.Context, project, id string, st model.ScenarioState) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET state=?, updated_at=? WHERE project_id=? AND id=?`, string(st), ms(now()), project, id)
	return err
}

// UpdateScenarioMeta refreshes human-facing metadata after a seed file changed.
func (s *Store) UpdateScenarioMeta(ctx context.Context, project, id, title, class string, mutation model.Mutation, locks []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET title=?, class=?, mutation=?, locks=?, updated_at=? WHERE project_id=? AND id=?`,
		title, class, string(mutation), jsonList(locks), ms(now()), project, id)
	return err
}

func (s *Store) SetScenarioNextDue(ctx context.Context, project, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET next_due_at=?, updated_at=? WHERE project_id=? AND id=?`, ms(at), ms(now()), project, id)
	return err
}

// RecordScenarioOutcome updates counters after a run and returns the refreshed row.
func (s *Store) RecordScenarioOutcome(ctx context.Context, project, id string, outcome model.Outcome, at time.Time) (*model.Scenario, error) {
	pass := outcome == model.OutcomePass
	var q string
	if pass {
		q = `UPDATE scenarios SET last_outcome=?, last_run_at=?, last_pass_at=?, consecutive_failures=0,
			soak_passes = CASE WHEN state='SOAK' THEN soak_passes+1 ELSE soak_passes END, updated_at=? WHERE project_id=? AND id=?`
		if _, err := s.db.ExecContext(ctx, q, string(outcome), ms(at), ms(at), ms(now()), project, id); err != nil {
			return nil, err
		}
	} else {
		fail := 0
		if outcome == model.OutcomeAppFailure || outcome == model.OutcomeScriptDrift || outcome == model.OutcomeQAFlake || outcome == model.OutcomeBrowserAmbiguous {
			fail = 1
		}
		q = `UPDATE scenarios SET last_outcome=?, last_run_at=?, consecutive_failures=consecutive_failures+?, updated_at=? WHERE project_id=? AND id=?`
		if _, err := s.db.ExecContext(ctx, q, string(outcome), ms(at), fail, ms(now()), project, id); err != nil {
			return nil, err
		}
	}
	return s.GetScenario(ctx, project, id)
}

func (s *Store) GetScenarioVersion(ctx context.Context, project, id string, version int) (*model.ScenarioVersion, error) {
	var v model.ScenarioVersion
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT scenario_id, version, yaml, fingerprint, created_by, reason, created_at FROM scenario_versions WHERE project_id=? AND scenario_id=? AND version=?`, project, id, version).
		Scan(&v.ScenarioID, &v.Version, &v.YAML, &v.Fingerprint, &v.CreatedBy, &v.Reason, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	v.CreatedAt = fromMs(created)
	return &v, err
}

// ListScenarioVersions returns every version of a scenario, newest first.
func (s *Store) ListScenarioVersions(ctx context.Context, project, id string) ([]*model.ScenarioVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scenario_id, version, yaml, fingerprint, created_by, reason, created_at FROM scenario_versions WHERE project_id=? AND scenario_id=? ORDER BY version DESC`, project, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ScenarioVersion
	for rows.Next() {
		var v model.ScenarioVersion
		var created int64
		if err := rows.Scan(&v.ScenarioID, &v.Version, &v.YAML, &v.Fingerprint, &v.CreatedBy, &v.Reason, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = fromMs(created)
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (s *Store) GetCurrentScenarioVersion(ctx context.Context, project, id string) (*model.Scenario, *model.ScenarioVersion, error) {
	m, err := s.GetScenario(ctx, project, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := s.GetScenarioVersion(ctx, project, id, m.CurrentVersion)
	return m, v, err
}

func (s *Store) ListCoverageLinks(ctx context.Context, project, scenarioID string) ([]model.CoverageLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scenario_id, link_type, link_value FROM coverage_links WHERE project_id=? AND scenario_id=?`, project, scenarioID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CoverageLink
	for rows.Next() {
		var l model.CoverageLink
		var lt string
		if err := rows.Scan(&l.ScenarioID, &lt, &l.LinkValue); err != nil {
			return nil, err
		}
		l.LinkType = model.LinkType(lt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ScenariosLinkedTo returns scenario ids (in the given states) that carry a coverage link
// of the given type whose value equals or prefix-matches one of values.
func (s *Store) ScenariosLinkedTo(ctx context.Context, project string, lt model.LinkType, values []string, states []model.ScenarioState) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	stPh := make([]string, len(states))
	args := []any{project, string(lt)}
	for i, st := range states {
		stPh[i] = "?"
		args = append(args, string(st))
	}
	var conds []string
	for _, v := range values {
		conds = append(conds, "(cl.link_value = ? OR ? LIKE cl.link_value || '%' OR cl.link_value LIKE ? || '%')")
		args = append(args, v, v, v)
	}
	q := `SELECT DISTINCT cl.scenario_id FROM coverage_links cl JOIN scenarios sc ON sc.project_id=cl.project_id AND sc.id=cl.scenario_id
		WHERE cl.project_id=? AND cl.link_type=? AND sc.state IN (` + strings.Join(stPh, ",") + `) AND (` + strings.Join(conds, " OR ") + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- flows ---------------------------------------------------------------

func (s *Store) UpsertFlow(ctx context.Context, project, id string, version int, yamlText string) error {
	t := ms(now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO flows(id, project_id, current_version, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(project_id, id) DO UPDATE SET current_version=excluded.current_version, updated_at=excluded.updated_at`, id, project, version, t); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO flow_versions(project_id, flow_id, version, yaml, created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(project_id, flow_id, version) DO UPDATE SET yaml=excluded.yaml`, project, id, version, yamlText, t); err != nil {
		return err
	}
	return tx.Commit()
}

// ListFlowYAML returns current flow yaml keyed by flow id.
func (s *Store) ListFlowYAML(ctx context.Context, project string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, fv.yaml FROM flows f JOIN flow_versions fv ON fv.project_id=f.project_id AND fv.flow_id=f.id AND fv.version=f.current_version WHERE f.project_id=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, y string
		if err := rows.Scan(&id, &y); err != nil {
			return nil, err
		}
		out[id] = y
	}
	return out, rows.Err()
}

// ---- jobs ----------------------------------------------------------------

const jobCols = `id, project_id, kind, state, priority, scheduled_at, scenario_id, feature_id, browser, payload, attempt, max_attempts, lease_owner, lease_expires_at, last_error, created_at, updated_at`

func scanJob(sc interface{ Scan(...any) error }) (*model.Job, error) {
	var j model.Job
	var kind, state, browser string
	var sched, created, updated int64
	var lease sql.NullInt64
	if err := sc.Scan(&j.ID, &j.ProjectID, &kind, &state, &j.Priority, &sched, &j.ScenarioID, &j.FeatureID, &browser, &j.Payload, &j.Attempt, &j.MaxAttempts, &j.LeaseOwner, &lease, &j.LastError, &created, &updated); err != nil {
		return nil, err
	}
	j.Kind = model.JobKind(kind)
	j.State = model.JobState(state)
	j.Browser = model.Browser(browser)
	j.ScheduledAt = fromMs(sched)
	j.LeaseExpiresAt = fromMsp(lease)
	j.CreatedAt = fromMs(created)
	j.UpdatedAt = fromMs(updated)
	return &j, nil
}

// EnqueueJob inserts a READY job unless an equivalent READY/LEASED job (same dedup key) exists.
// dedupKey "" disables dedup. Returns the job id (existing or new) and whether it was created.
// RaiseJobPriority lifts a READY job's priority (never lowers it): an operator
// asking for work the loop already queued must not wait behind it.
func (s *Store) RaiseJobPriority(ctx context.Context, id int64, priority int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET priority=?, updated_at=? WHERE id=? AND state='READY' AND priority<?`,
		priority, ms(now()), id, priority)
	return err
}

func (s *Store) EnqueueJob(ctx context.Context, j *model.Job, dedupKey string) (int64, bool, error) {
	if dedupKey != "" {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE project_id=? AND dedup_key=? AND state IN ('READY','LEASED') LIMIT 1`, j.ProjectID, dedupKey).Scan(&id)
		if err == nil {
			return id, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 2
	}
	if j.Payload == "" {
		j.Payload = "{}"
	}
	if j.ScheduledAt.IsZero() {
		j.ScheduledAt = now()
	}
	t := ms(now())
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(project_id, kind, state, priority, scheduled_at, scenario_id, feature_id, browser, payload, attempt, max_attempts, lease_owner, lease_expires_at, last_error, dedup_key, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,0,?,'',NULL,'',?,?,?)`,
		j.ProjectID, string(j.Kind), string(model.JobReady), j.Priority, ms(j.ScheduledAt), j.ScenarioID, j.FeatureID, string(j.Browser), j.Payload, j.MaxAttempts, dedupKey, t, t)
	if err != nil {
		return 0, false, err
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

// ClaimJob atomically leases the highest-priority due job of the given kinds
// (SQLite equivalent of FOR UPDATE SKIP LOCKED: a single UPDATE ... RETURNING).
func (s *Store) ClaimJob(ctx context.Context, project, worker string, lease time.Duration, kinds ...model.JobKind) (*model.Job, error) {
	n := now()
	args := []any{worker, ms(n.Add(lease)), ms(n), project, ms(n)}
	kindClause := ` AND kind NOT IN ('PROOF_PLAN','PROOF_RUN')`
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		kindClause = ` AND kind IN (` + strings.Join(ph, ",") + `)`
	}
	row := s.db.QueryRowContext(ctx, `UPDATE jobs SET state='LEASED', lease_owner=?, lease_expires_at=?, attempt=attempt+1, updated_at=?
		WHERE id = (SELECT id FROM jobs WHERE project_id=? AND state='READY' AND scheduled_at<=?`+kindClause+` ORDER BY priority DESC, scheduled_at LIMIT 1)
		RETURNING `+jobCols, args...)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO job_attempts(job_id, attempt, worker_id, started_at) VALUES(?,?,?,?)`, j.ID, j.Attempt, worker, ms(n))
	return j, nil
}

func (s *Store) HeartbeatJob(ctx context.Context, id int64, worker string, lease time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=?, updated_at=? WHERE id=? AND lease_owner=? AND state='LEASED'`, ms(now().Add(lease)), ms(now()), id, worker)
	return err
}

func (s *Store) CompleteJob(ctx context.Context, id int64, errText string) error {
	st := model.JobDone
	if errText != "" {
		st = model.JobFailed
	}
	t := ms(now())
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state=?, last_error=?, lease_owner='', lease_expires_at=NULL, updated_at=? WHERE id=?`, string(st), errText, t, id)
	if err == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE job_attempts SET finished_at=?, error=? WHERE job_id=? AND finished_at IS NULL`, t, errText, id)
	}
	return err
}

// RequeueJob returns a leased job to READY at a later time (retry/backoff) if attempts remain;
// otherwise marks it FAILED. Returns true when requeued.
func (s *Store) RequeueJob(ctx context.Context, id int64, at time.Time, errText string) (bool, error) {
	t := ms(now())
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='READY', scheduled_at=?, lease_owner='', lease_expires_at=NULL, last_error=?, updated_at=? WHERE id=? AND attempt<max_attempts`, ms(at), errText, t, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	return false, s.CompleteJob(ctx, id, errText)
}

// RestorePreemptedJob gives a cooperatively cancelled worker's lease back to the
// queue without charging an attempt. The lease owner guard prevents a late
// cancellation from reviving work that another worker has already claimed.
func (s *Store) RestorePreemptedJob(ctx context.Context, id int64, leaseOwner string, at time.Time, reason string) (restored bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var claimedAttempt int
	err = tx.QueryRowContext(ctx, `UPDATE jobs
		SET state='READY', scheduled_at=?, attempt=attempt-1, lease_owner='', lease_expires_at=NULL, last_error=?, updated_at=?
		WHERE id=? AND state='LEASED' AND lease_owner=? AND attempt>0
		RETURNING attempt+1`, ms(at), reason, ms(now()), id, leaseOwner).Scan(&claimedAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM job_attempts
		WHERE job_id=? AND attempt=? AND worker_id=? AND finished_at IS NULL`, id, claimedAttempt, leaseOwner); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReapExpiredLeases returns dead-worker jobs to the ready queue (AC-21).
func (s *Store) ReapExpiredLeases(ctx context.Context) (int64, error) {
	t := ms(now())
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='READY', lease_owner='', lease_expires_at=NULL, last_error='lease expired', updated_at=? WHERE kind NOT IN ('PROOF_PLAN','PROOF_RUN') AND state='LEASED' AND lease_expires_at IS NOT NULL AND lease_expires_at<?`, t, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) ListJobs(ctx context.Context, project string, states []model.JobState, limit int) ([]*model.Job, error) {
	args := []any{project}
	q := `SELECT ` + jobCols + ` FROM jobs WHERE project_id=?`
	if len(states) > 0 {
		ph := make([]string, len(states))
		for i, st := range states {
			ph[i] = "?"
			args = append(args, string(st))
		}
		q += ` AND state IN (` + strings.Join(ph, ",") + `)`
	}
	q += ` ORDER BY priority DESC, scheduled_at LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) CountJobs(ctx context.Context, project string, state model.JobState) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE project_id=? AND state=?`, project, string(state)).Scan(&n)
	return n, err
}

// ---- runs ----------------------------------------------------------------

func (s *Store) InsertRun(ctx context.Context, r *model.Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs(job_id, project_id, scenario_id, scenario_version, feature_id, shipped_sha, browser, outcome, attempt, started_at, finished_at, duration_ms, failed_step, failed_action, expected, actual, error, evidence_dir, deploy_marker, environment)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.JobID, r.ProjectID, r.ScenarioID, r.ScenarioVersion, r.FeatureID, r.ShippedSHA, string(r.Browser), string(r.Outcome), r.Attempt, ms(r.StartedAt), ms(r.FinishedAt), r.DurationMs, r.FailedStep, r.FailedAction, r.Expected, r.Actual, r.Error, r.EvidenceDir, r.DeployMarker, r.Environment)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	r.ID = id
	// metrics
	pass, fail, flake := 0, 0, 0
	switch r.Outcome {
	case model.OutcomePass:
		pass = 1
	case model.OutcomeQAFlake:
		flake = 1
	case model.OutcomeAppFailure:
		fail = 1
	}
	var lastVerified any
	if pass == 1 {
		lastVerified = ms(r.FinishedAt)
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO scenario_metrics(scenario_id, runs, passes, failures, flakes, total_duration_ms, last_verified_at) VALUES(?,1,?,?,?,?,?)
		ON CONFLICT(scenario_id) DO UPDATE SET runs=runs+1, passes=passes+excluded.passes, failures=failures+excluded.failures, flakes=flakes+excluded.flakes,
		total_duration_ms=total_duration_ms+excluded.total_duration_ms, last_verified_at=COALESCE(excluded.last_verified_at, last_verified_at)`,
		r.ScenarioID, pass, fail, flake, r.DurationMs, lastVerified)
	return id, nil
}

func (s *Store) AddRunArtifact(ctx context.Context, runID int64, kind, path string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO run_artifacts(run_id, kind, path) VALUES(?,?,?)`, runID, kind, path)
	return err
}

const runCols = `id, job_id, project_id, scenario_id, scenario_version, feature_id, shipped_sha, browser, outcome, attempt, started_at, finished_at, duration_ms, failed_step, failed_action, expected, actual, error, evidence_dir, deploy_marker, environment`

func scanRun(sc interface{ Scan(...any) error }) (*model.Run, error) {
	var r model.Run
	var browser, outcome string
	var started, finished int64
	if err := sc.Scan(&r.ID, &r.JobID, &r.ProjectID, &r.ScenarioID, &r.ScenarioVersion, &r.FeatureID, &r.ShippedSHA, &browser, &outcome, &r.Attempt, &started, &finished, &r.DurationMs, &r.FailedStep, &r.FailedAction, &r.Expected, &r.Actual, &r.Error, &r.EvidenceDir, &r.DeployMarker, &r.Environment); err != nil {
		return nil, err
	}
	r.Browser = model.Browser(browser)
	r.Outcome = model.Outcome(outcome)
	r.StartedAt = fromMs(started)
	r.FinishedAt = fromMs(finished)
	return &r, nil
}

func (s *Store) ListRuns(ctx context.Context, project, scenarioID string, limit int) ([]*model.Run, error) {
	q := `SELECT ` + runCols + ` FROM runs WHERE project_id=?`
	args := []any{project}
	if scenarioID != "" {
		q += ` AND scenario_id=?`
		args = append(args, scenarioID)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunsByID loads the given run ids in one query, keyed by id. Callers that
// explain a list of incidents need their runs without one query per row.
func (s *Store) RunsByID(ctx context.Context, project string, ids []int64) (map[int64]*model.Run, error) {
	out := map[int64]*model.Run{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{project}
	holes := make([]string, 0, len(ids))
	for _, id := range ids {
		holes = append(holes, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE project_id=? AND id IN (`+strings.Join(holes, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

// Deployment summarizes the runs recorded against one deployed build.
type Deployment struct {
	Marker    string
	FirstSeen time.Time
	LastSeen  time.Time
	Runs      int
	Passes    int
	Scenarios int
}

// ListDeployments groups runs by deploy marker, newest first.
func (s *Store) ListDeployments(ctx context.Context, project string, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT deploy_marker, MIN(started_at), MAX(finished_at), COUNT(*), SUM(CASE WHEN outcome='PASS' THEN 1 ELSE 0 END), COUNT(DISTINCT scenario_id)
		FROM runs WHERE project_id=? AND deploy_marker<>'' GROUP BY deploy_marker ORDER BY MAX(finished_at) DESC LIMIT ?`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var first, last int64
		if err := rows.Scan(&d.Marker, &first, &last, &d.Runs, &d.Passes, &d.Scenarios); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = fromMs(first), fromMs(last)
		out = append(out, d)
	}
	return out, rows.Err()
}

// LatestRunsForDeployment returns the newest run of every scenario recorded against marker.
func (s *Store) LatestRunsForDeployment(ctx context.Context, project, marker string) ([]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs r WHERE project_id=? AND deploy_marker=?
		AND id = (SELECT MAX(id) FROM runs r2 WHERE r2.project_id=r.project_id AND r2.scenario_id=r.scenario_id AND r2.deploy_marker=r.deploy_marker) ORDER BY scenario_id`, project, marker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasPassOnBrowser reports whether this scenario has ever passed on the given engine.
// Pinning a scenario to an engine it has never passed on is how a green check goes red
// silently, so the importer consults this before honouring a new pin.
func (s *Store) HasPassOnBrowser(ctx context.Context, project, scenario string, browser model.Browser) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM runs WHERE project_id=? AND scenario_id=? AND browser=? AND outcome=?`,
		project, scenario, string(browser), string(model.OutcomePass)).Scan(&n)
	return n > 0, err
}

// ResetSoak returns a scenario to SOAK with a fresh counter, for when a change invalidates
// the evidence that promoted it. Unlike approve it does not touch next_due_at.
func (s *Store) ResetSoak(ctx context.Context, project, scenario string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenarios SET state=?, soak_passes=0, consecutive_failures=0, updated_at=? WHERE project_id=? AND id=?`,
		string(model.StateSoak), time.Now().UTC().UnixMilli(), project, scenario)
	return err
}

// HasRunForDeployment reports whether a run of scenario on browser exists for
// marker in env. The environment is part of the key: two environments can serve the same
// marker, and a capture on one must not suppress the capture on the other.
func (s *Store) HasRunForDeployment(ctx context.Context, project, scenario, marker, env string, browser model.Browser) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE project_id=? AND scenario_id=? AND deploy_marker=? AND environment=? AND browser=?`,
		project, scenario, marker, env, string(browser)).Scan(&n)
	return n > 0, err
}

// ListRunsForDeployment returns runs recorded against marker, newest first.
func (s *Store) ListRunsForDeployment(ctx context.Context, project, marker string, limit int) ([]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE project_id=? AND deploy_marker=? ORDER BY id DESC LIMIT ?`, project, marker, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRunsBefore removes run rows (and artifact rows) older than `before`, keeping
// runs referenced by incidents and the newest run of every scenario.
func (s *Store) DeleteRunsBefore(ctx context.Context, project string, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE project_id=? AND finished_at<?
		AND id NOT IN (SELECT run_id FROM incidents WHERE project_id=?)
		AND id NOT IN (SELECT MAX(id) FROM runs WHERE project_id=? GROUP BY scenario_id)`, project, ms(before), project, project)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM run_artifacts WHERE run_id NOT IN (SELECT id FROM runs)`)
	}
	return n, nil
}

// RecentFailures returns distinct scenario ids with a non-PASS classified failure since `since`.
func (s *Store) RecentFailures(ctx context.Context, project string, since time.Time, outcomes ...model.Outcome) ([]string, error) {
	args := []any{project, ms(since)}
	ph := make([]string, len(outcomes))
	for i, o := range outcomes {
		ph[i] = "?"
		args = append(args, string(o))
	}
	q := `SELECT DISTINCT scenario_id FROM runs WHERE project_id=? AND started_at>=?`
	if len(outcomes) > 0 {
		q += ` AND outcome IN (` + strings.Join(ph, ",") + `)`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) GetMetrics(ctx context.Context, scenarioID string) (*model.ScenarioMetrics, error) {
	var m model.ScenarioMetrics
	var total int64
	var last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT scenario_id, runs, passes, failures, flakes, regressions_caught, total_duration_ms, last_verified_at FROM scenario_metrics WHERE scenario_id=?`, scenarioID).
		Scan(&m.ScenarioID, &m.Runs, &m.Passes, &m.Failures, &m.Flakes, &m.RegressionsCaught, &total, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return &model.ScenarioMetrics{ScenarioID: scenarioID}, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Runs > 0 {
		m.MedianDurationMs = total / int64(m.Runs) // mean as cheap median proxy
	}
	m.LastVerifiedAt = fromMsp(last)
	return &m, nil
}

func (s *Store) BumpRegressionsCaught(ctx context.Context, scenarioID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE scenario_metrics SET regressions_caught=regressions_caught+1 WHERE scenario_id=?`, scenarioID)
	return err
}

// ---- incidents -----------------------------------------------------------

func (s *Store) CreateIncident(ctx context.Context, in *model.Incident) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO incidents(project_id, kind, scenario_id, scenario_version, feature_id, shipped_sha, run_id, title, summary, state, markdown_path, json_path, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'OPEN',?,?,?)`,
		in.ProjectID, string(in.Kind), in.ScenarioID, in.ScenarioVersion, in.FeatureID, in.ShippedSHA, in.RunID, in.Title, in.Summary, in.MarkdownPath, in.JSONPath, ms(now()))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	in.ID = id
	return id, nil
}

func (s *Store) UpdateIncidentPaths(ctx context.Context, id int64, md, js string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET markdown_path=?, json_path=? WHERE id=?`, md, js, id)
	return err
}

// OpenIncidentFor returns the open incident for a scenario/kind (dedup), if any.
func (s *Store) OpenIncidentFor(ctx context.Context, project string, kind model.IncidentKind, scenarioID string) (*model.Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, kind, scenario_id, scenario_version, feature_id, shipped_sha, run_id, title, summary, state, markdown_path, json_path, created_at, resolved_at
		FROM incidents WHERE project_id=? AND kind=? AND scenario_id=? AND state='OPEN' ORDER BY created_at DESC LIMIT 1`, project, string(kind), scenarioID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scanIncident(rows)
}

func scanIncident(sc interface{ Scan(...any) error }) (*model.Incident, error) {
	var in model.Incident
	var kind string
	var created int64
	var resolved sql.NullInt64
	if err := sc.Scan(&in.ID, &in.ProjectID, &kind, &in.ScenarioID, &in.ScenarioVersion, &in.FeatureID, &in.ShippedSHA, &in.RunID, &in.Title, &in.Summary, &in.State, &in.MarkdownPath, &in.JSONPath, &created, &resolved); err != nil {
		return nil, err
	}
	in.Kind = model.IncidentKind(kind)
	in.CreatedAt = fromMs(created)
	in.ResolvedAt = fromMsp(resolved)
	return &in, nil
}

func (s *Store) ListIncidents(ctx context.Context, project string, openOnly bool, limit int) ([]*model.Incident, error) {
	q := `SELECT id, project_id, kind, scenario_id, scenario_version, feature_id, shipped_sha, run_id, title, summary, state, markdown_path, json_path, created_at, resolved_at FROM incidents WHERE project_id=?`
	if openOnly {
		q += ` AND state='OPEN'`
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) ResolveIncidents(ctx context.Context, project string, kind model.IncidentKind, scenarioID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET state='RESOLVED', resolved_at=? WHERE project_id=? AND kind=? AND scenario_id=? AND state='OPEN'`, ms(now()), project, string(kind), scenarioID)
	return err
}

// ---- resource locks (PRD §14) --------------------------------------------

// TryAcquireLocks atomically acquires all keys for owner or none. Expired locks are reclaimable.
func (s *Store) TryAcquireLocks(ctx context.Context, keys []string, owner string, ttl time.Duration) (bool, error) {
	if len(keys) == 0 {
		return true, nil
	}
	n := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, k := range keys {
		res, err := tx.ExecContext(ctx, `INSERT INTO resource_locks(lock_key, owner, expires_at) VALUES(?,?,?)
			ON CONFLICT(lock_key) DO UPDATE SET owner=excluded.owner, expires_at=excluded.expires_at
			WHERE resource_locks.expires_at<? OR resource_locks.owner=excluded.owner`, k, owner, ms(n.Add(ttl)), ms(n))
		if err != nil {
			return false, err
		}
		if a, _ := res.RowsAffected(); a == 0 {
			return false, nil // held by someone else -> rollback releases any we took
		}
	}
	return true, tx.Commit()
}

func (s *Store) ReleaseLocks(ctx context.Context, keys []string, owner string) error {
	for _, k := range keys {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM resource_locks WHERE lock_key=? AND owner=?`, k, owner); err != nil {
			return err
		}
	}
	return nil
}

// ---- budgets (PRD §12) ---------------------------------------------------

func (s *Store) RecordBudget(ctx context.Context, project, kind string, amount int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO budget_events(project_id, kind, amount, at) VALUES(?,?,?,?)`, project, kind, amount, ms(now()))
	return err
}

// BudgetUsed sums amounts of kind within the trailing window.
func (s *Store) BudgetUsed(ctx context.Context, project, kind string, window time.Duration) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT SUM(amount) FROM budget_events WHERE project_id=? AND kind=? AND at>=?`, project, kind, ms(now().Add(-window))).Scan(&v)
	return v.Int64, err
}

// ---- scheduler state / workers ------------------------------------------

func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM scheduler_state WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scheduler_state(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) HeartbeatWorker(ctx context.Context, id, kind string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workers(id, kind, last_heartbeat) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET last_heartbeat=excluded.last_heartbeat`, id, kind, ms(now()))
	return err
}

// Counts is a cheap status snapshot.
type Counts struct {
	Scenarios map[string]int
	Jobs      map[string]int
	Incidents int
	Features  int
}

func (s *Store) Counts(ctx context.Context, project string) (*Counts, error) {
	c := &Counts{Scenarios: map[string]int{}, Jobs: map[string]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM scenarios WHERE project_id=? GROUP BY state`, project)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		_ = rows.Scan(&k, &n)
		c.Scenarios[k] = n
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs WHERE project_id=? GROUP BY state`, project)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		_ = rows.Scan(&k, &n)
		c.Jobs[k] = n
	}
	rows.Close()
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE project_id=? AND state='OPEN'`, project).Scan(&c.Incidents)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM features WHERE project_id=?`, project).Scan(&c.Features)
	return c, nil
}
