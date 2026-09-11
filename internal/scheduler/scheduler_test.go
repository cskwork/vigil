package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/evidence"
	"vigil/internal/gate"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// ---- fakes -------------------------------------------------------------------

type fakeRunner struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, spec runner.Spec) (*runner.Result, error)
}

type preemptIntegrationAgent struct {
	oldStarted chan struct{}
	userDone   chan struct{}
	onceOld    sync.Once
	onceUser   sync.Once
}

func (a *preemptIntegrationAgent) Run(ctx context.Context, req agent.Request, _ string) (*agent.Result, error) {
	switch req.FeatureID {
	case "old":
		a.onceOld.Do(func() { close(a.oldStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	case "user":
		a.onceUser.Do(func() { close(a.userDone) })
	}
	return &agent.Result{Decision: agent.DecisionNoNewCoverage}, nil
}

func (a *preemptIntegrationAgent) Plan(context.Context, string, string) (*agent.Plan, error) {
	return nil, errors.New("not used")
}
func (a *preemptIntegrationAgent) Reparse(string) (*agent.Result, error) {
	return nil, errors.New("not used")
}
func (a *preemptIntegrationAgent) Doctor(context.Context) error { return nil }

func (f *fakeRunner) Run(ctx context.Context, spec runner.Spec) (*runner.Result, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(n, spec)
}

type fakeOrch struct {
	mu               sync.Mutex
	planned          []string
	enqueued         []string
	afterRun         []model.Outcome
	agent            []int64
	afterErr         error
	supervised       int
	supervisorResult *orchestrator.SupervisorResult
	agentFn          func(context.Context, *model.Job) error
}

func (o *fakeOrch) SupervisorTick(context.Context) (*orchestrator.SupervisorResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.supervised++
	return o.supervisorResult, nil
}

func (o *fakeOrch) supervisedCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.supervised
}

func (o *fakeOrch) PlanFeature(ctx context.Context, f *model.Feature) (*orchestrator.Decision, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.planned = append(o.planned, f.ID)
	return &orchestrator.Decision{Action: model.ActionRunScript, Reason: "fake"}, nil
}
func (o *fakeOrch) EnqueueForFeature(ctx context.Context, f *model.Feature, d *orchestrator.Decision) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.enqueued = append(o.enqueued, f.ID)
	return nil
}
func (o *fakeOrch) AfterRun(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.afterRun = append(o.afterRun, run.Outcome)
	return o.afterErr
}
func (o *fakeOrch) HandleAgentJob(ctx context.Context, job *model.Job) error {
	o.mu.Lock()
	o.agent = append(o.agent, job.ID)
	fn := o.agentFn
	o.mu.Unlock()
	if fn != nil {
		return fn(ctx, job)
	}
	return nil
}

type fakeGate struct {
	state  model.Readiness
	marker string
}

func (g *fakeGate) Check(ctx context.Context, f *model.Feature, last string) (model.Readiness, string, error) {
	if g.marker != "" {
		return g.state, g.marker, nil
	}
	return g.state, last, nil
}

type fakeIngest struct {
	events []model.FeatureEvent
	polls  int
}

func (i *fakeIngest) Name() string { return "fake" }
func (i *fakeIngest) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	i.polls++
	if i.polls == 1 {
		return i.events, nil
	}
	return nil, nil
}
func (i *fakeIngest) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	return i.events, nil
}

// ---- helpers -----------------------------------------------------------------

const scenarioYAML = `scenario:
  id: %s
  version: 1
  class: %s
  mutation: %s
covers:
  feature: training-entry-page
  capability: training.entry
steps:
  - goto: /app/training-entry
  - assert_text: { value: 초등 }
assert:
  no_http_5xx: true
oracle:
  source: contract
`

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	c := &config.Config{}
	c.BaseDir = dir
	c.Project.ID = "p"
	c.Target.BaseURL = "https://example.test"
	c.Target.AllowedHosts = []string{"example.test"}
	c.Browser.Primary = "lightpanda"
	c.Browser.RunTimeout.Duration = 3 * time.Minute
	c.Browser.StepTimeout.Duration = 15 * time.Second
	c.Workers.Functional = 1
	c.State.Path = filepath.Join(dir, ".vigil", "state.db")
	c.Evidence.Dir = filepath.Join(dir, "evidence")
	c.Evidence.RetainPassDays = 3
	c.Evidence.RetainFailDays = 30
	c.Budget.BrowserMinutesPerHour = 60
	c.Policy.RetryOnFail = 1
	c.Schedule.Soak.Duration = 10 * time.Minute
	c.Schedule.P0.Duration = 15 * time.Minute
	c.Schedule.P1.Duration = time.Hour
	c.Schedule.P2.Duration = 6 * time.Hour
	c.Schedule.FailureBackoff.Duration = 5 * time.Minute
	c.Schedule.Tick.Duration = 50 * time.Millisecond
	c.Discovery.PollInterval.Duration = 100 * time.Millisecond
	c.Personas = map[string]config.Persona{"training_teacher": {}}
	return c
}

type harness struct {
	cfg  *config.Config
	st   *store.Store
	run  *fakeRunner
	orch *fakeOrch
	s    *Scheduler
}

func newHarness(t *testing.T, run *fakeRunner, gate Gate, in *fakeIngest) *harness {
	t.Helper()
	cfg := testCfg(t)
	st, err := store.Open(cfg.State.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertProject(context.Background(), cfg.Project.ID, cfg.Target.BaseURL); err != nil {
		t.Fatal(err)
	}
	orch := &fakeOrch{}
	var adapter ingestAdapter
	if in != nil {
		adapter = in
	}
	s := NewWith(cfg, st, orch, run, gate, adapter)
	s.Log = log.New(io.Discard, "", 0)
	s.TargetHealthy = func(context.Context) bool { return true }
	return &harness{cfg: cfg, st: st, run: run, orch: orch, s: s}
}

// ingestAdapter mirrors ingest.Adapter so a nil *fakeIngest stays a nil interface.
type ingestAdapter interface {
	Name() string
	Poll(ctx context.Context) ([]model.FeatureEvent, error)
	History(ctx context.Context, limit int) ([]model.FeatureEvent, error)
}

func (h *harness) addScenario(t *testing.T, id, class string, state model.ScenarioState, mutation model.Mutation, locks []string, failures int) {
	t.Helper()
	y := sprintf(scenarioYAML, id, class, string(mutation))
	m := &model.Scenario{ID: id, ProjectID: h.cfg.Project.ID, State: state, Fingerprint: "fp-" + id, Class: class, Mutation: mutation, Locks: locks,
		OracleSource: "contract", Origin: "seed", ConsecutiveFailures: failures, SoakTarget: 3}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: y, Fingerprint: "fp-" + id, CreatedBy: "seed"}
	links := []model.CoverageLink{{ScenarioID: id, LinkType: model.LinkFeature, LinkValue: "training-entry-page"}}
	if err := h.st.CreateScenario(context.Background(), m, v, links); err != nil {
		t.Fatal(err)
	}
}

func sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

func passResult(spec runner.Spec) *runner.Result {
	now := time.Now().UTC()
	_ = os.WriteFile(filepath.Join(spec.EvidenceDir, "steps.json"), []byte("[]"), 0o644)
	return &runner.Result{Passed: true, Browser: spec.Browser, StartedAt: now.Add(-1500 * time.Millisecond), FinishedAt: now,
		Artifacts: map[string]string{"steps": filepath.Join(spec.EvidenceDir, "steps.json")}}
}

func failResult(spec runner.Spec) *runner.Result {
	now := time.Now().UTC()
	return &runner.Result{Passed: false, Class: runner.FailAssertion, Browser: spec.Browser, StartedAt: now.Add(-time.Second), FinishedAt: now,
		FailedStep: &runner.StepResult{Index: 2, Kind: "assert_text", Expected: "초등", Actual: "(missing)", Error: "text not found"}}
}

// ---- tests -------------------------------------------------------------------

func TestTickEnqueuesDueDedupAndPriority(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "p0-active", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "soaking", "P1", model.StateSoak, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "failing-p2", "P2", model.StateActive, model.MutationReadOnly, nil, 2)
	h.addScenario(t, "reviewed", "P1", model.StateNeedsReview, model.MutationReadOnly, nil, 0)

	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Tick(ctx); err != nil { // second tick must dedup
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 50)
	if len(jobs) != 3 {
		t.Fatalf("expected 3 READY jobs (dedup), got %d", len(jobs))
	}
	want := []struct {
		id   string
		prio int
	}{{"failing-p2", model.PriorityRecentFailure}, {"soaking", model.PrioritySoak}, {"p0-active", model.PriorityP0}}
	for i, w := range want {
		if jobs[i].ScenarioID != w.id || jobs[i].Priority != w.prio || jobs[i].Browser != model.BrowserLightpanda || jobs[i].Kind != model.JobRunScenario {
			t.Fatalf("job %d = %s/%d/%s, want %s/%d", i, jobs[i].ScenarioID, jobs[i].Priority, jobs[i].Browser, w.id, w.prio)
		}
	}
}

func TestTickBudgetDeferral(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "p0", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "p2", "P2", model.StateActive, model.MutationReadOnly, nil, 0)
	limit := int64(h.cfg.Budget.BrowserMinutesPerHour) * 60_000

	// 85% used → P2 deferred, P0 still enqueued
	_ = h.st.RecordBudget(ctx, "p", "browser", limit*85/100)
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 50)
	if len(jobs) != 1 || jobs[0].ScenarioID != "p0" {
		t.Fatalf("soft pressure: got %d jobs (%+v)", len(jobs), jobs)
	}
	// 100% used → everything deferred
	_ = h.st.RecordBudget(ctx, "p", "browser", limit)
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, _ = h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 50)
	if len(jobs) != 1 {
		t.Fatalf("hard pressure: expected still 1 job, got %d", len(jobs))
	}
}

func TestRunScenarioNowPassFlow(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) {
		if spec.BaseURL != "https://example.test" || spec.Scenario == nil || spec.Browser != model.BrowserLightpanda {
			t.Errorf("spec: %+v", spec)
		}
		return passResult(spec), nil
	}}
	h := newHarness(t, fr, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "landing", "P0", model.StateActive, model.MutationReadOnly, []string{"acct-1"}, 0)
	_, _ = h.st.UpsertFeature(ctx, "p", model.FeatureEvent{FeatureID: "training-entry-page", Status: "shipped", ShippedSHA: "6f22ff7a1dcc", ShippedAt: time.Now()})

	run, err := h.s.RunScenarioNow(ctx, "landing", "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != model.OutcomePass || run.Attempt != 1 || run.ScenarioVersion != 1 || run.ShippedSHA != "6f22ff7a1dcc" || run.FeatureID != "training-entry-page" {
		t.Fatalf("run: %+v", run)
	}
	if !evidence.IsPass(run.EvidenceDir) {
		t.Fatal("PASS marker missing")
	}
	if _, err := os.Stat(filepath.Join(run.EvidenceDir, "result.json")); err != nil {
		t.Fatal("result.json missing")
	}
	if used, _ := h.st.BudgetUsed(ctx, "p", "browser", time.Hour); used < 1000 {
		t.Fatalf("budget not recorded: %d", used)
	}
	m, _ := h.st.GetScenario(ctx, "p", "landing")
	if m.NextDueAt == nil || m.NextDueAt.Before(time.Now().Add(10*time.Minute)) || m.LastOutcome != model.OutcomePass {
		t.Fatalf("next due / outcome not set: %+v", m)
	}
	if len(h.orch.afterRun) != 1 || h.orch.afterRun[0] != model.OutcomePass {
		t.Fatalf("AfterRun calls: %v", h.orch.afterRun)
	}
	if ok, _ := h.st.TryAcquireLocks(ctx, []string{"acct-1"}, "someone-else", time.Minute); !ok {
		t.Fatal("lock not released after run")
	}
	if n, _ := h.st.CountJobs(ctx, "p", model.JobDone); n != 1 {
		t.Fatalf("job not DONE: %d", n)
	}
	runs, _ := h.st.ListRuns(ctx, "p", "landing", 10)
	if len(runs) != 1 {
		t.Fatalf("runs persisted: %d", len(runs))
	}
}

func TestRetryThenFlake(t *testing.T) {
	fr := &fakeRunner{fn: func(call int, spec runner.Spec) (*runner.Result, error) {
		if call == 1 {
			return failResult(spec), nil
		}
		return passResult(spec), nil
	}}
	h := newHarness(t, fr, nil, nil)
	h.addScenario(t, "flaky", "P1", model.StateActive, model.MutationReadOnly, nil, 0)
	run, err := h.s.RunScenarioNow(context.Background(), "flaky", "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != model.OutcomeQAFlake || run.Attempt != 2 || fr.calls != 2 {
		t.Fatalf("run %+v calls=%d", run, fr.calls)
	}
	entries, _ := os.ReadDir(filepath.Join(h.s.Evidence().RunsDir(), "flaky"))
	if len(entries) != 2 {
		t.Fatalf("expected 2 attempt dirs, got %d", len(entries))
	}
}

func TestFailureRecordsExpectedActual(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return failResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	h.orch.afterErr = errors.New("orchestrator not implemented") // must be logged, not fatal
	h.addScenario(t, "broken", "P1", model.StateActive, model.MutationReversible, []string{"acct"}, 0)
	run, err := h.s.RunScenarioNow(context.Background(), "broken", "")
	if err != nil {
		t.Fatal(err)
	}
	// reversible → no cheap retry; classify stub without a classifier says NEEDS_REVIEW
	if fr.calls != 1 || run.Attempt != 1 || run.Outcome == model.OutcomePass {
		t.Fatalf("run %+v calls=%d", run, fr.calls)
	}
	if run.FailedStep != 2 || run.FailedAction != "assert_text" || run.Expected != "초등" || run.Actual != "(missing)" {
		t.Fatalf("expected/actual not captured: %+v", run)
	}
	if evidence.IsPass(run.EvidenceDir) {
		t.Fatal("failure must not carry PASS marker")
	}
	m, _ := h.st.GetScenario(context.Background(), "p", "broken")
	if m.NextDueAt == nil || m.NextDueAt.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("failure backoff not applied: %+v", m.NextDueAt)
	}
}

func TestLockConflictRequeues(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "teacher-entry", "P0", model.StateActive, model.MutationReversible, []string{"training-teacher-e-math"}, 0)
	if ok, _ := h.st.TryAcquireLocks(ctx, []string{"training-teacher-e-math"}, "other-worker", time.Minute); !ok {
		t.Fatal("pre-acquire")
	}
	m, _ := h.st.GetScenario(ctx, "p", "teacher-entry")
	id, _, err := h.s.EnqueueScenario(ctx, m, model.PriorityP0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.RunJobNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if fr.calls != 0 {
		t.Fatal("runner must not run while the lock is held (AC-16)")
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	if len(jobs) != 1 || jobs[0].ID != id || !jobs[0].ScheduledAt.After(time.Now().Add(20*time.Second)) || jobs[0].LastError == "" {
		t.Fatalf("job not requeued with delay: %+v", jobs)
	}
	if runs, _ := h.st.ListRuns(ctx, "p", "teacher-entry", 10); len(runs) != 0 {
		t.Fatal("no run must be recorded on lock conflict")
	}
	// second inline attempt on the same job is refused while it is not READY yet? It is READY (future) → claimable by id.
	_ = h.st.ReleaseLocks(ctx, []string{"training-teacher-e-math"}, "other-worker")
	if err := h.s.RunJobNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if fr.calls != 1 {
		t.Fatalf("runner calls after lock release: %d", fr.calls)
	}
}

func TestDestructiveNeedsReviewWithoutRunning(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	h.addScenario(t, "wipe", "P1", model.StateActive, model.MutationDestructive, []string{"fixture"}, 0)
	run, err := h.s.RunScenarioNow(context.Background(), "wipe", "")
	if err != nil {
		t.Fatal(err)
	}
	if fr.calls != 0 || run.Outcome != model.OutcomeNeedsReview {
		t.Fatalf("destructive must not run: calls=%d run=%+v", fr.calls, run)
	}
	m, _ := h.st.GetScenario(context.Background(), "p", "wipe")
	if m.State != model.StateNeedsReview {
		t.Fatalf("state %s", m.State)
	}
}

func TestPanicIsolation(t *testing.T) {
	fr := &fakeRunner{fn: func(call int, spec runner.Spec) (*runner.Result, error) {
		if spec.Scenario.Scenario.ID == "boom" {
			panic("runner exploded")
		}
		return passResult(spec), nil
	}}
	h := newHarness(t, fr, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "boom", "P1", model.StateActive, model.MutationReadOnly, []string{"shared"}, 0)
	h.addScenario(t, "fine", "P1", model.StateActive, model.MutationReadOnly, []string{"shared"}, 0)

	if _, err := h.s.RunScenarioNow(ctx, "boom", ""); err == nil {
		t.Fatal("panicking job must not yield a run")
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobFailed}, 10)
	if len(jobs) != 1 || jobs[0].ScenarioID != "boom" || jobs[0].LastError == "" {
		t.Fatalf("panicked job must be FAILED with error: %+v", jobs)
	}
	// unrelated work continues, and the panicking job's lock was released (AC-17)
	run, err := h.s.RunScenarioNow(ctx, "fine", "")
	if err != nil || run.Outcome != model.OutcomePass {
		t.Fatalf("unrelated scenario after panic: %v %+v", err, run)
	}
}

func TestScanOnceGateAndOrchestrate(t *testing.T) {
	in := &fakeIngest{events: []model.FeatureEvent{{FeatureID: "PROJ-1001", Status: "shipped", ShippedSHA: "6f22ff7a1dcc309395173a5d52aba5ae01ad769a", ShippedAt: time.Now().Add(-time.Minute), Routes: []string{"/app/training-entry"}}}}
	g := &fakeGate{state: model.ReadinessWaiting, marker: "1000"}
	h := newHarness(t, &fakeRunner{}, g, in)
	ctx := context.Background()

	if err := h.s.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := h.st.GetFeature(ctx, "p", "PROJ-1001")
	if err != nil {
		t.Fatal(err)
	}
	if f.Source != "fake" || f.Readiness != model.ReadinessWaiting || f.LastHandledSHA != "" {
		t.Fatalf("feature after waiting scan: %+v", f)
	}
	if v, _ := h.st.GetState(ctx, "gate:marker:PROJ-1001"); v != f.LatestShippedSHA+" 1000" {
		t.Fatalf("marker not persisted: %q", v)
	}
	if len(h.orch.planned) != 0 {
		t.Fatal("must not plan while waiting (AC-02)")
	}

	g.state, g.marker = model.ReadinessReady, "2000"
	if err := h.s.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ = h.st.GetFeature(ctx, "p", "PROJ-1001")
	if f.Readiness != model.ReadinessReady || f.ReadyAt == nil || f.LastHandledSHA != f.LatestShippedSHA {
		t.Fatalf("feature after ready scan: %+v", f)
	}
	if len(h.orch.planned) != 1 || len(h.orch.enqueued) != 1 {
		t.Fatalf("orchestrator calls: %v %v", h.orch.planned, h.orch.enqueued)
	}
	// handled features are not re-planned
	if err := h.s.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.orch.planned) != 1 {
		t.Fatal("handled feature re-planned")
	}
}

func TestLoopRunsWorkersAndStops(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	h.cfg.Workers.Functional = 2
	ctx := context.Background()
	h.addScenario(t, "a", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "b", "P1", model.StateActive, model.MutationReadOnly, nil, 0)
	// an agent job must stay READY because no agent is available (rule 12)
	_, _, _ = h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "f"}, "agent:f")

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := h.st.CountJobs(ctx, "p", model.JobDone); n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
	if n, _ := h.st.CountJobs(ctx, "p", model.JobDone); n != 2 {
		t.Fatalf("expected both run jobs DONE, got %d", n)
	}
	if jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10); len(jobs) != 1 || jobs[0].Kind != model.JobAgentDiscover {
		t.Fatalf("agent job must stay READY without an agent: %+v", jobs)
	}
	if len(h.orch.agent) != 0 {
		t.Fatal("agent job must not be handled without an agent")
	}
}

func TestAgentWorkerRespectsBudgetThenRuns(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	h.cfg.Budget.AgentTasksPerHour = 1
	h.s.AgentAvailable = true
	ctx := context.Background()
	_ = h.st.RecordBudget(ctx, "p", "agent", 1) // window already spent
	_, _, _ = h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "f"}, "agent:f")

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	time.Sleep(300 * time.Millisecond)
	if len(h.orch.agent) != 0 {
		cancel()
		t.Fatal("agent job must wait while the agent budget is exhausted")
	}
	// Stop the first loop before changing its config. A restarted loop must pick
	// up the READY job after the operator widens the budget.
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.cfg.Budget.AgentTasksPerHour = 5
	loopCtx, cancel = context.WithCancel(ctx)
	done = make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.orch.mu.Lock()
		n := len(h.orch.agent)
		h.orch.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if len(h.orch.agent) != 1 {
		t.Fatalf("agent job handled %d times", len(h.orch.agent))
	}
	if n, _ := h.st.CountJobs(ctx, "p", model.JobDone); n != 1 {
		t.Fatalf("agent job not DONE: %d", n)
	}
	if used, _ := h.st.BudgetUsed(ctx, "p", "agent", time.Hour); used != 2 {
		t.Fatalf("agent budget not recorded: %d", used)
	}
}

func TestPrioritizeAgentJobPreemptsActiveAndBypassesBudget(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	h.cfg.Budget.AgentTasksPerHour = 1
	h.s.AgentAvailable = true
	ctx := context.Background()

	oldID, _, err := h.st.EnqueueJob(ctx, &model.Job{
		ProjectID: "p", Kind: model.JobAgentRepair, Priority: model.PriorityRecentFailure, FeatureID: "old",
	}, "agent:old")
	if err != nil {
		t.Fatal(err)
	}
	oldStarted := make(chan struct{})
	userDone := make(chan struct{})
	var oldOnce, userOnce sync.Once
	h.orch.agentFn = func(runCtx context.Context, job *model.Job) error {
		switch job.FeatureID {
		case "old":
			oldOnce.Do(func() { close(oldStarted) })
			<-runCtx.Done()
			return runCtx.Err()
		case "user":
			userOnce.Do(func() { close(userDone) })
		}
		return nil
	}

	// Let the existing job start before exhausting the hourly budget. The
	// running task is allowed to finish even if the budget changes underneath it.
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	select {
	case <-oldStarted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("old agent job did not start")
	}
	if err := h.st.RecordBudget(ctx, "p", "agent", 1); err != nil {
		cancel()
		t.Fatal(err)
	}

	userID, _, err := h.st.EnqueueJob(ctx, &model.Job{
		ProjectID: "p", Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest, FeatureID: "user",
	}, "agent:user")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := h.s.PrioritizeAgentJob(ctx, userID); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-userDone:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("prioritized user job did not bypass the exhausted budget")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	jobs, err := h.st.ListJobs(ctx, "p", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*model.Job{}
	for _, job := range jobs {
		byID[job.ID] = job
	}
	if got := byID[oldID]; got == nil || got.State != model.JobReady || got.Attempt != 0 || got.LeaseOwner != "" {
		t.Fatalf("displaced job = %+v, want READY with its attempt restored", got)
	}
	if got := byID[userID]; got == nil || got.State != model.JobDone || got.Attempt != 1 {
		t.Fatalf("user job = %+v, want DONE after one attempt", got)
	}
	if used, _ := h.st.BudgetUsed(ctx, "p", "agent", time.Hour); used != 2 {
		t.Fatalf("agent budget = %d, want initial 1 + prioritized user job 1", used)
	}
	var oldAttempts int
	if err := h.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM job_attempts WHERE job_id=?`, oldID).Scan(&oldAttempts); err != nil {
		t.Fatal(err)
	}
	if oldAttempts != 0 {
		t.Fatalf("displaced job retained %d consumed attempt record(s)", oldAttempts)
	}
	if len(h.orch.agent) != 2 || h.orch.agent[0] != oldID || h.orch.agent[1] != userID {
		t.Fatalf("agent execution order = %v, want [%d %d]", h.orch.agent, oldID, userID)
	}
}

func TestPreemptionWithRealOrchestratorDoesNotChargeCancelledAgent(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	h.cfg.Budget.AgentTasksPerHour = 1
	ag := &preemptIntegrationAgent{oldStarted: make(chan struct{}), userDone: make(chan struct{})}
	realOrch := orchestrator.New(h.cfg, h.st, nil, ag, h.s.Evidence())
	s := NewWith(h.cfg, h.st, realOrch, nil, nil, nil)
	s.AgentAvailable = true
	s.Log = log.New(io.Discard, "", 0)
	ctx := context.Background()

	oldID, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 100, FeatureID: "old"}, "old")
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Loop(loopCtx) }()
	select {
	case <-ag.oldStarted:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("old real-orchestrator job did not start")
	}
	if err := h.st.RecordBudget(ctx, "p", "agent", 1); err != nil {
		cancel()
		t.Fatal(err)
	}
	userID, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest, FeatureID: "user"}, "user")
	if err := s.PrioritizeAgentJob(ctx, userID); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-ag.userDone:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("user real-orchestrator job did not run")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if used, _ := h.st.BudgetUsed(ctx, "p", "agent", time.Hour); used != 2 {
		t.Fatalf("budget = %d, want preexisting 1 + user 1; cancelled old job must cost 0", used)
	}
	jobs, _ := h.st.ListJobs(ctx, "p", nil, 10)
	byID := map[int64]*model.Job{}
	for _, job := range jobs {
		byID[job.ID] = job
	}
	if byID[oldID] == nil || byID[oldID].State != model.JobReady || byID[oldID].Attempt != 0 {
		t.Fatalf("cancelled old job = %+v", byID[oldID])
	}
}

func TestPrioritizeAgentJobRejectsDeterministicJob(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	id, _, err := h.st.EnqueueJob(ctx, &model.Job{
		ProjectID: "p", Kind: model.JobRunScenario, Priority: 1000, ScenarioID: "scenario",
	}, "run:scenario")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.PrioritizeAgentJob(ctx, id); err == nil {
		t.Fatal("deterministic job must not be accepted for agent preemption")
	}
	jobs, err := h.st.ListJobs(ctx, "p", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != model.JobReady || jobs[0].Attempt != 0 {
		t.Fatalf("deterministic job changed: %+v", jobs)
	}
}

func TestPrioritizeLeasedUserJobStillCancelsDifferentLocalAgent(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	userID, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest, FeatureID: "user"}, "user")
	if _, err := h.st.ClaimJob(ctx, "p", "other-loop", time.Minute, model.JobAgentDiscover); err != nil {
		t.Fatal(err)
	}
	activeCtx, cancel := context.WithCancelCause(ctx)
	h.s.agentMu.Lock()
	h.s.activeAgent = &activeAgentJob{jobID: userID + 100, cancel: cancel}
	h.s.agentMu.Unlock()

	if err := h.s.PrioritizeAgentJob(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(activeCtx), errAgentPreempted) {
		t.Fatalf("active agent cause = %v", context.Cause(activeCtx))
	}
}

func TestAgentWorkerPreservesRequeuedJobState(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	h.s.AgentAvailable = true
	ctx := context.Background()
	id, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "retry", MaxAttempts: 2}, "retry")
	h.orch.agentFn = func(_ context.Context, job *model.Job) error {
		_, err := h.st.RequeueJob(context.Background(), job.ID, time.Now().Add(time.Minute), "model unavailable")
		if err != nil {
			return err
		}
		return orchestrator.ErrAgentJobRequeued
	}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
		if len(jobs) == 1 && jobs[0].ID == id && strings.Contains(jobs[0].LastError, "model unavailable") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	if len(jobs) != 1 || jobs[0].ID != id {
		t.Fatalf("requeued job was completed: %+v", jobs)
	}
}

func TestLatePreemptionDoesNotRestoreCompletedAgentJob(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	id, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "completed"}, "completed")
	job, err := h.st.ClaimJob(ctx, "p", "agent-1", time.Minute, model.JobAgentDiscover)
	if err != nil {
		t.Fatal(err)
	}
	h.orch.agentFn = func(context.Context, *model.Job) error {
		h.s.agentMu.Lock()
		h.s.activeAgent.cancel(errAgentPreempted)
		h.s.agentMu.Unlock()
		return nil
	}

	h.s.executeAgent(ctx, "agent-1", job)
	jobs, _ := h.st.ListJobs(ctx, "p", nil, 10)
	if len(jobs) != 1 || jobs[0].ID != id || jobs[0].State != model.JobDone || jobs[0].Attempt != 1 {
		t.Fatalf("successfully completed job was restored: %+v", jobs)
	}
}

func TestPersistedUserPriorityBypassesBudgetOnceAfterRestart(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	h.cfg.Budget.AgentTasksPerHour = 1
	h.s.AgentAvailable = true
	ctx := context.Background()
	if err := h.st.RecordBudget(ctx, "p", "agent", 1); err != nil {
		t.Fatal(err)
	}
	id, _, _ := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest, FeatureID: "user"}, "user-restart")
	doneUser := make(chan struct{})
	var once sync.Once
	h.orch.agentFn = func(context.Context, *model.Job) error {
		once.Do(func() { close(doneUser) })
		return nil
	}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	select {
	case <-doneUser:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("persisted user request did not bypass budget after restart")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.st.ListJobs(ctx, "p", nil, 10)
	if len(jobs) != 1 || jobs[0].ID != id || jobs[0].State != model.JobDone || jobs[0].Priority != model.PriorityNewDirectCoverage {
		t.Fatalf("user job did not consume one-shot priority: %+v", jobs)
	}
	if used, _ := h.st.BudgetUsed(ctx, "p", "agent", time.Hour); used != 2 {
		t.Fatalf("budget = %d, want existing 1 + user request 1", used)
	}
}

func TestBrokenScenarioBacksOffInsteadOfHotLooping(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	ctx := context.Background()
	m := &model.Scenario{ID: "broken-yaml", ProjectID: "p", State: model.StateActive, Fingerprint: "x", Class: "P1", Origin: "seed"}
	v := &model.ScenarioVersion{ScenarioID: "broken-yaml", Version: 1, YAML: "scenario: [not, a, mapping", CreatedBy: "seed"}
	if err := h.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RunScenarioNow(ctx, "broken-yaml", ""); err == nil {
		t.Fatal("unparseable scenario must not yield a run")
	}
	if fr.calls != 0 {
		t.Fatal("runner must not be called")
	}
	got, _ := h.st.GetScenario(ctx, "p", "broken-yaml")
	if got.NextDueAt == nil || got.NextDueAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("failure backoff not applied: %v", got.NextDueAt)
	}
	if due, _ := h.st.ListDueScenarios(ctx, "p", time.Now(), 10); len(due) != 0 {
		t.Fatal("broken scenario must not be due again immediately")
	}
}

// Some origins answer GET immediately but never complete a HEAD. The probe used HEAD, so it
// timed out and reported an unhealthy target for a site that was serving fine, and that
// verdict is what decides whether a failing script is drift worth repairing.
func TestProbeTargetSurvivesAnOriginThatIgnoresHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			<-r.Context().Done() // never answers
			return
		}
		w.WriteHeader(http.StatusNotFound) // the root having no page is still a healthy origin
	}))
	defer srv.Close()

	s := &Scheduler{cfg: &config.Config{}}
	s.cfg.Target.BaseURL = srv.URL
	if !s.probeTarget(context.Background()) {
		t.Fatal("probeTarget = false for an origin that answers GET; a hanging HEAD must not mark the target unhealthy")
	}
}

func TestProbeTargetReportsAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s := &Scheduler{cfg: &config.Config{}}
	s.cfg.Target.BaseURL = srv.URL
	if s.probeTarget(context.Background()) {
		t.Fatal("probeTarget = true for a 502; 5xx must still count as unhealthy")
	}
}

func TestIssueEventIsReadyAtOnceAndReproduceJobReachesTheAgentWorker(t *testing.T) {
	in := &fakeIngest{events: []model.FeatureEvent{{FeatureID: "PROJ-130", Status: "shipped", ShippedSHA: "jira:PROJ-130:abcd1234", ShippedAt: time.Now(),
		Kind: model.FeatureKindIssue, Ref: "PROJ-130", Summary: "cart total wrong", Details: "steps", Source: "jira"}}}
	cfg := testCfg(t)
	cfg.Deployment.Readiness.Strategy = "delay"
	cfg.Deployment.Readiness.DelayAfterShip.Duration = 2 * time.Hour
	h := newHarness(t, &fakeRunner{}, gate.New(cfg), in)
	ctx := context.Background()
	if err := h.s.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := h.st.GetFeature(ctx, "p", "PROJ-130")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != model.FeatureKindIssue || f.Ref != "PROJ-130" || f.Details != "steps" || f.Readiness != model.ReadinessReady || f.ReadyAt == nil {
		t.Fatalf("issue feature after scan: %+v (must be READY without waiting for a deployment)", f)
	}
	if len(h.orch.planned) != 1 || h.orch.planned[0] != "PROJ-130" {
		t.Fatalf("planned = %v", h.orch.planned)
	}

	// The agent worker claims AGENT_REPRODUCE like the other agent kinds.
	h.s.AgentAvailable = true
	h.cfg.Budget.AgentTasksPerHour = 5
	if _, _, err := h.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentReproduce, Priority: model.PriorityNewDirectCoverage, FeatureID: "PROJ-130"}, "agent:PROJ-130"); err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.s.Loop(loopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.orch.mu.Lock()
		n := len(h.orch.agent)
		h.orch.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if len(h.orch.agent) != 1 {
		t.Fatalf("reproduce job handled %d times", len(h.orch.agent))
	}
	if n, _ := h.st.CountJobs(ctx, "p", model.JobDone); n != 1 {
		t.Fatalf("reproduce job not DONE: %d", n)
	}
}

func TestScanDefersIssueFeatureUntilAnAgentIsAvailable(t *testing.T) {
	ev := model.FeatureEvent{FeatureID: "PROJ-131", Status: "shipped", ShippedSHA: "jira:PROJ-131:abcd1234", ShippedAt: time.Now(),
		Kind: model.FeatureKindIssue, Ref: "PROJ-131", Summary: "cart total wrong", Details: "steps", Source: "jira"}
	h := newHarness(t, &fakeRunner{}, nil, &fakeIngest{events: []model.FeatureEvent{ev}})
	h.cfg.Budget.AgentTasksPerHour = 5
	ctx := context.Background()

	// `vigil scan` builds the orchestrator without an agent: the decision is circumstantial and must not be marked handled.
	noAgent := NewWith(h.cfg, h.st, orchestrator.New(h.cfg, h.st, nil, nil, h.s.Evidence()), nil, gate.New(h.cfg), &fakeIngest{events: []model.FeatureEvent{ev}})
	noAgent.Log = log.New(io.Discard, "", 0)
	if err := noAgent.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := h.st.GetFeature(ctx, "p", "PROJ-131")
	if err != nil {
		t.Fatal(err)
	}
	if f.Readiness != model.ReadinessReady || f.LastHandledSHA != "" {
		t.Fatalf("feature after agent-less scan: %+v (must stay unhandled)", f)
	}
	if n := len(jobsOf(t, h.st, model.JobAgentReproduce)); n != 0 {
		t.Fatalf("reproduce jobs without an agent: %d", n)
	}

	// The loop has an agent: the same feature is planned again, gets its job and is handled.
	withAgent := NewWith(h.cfg, h.st, orchestrator.New(h.cfg, h.st, nil, &preemptIntegrationAgent{}, h.s.Evidence()), nil, gate.New(h.cfg), nil)
	withAgent.Log = log.New(io.Discard, "", 0)
	if err := withAgent.ScanOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f, _ = h.st.GetFeature(ctx, "p", "PROJ-131")
	if f.LastHandledSHA != f.LatestShippedSHA {
		t.Fatalf("feature after scan with agent: %+v (must be handled)", f)
	}
	if jobs := jobsOf(t, h.st, model.JobAgentReproduce); len(jobs) != 1 || jobs[0].FeatureID != "PROJ-131" {
		t.Fatalf("reproduce jobs = %+v", jobs)
	}
}

func jobsOf(t *testing.T, st *store.Store, kind model.JobKind) []*model.Job {
	t.Helper()
	jobs, err := st.ListJobs(context.Background(), "p", []model.JobState{model.JobReady, model.JobLeased}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []*model.Job
	for _, j := range jobs {
		if j.Kind == kind {
			out = append(out, j)
		}
	}
	return out
}
