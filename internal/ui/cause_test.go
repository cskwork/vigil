package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

// seedFailingRun adds one ACTIVE script whose newest run failed on a locator,
// plus the incident that failure produced.
func seedFailingRun(t *testing.T, s *Server) int64 {
	t.Helper()
	ctx := context.Background()
	m := &model.Scenario{ID: "entry-tabs", ProjectID: "p", State: model.StateActive, Fingerprint: "fp", Title: "입장 탭",
		Class: "P1", Mutation: model.MutationReadOnly, OracleSource: "spec", SoakTarget: 3, LastOutcome: model.OutcomeScriptDrift}
	v := &model.ScenarioVersion{ScenarioID: "entry-tabs", Version: 1, YAML: "scenario:\n  id: entry-tabs\n", Fingerprint: "fp", CreatedBy: "seed"}
	if err := s.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id, err := s.st.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "entry-tabs", ScenarioVersion: 1,
		Browser: model.BrowserChromium, Outcome: model.OutcomeScriptDrift, StartedAt: now, FinishedAt: now,
		DeployMarker: "v1", Environment: "stg", FailedStep: 4, FailedAction: "click", Expected: "element present",
		Actual: `0 matches; candidates(role=button): "내보내기"`,
		Error:  `step 4 (click): locate by=role role=button name="가져오기": 0 matches; candidates(role=button): "내보내기"`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.CreateIncident(ctx, &model.Incident{ProjectID: "p", Kind: model.IncidentAppRegression,
		ScenarioID: "entry-tabs", RunID: id, Title: "APP_REGRESSION: 입장 탭", State: "OPEN", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return id
}

const wantCause = `4단계에서 누르려던 "가져오기" 버튼이 화면에 없습니다.`

func decode(t *testing.T, s *Server, path string, v any) {
	t.Helper()
	w := get(t, s, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status %d", path, w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v (body %q)", path, err, w.Body.String())
	}
}

// Every failing row carries the four cause layers, and keeps the raw fields for
// level 3.
func TestPayloadsCarryTheCause(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	seedFailingRun(t, s)

	var ver verificationPayload
	decode(t, s, "/api/verification", &ver)
	if len(ver.Checks) != 1 || ver.Checks[0].Cause == nil {
		t.Fatalf("checks = %+v", ver.Checks)
	}
	c := ver.Checks[0].Cause
	if c.Headline != wantCause || c.Kind != "script" || c.Why == "" || c.Next == "" {
		t.Errorf("cause = %+v", c)
	}
	if !strings.Contains(c.Detail, "candidates(role=button)") {
		t.Errorf("level 3 lost the raw text: %q", c.Detail)
	}
	if ver.Checks[0].Error == "" || ver.Checks[0].Actual == "" {
		t.Error("raw fields must stay on the row for level 3")
	}
	if len(ver.Incidents) != 1 || ver.Incidents[0].Cause == nil || ver.Incidents[0].Cause.Headline != wantCause {
		t.Errorf("incident cause = %+v", ver.Incidents)
	}

	var todo todoPayload
	decode(t, s, "/api/todo", &todo)
	if len(todo.Recent) != 1 || todo.Recent[0].Cause == nil || todo.Recent[0].Cause.Headline != wantCause {
		t.Errorf("todo recent = %+v", todo.Recent)
	}

	var scripts scriptsPayload
	decode(t, s, "/api/scripts", &scripts)
	if len(scripts.Scripts) != 1 || len(scripts.Scripts[0].RecentRuns) != 1 {
		t.Fatalf("scripts = %+v", scripts.Scripts)
	}
	run := scripts.Scripts[0].RecentRuns[0]
	if run.Cause == nil || run.Cause.Headline != wantCause || run.FailedAction != "click" {
		t.Errorf("script run = %+v", run)
	}

	var ov struct {
		Runs      []runView      `json:"runs"`
		Incidents []incidentView `json:"incidents"`
	}
	decode(t, s, "/api/overview", &ov)
	if len(ov.Runs) != 1 || ov.Runs[0].Cause == nil || ov.Runs[0].Cause.Headline != wantCause {
		t.Errorf("overview runs = %+v", ov.Runs)
	}
	if len(ov.Incidents) != 1 || ov.Incidents[0].Cause == nil {
		t.Errorf("overview incidents = %+v", ov.Incidents)
	}
}

// A passing run says nothing: level 1 stays empty rather than showing noise.
func TestPassingRowsHaveNoCause(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	seedScenario(t, s, "ok-one", model.StateActive, "spec", model.OutcomePass)
	var ver verificationPayload
	decode(t, s, "/api/verification", &ver)
	if len(ver.Checks) != 1 || ver.Checks[0].Cause != nil {
		t.Errorf("passing check carries a cause: %+v", ver.Checks)
	}
}
