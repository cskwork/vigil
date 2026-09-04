package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"vigil/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJobClaimAndLease(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, created, err := s.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobRunScenario, Priority: 50, ScenarioID: "a"}, "run:a")
	if err != nil || !created {
		t.Fatalf("enqueue: %v %v", err, created)
	}
	if _, created2, _ := s.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobRunScenario, Priority: 50, ScenarioID: "a"}, "run:a"); created2 {
		t.Fatal("dedup failed")
	}
	_, _, _ = s.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobRunScenario, Priority: 90, ScenarioID: "b"}, "run:b")
	j, err := s.ClaimJob(ctx, "p", "w1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if j.ScenarioID != "b" {
		t.Fatalf("priority order wrong: %s", j.ScenarioID)
	}
	j2, _ := s.ClaimJob(ctx, "p", "w2", time.Millisecond)
	if j2 == nil || j2.ID != id {
		t.Fatal("second claim should get job a")
	}
	time.Sleep(5 * time.Millisecond)
	n, _ := s.ReapExpiredLeases(ctx)
	if n != 1 {
		t.Fatalf("reap expected 1 got %d", n)
	}
	if _, err := s.ClaimJob(ctx, "p", "w3", time.Second); err != nil {
		t.Fatalf("reaped job should be claimable: %v", err)
	}
}

func TestRestorePreemptedJobReturnsLeaseWithoutChargingAttempt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, _, err := s.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "old"}, "agent:old")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimJob(ctx, "p", "agent-1", time.Minute, model.JobAgentDiscover)
	if err != nil || j.ID != id || j.Attempt != 1 {
		t.Fatalf("claim = %+v, %v", j, err)
	}

	restored, err := s.RestorePreemptedJob(ctx, id, "agent-1", time.Now(), "preempted by user request")
	if err != nil || !restored {
		t.Fatalf("restore = %v, %v", restored, err)
	}
	ready, err := s.ClaimJob(ctx, "p", "agent-2", time.Minute, model.JobAgentDiscover)
	if err != nil || ready.ID != id || ready.Attempt != 1 {
		t.Fatalf("reclaim = %+v, %v; cancelled attempt must not count", ready, err)
	}
	var attempts int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id=?`, id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("job_attempts = %d, want only the replacement claim", attempts)
	}
}

func TestRestorePreemptedJobRejectsStaleOwner(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, _, _ := s.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "old"}, "agent:old")
	_, _ = s.ClaimJob(ctx, "p", "agent-1", time.Minute, model.JobAgentDiscover)

	restored, err := s.RestorePreemptedJob(ctx, id, "not-the-owner", time.Now(), "preempted")
	if err != nil || restored {
		t.Fatalf("restore = %v, %v", restored, err)
	}
	jobs, _ := s.ListJobs(ctx, "p", []model.JobState{model.JobLeased}, 10)
	if len(jobs) != 1 || jobs[0].ID != id || jobs[0].Attempt != 1 {
		t.Fatalf("stale restore changed leased job: %+v", jobs)
	}
}

func TestRegisterManualRequestRollsBackFeatureWhenJobInsertFails(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_manual_job BEFORE INSERT ON jobs BEGIN SELECT RAISE(FAIL, 'job rejected'); END`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ev := model.FeatureEvent{FeatureID: "user-qa-1", Status: "requested", ShippedSHA: "manual-1", ShippedAt: now, Summary: "check login", Source: "manual"}
	_, _, err := s.RegisterManualRequest(ctx, "p", ev, &model.Job{
		ProjectID: "p", Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest, FeatureID: ev.FeatureID,
	}, "request:1")
	if err == nil {
		t.Fatal("expected job insert failure")
	}
	if _, err := s.GetFeature(ctx, "p", ev.FeatureID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("feature survived rolled-back request: %v", err)
	}
}

func TestLocks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ok, _ := s.TryAcquireLocks(ctx, []string{"acct1", "fixtureA"}, "job1", time.Minute)
	if !ok {
		t.Fatal("first acquire")
	}
	ok, _ = s.TryAcquireLocks(ctx, []string{"fixtureA", "other"}, "job2", time.Minute)
	if ok {
		t.Fatal("conflicting lock acquired")
	}
	// rollback must not leave "other" held
	ok, _ = s.TryAcquireLocks(ctx, []string{"other"}, "job3", time.Minute)
	if !ok {
		t.Fatal("other should be free after failed all-or-nothing acquire")
	}
	_ = s.ReleaseLocks(ctx, []string{"acct1", "fixtureA"}, "job1")
	ok, _ = s.TryAcquireLocks(ctx, []string{"fixtureA"}, "job2", time.Minute)
	if !ok {
		t.Fatal("release failed")
	}
}

func TestScenarioLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	m := &model.Scenario{ID: "s1", ProjectID: "p", State: model.StateCandidate, Fingerprint: "fp", SoakTarget: 2}
	v := &model.ScenarioVersion{ScenarioID: "s1", Version: 1, YAML: "x", Fingerprint: "fp", CreatedBy: "seed"}
	if err := s.CreateScenario(ctx, m, v, []model.CoverageLink{{ScenarioID: "s1", LinkType: model.LinkRoute, LinkValue: "/training-entry"}}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.FindScenarioByFingerprint(ctx, "p", "fp"); err != nil || d.ID != "s1" {
		t.Fatalf("fingerprint lookup: %v", err)
	}
	ids, _ := s.ScenariosLinkedTo(ctx, "p", model.LinkRoute, []string{"/training-entry"}, []model.ScenarioState{model.StateCandidate})
	if len(ids) != 1 {
		t.Fatalf("impact lookup: %v", ids)
	}
	_ = s.SetScenarioState(ctx, "p", "s1", model.StateSoak)
	r, _ := s.RecordScenarioOutcome(ctx, "p", "s1", model.OutcomePass, time.Now())
	if r.SoakPasses != 1 {
		t.Fatalf("soak counter: %d", r.SoakPasses)
	}
	if _, err := s.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "s1", Browser: model.BrowserLightpanda, Outcome: model.OutcomePass, StartedAt: time.Now(), FinishedAt: time.Now(), DurationMs: 10}); err != nil {
		t.Fatal(err)
	}
	mt, _ := s.GetMetrics(ctx, "s1")
	if mt.Runs != 1 || mt.Passes != 1 {
		t.Fatalf("metrics: %+v", mt)
	}
}
