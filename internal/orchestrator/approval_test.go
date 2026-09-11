package orchestrator

import (
	"context"
	"testing"
	"time"

	"vigil/internal/model"
	"vigil/internal/store"
)

func pendingScenario(t *testing.T, st *store.Store, id string) {
	t.Helper()
	m := &model.Scenario{ID: id, ProjectID: "p", State: model.StatePendingApproval, Fingerprint: "fp-" + id, Class: "P1", Mutation: model.MutationReadOnly,
		OracleSource: "spec", Origin: "agent", SoakTarget: 2, SourceKind: model.FeatureKindIssue, SourceRef: "PROJ-123", CurrentVersion: 1}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "agent"}
	if err := st.CreateScenario(context.Background(), m, v, nil); err != nil {
		t.Fatal(err)
	}
}

// A manual run of a PENDING_APPROVAL script records the outcome and nothing else.
func TestAfterRunPendingApprovalOnlyRecords(t *testing.T) {
	o, st, _, _, _ := newTest(t)
	ctx := context.Background()
	pendingScenario(t, st, "pending")
	for _, outcome := range []model.Outcome{model.OutcomeAppFailure, model.OutcomeScriptDrift, model.OutcomePass, model.OutcomeQAFlake} {
		job := plainJob("pending")
		if err := o.AfterRun(ctx, job, runFor(t, st, job, "pending", outcome), nil); err != nil {
			t.Fatalf("%s: %v", outcome, err)
		}
		sc, _ := st.GetScenario(ctx, "p", "pending")
		if sc.State != model.StatePendingApproval || sc.NextDueAt != nil || sc.SoakPasses != 0 {
			t.Fatalf("%s: scenario changed: %+v", outcome, sc)
		}
		if sc.LastOutcome != outcome || sc.LastRunAt == nil {
			t.Fatalf("%s: outcome not recorded: %+v", outcome, sc)
		}
	}
	if jobs := jobsOfKind(t, st, model.JobAgentRepair); len(jobs) != 0 {
		t.Fatalf("repair jobs = %+v", jobs)
	}
	if jobs := jobsOfKind(t, st, model.JobChromiumConfirm); len(jobs) != 0 {
		t.Fatalf("confirm jobs = %+v", jobs)
	}
	if inc, _ := st.ListIncidents(ctx, "p", true, 10); len(inc) != 0 {
		t.Fatalf("incidents = %+v", inc)
	}
}

// An approved daily script is next due at schedule.daily_at, not now+cadence.
func TestAfterRunDailyCadenceNextDue(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	ctx := context.Background()
	cfg.Schedule.DailyAt = "09:00"
	cfg.Schedule.ActiveHours.TZ = "Asia/Seoul"
	seoul, _ := time.LoadLocation("Asia/Seoul")
	o.now = func() time.Time { return time.Date(2026, 9, 10, 9, 0, 30, 0, seoul).UTC() }
	pendingScenario(t, st, "daily")
	if err := st.SetScenarioApproved(ctx, "p", "daily", "daily", o.now(), o.now()); err != nil {
		t.Fatal(err)
	}
	job := plainJob("daily")
	if err := o.AfterRun(ctx, job, runFor(t, st, job, "daily", model.OutcomePass), nil); err != nil {
		t.Fatal(err)
	}
	sc, _ := st.GetScenario(ctx, "p", "daily")
	want := time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)
	if sc.State != model.StateActive || sc.NextDueAt == nil || !sc.NextDueAt.Equal(want) {
		t.Fatalf("scenario = %+v (want due %s)", sc, want)
	}
}
