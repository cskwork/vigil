package proof

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"vigil/internal/model"
)

var ErrConflict = errors.New("revision or payload conflict")
var ErrNotFound = errors.New("not found")
var ErrBusy = errors.New("another proof attempt is active")

func id() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func encode(v any) string { b, _ := json.Marshal(v); return string(b) }

type Repository struct{ DB *sql.DB }

func NewRepository(db *sql.DB) (*Repository, error) {
	_, e := db.Exec(`
CREATE TABLE IF NOT EXISTS proof_checks(id TEXT PRIMARY KEY,target_ref TEXT NOT NULL,current_revision INTEGER NOT NULL,row_version INTEGER NOT NULL,body TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS proof_attempts(id TEXT PRIMARY KEY,check_id TEXT NOT NULL REFERENCES proof_checks(id),job_id INTEGER NOT NULL REFERENCES jobs(id),idem_key TEXT NOT NULL,payload_hash TEXT NOT NULL,state TEXT NOT NULL,body TEXT NOT NULL,disposition TEXT NOT NULL DEFAULT '',cancel_requested INTEGER NOT NULL DEFAULT 0,disposition_history TEXT NOT NULL DEFAULT '[]',UNIQUE(check_id,idem_key));
CREATE TRIGGER IF NOT EXISTS proof_attempt_immutable BEFORE UPDATE OF body ON proof_attempts WHEN OLD.state IN ('DONE','INTERRUPTED','CANCELLED') BEGIN SELECT RAISE(ABORT,'terminal attempt is immutable'); END;
CREATE INDEX IF NOT EXISTS proof_checks_cursor ON proof_checks(created_at,id);
CREATE UNIQUE INDEX IF NOT EXISTS proof_intake_key ON proof_checks(json_extract(body,'$.created_by'),json_extract(body,'$.team'),json_extract(body,'$.intake_key')) WHERE COALESCE(json_extract(body,'$.intake_key'),'')<>'';
`)
	if e != nil {
		return nil, e
	}
	rows, e := db.Query(`PRAGMA table_info(proof_attempts)`)
	if e != nil {
		return nil, e
	}
	has := false
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var dflt any
		if e = rows.Scan(&cid, &name, &typ, &nn, &dflt, &pk); e != nil {
			rows.Close()
			return nil, e
		}
		if name == "disposition_history" {
			has = true
		}
	}
	rows.Close()
	if !has {
		_, e = db.Exec(`ALTER TABLE proof_attempts ADD COLUMN disposition_history TEXT NOT NULL DEFAULT '[]'`)
	}
	return &Repository{DB: db}, e
}
func job(ctx context.Context, tx *sql.Tx, kind, ref string) (int64, error) {
	n := time.Now().UnixMilli()
	res, e := tx.ExecContext(ctx, `INSERT INTO jobs(project_id,kind,state,priority,scheduled_at,payload,max_attempts,created_at,updated_at) VALUES('proof',?,'READY',100,?,?,1,?,?)`, kind, n, encode(map[string]string{"id": ref}), n, n)
	if e != nil {
		return 0, e
	}
	return res.LastInsertId()
}
func (r *Repository) Create(ctx context.Context, target, request, actor string, scope ...string) (*Check, error) {
	now := time.Now().UTC()
	c := &Check{ID: id(), TargetRef: target, Request: request, CreatedBy: actor, CurrentRevision: 1, RowVersion: 1, CreatedAt: now, UpdatedAt: now, Planning: "QUEUED", Draft: Contract{Actions: []string{}, Criteria: []Criterion{}}}
	if len(scope) > 0 {
		c.Team = scope[0]
	}
	if len(scope) > 1 {
		c.IntakeKey = scope[1]
	}
	if len(c.IntakeKey) > 128 {
		return nil, fmt.Errorf("idempotency key too long")
	}
	c.ContractHash = Hash(c.Draft)
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if c.IntakeKey != "" {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body FROM proof_checks WHERE json_extract(body,'$.created_by')=? AND COALESCE(json_extract(body,'$.team'),'')=? AND json_extract(body,'$.intake_key')=?`, actor, c.Team, c.IntakeKey).Scan(&body)
		if err == nil {
			var previous Check
			if err = json.Unmarshal([]byte(body), &previous); err != nil {
				return nil, err
			}
			if previous.TargetRef != target || previous.Request != request {
				return nil, ErrConflict
			}
			return &previous, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO proof_checks VALUES(?,?,?,?,?,?)`, c.ID, target, 1, 1, encode(c), c.CreatedAt.UnixMilli()); e != nil {
		return nil, e
	}
	if _, e = job(ctx, tx, string(model.JobProofPlan), c.ID); e != nil {
		return nil, e
	}
	return c, tx.Commit()
}
func (r *Repository) GetCheck(ctx context.Context, id string) (*Check, error) {
	var b string
	e := r.DB.QueryRowContext(ctx, `SELECT body FROM proof_checks WHERE id=?`, id).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	var c Check
	e = json.Unmarshal([]byte(b), &c)
	return &c, e
}
func (r *Repository) List(ctx context.Context, cursor string, teams ...string) ([]Check, error) {
	team := ""
	if len(teams) > 0 {
		team = teams[0]
	}
	rows, e := r.DB.QueryContext(ctx, `SELECT body, COALESCE((SELECT body FROM proof_attempts a WHERE a.check_id=proof_checks.id ORDER BY a.rowid DESC LIMIT 1),'') FROM proof_checks WHERE (?='' OR (created_at,id)<(SELECT created_at,id FROM proof_checks WHERE id=?)) AND (?='' OR json_extract(body,'$.team')=?) ORDER BY created_at DESC,id DESC LIMIT 51`, cursor, cursor, team, team)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Check{}
	for rows.Next() {
		var b, latest string
		var c Check
		if e = rows.Scan(&b, &latest); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &c); e != nil {
			return nil, e
		}
		if latest != "" {
			var a Attempt
			if e = json.Unmarshal([]byte(latest), &a); e != nil {
				return nil, e
			}
			c.LatestVerdict = a.Verdict
			for _, cr := range a.Results {
				if cr.Required && cr.Status == "UNKNOWN" {
					c.MissingCount++
				}
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (r *Repository) Patch(ctx context.Context, id string, version int, c Contract, audit ...string) (*Check, error) {
	old, e := r.GetCheck(ctx, id)
	if e != nil {
		return nil, e
	}
	actor, reason := "", ""
	if len(audit) > 0 {
		actor = audit[0]
	}
	if len(audit) > 1 {
		reason = audit[1]
	}
	old.RevisionHistory = append(old.RevisionHistory, Revision{At: time.Now().UTC(), Actor: actor, Reason: reason, Contract: old.Draft})
	old.Draft = c
	old.CurrentRevision++
	old.RowVersion++
	old.UpdatedAt = time.Now().UTC()
	old.ContractHash = Hash(c)
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_checks SET current_revision=?,row_version=?,body=? WHERE id=? AND row_version=?`, old.CurrentRevision, old.RowVersion, encode(old), id, version)
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrConflict
	}
	return old, nil
}
func (r *Repository) PlanResult(ctx context.Context, id string, suggestions json.RawMessage, errText string) error {
	c, e := r.GetCheck(ctx, id)
	if e != nil {
		return e
	}
	v := c.RowVersion
	c.RowVersion++
	c.UpdatedAt = time.Now().UTC()
	c.Planning = "READY"
	c.Suggestions = suggestions
	if len(suggestions) > 0 && len(c.Draft.Criteria) == 0 {
		var con Contract
		if e := json.Unmarshal(suggestions, &con); e != nil {
			return e
		}
		c.Draft = con
		c.CurrentRevision++
		c.ContractHash = Hash(con)
	}
	c.PlanError = errText
	c.Question = ""
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_checks SET current_revision=?,row_version=?,body=? WHERE id=? AND row_version=?`, c.CurrentRevision, c.RowVersion, encode(c), id, v)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	return nil
}
func (r *Repository) PlanQuestion(ctx context.Context, id, question string) error {
	if len(question) < 3 || len(question) > 500 {
		return fmt.Errorf("planner question length invalid")
	}
	c, e := r.GetCheck(ctx, id)
	if e != nil {
		return e
	}
	version := c.RowVersion
	c.RowVersion++
	c.UpdatedAt = time.Now().UTC()
	c.Planning = "READY"
	c.Question = question
	c.PlanError = ""
	c.Suggestions = nil
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_checks SET row_version=?,body=? WHERE id=? AND row_version=?`, c.RowVersion, encode(c), id, version)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	return nil
}
func (r *Repository) SavePlanObservation(ctx context.Context, id string, observation PlanObservation) (*Check, error) {
	c, e := r.GetCheck(ctx, id)
	if e != nil {
		return nil, e
	}
	version := c.RowVersion
	c.RowVersion++
	c.PlanObservation = observation
	c.UpdatedAt = time.Now().UTC()
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_checks SET row_version=?,body=? WHERE id=? AND row_version=?`, c.RowVersion, encode(c), id, version)
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrConflict
	}
	return c, nil
}
func (r *Repository) Attempt(ctx context.Context, id string) (*Attempt, error) {
	var b, dis, history string
	var cancel bool
	e := r.DB.QueryRowContext(ctx, `SELECT body,disposition,cancel_requested,disposition_history FROM proof_attempts WHERE id=?`, id).Scan(&b, &dis, &cancel, &history)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	var a Attempt
	if e = json.Unmarshal([]byte(b), &a); e != nil {
		return nil, e
	}
	if e = json.Unmarshal([]byte(history), &a.DispositionHistory); e != nil {
		return nil, e
	}
	a.Disposition = dis
	a.CancelRequested = cancel
	return &a, nil
}
func (r *Repository) Attempts(ctx context.Context, check string) ([]Attempt, error) {
	rows, e := r.DB.QueryContext(ctx, `SELECT body,disposition,cancel_requested,disposition_history FROM proof_attempts WHERE check_id=? ORDER BY rowid DESC`, check)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var b, d, history string
		var c bool
		if e = rows.Scan(&b, &d, &c, &history); e != nil {
			return nil, e
		}
		var a Attempt
		if e = json.Unmarshal([]byte(b), &a); e != nil {
			return nil, e
		}
		a.Disposition = d
		a.CancelRequested = c
		_ = json.Unmarshal([]byte(history), &a.DispositionHistory)
		out = append(out, a)
	}
	return out, rows.Err()
}
func (r *Repository) Approve(ctx context.Context, check string, ap Approval, actor string, reg Registry) (*Attempt, bool, error) {
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, false, e
	}
	defer tx.Rollback()
	hash := Hash(struct {
		Approval Approval
		Actor    string
	}{ap, actor})
	var prev, ph string
	e = tx.QueryRowContext(ctx, `SELECT id,payload_hash FROM proof_attempts WHERE check_id=? AND idem_key=?`, check, ap.IdempotencyKey).Scan(&prev, &ph)
	if e == nil {
		if ph != hash {
			return nil, false, ErrConflict
		}
		tx.Rollback()
		a, e := r.Attempt(ctx, prev)
		return a, true, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, false, e
	}
	var raw string
	if e = tx.QueryRowContext(ctx, `SELECT body FROM proof_checks WHERE id=?`, check).Scan(&raw); e != nil {
		return nil, false, ErrNotFound
	}
	var c Check
	if e = json.Unmarshal([]byte(raw), &c); e != nil {
		return nil, false, e
	}
	if ap.RegistryHash != Hash(reg.Targets[c.TargetRef]) {
		return nil, false, ErrConflict
	}
	if c.CurrentRevision != ap.Revision || c.ContractHash != ap.ContractHash || ap.Scope != c.TargetRef {
		return nil, false, ErrConflict
	}
	p, e := reg.Compile(c.TargetRef, c.Draft, sourceText(c))
	if e != nil {
		return nil, false, e
	}
	var active int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM proof_attempts WHERE state IN ('QUEUED','RUNNING')`).Scan(&active); e != nil {
		return nil, false, e
	}
	if active > 0 {
		return nil, false, ErrBusy
	}
	t := reg.Targets[c.TargetRef]
	a := &Attempt{ID: id(), CheckID: c.ID, Approval: ap, Actor: actor, ApprovedAt: time.Now().UTC(), Contract: c.Draft, Plan: p, Target: t, Fixture: t.Fixtures[c.Draft.Fixture], State: "QUEUED", Progress: "승인됨", Results: []CriterionResult{}, BaselineKind: "before-action", FixClaim: "Not proven"}
	if ap.Baseline != "" {
		var base string
		if e = tx.QueryRowContext(ctx, `SELECT body FROM proof_attempts WHERE id=? AND state='DONE'`, ap.Baseline).Scan(&base); e != nil {
			return nil, false, fmt.Errorf("baseline unavailable")
		}
		var b Attempt
		json.Unmarshal([]byte(base), &b)
		if b.CheckID != check || Hash(b.Contract) != Hash(c.Draft) || b.Verdict != "FAIL" || b.ObservedVersion == "" || b.ObservedVersion != b.VersionAfter || b.Target.BaseURL != t.BaseURL || b.Target.Environment != t.Environment || b.Target.PolicyVersion != t.PolicyVersion || Hash(b.Fixture) != Hash(a.Fixture) {
			return nil, false, fmt.Errorf("baseline is not comparable and versioned")
		}
		a.BaselineKind = "prior-deployment"
	}
	a.PlanHash = Hash(a.Plan)
	a.JobID, e = job(ctx, tx, string(model.JobProofRun), a.ID)
	if e != nil {
		return nil, false, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO proof_attempts(id,check_id,job_id,idem_key,payload_hash,state,body) VALUES(?,?,?,?,?,'QUEUED',?)`, a.ID, c.ID, a.JobID, ap.IdempotencyKey, hash, encode(a)); e != nil {
		return nil, false, e
	}
	return a, false, tx.Commit()
}
func (r *Repository) Update(ctx context.Context, a *Attempt) error {
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_attempts SET state=?,body=? WHERE id=? AND state IN ('QUEUED','RUNNING')`, a.State, encode(a), a.ID)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}
func (r *Repository) Cancel(ctx context.Context, id string) error {
	res, e := r.DB.ExecContext(ctx, `UPDATE proof_attempts SET cancel_requested=1 WHERE id=? AND state IN ('QUEUED','RUNNING')`, id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	return nil
}
func (r *Repository) Disposition(ctx context.Context, id, d string, audit ...string) error {
	if d != "OPEN" && d != "ACKNOWLEDGED" && d != "RESOLVED" && d != "DISMISSED" && d != "ACCEPTED" && d != "DEFERRED" && d != "REJECTED" {
		return fmt.Errorf("invalid disposition")
	}
	tx, e := r.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, `UPDATE proof_attempts SET disposition=? WHERE id=? AND state IN ('DONE','INTERRUPTED','CANCELLED')`, d, id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrConflict
	}
	actor, reason := "", ""
	if len(audit) > 0 {
		actor = audit[0]
	}
	if len(audit) > 1 {
		reason = audit[1]
	}
	if _, e = tx.ExecContext(ctx, `UPDATE proof_attempts SET disposition_history=json_insert(disposition_history,'$[#]',json(?)) WHERE id=?`, encode(DispositionEvent{Actor: actor, At: time.Now().UTC(), Disposition: d, Reason: reason}), id); e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Repository) Recover(ctx context.Context) error {
	rows, e := r.DB.QueryContext(ctx, `SELECT body FROM proof_attempts WHERE state IN ('QUEUED','RUNNING')`)
	if e != nil {
		return e
	}
	var list []Attempt
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		var a Attempt
		if e = json.Unmarshal([]byte(b), &a); e != nil {
			rows.Close()
			return e
		}
		list = append(list, a)
	}
	rows.Close()
	for _, a := range list {
		a.State = "INTERRUPTED"
		a.Progress = "서버가 재시작되어 중단되었습니다. 다시 승인해야 실행합니다."
		a.Verdict = "INCOMPLETE"
		now := time.Now().UTC()
		a.FinishedAt = &now
		existing := map[string]bool{}
		for _, v := range a.Results {
			existing[v.ID] = true
		}
		for _, c := range a.Contract.Criteria {
			if !existing[c.ID] {
				a.Results = append(a.Results, CriterionResult{ID: c.ID, Required: c.Required, Status: "UNKNOWN", Reason: "server restart"})
			}
		}
		a.Verdict = Verdict(a.Results)
		if a.Verdict == "PASS" {
			a.Verdict = "INCOMPLETE"
		}

		if e = r.Update(ctx, &a); e != nil {
			return e
		}
	}
	planRows, e := r.DB.QueryContext(ctx, `SELECT id FROM proof_checks WHERE json_extract(body,'$.planning')='QUEUED'`)
	if e != nil {
		return e
	}
	var checks []string
	for planRows.Next() {
		var id string
		if e = planRows.Scan(&id); e != nil {
			planRows.Close()
			return e
		}
		checks = append(checks, id)
	}
	planRows.Close()
	for _, id := range checks {
		if e = r.PlanResult(ctx, id, nil, "서버가 재시작되어 계획이 중단되었습니다. 새 요청으로 다시 시작하세요."); e != nil {
			return e
		}
	}
	if _, e = r.DB.ExecContext(ctx, `DELETE FROM resource_locks WHERE owner IN (SELECT 'proof:'||id FROM proof_attempts WHERE state IN ('DONE','INTERRUPTED','CANCELLED'))`); e != nil {
		return e
	}
	_, e = r.DB.ExecContext(ctx, `UPDATE jobs SET state='FAILED',last_error='proof service restart: no replay' WHERE kind IN ('PROOF_PLAN','PROOF_RUN') AND state IN ('READY','LEASED')`)
	return e
}
