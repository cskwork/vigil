package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

func TestVerificationCarriesRunEnvironment(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	ctx := context.Background()
	m := &model.Scenario{ID: "entry", ProjectID: "p", State: model.StateActive, Fingerprint: "fp", Class: "P1", Mutation: model.MutationReadOnly, OracleSource: "contract", SoakTarget: 3}
	v := &model.ScenarioVersion{ScenarioID: "entry", Version: 1, YAML: "scenario:\n  id: entry\n", Fingerprint: "fp", CreatedBy: "seed"}
	if err := s.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := &model.Run{ProjectID: "p", ScenarioID: "entry", ScenarioVersion: 1, Browser: model.BrowserLightpanda, Outcome: model.OutcomePass,
		StartedAt: now, FinishedAt: now, DeployMarker: "v1", Environment: "prod"}
	if _, err := s.st.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	w := get(t, s, "/api/verification?marker=v1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var out verificationPayload
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Checks) != 1 || out.Checks[0].Environment != "prod" {
		t.Fatalf("checks = %+v", out.Checks)
	}
}

// On-demand admin checks have no deployment marker and may still be waiting
// for approval. Neither condition may hide an actual result from its operator.
func TestOnDemandResultsIncludePendingChecksAndSeparateSites(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	s.SetOnDemandMode()
	seedScenario(t, s, "shared", model.StatePendingApproval, "spec", model.OutcomePass)
	now := time.Now()
	if _, e := s.st.InsertRun(context.Background(), &model.Run{ProjectID: "p", ScenarioID: "shared", ScenarioVersion: 1, Browser: model.BrowserChromium, Outcome: model.OutcomeAppFailure, StartedAt: now, FinishedAt: now, Environment: "other"}); e != nil {
		t.Fatal(e)
	}
	w := get(t, s, "/api/verification", nil)
	var out verificationPayload
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if len(out.Checks) != 2 {
		t.Fatalf("want one result per site including pending checks, got %+v", out.Checks)
	}
	sites := map[string]string{}
	for _, c := range out.Checks {
		sites[c.Environment] = c.Outcome
		if c.NotVerified {
			t.Fatal("actual run marked unverified")
		}
	}
	if sites["stg"] != "PASS" || sites["other"] != "APP_FAILURE" {
		t.Fatal(sites)
	}
}
