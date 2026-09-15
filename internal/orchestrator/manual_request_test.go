package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/model"
)

func TestNormalizeManualRequestTrimsAndBoundsInstructions(t *testing.T) {
	o, _, _, _, _ := newTest(t)

	in, err := o.NormalizeManualRequest(ManualRequest{Instructions: "  verify the dashboard  "})
	if err != nil {
		t.Fatal(err)
	}
	if in.Instructions != "verify the dashboard" {
		t.Fatalf("instructions = %q", in.Instructions)
	}
	if in.Mutation != string(model.MutationReadOnly) {
		t.Fatalf("mutation = %q", in.Mutation)
	}

	for _, tc := range []struct {
		name         string
		instructions string
	}{
		{name: "empty", instructions: " \n\t "},
		{name: "oversized", instructions: strings.Repeat("x", MaxManualInstructionsLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := o.NormalizeManualRequest(ManualRequest{Instructions: tc.instructions}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRegisterManualRequestPersistsFeatureAndUserPriorityJob(t *testing.T) {
	o, st, _, _, _ := newTest(t)
	result, err := o.RegisterManualRequest(context.Background(), ManualRequest{
		FeatureID: "  checkout-qa  ", Instructions: "  verify checkout totals  ", Mutation: "reversible",
		Accounts: []string{"buyer"}, MaxToolCalls: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FeatureID != "checkout-qa" || result.JobID == 0 || !result.Created {
		t.Fatalf("result = %+v", result)
	}
	f, err := st.GetFeature(context.Background(), "p", result.FeatureID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "requested" || f.Source != "manual" || f.Readiness != model.ReadinessReady || f.LastHandledSHA != f.LatestShippedSHA {
		t.Fatalf("feature = %+v", f)
	}
	jobs := jobsOfKind(t, st, model.JobAgentDiscover)
	if len(jobs) != 1 || jobs[0].ID != result.JobID || jobs[0].Priority != model.PriorityUserRequest {
		t.Fatalf("jobs = %+v", jobs)
	}
	p := parsePayload(jobs[0].Payload)
	if p.FeatureID != result.FeatureID || p.Request == nil || p.Request.Instructions != "verify checkout totals" || p.Request.Mutation != "reversible" || p.Request.MaxToolCalls != 12 {
		t.Fatalf("payload = %+v", p)
	}
}

func TestSubmitUserRequestGeneratesUniqueIDsAndSafeDefaults(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Agent.MaxTurns = 25
	cfg.Agent.Timeout.Duration = 7 * time.Minute
	cfg.Agent.Continuations = 1

	firstFeatureID, firstJobID, err := o.SubmitUserRequest(context.Background(), "  inspect the signed-out home page  ")
	if err != nil {
		t.Fatal(err)
	}
	secondFeatureID, secondJobID, err := o.SubmitUserRequest(context.Background(), "inspect the signed-out home page")
	if err != nil {
		t.Fatal(err)
	}
	if firstFeatureID == "" || secondFeatureID == "" || firstFeatureID == secondFeatureID {
		t.Fatalf("generated feature ids are not unique: %q %q", firstFeatureID, secondFeatureID)
	}
	if firstJobID == 0 || secondJobID == 0 || firstJobID == secondJobID {
		t.Fatalf("job ids are not unique: %d %d", firstJobID, secondJobID)
	}
	jobs := jobsOfKind(t, st, model.JobAgentDiscover)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	for _, job := range jobs {
		p := parsePayload(job.Payload)
		if p.Request == nil {
			t.Fatalf("job %d has no request", job.ID)
		}
		if p.Request.Mutation != string(model.MutationReadOnly) || p.Request.MaxToolCalls != 25 || p.Request.TimeoutMinutes != 7 || p.Request.MaxContinuations != 1 {
			t.Fatalf("unsafe or missing defaults: %+v", p.Request)
		}
	}
}

func TestNormalizeManualRequestRejectsUnsafeValues(t *testing.T) {
	o, _, _, _, _ := newTest(t)
	for _, in := range []ManualRequest{
		{Instructions: "test", Mutation: "destructive"},
		{Instructions: "test", Mutation: "unknown"},
		{Instructions: "test", EntryURL: "https://evil.example/test"},
		{Instructions: "test", EntryURL: "ftp://t.example.com/test"},
		{Instructions: "test", EntryURL: "https://user:secret@t.example.com/test"},
		{Instructions: "test", MaxToolCalls: -1},
	} {
		if _, err := o.NormalizeManualRequest(in); err == nil {
			t.Fatalf("expected validation error for %+v", in)
		}
	}
}

// A user bug report produces an executable regression test even when the
// expected behaviour fails. Never repair the oracle or schedule it implicitly.
func TestManualScenarioKeepsFailureAndSelectedSite(t *testing.T) {
	o, st, fr, fa, cfg := newTest(t)
	ctx := context.Background()
	cfg.Target.Environments = map[string]config.Environment{"qa": {BaseURL: "https://example.test/qa", AllowedHosts: []string{"example.test"}, ReadOnly: true}}
	cfg.Target.DefaultEnv = "qa"
	fr.passed = false
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{reproduceCandidate}, Reproduction: &agent.Reproduction{Symptom: "duplicated tab", Reproduced: true, AtStep: 2}}
	feature, id, err := o.SubmitUserRequest(ctx, "Open entry and click the tab. Only one tab should appear; reproduce the duplicate tab report.")
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.ProjectJob(ctx, "p", id)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	sc, err := st.GetScenario(ctx, "p", "entry-tabs-repro")
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != model.StatePendingApproval || sc.NextDueAt != nil {
		t.Fatalf("scenario=%+v", sc)
	}
	runs, err := st.ListRuns(ctx, "p", sc.ID, 1)
	if err != nil || len(runs) != 1 || runs[0].Outcome != model.OutcomeAppFailure || runs[0].Environment != "qa" || runs[0].JobID != id || runs[0].FeatureID != feature {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if len(jobsOfKind(t, st, model.JobAgentRepair)) != 0 {
		t.Fatal("a reproduced assertion failure must not trigger repair")
	}
	if len(fr.specs) != 1 || fr.specs[0].Environment != "qa" {
		t.Fatal(fr.specs)
	}
}

func TestManualScriptRepairStillWaitsForApproval(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	ctx := context.Background()
	cfg.Policy.AgentFixAttempts = 2
	fa.res = &agent.Result{Decision: agent.DecisionNewScript, ScriptCandidates: []string{invalidCandidate}}
	_, id, err := o.SubmitUserRequest(ctx, "Open the reported screen and confirm one tab.")
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.ProjectJob(ctx, "p", id)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.HandleAgentJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	repairs := jobsOfKind(t, st, model.JobAgentRepair)
	if len(repairs) != 1 {
		t.Fatal(repairs)
	}
	fixed := strings.Replace(invalidCandidate, `{ by: xpath, value: "//a" }`, `{ by: link, name: "중학" }`, 1)
	fa.res = &agent.Result{Decision: agent.DecisionPatchScript, ScriptPatch: fixed}
	if err = o.HandleAgentJob(ctx, repairs[0]); err != nil {
		t.Fatal(err)
	}
	sc, _, err := st.GetCurrentScenarioVersion(ctx, "p", "entry-tabs-invalid")
	if err != nil {
		t.Fatal(err)
	}
	if sc.State != model.StatePendingApproval || sc.NextDueAt != nil {
		t.Fatalf("manual repair scheduled without approval: %+v", sc)
	}
}
