// Package orchestrator applies the PRD §4 decision table and the candidate →
// validate → soak → ACTIVE lifecycle (PRD §9). It never changes a business
// oracle without provenance (AC-09) and never lets duplicates become permanent (AC-10).
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/dsl"
	"vigil/internal/evidence"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// scenarioRunner is what the orchestrator needs from the deterministic runner
// (*runner.Runner satisfies it; tests use a fake).
type scenarioRunner interface {
	Run(ctx context.Context, spec runner.Spec) (*runner.Result, error)
}

// evidenceStore is what the orchestrator needs from the evidence layer
// (*evidence.Store satisfies it; while it is unimplemented every call is best-effort).
type evidenceStore interface {
	RunDir(scenarioID string, at time.Time, attempt int) (string, error)
	AgentDir(featureID string, at time.Time) (string, error)
	WriteIncident(ctx context.Context, in evidence.IncidentInput) (mdPath, jsonPath string, err error)
}

type Orchestrator struct {
	cfg    *config.Config
	st     *store.Store
	run    scenarioRunner
	agent  agent.Adapter // may be nil when provider unavailable (rule 12)
	ev     evidenceStore
	logger *log.Logger
	now    func() time.Time
}

// ErrAgentJobRequeued is a successful handoff back to the queue, not a failed
// execution. The scheduler must retain READY instead of completing the job.
var ErrAgentJobRequeued = errors.New("agent job requeued")

func New(cfg *config.Config, st *store.Store, run *runner.Runner, ag agent.Adapter, ev *evidence.Store) *Orchestrator {
	o := &Orchestrator{cfg: cfg, st: st, agent: ag, logger: log.New(os.Stderr, "[orchestrator] ", log.LstdFlags), now: func() time.Time { return time.Now().UTC() }}
	if run != nil {
		o.run = run
	}
	if ev != nil {
		o.ev = ev
	}
	return o
}

// Decision is the orchestrator's plan for one ready feature.
type Decision struct {
	Action      model.Action
	Reason      string
	ScenarioIDs []string // impacted scripts to run first / to repair
	// Defer marks a NO_ACTION that is only circumstantial (no agent in this
	// process, agent budget spent, read-only default env): the feature must stay
	// unhandled so the next scan/loop plans it again.
	Defer bool
}

// State keys (scheduler_state) written by the orchestrator.
const (
	stateImpactPrefix = "impact:" // impact:<feature>:<sha> → impactState JSON
	stateFlakePrefix  = "flakes:" // flakes:<scenario> → consecutive flake count
	stateGatePrefix   = "gate:"   // gate:<scenario> → last candidate/repair gate outcome JSON
	// agentdir:<job id> → the evidence directory that job's agent run wrote to.
	// Callers that execute a job inline read its gate.json from there instead of
	// guessing which directory belongs to which run.
	stateAgentDirPrefix = "agentdir:"
	stateReviewPrefix   = "review:" // review:<feature>:<sha> → agent findings needing a human
)

// jobPayload is the kind-specific JSON carried by jobs the orchestrator creates.
type jobPayload struct {
	Request      *agent.Request `json:"request,omitempty"` // manual QA request fields merged into the agent request
	FeatureID    string         `json:"feature_id,omitempty"`
	ShippedSHA   string         `json:"shipped_sha,omitempty"`
	Impacted     bool           `json:"impacted,omitempty"`
	ScenarioID   string         `json:"scenario_id,omitempty"`
	Version      int            `json:"version,omitempty"`
	RunID        int64          `json:"run_id,omitempty"`
	ConfirmRunID int64          `json:"confirm_run_id,omitempty"`
	EntryPath    string         `json:"entry_path,omitempty"`
	Trigger      string         `json:"trigger,omitempty"`
	// Operator marks work a person started at a terminal (`vigil reproduce`).
	// It survives the one-shot priority bypass being consumed on claim, so the
	// follow-up repair still knows where it came from.
	Operator bool `json:"operator,omitempty"`
	// ValidationError carries the DSL validation failure of the current script
	// version into an AGENT_REPAIR job: there is no run to read a failure from.
	ValidationError string `json:"validation_error,omitempty"`
	// Env is the target environment. The orchestrator always sets the default
	// environment: agent, impacted and validation work never targets another one.
	Env string `json:"env,omitempty"`
}

func (p jobPayload) String() string {
	b, _ := json.Marshal(p)
	return string(b)
}

func parsePayload(s string) jobPayload {
	var p jobPayload
	_ = json.Unmarshal([]byte(s), &p)
	return p
}

// ---- PlanFeature / EnqueueForFeature ----------------------------------------

// PlanFeature decides what to do for a READY feature (decision table).
func (o *Orchestrator) PlanFeature(ctx context.Context, f *model.Feature) (*Decision, error) {
	if f == nil {
		return nil, errors.New("orchestrator: nil feature")
	}
	if !model.IsShipKind(f.Kind) {
		// A queue item (issue / log signature) is not a deployment: nothing to
		// re-run; the agent reproduces the symptom and a human approves the script.
		if o.agent == nil {
			return &Decision{Action: model.ActionNone, Defer: true, Reason: fmt.Sprintf("%s %s needs the Browser Agent to reproduce it (rule 12: deterministic QA continues)", f.Kind, f.Ref)}, nil
		}
		if env := o.cfg.DefaultEnv(); env.ReadOnly {
			return &Decision{Action: model.ActionNone, Defer: true, Reason: fmt.Sprintf("default environment %s is read-only; reproduce jobs never start there", env.Name)}, nil
		}
		if ok, reason := o.agentBudgetOK(ctx); !ok {
			return &Decision{Action: model.ActionNone, Defer: true, Reason: reason}, nil
		}
		return &Decision{Action: model.ActionReproduce, Reason: fmt.Sprintf("%s %s: reproduce the reported symptom, then wait for approval", f.Kind, f.Ref)}, nil
	}
	ids, err := o.ImpactedScenarios(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		return &Decision{
			Action:      model.ActionRunImpactedScriptsFirst,
			Reason:      fmt.Sprintf("%d ACTIVE/SOAK scenario(s) cover feature/capability/route/path of %s@%s; prove with scripts before rediscovery", len(ids), f.ID, short(f.LatestShippedSHA)),
			ScenarioIDs: ids,
		}, nil
	}
	if o.agent == nil {
		return &Decision{Action: model.ActionNone, Defer: true, Reason: "no trustworthy coverage and Browser Agent unavailable (rule 12: deterministic QA continues)"}, nil
	}
	if ok, reason := o.agentBudgetOK(ctx); !ok {
		return &Decision{Action: model.ActionNone, Defer: true, Reason: reason}, nil
	}
	return &Decision{Action: model.ActionBrowserAgentDiscover, Reason: "new feature with no trustworthy coverage"}, nil
}

func (o *Orchestrator) agentBudgetOK(ctx context.Context) (bool, string) {
	used, err := o.st.BudgetUsed(ctx, o.cfg.Project.ID, "agent", time.Hour)
	if err != nil {
		return false, "budget lookup failed: " + err.Error()
	}
	if limit := int64(o.cfg.Budget.AgentTasksPerHour); limit > 0 && used >= limit {
		return false, fmt.Sprintf("agent budget exhausted (%d/%d tasks in the last hour)", used, limit)
	}
	return true, ""
}

// ImpactedScenarios returns ACTIVE/SOAK scenario ids linked to the feature, ordered by
// impact signal strength (PRD §12): feature → capability → route → source path.
func (o *Orchestrator) ImpactedScenarios(ctx context.Context, f *model.Feature) ([]string, error) {
	states := []model.ScenarioState{model.StateActive, model.StateSoak}
	routes, caps := o.routeMap(f.ChangedPaths)
	routes = append(routes, f.Routes...)
	var out []string
	seen := map[string]bool{}
	add := func(ids []string) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	queries := []struct {
		lt     model.LinkType
		values []string
	}{
		{model.LinkFeature, []string{f.ID}},
		{model.LinkCapability, caps},
		{model.LinkRoute, routes},
		{model.LinkPath, f.ChangedPaths},
	}
	for _, q := range queries {
		if len(q.values) == 0 {
			continue
		}
		ids, err := o.st.ScenariosLinkedTo(ctx, o.cfg.Project.ID, q.lt, q.values, states)
		if err != nil {
			return nil, err
		}
		add(ids)
	}
	return out, nil
}

// routeMap maps changed source paths to routes/capabilities via cfg.Discovery.RouteMap.
func (o *Orchestrator) routeMap(paths []string) (routes, caps []string) {
	seenR, seenC := map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		for _, e := range o.cfg.Discovery.RouteMap {
			if e.PathPrefix == "" || !strings.HasPrefix(p, e.PathPrefix) {
				continue
			}
			if e.Route != "" && !seenR[e.Route] {
				seenR[e.Route] = true
				routes = append(routes, e.Route)
			}
			if e.Capability != "" && !seenC[e.Capability] {
				seenC[e.Capability] = true
				caps = append(caps, e.Capability)
			}
		}
	}
	return routes, caps
}

// impactState tracks impacted-run completion for one feature@sha.
type impactState struct {
	Pending  []int64           `json:"pending"`
	Outcomes map[string]string `json:"outcomes"` // job id → outcome
	Done     bool              `json:"done,omitempty"`
	Result   string            `json:"result,omitempty"`
}

// EnqueueForFeature turns the decision into jobs (impacted runs first, agent task when needed).
func (o *Orchestrator) EnqueueForFeature(ctx context.Context, f *model.Feature, d *Decision) error {
	if f == nil || d == nil {
		return errors.New("orchestrator: nil feature/decision")
	}
	sha := f.LatestShippedSHA
	switch d.Action {
	case model.ActionRunImpactedScriptsFirst:
		st := impactState{Outcomes: map[string]string{}}
		for _, id := range d.ScenarioIDs {
			j := &model.Job{
				ProjectID:  o.cfg.Project.ID,
				Kind:       model.JobRunScenario,
				Priority:   model.PriorityImpacted,
				ScenarioID: id,
				FeatureID:  f.ID,
				Payload:    jobPayload{FeatureID: f.ID, ShippedSHA: sha, Impacted: true, Env: o.cfg.DefaultEnv().Name}.String(),
			}
			jid, _, err := o.st.EnqueueJob(ctx, j, "impacted:"+id+":"+f.ID+":"+sha)
			if err != nil {
				return err
			}
			st.Pending = append(st.Pending, jid)
		}
		if err := o.setJSONState(ctx, stateImpactPrefix+f.ID+":"+sha, st); err != nil {
			return err
		}
	case model.ActionBrowserAgentDiscover, model.ActionBrowserAgentVerifyChange:
		kind := model.JobAgentDiscover
		if d.Action == model.ActionBrowserAgentVerifyChange {
			kind = model.JobAgentVerify
		}
		if _, _, err := o.enqueueAgentJob(ctx, kind, f.ID, sha, jobPayload{FeatureID: f.ID, ShippedSHA: sha, Trigger: string(d.Action), Env: o.cfg.DefaultEnv().Name}); err != nil {
			return err
		}
	case model.ActionReproduce:
		if _, _, err := o.enqueueAgentJob(ctx, model.JobAgentReproduce, f.ID, sha, jobPayload{FeatureID: f.ID, ShippedSHA: sha, Trigger: string(d.Action), Env: o.cfg.DefaultEnv().Name}); err != nil {
			return err
		}
	case model.ActionNone:
		if d.Defer {
			o.logger.Printf("feature %s@%s: deferred, stays unhandled (%s)", f.ID, short(sha), d.Reason)
			return nil
		}
		o.logger.Printf("feature %s@%s: no action (%s)", f.ID, short(sha), d.Reason)
	default:
		return fmt.Errorf("orchestrator: unsupported feature action %s", d.Action)
	}
	return o.st.MarkFeatureHandled(ctx, o.cfg.Project.ID, f.ID, sha)
}

func (o *Orchestrator) enqueueAgentJob(ctx context.Context, kind model.JobKind, featureID, sha string, p jobPayload) (int64, bool, error) {
	return o.enqueueAgentJobAt(ctx, kind, featureID, sha, p, model.PriorityNewDirectCoverage)
}

// enqueueAgentJobAt is enqueueAgentJob with an explicit priority: operator-initiated
// work uses PriorityUserRequest, which is also the hourly-agent-budget bypass.
func (o *Orchestrator) enqueueAgentJobAt(ctx context.Context, kind model.JobKind, featureID, sha string, p jobPayload, priority int) (int64, bool, error) {
	p.Env = o.cfg.DefaultEnv().Name // agent jobs always run on the default environment (never a read-only one)
	j := &model.Job{
		ProjectID: o.cfg.Project.ID,
		Kind:      kind,
		Priority:  priority,
		FeatureID: featureID,
		Payload:   p.String(),
	}
	return o.st.EnqueueJob(ctx, j, "agent:"+featureID+":"+sha)
}

// ---- shared helpers ---------------------------------------------------------

func (o *Orchestrator) setJSONState(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return o.st.SetState(ctx, key, string(b))
}

func (o *Orchestrator) getJSONState(ctx context.Context, key string, v any) (bool, error) {
	s, err := o.st.GetState(ctx, key)
	if err != nil || s == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(s), v)
}

// nextDue is when a scenario runs again: approved daily scripts wait for the
// next schedule.daily_at slot, everything else follows the state/class cadence.
func (o *Orchestrator) nextDue(sc *model.Scenario) time.Time {
	if sc.Cadence == config.DailyCadence {
		return o.cfg.NextDailyRun(o.now())
	}
	return o.now().Add(o.cadence(sc))
}

// cadence returns the scheduling interval for a scenario by state/class (PRD §12).
func (o *Orchestrator) cadence(sc *model.Scenario) time.Duration {
	if sc.State == model.StateSoak {
		return o.cfg.Schedule.Soak.Duration
	}
	switch sc.Class {
	case "P0":
		return o.cfg.Schedule.P0.Duration
	case "P2":
		return o.cfg.Schedule.P2.Duration
	default:
		return o.cfg.Schedule.P1.Duration
	}
}

// loadFlows returns parsed flows and the id set used by dsl.Validate.
func (o *Orchestrator) loadFlows(ctx context.Context) (map[string]*dsl.Flow, map[string]bool, error) {
	raw, err := o.st.ListFlowYAML(ctx, o.cfg.Project.ID)
	if err != nil {
		return nil, nil, err
	}
	flows := map[string]*dsl.Flow{}
	ids := map[string]bool{}
	for id, y := range raw {
		ids[id] = true
		if f, err := dsl.ParseFlow([]byte(y)); err == nil {
			flows[id] = f
		}
	}
	return flows, ids, nil
}

// linksFor derives coverage links from the scenario's covers/persona (PRD §8).
func linksFor(sc *dsl.Scenario) []model.CoverageLink {
	var links []model.CoverageLink
	add := func(lt model.LinkType, v string) {
		if v = strings.TrimSpace(v); v != "" {
			links = append(links, model.CoverageLink{ScenarioID: sc.Scenario.ID, LinkType: lt, LinkValue: v})
		}
	}
	add(model.LinkFeature, sc.Covers.Feature)
	add(model.LinkCapability, sc.Covers.Capability)
	for _, r := range sc.Covers.Routes {
		add(model.LinkRoute, r)
	}
	for _, a := range sc.Covers.APIs {
		add(model.LinkAPI, a)
	}
	for _, p := range sc.Covers.Paths {
		add(model.LinkPath, p)
	}
	add(model.LinkPersona, sc.Preconditions.Persona)
	return links
}

// scenarioModel builds the persistent row for a parsed scenario.
func (o *Orchestrator) scenarioModel(sc *dsl.Scenario, id string, state model.ScenarioState, fp, origin string) *model.Scenario {
	return &model.Scenario{
		ID:             id,
		ProjectID:      o.cfg.Project.ID,
		State:          state,
		Fingerprint:    fp,
		Title:          sc.Scenario.Title,
		Class:          sc.Scenario.Class,
		Mutation:       model.Mutation(sc.Scenario.Mutation),
		Locks:          sc.Resources.Locks,
		CurrentVersion: sc.Scenario.Version,
		OracleSource:   sc.Oracle.Source,
		OracleFeature:  sc.Oracle.SourceFeature,
		OracleSHA:      sc.Oracle.SourceSHA,
		SoakTarget:     o.cfg.Policy.SoakPasses,
		Origin:         origin,
	}
}

// personaMap resolves a persona name to the runner's credential map (never logged).
func (o *Orchestrator) personaMap(name string) map[string]string {
	if name == "" {
		return nil
	}
	p, ok := o.cfg.Personas[name]
	if !ok {
		return nil
	}
	m := map[string]string{"username": p.Username, "password": p.Password}
	for k, v := range p.Extra {
		m[k] = v
	}
	return m
}

// runDir returns the evidence dir for one run attempt (falls back to a temp dir
// under cfg.Evidence.Dir while the evidence package is unimplemented).
func (o *Orchestrator) runDir(scenarioID string, at time.Time, attempt int) string {
	if o.ev != nil {
		if d, err := o.ev.RunDir(scenarioID, at, attempt); err == nil && d != "" {
			return d
		}
	}
	root := filepath.Join(o.cfg.Abs(o.cfg.Evidence.Dir), scenarioID)
	_ = os.MkdirAll(root, 0o755)
	d, err := os.MkdirTemp(root, at.Format("20060102-150405")+"-a"+fmt.Sprint(attempt)+"-")
	if err != nil {
		return root
	}
	return d
}

func (o *Orchestrator) agentDir(featureID string, at time.Time) string {
	if o.ev != nil {
		if d, err := o.ev.AgentDir(featureID, at); err == nil && d != "" {
			return d
		}
	}
	root := filepath.Join(o.cfg.Abs(o.cfg.Evidence.Dir), "agent", safeName(featureID))
	_ = os.MkdirAll(root, 0o755)
	d, err := os.MkdirTemp(root, at.Format("20060102-150405")+"-")
	if err != nil {
		return root
	}
	return d
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "task"
	}
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// stepKey is a locator-insensitive hash of one step's business meaning; mechanics
// (waits, hover, screenshots, flows) are skipped. Flows are expanded first.
func stepKeys(sc *dsl.Scenario, flows map[string]*dsl.Flow) []string {
	var keys []string
	var walk func(steps []dsl.Step, depth int)
	walk = func(steps []dsl.Step, depth int) {
		for _, st := range steps {
			if st.UseFlow != "" {
				if f, ok := flows[st.UseFlow]; ok && depth < 4 {
					walk(f.Steps, depth+1)
				} else {
					keys = append(keys, "flow:"+strings.ToLower(st.UseFlow))
				}
				continue
			}
			switch st.Kind() {
			case "", "wait_for", "wait_ms", "wait_url", "hover", "screenshot":
				continue
			}
			one := &dsl.Scenario{Steps: []dsl.Step{st}}
			keys = append(keys, one.Fingerprint(nil))
		}
	}
	for _, u := range sc.Uses {
		if f, ok := flows[u]; ok {
			walk(f.Steps, 1)
		} else {
			keys = append(keys, "flow:"+strings.ToLower(u))
		}
	}
	walk(sc.Steps, 0)
	return keys
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	sa, sb := map[string]bool{}, map[string]bool{}
	for _, x := range a {
		sa[x] = true
	}
	for _, x := range b {
		sb[x] = true
	}
	inter := 0
	for x := range sa {
		if sb[x] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

func sortedLower(v []string) []string {
	out := make([]string, 0, len(v))
	for _, s := range v {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
