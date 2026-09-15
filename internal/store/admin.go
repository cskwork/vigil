package store

import (
	"context"
	"vigil/internal/model"
)

// LatestRunsByEnvironment keeps separate outcomes for sites sharing a script.
func (s *Store) LatestRunsByEnvironment(ctx context.Context, project string) ([]*model.Run, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs r WHERE project_id=? AND id=(SELECT MAX(id) FROM runs r2 WHERE r2.project_id=r.project_id AND r2.scenario_id=r.scenario_id AND r2.environment=r.environment) ORDER BY scenario_id,environment`, project)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) ProjectJob(ctx context.Context, project string, id int64) (*model.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE project_id=? AND id=?`, project, id))
}
func (s *Store) LatestRunForJob(ctx context.Context, project string, id int64) (*model.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE project_id=? AND job_id=? ORDER BY id DESC LIMIT 1`, project, id))
}
