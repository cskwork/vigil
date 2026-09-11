package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

func issueFeature(t *testing.T, st *store.Store, id, kind, ref, details string) *model.Feature {
	t.Helper()
	ctx := context.Background()
	ev := model.FeatureEvent{FeatureID: id, Status: "shipped", ShippedSHA: "jira:" + ref + ":abcd1234", ShippedAt: time.Now(),
		Routes: []string{"/app/training-entry"}, Summary: "Training entry tabs are duplicated", Kind: kind, Ref: ref, Details: details, Source: "jira"}
	if _, err := st.UpsertFeature(ctx, "p", ev); err != nil {
		t.Fatal(err)
	}
	f, err := st.GetFeature(ctx, "p", id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIssueFeaturePlansReproduce(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	f := issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "Steps: open entry, click 중학 → two 정보 tabs")
	d, err := o.PlanFeature(ctx, f)
	if err != nil || d.Action != model.ActionReproduce {
		t.Fatalf("plan = %+v %v", d, err)
	}
	if err := o.EnqueueForFeature(ctx, f, d); err != nil {
		t.Fatal(err)
	}
	jobs := jobsOfKind(t, st, model.JobAgentReproduce)
	if len(jobs) != 1 || jobs[0].Priority != model.PriorityNewDirectCoverage || jobs[0].FeatureID != "PROJ-123" {
		t.Fatalf("reproduce jobs = %+v", jobs)
	}
	if p := parsePayload(jobs[0].Payload); p.Env != cfg.DefaultEnv().Name || p.Trigger != string(model.ActionReproduce) {
		t.Fatalf("payload = %+v", p)
	}
	if got, _ := st.GetFeature(ctx, "p", "PROJ-123"); got.LastHandledSHA != f.LatestShippedSHA {
		t.Fatal("feature must be marked handled")
	}
	// log kind takes the same path; a ship feature does not
	lg := issueFeature(t, st, "log-deadbeef", model.FeatureKindLog, "deadbeef", "signature: NPE")
	if d, _ := o.PlanFeature(ctx, lg); d.Action != model.ActionReproduce {
		t.Fatalf("log plan = %+v", d)
	}
	ship := feature(t, st, "ship-1", "abc", []string{"/x"}, nil)
	if d, _ := o.PlanFeature(ctx, ship); d.Action != model.ActionBrowserAgentDiscover {
		t.Fatalf("ship plan = %+v", d)
	}
	// without an agent the loop stays deterministic; on a read-only default env nothing starts
	// Circumstantial NO_ACTION is deferred: the feature stays unhandled for the next scan/loop.
	o.agent = nil
	f2 := issueFeature(t, st, "PROJ-124", model.FeatureKindIssue, "PROJ-124", "d")
	d, _ = o.PlanFeature(ctx, f2)
	if d.Action != model.ActionNone || !d.Defer {
		t.Fatalf("no agent: %+v", d)
	}
	if err := o.EnqueueForFeature(ctx, f2, d); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetFeature(ctx, "p", "PROJ-124"); got.LastHandledSHA != "" {
		t.Fatalf("deferred feature must stay unhandled: %+v", got)
	}
	o.agent = &fakeAgent{}
	cfg.Target.Environments = map[string]config.Environment{"prod": {BaseURL: "https://t.example.com", AllowedHosts: []string{"t.example.com"}, ReadOnly: true}}
	cfg.Target.DefaultEnv = "prod"
	if d, _ := o.PlanFeature(ctx, f); d.Action != model.ActionNone || !d.Defer || !strings.Contains(d.Reason, "read-only") {
		t.Fatalf("read-only env: %+v", d)
	}
}

const reproduceCandidate = `scenario:
  id: entry-tabs-repro
  version: 1
  title: Entry shows one 정보 tab for 중학
covers:
  feature: PROJ-123
  capability: entry.training
  routes: [/app/training-entry]
steps:
  - goto: /app/training-entry
  - click: { by: role, role: tab, name: "중학" }
  - assert_count: { by: role, role: tab, name: "정보", equals: 1 }
oracle:
  source: spec
  source_feature: PROJ-123
  source_sha: jira:PROJ-123:abcd1234
`

func TestReproduceCandidateWaitsForApproval(t *testing.T) {
	o, st, fr, fa, _ := newTest(t)
	ctx := context.Background()
	issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "Steps: open entry, click 중학 → two 정보 tabs")
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{reproduceCandidate},
		Reproduction: &agent.Reproduction{Symptom: "two 정보 tabs after clicking 중학", Reproduced: false, Note: "saw one tab"}}

	// PASS: expected behaviour holds → not reproduced, still needs a human
	job := agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1234", "")
	if err := o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	req := fa.reqs[0]
	if req.Task != agent.TaskReproduce || req.MaxScenarios != 1 || req.Summary != "Training entry tabs are duplicated" || req.EntryPath != "/app/training-entry" {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(req.Evidence, "Reported issue PROJ-123:\nSteps: open entry") {
		t.Fatalf("evidence = %q", req.Evidence)
	}
	sc, err := st.GetScenario(ctx, "p", "entry-tabs-repro")
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != model.StatePendingApproval || sc.SourceRef != "PROJ-123" || sc.SourceKind != model.FeatureKindIssue || sc.NextDueAt != nil {
		t.Fatalf("scenario = %+v", sc)
	}
	var rec reproductionRecord
	if err := json.Unmarshal([]byte(sc.Reproduction), &rec); err != nil {
		t.Fatalf("reproduction json %q: %v", sc.Reproduction, err)
	}
	runs, _ := st.ListRuns(ctx, "p", sc.ID, 5)
	if rec.Reproduced || rec.AtStep != 0 || rec.Symptom != "two 정보 tabs after clicking 중학" || len(runs) != 1 || rec.RunID != runs[0].ID {
		t.Fatalf("record = %+v runs=%+v", rec, runs)
	}
	if due, _ := st.ListDueScenarios(ctx, "p", time.Now().Add(24*time.Hour), 10); len(due) != 0 {
		t.Fatalf("PENDING_APPROVAL must never be due: %+v", due)
	}

	// APP_FAILURE: the flow reached the symptom → reproduced at the failed step
	fr.passed = false
	second := strings.Replace(strings.Replace(reproduceCandidate, "id: entry-tabs-repro", "id: entry-tabs-repro-2", 1), "capability: entry.training", "capability: entry.tabs", 1)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{second},
		Reproduction: &agent.Reproduction{Symptom: "two 정보 tabs", Reproduced: true, AtStep: 3}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1235", "")); err != nil {
		t.Fatal(err)
	}
	sc2, _ := st.GetScenario(ctx, "p", "entry-tabs-repro-2")
	if sc2 == nil || sc2.State != model.StatePendingApproval {
		t.Fatalf("scenario 2 = %+v", sc2)
	}
	_ = json.Unmarshal([]byte(sc2.Reproduction), &rec)
	if !rec.Reproduced || rec.AtStep != 2 || rec.RunID == 0 {
		t.Fatalf("record 2 = %+v (fake runner fails at step 2)", rec)
	}

	// SCRIPT_DRIFT: not an answer about the app → repair loop, exhausted → NEEDS_REVIEW
	fr.class = runner.FailLocator
	third := strings.Replace(strings.Replace(reproduceCandidate, "id: entry-tabs-repro", "id: entry-tabs-repro-3", 1), "capability: entry.training", "capability: entry.drift", 1)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{third}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1236", "")); err != nil {
		t.Fatal(err)
	}
	sc3, _ := st.GetScenario(ctx, "p", "entry-tabs-repro-3")
	if sc3 == nil || sc3.State != model.StateNeedsReview || sc3.SourceRef != "PROJ-123" {
		t.Fatalf("scenario 3 = %+v", sc3)
	}
	if len(jobsOfKind(t, st, model.JobAgentRepair)) != 0 {
		t.Fatal("agent_fix_attempts is 0 here: no repair job expected")
	}
}

func TestShipCandidateKeepsSoakPath(t *testing.T) {
	o, st, _, fa, _ := newTest(t)
	ctx := context.Background()
	feature(t, st, "training-entry-page", "def456", []string{"/app/training-entry"}, nil)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{freshCandidate}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentDiscover, "training-entry-page", "def456", "")); err != nil {
		t.Fatal(err)
	}
	sc, _ := st.GetScenario(ctx, "p", "training-entry-tabs")
	if sc == nil || sc.State != model.StateSoak || sc.SourceRef != "" || sc.Reproduction != "" {
		t.Fatalf("ship candidate = %+v", sc)
	}
}

// The agent's script used `by: link` (a locator kind the DSL never defined) and
// was parked in NEEDS_REVIEW before anyone got a repair attempt. Aliases are
// normalized now, and a script that is still invalid enters the repair loop.
const invalidCandidate = `scenario:
  id: entry-tabs-invalid
  version: 1
covers:
  feature: PROJ-123
  capability: entry.invalid
  routes: [/app/training-entry]
steps:
  - goto: /app/training-entry
  - wait_for: { by: xpath, value: "//a" }
  - assert_count: { by: link, name: "정보", equals: 1 }
oracle:
  source: spec
`

func TestInvalidCandidateEntersRepairLoop(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	cfg.Policy.AgentFixAttempts = 2
	ctx := context.Background()
	issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "two 정보 tabs")
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{invalidCandidate}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1234", "")); err != nil {
		t.Fatal(err)
	}
	sc, v, err := st.GetCurrentScenarioVersion(ctx, "p", "entry-tabs-invalid")
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != model.StateCandidate || sc.SourceRef != "PROJ-123" || v.YAML != invalidCandidate {
		t.Fatalf("invalid candidate = %+v v=%q", sc, v.YAML)
	}
	repairs := jobsOfKind(t, st, model.JobAgentRepair)
	if len(repairs) != 1 || repairs[0].ScenarioID != "entry-tabs-invalid" {
		t.Fatalf("repair jobs = %+v", repairs)
	}
	// the reproduce job here is a loop job (priority 90), so its repair is a normal one
	if repairs[0].Priority != model.PriorityRecentFailure {
		t.Fatalf("repair priority = %d", repairs[0].Priority)
	}
	if p := parsePayload(repairs[0].Payload); !strings.Contains(p.ValidationError, `locator.by "xpath" invalid`) {
		t.Fatalf("repair payload = %+v", p)
	}
	var g GateOutcome
	raw, _ := st.GetState(ctx, stateGatePrefix+"entry-tabs-invalid")
	_ = json.Unmarshal([]byte(raw), &g)
	if g.State != model.StateCandidate || !strings.Contains(g.Reason, "DSL validation failed:") || !strings.Contains(g.Reason, "AGENT_REPAIR attempt 1/2") {
		t.Fatalf("gate = %+v", g)
	}

	// The repair request carries the raw script and the validation error, not a run.
	fixed := strings.Replace(invalidCandidate, `{ by: xpath, value: "//a" }`, `{ by: link, name: "중학" }`, 1)
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: fixed}
	j, err := st.ClaimJob(ctx, "p", "w1", time.Minute, model.JobAgentRepair)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.HandleAgentJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	req := fa.reqs[len(fa.reqs)-1]
	if req.Task != agent.TaskRepair || req.FailingScript != invalidCandidate || !strings.HasPrefix(req.FailureDetail, "DSL validation failed:") || !strings.Contains(req.FailureDetail, "xpath") {
		t.Fatalf("repair request = %+v", req)
	}
	// Repaired reproduce candidate: validation run PASS → PENDING_APPROVAL, v2 stored.
	sc, v, _ = st.GetCurrentScenarioVersion(ctx, "p", "entry-tabs-invalid")
	if sc.State != model.StatePendingApproval || v.Version != 2 || v.CreatedBy != "repair" || !strings.Contains(v.YAML, "role: link") {
		t.Fatalf("repaired = %+v v=%+v", sc, v)
	}
	if n := o.stateInt(ctx, fixAttemptsKey(sc.ID)); n != 0 {
		t.Fatalf("fix attempts must reset after validation: %d", n)
	}
}

func TestInvalidCandidateExhaustsToNeedsReview(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	cfg.Policy.AgentFixAttempts = 2
	ctx := context.Background()
	issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "two 정보 tabs")
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{invalidCandidate}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1234", "")); err != nil {
		t.Fatal(err)
	}
	// The repair is still invalid: the budget (2) is spent → NEEDS_REVIEW with the reason.
	stillBad := strings.Replace(invalidCandidate, "by: xpath", "by: xpath2", 1)
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: stillBad}
	j, err := st.ClaimJob(ctx, "p", "w1", time.Minute, model.JobAgentRepair)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.HandleAgentJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	sc, v, _ := st.GetCurrentScenarioVersion(ctx, "p", "entry-tabs-invalid")
	if sc.State != model.StateNeedsReview || v.Version != 2 || v.YAML != stillBad {
		t.Fatalf("exhausted = %+v v=%+v", sc, v)
	}
	ready, _ := st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 100)
	for _, r := range ready {
		if r.Kind == model.JobAgentRepair {
			t.Fatalf("no further repair after the budget is spent: %+v", r)
		}
	}
	var g GateOutcome
	raw, _ := st.GetState(ctx, stateGatePrefix+"entry-tabs-invalid")
	_ = json.Unmarshal([]byte(raw), &g)
	if g.State != model.StateNeedsReview || !strings.Contains(g.Reason, "xpath2") || !strings.Contains(g.Reason, "after 2 agent fix attempt(s)") {
		t.Fatalf("gate = %+v", g)
	}
	// With the default budget (1) an invalid candidate goes straight to review.
	cfg.Policy.AgentFixAttempts = 1
	one := strings.Replace(strings.Replace(invalidCandidate, "id: entry-tabs-invalid", "id: entry-tabs-one", 1), "capability: entry.invalid", "capability: entry.one", 1)
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{one}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentReproduce, "PROJ-123", "jira:PROJ-123:abcd1235", "")); err != nil {
		t.Fatal(err)
	}
	if sc, _ := st.GetScenario(ctx, "p", "entry-tabs-one"); sc == nil || sc.State != model.StateNeedsReview {
		t.Fatalf("one-shot = %+v", sc)
	}
}

func TestReproduceHelpersRefuseShipFeatures(t *testing.T) {
	o, st, _, _, _ := newTest(t)
	ctx := context.Background()
	ship := feature(t, st, "ship-1", "abc", []string{"/x"}, nil)
	if _, _, err := o.EnqueueReproduce(ctx, ship); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("ship enqueue err = %v", err)
	}
	if _, err := o.ReproduceRequest(ctx, ship); !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("ship request err = %v", err)
	}
	f := issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "Steps: open entry")
	req, err := o.ReproduceRequest(ctx, f)
	if err != nil || req.Task != agent.TaskReproduce || req.MaxScenarios != 1 || !strings.Contains(req.Evidence, "Reported issue PROJ-123:\nSteps: open entry") {
		t.Fatalf("request = %+v %v", req, err)
	}
	id, created, err := o.EnqueueReproduce(ctx, f)
	if err != nil || !created || id == 0 {
		t.Fatalf("enqueue = %d %v %v", id, created, err)
	}
	if id2, created2, _ := o.EnqueueReproduce(ctx, f); id2 != id || created2 {
		t.Fatalf("second enqueue must reuse job %d: %d %v", id, id2, created2)
	}
	jobs := jobsOfKind(t, st, model.JobAgentReproduce)
	// operator-initiated: priority 110 is the hourly-agent-budget bypass
	if len(jobs) != 1 || jobs[0].Priority != model.PriorityUserRequest || parsePayload(jobs[0].Payload).Trigger != string(model.ActionReproduce) {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// The repair of an operator-initiated job keeps the operator's precedence and
// budget bypass, so `vigil reproduce` can settle its own candidate inline.
func TestRepairInheritsOperatorPriority(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Policy.AgentFixAttempts = 2
	ctx := context.Background()
	mkScenario(t, st, "operator-candidate", model.StateCandidate, nil)
	job := &model.Job{ID: 7, ProjectID: "p", Kind: model.JobAgentReproduce, Priority: model.PriorityUserRequest}
	if _, _, retried := o.retryFix(ctx, "operator-candidate", job, jobPayload{}, "drift"); !retried {
		t.Fatal("expected a repair attempt")
	}
	jobs := jobsOfKind(t, st, model.JobAgentRepair)
	if len(jobs) != 1 || jobs[0].Priority != model.PriorityUserRequest {
		t.Fatalf("repair jobs = %+v", jobs)
	}
}

// An operator asking for a reproduce the loop already queued outranks the scan,
// and the evidence directory of the run is addressable by job id.
func TestEnqueueReproduceRaisesQueuedJobAndRecordsDir(t *testing.T) {
	o, st, _, fa, _ := newTest(t)
	ctx := context.Background()
	f := issueFeature(t, st, "PROJ-123", model.FeatureKindIssue, "PROJ-123", "two 정보 tabs")
	d, err := o.PlanFeature(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.EnqueueForFeature(ctx, f, d); err != nil {
		t.Fatal(err)
	}
	queued := jobsOfKind(t, st, model.JobAgentReproduce)
	if len(queued) != 1 || queued[0].Priority != model.PriorityNewDirectCoverage {
		t.Fatalf("loop job = %+v", queued)
	}
	id, created, err := o.EnqueueReproduce(ctx, f)
	if err != nil || created || id != queued[0].ID {
		t.Fatalf("enqueue = %d created=%v err=%v", id, created, err)
	}
	raised := jobsOfKind(t, st, model.JobAgentReproduce)
	if len(raised) != 1 || raised[0].Priority != model.PriorityUserRequest {
		t.Fatalf("priority not raised: %+v", raised)
	}

	fa.res = &agent.Result{Decision: agent.DecisionNoNewCoverage, CoverageDelta: "nothing new"}
	job, err := st.ClaimJob(ctx, "p", "w1", time.Minute, model.JobAgentReproduce)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	dir := o.AgentDirForJob(ctx, job.ID)
	if dir == "" {
		t.Fatal("agent dir not recorded for the job")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("agent dir %s: %v", dir, err)
	}
	if o.AgentDirForJob(ctx, job.ID+999) != "" {
		t.Fatal("unknown job must have no dir")
	}
}
