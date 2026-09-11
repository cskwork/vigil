// Package approval is the one place the human decision on a script lives:
// `vigil approve/reject` and the dashboard buttons both call it, so the state
// transition, the daily cadence and the Jira write-back cannot drift apart.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/explain"
	"vigil/internal/jira"
	"vigil/internal/model"
	"vigil/internal/store"
)

// fallbackIssueKey is used when discovery.issue_key_pattern is empty or invalid.
const fallbackIssueKey = `^[A-Z][A-Z0-9]+-\d+$`

// Commenter posts one comment to an issue (jira.Client, or a fake in tests).
type Commenter interface {
	Comment(ctx context.Context, key, body string) (id string, err error)
}

// Service performs approvals against the store and the configured Jira client.
type Service struct {
	Cfg *config.Config
	St  *store.Store
	// Jira is nil when write-back is off; Service.New wires it from config.
	Jira Commenter
	Log  *log.Logger
	Now  func() time.Time
}

// New builds a Service whose Jira client follows jira.{cli, comment_on_approve, dry_run}.
func New(cfg *config.Config, st *store.Store, logger *log.Logger) *Service {
	s := &Service{Cfg: cfg, St: st, Log: logger, Now: func() time.Time { return time.Now().UTC() }}
	if cfg.Jira.CommentsOnApprove() {
		c := &jira.Client{CLI: cfg.Jira.CLI}
		if cfg.Jira.DryRun {
			c.DryRunDir = filepath.Join(cfg.Abs(cfg.Evidence.Dir), "jira")
		}
		s.Jira = c
	}
	return s
}

// Options tunes Approve.
type Options struct {
	// Soak forces the SOAK path (soak counter reset, due now) even for PENDING_APPROVAL.
	Soak bool
}

// JiraResult is what happened to the source issue during an approval.
type JiraResult struct {
	Key        string `json:"key"`
	CommentID  string `json:"comment_id,omitempty"`
	DryRunPath string `json:"dry_run_path,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Receipt reports one decision.
type Receipt struct {
	ID        string
	From, To  model.ScenarioState
	Cadence   string
	NextDueAt *time.Time
	// SoakTarget is set on the SOAK path: clean passes needed before ACTIVE.
	SoakTarget int
	Jira       *JiraResult
}

// Message is the receipt in one Korean line for the CLI and the dashboard.
func (r Receipt) Message(loc *time.Location) string {
	var b strings.Builder
	switch r.To {
	case model.StateActive:
		fmt.Fprintf(&b, "%s: %s → 정식 검사 (매일 실행", r.ID, label(r.From))
		if r.NextDueAt != nil {
			fmt.Fprintf(&b, ", 다음 실행 %s", r.NextDueAt.In(loc).Format("01/02 15:04 MST"))
		}
		b.WriteString(")")
	case model.StateSoak:
		fmt.Fprintf(&b, "%s: %s → 안정화 중 (지금 실행, %d회 연속 정상이면 정식 검사)", r.ID, label(r.From), r.SoakTarget)
	case model.StateRejected:
		fmt.Fprintf(&b, "%s: %s → 제외", r.ID, label(r.From))
	default:
		fmt.Fprintf(&b, "%s: %s → %s", r.ID, label(r.From), label(r.To))
	}
	if j := r.Jira; j != nil {
		switch {
		case j.Error != "":
			fmt.Fprintf(&b, " · Jira %s 댓글 실패: %s", j.Key, j.Error)
		case j.DryRunPath != "":
			fmt.Fprintf(&b, " · Jira %s 댓글(모의): %s", j.Key, j.DryRunPath)
		default:
			fmt.Fprintf(&b, " · Jira %s 댓글 #%s", j.Key, j.CommentID)
		}
	}
	return b.String()
}

func label(s model.ScenarioState) string {
	switch s {
	case model.StatePendingApproval:
		return "승인 대기"
	case model.StateNeedsReview:
		return "사람 확인 필요"
	case model.StateActive:
		return "정식 검사"
	case model.StateSoak:
		return "안정화 중"
	case model.StateCandidate:
		return "후보"
	case model.StateQuarantined:
		return "잠시 제외"
	case model.StateRejected:
		return "제외"
	}
	return string(s)
}

// ErrState is returned when the scenario's state cannot be approved.
type ErrState struct {
	ID    string
	State model.ScenarioState
}

func (e *ErrState) Error() string {
	return fmt.Sprintf("scenario %s is %s; only PENDING_APPROVAL/NEEDS_REVIEW/CANDIDATE/QUARANTINED/REJECTED can be approved", e.ID, e.State)
}

// Approve promotes a script. PENDING_APPROVAL → ACTIVE with cadence daily (next
// due = next schedule.daily_at) and a Jira comment on the source issue; every
// other approvable state (or opts.Soak) → SOAK, due now. A failed comment never
// undoes the approval: it is logged and reported in the receipt.
func (s *Service) Approve(ctx context.Context, id string, opts Options) (Receipt, error) {
	p := s.Cfg.Project.ID
	sc, err := s.St.GetScenario(ctx, p, id)
	if err != nil {
		return Receipt{}, fmt.Errorf("scenario %s: %w", id, err)
	}
	r := Receipt{ID: sc.ID, From: sc.State}
	switch sc.State {
	case model.StatePendingApproval, model.StateNeedsReview, model.StateCandidate, model.StateQuarantined, model.StateRejected:
	default:
		return r, &ErrState{ID: sc.ID, State: sc.State}
	}
	now := s.now()
	if sc.State != model.StatePendingApproval || opts.Soak {
		if err := s.St.SetScenarioSoak(ctx, p, sc.ID, now); err != nil {
			return r, err
		}
		r.To, r.NextDueAt, r.SoakTarget = model.StateSoak, &now, s.Cfg.Policy.SoakPasses
		return r, nil
	}
	due := s.Cfg.NextDailyRun(now)
	if err := s.St.SetScenarioApproved(ctx, p, sc.ID, config.DailyCadence, now, due); err != nil {
		return r, err
	}
	r.To, r.Cadence, r.NextDueAt = model.StateActive, config.DailyCadence, &due
	if key, ok := s.issueKey(sc); ok && s.Jira != nil {
		r.Jira = s.comment(ctx, key, sc, now)
	}
	return r, nil
}

// Reject parks any script as REJECTED and closes its regression incidents.
func (s *Service) Reject(ctx context.Context, id string) (Receipt, error) {
	p := s.Cfg.Project.ID
	sc, err := s.St.GetScenario(ctx, p, id)
	if err != nil {
		return Receipt{}, fmt.Errorf("scenario %s: %w", id, err)
	}
	if err := s.St.SetScenarioState(ctx, p, sc.ID, model.StateRejected); err != nil {
		return Receipt{}, err
	}
	_ = s.St.ResolveIncidents(ctx, p, model.IncidentAppRegression, sc.ID)
	return Receipt{ID: sc.ID, From: sc.State, To: model.StateRejected}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// issueKey returns the source issue when the script came from a Jira issue.
func (s *Service) issueKey(sc *model.Scenario) (string, bool) {
	if sc.SourceKind != model.FeatureKindIssue {
		return "", false
	}
	ref := strings.TrimSpace(sc.SourceRef)
	if ref == "" {
		return "", false
	}
	pat := s.Cfg.Discovery.IssueKeyPattern
	re, err := regexp.Compile("^(?:" + pat + ")$")
	if pat == "" || err != nil {
		re = regexp.MustCompile(fallbackIssueKey)
	}
	if !re.MatchString(ref) {
		return "", false
	}
	return ref, true
}

func (s *Service) comment(ctx context.Context, key string, sc *model.Scenario, now time.Time) *JiraResult {
	res := &JiraResult{Key: key}
	body := s.Body(ctx, sc, now)
	id, err := s.Jira.Comment(ctx, key, body)
	switch {
	case err != nil:
		res.Error = err.Error()
		if s.Log != nil {
			s.Log.Printf("approve %s: jira comment on %s failed: %v", sc.ID, key, err)
		}
	case jira.IsDryRun(id):
		res.DryRunPath = jira.DryRunPath(id)
	default:
		res.CommentID = id
	}
	return res
}

// Reproduction is the stored verdict of a reproduce script's validation run.
type Reproduction struct {
	// Verdict is confirmed | unconfirmed | disputed; rows written before the
	// three-way verdict existed carry only Reproduced and are mapped on read.
	Verdict     string `json:"verdict,omitempty"`
	Reproduced  bool   `json:"reproduced"`
	AtStep      int    `json:"at_step"`
	ClaimedStep int    `json:"claimed_step,omitempty"`
	Symptom     string `json:"symptom"`
	Why         string `json:"why,omitempty"`
	RunID       int64  `json:"run_id"`
}

// Reproduction verdict values (mirrors the orchestrator's record).
const (
	VerdictConfirmed   = "confirmed"
	VerdictUnconfirmed = "unconfirmed"
	VerdictDisputed    = "disputed"
)

// VerdictLabel is the Korean label shown for a verdict.
func VerdictLabel(verdict string) string {
	switch verdict {
	case VerdictConfirmed:
		return "재현됨"
	case VerdictDisputed:
		return "판정 불일치"
	default:
		return "재현 안 됨"
	}
}

// ParseReproduction decodes scenarios.reproduction; ok is false when empty/invalid.
func ParseReproduction(raw string) (Reproduction, bool) {
	var r Reproduction
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &r) != nil {
		return Reproduction{}, false
	}
	if r.Verdict == "" { // pre-verdict row: the boolean was the whole answer
		r.Verdict = VerdictUnconfirmed
		if r.Reproduced {
			r.Verdict = VerdictConfirmed
		}
	}
	r.Reproduced = r.Verdict == VerdictConfirmed
	return r, true
}

// firstLine keeps the approval comment to one line per field.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var kst = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Seoul"); err == nil {
		return loc
	}
	return time.FixedZone("KST", 9*3600)
}()

// Body renders the Korean approval comment (≤ 12 lines).
func (s *Service) Body(ctx context.Context, sc *model.Scenario, now time.Time) string {
	title := sc.Title
	if title == "" {
		title = sc.ID
	}
	lines := []string{
		fmt.Sprintf("[vigil] QA 스크립트 승인: %s v%d, %s", sc.ID, max(sc.CurrentVersion, 1), title),
		fmt.Sprintf("· 재현/회귀 실행: vigil run %s", sc.ID),
	}
	if ro := s.readOnlyEnv(); ro != "" {
		lines = append(lines, fmt.Sprintf("· 운영 확인: vigil run %s --env %s", sc.ID, ro))
	}
	verdict := "해당 없음"
	if rep, ok := ParseReproduction(sc.Reproduction); ok {
		verdict = VerdictLabel(rep.Verdict)
		if rep.Verdict == VerdictDisputed && rep.Why != "" {
			verdict += " (" + firstLine(rep.Why) + ")"
		}
	}
	last := "없음"
	evidenceDir := ""
	cause := ""
	if runs, err := s.St.ListRuns(ctx, s.Cfg.Project.ID, sc.ID, 1); err == nil && len(runs) > 0 {
		r := runs[0]
		env := r.Environment
		if env == "" {
			env = s.Cfg.DefaultEnv().Name
		}
		at := r.FinishedAt
		if at.IsZero() {
			at = r.StartedAt
		}
		last = fmt.Sprintf("%s (%s, %s KST)", r.Outcome, env, at.In(kst).Format("2006-01-02 15:04"))
		evidenceDir = r.EvidenceDir
		// The failing run says why in one sentence, the same one the dashboard
		// and the incident file carry (internal/explain).
		if r.Outcome != model.OutcomePass {
			cause = explain.ForRun(r, sc).Headline
		}
	}
	findings, _ := s.St.ListOpenFindingsFor(ctx, s.Cfg.Project.ID, sc.ID, sc.OracleFeature)
	lastLine := fmt.Sprintf("· 마지막 결과: %s, %s", last, verdict)
	if cause != "" {
		lastLine += " / " + cause
	}
	lines = append(lines,
		lastLine,
		fmt.Sprintf("· 발견 사항: %d건", len(findings)),
	)
	for i, f := range findings {
		if i == 3 {
			break
		}
		lines = append(lines, findingLine(f))
	}
	lines = append(lines,
		fmt.Sprintf("· 일일 자동 실행: 매일 %s %s", s.Cfg.Schedule.DailyAt, s.Cfg.DailyLocation()),
		fmt.Sprintf("· 증거: %s", s.relEvidence(evidenceDir, sc.ID)),
	)
	return strings.Join(lines, "\n")
}

// findingLine renders one OPEN finding for the comment: - <kind> @ <where>: <expected> ≠ <actual>
func findingLine(f *model.Finding) string {
	where := f.Where
	if where == "" {
		where = "-"
	}
	return fmt.Sprintf("- %s @ %s: %s ≠ %s", f.Kind, clip(where, 80), clip(orDash(f.Expected), 80), clip(orDash(f.Actual), 80))
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// clip shortens on rune boundaries so Korean text is never cut mid-character.
func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// readOnlyEnv names the first read-only environment (sorted), "" when none.
func (s *Service) readOnlyEnv() string {
	names := make([]string, 0, len(s.Cfg.Target.Environments))
	for name, e := range s.Cfg.Target.Environments {
		if e.ReadOnly {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// relEvidence renders the run's evidence dir relative to the config dir, falling
// back to the scenario's runs folder when the last run left no directory.
func (s *Service) relEvidence(dir, scenarioID string) string {
	if dir == "" {
		dir = filepath.Join(s.Cfg.Abs(s.Cfg.Evidence.Dir), "runs", scenarioID)
	}
	if rel, err := filepath.Rel(s.Cfg.Abs("."), dir); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(dir)
}

// IsNotFound reports whether err came from an unknown scenario id.
func IsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
