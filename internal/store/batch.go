package store

import (
	"context"
	"database/sql"

	"vigil/internal/model"
)

// Batch reads for the dashboard. Each replaces an N+1 loop that used to issue
// one query per scenario on every poll; all return maps keyed by scenario id.

// ListScenarioMetrics returns every scenario_metrics row for the project in one query.
// Scenarios without a row are simply absent from the map.
func (s *Store) ListScenarioMetrics(ctx context.Context, project string) (map[string]*model.ScenarioMetrics, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.scenario_id, m.runs, m.passes, m.failures, m.flakes, m.regressions_caught, m.total_duration_ms, m.last_verified_at
		FROM scenario_metrics m JOIN scenarios sc ON sc.id = m.scenario_id WHERE sc.project_id=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.ScenarioMetrics{}
	for rows.Next() {
		var m model.ScenarioMetrics
		var total int64
		var last sql.NullInt64
		if err := rows.Scan(&m.ScenarioID, &m.Runs, &m.Passes, &m.Failures, &m.Flakes, &m.RegressionsCaught, &total, &last); err != nil {
			return nil, err
		}
		if m.Runs > 0 {
			m.MedianDurationMs = total / int64(m.Runs)
		}
		m.LastVerifiedAt = fromMsp(last)
		out[m.ScenarioID] = &m
	}
	return out, rows.Err()
}

// ListCurrentScenarioVersions returns the current version (with YAML) of every
// scenario in the project, keyed by scenario id.
func (s *Store) ListCurrentScenarioVersions(ctx context.Context, project string) (map[string]*model.ScenarioVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT v.scenario_id, v.version, v.yaml, v.fingerprint, v.created_by, v.reason, v.created_at
		FROM scenario_versions v JOIN scenarios sc ON sc.project_id = v.project_id AND sc.id = v.scenario_id AND sc.current_version = v.version
		WHERE v.project_id=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.ScenarioVersion{}
	for rows.Next() {
		var v model.ScenarioVersion
		var created int64
		if err := rows.Scan(&v.ScenarioID, &v.Version, &v.YAML, &v.Fingerprint, &v.CreatedBy, &v.Reason, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = fromMs(created)
		out[v.ScenarioID] = &v
	}
	return out, rows.Err()
}

// ListScenarioVersionHistory returns every version of every scenario in the
// project, newest first per scenario, WITHOUT the YAML body (only the current
// version's YAML is worth shipping; older bodies are fetched on demand).
func (s *Store) ListScenarioVersionHistory(ctx context.Context, project string) (map[string][]*model.ScenarioVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scenario_id, version, fingerprint, created_by, reason, created_at
		FROM scenario_versions WHERE project_id=? ORDER BY scenario_id, version DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]*model.ScenarioVersion{}
	for rows.Next() {
		var v model.ScenarioVersion
		var created int64
		if err := rows.Scan(&v.ScenarioID, &v.Version, &v.Fingerprint, &v.CreatedBy, &v.Reason, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = fromMs(created)
		out[v.ScenarioID] = append(out[v.ScenarioID], &v)
	}
	return out, rows.Err()
}

// ListAllCoverageLinks returns the coverage links of every scenario in the project.
func (s *Store) ListAllCoverageLinks(ctx context.Context, project string) (map[string][]model.CoverageLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scenario_id, link_type, link_value FROM coverage_links WHERE project_id=? ORDER BY scenario_id, link_type, link_value`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.CoverageLink{}
	for rows.Next() {
		var l model.CoverageLink
		var lt string
		if err := rows.Scan(&l.ScenarioID, &lt, &l.LinkValue); err != nil {
			return nil, err
		}
		l.LinkType = model.LinkType(lt)
		out[l.ScenarioID] = append(out[l.ScenarioID], l)
	}
	return out, rows.Err()
}
