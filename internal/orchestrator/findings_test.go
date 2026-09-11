package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/agent"
	"vigil/internal/model"
)

func TestHandleAgentJobPersistsFindingsAndDomainRules(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	ctx := context.Background()
	cfg.Agent.DomainFile = "packs/domain.md"
	_ = os.MkdirAll(filepath.Join(cfg.BaseDir, "packs"), 0o755)
	_ = os.WriteFile(filepath.Join(cfg.BaseDir, "packs", "domain.md"), []byte("- DATA-1 counts equal list lengths\n"), 0o644)
	feature(t, st, "remedy-page-order", "sha1", []string{"/remedy"}, nil)
	fa.res = &agent.Result{Decision: agent.DecisionNoNewCoverage, CoverageDelta: "nothing new", Findings: []agent.Finding{
		{Kind: "data_mismatch", Where: "/remedy card", Expected: "api /api/remedy $.total = 3", Actual: "ui .total = 2", Evidence: "rule DATA-1"},
		{Kind: "Weird", Where: "/remedy", Actual: "typo"},
	}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentVerify, "remedy-page-order", "sha1", "")); err != nil {
		t.Fatal(err)
	}
	if req := fa.reqs[0]; !strings.Contains(req.DomainRules, "DATA-1") {
		t.Fatalf("domain rules not injected: %q", req.DomainRules)
	}
	list, err := st.ListFindings(ctx, "p", true, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("findings = %v err=%v", list, err)
	}
	// newest first: the normalised unknown kind, then the data mismatch
	if list[0].Kind != "display" || !strings.HasPrefix(list[0].Evidence, "kind: weird") || list[0].FeatureID != "remedy-page-order" || list[0].JobID == 0 {
		t.Fatalf("normalised = %+v", list[0])
	}
	if list[1].Kind != "data_mismatch" || list[1].Expected != "api /api/remedy $.total = 3" || list[1].ScenarioID != "" {
		t.Fatalf("mismatch = %+v", list[1])
	}

	// a repair job attaches findings to its own scenario
	seed(t, o, cfg, seedScenario)
	fa.res = &agent.Result{Decision: agent.DecisionNeedsReview, Evidence: "unclear", Findings: []agent.Finding{{Kind: "accessibility", Where: "/remedy", Expected: "label", Actual: "none"}}}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentRepair, "remedy-page-order", "sha1", "visible-remedy-order")); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListOpenFindingsFor(ctx, "p", "visible-remedy-order", "")
	if len(got) != 1 || got[0].Kind != "accessibility" {
		t.Fatalf("repair findings = %+v", got)
	}

	// missing domain file: task still runs, no rules
	cfg.Agent.DomainFile = "packs/missing.md"
	fa.res = &agent.Result{Decision: agent.DecisionNoNewCoverage}
	if err := o.HandleAgentJob(ctx, agentJob(t, st, model.JobAgentVerify, "remedy-page-order", "sha2", "")); err != nil {
		t.Fatal(err)
	}
	if req := fa.reqs[len(fa.reqs)-1]; req.DomainRules != "" {
		t.Fatalf("missing domain file must yield empty rules, got %q", req.DomainRules)
	}
}
