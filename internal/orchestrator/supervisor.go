package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/model"
)

// The supervisor is the slow coverage loop. It asks a model where testing is
// missing and turns the answer into ordinary jobs.
//
// It is deliberately the weakest actor in the system: it proposes, this file
// validates, and only actions that survive validation become jobs. Nothing here
// can remove coverage - there is no verb for deletion, and asking for one is
// refused and logged rather than ignored.

// Verdict records what happened to one proposed action (audit trail).
type Verdict struct {
	Action   agent.PlanAction `json:"action"`
	Accepted bool             `json:"accepted"`
	Reason   string           `json:"reason,omitempty"` // why it was refused
	Applied  bool             `json:"applied,omitempty"`
}

// SupervisorResult is one tick, kept for the dashboard and the log.
type SupervisorResult struct {
	At           time.Time `json:"at"`
	Assessment   string    `json:"assessment,omitempty"`
	CoverageGaps []string  `json:"coverage_gaps,omitempty"`
	Saturated    bool      `json:"saturated,omitempty"`
	Verdicts     []Verdict `json:"verdicts,omitempty"`
	DryRun       bool      `json:"dry_run"`
	Skipped      string    `json:"skipped,omitempty"` // set when the tick did nothing
	EvidenceDir  string    `json:"evidence_dir,omitempty"`
}

// SupervisorStateKey holds the most recent SupervisorResult for the UI.
const SupervisorStateKey = config.SupervisorStateKey

// SupervisorTick runs one coverage planning cycle.
func (o *Orchestrator) SupervisorTick(ctx context.Context) (*SupervisorResult, error) {
	out := &SupervisorResult{At: o.now(), DryRun: o.cfg.Supervisor.DryRunEnabled()}
	if !o.cfg.Supervisor.Enabled {
		out.Skipped = "supervisor disabled"
		return out, nil
	}
	// Reconciliation needs no model and no budget, so it runs before either
	// check: scenarios stuck on a relaxed policy are released even in an hour
	// where every planning task was spent.
	if n, err := o.ReconcileStaleReviews(ctx); err != nil {
		o.logger.Printf("supervisor: reconcile: %v", err)
	} else if n > 0 {
		o.logger.Printf("supervisor: released %d scenario(s) parked by the old oracle policy", n)
	}
	if o.agent == nil {
		out.Skipped = "model provider unavailable (rule 12: deterministic QA continues)"
		return out, o.saveSupervisorResult(ctx, out)
	}
	if ok, reason := o.supervisorBudgetOK(ctx); !ok {
		out.Skipped = reason
		return out, o.saveSupervisorResult(ctx, out)
	}

	briefing, err := o.BuildBriefing(ctx)
	if err != nil {
		return out, fmt.Errorf("supervisor: briefing: %w", err)
	}
	dir := o.agentDir("supervisor", o.now())
	out.EvidenceDir = dir
	if err := o.st.RecordBudget(ctx, o.cfg.Project.ID, "supervisor", 1); err != nil {
		return out, err
	}
	plan, err := o.agent.Plan(ctx, briefing, dir)
	if err != nil {
		if plan != nil && plan.ModelUnavailable {
			out.Skipped = "model unavailable: " + err.Error()
			return out, o.saveSupervisorResult(ctx, out)
		}
		return out, fmt.Errorf("supervisor: plan: %w", err)
	}
	out.Assessment, out.CoverageGaps, out.Saturated = plan.Assessment, plan.CoverageGaps, plan.Saturated
	out.Verdicts = o.ValidatePlan(ctx, plan.Actions)

	for i := range out.Verdicts {
		v := &out.Verdicts[i]
		if !v.Accepted {
			o.logger.Printf("supervisor: refused %s %q: %s", v.Action.Verb, v.Action.Target, v.Reason)
			continue
		}
		if out.DryRun {
			o.logger.Printf("supervisor: [dry-run] would %s %q: %s", v.Action.Verb, v.Action.Target, v.Action.Reason)
			continue
		}
		if err := o.applyPlanAction(ctx, v.Action); err != nil {
			v.Accepted, v.Reason = false, "apply failed: "+err.Error()
			o.logger.Printf("supervisor: apply %s %q failed: %v", v.Action.Verb, v.Action.Target, err)
			continue
		}
		v.Applied = true
		o.logger.Printf("supervisor: %s %q applied (%s)", v.Action.Verb, v.Action.Target, v.Action.Reason)
	}
	return out, o.saveSupervisorResult(ctx, out)
}

func (o *Orchestrator) saveSupervisorResult(ctx context.Context, r *SupervisorResult) error {
	return o.setJSONState(ctx, SupervisorStateKey, r)
}

func (o *Orchestrator) supervisorBudgetOK(ctx context.Context) (bool, string) {
	limit := int64(o.cfg.Budget.SupervisorTasksPerHour)
	if limit <= 0 {
		return true, ""
	}
	used, err := o.st.BudgetUsed(ctx, o.cfg.Project.ID, "supervisor", time.Hour)
	if err != nil {
		return false, "budget lookup failed: " + err.Error()
	}
	if used >= limit {
		return false, fmt.Sprintf("supervisor budget exhausted (%d/%d tasks in the last hour)", used, limit)
	}
	return true, ""
}

// ---- validation --------------------------------------------------------------

// ValidatePlan checks every proposed action against the same guards a
// human-written action would face. It never mutates anything.
func (o *Orchestrator) ValidatePlan(ctx context.Context, actions []agent.PlanAction) []Verdict {
	allowed := map[string]bool{}
	for _, v := range config.SupervisorVerbs {
		allowed[v] = true
	}
	denied := map[string]bool{}
	for _, v := range config.SupervisorDeniedVerbs {
		denied[v] = true
	}
	frontier := o.frontierSet(ctx)

	out := make([]Verdict, 0, len(actions))
	accepted, quarantines := 0, 0
	for _, a := range actions {
		v := Verdict{Action: a}
		switch {
		case denied[a.Verb]:
			// Named refusal: the log must show the model reaching outside its mandate.
			v.Reason = "verb is permanently forbidden; coverage is never removed by the supervisor"
		case !allowed[a.Verb]:
			v.Reason = "unknown verb; allowed: " + strings.Join(config.SupervisorVerbs, ", ")
		case strings.TrimSpace(a.Reason) == "":
			v.Reason = "no reason given; an action that cannot be justified is not executed"
		case a.Target == "":
			v.Reason = "no target"
		case accepted >= o.cfg.Supervisor.MaxActions:
			v.Reason = fmt.Sprintf("over max_actions (%d)", o.cfg.Supervisor.MaxActions)
		default:
			v.Reason = o.validateTarget(ctx, a, frontier, &quarantines)
		}
		v.Accepted = v.Reason == ""
		if v.Accepted {
			accepted++
		}
		out = append(out, v)
	}
	return out
}

// validateTarget returns "" when the action is allowed, else the refusal reason.
func (o *Orchestrator) validateTarget(ctx context.Context, a agent.PlanAction, frontier map[string]bool, quarantines *int) string {
	if a.Verb == "discover_route" {
		if reason := o.validateDiscoveryTarget(a.Target); reason != "" {
			return reason
		}
		if ok, reason := o.agentBudgetOK(ctx); !ok {
			return reason
		}
		_ = frontier // the frontier informs the model; the fence above binds it
		return ""
	}

	sc, err := o.st.GetScenario(ctx, o.cfg.Project.ID, a.Target)
	if err != nil || sc == nil {
		return "no scenario with that id"
	}
	if sc.Mutation == model.MutationDestructive && !o.cfg.Policy.AllowDestructive {
		return "scenario is destructive and policy.allow_destructive is false"
	}
	switch a.Verb {
	case "validate_candidate":
		if sc.State != model.StateCandidate {
			return "scenario is " + string(sc.State) + ", not CANDIDATE"
		}
	case "repair_script":
		if ok, reason := o.agentBudgetOK(ctx); !ok {
			return reason
		}
	case "raise_cadence":
		if a.Class != "P0" && a.Class != "P1" && a.Class != "P2" {
			return "class must be P0, P1 or P2"
		}
	case "quarantine":
		*quarantines++
		if *quarantines > o.cfg.Supervisor.MaxQuarantinePerPlan {
			return fmt.Sprintf("over max_quarantine_per_plan (%d)", o.cfg.Supervisor.MaxQuarantinePerPlan)
		}
		if sc.State != model.StateActive && sc.State != model.StateSoak {
			return "only ACTIVE or SOAK scenarios can be quarantined"
		}
		// Never let the supervisor blind a surface completely.
		if last, area := o.isLastCoverage(ctx, sc.ID); last {
			return "it is the only remaining ACTIVE/SOAK scenario covering " + area
		}
	}
	return ""
}

// isLastCoverage reports whether quarantining id would leave one of its linked
// routes or capabilities with no ACTIVE/SOAK scenario at all.
func (o *Orchestrator) isLastCoverage(ctx context.Context, id string) (bool, string) {
	links, err := o.st.ListCoverageLinks(ctx, o.cfg.Project.ID, id)
	if err != nil {
		return true, "an unknown area (coverage lookup failed)" // fail closed: refuse
	}
	states := []model.ScenarioState{model.StateActive, model.StateSoak}
	for _, l := range links {
		if l.LinkType != model.LinkRoute && l.LinkType != model.LinkCapability {
			continue
		}
		ids, err := o.st.ScenariosLinkedTo(ctx, o.cfg.Project.ID, l.LinkType, []string{l.LinkValue}, states)
		if err != nil {
			return true, string(l.LinkType) + " " + l.LinkValue
		}
		others := 0
		for _, other := range ids {
			if other != id {
				others++
			}
		}
		if others == 0 {
			return true, string(l.LinkType) + " " + l.LinkValue
		}
	}
	return false, ""
}

// SiteRoutesKey holds the discovered-route inventory: every in-scope path an
// agent or a run has actually visited. It is how "give me a URL and find the
// rest yourself" works - the map fills itself in as browsers move around.
const SiteRoutesKey = config.SiteRoutesKey

// frontierSet is every route/capability the supervisor knows about: declared in
// config, plus everything discovered by browsing. It is a menu, not a fence -
// the fence is validateDiscoveryTarget.
func (o *Orchestrator) frontierSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for _, e := range o.cfg.Discovery.RouteMap {
		if e.Route != "" {
			out[e.Route] = true
		}
		if e.Capability != "" {
			out[e.Capability] = true
		}
	}
	for _, r := range o.cfg.Supervisor.ExploreRoutes {
		if r != "" {
			out[r] = true
		}
	}
	for r := range o.SiteRoutes(ctx) {
		out[r] = true
	}
	// With nothing known yet, the entry path of the target is always a valid
	// starting point: one exploration there seeds the rest.
	if len(out) == 0 {
		out[basePath(o.cfg.Target.BaseURL)] = true
	}
	return out
}

func basePath(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// SiteRoutes returns the discovered-route inventory.
func (o *Orchestrator) SiteRoutes(ctx context.Context) map[string]string {
	out := map[string]string{}
	raw, err := o.st.GetState(ctx, SiteRoutesKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

// RecordSiteRoutes folds visited URLs into the inventory. Out-of-scope hosts and
// denied paths are dropped here so they can never become a discovery target.
func (o *Orchestrator) RecordSiteRoutes(ctx context.Context, urls []string) {
	if len(urls) == 0 {
		return
	}
	known := o.SiteRoutes(ctx)
	limit := o.cfg.Supervisor.MaxSiteRoutes
	if limit <= 0 {
		limit = 500
	}
	now := o.now().UTC().Format(time.RFC3339)
	added := 0
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Path == "" {
			continue
		}
		if u.Host != "" && !o.cfg.HostAllowed(u.Host) {
			continue
		}
		p := normalizeRoutePath(u.Path)
		if p == "" || known[p] != "" {
			continue
		}
		if o.deniedPath(p) {
			continue
		}
		if len(known) >= limit {
			break
		}
		known[p] = now
		added++
	}
	if added == 0 {
		return
	}
	if err := o.setJSONState(ctx, SiteRoutesKey, known); err != nil {
		o.logger.Printf("supervisor: recording %d discovered route(s): %v", added, err)
		return
	}
	o.logger.Printf("supervisor: %d new route(s) discovered (%d known)", added, len(known))
}

// normalizeRoutePath drops trailing slashes and collapses obvious id segments so
// /user/1 and /user/2 do not each occupy an inventory slot.
var numericSeg = regexp.MustCompile(`^\d+$|^[0-9a-f]{8}-[0-9a-f-]{20,}$`)

func normalizeRoutePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	for i, seg := range parts {
		if numericSeg.MatchString(seg) {
			parts[i] = ":id"
		}
	}
	out := strings.Join(parts, "/")
	if out == "" {
		return "/"
	}
	return out
}

// exploreInstructions is the plain-language brief handed to the Browser Agent.
// It carries the two things that make a discovered scenario reproducible: one
// pinned identity, and a rule for what to type into inputs.
func (o *Orchestrator) exploreInstructions(reason string) string {
	var b strings.Builder
	b.WriteString("Explore this area and propose scenarios for flows that have no test yet. ")
	b.WriteString(reason)
	if accts := o.cfg.Supervisor.Accounts; len(accts) > 0 {
		b.WriteString("\n\nAlways enter as " + strings.Join(accts, " or ") + ". Do not pick a different name off the page: a scenario that hardcodes whichever account happened to be listed that day will not reproduce.")
	}
	if o.cfg.Supervisor.FillData && o.exploreMutation() != string(model.MutationReadOnly) {
		b.WriteString("\n\nWhere a form or input exists, fill it with realistic values and carry the flow through to its result, then assert on what the application does with the data. A scenario that only looks at a page proves less than one that puts data in and checks the outcome. Prefer values you can recognise later (a title with a fixed prefix), keep every write undoable, and never submit anything that would affect another user.")
	}
	b.WriteString("\n\nRead the element type before acting on it: a list of accounts in a <select> needs a select step, not a click on a list item.")
	// Three of the first sixteen candidates were rejected for leaving the target
	// (a support link, a partner domain). The agent has to be told: an outbound
	// link is something to note, not something to follow.
	b.WriteString("\n\nStay on " + strings.Join(o.cfg.Target.AllowedHosts, ", ") + ". Some links leave the application - support pages, partner sites. Do not open them: a scenario that visits another host is rejected outright and the whole exploration is wasted. Note the link exists and carry on inside the target.")
	return b.String()
}

// exploreMutation refuses to hand out destructive unless policy allows it, so
// one config line cannot quietly widen what an autonomous loop may do.
func (o *Orchestrator) exploreMutation() string {
	m := o.cfg.Supervisor.EffectiveMutation()
	if m == string(model.MutationDestructive) && !o.cfg.Policy.AllowDestructive {
		o.logger.Printf("supervisor: explore_mutation=destructive ignored; policy.allow_destructive is false")
		return string(model.MutationReversible)
	}
	return m
}

func (o *Orchestrator) deniedPath(p string) bool {
	for _, pat := range o.cfg.Supervisor.EffectiveDenyPatterns() {
		if pat == "" {
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// validateDiscoveryTarget is the real fence for autonomous exploration. The
// operator gives a base URL; anything under an allowed host is fair game except
// paths that would end the session or press something irreversible.
func (o *Orchestrator) validateDiscoveryTarget(target string) string {
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return "target is not a usable path or URL"
		}
		if !o.cfg.HostAllowed(u.Host) {
			return "host " + u.Host + " is not in target.allowed_hosts"
		}
		target = u.Path
	}
	// A capability name (no slash) is a valid target too; it is resolved against
	// coverage links, not fetched.
	if !strings.HasPrefix(target, "/") {
		if strings.ContainsAny(target, " \t") {
			return "capability names cannot contain spaces"
		}
		return ""
	}
	if o.deniedPath(target) {
		return "path matches a supervisor deny pattern (session-ending or irreversible)"
	}
	return ""
}

// ---- apply --------------------------------------------------------------------

func (o *Orchestrator) applyPlanAction(ctx context.Context, a agent.PlanAction) error {
	p := o.cfg.Project.ID
	switch a.Verb {
	case "discover_route":
		// Reuse the manual-request path: an entry path plus plain-language intent.
		req := &agent.Request{
			Task:         agent.TaskDiscover,
			EntryPath:    a.Target,
			Instructions: o.exploreInstructions(a.Reason),
			Mutation:     o.exploreMutation(),
			Accounts:     o.cfg.Supervisor.Accounts,
		}
		_, _, err := o.enqueueAgentJob(ctx, model.JobAgentDiscover, "explore:"+a.Target, "",
			jobPayload{Request: req, EntryPath: a.Target, Trigger: "supervisor"})
		return err
	case "repair_script":
		_, _, err := o.st.EnqueueJob(ctx, &model.Job{
			ProjectID: p, Kind: model.JobAgentRepair, Priority: model.PriorityRecentFailure,
			ScenarioID: a.Target, Payload: jobPayload{ScenarioID: a.Target, Trigger: "supervisor"}.String(),
		}, "supervisor:repair:"+a.Target)
		return err
	case "validate_candidate":
		_, _, err := o.st.EnqueueJob(ctx, &model.Job{
			ProjectID: p, Kind: model.JobValidateCandidate, Priority: model.PrioritySoak,
			ScenarioID: a.Target, Payload: jobPayload{ScenarioID: a.Target, Trigger: "supervisor"}.String(),
		}, "supervisor:validate:"+a.Target)
		return err
	case "run_scenario":
		_, _, err := o.st.EnqueueJob(ctx, &model.Job{
			ProjectID: p, Kind: model.JobRunScenario, Priority: model.PriorityImpacted,
			ScenarioID: a.Target, Payload: jobPayload{ScenarioID: a.Target, Trigger: "supervisor"}.String(),
		}, "supervisor:run:"+a.Target)
		return err
	case "raise_cadence":
		sc, err := o.st.GetScenario(ctx, p, a.Target)
		if err != nil {
			return err
		}
		return o.st.UpdateScenarioMeta(ctx, p, a.Target, sc.Title, a.Class, sc.Mutation, sc.Locks)
	case "quarantine":
		return o.st.SetScenarioState(ctx, p, a.Target, model.StateQuarantined)
	}
	return fmt.Errorf("supervisor: no applier for verb %q", a.Verb)
}

// ---- briefing -------------------------------------------------------------------

// BuildBriefing renders the state the supervisor reasons over. It is plain text
// on purpose: it goes into a prompt, and it must be readable in the evidence dir
// when someone asks why the model decided what it did.
func (o *Orchestrator) BuildBriefing(ctx context.Context) (string, error) {
	p := o.cfg.Project.ID
	var b strings.Builder
	fmt.Fprintf(&b, "target: %s\n", o.cfg.Target.BaseURL)
	fmt.Fprintf(&b, "allowed_hosts: %s\n\n", strings.Join(o.cfg.Target.AllowedHosts, ", "))

	scs, err := o.st.ListScenarios(ctx, p)
	if err != nil {
		return "", err
	}
	// coverage per route/capability, and each scenario's health
	cover := map[string][]string{}
	b.WriteString("scenarios:\n")
	if len(scs) == 0 {
		b.WriteString("  (none yet - every area is a gap)\n")
	}
	for _, sc := range scs {
		m, _ := o.st.GetMetrics(ctx, sc.ID)
		runs, passes, flakes := 0, 0, 0
		if m != nil {
			runs, passes, flakes = m.Runs, m.Passes, m.Flakes
		}
		fmt.Fprintf(&b, "  - id: %s\n    state: %s\n    class: %s\n    last_outcome: %s\n    consecutive_failures: %d\n    runs: %d passes: %d flakes: %d\n    title: %s\n",
			sc.ID, sc.State, sc.Class, firstNonEmpty(string(sc.LastOutcome), "(never run)"), sc.ConsecutiveFailures, runs, passes, flakes, sc.Title)
		links, _ := o.st.ListCoverageLinks(ctx, p, sc.ID)
		for _, l := range links {
			if l.LinkType == model.LinkRoute || l.LinkType == model.LinkCapability {
				key := string(l.LinkType) + " " + l.LinkValue
				cover[key] = append(cover[key], sc.ID)
			}
		}
	}

	b.WriteString("\ncoverage_by_area:\n")
	frontier := o.frontierSet(ctx)
	keys := make([]string, 0, len(cover))
	for k := range cover {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %d scenario(s)\n", k, len(cover[k]))
	}
	// Areas the supervisor may explore that nothing covers yet: the frontier.
	var gaps []string
	for area := range frontier {
		if len(cover["route "+area]) == 0 && len(cover["capability "+area]) == 0 {
			gaps = append(gaps, area)
		}
	}
	sort.Strings(gaps)
	b.WriteString("\nknown_areas_with_no_scenario:\n")
	if len(gaps) == 0 {
		b.WriteString("  (every area seen so far has coverage - explore further to find more)\n")
	}
	for _, g := range gaps {
		fmt.Fprintf(&b, "  - %s\n", g)
	}

	b.WriteString("\nknown_areas (seen by a browser or declared in config):\n")
	fkeys := make([]string, 0, len(frontier))
	for k := range frontier {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	for _, k := range fkeys {
		mark := ""
		if len(cover["route "+k]) == 0 && len(cover["capability "+k]) == 0 {
			mark = "   <- no scenario"
		}
		fmt.Fprintf(&b, "  - %s%s\n", k, mark)
	}
	b.WriteString("\nYou are not limited to this list. Any path under the allowed hosts is a\n")
	b.WriteString("valid discover_route target, including one you infer but nobody has visited\n")
	b.WriteString("yet. The list is what is already known, not a fence.\n")

	if incs, err := o.st.ListIncidents(ctx, p, true, 20); err == nil && len(incs) > 0 {
		b.WriteString("\nopen_incidents:\n")
		for _, i := range incs {
			fmt.Fprintf(&b, "  - %s (%s)\n", i.Title, i.Kind)
		}
	}
	agentUsed, _ := o.st.BudgetUsed(ctx, p, "agent", time.Hour)
	fmt.Fprintf(&b, "\nbudget_last_hour:\n  browser_agent_tasks: %d/%d\n", agentUsed, o.cfg.Budget.AgentTasksPerHour)
	fmt.Fprintf(&b, "  max_actions_this_plan: %d\n", o.cfg.Supervisor.MaxActions)

	b.WriteString("\nexploration_limits:\n")
	switch o.exploreMutation() {
	case string(model.MutationReadOnly):
		b.WriteString("  mutation: read-only - scenarios may look but must never submit a form,\n")
		b.WriteString("    create anything, or change state. Propose viewing and asserting only.\n")
	case string(model.MutationReversible):
		b.WriteString("  mutation: reversible - scenarios may create things that can be undone\n")
		b.WriteString("    (start an assessment, submit an answer as a test account, save a draft).\n")
		b.WriteString("    They must never delete another user's data or do anything irreversible.\n")
	default:
		b.WriteString("  mutation: destructive is permitted by policy; still prefer the mildest\n")
		b.WriteString("    flow that proves the behaviour.\n")
	}
	b.WriteString("  paths that end a session (logout) or look irreversible (delete, purge)\n")
	b.WriteString("    are refused automatically; do not spend actions on them.\n")
	return b.String(), nil
}

// ---- reconcile ------------------------------------------------------------------

// ReconcileStaleReviews releases scenarios that are waiting on a policy that no
// longer exists. A scenario parked in NEEDS_REVIEW solely because
// observation_oracle said "ask a human" is stale once that policy is soak: the
// harness re-gates it itself instead of leaving it for someone to notice.
//
// Released scenarios do not skip anything: they go back to CANDIDATE and
// through the same validation run a fresh candidate faces. Scenarios parked for
// any other reason - host violations, failed validations, agent uncertainty -
// are untouched; those verdicts still stand.
func (o *Orchestrator) ReconcileStaleReviews(ctx context.Context) (int, error) {
	if o.cfg.Policy.ObservationOracle == "needs_review" {
		return 0, nil // the policy still wants a human; nothing is stale
	}
	scs, err := o.st.ListScenarios(ctx, o.cfg.Project.ID, model.StateNeedsReview)
	if err != nil {
		return 0, err
	}
	released := 0
	for _, sc := range scs {
		var g GateOutcome
		if ok, err := o.getJSONState(ctx, stateGatePrefix+sc.ID, &g); err != nil || !ok {
			continue
		}
		if g.Reason != ReasonObservationOracle {
			continue
		}
		if err := o.st.SetScenarioState(ctx, o.cfg.Project.ID, sc.ID, model.StateCandidate); err != nil {
			o.logger.Printf("reconcile: %s: %v", sc.ID, err)
			continue
		}
		j := &model.Job{
			ProjectID: o.cfg.Project.ID, Kind: model.JobValidateCandidate, Priority: model.PrioritySoak,
			ScenarioID: sc.ID, Payload: jobPayload{ScenarioID: sc.ID, Trigger: "reconcile"}.String(),
		}
		if _, _, err := o.st.EnqueueJob(ctx, j, "reconcile:"+sc.ID+":"+strconv.Itoa(sc.CurrentVersion)); err != nil {
			o.logger.Printf("reconcile: %s: enqueue: %v", sc.ID, err)
			continue
		}
		g.State, g.Reason = model.StateCandidate, "observation_oracle is no longer needs_review; re-gated for validation"
		g.At = o.now()
		_ = o.recordGate(ctx, g)
		released++
		o.logger.Printf("reconcile: %s NEEDS_REVIEW → CANDIDATE (oracle policy relaxed); validation enqueued", sc.ID)
	}
	return released, nil
}
