package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/evidence"
	"vigil/internal/model"
	"vigil/internal/store"
)

type fakeReproduceAgent struct {
	res  *agent.Result
	reqs []agent.Request
}

func (f *fakeReproduceAgent) Run(_ context.Context, req agent.Request, dir string) (*agent.Result, error) {
	f.reqs = append(f.reqs, req)
	_ = os.MkdirAll(dir, 0o755)
	return f.res, nil
}
func (f *fakeReproduceAgent) Plan(context.Context, string, string) (*agent.Plan, error) {
	return nil, nil
}
func (f *fakeReproduceAgent) Doctor(context.Context) error          { return nil }
func (f *fakeReproduceAgent) Reparse(string) (*agent.Result, error) { return nil, nil }

// newReproduceApp loads a real config in a temp dir and seeds one issue and one ship feature.
func newReproduceApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("https://www.example.com/app\n"), 0o644)
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\njira:\n  dry_run: true\n"), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Abs(cfg.State.Path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	_ = st.UpsertProject(ctx, "p", cfg.Target.BaseURL)
	for _, ev := range []model.FeatureEvent{
		{FeatureID: "PROJ-132", Status: "shipped", ShippedSHA: "jira:PROJ-132:abcd1234", ShippedAt: time.Now(), Routes: []string{"/app/entry"},
			Summary: "Entry tabs duplicated", Kind: model.FeatureKindIssue, Ref: "PROJ-132", Details: "Steps: open entry, click 중학", Source: "jira"},
		{FeatureID: "entry-page", Status: "shipped", ShippedSHA: "abc123", ShippedAt: time.Now(), Routes: []string{"/app/entry"}, Summary: "entry page"},
	} {
		if _, err := st.UpsertFeature(ctx, "p", ev); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfgPath: cfgPath, out: &bytes.Buffer{}, cfg: cfg, st: st, log: log.New(io.Discard, "", 0)}
	a.ev = evidence.New(cfg.Abs(cfg.Evidence.Dir))
	return a, a.out.(*bytes.Buffer)
}

const reproduceScript = `scenario:
  id: entry-tabs-repro
  version: 1
covers:
  capability: entry.tabs
  routes: [/app/entry]
steps:
  - goto: /app/entry
  - click: { by: link, name: "중학" }
  - assert_count: { by: role, role: tab, name: "정보", equals: 1 }
oracle:
  source: spec
`

func TestReproduceCommand(t *testing.T) {
	a, out := newReproduceApp(t)
	ctx := context.Background()
	fa := &fakeReproduceAgent{}
	prev := newReproduceAgent
	newReproduceAgent = func(*app) agent.Adapter { return fa }
	t.Cleanup(func() { newReproduceAgent = prev })

	// ship features are refused with a pointer to discover
	err := a.cmdReproduce(ctx, []string{"entry-page"})
	if err == nil || !strings.Contains(err.Error(), "only issue/log") || !strings.Contains(err.Error(), "vigil discover entry-page") {
		t.Fatalf("ship err = %v", err)
	}
	if err := a.cmdReproduce(ctx, nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage err = %v", err)
	}

	// --dry-run writes request + prompt and never calls the model
	if err := a.cmdReproduce(ctx, []string{"--dry-run", "PROJ-132"}); err != nil {
		t.Fatal(err)
	}
	if len(fa.reqs) != 0 {
		t.Fatal("dry-run must not call the agent")
	}
	got := out.String()
	if !strings.Contains(got, "dry-run: reproduce PROJ-132 (issue PROJ-132)") || !strings.Contains(got, "task: reproduce") {
		t.Fatalf("dry-run output = %q", got)
	}
	reqFiles, _ := filepath.Glob(filepath.Join(a.ev.AgentRoot(), "*", "*", agent.FileRequest))
	promptFiles, _ := filepath.Glob(filepath.Join(a.ev.AgentRoot(), "*", "*", agent.FileTask))
	if len(reqFiles) != 1 || len(promptFiles) != 1 {
		t.Fatalf("dry-run files: %v %v", reqFiles, promptFiles)
	}
	if body, _ := os.ReadFile(promptFiles[0]); !strings.Contains(string(body), "PROJ-132") || !strings.Contains(string(body), "Steps: open entry") {
		t.Fatalf("prompt body:\n%s", body)
	}

	// --no-repair keeps the single-job behaviour even when the gate queues a repair
	// (covered below); the inline run itself is exercised next.

	// inline run: the fake agent proposes a script the orchestrator parks for review
	out.Reset()
	fa.res = &agent.Result{Decision: agent.DecisionNeedsReview, Reason: "oracle unclear", ScriptCandidates: []string{reproduceScript}}
	if err := a.cmdReproduce(ctx, []string{"PROJ-132"}); err != nil {
		t.Fatalf("reproduce: %v\n%s", err, out.String())
	}
	if len(fa.reqs) != 1 || fa.reqs[0].Task != agent.TaskReproduce || fa.reqs[0].FeatureID != "PROJ-132" {
		t.Fatalf("agent requests = %+v", fa.reqs)
	}
	got = out.String()
	if !strings.Contains(got, "reproduce PROJ-132 (issue PROJ-132, sha") || !strings.Contains(got, "AGENT_REPRODUCE → DONE") ||
		!strings.Contains(got, "entry-tabs-repro") || !strings.Contains(got, "NEEDS_REVIEW") || !strings.Contains(got, "oracle unclear") {
		t.Fatalf("reproduce output = %q", got)
	}
	sc, err := a.st.GetScenario(ctx, "p", "entry-tabs-repro")
	if err != nil || sc.State != model.StateNeedsReview {
		t.Fatalf("scenario = %+v %v", sc, err)
	}
}

// invalidScript uses a locator kind the DSL does not define: the gate keeps it as
// a CANDIDATE and queues an AGENT_REPAIR, which needs no browser to reproduce here.
const invalidScript = `scenario:
  id: title-max-40
  version: 1
covers:
  capability: assignment.title
  routes: [/app/assignment]
steps:
  - goto: /app/assignment
  - wait_for: { by: xpath, value: "//input" }
  - assert_count: { by: link, name: "제목", equals: 1 }
oracle:
  source: spec
`

// jobsByKind returns the stored jobs of one kind, whatever their state.
func jobsByKind(t *testing.T, a *app, kind model.JobKind) []*model.Job {
	t.Helper()
	jobs, err := a.st.ListJobs(context.Background(), "p", nil, 1000)
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

// A candidate the gate could not settle must be repaired by this command, not
// left for the next loop; a later `reproduce` resumes that repair instead of
// asking the model for a second script.
func TestReproduceSettlesAndResumes(t *testing.T) {
	a, out := newReproduceApp(t)
	ctx := context.Background()
	a.cfg.Policy.AgentFixAttempts = 2
	fa := &fakeReproduceAgent{}
	prev := newReproduceAgent
	newReproduceAgent = func(*app) agent.Adapter { return fa }
	t.Cleanup(func() { newReproduceAgent = prev })

	// 1) --no-repair: the candidate is stored, the repair stays queued
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{invalidScript}}
	if err := a.cmdReproduce(ctx, []string{"--no-repair", "PROJ-132"}); err != nil {
		t.Fatalf("reproduce: %v\n%s", err, out.String())
	}
	sc, _ := a.st.GetScenario(ctx, "p", "title-max-40")
	if sc == nil || sc.State != model.StateCandidate {
		t.Fatalf("candidate = %+v (out %q)", sc, out.String())
	}
	if got := out.String(); !strings.Contains(got, "CANDIDATE") || !strings.Contains(got, `locator.by "xpath" invalid`) || strings.Contains(got, "repair title-max-40 → job") {
		t.Fatalf("--no-repair output = %q", got)
	}
	// operator-initiated work carries priority 110 (the agent-budget bypass) in the DB
	repro := jobsByKind(t, a, model.JobAgentReproduce)
	repairs := jobsByKind(t, a, model.JobAgentRepair)
	if len(repro) != 1 || repro[0].Priority != model.PriorityUserRequest {
		t.Fatalf("reproduce job rows = %+v", repro)
	}
	if len(repairs) != 1 || repairs[0].Priority != model.PriorityUserRequest || repairs[0].State != model.JobReady {
		t.Fatalf("repair job rows = %+v", repairs)
	}

	// 2) --fresh ignores the queued repair and asks the model again
	out.Reset()
	fa.res = &agent.Result{Decision: agent.DecisionNoNewCoverage, CoverageDelta: "nothing new"}
	if err := a.cmdReproduce(ctx, []string{"--fresh", "--no-repair", "PROJ-132"}); err != nil {
		t.Fatalf("fresh: %v\n%s", err, out.String())
	}
	if got := out.String(); strings.Contains(got, "resuming repair") || !strings.Contains(got, "reproduce PROJ-132 (issue PROJ-132") {
		t.Fatalf("--fresh output = %q", got)
	}
	if n := len(jobsByKind(t, a, model.JobAgentReproduce)); n != 2 {
		t.Fatalf("--fresh must start a new reproduce job, jobs = %d", n)
	}

	// 3) plain reproduce resumes the queued repair; the patch is still invalid,
	//    the budget is spent → NEEDS_REVIEW, and no new reproduce job is started
	out.Reset()
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: strings.Replace(invalidScript, "by: xpath", "by: xpath2", 1)}
	if err := a.cmdReproduce(ctx, []string{"PROJ-132"}); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "resuming repair of title-max-40 (job") || !strings.Contains(got, "repair title-max-40 → job") ||
		!strings.Contains(got, "AGENT_REPAIR → DONE") || !strings.Contains(got, `locator.by "xpath2" invalid`) ||
		!strings.Contains(got, "scenario title-max-40 settled: NEEDS_REVIEW") {
		t.Fatalf("resume output = %q", got)
	}
	if n := len(jobsByKind(t, a, model.JobAgentReproduce)); n != 2 {
		t.Fatalf("resume must not start a reproduce job, jobs = %d", n)
	}
	if sc, _ := a.st.GetScenario(ctx, "p", "title-max-40"); sc.State != model.StateNeedsReview {
		t.Fatalf("scenario = %+v", sc)
	}
	// exactly one gate table per agent job that ran (the resumed repair)
	if n := strings.Count(got, "SCENARIO"); n != 1 {
		t.Fatalf("gate tables = %d:\n%s", n, got)
	}
	if len(jobsByKind(t, a, model.JobAgentRepair)) != 1 {
		t.Fatal("the spent budget must not queue another repair")
	}
}

// stubAgentJobs records inline runs; it never produces a gate outcome.
type stubAgentJobs struct{ ran []int64 }

func (s *stubAgentJobs) RunAgentJobNow(_ context.Context, id int64) error {
	s.ran = append(s.ran, id)
	return nil
}
func (s *stubAgentJobs) AgentDirForJob(context.Context, int64) string { return "" }

func TestSettleCandidateStopsCleanly(t *testing.T) {
	a, out := newReproduceApp(t)
	ctx := context.Background()
	st := &stubAgentJobs{}

	// a script that is gone (deleted by a human, stale gate outcome) is reported, not an error
	if err := a.settleCandidate(ctx, st, "long-gone"); err != nil {
		t.Fatalf("missing scenario = %v", err)
	}
	if len(st.ran) != 0 || !strings.Contains(out.String(), "long-gone: not in the corpus any more") {
		t.Fatalf("missing scenario output = %q", out.String())
	}

	m := &model.Scenario{ID: "stuck", ProjectID: "p", State: model.StateCandidate, Fingerprint: "fp", Class: "P1", Mutation: model.MutationReadOnly,
		OracleSource: "spec", Origin: "agent", CurrentVersion: 1, SourceKind: model.FeatureKindIssue, SourceRef: "PROJ-132"}
	v := &model.ScenarioVersion{ScenarioID: "stuck", Version: 1, YAML: invalidScript, Fingerprint: "fp", CreatedBy: "agent"}
	if err := a.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}

	// no repair queued: nothing to run
	out.Reset()
	if err := a.settleCandidate(ctx, st, "stuck"); err != nil {
		t.Fatal(err)
	}
	if len(st.ran) != 0 || !strings.Contains(out.String(), "still CANDIDATE and no AGENT_REPAIR queued") {
		t.Fatalf("stuck output = %q", out.String())
	}

	// a repair that produces no gate outcome stops the loop with the job's reason
	out.Reset()
	id, _, err := a.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentRepair, Priority: model.PriorityUserRequest,
		ScenarioID: "stuck", FeatureID: "PROJ-132"}, "fix:stuck:1")
	if err != nil {
		t.Fatal(err)
	}
	_ = a.st.CompleteJob(ctx, id, "model unavailable: 429 rate limit")
	if _, err := a.st.RequeueJob(ctx, id, time.Now(), "model unavailable: 429 rate limit"); err != nil {
		t.Fatal(err)
	}
	if err := a.settleCandidate(ctx, st, "stuck"); err != nil {
		t.Fatal(err)
	}
	if len(st.ran) != 1 || st.ran[0] != id {
		t.Fatalf("inline runs = %v", st.ran)
	}
	if got := out.String(); !strings.Contains(got, "produced no gate outcome") || !strings.Contains(got, "429 rate limit") {
		t.Fatalf("no-gate output = %q", got)
	}
}
