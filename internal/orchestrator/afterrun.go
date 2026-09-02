package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vigil/internal/dsl"
	"vigil/internal/evidence"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// AfterRun reacts to a classified run outcome: schedule next due, soak/promote,
// quarantine, repair job, chromium confirm, incident create/resolve.
func (o *Orchestrator) AfterRun(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result) error {
	if job == nil || run == nil {
		return errors.New("orchestrator: AfterRun needs job and run")
	}
	sc, err := o.st.GetScenario(ctx, o.cfg.Project.ID, run.ScenarioID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return o.trackImpact(ctx, job, run)
		}
		return err
	}
	if job.Kind == model.JobChromiumConfirm {
		if err := o.afterChromiumConfirm(ctx, job, run, res, sc); err != nil {
			return err
		}
		return o.trackImpact(ctx, job, run)
	}
	at := run.FinishedAt
	if at.IsZero() {
		at = o.now()
	}
	sc, err = o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, sc.ID, run.Outcome, at)
	if err != nil {
		return err
	}
	if err := o.react(ctx, job, run, res, sc); err != nil {
		return err
	}
	return o.trackImpact(ctx, job, run)
}

func (o *Orchestrator) react(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result, sc *model.Scenario) error {
	switch run.Outcome {
	case model.OutcomePass:
		return o.afterPass(ctx, sc)
	case model.OutcomeQAFlake:
		return o.afterFlake(ctx, sc)
	case model.OutcomeScriptDrift:
		return o.afterDrift(ctx, sc, run)
	case model.OutcomeLightpandaIncompatible, model.OutcomeBrowserAmbiguous:
		return o.afterBrowserAmbiguous(ctx, sc, run)
	case model.OutcomeAppFailure:
		return o.afterAppFailure(ctx, sc, run, res, "")
	case model.OutcomeNeedsReview, model.OutcomeOracleUnknown:
		o.logger.Printf("scenario %s: %s → NEEDS_REVIEW (run %d)", sc.ID, run.Outcome, run.ID)
		return o.st.SetScenarioState(ctx, o.cfg.Project.ID, sc.ID, model.StateNeedsReview)
	default:
		if run.Outcome.IsInfrastructure() {
			return o.afterInfra(ctx, sc, run)
		}
		return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cadence(sc)))
	}
}

// afterPass: soak counter/promotion, cadence, resolve open regression incidents.
func (o *Orchestrator) afterPass(ctx context.Context, sc *model.Scenario) error {
	_ = o.st.SetState(ctx, stateFlakePrefix+sc.ID, "0")
	if sc.State == model.StateSoak && sc.SoakTarget > 0 && sc.SoakPasses >= sc.SoakTarget {
		if err := o.st.SetScenarioState(ctx, o.cfg.Project.ID, sc.ID, model.StateActive); err != nil {
			return err
		}
		o.logger.Printf("scenario %s: SOAK %d/%d → ACTIVE", sc.ID, sc.SoakPasses, sc.SoakTarget)
		sc.State = model.StateActive
	}
	if err := o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cadence(sc))); err != nil {
		return err
	}
	return o.st.ResolveIncidents(ctx, o.cfg.Project.ID, model.IncidentAppRegression, sc.ID)
}

// afterFlake: normal cadence; quarantine after N consecutive flakes.
func (o *Orchestrator) afterFlake(ctx context.Context, sc *model.Scenario) error {
	n := 0
	if s, _ := o.st.GetState(ctx, stateFlakePrefix+sc.ID); s != "" {
		n, _ = strconv.Atoi(s)
	}
	n++
	if err := o.st.SetState(ctx, stateFlakePrefix+sc.ID, strconv.Itoa(n)); err != nil {
		return err
	}
	if q := o.cfg.Policy.QuarantineAfter; q > 0 && n >= q {
		o.logger.Printf("scenario %s: %d consecutive flakes → QUARANTINED", sc.ID, n)
		if err := o.st.SetScenarioState(ctx, o.cfg.Project.ID, sc.ID, model.StateQuarantined); err != nil {
			return err
		}
	}
	return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cadence(sc)))
}

// afterDrift: bounded agent repair, backoff.
func (o *Orchestrator) afterDrift(ctx context.Context, sc *model.Scenario, run *model.Run) error {
	if o.agent != nil {
		j := &model.Job{
			ProjectID:  o.cfg.Project.ID,
			Kind:       model.JobAgentRepair,
			Priority:   model.PriorityRecentFailure,
			ScenarioID: sc.ID,
			FeatureID:  run.FeatureID,
			Payload:    jobPayload{ScenarioID: sc.ID, Version: sc.CurrentVersion, RunID: run.ID, FeatureID: run.FeatureID, ShippedSHA: run.ShippedSHA}.String(),
		}
		if _, created, err := o.st.EnqueueJob(ctx, j, fmt.Sprintf("repair:%s:%d", sc.ID, sc.CurrentVersion)); err != nil {
			return err
		} else if created {
			o.logger.Printf("scenario %s v%d: SCRIPT_DRIFT → AGENT_REPAIR enqueued", sc.ID, sc.CurrentVersion)
		}
	} else {
		o.logger.Printf("scenario %s: SCRIPT_DRIFT but Browser Agent unavailable; waiting (rule 12)", sc.ID)
	}
	return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cfg.Schedule.FailureBackoff.Duration))
}

// afterBrowserAmbiguous: confirm with Chromium before any product verdict (AC-14).
func (o *Orchestrator) afterBrowserAmbiguous(ctx context.Context, sc *model.Scenario, run *model.Run) error {
	j := &model.Job{
		ProjectID:  o.cfg.Project.ID,
		Kind:       model.JobChromiumConfirm,
		Priority:   model.PriorityRecentFailure,
		ScenarioID: sc.ID,
		FeatureID:  run.FeatureID,
		Browser:    model.BrowserChromium,
		Payload:    jobPayload{ConfirmRunID: run.ID, ScenarioID: sc.ID, Version: sc.CurrentVersion, FeatureID: run.FeatureID, ShippedSHA: run.ShippedSHA}.String(),
	}
	if _, created, err := o.st.EnqueueJob(ctx, j, fmt.Sprintf("chromium-confirm:%s:%d", sc.ID, sc.CurrentVersion)); err != nil {
		return err
	} else if created {
		o.logger.Printf("scenario %s: %s → CHROMIUM_CONFIRM enqueued (run %d)", sc.ID, run.Outcome, run.ID)
	}
	return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cfg.Schedule.FailureBackoff.Duration))
}

// afterAppFailure: one OPEN APP_REGRESSION incident per scenario, metrics, bounded retry.
func (o *Orchestrator) afterAppFailure(ctx context.Context, sc *model.Scenario, run *model.Run, res *runner.Result, reason string) error {
	if _, err := o.openAppIncident(ctx, sc, run, res, reason); err != nil {
		return err
	}
	_ = o.st.BumpRegressionsCaught(ctx, sc.ID)
	next := o.now().Add(o.cfg.Schedule.FailureBackoff.Duration)
	if err := o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, next); err != nil {
		return err
	}
	j := &model.Job{
		ProjectID:   o.cfg.Project.ID,
		Kind:        model.JobRunScenario,
		Priority:    model.PriorityRecentFailure,
		ScheduledAt: next,
		ScenarioID:  sc.ID,
		FeatureID:   run.FeatureID,
		Payload:     jobPayload{FeatureID: run.FeatureID, ShippedSHA: run.ShippedSHA, Trigger: "recent_failure"}.String(),
	}
	_, _, err := o.st.EnqueueJob(ctx, j, "recent-failure:"+sc.ID)
	return err
}

// openAppIncident creates (or returns the existing) OPEN APP_REGRESSION incident for the scenario.
func (o *Orchestrator) openAppIncident(ctx context.Context, sc *model.Scenario, run *model.Run, res *runner.Result, reason string) (*model.Incident, error) {
	if in, err := o.st.OpenIncidentFor(ctx, o.cfg.Project.ID, model.IncidentAppRegression, sc.ID); err == nil {
		return in, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	summary := fmt.Sprintf("scenario %s v%d failed its business assertion on %s (shipped %s)", sc.ID, run.ScenarioVersion, run.Browser, short(run.ShippedSHA))
	if run.FailedAction != "" {
		summary += fmt.Sprintf("; step %d %s expected %q actual %q", run.FailedStep, run.FailedAction, run.Expected, run.Actual)
	}
	if run.Error != "" {
		summary += "; " + run.Error
	}
	if reason != "" {
		summary += "; " + reason
	}
	in := &model.Incident{
		ProjectID:       o.cfg.Project.ID,
		Kind:            model.IncidentAppRegression,
		ScenarioID:      sc.ID,
		ScenarioVersion: run.ScenarioVersion,
		FeatureID:       run.FeatureID,
		ShippedSHA:      run.ShippedSHA,
		RunID:           run.ID,
		Title:           fmt.Sprintf("APP_REGRESSION: %s", firstNonEmpty(sc.Title, sc.ID)),
		Summary:         summary,
		State:           "OPEN",
	}
	if _, err := o.st.CreateIncident(ctx, in); err != nil {
		return nil, err
	}
	o.writeIncident(ctx, in, run, sc, res, reason)
	o.logger.Printf("scenario %s: APP_FAILURE → incident #%d (run %d)", sc.ID, in.ID, run.ID)
	return in, nil
}

// writeIncident renders the portable incident; the evidence package may still be
// unimplemented, so failures are logged, never fatal.
func (o *Orchestrator) writeIncident(ctx context.Context, in *model.Incident, run *model.Run, sc *model.Scenario, res *runner.Result, reason string) {
	if o.ev == nil {
		return
	}
	var ver *model.ScenarioVersion
	if sc != nil {
		ver, _ = o.st.GetScenarioVersion(ctx, o.cfg.Project.ID, sc.ID, in.ScenarioVersion)
	}
	var feat *model.Feature
	if in.FeatureID != "" {
		feat, _ = o.st.GetFeature(ctx, o.cfg.Project.ID, in.FeatureID)
	}
	artifacts := map[string]string{}
	if res != nil {
		for k, v := range res.Artifacts {
			artifacts[k] = v
		}
		if res.DOMPath != "" {
			artifacts["dom"] = res.DOMPath
		}
		if res.ScreenshotPath != "" {
			artifacts["screenshot"] = res.ScreenshotPath
		}
	}
	md, js, err := o.ev.WriteIncident(ctx, evidence.IncidentInput{Incident: in, Run: run, Scenario: sc, Version: ver, Feature: feat, Artifacts: artifacts, Reason: reason})
	if err != nil {
		o.logger.Printf("incident #%d: evidence not written: %v", in.ID, err)
		return
	}
	if err := o.st.UpdateIncidentPaths(ctx, in.ID, md, js); err == nil {
		in.MarkdownPath, in.JSONPath = md, js
	}
}

// afterInfra: one OPEN ENVIRONMENT incident (scenario_id "" dedupe), backoff, no consecutive bump.
func (o *Orchestrator) afterInfra(ctx context.Context, sc *model.Scenario, run *model.Run) error {
	if _, err := o.st.OpenIncidentFor(ctx, o.cfg.Project.ID, model.IncidentEnvironment, ""); errors.Is(err, store.ErrNotFound) {
		in := &model.Incident{
			ProjectID:  o.cfg.Project.ID,
			Kind:       model.IncidentEnvironment,
			ScenarioID: "",
			FeatureID:  run.FeatureID,
			ShippedSHA: run.ShippedSHA,
			RunID:      run.ID,
			Title:      fmt.Sprintf("ENVIRONMENT: %s", run.Outcome),
			Summary:    fmt.Sprintf("infrastructure classification %s while running %s (run %d): %s", run.Outcome, sc.ID, run.ID, firstNonEmpty(run.Error, run.Actual)),
			State:      "OPEN",
		}
		if _, err := o.st.CreateIncident(ctx, in); err != nil {
			return err
		}
		o.writeIncident(ctx, in, run, sc, nil, string(run.Outcome))
		o.logger.Printf("environment incident #%d opened (%s)", in.ID, run.Outcome)
	} else if err != nil {
		return err
	}
	return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cfg.Schedule.FailureBackoff.Duration))
}

// afterChromiumConfirm resolves a Lightpanda ambiguity with the Chromium result.
func (o *Orchestrator) afterChromiumConfirm(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result, sc *model.Scenario) error {
	at := run.FinishedAt
	if at.IsZero() {
		at = o.now()
	}
	assertionFailed := run.Outcome == model.OutcomeAppFailure || (res != nil && !res.Passed && res.Class == runner.FailAssertion)
	switch {
	case run.Outcome == model.OutcomePass:
		if err := o.switchToChromium(ctx, sc, run); err != nil {
			return err
		}
		sc, err := o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, sc.ID, model.OutcomePass, at)
		if err != nil {
			return err
		}
		return o.afterPass(ctx, sc)
	case assertionFailed:
		sc, err := o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, sc.ID, model.OutcomeAppFailure, at)
		if err != nil {
			return err
		}
		return o.afterAppFailure(ctx, sc, run, res, "confirmed on chromium")
	case run.Outcome.IsInfrastructure():
		sc, err := o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, sc.ID, run.Outcome, at)
		if err != nil {
			return err
		}
		return o.afterInfra(ctx, sc, run)
	default:
		if _, err := o.st.RecordScenarioOutcome(ctx, o.cfg.Project.ID, sc.ID, run.Outcome, at); err != nil {
			return err
		}
		// Inconclusive on both browsers usually means the script is wrong, not
		// that a person is needed - the same bounded self-repair budget applies
		// here as after a failed validation (policy.agent_fix_attempts).
		detail := fmt.Sprintf("chromium confirm inconclusive (%s)", run.Outcome)
		if n, limit, retried := o.retryFix(ctx, sc.ID, job, jobPayload{FeatureID: run.FeatureID, ShippedSHA: run.ShippedSHA, RunID: run.ID}, detail); retried {
			o.logger.Printf("scenario %s: %s; agent fix attempt %d/%d instead of review", sc.ID, detail, n, limit)
			return o.st.SetScenarioNextDue(ctx, o.cfg.Project.ID, sc.ID, o.now().Add(o.cfg.Schedule.FailureBackoff.Duration))
		}
		o.logger.Printf("scenario %s: %s → NEEDS_REVIEW", sc.ID, detail)
		return o.st.SetScenarioState(ctx, o.cfg.Project.ID, sc.ID, model.StateNeedsReview)
	}
}

// switchToChromium adds a version identical except browser.primary: chromium
// (mechanics, not oracle; created_by system).
func (o *Orchestrator) switchToChromium(ctx context.Context, sc *model.Scenario, run *model.Run) error {
	m, ver, err := o.st.GetCurrentScenarioVersion(ctx, o.cfg.Project.ID, sc.ID)
	if err != nil {
		return err
	}
	d, err := dsl.Parse([]byte(ver.YAML))
	if err != nil {
		return fmt.Errorf("scenario %s v%d: %w", sc.ID, ver.Version, err)
	}
	if d.Browser.Primary == string(model.BrowserChromium) {
		return nil
	}
	d.Browser.Primary = string(model.BrowserChromium)
	d.Scenario.Version = ver.Version + 1
	out, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	flows, _, _ := o.loadFlows(ctx)
	nv := &model.ScenarioVersion{
		ScenarioID:  sc.ID,
		Version:     ver.Version + 1,
		YAML:        string(out),
		Fingerprint: d.Fingerprint(flows),
		CreatedBy:   "system",
		Reason:      fmt.Sprintf("lightpanda incompatible; chromium confirmed on run %d", run.ID),
	}
	if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, nil); err != nil {
		return err
	}
	o.logger.Printf("scenario %s: v%d → v%d browser.primary=chromium (state %s unchanged)", sc.ID, ver.Version, nv.Version, m.State)
	return nil
}

// trackImpact updates impact:<feature>:<sha> and, when every impacted run is
// finished, either keeps coverage or enqueues BROWSER_AGENT_VERIFY_CHANGE (PRD §5.3).
func (o *Orchestrator) trackImpact(ctx context.Context, job *model.Job, run *model.Run) error {
	p := parsePayload(job.Payload)
	if !p.Impacted || p.FeatureID == "" {
		return nil
	}
	key := stateImpactPrefix + p.FeatureID + ":" + p.ShippedSHA
	var st impactState
	found, err := o.getJSONState(ctx, key, &st)
	if err != nil || !found || st.Done {
		return err
	}
	if st.Outcomes == nil {
		st.Outcomes = map[string]string{}
	}
	var pending []int64
	for _, id := range st.Pending {
		if id != job.ID {
			pending = append(pending, id)
		}
	}
	st.Pending = pending
	st.Outcomes[strconv.FormatInt(job.ID, 10)] = string(run.Outcome)
	if len(st.Pending) > 0 {
		return o.setJSONState(ctx, key, st)
	}
	st.Done = true
	uncertain := false
	for _, oc := range st.Outcomes {
		switch model.Outcome(oc) {
		case model.OutcomeAppFailure, model.OutcomeScriptDrift, model.OutcomeBrowserAmbiguous, model.OutcomeNeedsReview, model.OutcomeLightpandaIncompatible, model.OutcomeOracleUnknown:
			uncertain = true
		}
	}
	switch {
	case !uncertain:
		st.Result = "coverage kept: impacted scripts passed"
	case o.agent == nil:
		st.Result = "impacted scripts failed; Browser Agent unavailable (rule 12)"
	default:
		if ok, reason := o.agentBudgetOK(ctx); !ok {
			st.Result = "impacted scripts failed; " + reason
			break
		}
		if _, _, err := o.enqueueAgentJob(ctx, model.JobAgentVerify, p.FeatureID, p.ShippedSHA, jobPayload{FeatureID: p.FeatureID, ShippedSHA: p.ShippedSHA, Trigger: "impacted_failures"}); err != nil {
			return err
		}
		st.Result = "impacted scripts failed/uncertain → AGENT_VERIFY enqueued"
	}
	o.logger.Printf("feature %s@%s: %s", p.FeatureID, short(p.ShippedSHA), st.Result)
	return o.setJSONState(ctx, key, st)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

var _ = time.Second
