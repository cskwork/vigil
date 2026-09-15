package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/store"
)

func TestExecutionProgressUsesRealHooksAndSurvivesCompletion(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	tracker := &executionTracker{st: st, project: "p", root: t.TempDir()}
	tracker.begin(7, &model.Scenario{ID: "s", Title: "검색 확인"}, "main", "https://example.test/")
	hooks := progressRunner{tracker: tracker}
	ctx := context.Background()
	_ = hooks.Setup(ctx)
	_ = hooks.BeforeStep(ctx, 1, "goto")
	hooks.AfterStep(ctx, 1, "goto", true)
	_ = hooks.BeforeStep(ctx, 2, "assert_text")
	w := httptest.NewRecorder()
	tracker.handler(w, httptest.NewRequest("GET", "/api/execution?job=7", nil))
	var progress execution
	if e = json.Unmarshal(w.Body.Bytes(), &progress); e != nil {
		t.Fatal(e)
	}
	if progress.Completed || progress.Phase != "running" || len(progress.Steps) != 2 || progress.Steps[1].State != "running" {
		t.Fatal(progress)
	}
	hooks.AfterStep(ctx, 2, "assert_text", false)
	hooks.Finish(ctx)
	now := time.Now()
	_, e = st.InsertRun(ctx, &model.Run{JobID: 7, ProjectID: "p", ScenarioID: "s", ScenarioVersion: 1, Browser: model.BrowserChromium, Outcome: model.OutcomeAppFailure, StartedAt: now, FinishedAt: now, FailedStep: 2, FailedAction: "assert_text", Expected: "검색", Actual: "문구 없음"})
	if e != nil {
		t.Fatal(e)
	}
	tracker.complete(7, nil)
	if !tracker.items[7].Completed || tracker.items[7].Outcome != "APP_FAILURE" || tracker.items[7].Steps[1].State != "failed" {
		t.Fatal(tracker.items[7])
	}
	// Same numeric job ID in a different project cannot be observed.
	other := &executionTracker{st: st, project: "other", root: t.TempDir()}
	w = httptest.NewRecorder()
	other.handler(w, httptest.NewRequest("GET", "/api/execution?job=7", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestRequestAggregateDoesNotHideFailureOrReuseOldPass(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tracker := &executionTracker{st: st, project: "p", root: t.TempDir()}
	tracker.begin(77, &model.Scenario{Title: "multi scenario"}, "main", "https://example.test")
	for _, id := range []string{"failed", "passed", "old"} {
		err = st.CreateScenario(ctx, &model.Scenario{ProjectID: "p", ID: id, Title: id, State: model.StatePendingApproval, CurrentVersion: 1}, &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "test"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		outcome := model.OutcomePass
		if id == "failed" {
			outcome = model.OutcomeAppFailure
		}
		at := time.Now().Add(time.Second)
		if id == "old" {
			at = at.Add(-time.Hour)
		}
		_, err = st.InsertRun(ctx, &model.Run{ProjectID: "p", JobID: 77, ScenarioID: id, ScenarioVersion: 1, Browser: model.BrowserChromium, Outcome: outcome, StartedAt: at, FinishedAt: at})
		if err != nil {
			t.Fatal(err)
		}
	}
	gates := []orchestrator.GateOutcome{{ScenarioID: "failed"}, {ScenarioID: "passed"}, {ScenarioID: "old"}}
	tracker.completeRequest(77, gates, nil)
	e := tracker.items[77]
	if !e.Completed || e.Outcome == "PASS" || len(e.Scripts) != 3 || e.Scripts[0].Outcome != "APP_FAILURE" || e.Scripts[1].Outcome != "PASS" || e.Scripts[2].Outcome != "NEEDS_REVIEW" {
		t.Fatalf("%+v", e)
	}
	// Fresh process restores the complete aggregate, including the earlier failure.
	restored := &executionTracker{st: st, project: "p", root: tracker.root}
	w := httptest.NewRecorder()
	restored.handler(w, httptest.NewRequest("GET", "/api/execution?job=77", nil))
	var got execution
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome == "PASS" || len(got.Scripts) != 3 {
		t.Fatalf("%+v", got)
	}
	tracker.begin(78, &model.Scenario{Title: "duplicate"}, "main", "https://example.test")
	tracker.completeRequest(78, []orchestrator.GateOutcome{{ScenarioID: "new", DuplicateOf: "passed", State: model.StateDuplicate}}, nil)
	if tracker.items[78].Outcome == "PASS" {
		t.Fatal("duplicate is not a fresh verification")
	}
}
