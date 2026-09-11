package store

import (
	"context"
	"database/sql"

	"vigil/internal/model"
)

// ---- findings (WI-E data-analyst observations) ----------------------------------

const findingCols = `id, project_id, feature_id, scenario_id, job_id, kind, where_, expected, actual, evidence, state, created_at`

// InsertFinding stores one OPEN finding and fills f.ID / f.State / f.CreatedAt.
func (s *Store) InsertFinding(ctx context.Context, f *model.Finding) (int64, error) {
	at := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO findings(project_id, feature_id, scenario_id, job_id, kind, where_, expected, actual, evidence, state, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'OPEN',?)`, f.ProjectID, f.FeatureID, f.ScenarioID, f.JobID, f.Kind, f.Where, f.Expected, f.Actual, f.Evidence, ms(at))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	f.ID, f.State, f.CreatedAt = id, "OPEN", at
	return id, nil
}

func scanFinding(sc interface{ Scan(...any) error }) (*model.Finding, error) {
	var f model.Finding
	var created int64
	if err := sc.Scan(&f.ID, &f.ProjectID, &f.FeatureID, &f.ScenarioID, &f.JobID, &f.Kind, &f.Where, &f.Expected, &f.Actual, &f.Evidence, &f.State, &created); err != nil {
		return nil, err
	}
	f.CreatedAt = fromMs(created)
	return &f, nil
}

func (s *Store) queryFindings(ctx context.Context, q string, args ...any) ([]*model.Finding, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Finding
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFindings returns findings newest first (OPEN only when openOnly).
func (s *Store) ListFindings(ctx context.Context, project string, openOnly bool, limit int) ([]*model.Finding, error) {
	q := `SELECT ` + findingCols + ` FROM findings WHERE project_id=?`
	if openOnly {
		q += ` AND state='OPEN'`
	}
	q += ` ORDER BY id DESC LIMIT ?`
	return s.queryFindings(ctx, q, project, limit)
}

// ListOpenFindingsFor returns the OPEN findings attached to a scenario or (when
// featureID is non-empty) to its feature, newest first.
func (s *Store) ListOpenFindingsFor(ctx context.Context, project, scenarioID, featureID string) ([]*model.Finding, error) {
	return s.queryFindings(ctx, `SELECT `+findingCols+` FROM findings WHERE project_id=? AND state='OPEN'
		AND ((scenario_id<>'' AND scenario_id=?) OR (feature_id<>'' AND feature_id=?)) ORDER BY id DESC`, project, scenarioID, featureID)
}

// GetFinding returns one finding by id.
func (s *Store) GetFinding(ctx context.Context, project string, id int64) (*model.Finding, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+findingCols+` FROM findings WHERE project_id=? AND id=?`, project, id)
	f, err := scanFinding(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return f, err
}

// ResolveFinding marks one finding RESOLVED; ErrNotFound when it does not exist or is already resolved.
func (s *Store) ResolveFinding(ctx context.Context, project string, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE findings SET state='RESOLVED' WHERE project_id=? AND id=? AND state='OPEN'`, project, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
