package proof

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"vigil/internal/model"
)

// Replan preserves prior attempts. Revision invalidates any previously displayed approval.
func (r *Repository) Replan(ctx context.Context, check string, version int, answer string) (*Check, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var body string
	if err = tx.QueryRowContext(ctx, `SELECT body FROM proof_checks WHERE id=?`, check).Scan(&body); err != nil {
		return nil, err
	}
	var c Check
	if err = json.Unmarshal([]byte(body), &c); err != nil {
		return nil, err
	}
	if c.RowVersion != version {
		return nil, ErrConflict
	}
	if c.Planning == "QUEUED" {
		return &c, nil
	}
	if c.Question == "" && c.PlanError == "" && len(c.Draft.Criteria) > 0 {
		return nil, ErrConflict
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM proof_attempts WHERE check_id=? AND state IN ('QUEUED','RUNNING')`, check).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, ErrBusy
	}
	if len(answer) > 2000 {
		return nil, fmt.Errorf("answer too long")
	}
	if c.Question != "" && strings.TrimSpace(answer) == "" {
		return nil, fmt.Errorf("answer required for planner question")
	}
	if answer != "" {
		c.Clarifications = append(c.Clarifications, answer)
	}
	c.Planning = "QUEUED"
	c.PlanError = ""
	c.Question = ""
	c.Suggestions = nil
	c.RowVersion++
	c.CurrentRevision++
	c.UpdatedAt = time.Now().UTC()
	c.Draft = Contract{Actions: []string{}, Criteria: []Criterion{}}
	c.ContractHash = Hash(c.Draft)
	if _, err = tx.ExecContext(ctx, `UPDATE proof_checks SET body=?,row_version=?,current_revision=? WHERE id=?`, encode(c), c.RowVersion, c.CurrentRevision, c.ID); err != nil {
		return nil, err
	}
	if _, err = job(ctx, tx, string(model.JobProofPlan), c.ID); err != nil {
		return nil, err
	}
	return &c, tx.Commit()
}
