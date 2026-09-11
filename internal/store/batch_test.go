package store

import (
	"context"
	"testing"
	"time"

	"vigil/internal/model"
)

func seedTwoScenarios(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		m := &model.Scenario{ID: id, ProjectID: "p", State: model.StateActive, Fingerprint: "fp-" + id}
		v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "v1 " + id, Fingerprint: "fp-" + id, CreatedBy: "seed"}
		if err := s.CreateScenario(ctx, m, v, []model.CoverageLink{{ScenarioID: id, LinkType: model.LinkRoute, LinkValue: "/" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	// a gets a second, current version; b keeps v1
	if err := s.AddScenarioVersion(ctx, "p", &model.ScenarioVersion{ScenarioID: "a", Version: 2, YAML: "v2 a", Fingerprint: "fp-a2", CreatedBy: "repair", Reason: "fix"}, nil); err != nil {
		t.Fatal(err)
	}
	// a foreign project scenario must never leak into project p
	if err := s.CreateScenario(ctx, &model.Scenario{ID: "z", ProjectID: "other", State: model.StateActive, Fingerprint: "fp-z"},
		&model.ScenarioVersion{ScenarioID: "z", Version: 1, YAML: "z", Fingerprint: "fp-z", CreatedBy: "seed"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "a", Browser: model.BrowserChromium, Outcome: model.OutcomePass, StartedAt: time.Now(), FinishedAt: time.Now(), DurationMs: 40}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "a", Browser: model.BrowserChromium, Outcome: model.OutcomeQAFlake, StartedAt: time.Now(), FinishedAt: time.Now(), DurationMs: 20}); err != nil {
		t.Fatal(err)
	}
}

func TestListCurrentScenarioVersions(t *testing.T) {
	s := openTest(t)
	seedTwoScenarios(t, s)
	got, err := s.ListCurrentScenarioVersions(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(got))
	}
	if got["a"].Version != 2 || got["a"].YAML != "v2 a" || got["a"].CreatedBy != "repair" {
		t.Fatalf("a current = %+v", got["a"])
	}
	if got["b"].Version != 1 || got["b"].YAML != "v1 b" {
		t.Fatalf("b current = %+v", got["b"])
	}
	if _, leaked := got["z"]; leaked {
		t.Fatal("foreign project scenario leaked")
	}
}

func TestListScenarioVersionHistory(t *testing.T) {
	s := openTest(t)
	seedTwoScenarios(t, s)
	got, err := s.ListScenarioVersionHistory(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got["a"]) != 2 || got["a"][0].Version != 2 || got["a"][1].Version != 1 {
		t.Fatalf("a history = %+v", got["a"])
	}
	if got["a"][0].YAML != "" {
		t.Fatal("history must not carry YAML bodies")
	}
	if len(got["b"]) != 1 {
		t.Fatalf("b history = %+v", got["b"])
	}
}

func TestListScenarioMetricsAndLinks(t *testing.T) {
	s := openTest(t)
	seedTwoScenarios(t, s)
	ctx := context.Background()
	m, err := s.ListScenarioMetrics(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if m["a"] == nil || m["a"].Runs != 2 || m["a"].Passes != 1 || m["a"].Flakes != 1 || m["a"].MedianDurationMs != 30 {
		t.Fatalf("metrics a = %+v", m["a"])
	}
	if _, has := m["b"]; has {
		t.Fatal("b never ran; it must not have a metrics row")
	}
	single, _ := s.GetMetrics(ctx, "a")
	if single.Runs != m["a"].Runs || single.MedianDurationMs != m["a"].MedianDurationMs {
		t.Fatalf("batch and single metrics disagree: %+v vs %+v", single, m["a"])
	}
	links, err := s.ListAllCoverageLinks(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(links["a"]) != 1 || links["a"][0].LinkValue != "/a" || links["a"][0].LinkType != model.LinkRoute {
		t.Fatalf("links a = %+v", links["a"])
	}
	if len(links["b"]) != 1 {
		t.Fatalf("links b = %+v", links["b"])
	}
}
