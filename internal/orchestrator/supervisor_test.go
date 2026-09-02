package orchestrator

import (
	"context"
	"strings"
	"testing"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

func supervisorCfg(cfg *config.Config) {
	cfg.Supervisor.Enabled = true
	cfg.Supervisor.MaxActions = 3
	cfg.Supervisor.MaxQuarantinePerPlan = 1
	cfg.Supervisor.ExploreRoutes = []string{"/reports", "/settings"}
	cfg.Discovery.RouteMap = []config.RouteMapEntry{
		{PathPrefix: "src/entry/", Route: "/entry", Capability: "entry"},
	}
	cfg.Budget.SupervisorTasksPerHour = 4
}

// mkScenario inserts a scenario with the given coverage links.
func mkScenario(t *testing.T, st *store.Store, id string, state model.ScenarioState, links []model.CoverageLink) {
	t.Helper()
	m := &model.Scenario{ID: id, ProjectID: "p", State: state, Fingerprint: "fp-" + id, Class: "P1",
		Mutation: model.MutationReadOnly, OracleSource: "contract", Origin: "seed", SoakTarget: 2, Title: id}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "seed"}
	for i := range links {
		links[i].ScenarioID = id
	}
	if err := st.CreateScenario(context.Background(), m, v, links); err != nil {
		t.Fatal(err)
	}
}

func verdictFor(vs []Verdict, verb, target string) *Verdict {
	for i := range vs {
		if vs[i].Action.Verb == verb && vs[i].Action.Target == target {
			return &vs[i]
		}
	}
	return nil
}

// The verbs the user must never be able to lose coverage to. Each is refused by
// name so the log shows the model reaching outside its mandate.
func TestValidatePlanRefusesDestructiveVerbs(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	mkScenario(t, st, "s1", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/entry"}})

	var actions []agent.PlanAction
	for _, verb := range config.SupervisorDeniedVerbs {
		actions = append(actions, agent.PlanAction{Verb: verb, Target: "s1", Reason: "model wants to"})
	}
	vs := o.ValidatePlan(context.Background(), actions)
	for _, v := range vs {
		if v.Accepted {
			t.Errorf("verb %q was accepted; it must always be refused", v.Action.Verb)
		}
		if !strings.Contains(v.Reason, "forbidden") {
			t.Errorf("verb %q refused with %q, want a 'forbidden' reason", v.Action.Verb, v.Reason)
		}
	}
}

func TestValidatePlanRefusesUnknownVerbAndMissingFields(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	mkScenario(t, st, "s1", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/entry"}})

	vs := o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "rm -rf", Target: "s1", Reason: "why not"},
		{Verb: "run_scenario", Target: "s1"},            // no reason
		{Verb: "run_scenario", Reason: "no target set"}, // no target
		{Verb: "run_scenario", Target: "nope", Reason: "unknown scenario"},
	})
	for i, want := range []string{"unknown verb", "no reason given", "no target", "no scenario with that id"} {
		if vs[i].Accepted {
			t.Errorf("action %d accepted, want refusal", i)
		}
		if !strings.Contains(vs[i].Reason, want) {
			t.Errorf("action %d reason = %q, want it to mention %q", i, vs[i].Reason, want)
		}
	}
}

// The operator gives a base URL and nothing else, so an unvisited path must be
// a legal target. The fence is the host allowlist plus the deny patterns.
func TestValidatePlanAllowsUnseenInScopePaths(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	cfg.Supervisor.MaxActions = 10 // this test is about the fence, not the cap

	vs := o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "discover_route", Target: "/reports", Reason: "seeded"},
		{Verb: "discover_route", Target: "/entry", Reason: "declared in route_map"},
		{Verb: "discover_route", Target: "entry", Reason: "capability name"},
		{Verb: "discover_route", Target: "/nobody/has/been/here", Reason: "inferred from a menu link"},
		{Verb: "discover_route", Target: "https://t.example.com/deep/page", Reason: "absolute, in scope"},
	})
	for i, v := range vs {
		if !v.Accepted {
			t.Errorf("action %d (%s) refused: %s", i, v.Action.Target, v.Reason)
		}
	}
}

// Autonomous exploration must not wander off the target, end its own session,
// or press something irreversible.
func TestValidatePlanFencesDiscovery(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	supervisorCfg(cfg)

	cases := []struct{ target, wantReason string }{
		{"https://evil.example.org/phish", "allowed_hosts"},
		{"/account/logout", "deny pattern"},
		{"/users/42/delete", "deny pattern"},
		{"/admin/purge-all", "deny pattern"},
		{"/settings?action=delete", "deny pattern"},
		{"/account/deactivate", "deny pattern"},
	}
	var actions []agent.PlanAction
	for _, c := range cases {
		actions = append(actions, agent.PlanAction{Verb: "discover_route", Target: c.target, Reason: "model wants to"})
	}
	vs := o.ValidatePlan(context.Background(), actions)
	for i, c := range cases {
		if vs[i].Accepted {
			t.Errorf("%s was accepted; it must be refused", c.target)
			continue
		}
		if !strings.Contains(vs[i].Reason, c.wantReason) {
			t.Errorf("%s refused with %q, want it to mention %q", c.target, vs[i].Reason, c.wantReason)
		}
	}
}

// The inventory is how "just give me a URL" works: it fills itself in from
// where browsers actually went.
func TestRecordSiteRoutesBuildsTheMap(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	ctx := context.Background()

	o.RecordSiteRoutes(ctx, []string{
		"https://t.example.com/reports",
		"https://t.example.com/reports/",       // same route, trailing slash
		"https://t.example.com/user/42",        // id collapsed
		"https://t.example.com/user/77",        // same shape, one slot
		"https://t.example.com/account/logout", // denied, never recorded
		"https://evil.example.org/phish",       // off host, never recorded
		"not a url at all",
	})
	got := o.SiteRoutes(ctx)
	want := []string{"/reports", "/user/:id"}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("inventory missing %q: %v", w, got)
		}
	}
	for _, bad := range []string{"/account/logout", "/phish"} {
		if _, ok := got[bad]; ok {
			t.Errorf("inventory should not contain %q: %v", bad, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("inventory = %v, want exactly the two safe routes", got)
	}
}

// A discovered route becomes something the supervisor can be told about.
func TestDiscoveredRoutesReachTheBriefing(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	ctx := context.Background()
	o.RecordSiteRoutes(ctx, []string{"https://t.example.com/reports/monthly"})

	b, err := o.BuildBriefing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "/reports/monthly") {
		t.Errorf("briefing does not mention the discovered route:\n%s", b)
	}
	if !strings.Contains(b, "not a fence") {
		t.Errorf("briefing must tell the model it may go beyond the list:\n%s", b)
	}
}

// Quarantine is the only verb that reduces what is being watched, so it must
// never blind an area completely.
func TestValidatePlanRefusesQuarantiningLastCoverage(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	mkScenario(t, st, "only", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/entry"}})
	mkScenario(t, st, "pair-a", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/reports"}})
	mkScenario(t, st, "pair-b", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/reports"}})

	vs := o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "quarantine", Target: "only", Reason: "flaky"},
	})
	if vs[0].Accepted {
		t.Fatal("quarantining the only scenario for /entry was accepted")
	}
	if !strings.Contains(vs[0].Reason, "only remaining") {
		t.Errorf("reason = %q", vs[0].Reason)
	}

	vs = o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "quarantine", Target: "pair-a", Reason: "flaky, pair-b still covers /reports"},
	})
	if !vs[0].Accepted {
		t.Fatalf("quarantining one of two scenarios was refused: %s", vs[0].Reason)
	}
}

func TestValidatePlanCapsQuarantineAndActions(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		mkScenario(t, st, id, model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/shared"}})
	}
	vs := o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "quarantine", Target: "a1", Reason: "flaky"},
		{Verb: "quarantine", Target: "a2", Reason: "flaky too"},
		{Verb: "run_scenario", Target: "a3", Reason: "check now"},
		{Verb: "run_scenario", Target: "a4", Reason: "check now"},
		{Verb: "run_scenario", Target: "a5", Reason: "over the action cap"},
	})
	if !vs[0].Accepted {
		t.Fatalf("first quarantine refused: %s", vs[0].Reason)
	}
	if vs[1].Accepted || !strings.Contains(vs[1].Reason, "max_quarantine_per_plan") {
		t.Errorf("second quarantine: accepted=%v reason=%q", vs[1].Accepted, vs[1].Reason)
	}
	if vs[4].Accepted || !strings.Contains(vs[4].Reason, "max_actions") {
		t.Errorf("fifth action: accepted=%v reason=%q", vs[4].Accepted, vs[4].Reason)
	}
}

func TestValidatePlanChecksCadenceClassAndCandidateState(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	mkScenario(t, st, "act", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/entry"}})
	mkScenario(t, st, "cand", model.StateCandidate, nil)

	vs := o.ValidatePlan(context.Background(), []agent.PlanAction{
		{Verb: "raise_cadence", Target: "act", Class: "P0", Reason: "critical path"},
		{Verb: "raise_cadence", Target: "act", Class: "URGENT", Reason: "invalid class"},
		{Verb: "validate_candidate", Target: "cand", Reason: "ready to validate"},
		{Verb: "validate_candidate", Target: "act", Reason: "already ACTIVE"},
	})
	for i, wantOK := range []bool{true, false, true, false} {
		if vs[i].Accepted != wantOK {
			t.Errorf("action %d accepted=%v want %v (%s)", i, vs[i].Accepted, wantOK, vs[i].Reason)
		}
	}
}

// A disabled supervisor must be inert, and an absent model must not be an error.
func TestSupervisorTickIsInertWhenOff(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	res, err := o.SupervisorTick(context.Background())
	if err != nil || res == nil || res.Skipped == "" {
		t.Fatalf("disabled tick: res=%+v err=%v", res, err)
	}
	supervisorCfg(cfg)
	o.agent = nil
	res, err = o.SupervisorTick(context.Background())
	if err != nil {
		t.Fatalf("no-agent tick returned an error: %v", err)
	}
	if !strings.Contains(res.Skipped, "unavailable") {
		t.Errorf("skipped = %q", res.Skipped)
	}
}

// Dry run is the default: the first thing an operator sees is what it wanted to
// do, not what it did.
func TestSupervisorTickDryRunDoesNotEnqueue(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	supervisorCfg(cfg)
	fa.plan = &agent.Plan{
		Assessment: "nothing covers /reports",
		Actions:    []agent.PlanAction{{Verb: "discover_route", Target: "/reports", Reason: "no scenario covers it"}},
	}
	res, err := o.SupervisorTick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Fatal("dry_run should default to true")
	}
	if v := verdictFor(res.Verdicts, "discover_route", "/reports"); v == nil || !v.Accepted || v.Applied {
		t.Fatalf("verdict = %+v; want accepted but not applied", v)
	}
	if n, _ := st.CountJobs(context.Background(), "p", model.JobReady); n != 0 {
		t.Fatalf("dry run enqueued %d job(s)", n)
	}
}

func TestSupervisorTickAppliesWhenLive(t *testing.T) {
	o, st, _, fa, cfg := newTest(t)
	supervisorCfg(cfg)
	live := false
	cfg.Supervisor.DryRun = &live
	fa.plan = &agent.Plan{
		Actions: []agent.PlanAction{
			{Verb: "discover_route", Target: "/reports", Reason: "no scenario covers it"},
			{Verb: "delete_scenario", Target: "/reports", Reason: "tidy up"},
		},
	}
	res, err := o.SupervisorTick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v := verdictFor(res.Verdicts, "discover_route", "/reports"); v == nil || !v.Applied {
		t.Fatalf("discover verdict = %+v; want applied", v)
	}
	if v := verdictFor(res.Verdicts, "delete_scenario", "/reports"); v == nil || v.Accepted {
		t.Fatalf("delete verdict = %+v; want refused", v)
	}
	jobs, err := st.ListJobs(context.Background(), "p", []model.JobState{model.JobReady}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != model.JobAgentDiscover {
		t.Fatalf("jobs = %+v; want one AGENT_DISCOVER", jobs)
	}
}

func TestBuildBriefingNamesGapsAndFrontier(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	mkScenario(t, st, "covered", model.StateActive, []model.CoverageLink{{LinkType: model.LinkRoute, LinkValue: "/entry"}})

	b, err := o.BuildBriefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"covered", "route /entry: 1 scenario(s)", "known_areas_with_no_scenario", "/reports", "/settings"} {
		if !strings.Contains(b, want) {
			t.Errorf("briefing missing %q:\n%s", want, b)
		}
	}
	// A covered area must not be listed as a gap.
	gaps := b[strings.Index(b, "known_areas_with_no_scenario"):]
	gaps = gaps[:strings.Index(gaps, "known_areas (seen by")]
	if strings.Contains(gaps, "/entry") {
		t.Errorf("covered route listed as a gap:\n%s", gaps)
	}
}

// A failed validation usually means a brittle locator, not that a human is
// needed. The agent gets a bounded number of rewrites first.
func TestRetryFixIsBoundedAndResettable(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Policy.AgentFixAttempts = 3
	ctx := context.Background()
	mkScenario(t, st, "flaky", model.StateCandidate, nil)
	job := &model.Job{ID: 1, ProjectID: "p", Kind: model.JobAgentDiscover}

	// attempts 1 and 2 retry, the third escalates
	for want := 1; want <= 2; want++ {
		n, limit, retried := o.retryFix(ctx, "flaky", job, jobPayload{}, "locator not found")
		if !retried || n != want || limit != 3 {
			t.Fatalf("attempt %d: n=%d limit=%d retried=%v", want, n, limit, retried)
		}
	}
	if _, _, retried := o.retryFix(ctx, "flaky", job, jobPayload{}, "still broken"); retried {
		t.Fatal("third failure should escalate, not retry")
	}

	jobs, err := st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	if err != nil {
		t.Fatal(err)
	}
	repairs := 0
	for _, j := range jobs {
		if j.Kind == model.JobAgentRepair && j.ScenarioID == "flaky" {
			repairs++
		}
	}
	// Two distinct repair jobs: the attempt number is in the dedup key, so the
	// second is not swallowed as a duplicate of the first.
	if repairs != 2 {
		t.Fatalf("repair jobs = %d, want 2", repairs)
	}

	// Validating clears the budget, so a drift months later starts fresh.
	o.clearFixAttempts(ctx, "flaky")
	if _, _, retried := o.retryFix(ctx, "flaky", job, jobPayload{}, "new drift"); !retried {
		t.Fatal("after a clean validation the scenario should get a fresh budget")
	}
}

// The default keeps the previous behaviour: one shot, then a human.
func TestRetryFixOffByDefault(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Policy.AgentFixAttempts = 1
	mkScenario(t, st, "one-shot", model.StateCandidate, nil)
	if _, _, retried := o.retryFix(context.Background(), "one-shot", &model.Job{ID: 1}, jobPayload{}, "broken"); retried {
		t.Fatal("agent_fix_attempts=1 must not retry")
	}
}

// A discovered scenario is only reproducible if every run enters as the same
// identity, so the pinned account and the fill rule must reach the agent.
func TestExploreInstructionsCarryAccountAndFillRule(t *testing.T) {
	o, _, _, _, cfg := newTest(t)
	supervisorCfg(cfg)

	// default: no account pinned, no fill rule
	base := o.exploreInstructions("because")
	if strings.Contains(base, "Always enter as") || strings.Contains(base, "fill it with realistic") {
		t.Errorf("unconfigured instructions should not pin or fill:\n%s", base)
	}
	if !strings.Contains(base, "Read the element type") {
		t.Errorf("the element-type hint is unconditional:\n%s", base)
	}
	// Leaving the target wasted three of the first sixteen explorations.
	if !strings.Contains(base, "Stay on t.example.com") || !strings.Contains(base, "rejected outright") {
		t.Errorf("the brief must name the allowed hosts and the cost of leaving:\n%s", base)
	}

	cfg.Supervisor.Accounts = []string{"acct_T01"}
	cfg.Supervisor.FillData = true
	cfg.Supervisor.ExploreMutation = "reversible"
	full := o.exploreInstructions("because")
	for _, want := range []string{"Always enter as acct_T01", "will not reproduce", "fill it with realistic", "undoable"} {
		if !strings.Contains(full, want) {
			t.Errorf("instructions missing %q:\n%s", want, full)
		}
	}

	// fill_data is meaningless while exploration is read-only, so it stays quiet
	cfg.Supervisor.ExploreMutation = "read-only"
	if ro := o.exploreInstructions("because"); strings.Contains(ro, "fill it with realistic") {
		t.Errorf("read-only exploration must not be told to fill forms:\n%s", ro)
	}
}

func TestDiscoverJobCarriesAccounts(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	supervisorCfg(cfg)
	cfg.Supervisor.Accounts = []string{"acct_T01"}
	ctx := context.Background()

	if err := o.applyPlanAction(ctx, agent.PlanAction{Verb: "discover_route", Target: "/reports", Reason: "gap"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v err = %v", jobs, err)
	}
	if !strings.Contains(jobs[0].Payload, "acct_T01") {
		t.Errorf("payload does not carry the pinned account: %s", jobs[0].Payload)
	}
}

// A policy change must release what the old policy parked - by re-validation,
// never by a straight jump to SOAK - and must not touch scenarios parked for
// real findings.
func TestReconcileReleasesOnlyOracleParkedScenarios(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Policy.ObservationOracle = "soak"
	ctx := context.Background()

	mkScenario(t, st, "oracle-parked", model.StateNeedsReview, nil)
	mkScenario(t, st, "host-violation", model.StateNeedsReview, nil)
	mkScenario(t, st, "already-active", model.StateActive, nil)
	_ = o.recordGate(ctx, GateOutcome{ScenarioID: "oracle-parked", State: model.StateNeedsReview, Reason: ReasonObservationOracle})
	_ = o.recordGate(ctx, GateOutcome{ScenarioID: "host-violation", State: model.StateNeedsReview, Reason: "agent decision NEEDS_REVIEW: host allowlist violated: https://x"})

	n, err := o.ReconcileStaleReviews(ctx)
	if err != nil || n != 1 {
		t.Fatalf("released = %d err = %v, want 1", n, err)
	}
	if sc, _ := st.GetScenario(ctx, "p", "oracle-parked"); sc.State != model.StateCandidate {
		t.Errorf("oracle-parked = %s, want CANDIDATE (revalidation, not a free pass)", sc.State)
	}
	if sc, _ := st.GetScenario(ctx, "p", "host-violation"); sc.State != model.StateNeedsReview {
		t.Errorf("host-violation = %s; a real finding must stay parked", sc.State)
	}
	jobs, _ := st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	if len(jobs) != 1 || jobs[0].Kind != model.JobValidateCandidate || jobs[0].ScenarioID != "oracle-parked" {
		t.Fatalf("jobs = %+v, want one VALIDATE_CANDIDATE for oracle-parked", jobs)
	}

	// Idempotent: a second pass finds nothing left to release.
	if n, _ := o.ReconcileStaleReviews(ctx); n != 0 {
		t.Errorf("second pass released %d, want 0", n)
	}
}

// While the policy still says needs_review, nothing moves.
func TestReconcileIsInertUnderTheStrictPolicy(t *testing.T) {
	o, st, _, _, cfg := newTest(t)
	cfg.Policy.ObservationOracle = "needs_review"
	ctx := context.Background()
	mkScenario(t, st, "parked", model.StateNeedsReview, nil)
	_ = o.recordGate(ctx, GateOutcome{ScenarioID: "parked", State: model.StateNeedsReview, Reason: ReasonObservationOracle})
	if n, err := o.ReconcileStaleReviews(ctx); err != nil || n != 0 {
		t.Fatalf("released = %d err = %v, want 0", n, err)
	}
}
