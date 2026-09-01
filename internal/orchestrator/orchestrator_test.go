package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// ---- fakes ---------------------------------------------------------------

type fakeRunner struct {
	passed bool
	err    error
	specs  []runner.Spec
}

func (f *fakeRunner) Run(_ context.Context, spec runner.Spec) (*runner.Result, error) {
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return nil, f.err
	}
	now := time.Now()
	res := &runner.Result{Passed: f.passed, Browser: spec.Browser, StartedAt: now, FinishedAt: now.Add(time.Second)}
	if !f.passed {
		res.Class = runner.FailAssertion
		res.FailedStep = &runner.StepResult{Index: 2, Kind: "assert_text", Expected: "1", Actual: "9", Error: "text not found"}
	}
	return res, nil
}

type fakeAgent struct {
	res  *agent.Result
	err  error
	reqs []agent.Request
}

func (f *fakeAgent) Run(_ context.Context, req agent.Request, dir string) (*agent.Result, error) {
	f.reqs = append(f.reqs, req)
	_ = os.MkdirAll(dir, 0o755)
	return f.res, f.err
}
func (f *fakeAgent) Doctor(context.Context) error          { return nil }
func (f *fakeAgent) Reparse(string) (*agent.Result, error) { return nil, nil }

// ---- fixtures --------------------------------------------------------------

const seedScenario = `scenario:
  id: visible-remedy-order
  version: 1
  title: Remedy order
covers:
  feature: remedy-page-order
  capability: remedy.navigation
  routes: [/remedy]
steps:
  - goto: /remedy/current
  - assert_text: { value: "1" }
  - click: { by: role, role: button, name: Next }
  - assert_text: { value: "2" }
assert:
  no_http_5xx: true
oracle:
  source: spec
  source_feature: remedy-page-order
  source_sha: abc123
`

// same logical scenario, different locator mechanics + an extra wait (fingerprint-equal)
const dupCandidate = `scenario:
  id: remedy-order-agent
  version: 1
covers:
  feature: remedy-page-order
  capability: remedy.navigation
  routes: [/remedy]
steps:
  - goto: /remedy/current
  - wait_for: { by: css, value: ".remedy" }
  - assert_text: { value: "1" }
  - click: { by: test_id, value: next }
  - assert_text: { value: "2" }
assert:
  no_http_5xx: true
oracle:
  source: spec
  source_feature: remedy-page-order
  source_sha: abc123
`

// 4 of 5 major actions shared with the seed → Jaccard 0.8 (structural duplicate)
const structuralDupCandidate = `scenario:
  id: remedy-order-plus
  version: 1
covers:
  feature: remedy-page-order
  capability: remedy.navigation
  routes: [/remedy]
steps:
  - goto: /remedy/current
  - assert_text: { value: "1" }
  - click: { by: role, role: button, name: Next }
  - assert_text: { value: "2" }
  - assert_visible: { by: text, text: "완료" }
assert:
  no_http_5xx: true
oracle:
  source: spec
  source_feature: remedy-page-order
  source_sha: abc123
`

const freshCandidate = `scenario:
  id: training-entry-tabs
  version: 1
  title: Training entry tabs
covers:
  feature: training-entry-page
  capability: entry.training
  routes: [/lms-web/training-entry]
steps:
  - goto: /lms-web/training-entry
  - click: { by: role, role: tab, name: "중학" }
  - assert_visible: { by: role, role: tab, name: "정보" }
assert:
  no_uncaught_console_error: true
oracle:
  source: spec
  source_feature: training-entry-page
  source_sha: def456
`

func newTest(t *testing.T) (*Orchestrator, *store.Store, *fakeRunner, *fakeAgent, *config.Config) {
	t.Helper()
	base := t.TempDir()
	cfg := &config.Config{}
	cfg.BaseDir = base
	cfg.Project.ID = "p"
	cfg.Target.BaseURL = "https://t.example.com"
	cfg.Target.AllowedHosts = []string{"t.example.com"}
	cfg.Browser.Primary = "lightpanda"
	cfg.Browser.StepTimeout.Duration = 5 * time.Second
	cfg.Browser.RunTimeout.Duration = time.Minute
	cfg.Evidence.Dir = "evidence"
	cfg.Paths.Scenarios = "scenarios"
	cfg.Paths.Flows = "flows"
	cfg.Budget.AgentTasksPerHour = 10
	cfg.Policy.SoakPasses = 2
	cfg.Policy.QuarantineAfter = 2
	cfg.Policy.ObservationOracle = "needs_review"
	cfg.Policy.StructuralDupThreshold = 0.8
	cfg.Schedule.Soak.Duration = 10 * time.Minute
	cfg.Schedule.P0.Duration = 15 * time.Minute
	cfg.Schedule.P1.Duration = time.Hour
	cfg.Schedule.P2.Duration = 6 * time.Hour
	cfg.Schedule.FailureBackoff.Duration = 5 * time.Minute
	cfg.Agent.Backoff.Duration = 45 * time.Second
	cfg.Agent.MaxScenariosPerTask = 3
	st, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertProject(context.Background(), "p", cfg.Target.BaseURL); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{passed: true}
	fa := &fakeAgent{}
	o := New(cfg, st, nil, nil, nil)
	o.run = fr
	o.agent = fa
	return o, st, fr, fa, cfg
}

func seed(t *testing.T, o *Orchestrator, cfg *config.Config, yaml string) {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir, cfg.Paths.Scenarios)
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "seed.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := o.ImportScenarioFiles(context.Background()); err != nil || n != 1 {
		t.Fatalf("import: n=%d err=%v", n, err)
	}
}

func feature(t *testing.T, st *store.Store, id, sha string, routes, paths []string) *model.Feature {
	t.Helper()
	ctx := context.Background()
	if _, err := st.UpsertFeature(ctx, "p", model.FeatureEvent{FeatureID: id, Status: "shipped", ShippedSHA: sha, ShippedAt: time.Now(), Routes: routes, ChangedPaths: paths, Summary: id + " summary"}); err != nil {
		t.Fatal(err)
	}
	f, err := st.GetFeature(ctx, "p", id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func jobsOfKind(t *testing.T, st *store.Store, kind model.JobKind) []*model.Job {
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

func runFor(t *testing.T, st *store.Store, job *model.Job, scenarioID string, outcome model.Outcome) *model.Run {
	t.Helper()
	now := time.Now().UTC()
	r := &model.Run{JobID: job.ID, ProjectID: "p", ScenarioID: scenarioID, ScenarioVersion: 1, FeatureID: job.FeatureID, ShippedSHA: parsePayload(job.Payload).ShippedSHA,
		Browser: model.BrowserLightpanda, Outcome: outcome, Attempt: 1, StartedAt: now.Add(-time.Second), FinishedAt: now, DurationMs: 1000}
	if outcome == model.OutcomeAppFailure {
		r.FailedStep, r.FailedAction, r.Expected, r.Actual = 2, "assert_text", "1", "9"
	}
	if _, err := st.InsertRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func plainJob(scenarioID string) *model.Job {
	return &model.Job{ID: 999, ProjectID: "p", Kind: model.JobRunScenario, ScenarioID: scenarioID, Payload: "{}"}
}

// ---- tests -----------------------------------------------------------------

func TestNoCoverageEnqueuesDiscover(t *testing.T) {
	o, st, _, _, _ := newTest(t)
	ctx := context.Background()
	f := feature(t, st, "training-entry-page", "sha1", []string{"/lms-web/training-entry"}, nil)
	d, err := o.PlanFeature(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != model.ActionBrowserAgentDiscover {
		t.Fatalf("action = %s (%s)", d.Action, d.Reason)
	}
	if err := o.EnqueueForFeature(ctx, f, d); err != nil {
		t.Fatal(err)
	}
	jobs := jobsOfKind(t, st, model.JobAgentDiscover)
	if len(jobs) != 1 || jobs[0].FeatureID != f.ID || jobs[0].Priority != model.PriorityNewDirectCoverage {
		t.Fatalf("discover jobs = %+v", jobs)
	}
	// dedup on re-plan
	_ = o.EnqueueForFeature(ctx, f, d)
	if n := len(jobsOfKind(t, st, model.JobAgentDiscover)); n != 1 {
		t.Fatalf("dedup failed: %d discover jobs", n)
	}
	if un, _ := st.ListUnhandledFeatures(ctx, "p"); len(un) != 0 {
		t.Fatalf("feature not marked handled: %d", len(un))
	}
	// no agent → NO_ACTION (rule 12)
	o.agent = nil
	if d, _ := o.PlanFeature(ctx, f); d.Action != model.ActionNone {
		t.Fatalf("without agent: %s", d.Action)
	}
	// budget exhausted → NO_ACTION
	o.agent = &fakeAgent{}
	for i := 0; i < 10; i++ {
		_ = st.RecordBudget(ctx, "p", "agent", 1)
	}
	if d, _ := o.PlanFeature(ctx, f); d.Action != model.ActionNone || !strings.Contains(d.Reason, "budget") {
		t.Fatalf("budget: %s (%s)", d.Action, d.Reason)
	}
}

func TestCoverageRunsImpactedFirstThenVerifyOnFailure(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	f := feature(t, st, "remedy-page-order", "sha2", []string{"/remedy"}, []string{"src/pages/remedy/OctoPlayer.vue"})
	d, err := o.PlanFeature(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != model.ActionRunImpactedScriptsFirst || len(d.ScenarioIDs) != 1 || d.ScenarioIDs[0] != "visible-remedy-order" {
		t.Fatalf("decision = %+v", d)
	}
	if err := o.EnqueueForFeature(ctx, f, d); err != nil {
		t.Fatal(err)
	}
	jobs := jobsOfKind(t, st, model.JobRunScenario)
	if len(jobs) != 1 || jobs[0].Priority != model.PriorityImpacted || !parsePayload(jobs[0].Payload).Impacted {
		t.Fatalf("impacted jobs = %+v", jobs)
	}
	if n := len(jobsOfKind(t, st, model.JobAgentDiscover)); n != 0 {
		t.Fatalf("discovery must not run while coverage exists (AC-07): %d", n)
	}
	// impacted run fails → AGENT_VERIFY
	run := runFor(t, st, jobs[0], "visible-remedy-order", model.OutcomeAppFailure)
	if err := o.AfterRun(ctx, jobs[0], run, nil); err != nil {
		t.Fatal(err)
	}
	var is impactState
	if ok, _ := o.getJSONState(ctx, stateImpactPrefix+"remedy-page-order:sha2", &is); !ok || !is.Done {
		t.Fatalf("impact state = %+v", is)
	}
	if n := len(jobsOfKind(t, st, model.JobAgentVerify)); n != 1 {
		t.Fatalf("verify jobs = %d (%s)", n, is.Result)
	}
	// and a passing impacted set keeps coverage silently
	f2 := feature(t, st, "remedy-page-order", "sha3", []string{"/remedy"}, nil)
	d2, _ := o.PlanFeature(ctx, f2)
	_ = o.EnqueueForFeature(ctx, f2, d2)
	var j2 *model.Job
	for _, j := range jobsOfKind(t, st, model.JobRunScenario) {
		if parsePayload(j.Payload).ShippedSHA == "sha3" {
			j2 = j
		}
	}
	if j2 == nil {
		t.Fatal("no impacted job for sha3")
	}
	_ = o.AfterRun(ctx, j2, runFor(t, st, j2, "visible-remedy-order", model.OutcomePass), nil)
	_, _ = o.getJSONState(ctx, stateImpactPrefix+"remedy-page-order:sha3", &is)
	if !is.Done || !strings.Contains(is.Result, "coverage kept") {
		t.Fatalf("pass impact = %+v", is)
	}
	if n := len(jobsOfKind(t, st, model.JobAgentVerify)); n != 1 {
		t.Fatalf("verify jobs after pass = %d", n)
	}
}

func TestSoakPromotion(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	sc, _ := st.GetScenario(ctx, "p", "visible-remedy-order")
	if sc.State != model.StateSoak || sc.SoakTarget != 2 || sc.Origin != "seed" {
		t.Fatalf("seed state = %+v", sc)
	}
	job := plainJob(sc.ID)
	for i := 0; i < 2; i++ {
		if err := o.AfterRun(ctx, job, runFor(t, st, job, sc.ID, model.OutcomePass), nil); err != nil {
			t.Fatal(err)
		}
	}
	sc, _ = st.GetScenario(ctx, "p", sc.ID)
	if sc.State != model.StateActive || sc.SoakPasses != 2 {
		t.Fatalf("after 2 passes: state=%s soak=%d", sc.State, sc.SoakPasses)
	}
	if sc.NextDueAt == nil || time.Until(*sc.NextDueAt) < 50*time.Minute {
		t.Fatalf("P1 cadence not applied: %v", sc.NextDueAt)
	}
}

func TestDriftEnqueuesRepair(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	job := plainJob("visible-remedy-order")
	for i := 0; i < 2; i++ {
		if err := o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, model.OutcomeScriptDrift), nil); err != nil {
			t.Fatal(err)
		}
	}
	jobs := jobsOfKind(t, st, model.JobAgentRepair)
	if len(jobs) != 1 || jobs[0].ScenarioID != "visible-remedy-order" {
		t.Fatalf("repair jobs = %+v", jobs)
	}
	sc, _ := st.GetScenario(ctx, "p", job.ScenarioID)
	if sc.NextDueAt == nil || time.Until(*sc.NextDueAt) > 6*time.Minute {
		t.Fatalf("failure backoff not applied: %v", sc.NextDueAt)
	}
}

func TestLightpandaIncompatibleChromiumConfirm(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	job := plainJob("visible-remedy-order")
	run := runFor(t, st, job, job.ScenarioID, model.OutcomeLightpandaIncompatible)
	if err := o.AfterRun(ctx, job, run, nil); err != nil {
		t.Fatal(err)
	}
	jobs := jobsOfKind(t, st, model.JobChromiumConfirm)
	if len(jobs) != 1 || jobs[0].Browser != model.BrowserChromium || parsePayload(jobs[0].Payload).ConfirmRunID != run.ID {
		t.Fatalf("confirm jobs = %+v", jobs)
	}
	// chromium confirms PASS → new version with browser.primary chromium (mechanics, created_by system)
	crun := runFor(t, st, jobs[0], job.ScenarioID, model.OutcomePass)
	crun.Browser = model.BrowserChromium
	if err := o.AfterRun(ctx, jobs[0], crun, &runner.Result{Passed: true}); err != nil {
		t.Fatal(err)
	}
	sc, v, err := st.GetCurrentScenarioVersion(ctx, "p", job.ScenarioID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 2 || v.CreatedBy != "system" || !strings.Contains(v.YAML, "primary: chromium") || sc.State != model.StateSoak {
		t.Fatalf("version = %+v state=%s", v, sc.State)
	}
	// chromium fails the assertion → APP_FAILURE handling
	frun := runFor(t, st, jobs[0], job.ScenarioID, model.OutcomeAppFailure)
	if err := o.AfterRun(ctx, jobs[0], frun, &runner.Result{Class: runner.FailAssertion}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenIncidentFor(ctx, "p", model.IncidentAppRegression, job.ScenarioID); err != nil {
		t.Fatalf("no incident after chromium-confirmed failure: %v", err)
	}
}

func TestAppFailureIncidentDedupeAndResolve(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	job := plainJob("visible-remedy-order")
	for i := 0; i < 2; i++ {
		if err := o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, model.OutcomeAppFailure), nil); err != nil {
			t.Fatal(err)
		}
	}
	open, _ := st.ListIncidents(ctx, "p", true, 10)
	if len(open) != 1 || open[0].Kind != model.IncidentAppRegression || open[0].ScenarioID != job.ScenarioID {
		t.Fatalf("open incidents = %+v", open)
	}
	if m, _ := st.GetMetrics(ctx, job.ScenarioID); m.RegressionsCaught != 2 {
		t.Fatalf("regressions_caught = %d", m.RegressionsCaught)
	}
	if jobs := jobsOfKind(t, st, model.JobRunScenario); len(jobs) != 1 || jobs[0].Priority != model.PriorityRecentFailure {
		t.Fatalf("recent-failure retry jobs = %+v", jobs)
	}
	if err := o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, model.OutcomePass), nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.ListIncidents(ctx, "p", true, 10); len(open) != 0 {
		t.Fatalf("incident not resolved after PASS: %+v", open)
	}
	// infra failures → one ENVIRONMENT incident, scenario_id ""
	for _, oc := range []model.Outcome{model.OutcomeAuthFailure, model.OutcomeEnvFailure} {
		if err := o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, oc), nil); err != nil {
			t.Fatal(err)
		}
	}
	open, _ = st.ListIncidents(ctx, "p", true, 10)
	if len(open) != 1 || open[0].Kind != model.IncidentEnvironment || open[0].ScenarioID != "" {
		t.Fatalf("env incidents = %+v", open)
	}
	if sc, _ := st.GetScenario(ctx, "p", job.ScenarioID); sc.ConsecutiveFailures != 0 {
		t.Fatalf("infra must not bump consecutive_failures: %d", sc.ConsecutiveFailures)
	}
}

func TestFlakeQuarantine(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	job := plainJob("visible-remedy-order")
	_ = o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, model.OutcomeQAFlake), nil)
	if sc, _ := st.GetScenario(ctx, "p", job.ScenarioID); sc.State != model.StateSoak {
		t.Fatalf("one flake must not quarantine: %s", sc.State)
	}
	_ = o.AfterRun(ctx, job, runFor(t, st, job, job.ScenarioID, model.OutcomeQAFlake), nil)
	if sc, _ := st.GetScenario(ctx, "p", job.ScenarioID); sc.State != model.StateQuarantined {
		t.Fatalf("expected QUARANTINED, got %s", sc.State)
	}
}

func agentJob(t *testing.T, st *store.Store, kind model.JobKind, featureID, sha, scenarioID string) *model.Job {
	t.Helper()
	ctx := context.Background()
	p := jobPayload{FeatureID: featureID, ShippedSHA: sha, ScenarioID: scenarioID, Version: 1}
	id, _, err := st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: kind, Priority: 50, FeatureID: featureID, ScenarioID: scenarioID, Payload: p.String()}, "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.ClaimJob(ctx, "p", "w1", time.Minute, kind)
	if err != nil || j.ID != id {
		t.Fatalf("claim: %v", err)
	}
	return j
}

func TestDuplicateCandidatesRejected(t *testing.T) {
	o, st, fr, fa, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	feature(t, st, "remedy-page-order", "sha9", []string{"/remedy"}, nil)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, CoverageDelta: "same thing", ScriptCandidates: []string{dupCandidate, structuralDupCandidate}}
	job := agentJob(t, st, model.JobAgentVerify, "remedy-page-order", "sha9", "")
	if err := o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListScenarios(ctx, "p")
	if len(all) != 1 {
		t.Fatalf("duplicates were persisted: %d scenarios", len(all))
	}
	if len(fr.specs) != 0 {
		t.Fatalf("duplicates must not be validated by the runner")
	}
	var g GateOutcome
	if ok, _ := o.getJSONState(ctx, stateGatePrefix+"remedy-order-plus", &g); !ok || g.State != model.StateDuplicate || g.DuplicateOf != "visible-remedy-order" {
		t.Fatalf("structural gate = %+v", g)
	}
	if ok, _ := o.getJSONState(ctx, stateGatePrefix+"remedy-order-agent", &g); !ok || g.State != model.StateDuplicate {
		t.Fatalf("fingerprint gate = %+v", g)
	}
	if used, _ := st.BudgetUsed(ctx, "p", "agent", time.Hour); used != 1 {
		t.Fatalf("budget recorded = %d", used)
	}
	if len(fa.reqs) != 1 || fa.reqs[0].Task != agent.TaskVerifyChange || len(fa.reqs[0].KnownScripts) != 1 || fa.reqs[0].KnownScripts[0] != "visible-remedy-order/v1" {
		t.Fatalf("request = %+v", fa.reqs)
	}
}

func TestCandidateValidatedToSoakOrReview(t *testing.T) {
	o, st, fr, fa, _ := newTest(t)
	ctx := context.Background()
	feature(t, st, "training-entry-page", "def456", []string{"/lms-web/training-entry"}, nil)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, CoverageDelta: "entry tabs", ScriptCandidates: []string{freshCandidate}}
	job := agentJob(t, st, model.JobAgentDiscover, "training-entry-page", "def456", "")
	if err := o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	sc, err := st.GetScenario(ctx, "p", "training-entry-tabs")
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != model.StateSoak || sc.Origin != "agent" || sc.OracleSource != "spec" || sc.SoakTarget != 2 || sc.SoakPasses != 0 {
		t.Fatalf("scenario = %+v", sc)
	}
	if len(fr.specs) != 1 || fr.specs[0].Browser != model.BrowserLightpanda || fr.specs[0].BaseURL != "https://t.example.com" {
		t.Fatalf("validation spec = %+v", fr.specs)
	}
	if runs, _ := st.ListRuns(ctx, "p", sc.ID, 5); len(runs) != 1 || runs[0].Outcome != model.OutcomePass {
		t.Fatalf("validation run not recorded: %+v", runs)
	}
	links, _ := st.ListCoverageLinks(ctx, "p", sc.ID)
	if len(links) != 3 {
		t.Fatalf("links = %+v", links)
	}

	// failing validation → NEEDS_REVIEW; observation oracle → NEEDS_REVIEW without a run
	fr.passed = false
	obs := strings.Replace(strings.Replace(freshCandidate, "id: training-entry-tabs", "id: entry-obs", 1), "source: spec", "source: observation", 1)
	obs = strings.Replace(obs, "capability: entry.training", "capability: entry.obs", 1)
	failing := strings.Replace(freshCandidate, "id: training-entry-tabs", "id: entry-fail", 1)
	failing = strings.Replace(failing, "capability: entry.training", "capability: entry.other", 1)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{obs, failing}}
	job2 := agentJob(t, st, model.JobAgentDiscover, "training-entry-page", "def457", "")
	if err := o.HandleAgentJob(ctx, job2); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetScenario(ctx, "p", "entry-obs"); sc == nil || sc.State != model.StateNeedsReview {
		t.Fatalf("observation oracle: %+v", sc)
	}
	if sc, _ := st.GetScenario(ctx, "p", "entry-fail"); sc == nil || sc.State != model.StateNeedsReview {
		t.Fatalf("failed validation: %+v", sc)
	}
	if len(fr.specs) != 2 {
		t.Fatalf("runner calls = %d (observation candidates must not run)", len(fr.specs))
	}
	// ephemeral
	fr.passed = true
	eph := strings.Replace(freshCandidate, "id: training-entry-tabs", "id: entry-eph", 1)
	eph = strings.Replace(eph, "capability: entry.training", "capability: entry.temp", 1)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, Ephemeral: true, ScriptCandidates: []string{eph}}
	_ = o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentDiscover, "training-entry-page", "def458", ""))
	if sc, _ := st.GetScenario(ctx, "p", "entry-eph"); sc == nil || sc.State != model.StateEphemeral || sc.NextDueAt != nil {
		t.Fatalf("ephemeral: %+v", sc)
	}
}

func TestRepairGate(t *testing.T) {
	o, st, fr, fa, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	// 1) repair that changes an assertion → NEEDS_REVIEW, no new version (AC-09)
	bad := strings.Replace(seedScenario, `assert_text: { value: "2" }`, `assert_text: { value: "3" }`, 1)
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: bad}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentRepair, "remedy-page-order", "abc123", "visible-remedy-order")); err != nil {
		t.Fatal(err)
	}
	sc, v, _ := st.GetCurrentScenarioVersion(ctx, "p", "visible-remedy-order")
	if sc.State != model.StateNeedsReview || v.Version != 1 || len(fr.specs) != 0 {
		t.Fatalf("oracle change accepted: state=%s v=%d runs=%d", sc.State, v.Version, len(fr.specs))
	}
	// 2) locator-only repair → validated, new version by "repair", state unchanged
	_ = st.SetScenarioState(ctx, "p", sc.ID, model.StateActive)
	good := strings.Replace(seedScenario, `click: { by: role, role: button, name: Next }`, `click: { by: test_id, value: next-btn }`, 1)
	good = strings.Replace(good, "  - goto: /remedy/current\n", "  - goto: /remedy/current\n  - wait_for: { by: test_id, value: next-btn }\n", 1)
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: good, CoverageDelta: "test id locator"}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentRepair, "remedy-page-order", "abc123", "visible-remedy-order")); err != nil {
		t.Fatal(err)
	}
	sc, v, _ = st.GetCurrentScenarioVersion(ctx, "p", "visible-remedy-order")
	if sc.State != model.StateActive || v.Version != 2 || v.CreatedBy != "repair" || !strings.Contains(v.YAML, "next-btn") || len(fr.specs) != 1 {
		t.Fatalf("repair not applied: state=%s v=%+v runs=%d", sc.State, v, len(fr.specs))
	}
	if fa.reqs[1].FailingScript == "" || fa.reqs[1].Task != agent.TaskRepair {
		t.Fatalf("repair request = %+v", fa.reqs[1])
	}
}

func TestModelUnavailableRequeues(t *testing.T) {
	o, st, _, fa, _ := newTest(t)
	ctx := context.Background()
	feature(t, st, "training-entry-page", "def456", nil, nil)
	fa.res = &agent.Result{ModelUnavailable: true, Reason: "429 rate limit"}
	job := agentJob(t, st, model.JobAgentDiscover, "training-entry-page", "def456", "")
	if err := o.HandleAgentJob(ctx, job); err != nil {
		t.Fatalf("model unavailable must not error: %v", err)
	}
	jobs, _ := st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	if len(jobs) != 1 || jobs[0].ID != job.ID || !strings.Contains(jobs[0].LastError, "model unavailable") || time.Until(jobs[0].ScheduledAt) < 30*time.Second {
		t.Fatalf("job not requeued with backoff: %+v", jobs)
	}
	if n, _ := st.CountJobs(ctx, "p", model.JobDone); n != 0 {
		t.Fatalf("job wrongly completed")
	}
}

func TestAgentAppFailureNeedsLinkedEvidence(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	ctx := context.Background()
	feature(t, st, "remedy-page-order", "abc123", []string{"/remedy"}, nil)
	fa.res = &agent.Result{Decision: agent.DecisionAppFailure, Evidence: "button missing"}
	// no linked scenario → review marker, no incident
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentVerify, "remedy-page-order", "abc123", "")); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.ListIncidents(ctx, "p", true, 10); len(open) != 0 {
		t.Fatalf("incident without evidence: %+v", open)
	}
	if v, _ := st.GetState(ctx, stateReviewPrefix+"remedy-page-order:abc123"); !strings.Contains(v, "APP_FAILURE") {
		t.Fatalf("review marker = %q", v)
	}
	// linked scenario with a failing run → incident
	seed(t, o, cfg, seedScenario)
	job := plainJob("visible-remedy-order")
	job.FeatureID = "remedy-page-order"
	runFor(t, st, job, job.ScenarioID, model.OutcomeAppFailure)
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentVerify, "remedy-page-order", "abc124", "")); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.ListIncidents(ctx, "p", true, 10); len(open) != 1 || open[0].ScenarioID != "visible-remedy-order" {
		t.Fatalf("incidents = %+v", open)
	}
}

func TestImportUpdatesVersionKeepsState(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, seedScenario)
	_ = st.SetScenarioState(ctx, "p", "visible-remedy-order", model.StateActive)
	if n, err := o.ImportScenarioFiles(ctx); err != nil || n != 0 {
		t.Fatalf("unchanged import: n=%d err=%v", n, err)
	}
	changed := strings.Replace(seedScenario, "version: 1", "version: 2", 1)
	changed = strings.Replace(changed, `assert_text: { value: "2" }`, `assert_text: { value: "two" }`, 1)
	_ = os.WriteFile(filepath.Join(cfg.BaseDir, "scenarios", "seed.yaml"), []byte(changed), 0o644)
	if n, err := o.ImportScenarioFiles(ctx); err != nil || n != 1 {
		t.Fatalf("changed import: n=%d err=%v", n, err)
	}
	sc, v, _ := st.GetCurrentScenarioVersion(ctx, "p", "visible-remedy-order")
	if sc.State != model.StateActive || v.Version != 2 || v.CreatedBy != "seed" {
		t.Fatalf("after change: state=%s v=%+v", sc.State, v)
	}
	// flows are imported and usable
	_ = os.MkdirAll(filepath.Join(cfg.BaseDir, "flows"), 0o755)
	_ = os.WriteFile(filepath.Join(cfg.BaseDir, "flows", "login.yaml"), []byte("flow:\n  id: login-as-student\n  version: 1\nsteps:\n  - goto: /login\n  - fill: { by: label, name: ID, input: x }\n"), 0o644)
	withFlow := strings.Replace(changed, "steps:\n", "uses: [login-as-student]\nsteps:\n", 1)
	withFlow = strings.Replace(withFlow, "version: 2", "version: 3", 1)
	_ = os.WriteFile(filepath.Join(cfg.BaseDir, "scenarios", "seed.yaml"), []byte(withFlow), 0o644)
	if n, err := o.ImportScenarioFiles(ctx); err != nil || n != 1 {
		t.Fatalf("flow import: n=%d err=%v", n, err)
	}
	if flows, _ := st.ListFlowYAML(ctx, "p"); len(flows) != 1 {
		t.Fatalf("flows = %v", flows)
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	p := jobPayload{FeatureID: "f", ShippedSHA: "s", Impacted: true, RunID: 7}
	var back map[string]any
	if err := json.Unmarshal([]byte(p.String()), &back); err != nil {
		t.Fatal(err)
	}
	if back["feature_id"] != "f" || back["impacted"] != true || back["run_id"] != float64(7) {
		t.Fatalf("payload = %v", back)
	}
}

// Re-importing an UNCHANGED seed file must not overwrite a system-generated
// version (e.g. CHROMIUM_CONFIRM → browser.primary: chromium).
func TestReimportUnchangedFileKeepsSystemVersion(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	seed(t, o, cfg, strings.Replace(seedScenario, "visible-remedy-order", "re-import-a", 1))
	sc, err := st.GetScenario(ctx, cfg.Project.ID, "re-import-a")
	if err != nil {
		t.Fatal(err)
	}
	// system adds v2 (mechanics-only change)
	v2 := &model.ScenarioVersion{ScenarioID: sc.ID, Version: sc.CurrentVersion + 1, YAML: "scenario:\n  id: re-import-a\n  version: 2\n# system: chromium\n", Fingerprint: sc.Fingerprint, CreatedBy: "system", Reason: "lightpanda incompatible"}
	if err := st.AddScenarioVersion(ctx, cfg.Project.ID, v2, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := o.ImportScenarioFiles(ctx); err != nil || n != 0 {
		t.Fatalf("unchanged file re-import must be a no-op, got n=%d err=%v", n, err)
	}
	after, _ := st.GetScenario(ctx, cfg.Project.ID, "re-import-a")
	if after.CurrentVersion != v2.Version {
		t.Fatalf("system version overwritten: current=%d want %d", after.CurrentVersion, v2.Version)
	}
}
