package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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

// ReasonObservationOracle marks a scenario parked only because policy said an
// observed behaviour needs a human. The reconciler matches on it when the
// policy is later relaxed, so it must stay stable.
const ReasonObservationOracle = "oracle.source=observation: observation alone is not an oracle (PRD §5.1)"

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
// When the model is unavailable the job is requeued with backoff and
// ErrAgentJobRequeued is returned so the caller preserves READY.
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
	case model.JobAgentReproduce:
		task = agent.TaskReproduce
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
	_ = o.st.SetState(ctx, stateAgentDirPrefix+strconv.FormatInt(job.ID, 10), dir)
	res, err := o.agent.Run(ctx, req, dir)
	if err != nil {
		return fmt.Errorf("agent job %d: %w", job.ID, err)
	}
	// Every place a browser actually reached is a candidate area to test later.
	// This is what lets an operator hand over only a base URL.
	if res != nil {
		o.RecordSiteRoutes(ctx, res.VisitedURLs)
	}
	if res.ModelUnavailable {
		at := o.now().Add(o.cfg.Agent.Backoff.Duration)
		requeued, err := o.st.RequeueJob(ctx, job.ID, at, "model unavailable: "+res.Reason)
		if err != nil {
			return err
		}
		o.logger.Printf("agent job %d: model unavailable (%s); requeued=%v at %s (deterministic QA continues)", job.ID, res.Reason, requeued, at.Format(time.RFC3339))
		return ErrAgentJobRequeued
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
	o.persistFindings(ctx, job, p, res, gates)
	o.writeGateLog(ctx, dir, gates)
	return nil
}

// AgentDirForJob returns the evidence directory the given agent job wrote to
// (empty when the job never ran here). gate.json lives in it.
func (o *Orchestrator) AgentDirForJob(ctx context.Context, jobID int64) string {
	dir, err := o.st.GetState(ctx, stateAgentDirPrefix+strconv.FormatInt(jobID, 10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(dir)
}

// persistFindings stores the agent's data-analyst findings for every task kind.
// The scenario is the job's own (repair) or the first script the gate persisted
// (new/reproduce); findings without a scenario still hang off the feature.
func (o *Orchestrator) persistFindings(ctx context.Context, job *model.Job, p jobPayload, res *agent.Result, gates []GateOutcome) {
	if res == nil || len(res.Findings) == 0 {
		return
	}
	scenarioID := p.ScenarioID
	if scenarioID == "" {
		for _, g := range gates {
			if g.ScenarioID != "" && g.State != "" && g.State != model.StateDuplicate {
				scenarioID = g.ScenarioID
				break
			}
		}
	}
	for _, f := range agent.NormalizeFindings(res.Findings) {
		rec := &model.Finding{ProjectID: o.cfg.Project.ID, FeatureID: p.FeatureID, ScenarioID: scenarioID, JobID: job.ID,
			Kind: f.Kind, Where: f.Where, Expected: f.Expected, Actual: f.Actual, Evidence: f.Evidence}
		if _, err := o.st.InsertFinding(ctx, rec); err != nil {
			o.logger.Printf("agent job %d: finding not stored: %v", job.ID, err)
		}
	}
	o.logger.Printf("agent job %d: %d finding(s) stored (scenario=%s feature=%s)", job.ID, len(res.Findings), scenarioID, p.FeatureID)
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
		if task == agent.TaskReproduce {
			// The report itself is the oracle source: what the reporter saw and expected.
			report := fmt.Sprintf("Reported %s %s", firstNonEmpty(feat.Kind, "item"), firstNonEmpty(feat.Ref, feat.ID))
			if feat.Details != "" {
				report += ":\n" + feat.Details
			}
			evidenceParts = append(evidenceParts, report)
			req.MaxScenarios = 1
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
		if p.ValidationError != "" {
			req.FailureDetail = "DSL validation failed: " + p.ValidationError
		} else {
			req.FailureDetail = o.failureDetail(ctx, p.ScenarioID, p.RunID)
		}
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
	if o.cfg.Agent.DomainFile != "" {
		rules, err := agent.ReadDomainFile(o.cfg.Abs(o.cfg.Agent.DomainFile))
		if err != nil {
			o.logger.Printf("agent.domain_file %s unreadable, task runs without domain rules: %v", o.cfg.Agent.DomainFile, err)
		}
		req.DomainRules = rules
	}
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
	return fmt.Sprintf("run %d on %s: outcome %s; failed step %d (%s); expected %q; actual %q; error: %s; evidence: %s%s",
		pick.ID, pick.Browser, pick.Outcome, pick.FailedStep, pick.FailedAction, pick.Expected, pick.Actual, pick.Error, pick.EvidenceDir,
		stepTrace(pick.EvidenceDir, pick.FailedStep))
}

// stepTrace renders what every step of the failing run actually did, up to the one that
// failed. The failed step on its own is misleading: a click that lands on the wrong element
// succeeds, and the run only breaks a step or two later at a wait. Given just that wait, the
// model reasonably concludes the application changed, and repairs the wrong thing. The agent
// cannot open the evidence directory itself - it has browser tools only - so the trace has to
// travel in the request.
func stepTrace(evidenceDir string, failedStep int) string {
	if evidenceDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(evidenceDir, "steps.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Steps []struct {
			Index  int    `json:"index"`
			Kind   string `json:"kind"`
			OK     bool   `json:"ok"`
			Actual string `json:"actual"`
		} `json:"steps"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Steps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("; what the run actually did (check this before concluding the app changed):")
	for _, s := range doc.Steps {
		if failedStep > 0 && s.Index > failedStep {
			break
		}
		state := "ok"
		if !s.OK {
			state = "FAILED"
		}
		fmt.Fprintf(&b, " [%d %s %s: %s]", s.Index, s.Kind, state, clip(s.Actual, 160))
	}
	for _, n := range doc.Notes {
		b.WriteString(" note: " + clip(n, 200))
	}
	return b.String()
}

// clip shortens on rune boundaries so Korean evidence text is never cut mid-character.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
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
	reproduce := isReproduce(job, feat)
	if err := sc.Validate(flowIDs); err != nil {
		// An invalid script is usually a wrong locator kind or a missing field,
		// not a reason to spend a human's attention: keep the raw text as a
		// CANDIDATE and let the agent repair it a bounded number of times.
		id, perr := o.persist(ctx, sc, model.StateCandidate, "agent", "invalid candidate from agent job "+fmt.Sprint(job.ID)+": "+err.Error(), yamlText, flows)
		if perr != nil {
			g.Reason = "invalid: " + err.Error() + " (not persisted: " + perr.Error() + ")"
			return o.recordGate(ctx, g)
		}
		g.ScenarioID, g.Version = id, sc.Scenario.Version
		if reproduce {
			o.tagSource(ctx, id, feat)
		}
		p.ValidationError = err.Error()
		g = o.retryOrReview(ctx, g, id, job, p, "DSL validation failed: "+err.Error())
		o.logger.Printf("agent job %d: candidate %s → %s (%s)", job.ID, id, g.State, g.Reason)
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
		g.State, g.Reason = model.StateNeedsReview, ReasonObservationOracle
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
	if reproduce {
		o.tagSource(ctx, id, feat)
	}
	if g.State != model.StateCandidate {
		return o.recordGate(ctx, g)
	}
	// Independent validation through the deterministic runner (AC-05).
	outcome, detail, run, err := o.validateByRun(ctx, job, id, sc, flows, p)
	if err != nil {
		g.State, g.Reason = model.StateNeedsReview, "validation not possible: "+err.Error()
		_ = o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateNeedsReview)
		return o.recordGate(ctx, g)
	}
	g = o.settleValidation(ctx, g, id, job, p, res, feat, reproduce, outcome, detail, run)
	o.logger.Printf("agent job %d: candidate %s → %s (%s)", job.ID, id, g.State, g.Reason)
	return o.recordGate(ctx, g)
}

// settleValidation turns a validation run of a CANDIDATE into its next state:
// PENDING_APPROVAL (reproduce: PASS or APP_FAILURE), SOAK (ship: PASS), or the
// bounded repair loop. Shared by the candidate gate and the repair gate.
func (o *Orchestrator) settleValidation(ctx context.Context, g GateOutcome, id string, job *model.Job, p jobPayload, res *agent.Result, feat *model.Feature, reproduce bool, outcome model.Outcome, detail string, run *model.Run) GateOutcome {
	if reproduce && (outcome == model.OutcomePass || outcome == model.OutcomeAppFailure) {
		// Both answers are useful to a human: PASS = the expected behaviour holds
		// (not reproduced), APP_FAILURE = the flow reached the symptom. Neither
		// becomes coverage until someone approves it.
		o.clearFixAttempts(ctx, id)
		rec := reproductionFor(outcome, run, res, feat)
		if err := o.st.SetScenarioPendingApproval(ctx, o.cfg.Project.ID, id, rec.JSON()); err != nil {
			g.Reason = err.Error()
		} else {
			g.State, g.Reason = model.StatePendingApproval, fmt.Sprintf("validation run %s: verdict=%s reproduced=%v at_step=%d; waiting for approval", outcome, rec.Verdict, rec.Reproduced, rec.AtStep)
		}
	} else if outcome == model.OutcomePass {
		o.clearFixAttempts(ctx, id)
		if err := o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateSoak); err == nil {
			_ = o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, id, o.now())
			g.State, g.Reason = model.StateSoak, fmt.Sprintf("validated by runner; SOAK until %d clean passes", o.cfg.Policy.SoakPasses)
		} else {
			g.Reason = err.Error()
		}
	} else {
		p.ValidationError = "" // a run failure, not a DSL one: the repair reads the run
		g = o.retryOrReview(ctx, g, id, job, p, fmt.Sprintf("validation run %s: %s", outcome, detail))
	}
	return g
}

// retryOrReview enqueues another agent repair while policy.agent_fix_attempts
// allows it (state stays CANDIDATE), otherwise parks the scenario in NEEDS_REVIEW.
func (o *Orchestrator) retryOrReview(ctx context.Context, g GateOutcome, id string, job *model.Job, p jobPayload, detail string) GateOutcome {
	if n, limit, retried := o.retryFix(ctx, id, job, p, detail); retried {
		// A failed validation usually means the agent wrote a brittle locator,
		// not that a human is needed. Let it rewrite the script a bounded number
		// of times before spending someone's attention.
		g.State = model.StateCandidate
		g.Reason = fmt.Sprintf("%s; AGENT_REPAIR attempt %d/%d enqueued", detail, n, limit)
	} else {
		_ = o.st.SetScenarioState(ctx, o.cfg.Project.ID, id, model.StateNeedsReview)
		g.State = model.StateNeedsReview
		g.Reason = detail
		if limit > 1 {
			g.Reason += fmt.Sprintf(" (after %d agent fix attempt(s))", limit)
		}
	}
	return g
}

// fixAttemptsKey counts attempts in the current cycle; it governs the limit and
// is cleared once the scenario validates.
func fixAttemptsKey(scenarioID string) string { return "fix:" + scenarioID }

// fixSeqKey is a sequence that never resets. It exists only to keep the repair
// dedup key unique: reusing the cycle counter there means that after a reset the
// new attempt collides with an old job and the retry is silently dropped.
func fixSeqKey(scenarioID string) string { return "fixseq:" + scenarioID }

func (o *Orchestrator) stateInt(ctx context.Context, key string) int {
	raw, err := o.st.GetState(ctx, key)
	if err != nil || strings.TrimSpace(raw) == "" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

// retryFix enqueues another AGENT_REPAIR when the budget of attempts allows it.
// It reports the attempt number, the configured limit, and whether it retried.
func (o *Orchestrator) retryFix(ctx context.Context, scenarioID string, job *model.Job, p jobPayload, detail string) (int, int, bool) {
	limit := o.cfg.Policy.AgentFixAttempts
	if limit <= 1 || o.agent == nil {
		return 0, limit, false
	}
	used := o.stateInt(ctx, fixAttemptsKey(scenarioID))
	if used+1 >= limit {
		// This was the last allowed attempt; escalate.
		return used, limit, false
	}
	next := used + 1
	if err := o.st.SetState(ctx, fixAttemptsKey(scenarioID), strconv.Itoa(next)); err != nil {
		return used, limit, false
	}
	operator := p.Operator || (job != nil && job.Priority >= model.PriorityUserRequest)
	priority := model.PriorityRecentFailure
	if operator {
		// The repair of an operator-initiated task stays operator-initiated:
		// same queue precedence, same agent-budget bypass. The flag travels in
		// the payload because claiming a user request consumes priority 110.
		priority = model.PriorityUserRequest
	}
	j := &model.Job{
		ProjectID:  o.cfg.Project.ID,
		Kind:       model.JobAgentRepair,
		Priority:   priority,
		ScenarioID: scenarioID,
		FeatureID:  p.FeatureID,
		Payload:    jobPayload{ScenarioID: scenarioID, FeatureID: p.FeatureID, ShippedSHA: p.ShippedSHA, RunID: p.RunID, Trigger: "validation-retry", ValidationError: p.ValidationError, Operator: operator}.String(),
	}
	// The never-resetting sequence is in the dedup key: without it a second
	// repair of the same version is silently dropped, and after a counter reset
	// every future attempt collides with an old job and the loop stalls.
	seq := o.stateInt(ctx, fixSeqKey(scenarioID)) + 1
	_ = o.st.SetState(ctx, fixSeqKey(scenarioID), strconv.Itoa(seq))
	if _, created, err := o.st.EnqueueJob(ctx, j, fmt.Sprintf("fix:%s:%d", scenarioID, seq)); err != nil || !created {
		return used, limit, false
	}
	o.logger.Printf("scenario %s: validation failed (%s); agent fix attempt %d/%d", scenarioID, detail, next, limit)
	return next, limit, true
}

// clearFixAttempts resets the counter once a scenario validates, so a scenario
// that drifts again months later gets a fresh budget of attempts.
func (o *Orchestrator) clearFixAttempts(ctx context.Context, scenarioID string) {
	_ = o.st.SetState(ctx, fixAttemptsKey(scenarioID), "")
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
func (o *Orchestrator) validateByRun(ctx context.Context, job *model.Job, id string, sc *dsl.Scenario, flows map[string]*dsl.Flow, p jobPayload) (model.Outcome, string, *model.Run, error) {
	if o.run == nil {
		return "", "", nil, errors.New("runner unavailable")
	}
	browser := model.Browser(sc.Browser.Primary)
	if browser == "" {
		browser = model.Browser(o.cfg.Browser.Primary)
	}
	if sc.RequiresChromiumEngine() {
		browser = model.BrowserChromium
	}
	owner := fmt.Sprintf("validate:%s:%d", id, job.ID)
	if len(sc.Resources.Locks) > 0 {
		ok, err := o.st.TryAcquireLocks(ctx, sc.Resources.Locks, owner, 2*o.cfg.Browser.RunTimeout.Duration+time.Minute)
		if err != nil {
			return "", "", nil, err
		}
		if !ok {
			return "", "", nil, fmt.Errorf("resource locks busy: %v", sc.Resources.Locks)
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
		BaseURL:          o.cfg.DefaultEnv().BaseURL,
		Environment:      o.cfg.DefaultEnv().Name,
		AllowedHosts:     o.cfg.DefaultEnv().AllowedHosts,
		Persona:          o.personaMap(sc.Preconditions.Persona),
		EvidenceDir:      dir,
		StepTimeout:      o.cfg.Browser.StepTimeout.Duration,
		RunTimeout:       o.cfg.Browser.RunTimeout.Duration,
		CaptureDOMOnFail: true,
	}
	res, err := o.run.Run(ctx, spec)
	if err != nil {
		return "", "", nil, err
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
	runID, err := o.st.InsertRun(ctx, run)
	if err != nil {
		return outcome, detail, nil, err
	}
	run.ID = runID
	// Counters only (state is CANDIDATE here, so soak_passes is untouched).
	_, _ = o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, id, outcome, run.FinishedAt)
	return outcome, firstNonEmpty(detail, res.Error), run, nil
}

// ---- reproduce gate ----------------------------------------------------------

// ErrNotReproducible is returned when a ship feature is handed to `vigil reproduce`.
var ErrNotReproducible = errors.New("orchestrator: only issue/log features can be reproduced")

// EnqueueReproduce queues an AGENT_REPRODUCE job for a known issue/log feature
// (same dedup key as the loop, so a queued job is reused) and reports its id.
// It is operator-initiated (`vigil reproduce`), so it carries PriorityUserRequest:
// a person waiting at a terminal is not deferred by the hourly agent budget.
func (o *Orchestrator) EnqueueReproduce(ctx context.Context, f *model.Feature) (int64, bool, error) {
	if f == nil {
		return 0, false, errors.New("orchestrator: nil feature")
	}
	if model.IsShipKind(f.Kind) {
		return 0, false, fmt.Errorf("%w: %s is a %s feature", ErrNotReproducible, f.ID, firstNonEmpty(f.Kind, model.FeatureKindShip))
	}
	id, created, err := o.enqueueAgentJobAt(ctx, model.JobAgentReproduce, f.ID, f.LatestShippedSHA,
		jobPayload{FeatureID: f.ID, ShippedSHA: f.LatestShippedSHA, Trigger: string(model.ActionReproduce), Operator: true}, model.PriorityUserRequest)
	if err == nil && !created {
		// A job the loop already queued for this feature is reused; the person
		// asking for it now outranks the scan that queued it.
		_ = o.st.RaiseJobPriority(ctx, id, model.PriorityUserRequest)
	}
	return id, created, err
}

// ReproduceRequest builds the bounded request an AGENT_REPRODUCE job would send,
// without calling the model (dry runs).
func (o *Orchestrator) ReproduceRequest(ctx context.Context, f *model.Feature) (agent.Request, error) {
	if f == nil {
		return agent.Request{}, errors.New("orchestrator: nil feature")
	}
	if model.IsShipKind(f.Kind) {
		return agent.Request{}, fmt.Errorf("%w: %s is a %s feature", ErrNotReproducible, f.ID, firstNonEmpty(f.Kind, model.FeatureKindShip))
	}
	job := &model.Job{Kind: model.JobAgentReproduce, FeatureID: f.ID}
	return o.buildRequest(ctx, agent.TaskReproduce, job, jobPayload{FeatureID: f.ID, ShippedSHA: f.LatestShippedSHA, Trigger: string(model.ActionReproduce)}, f)
}

// isReproduce reports whether a candidate comes from a reproduce task (queue item).
func isReproduce(job *model.Job, feat *model.Feature) bool {
	if job != nil && job.Kind == model.JobAgentReproduce {
		return true
	}
	return feat != nil && !model.IsShipKind(feat.Kind)
}

// tagSource records which queue item (issue key / log signature) a scenario came from.
func (o *Orchestrator) tagSource(ctx context.Context, id string, feat *model.Feature) {
	if feat == nil || id == "" {
		return
	}
	if err := o.st.SetScenarioSource(ctx, o.cfg.Project.ID, id, firstNonEmpty(feat.Ref, feat.ID), firstNonEmpty(feat.Kind, model.FeatureKindShip)); err != nil {
		o.logger.Printf("scenario %s: source not recorded: %v", id, err)
	}
}

// reproductionRecord is the JSON stored in scenarios.reproduction for a human to read.
// Verdict is the corroborated answer; Reproduced stays in the JSON for older readers.
type reproductionRecord struct {
	Verdict     string `json:"verdict"`
	Reproduced  bool   `json:"reproduced"`
	AtStep      int    `json:"at_step"`
	ClaimedStep int    `json:"claimed_step,omitempty"`
	Symptom     string `json:"symptom"`
	Note        string `json:"note,omitempty"`
	Why         string `json:"why,omitempty"`
	RunID       int64  `json:"run_id"`
}

// Reproduction verdicts (H-1). All three still wait for a human decision.
const (
	verdictConfirmed   = "confirmed"   // the run reached the symptom and the agent agrees
	verdictUnconfirmed = "unconfirmed" // the expected behaviour held, or the agent said nothing
	verdictDisputed    = "disputed"    // the run failed but the agent's own claim disagrees
)

func (r reproductionRecord) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// reproductionFor corroborates the validation run with the agent's own block.
// An APP_FAILURE alone proves nothing: a script may assert an exact number that
// a healthy page never shows (`expected 40, got 37`). It counts as a reproduction
// only when the agent also claims the symptom at the same step (±1) or reports a
// finding on the same route; otherwise the disagreement is recorded for the human.
func reproductionFor(outcome model.Outcome, run *model.Run, res *agent.Result, feat *model.Feature) reproductionRecord {
	rec := reproductionRecord{Verdict: verdictUnconfirmed}
	if run != nil {
		rec.RunID = run.ID
	}
	var claim *agent.Reproduction
	if res != nil {
		claim = res.Reproduction
	}
	if claim != nil {
		rec.Symptom, rec.Note = strings.TrimSpace(claim.Symptom), strings.TrimSpace(claim.Note)
		rec.ClaimedStep = claim.AtStep
	}
	if rec.Symptom == "" && feat != nil {
		rec.Symptom = firstLine(feat.Summary)
	}
	switch {
	case outcome != model.OutcomeAppFailure:
		rec.Why = fmt.Sprintf("검증 실행 결과가 %s입니다: 기대 동작이 유지됐습니다", outcome)
		return rec
	case claim == nil:
		rec.Why = "에이전트가 재현 결과를 보고하지 않아 실행 실패만으로는 확정할 수 없습니다"
		return rec
	}
	failed := 0
	if run != nil {
		failed = run.FailedStep
	}
	rec.AtStep = failed
	if rec.AtStep == 0 {
		rec.AtStep = claim.AtStep
	}
	switch {
	case !claim.Reproduced:
		rec.Verdict, rec.Why = verdictDisputed, fmt.Sprintf("실행은 %d단계에서 실패했지만 에이전트는 재현되지 않았다고 보고했습니다", failed)
	case stepAgrees(failed, claim.AtStep) || findingCorroborates(res, feat):
		rec.Verdict, rec.Reproduced = verdictConfirmed, true
	default:
		rec.Verdict, rec.Why = verdictDisputed, fmt.Sprintf("실행은 %d단계에서 실패했지만 에이전트는 %d단계를 주장했고 같은 화면의 발견 사항도 없습니다", failed, claim.AtStep)
	}
	return rec
}

// stepAgrees accepts the agent's step when it is within ±1 of the failed step
// (the agent counts the step it observed, the runner the step that asserted).
func stepAgrees(failed, claimed int) bool {
	if failed == 0 || claimed == 0 {
		return false
	}
	d := failed - claimed
	return d >= -1 && d <= 1
}

// findingCorroborates reports whether the agent filed a finding on a route the
// validated feature covers, which independently places the symptom on that screen.
func findingCorroborates(res *agent.Result, feat *model.Feature) bool {
	if res == nil || feat == nil {
		return false
	}
	for _, f := range res.Findings {
		where := strings.TrimSpace(f.Where)
		if where == "" {
			continue
		}
		for _, route := range feat.Routes {
			route = strings.TrimSpace(route)
			if len(route) > 1 && strings.Contains(where, route) {
				return true
			}
		}
	}
	return false
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
	flows, flowIDs, err := o.loadFlows(ctx)
	if err != nil {
		return review(err.Error())
	}
	candidate := m.State == model.StateCandidate
	newSc, err := dsl.Parse([]byte(res.ScriptPatch))
	if err != nil {
		if !candidate {
			return review("patch does not parse: " + err.Error())
		}
		// Keep the agent's own text as the current version so the next attempt
		// sees what it wrote; the fingerprint is a hash of the raw text.
		nv := o.repairVersion(m.ID, ver.Version+1, res.ScriptPatch, rawFingerprint(res.ScriptPatch), job, res)
		if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, nil); err != nil {
			return review("new version not stored: " + err.Error())
		}
		g.Version = nv.Version
		p.ValidationError = "parse: " + err.Error()
		g = o.retryOrReview(ctx, g, m.ID, job, p, "DSL validation failed: patch does not parse: "+err.Error())
		o.logger.Printf("scenario %s: %s", m.ID, g.Reason)
		return o.recordGate(ctx, g)
	}
	// The oracle must not move (AC-09). A current version that does not parse or
	// validate has no oracle to compare against: treat it as no diff.
	if oldSc, err := dsl.Parse([]byte(ver.YAML)); err == nil && oldSc.Validate(flowIDs) == nil {
		if why := oracleDiff(oldSc, newSc, flows); why != "" {
			return review("repair changed the business oracle (AC-09): " + why)
		}
	}
	newSc.Scenario.ID = m.ID
	newSc.Scenario.Version = ver.Version + 1
	if err := newSc.Validate(flowIDs); err != nil {
		if !candidate {
			return review("patch invalid: " + err.Error())
		}
		nv := o.repairVersion(m.ID, newSc.Scenario.Version, res.ScriptPatch, newSc.Fingerprint(flows), job, res)
		if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, linksFor(newSc)); err != nil {
			return review("new version not stored: " + err.Error())
		}
		g.Version = nv.Version
		p.ValidationError = err.Error()
		g = o.retryOrReview(ctx, g, m.ID, job, p, "DSL validation failed: "+err.Error())
		o.logger.Printf("scenario %s: %s", m.ID, g.Reason)
		return o.recordGate(ctx, g)
	}
	outcome, detail, run, err := o.validateByRun(ctx, job, m.ID, newSc, flows, p)
	if err != nil {
		return review("patch validation not possible: " + err.Error())
	}
	reproduce := !model.IsShipKind(m.SourceKind) && m.SourceKind != ""
	if !candidate && outcome != model.OutcomePass && !(reproduce && outcome == model.OutcomeAppFailure) {
		return review(fmt.Sprintf("patch validation run %s: %s", outcome, detail))
	}
	out, err := yaml.Marshal(newSc)
	if err != nil {
		return review(err.Error())
	}
	nv := o.repairVersion(m.ID, newSc.Scenario.Version, string(out), newSc.Fingerprint(flows), job, res)
	if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, linksFor(newSc)); err != nil {
		return review("new version not stored: " + err.Error())
	}
	if candidate {
		// A repaired candidate takes the same road as a fresh one: approval for
		// reproduce scripts, SOAK for ship coverage, another repair otherwise.
		var feat *model.Feature
		if f, err := o.st.GetFeature(ctx, o.cfg.Project.ID, firstNonEmpty(p.FeatureID, m.OracleFeature)); err == nil {
			feat = f
		}
		g.Version = nv.Version
		g = o.settleValidation(ctx, g, m.ID, job, p, res, feat, reproduce, outcome, detail, run)
		g.Reason = fmt.Sprintf("repair v%d → v%d: %s", ver.Version, nv.Version, g.Reason)
		o.logger.Printf("scenario %s: %s", m.ID, g.Reason)
		return o.recordGate(ctx, g)
	}
	_ = o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, m.ID, o.now())
	g.State, g.Version, g.Reason = m.State, nv.Version, fmt.Sprintf("locator/navigation repair validated; v%d → v%d, state %s unchanged", ver.Version, nv.Version, m.State)
	o.logger.Printf("scenario %s: %s", m.ID, g.Reason)
	return o.recordGate(ctx, g)
}

// repairVersion builds the version row an agent repair produces.
func (o *Orchestrator) repairVersion(id string, version int, yamlText, fingerprint string, job *model.Job, res *agent.Result) *model.ScenarioVersion {
	return &model.ScenarioVersion{
		ScenarioID:  id,
		Version:     version,
		YAML:        yamlText,
		Fingerprint: fingerprint,
		CreatedBy:   "repair",
		Reason:      fmt.Sprintf("agent repair job %d: %s", job.ID, firstLine(firstNonEmpty(res.CoverageDelta, res.Evidence))),
	}
}

// rawFingerprint hashes script text that does not parse (no logical fingerprint exists).
func rawFingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "raw:" + hex.EncodeToString(sum[:])
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
