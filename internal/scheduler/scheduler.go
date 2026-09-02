// Package scheduler runs the continuous loop: ingest → gate → orchestrate →
// select due/impacted work under budgets and locks → workers (PRD §12).
//
// The scheduler never needs an LLM: deterministic RUN_SCENARIO work continues
// when the Browser Agent is unavailable (rule 12); AGENT_* jobs simply stay
// READY until an agent worker exists.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"vigil/internal/classify"
	"vigil/internal/config"
	"vigil/internal/dsl"
	"vigil/internal/evidence"
	"vigil/internal/gate"
	"vigil/internal/ingest"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/runner"
	"vigil/internal/store"
)

// Runner is the subset of *runner.Runner the scheduler needs (fakeable in tests).
type Runner interface {
	Run(ctx context.Context, spec runner.Spec) (*runner.Result, error)
}

// Orchestrator is the subset of *orchestrator.Orchestrator the scheduler needs.
type Orchestrator interface {
	PlanFeature(ctx context.Context, f *model.Feature) (*orchestrator.Decision, error)
	EnqueueForFeature(ctx context.Context, f *model.Feature, d *orchestrator.Decision) error
	AfterRun(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result) error
	HandleAgentJob(ctx context.Context, job *model.Job) error
	SupervisorTick(ctx context.Context) (*orchestrator.SupervisorResult, error)
}

// Gate is the subset of *gate.Gate the scheduler needs.
type Gate interface {
	Check(ctx context.Context, f *model.Feature, lastMarker string) (model.Readiness, string, error)
}

const (
	leaseDuration     = 5 * time.Minute
	heartbeatEvery    = 30 * time.Second
	dueLimit          = 200 // AC-22: never load the whole corpus
	lockRequeueDelay  = 30 * time.Second
	runJobMaxAttempts = 5 // lock conflicts consume attempts; leave room
	pruneEvery        = time.Hour
	inlineWorker      = "inline"
)

var runKinds = []model.JobKind{model.JobRunScenario, model.JobChromiumConfirm, model.JobValidateCandidate, model.JobChromiumEvidence}
var agentKinds = []model.JobKind{model.JobAgentDiscover, model.JobAgentVerify, model.JobAgentRepair}

type Scheduler struct {
	cfg  *config.Config
	st   *store.Store
	orch Orchestrator   // nil → features are gated but not planned
	run  Runner         // nil → run jobs fail with a clear error
	gate Gate           // nil → every feature is READY
	in   ingest.Adapter // may be nil (no discovery)
	ev   *evidence.Store

	// Log receives timestamped loop/worker lines; defaults to log.Default().
	Log *log.Logger
	// Classify maps a runner result to an outcome; defaults to classify.Classify.
	Classify func(classify.Input) (model.Outcome, string)
	// TargetHealthy is the cheap base_url probe used at classification time.
	TargetHealthy func(ctx context.Context) bool
	// AgentAvailable enables the agent worker goroutine (rule 12: off when the provider is missing).
	AgentAvailable bool
	// Now is injectable for tests.
	Now func() time.Time

	browserCache      sync.Map // "<scenario>@<version>" → model.Browser
	budgetWarned      bool
	agentBudgetWarned bool
	windowWarned      bool
}

// New wires the concrete production types. Nil pointers are tolerated
// (a nil orchestrator/runner/gate degrades gracefully instead of panicking).
func New(cfg *config.Config, st *store.Store, orch *orchestrator.Orchestrator, run *runner.Runner, g *gate.Gate, in ingest.Adapter) *Scheduler {
	var o Orchestrator
	if orch != nil {
		o = orch
	}
	var r Runner
	if run != nil {
		r = run
	}
	var gt Gate
	if g != nil {
		gt = g
	}
	return NewWith(cfg, st, o, r, gt, in)
}

// NewWith accepts interfaces so tests can inject fakes.
func NewWith(cfg *config.Config, st *store.Store, orch Orchestrator, run Runner, g Gate, in ingest.Adapter) *Scheduler {
	s := &Scheduler{
		cfg:      cfg,
		st:       st,
		orch:     orch,
		run:      run,
		gate:     g,
		in:       in,
		ev:       evidence.New(cfg.Abs(cfg.Evidence.Dir)),
		Log:      log.Default(),
		Classify: classify.Classify,
		Now:      func() time.Time { return time.Now().UTC() },
	}
	s.TargetHealthy = s.probeTarget
	return s
}

// Evidence exposes the evidence store used for run dirs and PASS markers.
func (s *Scheduler) Evidence() *evidence.Store { return s.ev }

func (s *Scheduler) project() string { return s.cfg.Project.ID }

func (s *Scheduler) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(format, a...)
	}
}

// ---- ingest + gate + orchestrate ---------------------------------------------

// ScanOnce ingests new features, applies the deployment gate and hands READY
// features to the orchestrator (used by `scan` and every poll interval).
func (s *Scheduler) ScanOnce(ctx context.Context) error {
	if s.in != nil {
		events, err := s.in.Poll(ctx)
		if err != nil {
			s.logf("scan: ingest %s failed: %v", s.in.Name(), err)
		}
		for _, ev := range events {
			if ev.Source == "" {
				ev.Source = s.in.Name()
			}
			isNew, err := s.st.UpsertFeature(ctx, s.project(), ev)
			if err != nil {
				s.logf("scan: upsert feature %s: %v", ev.FeatureID, err)
				continue
			}
			if isNew {
				s.logf("scan: feature %s shipped sha=%s paths=%d routes=%v", ev.FeatureID, short(ev.ShippedSHA), len(ev.ChangedPaths), ev.Routes)
			}
		}
	}
	feats, err := s.st.ListUnhandledFeatures(ctx, s.project())
	if err != nil {
		return fmt.Errorf("scan: list unhandled features: %w", err)
	}
	for _, f := range feats {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.gateFeature(ctx, f)
	}
	return nil
}

// markerKey stores "<sha> <marker>" so a marker captured for an older SHA of the
// same feature is never mistaken for the current one.
func markerKey(featureID string) string { return "gate:marker:" + featureID }

func (s *Scheduler) gateFeature(ctx context.Context, f *model.Feature) {
	last := ""
	if raw, err := s.st.GetState(ctx, markerKey(f.ID)); err == nil && raw != "" {
		if sha, marker, ok := strings.Cut(raw, " "); ok && sha == f.LatestShippedSHA {
			last = marker
		}
	}
	state := model.ReadinessReady
	marker := last
	if s.gate != nil {
		var err error
		state, marker, err = s.gate.Check(ctx, f, last)
		if err != nil {
			s.logf("gate: %s: %v", f.ID, err)
			return
		}
	}
	if marker != last {
		if err := s.st.SetState(ctx, markerKey(f.ID), f.LatestShippedSHA+" "+marker); err != nil {
			s.logf("gate: persist marker for %s: %v", f.ID, err)
		}
	}
	switch state {
	case model.ReadinessReady, model.ReadinessUnknown:
		now := s.Now()
		if err := s.st.SetFeatureReadiness(ctx, s.project(), f.ID, state, &now); err != nil {
			s.logf("gate: readiness %s: %v", f.ID, err)
		}
		f.Readiness = state
		f.ReadyAt = &now
		if s.orch == nil {
			s.logf("gate: %s is %s but no orchestrator is configured; leaving unhandled", f.ID, state)
			return
		}
		d, err := s.orch.PlanFeature(ctx, f)
		if err != nil {
			s.logf("orchestrate: plan %s: %v", f.ID, err)
			return
		}
		if err := s.orch.EnqueueForFeature(ctx, f, d); err != nil {
			s.logf("orchestrate: enqueue %s: %v", f.ID, err)
			return
		}
		if err := s.st.MarkFeatureHandled(ctx, s.project(), f.ID, f.LatestShippedSHA); err != nil {
			s.logf("orchestrate: mark handled %s: %v", f.ID, err)
		}
		action, reason := "", ""
		if d != nil {
			action, reason = string(d.Action), d.Reason
		}
		s.logf("orchestrate: %s sha=%s readiness=%s → %s %s", f.ID, short(f.LatestShippedSHA), state, action, reason)
	default:
		if f.Readiness != model.ReadinessWaiting {
			_ = s.st.SetFeatureReadiness(ctx, s.project(), f.ID, model.ReadinessWaiting, nil)
		}
		s.logf("gate: %s sha=%s waiting for deployment (marker=%q)", f.ID, short(f.LatestShippedSHA), marker)
	}
}

// ---- due selection -------------------------------------------------------------

// ActiveHoursKey is where the UI writes runtime overrides; see config.
const ActiveHoursKey = config.ActiveHoursStateKey

// ActiveHours resolves the effective window: a UI override in scheduler_state
// wins over the config default. A malformed or absent override falls back to
// config so a bad write can never wedge the loop.
func (s *Scheduler) ActiveHours(ctx context.Context) config.ActiveHours {
	raw, err := s.st.GetState(ctx, ActiveHoursKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return s.cfg.Schedule.ActiveHours
	}
	var override config.ActiveHours
	if err := json.Unmarshal([]byte(raw), &override); err != nil {
		s.logf("tick: ignoring malformed %s override: %v", ActiveHoursKey, err)
		return s.cfg.Schedule.ActiveHours
	}
	return override
}

// Tick enqueues due ACTIVE/SOAK scenarios (cadence + priority), respecting
// budgets and the active-hours window.
func (s *Scheduler) Tick(ctx context.Context) error {
	// Gate before ListDueScenarios: outside the window nothing is enqueued at
	// all, so 08:00 starts from an empty queue instead of a backlog burst.
	// Deployment-triggered work (ScanOnce → orchestrator) is deliberately
	// unaffected: a ship still gets verified.
	if w := s.ActiveHours(ctx); !w.Allows(s.Now()) {
		if !s.windowWarned {
			s.logf("tick: outside active hours (%s); cadence work paused", w)
			s.windowWarned = true
		}
		return nil
	}
	if s.windowWarned {
		s.logf("tick: inside active hours; cadence work resumed")
		s.windowWarned = false
	}
	due, err := s.st.ListDueScenarios(ctx, s.project(), s.Now(), dueLimit)
	if err != nil {
		return fmt.Errorf("tick: list due: %w", err)
	}
	if len(due) == 0 {
		return nil
	}
	skipAll, skipP2 := s.budgetPressure(ctx)
	if skipAll {
		if !s.budgetWarned {
			s.logf("tick: browser budget exhausted (%d min/h); deferring %d due scenario(s)", s.cfg.Budget.BrowserMinutesPerHour, len(due))
			s.budgetWarned = true
		}
		return nil
	}
	s.budgetWarned = false
	enq, deferred := 0, 0
	for _, m := range due {
		if skipP2 && classOf(m) == "P2" && m.ConsecutiveFailures == 0 {
			deferred++
			continue
		}
		_, created, err := s.EnqueueScenario(ctx, m, PriorityFor(m), s.scenarioBrowser(ctx, m), "")
		if err != nil {
			s.logf("tick: enqueue %s: %v", m.ID, err)
			continue
		}
		if created {
			enq++
		}
	}
	if enq > 0 || deferred > 0 {
		s.logf("tick: due=%d enqueued=%d deferred(P2 budget)=%d", len(due), enq, deferred)
	}
	return nil
}

// EnqueueScenario adds one RUN_SCENARIO job with the standard dedup key.
func (s *Scheduler) EnqueueScenario(ctx context.Context, m *model.Scenario, priority int, browser model.Browser, featureID string) (int64, bool, error) {
	if browser == "" {
		browser = model.Browser(s.cfg.Browser.Primary)
	}
	return s.st.EnqueueJob(ctx, &model.Job{
		ProjectID:   s.project(),
		Kind:        model.JobRunScenario,
		Priority:    priority,
		ScenarioID:  m.ID,
		FeatureID:   featureID,
		Browser:     browser,
		MaxAttempts: runJobMaxAttempts,
	}, "run:"+m.ID)
}

// PriorityFor maps scenario state/class/recent failure to PRD §12 priority.
func PriorityFor(m *model.Scenario) int {
	if m.ConsecutiveFailures > 0 {
		return model.PriorityRecentFailure
	}
	if m.State == model.StateSoak {
		return model.PrioritySoak
	}
	switch classOf(m) {
	case "P0":
		return model.PriorityP0
	case "P2":
		return model.PriorityP2
	default:
		return model.PriorityP1
	}
}

func classOf(m *model.Scenario) string {
	if m.Class == "" {
		return "P1"
	}
	return m.Class
}

// budgetPressure: ≥100% of the browser minutes budget defers everything;
// ≥80% defers P2/background first (PRD §12).
func (s *Scheduler) budgetPressure(ctx context.Context) (skipAll, skipP2 bool) {
	limit := int64(s.cfg.Budget.BrowserMinutesPerHour) * 60_000
	if limit <= 0 {
		return false, false
	}
	used, err := s.st.BudgetUsed(ctx, s.project(), "browser", time.Hour)
	if err != nil {
		s.logf("tick: budget query: %v", err)
		return false, false
	}
	if used >= limit {
		return true, true
	}
	return false, used*10 >= limit*8
}

// scenarioBrowser reads browser.primary / requires_chromium from the current
// script version (cached per version), else the configured primary.
func (s *Scheduler) scenarioBrowser(ctx context.Context, m *model.Scenario) model.Browser {
	key := fmt.Sprintf("%s@%d", m.ID, m.CurrentVersion)
	if b, ok := s.browserCache.Load(key); ok {
		return b.(model.Browser)
	}
	b := model.Browser(s.cfg.Browser.Primary)
	if v, err := s.st.GetScenarioVersion(ctx, s.project(), m.ID, m.CurrentVersion); err == nil {
		if sc, err := dsl.Parse([]byte(v.YAML)); err == nil {
			switch {
			case sc.Browser.RequiresChromium:
				b = model.BrowserChromium
			case sc.Browser.Primary != "":
				b = model.Browser(sc.Browser.Primary)
			}
		}
	}
	s.browserCache.Store(key, b)
	return b
}

// ---- loop + workers ------------------------------------------------------------

// Loop runs ScanOnce/Tick on intervals plus N functional workers and one agent
// worker until ctx is cancelled. It returns nil on graceful shutdown.
func (s *Scheduler) Loop(ctx context.Context) error {
	workers := s.cfg.Workers.Functional
	if workers < 1 {
		workers = 1
	}
	s.logf("loop: start project=%s workers=%d tick=%s poll=%s agent=%v evidence=%s",
		s.project(), workers, s.cfg.Schedule.Tick.Duration, s.cfg.Discovery.PollInterval.Duration, s.AgentAvailable, s.ev.Root())

	var wg sync.WaitGroup
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s.workerLoop(ctx, id, "functional", runKinds)
		}(fmt.Sprintf("w%d-%d", i, os.Getpid()))
	}
	if s.AgentAvailable && s.orch != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.workerLoop(ctx, fmt.Sprintf("agent-%d", os.Getpid()), "agent", agentKinds)
		}()
	} else {
		s.logf("loop: Browser Agent unavailable; AGENT_* jobs stay READY, deterministic QA continues (rule 12)")
	}

	if err := s.ScanOnce(ctx); err != nil && ctx.Err() == nil {
		s.logf("scan: %v", err)
	}
	s.reapAndTick(ctx)
	s.prune()

	tick := time.NewTicker(s.cfg.Schedule.Tick.Duration)
	poll := time.NewTicker(s.cfg.Discovery.PollInterval.Duration)
	prune := time.NewTicker(pruneEvery)
	// A disabled supervisor still gets a ticker so the select stays simple; the
	// tick itself is a no-op. Its interval is minutes, so the cost is nil.
	supTick := s.cfg.Supervisor.Tick.Duration
	if supTick <= 0 {
		supTick = 15 * time.Minute
	}
	supervise := time.NewTicker(supTick)
	defer tick.Stop()
	defer poll.Stop()
	defer prune.Stop()
	defer supervise.Stop()
	if s.cfg.Supervisor.Enabled {
		s.logf("loop: supervisor every %s (model=%s dry_run=%v max_actions=%d)", supTick,
			s.cfg.Supervisor.EffectiveModel(s.cfg.Agent.Model), s.cfg.Supervisor.DryRunEnabled(), s.cfg.Supervisor.MaxActions)
	}
	for {
		select {
		case <-ctx.Done():
			s.logf("loop: shutdown requested; waiting for workers")
			wg.Wait()
			s.logf("loop: stopped")
			return nil
		case <-tick.C:
			s.reapAndTick(ctx)
		case <-poll.C:
			if err := s.ScanOnce(ctx); err != nil && ctx.Err() == nil {
				s.logf("scan: %v", err)
			}
		case <-prune.C:
			s.prune()
		case <-supervise.C:
			s.superviseOnce(ctx)
		}
	}
}

func (s *Scheduler) reapAndTick(ctx context.Context) {
	if n, err := s.st.ReapExpiredLeases(ctx); err != nil {
		s.logf("tick: reap leases: %v", err)
	} else if n > 0 {
		s.logf("tick: returned %d expired lease(s) to the queue", n)
	}
	if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
		s.logf("%v", err)
	}
}

// superviseOnce runs one coverage-planning tick. It shares the active-hours
// window with cadence work: the supervisor enqueues browser jobs, so letting it
// run at 03:00 would defeat the window it is subject to.
func (s *Scheduler) superviseOnce(ctx context.Context) {
	if !s.cfg.Supervisor.Enabled || s.orch == nil {
		return
	}
	if w := s.ActiveHours(ctx); !w.Allows(s.Now()) {
		return
	}
	res, err := s.orch.SupervisorTick(ctx)
	if err != nil && ctx.Err() == nil {
		s.logf("%v", err) // the orchestrator's errors already say "supervisor:"
		return
	}
	if res == nil {
		return
	}
	if res.Skipped != "" {
		s.logf("supervisor: skipped (%s)", res.Skipped)
		return
	}
	applied, refused := 0, 0
	for _, v := range res.Verdicts {
		if v.Applied {
			applied++
		} else if !v.Accepted {
			refused++
		}
	}
	s.logf("supervisor: %d action(s) proposed, %d applied, %d refused, %d gap(s) named%s",
		len(res.Verdicts), applied, refused, len(res.CoverageGaps), map[bool]string{true: " [dry-run]"}[res.DryRun])
}

func (s *Scheduler) prune() {
	rep, err := s.ev.PruneWithCap(s.cfg.Evidence.RetainPassDays, s.cfg.Evidence.RetainFailDays, s.cfg.Evidence.MaxTotalMB, false)
	if err != nil {
		s.logf("evidence: prune: %v", err)
	}
	if rep.DeletedDirs > 0 {
		s.logf("evidence: pruned %d dir(s), freed %.1f MB; now %.1f MB (%d runs, %d agent runs)", rep.DeletedDirs, float64(rep.FreedBytes)/1048576, float64(rep.TotalBytes)/1048576, rep.RunDirs, rep.AgentDirs)
	}
	if days := s.cfg.Evidence.RetainRunsDays; days > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if n, err := s.st.DeleteRunsBefore(ctx, s.project(), time.Now().Add(-time.Duration(days)*24*time.Hour)); err != nil {
			s.logf("evidence: db retention: %v", err)
		} else if n > 0 {
			s.logf("evidence: deleted %d run row(s) older than %d days", n, days)
		}
	}
}

func (s *Scheduler) workerLoop(ctx context.Context, workerID, kind string, kinds []model.JobKind) {
	idle := s.cfg.Schedule.Tick.Duration / 2
	if idle < 200*time.Millisecond {
		idle = 200 * time.Millisecond
	}
	for ctx.Err() == nil {
		_ = s.st.HeartbeatWorker(ctx, workerID, kind)
		if kind == "agent" && s.agentBudgetExhausted(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(idle):
			}
			continue
		}
		job, err := s.st.ClaimJob(ctx, s.project(), workerID, leaseDuration, kinds...)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err != store.ErrNotFound {
				s.logf("worker %s: claim: %v", workerID, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(idle):
			}
			continue
		}
		s.execute(ctx, workerID, job)
	}
}

// agentBudgetExhausted reports whether budget.agent_tasks_per_hour is spent;
// AGENT_* jobs then stay READY until the window moves (PRD §12).
func (s *Scheduler) agentBudgetExhausted(ctx context.Context) bool {
	limit := int64(s.cfg.Budget.AgentTasksPerHour)
	if limit <= 0 {
		return false
	}
	used, err := s.st.BudgetUsed(ctx, s.project(), "agent", time.Hour)
	if err != nil {
		return false
	}
	if used >= limit {
		if !s.agentBudgetWarned {
			s.logf("agent: budget exhausted (%d task(s)/h); AGENT_* jobs stay READY", limit)
			s.agentBudgetWarned = true
		}
		return true
	}
	s.agentBudgetWarned = false
	return false
}

// execute runs one claimed job with lease heartbeats and panic isolation (AC-17).
func (s *Scheduler) execute(ctx context.Context, workerID string, job *model.Job) (run *model.Run) {
	fin := context.WithoutCancel(ctx)
	defer func() {
		if r := recover(); r != nil {
			s.logf("worker %s: job %d (%s %s) panicked: %v\n%s", workerID, job.ID, job.Kind, job.ScenarioID, r, debug.Stack())
			_ = s.st.CompleteJob(fin, job.ID, fmt.Sprintf("panic: %v", r))
			run = nil
		}
	}()
	hbCtx, stop := context.WithCancel(ctx)
	defer stop()
	go s.heartbeat(hbCtx, job.ID, workerID)

	switch job.Kind {
	case model.JobRunScenario, model.JobChromiumConfirm, model.JobValidateCandidate, model.JobChromiumEvidence:
		return s.runScenarioJob(ctx, job)
	case model.JobAgentDiscover, model.JobAgentVerify, model.JobAgentRepair:
		if s.orch == nil {
			_ = s.st.CompleteJob(fin, job.ID, "no orchestrator configured for agent jobs")
			return nil
		}
		started := s.Now()
		err := s.orch.HandleAgentJob(ctx, job)
		_ = s.st.RecordBudget(fin, s.project(), "agent", 1)
		if err != nil {
			s.logf("worker %s: agent job %d (%s %s) failed after %s: %v", workerID, job.ID, job.Kind, job.FeatureID, s.Now().Sub(started).Round(time.Second), err)
			_ = s.st.CompleteJob(fin, job.ID, err.Error())
			return nil
		}
		s.logf("worker %s: agent job %d (%s %s) done in %s", workerID, job.ID, job.Kind, job.FeatureID, s.Now().Sub(started).Round(time.Second))
		_ = s.st.CompleteJob(fin, job.ID, "")
		return nil
	default:
		_ = s.st.CompleteJob(fin, job.ID, "unknown job kind "+string(job.Kind))
		return nil
	}
}

func (s *Scheduler) heartbeat(ctx context.Context, jobID int64, workerID string) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.st.HeartbeatJob(ctx, jobID, workerID, leaseDuration); err != nil && ctx.Err() == nil {
				s.logf("worker %s: heartbeat job %d: %v", workerID, jobID, err)
			}
		}
	}
}

// runScenarioJob is the deterministic run worker flow (PRD §5.2).
func (s *Scheduler) runScenarioJob(ctx context.Context, job *model.Job) *model.Run {
	fin := context.WithoutCancel(ctx)
	backoff := func() {
		_ = s.st.SetScenarioNextDue(fin, s.project(), job.ScenarioID, s.Now().Add(s.cfg.Schedule.FailureBackoff.Duration))
	}
	fail := func(msg string) *model.Run {
		s.logf("job %d %s: %s", job.ID, job.ScenarioID, msg)
		_ = s.st.CompleteJob(fin, job.ID, msg)
		backoff() // do not re-enqueue a broken scenario on every tick
		return nil
	}
	m, v, err := s.st.GetCurrentScenarioVersion(fin, s.project(), job.ScenarioID)
	if err != nil {
		return fail("load scenario: " + err.Error())
	}
	sc, err := dsl.Parse([]byte(v.YAML))
	if err != nil {
		return fail("parse scenario v" + fmt.Sprint(v.Version) + ": " + err.Error())
	}
	flows := s.loadFlows(fin)

	locks := uniqStrings(append(append([]string{}, m.Locks...), sc.Resources.Locks...))
	owner := fmt.Sprintf("job-%d", job.ID)
	ok, err := s.st.TryAcquireLocks(fin, locks, owner, s.cfg.Browser.RunTimeout.Duration*2)
	if err != nil {
		return fail("acquire locks: " + err.Error())
	}
	if !ok {
		at := s.Now().Add(lockRequeueDelay)
		requeued, err := s.st.RequeueJob(fin, job.ID, at, "lock conflict: "+strings.Join(locks, ","))
		if err != nil {
			s.logf("job %d %s: requeue after lock conflict: %v", job.ID, m.ID, err)
		}
		s.logf("job %d %s: locks %v held elsewhere; requeued=%v at %s (AC-16)", job.ID, m.ID, locks, requeued, at.Format(time.RFC3339))
		if !requeued {
			backoff()
		}
		return nil
	}
	defer func() {
		if err := s.st.ReleaseLocks(fin, locks, owner); err != nil {
			s.logf("job %d %s: release locks: %v", job.ID, m.ID, err)
		}
	}()

	mutation := model.Mutation(sc.Scenario.Mutation)
	if mutation == "" {
		mutation = m.Mutation
	}
	if mutation == "" {
		mutation = model.MutationReadOnly
	}
	featureID, sha := s.resolveFeature(fin, job, m)

	if mutation == model.MutationDestructive && !s.cfg.Policy.AllowDestructive {
		reason := "destructive mutation requires policy.allow_destructive"
		s.logf("job %d %s: %s → NEEDS_REVIEW without running", job.ID, m.ID, reason)
		res := &runner.Result{Passed: false, Class: runner.FailNone, Browser: job.Browser, StartedAt: s.Now(), FinishedAt: s.Now(), Error: reason}
		run := s.persistRun(fin, job, m, v, featureID, sha, res, model.OutcomeNeedsReview, reason, 0, "")
		_ = s.st.SetScenarioState(fin, s.project(), m.ID, model.StateNeedsReview)
		s.afterRun(fin, job, run, res)
		_ = s.st.CompleteJob(fin, job.ID, "")
		return run
	}
	if s.run == nil {
		return fail("no runner configured")
	}

	browser := s.pickBrowser(job, sc)
	persona := personaMap(s.cfg.Personas[sc.Preconditions.Persona])
	var res *runner.Result
	var dir string
	attempt, prevFailed := 0, false
	for {
		attempt++
		started := s.Now()
		if dir, err = s.ev.RunDir(m.ID, started, attempt); err != nil {
			s.logf("job %d %s: evidence dir: %v", job.ID, m.ID, err)
			dir = ""
		}
		spec := runner.Spec{
			ProjectID:        s.project(),
			Scenario:         sc,
			Flows:            flows,
			Browser:          browser,
			BaseURL:          s.cfg.Target.BaseURL,
			Persona:          persona,
			EvidenceDir:      dir,
			StepTimeout:      s.cfg.Browser.StepTimeout.Duration,
			RunTimeout:       s.cfg.Browser.RunTimeout.Duration,
			CaptureDOMOnFail: true,
		}
		runCtx, cancel := context.WithTimeout(ctx, s.cfg.Browser.RunTimeout.Duration+30*time.Second)
		res, err = s.run.Run(runCtx, spec)
		cancel()
		if err != nil || res == nil {
			msg := "runner returned no result"
			if err != nil {
				msg = err.Error()
			}
			res = &runner.Result{Passed: false, Class: runner.FailInternal, Browser: browser, StartedAt: started, FinishedAt: s.Now(), Error: msg}
		}
		if res.StartedAt.IsZero() {
			res.StartedAt = started
		}
		if res.FinishedAt.IsZero() {
			res.FinishedAt = s.Now()
		}
		if res.Passed || attempt > 1 || mutation != model.MutationReadOnly || s.cfg.Policy.RetryOnFail <= 0 || ctx.Err() != nil {
			break
		}
		prevFailed = true
		s.logf("job %d %s: attempt 1 failed (%s); cheap safe retry", job.ID, m.ID, res.Class)
	}

	healthy := true
	if !res.Passed && s.TargetHealthy != nil {
		healthy = s.TargetHealthy(ctx)
	}
	outcome, reason := s.Classify(classify.Input{
		Result:        res,
		Browser:       browser,
		Attempt:       attempt,
		RetriedOnce:   attempt > 1,
		PrevFailed:    prevFailed,
		TargetHealthy: healthy,
	})
	run := s.persistRun(fin, job, m, v, featureID, sha, res, outcome, reason, attempt, dir)
	s.enqueueEvidenceCapture(fin, job, run)
	s.afterRun(fin, job, run, res)
	_ = s.st.CompleteJob(fin, job.ID, "")
	s.logf("job %d %s v%d %s → %s%s %dms attempt=%d evidence=%s", job.ID, m.ID, v.Version, browser, outcome, parens(reason), run.DurationMs, attempt, dir)
	return run
}

// persistRun records the run, artifacts, PASS marker, budget, scenario counters and next due.
func (s *Scheduler) persistRun(ctx context.Context, job *model.Job, m *model.Scenario, v *model.ScenarioVersion, featureID, sha string, res *runner.Result, outcome model.Outcome, reason string, attempt int, dir string) *model.Run {
	run := &model.Run{
		JobID:           job.ID,
		ProjectID:       s.project(),
		ScenarioID:      m.ID,
		ScenarioVersion: v.Version,
		FeatureID:       featureID,
		ShippedSHA:      sha,
		Browser:         res.Browser,
		Outcome:         outcome,
		Attempt:         attempt,
		StartedAt:       res.StartedAt,
		FinishedAt:      res.FinishedAt,
		DurationMs:      res.Duration().Milliseconds(),
		Error:           res.Error,
		EvidenceDir:     dir,
		DeployMarker:    s.currentMarker(ctx),
	}
	if run.Browser == "" {
		run.Browser = job.Browser
	}
	if fs := res.FailedStep; fs != nil {
		run.FailedStep = fs.Index
		run.FailedAction = fs.Kind
		run.Expected = fs.Expected
		run.Actual = fs.Actual
		if run.Error == "" {
			run.Error = fs.Error
		}
	}
	if len(res.GlobalAssertionFailures) > 0 {
		if run.FailedAction == "" {
			run.FailedAction = "assert"
		}
		if run.Actual == "" {
			run.Actual = strings.Join(res.GlobalAssertionFailures, "; ")
		}
	}
	if run.Error == "" && reason != "" && outcome != model.OutcomePass {
		run.Error = reason
	}
	if _, err := s.st.InsertRun(ctx, run); err != nil {
		s.logf("job %d %s: insert run: %v", job.ID, m.ID, err)
	} else {
		for kind, p := range res.Artifacts {
			if p != "" {
				_ = s.st.AddRunArtifact(ctx, run.ID, kind, p)
			}
		}
		if res.DOMPath != "" {
			_ = s.st.AddRunArtifact(ctx, run.ID, "dom", res.DOMPath)
		}
		if res.ScreenshotPath != "" {
			_ = s.st.AddRunArtifact(ctx, run.ID, "screenshot", res.ScreenshotPath)
		}
	}
	if dir != "" {
		s.writeRunSummary(dir, run, reason)
		if outcome == model.OutcomePass {
			if err := evidence.MarkPass(dir); err != nil {
				s.logf("job %d %s: pass marker: %v", job.ID, m.ID, err)
			}
		}
	}
	if run.DurationMs > 0 {
		_ = s.st.RecordBudget(ctx, s.project(), "browser", run.DurationMs)
		if run.Browser == model.BrowserChromium {
			_ = s.st.RecordBudget(ctx, s.project(), "chromium", run.DurationMs)
		}
	}
	refreshed, err := s.st.RecordScenarioOutcome(ctx, s.project(), m.ID, outcome, run.FinishedAt)
	if err != nil {
		s.logf("job %d %s: record outcome: %v", job.ID, m.ID, err)
		refreshed = m
	}
	s.scheduleNext(ctx, refreshed, outcome)
	return run
}

// scheduleNext sets the cadence-based next due time (the orchestrator's
// AfterRun may override it; this keeps the loop sane when it cannot).
func (s *Scheduler) scheduleNext(ctx context.Context, m *model.Scenario, outcome model.Outcome) {
	var d time.Duration
	switch outcome {
	case model.OutcomePass, model.OutcomeQAFlake:
		if m.State == model.StateSoak {
			d = s.cfg.Schedule.Soak.Duration
		} else {
			switch classOf(m) {
			case "P0":
				d = s.cfg.Schedule.P0.Duration
			case "P2":
				d = s.cfg.Schedule.P2.Duration
			default:
				d = s.cfg.Schedule.P1.Duration
			}
		}
	default:
		d = s.cfg.Schedule.FailureBackoff.Duration
	}
	if d <= 0 {
		d = time.Minute
	}
	if err := s.st.SetScenarioNextDue(ctx, s.project(), m.ID, s.Now().Add(d)); err != nil {
		s.logf("%s: next due: %v", m.ID, err)
	}
}

func (s *Scheduler) afterRun(ctx context.Context, job *model.Job, run *model.Run, res *runner.Result) {
	if s.orch == nil {
		return
	}
	if err := s.orch.AfterRun(ctx, job, run, res); err != nil {
		s.logf("job %d %s: orchestrator.AfterRun: %v", job.ID, run.ScenarioID, err)
	}
}

func (s *Scheduler) pickBrowser(job *model.Job, sc *dsl.Scenario) model.Browser {
	if job.Kind == model.JobChromiumConfirm || job.Kind == model.JobChromiumEvidence || sc.Browser.RequiresChromium {
		return model.BrowserChromium
	}
	if job.Browser != "" {
		return job.Browser
	}
	if sc.Browser.Primary != "" {
		return model.Browser(sc.Browser.Primary)
	}
	return model.Browser(s.cfg.Browser.Primary)
}

// resolveFeature finds the feature/SHA a run is evidence for: the job's feature,
// else the scenario's feature coverage link (AC-18: runs carry the shipped SHA).
func (s *Scheduler) resolveFeature(ctx context.Context, job *model.Job, m *model.Scenario) (featureID, sha string) {
	featureID = job.FeatureID
	if featureID == "" {
		links, _ := s.st.ListCoverageLinks(ctx, s.project(), m.ID)
		for _, l := range links {
			if l.LinkType == model.LinkFeature {
				featureID = l.LinkValue
				break
			}
		}
	}
	if featureID == "" {
		return "", ""
	}
	f, err := s.st.GetFeature(ctx, s.project(), featureID)
	if err != nil {
		return featureID, ""
	}
	return f.ID, f.LatestShippedSHA
}

func (s *Scheduler) loadFlows(ctx context.Context) map[string]*dsl.Flow {
	flows := map[string]*dsl.Flow{}
	ys, err := s.st.ListFlowYAML(ctx, s.project())
	if err != nil {
		s.logf("flows: %v", err)
		return flows
	}
	for id, y := range ys {
		f, err := dsl.ParseFlow([]byte(y))
		if err != nil {
			s.logf("flow %s: %v", id, err)
			continue
		}
		flows[id] = f
	}
	return flows
}

func (s *Scheduler) writeRunSummary(dir string, run *model.Run, reason string) {
	doc := map[string]any{
		"scenario_id":      run.ScenarioID,
		"scenario_version": run.ScenarioVersion,
		"feature_id":       run.FeatureID,
		"shipped_sha":      run.ShippedSHA,
		"browser":          run.Browser,
		"outcome":          run.Outcome,
		"reason":           reason,
		"attempt":          run.Attempt,
		"started_at":       run.StartedAt,
		"finished_at":      run.FinishedAt,
		"duration_ms":      run.DurationMs,
		"failed_step":      run.FailedStep,
		"failed_action":    run.FailedAction,
		"expected":         run.Expected,
		"actual":           run.Actual,
		"error":            run.Error,
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
}

// probeTarget is the default TargetHealthy: a HEAD of base_url answered by the origin.
func (s *Scheduler) probeTarget(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.cfg.Target.BaseURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// ---- inline execution (CLI) ------------------------------------------------------

// RunJobNow leases the given READY job and executes it synchronously (used by `run`/`discover`).
func (s *Scheduler) RunJobNow(ctx context.Context, jobID int64) error {
	job, err := s.claimJobByID(ctx, jobID, inlineWorker)
	if err != nil {
		return err
	}
	s.execute(ctx, inlineWorker, job)
	return nil
}

// RunScenarioNow enqueues (dedup) and executes one scenario inline, returning the recorded run.
func (s *Scheduler) RunScenarioNow(ctx context.Context, scenarioID string, browser model.Browser) (*model.Run, error) {
	m, err := s.st.GetScenario(ctx, s.project(), scenarioID)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", scenarioID, err)
	}
	if browser == "" {
		browser = s.scenarioBrowser(ctx, m)
	}
	id, created, err := s.EnqueueScenario(ctx, m, model.PriorityRecentFailure, browser, "")
	if err != nil {
		return nil, err
	}
	if !created {
		s.logf("run: reusing queued job %d for %s", id, scenarioID)
	}
	job, err := s.claimJobByID(ctx, id, inlineWorker)
	if err != nil {
		return nil, err
	}
	run := s.execute(ctx, inlineWorker, job)
	if run != nil {
		// no workers here: run the follow-up Chromium evidence capture inline as well
		if jobs, err := s.st.ListJobs(ctx, s.project(), []model.JobState{model.JobReady}, 200); err == nil {
			for _, j := range jobs {
				if j.Kind == model.JobChromiumEvidence && j.ScenarioID == scenarioID {
					if ej, err := s.claimJobByID(ctx, j.ID, inlineWorker); err == nil {
						if er := s.execute(ctx, inlineWorker, ej); er != nil {
							run = er
						}
					}
				}
			}
		}
	}
	if run == nil {
		jobs, _ := s.st.ListJobs(ctx, s.project(), []model.JobState{model.JobFailed, model.JobReady}, 500)
		for _, j := range jobs {
			if j.ID == id {
				return nil, fmt.Errorf("job %d did not produce a run (%s: %s)", id, j.State, j.LastError)
			}
		}
		return nil, fmt.Errorf("job %d did not produce a run", id)
	}
	return run, nil
}

// claimJobByID leases one specific READY job (the store's ClaimJob picks by priority).
func (s *Scheduler) claimJobByID(ctx context.Context, id int64, worker string) (*model.Job, error) {
	now := s.Now()
	res, err := s.st.DB().ExecContext(ctx, `UPDATE jobs SET state='LEASED', lease_owner=?, lease_expires_at=?, attempt=attempt+1, updated_at=? WHERE id=? AND state='READY'`,
		worker, now.Add(leaseDuration).UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("job %d is not READY (already running, finished or missing)", id)
	}
	jobs, err := s.st.ListJobs(ctx, s.project(), []model.JobState{model.JobLeased}, 1000)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, fmt.Errorf("job %d vanished after lease", id)
}

// ---- helpers ------------------------------------------------------------------------

func personaMap(p config.Persona) map[string]string {
	m := map[string]string{}
	if p.Username != "" {
		m["username"] = p.Username
	}
	if p.Password != "" {
		m["password"] = p.Password
	}
	for k, v := range p.Extra {
		m[k] = v
	}
	return m
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// currentMarker tags a run with the deployed build it verified when the gate can tell.
func (s *Scheduler) currentMarker(ctx context.Context) string {
	cm, ok := s.gate.(interface {
		CurrentMarker(context.Context) (string, error)
	})
	if !ok || s.gate == nil {
		return ""
	}
	mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m, err := cm.CurrentMarker(mctx)
	if err != nil {
		return ""
	}
	return m
}

// enqueueEvidenceCapture schedules one Chromium run per (scenario, deployment) after a
// Lightpanda PASS so the verification report carries a real rendered screenshot
// (Lightpanda does not paint; its captures are text renderings). Background priority.
func (s *Scheduler) enqueueEvidenceCapture(ctx context.Context, job *model.Job, run *model.Run) {
	if run == nil || s.cfg.Evidence.ChromiumCapture == "off" || run.DeployMarker == "" || run.Browser != model.BrowserLightpanda || run.Outcome != model.OutcomePass {
		return
	}
	if job.Kind == model.JobChromiumEvidence || job.Kind == model.JobChromiumConfirm {
		return
	}
	has, err := s.st.HasRunForDeployment(ctx, s.project(), run.ScenarioID, run.DeployMarker, model.BrowserChromium)
	if err != nil || has {
		return
	}
	id, created, err := s.st.EnqueueJob(ctx, &model.Job{
		ProjectID: s.project(), Kind: model.JobChromiumEvidence, Priority: model.PriorityBackground, Browser: model.BrowserChromium,
		ScenarioID: run.ScenarioID, FeatureID: run.FeatureID, MaxAttempts: 1, Payload: job.Payload,
	}, "evidence:"+run.ScenarioID+":"+run.DeployMarker)
	if err == nil && created {
		s.logf("evidence: queued Chromium capture job %d for %s on deployment %s", id, run.ScenarioID, run.DeployMarker)
	}
}
