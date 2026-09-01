package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vigil/internal/agent"
	"vigil/internal/classify"
	"vigil/internal/dsl"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// ErrAgentUnavailable is returned when an agent job is handled without an adapter.
var ErrAgentUnavailable = errors.New("orchestrator: Browser Agent unavailable")

const maxGitEvidence = 12 * 1024

// GateOutcome records what happened to one agent-proposed script (audit trail;
// written to <agent dir>/gate.json and scheduler_state gate:<scenario>).
type GateOutcome struct {
	ScenarioID  string              `json:"scenario_id"`
	Candidate   int                 `json:"candidate,omitempty"`
	State       model.ScenarioState `json:"state"` // DUPLICATE | NEEDS_REVIEW | EPHEMERAL | SOAK | CANDIDATE | "" (invalid)
	Reason      string              `json:"reason"`
	DuplicateOf string              `json:"duplicate_of,omitempty"`
	Version     int                 `json:"version,omitempty"`
	At          time.Time           `json:"at"`
}

// HandleAgentJob executes an agent job end-to-end: build Request, run adapter,
// gate candidates (oracle/dedup/validate), persist CANDIDATE→SOAK or NEEDS_REVIEW/EPHEMERAL/DUPLICATE.
//
// When the model is unavailable the job is requeued with backoff (store.RequeueJob)
// and nil is returned: the caller must not CompleteJob a job that is READY again.
func (o *Orchestrator) HandleAgentJob(ctx context.Context, job *model.Job) error {
	if job == nil {
		return errors.New("orchestrator: nil job")
	}
	if o.agent == nil {
		return ErrAgentUnavailable
	}
	p := parsePayload(job.Payload)
	if p.FeatureID == "" {
		p.FeatureID = job.FeatureID
	}
	if p.ScenarioID == "" {
		p.ScenarioID = job.ScenarioID
	}
	var task string
	switch job.Kind {
	case model.JobAgentDiscover:
		task = agent.TaskDiscover
	case model.JobAgentVerify:
		task = agent.TaskVerifyChange
	case model.JobAgentRepair:
		task = agent.TaskRepair
	default:
		return fmt.Errorf("orchestrator: job %d kind %s is not an agent job", job.ID, job.Kind)
	}

	var feat *model.Feature
	if p.FeatureID != "" {
		if f, err := o.st.GetFeature(ctx, o.cfg.Project.ID, p.FeatureID); err == nil {
			feat = f
		}
	}
	req, err := o.buildRequest(ctx, task, job, p, feat)
	if err != nil {
		return err
	}
	dir := o.agentDir(firstNonEmpty(p.FeatureID, p.ScenarioID, "task"), o.now())
	if err := o.st.RecordBudget(ctx, o.cfg.Project.ID, "agent", 1); err != nil {
		return err
	}
	res, err := o.agent.Run(ctx, req, dir)
	if err != nil {
		return fmt.Errorf("agent job %d: %w", job.ID, err)
	}
	if res.ModelUnavailable {
		at := o.now().Add(o.cfg.Agent.Backoff.Duration)
		requeued, err := o.st.RequeueJob(ctx, job.ID, at, "model unavailable: "+res.Reason)
		if err != nil {
			return err
		}
		o.logger.Printf("agent job %d: model unavailable (%s); requeued=%v at %s (deterministic QA continues)", job.ID, res.Reason, requeued, at.Format(time.RFC3339))
		return nil
	}

	var gates []GateOutcome
	switch res.Decision {
	case agent.DecisionNewScript:
		gates = o.gateCandidates(ctx, job, feat, p, res, dir)
	case agent.DecisionPatchScript:
		gates = append(gates, o.gateRepair(ctx, job, p, res, dir))
	case agent.DecisionAppFailure:
		gates = append(gates, o.agentAppFailure(ctx, job, feat, p, res))
	case agent.DecisionNoNewCoverage:
		o.logger.Printf("agent job %d: NO_NEW_COVERAGE (%s)", job.ID, firstLine(res.CoverageDelta))
	default: // NEEDS_REVIEW, ORACLE_UNKNOWN
		gates = o.persistForReview(ctx, job, feat, p, res, "agent decision "+res.Decision+": "+firstNonEmpty(res.Reason, firstLine(res.Evidence)))
		_ = o.st.SetState(ctx, stateReviewPrefix+p.FeatureID+":"+p.ShippedSHA, res.Decision+": "+firstNonEmpty(res.Reason, firstLine(res.Evidence)))
	}
	if len(res.HostViolations) > 0 {
		o.logger.Printf("agent job %d: host allowlist violated: %v", job.ID, res.HostViolations)
	}
	o.writeGateLog(ctx, dir, gates)
	return nil
}

// buildRequest assembles the bounded PRD §11 request.
func (o *Orchestrator) buildRequest(ctx context.Context, task string, job *model.Job, p jobPayload, feat *model.Feature) (agent.Request, error) {
	req := agent.Request{
		Task:         task,
		ProjectID:    o.cfg.Project.ID,
		FeatureID:    p.FeatureID,
		ShippedSHA:   p.ShippedSHA,
		Target:       o.cfg.Target.BaseURL,
		EntryPath:    p.EntryPath,
		AllowedHosts: o.cfg.Target.AllowedHosts,
		Allowed:      agent.AllowedActions,
		Forbidden:    agent.ForbiddenActions,
		MaxScenarios: o.cfg.Agent.MaxScenariosPerTask,
	}
	var evidenceParts []string
	if feat != nil {
		req.Summary = feat.Summary
		req.ChangedPaths = feat.ChangedPaths
		req.Routes = feat.Routes
		if req.ShippedSHA == "" {
			req.ShippedSHA = feat.LatestShippedSHA
		}
		if req.EntryPath == "" && len(feat.Routes) > 0 {
			req.EntryPath = feat.Routes[0]
		}
		if feat.Summary != "" {
			evidenceParts = append(evidenceParts, "Feature summary: "+feat.Summary)
		}
		ids, err := o.ImpactedScenarios(ctx, feat)
		if err != nil {
			return req, err
		}
		for _, id := range ids {
			m, v, err := o.st.GetCurrentScenarioVersion(ctx, o.cfg.Project.ID, id)
			if err != nil {
				continue
			}
			req.KnownScripts = append(req.KnownScripts, fmt.Sprintf("%s/v%d", m.ID, m.CurrentVersion))
			if len(req.ExistingScenarios) < o.cfg.Agent.MaxScenariosPerTask {
				req.ExistingScenarios = append(req.ExistingScenarios, v.YAML)
			}
		}
		if g := gitEvidence(o.cfg.Project.Repo, req.ShippedSHA, feat.ChangedPaths); g != "" {
			evidenceParts = append(evidenceParts, g)
		}
	}
	if task == agent.TaskRepair && p.ScenarioID != "" {
		m, v, err := o.st.GetCurrentScenarioVersion(ctx, o.cfg.Project.ID, p.ScenarioID)
		if err != nil {
			return req, fmt.Errorf("repair job %d: %w", job.ID, err)
		}
		req.FailingScript = v.YAML
		req.KnownScripts = append(req.KnownScripts, fmt.Sprintf("%s/v%d", m.ID, m.CurrentVersion))
		req.FailureDetail = o.failureDetail(ctx, p.ScenarioID, p.RunID)
		if req.FeatureID == "" {
			req.FeatureID = m.OracleFeature
		}
		if req.ShippedSHA == "" {
			req.ShippedSHA = m.OracleSHA
		}
	}
	if p.Request != nil {
		mr := p.Request
		req.Instructions, req.EntryURL, req.Accounts, req.Mutation, req.Locks = mr.Instructions, mr.EntryURL, mr.Accounts, mr.Mutation, mr.Locks
		req.MaxToolCalls, req.TimeoutMinutes, req.MaxContinuations = mr.MaxToolCalls, mr.TimeoutMinutes, mr.MaxContinuations
		if len(mr.Routes) > 0 {
			req.Routes = mr.Routes
		}
		if mr.Summary != "" {
			req.Summary = mr.Summary
		}
		if mr.Mutation != "" && mr.Mutation != "read-only" {
			req.Allowed = append(append([]string{}, req.Allowed...), "mutate_data")
			var fb []string
			for _, f := range req.Forbidden {
				if f != "mutate_data" {
					fb = append(fb, f)
				}
			}
			req.Forbidden = fb
		}
		if mr.Instructions != "" {
			evidenceParts = append(evidenceParts, "Manual QA request:\n"+mr.Instructions)
		}
	}
	req.Evidence = strings.Join(evidenceParts, "\n\n")
	for name := range o.cfg.Personas {
		req.Personas = append(req.Personas, name)
	}
	sort.Strings(req.Personas)
	return req, nil
}

func (o *Orchestrator) failureDetail(ctx context.Context, scenarioID string, runID int64) string {
	runs, err := o.st.ListRuns(ctx, o.cfg.Project.ID, scenarioID, 10)
	if err != nil {
		return ""
	}
	var pick *model.Run
	for _, r := range runs {
		if runID != 0 && r.ID == runID {
			pick = r
			break
		}
		if pick == nil && r.Outcome != model.OutcomePass {
			pick = r
		}
	}
	if pick == nil {
		return ""
	}
	return fmt.Sprintf("run %d on %s: outcome %s; failed step %d (%s); expected %q; actual %q; error: %s; evidence: %s",
		pick.ID, pick.Browser, pick.Outcome, pick.FailedStep, pick.FailedAction, pick.Expected, pick.Actual, pick.Error, pick.EvidenceDir)
}

// gitEvidence returns `git show --stat` plus diffs of changed paths (best-effort, ≤12KB).
func gitEvidence(repo, sha string, paths []string) string {
	if repo == "" || sha == "" {
		return ""
	}
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var buf bytes.Buffer
	run := func(args ...string) {
		if buf.Len() >= maxGitEvidence {
			return
		}
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.Output()
		if err != nil || len(out) == 0 {
			return
		}
		buf.Write(out)
		buf.WriteString("\n")
	}
	run("show", "--stat", "--format=medium", sha)
	for _, p := range paths {
		run("show", "--format=", "--unified=3", sha, "--", p)
	}
	s := buf.String()
	if len(s) > maxGitEvidence {
		s = s[:maxGitEvidence] + "\n…[truncated]"
	}
	return strings.TrimSpace(s)
}

// ---- candidate gate (PRD §9, AC-05, AC-10, AC-24) -----------------------------

func (o *Orchestrator) gateCandidates(ctx context.Context, job *model.Job, feat *model.Feature, p jobPayload, res *agent.Result, dir string) []GateOutcome {
	var out []GateOutcome
	limit := o.cfg.Agent.MaxScenariosPerTask
	for i, y := range res.ScriptCandidates {
		if limit > 0 && i >= limit {
			o.logger.Printf("agent job %d: candidate %d ignored (max_scenarios_per_task=%d)", job.ID, i+1, limit)
			break
		}
		g := o.gateCandidate(ctx, job, feat, p, res, y)
		g.Candidate = i + 1
		out = append(out, g)
	}
	if len(res.ScriptCandidates) == 0 {
		o.logger.Printf("agent job %d: NEW_SCRIPT without script_candidates", job.ID)
	}
	return out
}

func (o *Orchestrator) gateCandidate(ctx context.Context, job *model.Job, feat *model.Feature, p jobPayload, res *agent.Result, yamlText string) GateOutcome {
	g := GateOutcome{At: o.now()}
	sc, err := dsl.Parse([]byte(yamlText))
	if err != nil {
		g.Reason = "invalid DSL: " + err.Error()
		return g
	}
	g.ScenarioID = sc.Scenario.ID
	o.fillProvenance(sc, feat, p)
	flows, flowIDs, err := o.loadFlows(ctx)
	if err != nil {
		g.Reason = err.Error()
		return g
	}
	if err := sc.Validate(flowIDs); err != nil {
		g.Reason = "invalid: " + err.Error()
		if _, perr := o.persist(ctx, sc, model.StateNeedsReview, "agent", "invalid candidate from agent job "+fmt.Sprint(job.ID)+": "+err.Error(), yamlText, flows); perr != nil {
			g.Reason += " (not persisted: " + perr.Error() + ")"
		} else {
			g.State = model.StateNeedsReview
		}
		return o.recordGate(ctx, g)
	}
	fp := sc.Fingerprint(flows)
	if ex, err := o.st.FindScenarioByFingerprint(ctx, o.cfg.Project.ID, fp); err == nil && ex != nil {
		g.State, g.DuplicateOf, g.Reason = model.StateDuplicate, ex.ID, "exact fingerprint matches "+ex.ID
		o.logger.Printf("agent job %d: candidate %s DUPLICATE of %s (fingerprint)", job.ID, sc.Scenario.ID, ex.ID)
		return o.recordGate(ctx, g)
	}
	if dup, sim := o.structuralDuplicate(ctx, sc, flows); dup != "" {
		g.State, g.DuplicateOf, g.Reason = model.StateDuplicate, dup, fmt.Sprintf("structural similarity %.2f ≥ %.2f with %s", sim, o.cfg.Policy.StructuralDupThreshold, dup)
		o.logger.Printf("agent job %d: candidate %s DUPLICATE of %s (%s)", job.ID, sc.Scenario.ID, dup, g.Reason)
		return o.recordGate(ctx, g)
	}
	reason := fmt.Sprintf("discovered by agent job %d (%s): %s", job.ID, job.Kind, firstLine(res.CoverageDelta))
	switch {
	case sc.Oracle.Source == "observation" && o.cfg.Policy.ObservationOracle == "needs_review":
		g.State, g.Reason = model.StateNeedsReview, "oracle.source=observation: observation alone is not an oracle (PRD §5.1)"
	case res.Ephemeral:
		g.State, g.Reason = model.StateEphemeral, "agent marked behavior ephemeral; persisted for audit, never scheduled"
	default:
		g.State, g.Reason = model.StateCandidate, "candidate persisted; validating independently"
	}
	id, err := o.persist(ctx, sc, g.State, "agent", reason, "", flows)
	if err != nil {
		g.State, g.Reason = "", "not persisted: "+err.Error()
		return o.recordGate(ctx, g)
	}
	g.ScenarioID, g.Version = id, sc.Scenario.Version
	if g.State != model.StateCandidate {
		return o.recordGate(ctx, g)
	}
	// Independent validation through the deterministic runner (AC-05).
	outcome, detail, err := o.validateByRun(ctx, job, id, sc, flows, p)
	if err != nil {
		g.State, g.Reason = model.StateNeedsReview, "validation not possible: "+err.Error()
		_ = o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateNeedsReview)
		return o.recordGate(ctx, g)
	}
	if outcome == model.OutcomePass {
		if err := o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateSoak); err == nil {
			_ = o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, id, o.now())
			g.State, g.Reason = model.StateSoak, fmt.Sprintf("validated by runner; SOAK until %d clean passes", o.cfg.Policy.SoakPasses)
		} else {
			g.Reason = err.Error()
		}
	} else {
		_ = o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateNeedsReview)
		g.State, g.Reason = model.StateNeedsReview, fmt.Sprintf("validation run %s: %s", outcome, detail)
	}
	o.logger.Printf("agent job %d: candidate %s → %s (%s)", job.ID, id, g.State, g.Reason)
	return o.recordGate(ctx, g)
}

// fillProvenance completes missing feature/sha metadata (never the oracle source).
func (o *Orchestrator) fillProvenance(sc *dsl.Scenario, feat *model.Feature, p jobPayload) {
	fid, sha := p.FeatureID, p.ShippedSHA
	if feat != nil {
		if fid == "" {
			fid = feat.ID
		}
		if sha == "" {
			sha = feat.LatestShippedSHA
		}
	}
	if sc.Covers.Feature == "" {
		sc.Covers.Feature = fid
	}
	if sc.Oracle.SourceFeature == "" {
		sc.Oracle.SourceFeature = fid
	}
	if sc.Oracle.SourceSHA == "" {
		sc.Oracle.SourceSHA = sha
	}
	if sc.Scenario.Version < 1 {
		sc.Scenario.Version = 1
	}
}

// persist stores a new scenario row (+ first version + links). The id is
// suffixed when it already exists. rawYAML, when set, is stored verbatim.
func (o *Orchestrator) persist(ctx context.Context, sc *dsl.Scenario, state model.ScenarioState, origin, reason, rawYAML string, flows map[string]*dsl.Flow) (string, error) {
	fp := sc.Fingerprint(flows)
	id := sc.Scenario.ID
	if id == "" {
		return "", errors.New("scenario.id missing")
	}
	if _, err := o.st.GetScenario(ctx, o.cfg.Project.ID, id); err == nil {
		id = fmt.Sprintf("%s-%s", id, fp[:6])
		sc.Scenario.ID = id
	}
	y := rawYAML
	if y == "" {
		b, err := yaml.Marshal(sc)
		if err != nil {
			return "", err
		}
		y = string(b)
	}
	m := o.scenarioModel(sc, id, state, fp, origin)
	if state == model.StateSoak || state == model.StateCandidate {
		t := o.now()
		m.NextDueAt = &t
	}
	v := &model.ScenarioVersion{ScenarioID: id, Version: sc.Scenario.Version, YAML: y, Fingerprint: fp, CreatedBy: origin, Reason: reason}
	if err := o.st.CreateScenario(ctx, m, v, linksFor(sc)); err != nil {
		return "", err
	}
	return id, nil
}

// structuralDuplicate finds an existing scenario with equal capability+routes whose
// major-action set is Jaccard-similar above cfg.Policy.StructuralDupThreshold.
func (o *Orchestrator) structuralDuplicate(ctx context.Context, sc *dsl.Scenario, flows map[string]*dsl.Flow) (string, float64) {
	cap := strings.ToLower(strings.TrimSpace(sc.Covers.Capability))
	if cap == "" {
		return "", 0
	}
	states := []model.ScenarioState{model.StateActive, model.StateSoak, model.StateCandidate}
	ids, err := o.st.ScenariosLinkedTo(ctx, o.cfg.Project.ID, model.LinkCapability, []string{cap}, states)
	if err != nil {
		return "", 0
	}
	mine := stepKeys(sc, flows)
	routes := sortedLower(sc.Covers.Routes)
	best, bestSim := "", 0.0
	for _, id := range ids {
		_, v, err := o.st.GetCurrentScenarioVersion(ctx, o.cfg.Project.ID, id)
		if err != nil {
			continue
		}
		other, err := dsl.Parse([]byte(v.YAML))
		if err != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(other.Covers.Capability)) != cap || !equalStrings(routes, sortedLower(other.Covers.Routes)) {
			continue
		}
		if sim := jaccard(mine, stepKeys(other, flows)); sim >= o.cfg.Policy.StructuralDupThreshold && sim > bestSim {
			best, bestSim = id, sim
		}
	}
	return best, bestSim
}

// validateByRun executes the scenario once through the deterministic runner and
// records the run. Returns the classified outcome and a short detail.
func (o *Orchestrator) validateByRun(ctx context.Context, job *model.Job, id string, sc *dsl.Scenario, flows map[string]*dsl.Flow, p jobPayload) (model.Outcome, string, error) {
	if o.run == nil {
		return "", "", errors.New("runner unavailable")
	}
	browser := model.Browser(sc.Browser.Primary)
	if browser == "" {
		browser = model.Browser(o.cfg.Browser.Primary)
	}
	if sc.Browser.RequiresChromium {
		browser = model.BrowserChromium
	}
	owner := fmt.Sprintf("validate:%s:%d", id, job.ID)
	if len(sc.Resources.Locks) > 0 {
		ok, err := o.st.TryAcquireLocks(ctx, sc.Resources.Locks, owner, 2*o.cfg.Browser.RunTimeout.Duration+time.Minute)
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("resource locks busy: %v", sc.Resources.Locks)
		}
		defer func() { _ = o.st.ReleaseLocks(context.Background(), sc.Resources.Locks, owner) }()
	}
	start := o.now()
	dir := o.runDir(id, start, 1)
	spec := runner.Spec{
		ProjectID:        o.cfg.Project.ID,
		Scenario:         sc,
		Flows:            flows,
		Browser:          browser,
		BaseURL:          o.cfg.Target.BaseURL,
		Persona:          o.personaMap(sc.Preconditions.Persona),
		EvidenceDir:      dir,
		StepTimeout:      o.cfg.Browser.StepTimeout.Duration,
		RunTimeout:       o.cfg.Browser.RunTimeout.Duration,
		CaptureDOMOnFail: true,
	}
	res, err := o.run.Run(ctx, spec)
	if err != nil {
		return "", "", err
	}
	outcome, why := classify.Classify(classify.Input{Result: res, Browser: browser, Attempt: 1, TargetHealthy: true})
	run := &model.Run{
		JobID:           job.ID,
		ProjectID:       o.cfg.Project.ID,
		ScenarioID:      id,
		ScenarioVersion: sc.Scenario.Version,
		FeatureID:       p.FeatureID,
		ShippedSHA:      p.ShippedSHA,
		Browser:         browser,
		Outcome:         outcome,
		Attempt:         1,
		StartedAt:       start,
		FinishedAt:      o.now(),
		Error:           res.Error,
		EvidenceDir:     dir,
	}
	if !res.StartedAt.IsZero() {
		run.StartedAt, run.FinishedAt = res.StartedAt, res.FinishedAt
	}
	run.DurationMs = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	detail := why
	if fs := res.FailedStep; fs != nil {
		run.FailedStep, run.FailedAction, run.Expected, run.Actual = fs.Index, fs.Kind, fs.Expected, fs.Actual
		detail = fmt.Sprintf("step %d %s expected %q actual %q %s", fs.Index, fs.Kind, fs.Expected, fs.Actual, fs.Error)
	}
	if len(res.GlobalAssertionFailures) > 0 {
		detail = strings.TrimSpace(detail + " " + strings.Join(res.GlobalAssertionFailures, "; "))
	}
	if _, err := o.st.InsertRun(ctx, run); err != nil {
		return outcome, detail, err
	}
	// Counters only (state is CANDIDATE here, so soak_passes is untouched).
	_, _ = o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, id, outcome, run.FinishedAt)
	return outcome, firstNonEmpty(detail, res.Error), nil
}

// ---- repair gate (AC-09) --------------------------------------------------

func (o *Orchestrator) gateRepair(ctx context.Context, job *model.Job, p jobPayload, res *agent.Result, dir string) GateOutcome {
	g := GateOutcome{ScenarioID: p.ScenarioID, At: o.now()}
	if p.ScenarioID == "" {
		g.Reason = "PATCH_SCRIPT without a target scenario"
		return g
	}
	m, ver, err := o.st.GetCurrentScenarioVersion(ctx, o.cfg.Project.ID, p.ScenarioID)
	if err != nil {
		g.Reason = err.Error()
		return g
	}
	review := func(reason string) GateOutcome {
		_ = o.st.SetScenarioState(ctx, o.cfg.Project.ID, m.ID, model.StateNeedsReview)
		g.State, g.Reason = model.StateNeedsReview, reason
		o.logger.Printf("scenario %s: repair rejected → NEEDS_REVIEW (%s)", m.ID, reason)
		return o.recordGate(ctx, g)
	}
	if strings.TrimSpace(res.ScriptPatch) == "" {
		return review("PATCH_SCRIPT without script_patch")
	}
	oldSc, err := dsl.Parse([]byte(ver.YAML))
	if err != nil {
		return review("current version does not parse: " + err.Error())
	}
	newSc, err := dsl.Parse([]byte(res.ScriptPatch))
	if err != nil {
		return review("patch does not parse: " + err.Error())
	}
	flows, flowIDs, err := o.loadFlows(ctx)
	if err != nil {
		return review(err.Error())
	}
	if why := oracleDiff(oldSc, newSc, flows); why != "" {
		return review("repair changed the business oracle (AC-09): " + why)
	}
	newSc.Scenario.ID = m.ID
	newSc.Scenario.Version = ver.Version + 1
	if err := newSc.Validate(flowIDs); err != nil {
		return review("patch invalid: " + err.Error())
	}
	outcome, detail, err := o.validateByRun(ctx, job, m.ID, newSc, flows, p)
	if err != nil {
		return review("patch validation not possible: " + err.Error())
	}
	if outcome != model.OutcomePass {
		return review(fmt.Sprintf("patch validation run %s: %s", outcome, detail))
	}
	out, err := yaml.Marshal(newSc)
	if err != nil {
		return review(err.Error())
	}
	nv := &model.ScenarioVersion{
		ScenarioID:  m.ID,
		Version:     newSc.Scenario.Version,
		YAML:        string(out),
		Fingerprint: newSc.Fingerprint(flows),
		CreatedBy:   "repair",
		Reason:      fmt.Sprintf("agent repair job %d: %s", job.ID, firstLine(firstNonEmpty(res.CoverageDelta, res.Evidence))),
	}
	if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, linksFor(newSc)); err != nil {
		return review("new version not stored: " + err.Error())
	}
	_ = o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, m.ID, o.now())
	g.State, g.Version, g.Reason = m.State, nv.Version, fmt.Sprintf("locator/navigation repair validated; v%d → v%d, state %s unchanged", ver.Version, nv.Version, m.State)
	o.logger.Printf("scenario %s: %s", m.ID, g.Reason)
	return o.recordGate(ctx, g)
}

// oracleDiff returns "" when both scenarios carry the same business oracle:
// the ordered assert_* steps (locator-insensitive, flows expanded), the
// scenario-level assert block and the oracle provenance.
func oracleDiff(a, b *dsl.Scenario, flows map[string]*dsl.Flow) string {
	if !equalStrings(assertionKeys(a, flows), assertionKeys(b, flows)) {
		return "assertion steps differ"
	}
	if !reflect.DeepEqual(normAssert(a), normAssert(b)) {
		return "scenario-level assert block differs"
	}
	if a.Oracle.Source != b.Oracle.Source || a.Oracle.SourceFeature != b.Oracle.SourceFeature || a.Oracle.SourceSHA != b.Oracle.SourceSHA {
		return "oracle provenance differs"
	}
	return ""
}

func assertionKeys(sc *dsl.Scenario, flows map[string]*dsl.Flow) []string {
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
			if st.IsAssertion() || (st.Eval != nil && st.Eval.Expect != "") {
				one := &dsl.Scenario{Steps: []dsl.Step{st}}
				keys = append(keys, one.Fingerprint(nil))
			}
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

type assertBlock struct {
	NoConsole bool
	No5xx     bool
	No4xxOn   []string
}

func normAssert(sc *dsl.Scenario) assertBlock {
	return assertBlock{NoConsole: sc.Assert.NoUncaughtConsoleError, No5xx: sc.Assert.NoHTTP5xx, No4xxOn: sortedLower(sc.Assert.NoHTTP4xxOn)}
}

// ---- APP_FAILURE from the agent ---------------------------------------------

// agentAppFailure opens an incident only when a linked scenario with run evidence
// exists; otherwise the finding is parked for human review.
func (o *Orchestrator) agentAppFailure(ctx context.Context, job *model.Job, feat *model.Feature, p jobPayload, res *agent.Result) GateOutcome {
	g := GateOutcome{At: o.now(), ScenarioID: p.ScenarioID}
	var ids []string
	if p.ScenarioID != "" {
		ids = append(ids, p.ScenarioID)
	} else if feat != nil {
		ids, _ = o.ImpactedScenarios(ctx, feat)
	}
	for _, id := range ids {
		runs, err := o.st.ListRuns(ctx, o.cfg.Project.ID, id, 10)
		if err != nil {
			continue
		}
		for _, r := range runs {
			if r.Outcome == model.OutcomePass || r.Outcome.IsInfrastructure() {
				continue
			}
			sc, err := o.st.GetScenario(ctx, o.cfg.Project.ID, id)
			if err != nil {
				break
			}
			in, err := o.openAppIncident(ctx, sc, r, nil, "Browser Agent confirmed APP_FAILURE: "+firstLine(res.Evidence))
			if err != nil {
				g.Reason = err.Error()
				return g
			}
			_ = o.st.BumpRegressionsCaught(ctx, sc.ID)
			g.ScenarioID, g.Reason = sc.ID, fmt.Sprintf("incident #%d (run %d)", in.ID, r.ID)
			return g
		}
	}
	key := stateReviewPrefix + p.FeatureID + ":" + p.ShippedSHA
	_ = o.st.SetState(ctx, key, "APP_FAILURE reported by agent without a linked scenario run: "+firstLine(res.Evidence))
	g.State, g.Reason = model.StateNeedsReview, "agent APP_FAILURE without linked scenario evidence → human review ("+key+")"
	o.logger.Printf("agent job %d: %s", job.ID, g.Reason)
	return g
}

// persistForReview stores any proposed candidates as NEEDS_REVIEW (audit only).
func (o *Orchestrator) persistForReview(ctx context.Context, job *model.Job, feat *model.Feature, p jobPayload, res *agent.Result, reason string) []GateOutcome {
	var out []GateOutcome
	flows, _, _ := o.loadFlows(ctx)
	for i, y := range res.ScriptCandidates {
		g := GateOutcome{At: o.now(), Candidate: i + 1}
		sc, err := dsl.Parse([]byte(y))
		if err != nil {
			g.Reason = "invalid DSL: " + err.Error()
			out = append(out, g)
			continue
		}
		o.fillProvenance(sc, feat, p)
		id, err := o.persist(ctx, sc, model.StateNeedsReview, "agent", fmt.Sprintf("agent job %d: %s", job.ID, reason), y, flows)
		if err != nil {
			g.Reason = err.Error()
		} else {
			g.ScenarioID, g.State, g.Reason = id, model.StateNeedsReview, reason
		}
		out = append(out, o.recordGate(ctx, g))
	}
	return out
}

func (o *Orchestrator) recordGate(ctx context.Context, g GateOutcome) GateOutcome {
	if g.ScenarioID != "" {
		_ = o.setJSONState(ctx, stateGatePrefix+g.ScenarioID, g)
	}
	return g
}

func (o *Orchestrator) writeGateLog(ctx context.Context, dir string, gates []GateOutcome) {
	if dir == "" || len(gates) == 0 {
		return
	}
	b, err := json.MarshalIndent(gates, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "gate.json"), b, 0o644)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var _ = store.ErrNotFound
