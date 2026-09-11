package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vigil/internal/agent"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/scheduler"
)

// newReproduceAgent builds the Browser Agent for `vigil reproduce`; tests swap it for a fake.
var newReproduceAgent = func(a *app) agent.Adapter { return a.buildAgent() }

// agentJobs is what the settle loop needs from the scheduler and the orchestrator:
// run one queued agent job inline, and say which evidence directory it wrote to.
// *scheduler.Scheduler and *orchestrator.Orchestrator provide the two halves.
type agentJobs interface {
	RunAgentJobNow(ctx context.Context, jobID int64) error
	AgentDirForJob(ctx context.Context, jobID int64) string
}

// inlineAgent joins the scheduler (execution) and the orchestrator (evidence dir).
type inlineAgent struct {
	sched *scheduler.Scheduler
	orch  *orchestrator.Orchestrator
}

func (i inlineAgent) RunAgentJobNow(ctx context.Context, jobID int64) error {
	return i.sched.RunAgentJobNow(ctx, jobID)
}

func (i inlineAgent) AgentDirForJob(ctx context.Context, jobID int64) string {
	return i.orch.AgentDirForJob(ctx, jobID)
}

// cmdReproduce hands one known issue/log feature to the Browser Agent
// (AGENT_REPRODUCE) right now, like `discover` does for ship features, and
// prints what the gate did with the proposed script. A candidate the gate could
// not settle (still CANDIDATE, an AGENT_REPAIR queued) is repaired inline too,
// so the command returns a settled script instead of leaving work for whichever
// loop process runs next.
func (a *app) cmdReproduce(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reproduce", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "write the agent request and prompt, do not call the model")
	noRepair := fs.Bool("no-repair", false, "run only the reproduce job; leave a queued AGENT_REPAIR to the loop")
	fresh := fs.Bool("fresh", false, "always start a new reproduce job, even when a candidate of this feature is still being repaired")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errors.New("usage: vigil reproduce <feature-id> [--dry-run] [--no-repair] [--fresh]")
	}
	featureID := pos[0]
	f, err := a.st.GetFeature(ctx, a.cfg.Project.ID, featureID)
	if err != nil {
		return fmt.Errorf("feature %s: %w (run `scan` or `import --history` first)", featureID, err)
	}
	if model.IsShipKind(f.Kind) {
		return fmt.Errorf("feature %s is a %s feature: only issue/log items (Jira issue, log signature) can be reproduced; use `vigil discover %s` for deployed changes",
			f.ID, orDefault(f.Kind, model.FeatureKindShip), f.ID)
	}
	if *dry {
		return a.reproduceDryRun(ctx, f)
	}
	run := a.buildRunner()
	ag := newReproduceAgent(a)
	if ag == nil {
		return errors.New("reproduce needs the Browser Agent; see `vigil doctor`")
	}
	orch := a.buildOrchestrator(run, ag)
	ag2 := inlineAgent{sched: a.buildScheduler(orch, run, false, true), orch: orch}

	// Resume: a candidate of this feature that is still being repaired is
	// finished first. Asking the model for a second script would abandon the
	// one it already wrote and burn another task.
	if !*fresh && !*noRepair {
		if sc, job, err := a.repairInProgress(ctx, f); err != nil {
			return err
		} else if sc != "" {
			a.printf("resuming repair of %s (job %d)\n", sc, job)
			return a.settleCandidate(ctx, ag2, sc)
		}
	}
	id, created, err := orch.EnqueueReproduce(ctx, f)
	if err != nil {
		return err
	}
	if !created {
		a.printf("reusing queued reproduce job %d\n", id)
	}
	a.printf("reproduce %s (%s %s, sha %s) → job %d\n", f.ID, orDefault(f.Kind, "item"), orDefault(f.Ref, f.ID), short(f.LatestShippedSHA), id)
	gates, err := a.runAgentJob(ctx, ag2, id)
	if err != nil {
		return err
	}
	if *noRepair {
		return nil
	}
	for _, g := range gates {
		if g.State != model.StateCandidate || g.ScenarioID == "" {
			continue
		}
		if err := a.settleCandidate(ctx, ag2, g.ScenarioID); err != nil {
			return err
		}
	}
	return nil
}

// repairInProgress finds a CANDIDATE script of this feature whose AGENT_REPAIR
// job is still queued (the previous `reproduce` was interrupted, or the gate
// queued a repair nobody ran).
func (a *app) repairInProgress(ctx context.Context, f *model.Feature) (string, int64, error) {
	scs, err := a.st.ListScenarios(ctx, a.cfg.Project.ID, model.StateCandidate)
	if err != nil {
		return "", 0, err
	}
	ref := firstNonEmptyStr(f.Ref, f.ID)
	for _, sc := range scs {
		if sc.SourceRef != ref && sc.OracleFeature != f.ID {
			continue
		}
		job, err := a.readyRepairJob(ctx, sc.ID)
		if err != nil {
			return "", 0, err
		}
		if job != nil {
			return sc.ID, job.ID, nil
		}
	}
	return "", 0, nil
}

// runAgentJob executes one queued agent job inline and prints its outcome: the
// job row, then the gate outcomes of that job's own evidence directory.
func (a *app) runAgentJob(ctx context.Context, ag agentJobs, id int64) ([]orchestrator.GateOutcome, error) {
	if err := ag.RunAgentJobNow(ctx, id); err != nil {
		return nil, err
	}
	if err := a.printJobResult(ctx, id); err != nil {
		return nil, err
	}
	return a.printGateOutcomes(gatesForJob(ctx, ag, id))
}

// settleCandidate runs the AGENT_REPAIR jobs the gate queued for a still-unsettled
// candidate, one at a time and inline, until the scenario leaves CANDIDATE
// (PENDING_APPROVAL / NEEDS_REVIEW / SOAK / ...) or policy.agent_fix_attempts is
// spent and no repair job is queued any more.
func (a *app) settleCandidate(ctx context.Context, ag agentJobs, scenarioID string) error {
	for i := 0; i <= a.cfg.Policy.AgentFixAttempts; i++ {
		sc, err := a.st.GetScenario(ctx, a.cfg.Project.ID, scenarioID)
		if err != nil {
			// The script was deleted (or never stored): say so, do not fail the command.
			a.printf("scenario %s: not in the corpus any more (%v); nothing to settle\n", scenarioID, err)
			return nil
		}
		if sc.State != model.StateCandidate {
			a.printf("scenario %s settled: %s\n", scenarioID, sc.State)
			return nil
		}
		job, err := a.readyRepairJob(ctx, scenarioID)
		if err != nil {
			return err
		}
		if job == nil {
			a.printf("scenario %s: still CANDIDATE and no AGENT_REPAIR queued (policy.agent_fix_attempts=%d spent); see `vigil show %s`\n",
				scenarioID, a.cfg.Policy.AgentFixAttempts, scenarioID)
			return nil
		}
		a.printf("repair %s → job %d\n", scenarioID, job.ID)
		gates, err := a.runAgentJob(ctx, ag, job.ID)
		if err != nil {
			return err
		}
		if len(gates) == 0 {
			// No gate outcome means the agent job produced nothing (model
			// unavailable, adapter error): the job row carries the reason.
			a.printf("repair job %d produced no gate outcome%s; stopping\n", job.ID, jobErrorSuffix(a.jobError(ctx, job.ID)))
			return nil
		}
	}
	return nil
}

// jobError returns the job's last error (empty when unknown or clean).
func (a *app) jobError(ctx context.Context, id int64) string {
	jobs, err := a.st.ListJobs(ctx, a.cfg.Project.ID, nil, 1000)
	if err != nil {
		return ""
	}
	for _, j := range jobs {
		if j.ID == id {
			return strings.TrimSpace(j.LastError)
		}
	}
	return ""
}

func jobErrorSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// readyRepairJob returns the queued AGENT_REPAIR job for one scenario, if any.
func (a *app) readyRepairJob(ctx context.Context, scenarioID string) (*model.Job, error) {
	jobs, err := a.st.ListJobs(ctx, a.cfg.Project.ID, []model.JobState{model.JobReady}, 500)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Kind == model.JobAgentRepair && j.ScenarioID == scenarioID {
			return j, nil
		}
	}
	return nil, nil
}

// reproduceDryRun writes the bounded request (YAML) and the task prompt the
// model would receive into the feature's agent evidence directory.
func (a *app) reproduceDryRun(ctx context.Context, f *model.Feature) error {
	orch := a.buildOrchestrator(nil, nil)
	req, err := orch.ReproduceRequest(ctx, f)
	if err != nil {
		return err
	}
	dir, err := a.ev.AgentDir(f.ID+"-dry-run", time.Now())
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(req)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, agent.FileRequest), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, agent.FileTask), []byte(agent.TaskPrompt(req)), 0o644); err != nil {
		return err
	}
	if a.jsonOut {
		return a.printJSON(map[string]any{"feature_id": f.ID, "dry_run": true, "dir": dir, "request": req})
	}
	a.printf("dry-run: reproduce %s (%s %s) — model not called\n  request: %s\n  prompt:  %s\n%s",
		f.ID, orDefault(f.Kind, "item"), orDefault(f.Ref, f.ID), filepath.Join(dir, agent.FileRequest), filepath.Join(dir, agent.FileTask), b)
	return nil
}

// gatesForJob reads the gate outcomes of exactly one agent job, from the evidence
// directory that job wrote. Scanning the evidence tree instead would pick up every
// older run of the project (and try to settle scripts that are long gone).
func gatesForJob(ctx context.Context, ag agentJobs, jobID int64) []orchestrator.GateOutcome {
	dir := ag.AgentDirForJob(ctx, jobID)
	if dir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gate.json"))
	if err != nil {
		return nil
	}
	var gates []orchestrator.GateOutcome
	if err := json.Unmarshal(raw, &gates); err != nil {
		return nil
	}
	return gates
}

// printGateOutcomes prints one job's gate outcomes (candidate → scenario → state
// → reason) as a table, or JSON with --json.
func (a *app) printGateOutcomes(gates []orchestrator.GateOutcome) ([]orchestrator.GateOutcome, error) {
	if a.jsonOut {
		return gates, a.printJSON(gates)
	}
	if len(gates) == 0 {
		a.printf("gate: no script proposed (see the agent evidence directory and `vigil status`)\n")
		return gates, nil
	}
	a.printf("%-4s %-36s %-18s %-4s %s\n", "#", "SCENARIO", "STATE", "VER", "REASON")
	for _, g := range gates {
		state := string(g.State)
		if state == "" {
			state = "(rejected)"
		}
		ver := ""
		if g.Version > 0 {
			ver = fmt.Sprintf("v%d", g.Version)
		}
		a.printf("%-4d %-36s %-18s %-4s %s\n", g.Candidate, trunc(orDefault(g.ScenarioID, "-"), 36), state, ver, strings.ReplaceAll(g.Reason, "\n", " "))
	}
	return gates, nil
}
