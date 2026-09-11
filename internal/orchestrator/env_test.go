package orchestrator

import (
	"context"
	"testing"

	"vigil/internal/config"
	"vigil/internal/model"
)

// Agent and impacted jobs always carry the default environment, never a read-only one.
func TestOrchestratorJobsCarryDefaultEnv(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Target.DefaultEnv = "stg"
	cfg.Target.Environments = map[string]config.Environment{
		"stg":  {Name: "stg", BaseURL: cfg.Target.BaseURL, AllowedHosts: cfg.Target.AllowedHosts},
		"prod": {Name: "prod", BaseURL: "https://www.example.com", AllowedHosts: []string{"example.com"}, ReadOnly: true},
	}
	ctx := context.Background()
	f := feature(t, st, "training-entry-page", "sha1", []string{"/app/training-entry"}, nil)
	d, err := o.PlanFeature(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.EnqueueForFeature(ctx, f, d); err != nil {
		t.Fatal(err)
	}
	jobs := jobsOfKind(t, st, model.JobAgentDiscover)
	if len(jobs) != 1 {
		t.Fatalf("discover jobs = %+v", jobs)
	}
	if p := parsePayload(jobs[0].Payload); p.Env != "stg" {
		t.Fatalf("agent job env = %q payload=%s", p.Env, jobs[0].Payload)
	}
	// round-trip
	p := parsePayload(jobPayload{FeatureID: "f", Env: "stg"}.String())
	if p.Env != "stg" || p.FeatureID != "f" {
		t.Fatalf("payload round-trip: %+v", p)
	}
}
