package approval

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

type fakeCommenter struct {
	key, body string
	id        string
	err       error
	calls     int
}

func (f *fakeCommenter) Comment(_ context.Context, key, body string) (string, error) {
	f.calls++
	f.key, f.body = key, body
	return f.id, f.err
}

func newService(t *testing.T) (*Service, *store.Store, *fakeCommenter) {
	t.Helper()
	base := t.TempDir()
	cfg := &config.Config{}
	cfg.BaseDir = base
	cfg.Project.ID = "p"
	cfg.Target.BaseURL = "https://t.example.com"
	cfg.Target.AllowedHosts = []string{"t.example.com"}
	cfg.Evidence.Dir = "evidence"
	cfg.Policy.SoakPasses = 2
	cfg.Schedule.DailyAt = "09:00"
	cfg.Schedule.ActiveHours.TZ = "Asia/Seoul"
	cfg.Discovery.IssueKeyPattern = config.DefaultIssueKeyPattern
	st, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertProject(context.Background(), "p", cfg.Target.BaseURL); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCommenter{id: "77"}
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc := &Service{Cfg: cfg, St: st, Jira: fc, Log: log.New(io.Discard, "", 0),
		Now: func() time.Time { return time.Date(2026, 9, 10, 23, 30, 0, 0, seoul).UTC() }}
	return svc, st, fc
}

func addScenario(t *testing.T, st *store.Store, id string, state model.ScenarioState, kind, ref, reproduction string) {
	t.Helper()
	m := &model.Scenario{ID: id, ProjectID: "p", State: state, Fingerprint: "fp-" + id, Title: "탭 중복 재현", Class: "P1", Mutation: model.MutationReadOnly,
		OracleSource: "spec", Origin: "agent", SoakTarget: 2, SoakPasses: 1, ConsecutiveFailures: 3, CurrentVersion: 2,
		SourceKind: kind, SourceRef: ref, Reproduction: reproduction}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 2, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "agent"}
	if err := st.CreateScenario(context.Background(), m, v, nil); err != nil {
		t.Fatal(err)
	}
}

func TestApprovePendingBecomesDailyActiveAndComments(t *testing.T) {
	svc, st, fc := newService(t)
	ctx := context.Background()
	addScenario(t, st, "entry-tabs", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-123", `{"reproduced":true,"at_step":3,"symptom":"tabs duplicated","run_id":5}`)
	now := svc.Now()
	if _, err := st.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "entry-tabs", ScenarioVersion: 2, Browser: model.BrowserLightpanda, Outcome: model.OutcomeAppFailure,
		Attempt: 1, StartedAt: now.Add(-time.Minute), FinishedAt: now, Environment: "dev", EvidenceDir: filepath.Join(svc.Cfg.BaseDir, "evidence", "runs", "entry-tabs", "x")}); err != nil {
		t.Fatal(err)
	}

	r, err := svc.Approve(ctx, "entry-tabs", Options{})
	if err != nil {
		t.Fatal(err)
	}
	seoul, _ := time.LoadLocation("Asia/Seoul")
	wantDue := time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)
	if r.From != model.StatePendingApproval || r.To != model.StateActive || r.Cadence != "daily" || r.NextDueAt == nil || !r.NextDueAt.Equal(wantDue) {
		t.Fatalf("receipt = %+v", r)
	}
	sc, _ := st.GetScenario(ctx, "p", "entry-tabs")
	if sc.State != model.StateActive || sc.Cadence != "daily" || sc.ApprovedAt == nil || !sc.ApprovedAt.Equal(now) || sc.SoakPasses != 0 || sc.ConsecutiveFailures != 0 || sc.NextDueAt == nil || !sc.NextDueAt.Equal(wantDue) {
		t.Fatalf("scenario = %+v", sc)
	}
	if r.Jira == nil || r.Jira.Key != "PROJ-123" || r.Jira.CommentID != "77" || r.Jira.Error != "" || fc.calls != 1 {
		t.Fatalf("jira = %+v calls=%d", r.Jira, fc.calls)
	}
	body := fc.body
	lines := strings.Split(body, "\n")
	if len(lines) > 12 {
		t.Fatalf("body has %d lines:\n%s", len(lines), body)
	}
	for _, want := range []string{"[vigil] QA 스크립트 승인: entry-tabs v2, 탭 중복 재현", "· 재현/회귀 실행: vigil run entry-tabs", "APP_FAILURE (dev, 2026-09-10 23:30 KST), 재현됨",
		"· 발견 사항: 0건", "· 일일 자동 실행: 매일 09:00 Asia/Seoul", "· 증거: evidence/runs/entry-tabs/x"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "--env") {
		t.Fatalf("no read-only env configured, body must not advertise one:\n%s", body)
	}
	if msg := r.Message(seoul); !strings.Contains(msg, "정식 검사") || !strings.Contains(msg, "Jira PROJ-123 댓글 #77") {
		t.Fatalf("message = %q", msg)
	}
}

func TestApproveBodyAdvertisesReadOnlyEnv(t *testing.T) {
	svc, st, fc := newService(t)
	svc.Cfg.Target.Environments = map[string]config.Environment{
		"dev":  {BaseURL: "https://t.example.com", AllowedHosts: []string{"t.example.com"}},
		"prod": {BaseURL: "https://p.example.com", AllowedHosts: []string{"p.example.com"}, ReadOnly: true},
	}
	svc.Cfg.Target.DefaultEnv = "dev"
	addScenario(t, st, "s1", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-124", `{"reproduced":false}`)
	if _, err := svc.Approve(context.Background(), "s1", Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.body, "· 운영 확인: vigil run s1 --env prod") || !strings.Contains(fc.body, "마지막 결과: 없음, 재현 안 됨") {
		t.Fatalf("body:\n%s", fc.body)
	}
}

func TestApproveSurvivesCommentFailure(t *testing.T) {
	svc, st, fc := newService(t)
	fc.err = errors.New("comment not visible after create")
	addScenario(t, st, "s1", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-125", "")
	r, err := svc.Approve(context.Background(), "s1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.To != model.StateActive || r.Jira == nil || r.Jira.Error != "comment not visible after create" {
		t.Fatalf("receipt = %+v jira=%+v", r, r.Jira)
	}
	if sc, _ := st.GetScenario(context.Background(), "p", "s1"); sc.State != model.StateActive {
		t.Fatalf("state = %s", sc.State)
	}
	if !strings.Contains(r.Message(time.UTC), "댓글 실패") {
		t.Fatalf("message = %q", r.Message(time.UTC))
	}
}

func TestApproveSkipsJiraForNonIssueSources(t *testing.T) {
	svc, st, fc := newService(t)
	addScenario(t, st, "log1", model.StatePendingApproval, model.FeatureKindLog, "deadbeef", "")
	addScenario(t, st, "badref", model.StatePendingApproval, model.FeatureKindIssue, "not a key", "")
	for _, id := range []string{"log1", "badref"} {
		r, err := svc.Approve(context.Background(), id, Options{})
		if err != nil || r.To != model.StateActive || r.Jira != nil {
			t.Fatalf("%s: receipt = %+v err=%v", id, r, err)
		}
	}
	if fc.calls != 0 {
		t.Fatalf("jira called %d times", fc.calls)
	}
	// write-back off entirely
	svc.Jira = nil
	addScenario(t, st, "s2", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-129", "")
	if r, err := svc.Approve(context.Background(), "s2", Options{}); err != nil || r.Jira != nil {
		t.Fatalf("receipt = %+v err=%v", r, err)
	}
}

func TestApproveSoakPaths(t *testing.T) {
	svc, st, fc := newService(t)
	addScenario(t, st, "review", model.StateNeedsReview, "", "", "")
	addScenario(t, st, "pending", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-126", "")
	addScenario(t, st, "active", model.StateActive, "", "", "")
	ctx := context.Background()
	r, err := svc.Approve(ctx, "review", Options{})
	if err != nil || r.To != model.StateSoak || r.SoakTarget != 2 || r.Cadence != "" {
		t.Fatalf("needs_review: %+v %v", r, err)
	}
	sc, _ := st.GetScenario(ctx, "p", "review")
	if sc.State != model.StateSoak || sc.SoakPasses != 0 || sc.ConsecutiveFailures != 0 || sc.NextDueAt == nil || !sc.NextDueAt.Equal(svc.Now()) {
		t.Fatalf("scenario = %+v", sc)
	}
	r, err = svc.Approve(ctx, "pending", Options{Soak: true})
	if err != nil || r.To != model.StateSoak || r.Jira != nil || fc.calls != 0 {
		t.Fatalf("--soak: %+v %v calls=%d", r, err, fc.calls)
	}
	if sc, _ := st.GetScenario(ctx, "p", "pending"); sc.State != model.StateSoak || sc.Cadence != "" {
		t.Fatalf("scenario = %+v", sc)
	}
	var es *ErrState
	if _, err := svc.Approve(ctx, "active", Options{}); !errors.As(err, &es) {
		t.Fatalf("active: err = %v", err)
	}
	if _, err := svc.Approve(ctx, "missing", Options{}); !IsNotFound(err) {
		t.Fatalf("missing: err = %v", err)
	}
}

func TestRejectAnyState(t *testing.T) {
	svc, st, _ := newService(t)
	addScenario(t, st, "pending", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-127", "")
	r, err := svc.Reject(context.Background(), "pending")
	if err != nil || r.From != model.StatePendingApproval || r.To != model.StateRejected {
		t.Fatalf("receipt = %+v %v", r, err)
	}
	if sc, _ := st.GetScenario(context.Background(), "p", "pending"); sc.State != model.StateRejected {
		t.Fatalf("state = %s", sc.State)
	}
	if _, err := svc.Reject(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("missing: err = %v", err)
	}
}

func TestNewWiresJiraFromConfig(t *testing.T) {
	svc, _, _ := newService(t)
	cfg := svc.Cfg
	off := false
	cfg.Jira.CommentOnApprove = &off
	if s := New(cfg, svc.St, nil); s.Jira != nil {
		t.Fatal("comment_on_approve=false must disable the client")
	}
	on := true
	cfg.Jira.CommentOnApprove, cfg.Jira.DryRun, cfg.Jira.CLI = &on, true, "acli"
	s := New(cfg, svc.St, nil)
	if s.Jira == nil {
		t.Fatal("client expected")
	}
}

func TestApproveBodyListsOpenFindings(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	addScenario(t, st, "entry-tabs", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-123", "")
	sc, _ := st.GetScenario(ctx, "p", "entry-tabs")
	for i, f := range []model.Finding{
		{ScenarioID: "entry-tabs", Kind: "data_mismatch", Where: "/entry 카드", Expected: "api /api/x $.n = 3", Actual: "ui .n = 2"},
		{FeatureID: sc.OracleFeature, Kind: "display", Where: "/entry", Expected: "", Actual: "빈 목록 문구"},
		{ScenarioID: "entry-tabs", Kind: "domain_rule", Where: "/entry", Expected: "DATA-1", Actual: "위반"},
		{ScenarioID: "entry-tabs", Kind: "accessibility", Where: "/entry", Expected: "label", Actual: "none"},
		{ScenarioID: "other", Kind: "display", Where: "/other"},
	} {
		f.ProjectID = "p"
		if _, err := st.InsertFinding(ctx, &f); err != nil {
			t.Fatalf("finding %d: %v", i, err)
		}
	}
	body := svc.Body(ctx, sc, svc.Now())
	lines := strings.Split(body, "\n")
	if len(lines) > 12 {
		t.Fatalf("body has %d lines:\n%s", len(lines), body)
	}
	want := sc.OracleFeature != "" // feature-attached finding counts only when the scenario names a feature
	n := 3
	if want {
		n = 4
	}
	if !strings.Contains(body, "· 발견 사항: "+string(rune('0'+n))+"건") {
		t.Fatalf("count missing (want %d):\n%s", n, body)
	}
	// newest first, top 3 only, one line each
	if !strings.Contains(body, "- accessibility @ /entry: label ≠ none") || !strings.Contains(body, "- domain_rule @ /entry: DATA-1 ≠ 위반") {
		t.Fatalf("top findings missing:\n%s", body)
	}
	if strings.Contains(body, "/other") || strings.Count(body, "\n- ") != 3 {
		t.Fatalf("expected exactly three one-liners for this scenario:\n%s", body)
	}
	if !strings.Contains(body, "· 일일 자동 실행") {
		t.Fatalf("tail lines lost:\n%s", body)
	}
}

// H-1: old rows carry only `reproduced`; new rows carry the three-way verdict.
func TestParseReproductionVerdictCompatibility(t *testing.T) {
	cases := []struct {
		raw        string
		verdict    string
		reproduced bool
	}{
		{`{"reproduced":true,"at_step":3}`, VerdictConfirmed, true},
		{`{"reproduced":false}`, VerdictUnconfirmed, false},
		{`{"verdict":"confirmed","reproduced":true,"at_step":13,"claimed_step":14}`, VerdictConfirmed, true},
		{`{"verdict":"disputed","reproduced":false,"at_step":13,"claimed_step":9,"why":"실행은 13단계"}`, VerdictDisputed, false},
		{`{"verdict":"unconfirmed","reproduced":false}`, VerdictUnconfirmed, false},
	}
	for _, tc := range cases {
		rep, ok := ParseReproduction(tc.raw)
		if !ok || rep.Verdict != tc.verdict || rep.Reproduced != tc.reproduced {
			t.Fatalf("%s → %+v ok=%v", tc.raw, rep, ok)
		}
	}
	if _, ok := ParseReproduction("  "); ok {
		t.Fatal("empty reproduction must not parse")
	}
}

// A disputed verdict tells the human why the run and the agent disagree.
func TestApproveBodyShowsDisputedVerdict(t *testing.T) {
	svc, st, fc := newService(t)
	addScenario(t, st, "s2", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-125",
		`{"verdict":"disputed","reproduced":false,"at_step":13,"claimed_step":9,"why":"실행은 13단계에서 실패했지만 에이전트는 9단계를 주장했습니다"}`)
	if _, err := svc.Approve(context.Background(), "s2", Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.body, "판정 불일치 (실행은 13단계에서 실패했지만 에이전트는 9단계를 주장했습니다)") {
		t.Fatalf("body:\n%s", fc.body)
	}
}

// J-3: the approval comment's 마지막 결과 line carries the same one sentence.
func TestApproveBodyAppendsCauseOfFailingRun(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	addScenario(t, st, "entry-tabs", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-123", "")
	sc, _ := st.GetScenario(ctx, "p", "entry-tabs")
	now := svc.Now()
	if _, err := st.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "entry-tabs", ScenarioVersion: 2,
		Browser: model.BrowserChromium, Outcome: model.OutcomeScriptDrift, StartedAt: now, FinishedAt: now,
		Environment: "stg", FailedStep: 4, FailedAction: "click", Expected: "element present", Actual: "0 matches",
		Error: `step 4 (click): locate by=role role=button name="가져오기": 0 matches`}); err != nil {
		t.Fatal(err)
	}
	body := svc.Body(ctx, sc, now)
	want := `4단계에서 누르려던 "가져오기" 버튼이 화면에 없습니다.`
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "· 마지막 결과:") {
			line = l
		}
	}
	if !strings.Contains(line, want) || !strings.Contains(line, "SCRIPT_DRIFT") {
		t.Fatalf("마지막 결과 line = %q\n%s", line, body)
	}
	if len(strings.Split(body, "\n")) > 12 {
		t.Fatalf("body grew past 12 lines:\n%s", body)
	}
}

// A passing last run leaves the line as it was.
func TestApproveBodyKeepsPassLineUnchanged(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	addScenario(t, st, "entry-tabs", model.StatePendingApproval, model.FeatureKindIssue, "PROJ-123", "")
	sc, _ := st.GetScenario(ctx, "p", "entry-tabs")
	now := svc.Now()
	if _, err := st.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "entry-tabs", ScenarioVersion: 2,
		Browser: model.BrowserChromium, Outcome: model.OutcomePass, StartedAt: now, FinishedAt: now, Environment: "stg"}); err != nil {
		t.Fatal(err)
	}
	body := svc.Body(ctx, sc, now)
	if strings.Contains(body, "화면에 없습니다") || !strings.Contains(body, "PASS") {
		t.Fatalf("pass body:\n%s", body)
	}
}
