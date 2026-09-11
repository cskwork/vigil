package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"vigil/internal/agent"
	"vigil/internal/approval"
	"vigil/internal/config"

	"vigil/internal/dsl"
	"vigil/internal/explain"
	"vigil/internal/ingest"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/scheduler"
	"vigil/internal/store"
	"vigil/internal/ui"
)

// ---- init ---------------------------------------------------------------------------

const starterConfig = `# vigil starter config (schema version 4). Edit url.md first: its first URL
# becomes target.base_url and its host is allowlisted automatically.
version: 4

runtime:
  mode: local

project:
  id: my-web-app
  repo: ../my-web-app              # git checkout of the deployed application

target:
  url_file: url.md                 # first URL = primary target; its host is allowlisted automatically
  base_url: https://dev.example.com
  allowed_hosts: [dev.example.com]

discovery:
  adapter: generic-git             # generic-git | sdlc-kit | file
  branch: origin/main
  fetch: true
  path_prefixes: [src/]
  route_map:
    - { path_prefix: src/, route: /, capability: app }
  poll_interval: 120s
  history_limit: 3

deployment:
  readiness:
    strategy: delay                # delay | asset_version | version_endpoint
    delay_after_ship: 2m
    max_wait: 30m

browser:
  primary: lightpanda
  fallback: chromium
  lightpanda: { binary: bin/lightpanda, host: 127.0.0.1, port: 9333 }
  chromium: { binary: "", headless: true }   # empty = auto-detect
  step_timeout: 15s
  run_timeout: 3m

workers:
  functional: 1

state: { type: sqlite, path: .vigil/state.db }
evidence:
  type: local
  dir: evidence/
  retain_pass_days: 3              # pass evidence
  retain_fail_days: 30             # failure + agent evidence
  max_total_mb: 1024               # cap; oldest pass runs -> agent runs -> failures are deleted (newest per scenario/feature kept)
  retain_runs_days: 90             # run rows in the database (incident-linked and newest-per-scenario kept)
  chromium_capture: per-deploy     # one Chromium re-run per deployment for a real screenshot after a Lightpanda pass

budget:
  browser_minutes_per_hour: 60
  chromium_minutes_per_hour: 5
  agent_tasks_per_hour: 4

policy:
  allow_destructive: false
  soak_passes: 3
  quarantine_after: 3
  retry_on_fail: 1
  observation_oracle: needs_review

schedule: { soak: 10m, p0: 15m, p1: 60m, p2: 6h, failure_backoff: 5m, tick: 10s }

agent:
  provider: pi
  model: zai/glm-5.3-flash
  thinking: high
  timeout: 20m                     # on timeout the session is resumed once with WRAP UP
  extension: ""                    # empty = bundled piext/vigil-browser.js
  env_map: { ZAI_API_KEY: Z_AI_API_KEY }
  retries: 3                       # whole-task retries on provider errors
  backoff: 60s
  sandbox: auto                    # nono when on PATH, else unsandboxed
  max_scenarios_per_task: 3
  max_turns: 40                    # tool-call budget per task
  continuations: 2                 # session resumes with a fresh budget before WRAP UP
  model_retries: 10                # in-conversation 429/5xx retries (3s, 6s, 12s ...)
  model_retry_delay: 3s

personas:
  qa_user:
    username: ${QA_USER}
    password: ${QA_PASSWORD}

paths: { scenarios: scenarios, flows: flows }
`

const starterURLFile = `# vigil targets
#
# One deployed URL per line. The first URL is the primary target:
#   - its origin becomes target.base_url (and its host is auto-allowlisted)
#   - its path becomes the default entry route for seed scenarios and agent discovery
# Format: <url>  or  <label> | <url>. Lines starting with # are ignored.

home | https://dev.example.com/
`

func (a *app) cmdInit() error {
	wrote := false
	if _, err := os.Stat(a.cfgPath); err == nil {
		a.printf("%s already exists; leaving it untouched\n", a.cfgPath)
	} else {
		if err := os.WriteFile(a.cfgPath, []byte(starterConfig), 0o644); err != nil {
			return err
		}
		a.printf("wrote %s\n", a.cfgPath)
		wrote = true
	}
	urlPath := filepath.Join(filepath.Dir(a.cfgPath), "url.md")
	if _, err := os.Stat(urlPath); err != nil {
		if err := os.WriteFile(urlPath, []byte(starterURLFile), 0o644); err != nil {
			return err
		}
		a.printf("wrote %s\n", urlPath)
		wrote = true
	}
	for _, d := range []string{"scenarios", "flows", "features"} {
		_ = os.MkdirAll(filepath.Join(filepath.Dir(a.cfgPath), d), 0o755)
	}
	if wrote {
		a.printf("next: edit url.md + project.repo, then `vigil doctor`, `vigil add`, `vigil scan`, `vigil loop`\n")
	}
	return nil
}

// ---- request (manual QA request → orchestrator) ------------------------------------

// manualRequest is the on-disk shape of `vigil request <file.yaml>`.
type manualRequest struct {
	FeatureID        string   `yaml:"feature_id"`
	Summary          string   `yaml:"summary"`
	EntryURL         string   `yaml:"entry_url"`
	Routes           []string `yaml:"routes"`
	Accounts         []string `yaml:"accounts"`
	Instructions     string   `yaml:"instructions"`
	Mutation         string   `yaml:"mutation"` // read-only | reversible | destructive
	Locks            []string `yaml:"locks"`
	MaxToolCalls     int      `yaml:"max_tool_calls"`
	TimeoutMinutes   int      `yaml:"timeout_minutes"`
	MaxContinuations int      `yaml:"max_continuations"`
}

func (a *app) cmdRequest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	queue := fs.Bool("queue", false, "only enqueue; let a running `loop` execute it")
	dry := fs.Bool("dry-run", false, "print the agent request and exit")
	// accept flags before or after the file argument
	var flags, positional []string
	for _, x := range args {
		if strings.HasPrefix(x, "-") {
			flags = append(flags, x)
		} else {
			positional = append(positional, x)
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: vigil request <file.yaml> [--queue] [--dry-run]")
	}
	raw, err := os.ReadFile(positional[0])
	if err != nil {
		return err
	}
	var mr manualRequest
	if err := yaml.Unmarshal(raw, &mr); err != nil {
		return fmt.Errorf("%s: %w", positional[0], err)
	}
	if strings.TrimSpace(mr.FeatureID) == "" {
		return errors.New("request needs feature_id and instructions")
	}
	orch := a.buildOrchestrator(nil, nil)
	in, err := orch.NormalizeManualRequest(orchestrator.ManualRequest{
		FeatureID: mr.FeatureID, Summary: mr.Summary, EntryURL: mr.EntryURL, Routes: mr.Routes,
		Accounts: mr.Accounts, Instructions: mr.Instructions, Mutation: mr.Mutation, Locks: mr.Locks,
		MaxToolCalls: mr.MaxToolCalls, TimeoutMinutes: mr.TimeoutMinutes, MaxContinuations: mr.MaxContinuations,
	})
	if err != nil {
		return err
	}
	if *dry {
		req := &agent.Request{Summary: in.Summary, EntryURL: in.EntryURL, Routes: in.Routes, Accounts: in.Accounts, Instructions: in.Instructions,
			Mutation: in.Mutation, Locks: in.Locks, MaxToolCalls: in.MaxToolCalls, TimeoutMinutes: in.TimeoutMinutes, MaxContinuations: in.MaxContinuations}
		b, _ := yaml.Marshal(req)
		a.printf("feature %s\n%s", in.FeatureID, b)
		return nil
	}
	result, err := orch.RegisterManualRequest(ctx, in)
	if err != nil {
		return err
	}
	if !result.Created {
		a.printf("reusing queued request job %d\n", result.JobID)
	}
	a.printf("request %s → job %d (mutation=%s accounts=%v max_tool_calls=%d)\n", result.FeatureID, result.JobID, in.Mutation, in.Accounts, in.MaxToolCalls)
	if *queue {
		a.printf("queued; a running `vigil loop` will execute it\n")
		return nil
	}
	run := a.buildRunner()
	ag := a.buildAgent()
	if ag == nil {
		return errors.New("request needs the Browser Agent; see `vigil doctor`")
	}
	orch = a.buildOrchestrator(run, ag)
	s := a.buildScheduler(orch, run, false, true)
	if err := s.RunJobNow(ctx, result.JobID); err != nil {
		return err
	}
	return a.printJobResult(ctx, result.JobID)
}

// cmdPrune applies the retention policy immediately and prints what it did.
func (a *app) cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "report only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e := a.cfg.Evidence
	rep, err := a.ev.PruneWithCap(e.RetainPassDays, e.RetainFailDays, e.MaxTotalMB, *dry)
	if err != nil {
		return err
	}
	mode := "pruned"
	if *dry {
		mode = "would prune"
	}
	a.printf("%s %d dir(s), %.1f MB; evidence now %.1f MB (%d runs, %d agent runs); policy pass %dd / fail %dd / cap %d MB\n",
		mode, rep.DeletedDirs, float64(rep.FreedBytes)/1048576, float64(rep.TotalBytes)/1048576, rep.RunDirs, rep.AgentDirs, e.RetainPassDays, e.RetainFailDays, e.MaxTotalMB)
	if !*dry && e.RetainRunsDays > 0 {
		n, err := a.st.DeleteRunsBefore(context.Background(), a.cfg.Project.ID, time.Now().Add(-time.Duration(e.RetainRunsDays)*24*time.Hour))
		if err != nil {
			return err
		}
		a.printf("db: deleted %d run row(s) older than %d days\n", n, e.RetainRunsDays)
	}
	return nil
}

// cmdReparse re-reads an agent transcript with the current (more tolerant) parser.
func (a *app) cmdReparse(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: vigil reparse <evidence/agent/<feature>/<ts>>")
	}
	ag := a.buildAgent()
	if ag == nil {
		return errors.New("reparse needs the Browser Agent adapter; see `vigil doctor`")
	}
	res, err := ag.Reparse(a.cfg.Abs(args[0]))
	if err != nil {
		return err
	}
	a.printf("decision=%s candidates=%d tool_calls=%d evidence=%d chars\n", res.Decision, len(res.ScriptCandidates), res.ToolCalls, len(res.Evidence))
	return nil
}

// ---- add / scan / import / discover -------------------------------------------------

func (a *app) importFiles(ctx context.Context) {
	orch := a.buildOrchestrator(nil, nil)
	n, err := orch.ImportScenarioFiles(ctx)
	if err != nil {
		a.log.Printf("warn: import scenario files: %v", err)
		return
	}
	a.printf("imported %d scenario/flow file(s) from %s and %s\n", n, a.cfg.Paths.Scenarios, a.cfg.Paths.Flows)
}

func (a *app) cmdAdd(ctx context.Context, args []string) error {
	id := a.cfg.Project.ID
	if len(args) > 0 && args[0] != "" {
		id = args[0]
	}
	if id != a.cfg.Project.ID {
		return fmt.Errorf("project %q differs from vigil.yaml project.id %q; edit the config instead", id, a.cfg.Project.ID)
	}
	if err := a.st.UpsertProject(ctx, id, a.cfg.Target.BaseURL); err != nil {
		return err
	}
	a.printf("project %s → %s (hosts %v)\n", id, a.cfg.Target.BaseURL, a.cfg.Target.AllowedHosts)
	if t, ok := a.cfg.PrimaryTarget(); ok {
		a.printf("primary target: %s %s\n", t.Label, t.URL)
	}
	a.importFiles(ctx)
	return nil
}

func (a *app) cmdScan(ctx context.Context) error {
	if err := a.st.UpsertProject(ctx, a.cfg.Project.ID, a.cfg.Target.BaseURL); err != nil {
		return err
	}
	a.importFiles(ctx)
	run := a.buildRunner()
	orch := a.buildOrchestrator(run, nil)
	s := a.buildScheduler(orch, run, true, false)
	if err := s.ScanOnce(ctx); err != nil {
		return err
	}
	feats, err := a.st.ListFeatures(ctx, a.cfg.Project.ID)
	if err != nil {
		return err
	}
	a.printf("%-28s %-5s %-14s %-9s %-24s %-8s %s\n", "FEATURE", "KIND", "REF", "SHA", "READINESS", "HANDLED", "SUMMARY")
	for _, f := range feats {
		handled := "no"
		if f.LastHandledSHA == f.LatestShippedSHA {
			handled = "yes"
		}
		a.printf("%-28s %-5s %-14s %-9s %-24s %-8s %s\n", f.ID, featureKind(f), trunc(f.Ref, 14), short(f.LatestShippedSHA), f.Readiness, handled, trunc(f.Summary, 60))
	}
	c, _ := a.st.Counts(ctx, a.cfg.Project.ID)
	if c != nil {
		a.printf("jobs: %v\n", c.Jobs)
	}
	return nil
}

func (a *app) cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	history := fs.Bool("history", false, "ingest the last discovery.history_limit features")
	limit := fs.Int("limit", 0, "override history_limit")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if !*history {
		return errors.New("usage: vigil import --history [--limit N]")
	}
	if err := a.st.UpsertProject(ctx, a.cfg.Project.ID, a.cfg.Target.BaseURL); err != nil {
		return err
	}
	in, err := ingest.New(a.cfg, a.st)
	if err != nil {
		return err
	}
	n := a.cfg.Discovery.HistoryLimit
	if *limit > 0 {
		n = *limit
	}
	events, err := in.History(ctx, n)
	if err != nil {
		return err
	}
	created := 0
	for _, ev := range events {
		if ev.Source == "" {
			ev.Source = in.Name()
		}
		isNew, err := a.st.UpsertFeature(ctx, a.cfg.Project.ID, ev)
		if err != nil {
			return err
		}
		if isNew {
			created++
		}
		a.printf("%-28s %-5s %-14s %-9s %s  %s\n", ev.FeatureID, orDefault(ev.Kind, model.FeatureKindShip), trunc(ev.Ref, 14), short(ev.ShippedSHA), ev.ShippedAt.UTC().Format(time.RFC3339), trunc(ev.Summary, 70))
	}
	a.printf("history: %d event(s) from %s (limit %d), %d new; gate/plan happens on the next `scan`/`loop`\n", len(events), in.Name(), n, created)
	return nil
}

func (a *app) cmdDiscover(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: vigil discover <feature>")
	}
	featureID := args[0]
	f, err := a.st.GetFeature(ctx, a.cfg.Project.ID, featureID)
	if err != nil {
		return fmt.Errorf("feature %s: %w (run `scan` or `import --history` first)", featureID, err)
	}
	run := a.buildRunner()
	ag := a.buildAgent()
	if ag == nil {
		return errors.New("discover needs the Browser Agent; see `vigil doctor`")
	}
	orch := a.buildOrchestrator(run, ag)
	s := a.buildScheduler(orch, run, false, true)
	id, created, err := a.st.EnqueueJob(ctx, &model.Job{
		ProjectID: a.cfg.Project.ID, Kind: model.JobAgentDiscover, Priority: model.PriorityNewDirectCoverage,
		FeatureID: f.ID, MaxAttempts: 1,
	}, "agent:discover:"+f.ID)
	if err != nil {
		return err
	}
	if !created {
		a.printf("reusing queued discovery job %d\n", id)
	}
	a.printf("discover %s (sha %s) → job %d\n", f.ID, short(f.LatestShippedSHA), id)
	if err := s.RunJobNow(ctx, id); err != nil {
		return err
	}
	return a.printJobResult(ctx, id)
}

func (a *app) printJobResult(ctx context.Context, id int64) error {
	jobs, err := a.st.ListJobs(ctx, a.cfg.Project.ID, nil, 1000)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.ID == id {
			a.printf("job %d %s → %s %s\n", j.ID, j.Kind, j.State, j.LastError)
			if j.State == model.JobFailed {
				return fmt.Errorf("job %d failed: %s", j.ID, j.LastError)
			}
			return nil
		}
	}
	return nil
}

// ---- run ----------------------------------------------------------------------------

// envRefuses checks every scenario's mutation class against the environment
// before anything runs, so a read-only environment fails fast (exit 2).
func (a *app) envRefuses(ctx context.Context, env config.Environment, ids []string) error {
	if !env.ReadOnly {
		return nil
	}
	for _, id := range ids {
		m, err := a.st.GetScenario(ctx, a.cfg.Project.ID, id)
		if err != nil {
			return fmt.Errorf("scenario %s: %w", id, err)
		}
		if err := config.EnvAllows(env, string(m.Mutation)); err != nil {
			return fmt.Errorf("--env %s: scenario %s: %w", env.Name, id, err)
		}
	}
	return nil
}

func (a *app) cmdRun(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	impacted := fs.String("impacted", "", "run scenarios impacted by this feature")
	feature := fs.String("feature", "", "run scenarios covering this feature")
	all := fs.Bool("all", false, "run every ACTIVE/SOAK scenario")
	browserFlag := fs.String("browser", "", "lightpanda | chromium")
	envFlag := fs.String("env", "", "target environment (target.environments name; default: target.default_env)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2, err
	}
	switch model.Browser(*browserFlag) {
	case "", model.BrowserLightpanda, model.BrowserChromium:
	default:
		return 2, fmt.Errorf("--browser must be lightpanda or chromium, got %q", *browserFlag)
	}
	env, err := a.cfg.Env(*envFlag)
	if err != nil {
		return 2, fmt.Errorf("--env: %w", err)
	}
	var ids []string
	switch {
	case *all:
		list, err := a.st.ListScenarios(ctx, a.cfg.Project.ID, model.StateActive, model.StateSoak)
		if err != nil {
			return 1, err
		}
		for _, m := range list {
			ids = append(ids, m.ID)
		}
	case *feature != "":
		ids, err = a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkFeature, []string{*feature}, runnableStates)
		if err != nil {
			return 1, err
		}
	case *impacted != "":
		ids, err = a.impactedScenarios(ctx, *impacted)
		if err != nil {
			return 1, err
		}
	case len(pos) == 1:
		if _, err := a.st.GetScenario(ctx, a.cfg.Project.ID, pos[0]); err == nil {
			ids = []string{pos[0]}
		} else if _, ferr := a.st.GetFeature(ctx, a.cfg.Project.ID, pos[0]); ferr == nil {
			ids, err = a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkFeature, []string{pos[0]}, runnableStates)
			if err != nil {
				return 1, err
			}
		} else {
			return 1, fmt.Errorf("%q is neither a scenario nor a feature", pos[0])
		}
	default:
		return 2, errors.New("usage: vigil run <scenario|feature> | --feature <f> | --impacted <f> | --all  [--browser lightpanda|chromium] [--env <name>]")
	}
	if len(ids) == 0 {
		a.printf("nothing to run\n")
		return 0, nil
	}
	sort.Strings(ids)
	if err := a.envRefuses(ctx, env, ids); err != nil {
		return 2, err
	}
	run := a.buildRunner()
	orch := a.buildOrchestrator(run, nil)
	s := a.buildScheduler(orch, run, false, false)
	failed := 0
	for _, id := range ids {
		r, err := s.RunScenarioNowEnv(ctx, id, model.Browser(*browserFlag), env.Name)
		if err != nil {
			failed++
			a.printf("%-44s ERROR  %v\n", id, err)
			continue
		}
		mark := "PASS "
		if r.Outcome != model.OutcomePass {
			mark = "FAIL "
			if r.Outcome == model.OutcomeQAFlake {
				mark = "FLAKE"
			}
			failed++
		}
		a.printf("%-44s %s %-22s %-10s env=%s %6dms a%d %s\n", id, mark, r.Outcome, r.Browser, r.Environment, r.DurationMs, r.Attempt, r.EvidenceDir)
		if r.Outcome != model.OutcomePass {
			// One sentence on screen, the raw text in the log and behind --json.
			raw := fmt.Sprintf("step %d %s: expected=%q actual=%q %s", r.FailedStep, r.FailedAction, trunc(r.Expected, 60), trunc(r.Actual, 60), trunc(r.Error, 120))
			if r.Error != "" {
				a.log.Printf("run %s: %s", id, raw)
			}
			switch {
			case a.jsonOut && r.Error != "":
				a.printf("%-44s        %s\n", "", raw)
			default:
				if c := explain.ForRun(r, nil); !c.Empty() {
					a.printf("%-44s        %s\n", "", c.Headline)
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

var runnableStates = []model.ScenarioState{model.StateActive, model.StateSoak}

// impactedScenarios applies the PRD §12 impact signals (feature → capability → route → path).
func (a *app) impactedScenarios(ctx context.Context, featureID string) ([]string, error) {
	f, err := a.st.GetFeature(ctx, a.cfg.Project.ID, featureID)
	if err != nil {
		return nil, fmt.Errorf("feature %s: %w", featureID, err)
	}
	seen := map[string]bool{}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	ids, _ := a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkFeature, []string{f.ID}, runnableStates)
	add(ids)
	var caps []string
	for _, p := range f.ChangedPaths {
		for _, e := range a.cfg.Discovery.RouteMap {
			if e.Capability != "" && strings.HasPrefix(p, e.PathPrefix) {
				caps = append(caps, e.Capability)
			}
		}
	}
	ids, _ = a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkCapability, caps, runnableStates)
	add(ids)
	ids, _ = a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkRoute, f.Routes, runnableStates)
	add(ids)
	ids, _ = a.st.ScenariosLinkedTo(ctx, a.cfg.Project.ID, model.LinkPath, f.ChangedPaths, runnableStates)
	add(ids)
	return out, nil
}

// ---- loop ---------------------------------------------------------------------------

type loopRequestSubmitter struct {
	orch  *orchestrator.Orchestrator
	sched *scheduler.Scheduler
}

func (s loopRequestSubmitter) SubmitUserRequest(ctx context.Context, situation string) (string, int64, error) {
	featureID, jobID, err := s.orch.SubmitUserRequest(ctx, situation)
	if err != nil {
		return "", 0, err
	}
	if err := s.sched.PrioritizeAgentJob(ctx, jobID); err != nil {
		return featureID, jobID, fmt.Errorf("request queued but could not preempt current agent work: %w", err)
	}
	return featureID, jobID, nil
}

// loopScriptActions backs the dashboard's ▶ 실행 / ✔ 승인 / ✖ 반려 buttons when
// the scheduler runs in this process.
type loopScriptActions struct {
	svc   *approval.Service
	sched *scheduler.Scheduler
}

func (l loopScriptActions) CanRun() bool { return true }

func (l loopScriptActions) RunScript(ctx context.Context, sc *model.Scenario, env config.Environment, browser model.Browser) (int64, error) {
	id, _, err := l.sched.EnqueueScenarioEnv(ctx, sc, model.PriorityUserRequest, browser, "", env.Name)
	return id, err
}

func (l loopScriptActions) ApproveScript(ctx context.Context, id string) (approval.Receipt, error) {
	return l.svc.Approve(ctx, id, approval.Options{})
}

func (l loopScriptActions) RejectScript(ctx context.Context, id string) (approval.Receipt, error) {
	return l.svc.Reject(ctx, id)
}

func (a *app) cmdLoop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("loop", flag.ContinueOnError)
	uiAddr := fs.String("ui", "", "serve the read-only live view on this address (e.g. 127.0.0.1:8787)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.useLoopLogger(); err != nil {
		return err
	}
	if err := a.st.UpsertProject(ctx, a.cfg.Project.ID, a.cfg.Target.BaseURL); err != nil {
		return err
	}
	a.importFiles(ctx)
	run := a.buildRunner()
	ag := a.buildAgent()
	orch := a.buildOrchestrator(run, ag)
	s := a.buildScheduler(orch, run, true, ag != nil)
	if *uiAddr != "" {
		srv := ui.New(a.cfg, a.st, a.cfg.Abs(a.cfg.Evidence.Dir))
		if ag != nil {
			srv.SetRequestSubmitter(loopRequestSubmitter{orch: orch, sched: s})
		}
		srv.SetScriptActions(loopScriptActions{svc: a.approvalService(), sched: s})
		go func() {
			if err := srv.ListenAndServe(ctx, *uiAddr); err != nil {
				a.log.Printf("ui: %v", err)
			}
		}()
		a.log.Printf("ui: live view at http://%s/", *uiAddr)
	}
	a.log.Printf("loop: config=%s state=%s log=%s", a.cfgPath, a.cfg.Abs(a.cfg.State.Path), filepath.Join(a.stateDir(), "vigil.log"))
	a.log.Printf("loop: target=%s hosts=%v discovery=%s/%s readiness=%s primary=%s", a.cfg.Target.BaseURL, a.cfg.Target.AllowedHosts,
		strings.Join(a.cfg.AdapterNames(), ","), a.cfg.Discovery.Branch, a.cfg.Deployment.Readiness.Strategy, a.cfg.Browser.Primary)
	return s.Loop(ctx)
}

// cmdServe runs only the read-only live view (a `loop` may run in another process;
// SQLite WAL allows concurrent readers).
func (a *app) cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	devDir := fs.String("dev-ui", "", "serve the dashboard pages, theme.css and app.js from this directory (edit without restart)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := ui.New(a.cfg, a.st, a.cfg.Abs(a.cfg.Evidence.Dir))
	srv.SetScriptActions(ui.StoreScriptActions{Svc: a.approvalService()}) // approve/reject only; run needs `loop --ui`
	if *devDir != "" {
		srv.SetDevDir(a.cfg.Abs(*devDir))
	}
	a.printf("live view: http://%s/  (Ctrl-C to stop)\n", *addr)
	return srv.ListenAndServe(ctx, *addr)
}

// ---- status / coverage / incidents --------------------------------------------------

type statusDoc struct {
	Project       string            `json:"project"`
	BaseURL       string            `json:"base_url"`
	Scenarios     map[string]int    `json:"scenarios"`
	Jobs          map[string]int    `json:"jobs"`
	Features      []*model.Feature  `json:"features"`
	OpenIncidents []*model.Incident `json:"open_incidents"`
	NextDue       []*model.Scenario `json:"next_due"`
	RecentRuns    []*model.Run      `json:"recent_runs"`
	Budget        map[string]int64  `json:"budget_used_last_hour"`
	Queue         []*model.Job      `json:"queue"`
}

func (a *app) cmdStatus(ctx context.Context) error {
	p := a.cfg.Project.ID
	c, err := a.st.Counts(ctx, p)
	if err != nil {
		return err
	}
	doc := statusDoc{Project: p, BaseURL: a.cfg.Target.BaseURL, Scenarios: c.Scenarios, Jobs: c.Jobs, Budget: map[string]int64{}}
	doc.Features, _ = a.st.ListFeatures(ctx, p)
	doc.OpenIncidents, _ = a.st.ListIncidents(ctx, p, true, 20)
	doc.RecentRuns, _ = a.st.ListRuns(ctx, p, "", 10)
	doc.Queue, _ = a.st.ListJobs(ctx, p, []model.JobState{model.JobReady, model.JobLeased}, 20)
	for _, k := range []string{"browser", "chromium", "agent"} {
		doc.Budget[k], _ = a.st.BudgetUsed(ctx, p, k, time.Hour)
	}
	if all, err := a.st.ListScenarios(ctx, p, model.StateActive, model.StateSoak); err == nil {
		sort.SliceStable(all, func(i, j int) bool { return dueOf(all[i]).Before(dueOf(all[j])) })
		if len(all) > 8 {
			all = all[:8]
		}
		doc.NextDue = all
	}
	if a.jsonOut {
		return a.printJSON(doc)
	}
	a.printf("project %s → %s\n", p, a.cfg.Target.BaseURL)
	a.printf("scenarios: %s\n", fmtCounts(c.Scenarios))
	a.printf("jobs:      %s\n", fmtCounts(c.Jobs))
	if b, rd, ad := a.ev.Usage(); true {
		a.printf("evidence: %.1f MB (%d run dirs, %d agent runs); policy pass %dd / fail %dd / cap %d MB / db rows %dd\n", float64(b)/1048576, rd, ad, a.cfg.Evidence.RetainPassDays, a.cfg.Evidence.RetainFailDays, a.cfg.Evidence.MaxTotalMB, a.cfg.Evidence.RetainRunsDays)
	}
	a.printf("budget (last hour): browser %.1f min, chromium %.1f min, agent %d task(s)\n",
		float64(doc.Budget["browser"])/60000, float64(doc.Budget["chromium"])/60000, doc.Budget["agent"])
	a.printf("open incidents: %d   features: %d\n\n", c.Incidents, c.Features)
	if len(doc.Features) > 0 {
		a.printf("%-28s %-5s %-14s %-9s %-24s %-20s %s\n", "FEATURE", "KIND", "REF", "SHA", "READINESS", "SHIPPED", "HANDLED")
		for _, f := range doc.Features {
			handled := "no"
			if f.LastHandledSHA == f.LatestShippedSHA {
				handled = "yes"
			}
			a.printf("%-28s %-5s %-14s %-9s %-24s %-20s %s\n", f.ID, featureKind(f), trunc(f.Ref, 14), short(f.LatestShippedSHA), f.Readiness, f.ShippedAt.UTC().Format("2006-01-02 15:04Z"), handled)
		}
		a.printf("\n")
	}
	if len(doc.Queue) > 0 {
		a.printf("%-6s %-18s %-8s %-6s %-44s %-20s %s\n", "JOB", "KIND", "STATE", "PRIO", "SCENARIO/FEATURE", "SCHEDULED", "ERROR")
		for _, j := range doc.Queue {
			target := j.ScenarioID
			if target == "" {
				target = j.FeatureID
			}
			a.printf("%-6d %-18s %-8s %-6d %-44s %-20s %s\n", j.ID, j.Kind, j.State, j.Priority, target, j.ScheduledAt.UTC().Format("2006-01-02 15:04:05Z"), trunc(j.LastError, 50))
		}
		a.printf("\n")
	}
	if len(doc.NextDue) > 0 {
		a.printf("%-44s %-8s %-4s %-20s %-16s %s\n", "NEXT DUE", "STATE", "CLS", "DUE", "LAST", "FAILS")
		for _, m := range doc.NextDue {
			a.printf("%-44s %-8s %-4s %-20s %-16s %d\n", m.ID, m.State, m.Class, fmtTime(m.NextDueAt), m.LastOutcome, m.ConsecutiveFailures)
		}
		a.printf("\n")
	}
	if len(doc.RecentRuns) > 0 {
		a.printf("%-44s %-22s %-10s %-20s %s\n", "RECENT RUN", "OUTCOME", "BROWSER", "FINISHED", "DURATION")
		for _, r := range doc.RecentRuns {
			a.printf("%-44s %-22s %-10s %-20s %dms\n", r.ScenarioID, r.Outcome, r.Browser, r.FinishedAt.UTC().Format("2006-01-02 15:04:05Z"), r.DurationMs)
			// One plain sentence under a failing run; the raw fields stay in --json.
			if c := explain.ForRun(r, nil); !c.Empty() {
				a.printf("%-44s %s\n", "", c.Headline)
			}
		}
	}
	incidentRuns := map[int64]*model.Run{}
	if ids := incidentRunIDs(doc.OpenIncidents); len(ids) > 0 {
		incidentRuns, _ = a.st.RunsByID(ctx, p, ids)
	}
	for _, in := range doc.OpenIncidents {
		a.printf("incident #%d %s %s: %s (%s)\n", in.ID, in.Kind, in.ScenarioID, in.Title, in.MarkdownPath)
		if c := explain.ForIncident(in, incidentRuns[in.RunID], nil); !c.Empty() {
			a.printf("            원인: %s\n", c.Headline)
		}
	}
	return nil
}

// incidentRunIDs collects the run ids an incident list refers to.
func incidentRunIDs(list []*model.Incident) []int64 {
	var ids []int64
	for _, in := range list {
		if in.RunID != 0 {
			ids = append(ids, in.RunID)
		}
	}
	return ids
}

type coverageRow struct {
	ID           string                `json:"id"`
	State        model.ScenarioState   `json:"state"`
	Class        string                `json:"class"`
	Mutation     model.Mutation        `json:"mutation"`
	Origin       string                `json:"origin"`
	Oracle       string                `json:"oracle_source"`
	Links        []model.CoverageLink  `json:"links"`
	Metrics      model.ScenarioMetrics `json:"metrics"`
	LastVerified *time.Time            `json:"last_verified,omitempty"`
}

type corpusHealth struct {
	Total       int                 `json:"total"`
	ByState     map[string]int      `json:"by_state"`
	Duplicates  map[string][]string `json:"duplicate_fingerprints"`
	Quarantined []string            `json:"quarantined"`
	NeverRun    []string            `json:"never_run"`
	NeedsReview []string            `json:"needs_review"`
	FlakyTop    []string            `json:"flaky_top"`
}

func (a *app) cmdCoverage(ctx context.Context) error {
	p := a.cfg.Project.ID
	all, err := a.st.ListScenarios(ctx, p)
	if err != nil {
		return err
	}
	rows := make([]coverageRow, 0, len(all))
	health := corpusHealth{Total: len(all), ByState: map[string]int{}, Duplicates: map[string][]string{}}
	byFP := map[string][]string{}
	type flaky struct {
		id   string
		rate float64
	}
	var flakes []flaky
	for _, m := range all {
		health.ByState[string(m.State)]++
		links, _ := a.st.ListCoverageLinks(ctx, p, m.ID)
		met, _ := a.st.GetMetrics(ctx, m.ID)
		if met == nil {
			met = &model.ScenarioMetrics{ScenarioID: m.ID}
		}
		rows = append(rows, coverageRow{ID: m.ID, State: m.State, Class: m.Class, Mutation: m.Mutation, Origin: m.Origin, Oracle: m.OracleSource, Links: links, Metrics: *met, LastVerified: met.LastVerifiedAt})
		switch m.State {
		case model.StateRejected, model.StateRetired, model.StateDuplicate, model.StateMerged, model.StateSuperseded:
		default:
			byFP[m.Fingerprint] = append(byFP[m.Fingerprint], m.ID)
		}
		if m.State == model.StateQuarantined {
			health.Quarantined = append(health.Quarantined, m.ID)
		}
		if m.State == model.StateNeedsReview {
			health.NeedsReview = append(health.NeedsReview, m.ID)
		}
		if (m.State == model.StateActive || m.State == model.StateSoak) && m.LastRunAt == nil {
			health.NeverRun = append(health.NeverRun, m.ID)
		}
		if met.Runs >= 3 && met.Flakes > 0 {
			flakes = append(flakes, flaky{m.ID, float64(met.Flakes) / float64(met.Runs)})
		}
	}
	for fp, ids := range byFP {
		if len(ids) > 1 && fp != "" {
			health.Duplicates[fp] = ids
		}
	}
	sort.Slice(flakes, func(i, j int) bool { return flakes[i].rate > flakes[j].rate })
	for i, f := range flakes {
		if i >= 5 {
			break
		}
		health.FlakyTop = append(health.FlakyTop, fmt.Sprintf("%s (%.0f%%)", f.id, f.rate*100))
	}
	if a.jsonOut {
		return a.printJSON(map[string]any{"scenarios": rows, "corpus_health": health})
	}
	a.printf("%-44s %-12s %-3s %-10s %-9s %5s %5s %5s %4s %-16s %s\n", "SCENARIO", "STATE", "CLS", "MUTATION", "ORACLE", "RUNS", "PASS", "FLAKE", "REG", "LAST VERIFIED", "LINKS")
	for _, r := range rows {
		a.printf("%-44s %-12s %-3s %-10s %-9s %5d %5d %5d %4d %-16s %s\n", r.ID, r.State, r.Class, r.Mutation, r.Oracle, r.Metrics.Runs, r.Metrics.Passes, r.Metrics.Flakes, r.Metrics.RegressionsCaught, fmtShortTime(r.LastVerified), fmtLinks(r.Links))
	}
	a.printf("\ncorpus health: %d scenario(s) %s\n", health.Total, fmtCounts(health.ByState))
	a.printf("  duplicates by fingerprint: %d group(s)\n", len(health.Duplicates))
	for fp, ids := range health.Duplicates {
		a.printf("    %s: %s\n", short(fp), strings.Join(ids, ", "))
	}
	a.printf("  quarantined: %d %v\n", len(health.Quarantined), health.Quarantined)
	a.printf("  never run (ACTIVE/SOAK): %d %v\n", len(health.NeverRun), health.NeverRun)
	a.printf("  needs review: %d %v\n", len(health.NeedsReview), health.NeedsReview)
	if len(health.FlakyTop) > 0 {
		a.printf("  flakiest: %s\n", strings.Join(health.FlakyTop, ", "))
	}
	return nil
}

func (a *app) cmdIncidents(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("incidents", flag.ContinueOnError)
	all := fs.Bool("all", false, "include resolved incidents")
	limit := fs.Int("limit", 50, "max rows")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	list, err := a.st.ListIncidents(ctx, a.cfg.Project.ID, !*all, *limit)
	if err != nil {
		return err
	}
	if a.jsonOut {
		if list == nil {
			list = []*model.Incident{}
		}
		return a.printJSON(list)
	}
	if len(list) == 0 {
		a.printf("no incidents\n")
		return nil
	}
	a.printf("%-4s %-9s %-15s %-44s %-9s %-20s %s\n", "ID", "STATE", "KIND", "SCENARIO", "SHA", "CREATED", "TITLE")
	for _, in := range list {
		a.printf("%-4d %-9s %-15s %-44s %-9s %-20s %s\n", in.ID, in.State, in.Kind, in.ScenarioID, short(in.ShippedSHA), in.CreatedAt.UTC().Format("2006-01-02 15:04Z"), in.Title)
		if in.MarkdownPath != "" {
			a.printf("     %s\n", in.MarkdownPath)
		}
	}
	return nil
}

// cmdFindings lists data-analyst findings (OPEN by default) or resolves one:
// vigil findings [--all] [--limit N] | vigil findings resolve <id>
func (a *app) cmdFindings(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "resolve" {
		if len(args) != 2 {
			return fmt.Errorf("usage: vigil findings resolve <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("findings resolve: %q is not a finding id", args[1])
		}
		if err := a.st.ResolveFinding(ctx, a.cfg.Project.ID, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("finding %d: not found or already resolved", id)
			}
			return err
		}
		if a.jsonOut {
			return a.printJSON(map[string]any{"id": id, "state": "RESOLVED"})
		}
		a.printf("finding %d: RESOLVED\n", id)
		return nil
	}
	fs := flag.NewFlagSet("findings", flag.ContinueOnError)
	all := fs.Bool("all", false, "include resolved findings")
	limit := fs.Int("limit", 50, "max rows")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	list, err := a.st.ListFindings(ctx, a.cfg.Project.ID, !*all, *limit)
	if err != nil {
		return err
	}
	if a.jsonOut {
		if list == nil {
			list = []*model.Finding{}
		}
		return a.printJSON(list)
	}
	if len(list) == 0 {
		a.printf("no findings\n")
		return nil
	}
	a.printf("%-4s %-9s %-14s %-32s %-20s %s\n", "ID", "STATE", "KIND", "SCENARIO/FEATURE", "CREATED", "WHERE")
	for _, f := range list {
		a.printf("%-4d %-9s %-14s %-32s %-20s %s\n", f.ID, f.State, f.Kind, scenarioOrFeature(f), f.CreatedAt.UTC().Format("2006-01-02 15:04Z"), f.Where)
		a.printf("     expected: %s\n     actual:   %s\n", f.Expected, f.Actual)
		if f.Evidence != "" {
			a.printf("     evidence: %s\n", f.Evidence)
		}
	}
	return nil
}

// ---- approve / reject / list / show ---------------------------------------------------

func (a *app) approvalService() *approval.Service { return approval.New(a.cfg, a.st, a.log) }

func (a *app) cmdApprove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	soak := fs.Bool("soak", false, "force the SOAK path (also for PENDING_APPROVAL scripts) instead of daily ACTIVE")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errors.New("usage: vigil approve [--soak] <scenario>")
	}
	r, err := a.approvalService().Approve(ctx, pos[0], approval.Options{Soak: *soak})
	if err != nil {
		return err
	}
	return a.printReceipt(r)
}

func (a *app) cmdReject(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: vigil reject <scenario>")
	}
	r, err := a.approvalService().Reject(ctx, args[0])
	if err != nil {
		return err
	}
	return a.printReceipt(r)
}

// printReceipt prints what happened to the script and, when the source issue
// was commented, the Jira result (json mode emits the receipt as an object).
func (a *app) printReceipt(r approval.Receipt) error {
	if a.jsonOut {
		out := map[string]any{"id": r.ID, "from": r.From, "state": r.To, "cadence": r.Cadence, "next_due_at": r.NextDueAt}
		if r.Jira != nil {
			out["jira"] = r.Jira
		}
		return a.printJSON(out)
	}
	a.printf("%s\n", r.Message(a.cfg.DailyLocation()))
	return nil
}

func (a *app) cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	state := fs.String("state", "", "filter by state (comma-separated)")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	var states []model.ScenarioState
	for _, s := range strings.Split(*state, ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			states = append(states, model.ScenarioState(s))
		}
	}
	list, err := a.st.ListScenarios(ctx, a.cfg.Project.ID, states...)
	if err != nil {
		return err
	}
	if a.jsonOut {
		if list == nil {
			list = []*model.Scenario{}
		}
		return a.printJSON(list)
	}
	a.printf("%-44s %-12s %-3s %-10s %-3s %-16s %-20s %-5s %s\n", "SCENARIO", "STATE", "CLS", "MUTATION", "VER", "LAST OUTCOME", "NEXT DUE", "FAILS", "TITLE")
	for _, m := range list {
		a.printf("%-44s %-12s %-3s %-10s %-3d %-16s %-20s %-5d %s\n", m.ID, m.State, m.Class, m.Mutation, m.CurrentVersion, m.LastOutcome, fmtTime(m.NextDueAt), m.ConsecutiveFailures, trunc(m.Title, 50))
	}
	return nil
}

func (a *app) cmdShow(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: vigil show <scenario>")
	}
	p := a.cfg.Project.ID
	m, v, err := a.st.GetCurrentScenarioVersion(ctx, p, args[0])
	if err != nil {
		return fmt.Errorf("scenario %s: %w", args[0], err)
	}
	links, _ := a.st.ListCoverageLinks(ctx, p, m.ID)
	runs, _ := a.st.ListRuns(ctx, p, m.ID, 10)
	met, _ := a.st.GetMetrics(ctx, m.ID)
	if a.jsonOut {
		return a.printJSON(map[string]any{"scenario": m, "version": v, "links": links, "runs": runs, "metrics": met})
	}
	a.printf("%s  state=%s class=%s mutation=%s v%d origin=%s oracle=%s/%s@%s\n", m.ID, m.State, m.Class, m.Mutation, v.Version, m.Origin, m.OracleSource, m.OracleFeature, short(m.OracleSHA))
	a.printf("locks=%v soak=%d/%d fails=%d last=%s next_due=%s\n", m.Locks, m.SoakPasses, m.SoakTarget, m.ConsecutiveFailures, m.LastOutcome, fmtTime(m.NextDueAt))
	if m.SourceRef != "" || m.State == model.StatePendingApproval {
		a.printf("source=%s/%s reproduction=%s approved_at=%s\n", orDefault(m.SourceKind, "-"), orDefault(m.SourceRef, "-"), orDefault(m.Reproduction, "-"), fmtTime(m.ApprovedAt))
	}
	a.printf("links: %s\n", fmtLinks(links))
	if met != nil {
		a.printf("metrics: runs=%d pass=%d fail=%d flake=%d regressions=%d avg=%dms last_verified=%s\n", met.Runs, met.Passes, met.Failures, met.Flakes, met.RegressionsCaught, met.MedianDurationMs, fmtTime(met.LastVerifiedAt))
	}
	a.printf("\n--- v%d (%s: %s) ---\n%s\n", v.Version, v.CreatedBy, v.Reason, strings.TrimRight(v.YAML, "\n"))
	if len(runs) > 0 {
		a.printf("\n%-4s %-22s %-10s %-20s %8s %-3s %s\n", "RUN", "OUTCOME", "BROWSER", "FINISHED", "MS", "ATT", "ERROR / EVIDENCE")
		for _, r := range runs {
			detail := r.EvidenceDir
			if r.Error != "" {
				detail = trunc(r.Error, 80) + "  " + r.EvidenceDir
			}
			a.printf("%-4d %-22s %-10s %-20s %8d %-3d %s\n", r.ID, r.Outcome, r.Browser, r.FinishedAt.UTC().Format("2006-01-02 15:04:05Z"), r.DurationMs, r.Attempt, detail)
		}
	}
	return nil
}

// ---- validate -------------------------------------------------------------------------

func (a *app) cmdValidate() (int, error) {
	flowsDir := a.cfg.Abs(a.cfg.Paths.Flows)
	scDir := a.cfg.Abs(a.cfg.Paths.Scenarios)
	flows := map[string]*dsl.Flow{}
	known := map[string]bool{}
	problems := 0
	for _, path := range yamlFiles(flowsDir) {
		f, err := dsl.ParseFlowFile(path)
		if err == nil {
			err = f.Validate()
		}
		if err != nil {
			problems++
			a.printf("FAIL flow     %s\n     %v\n", rel(path), err)
			continue
		}
		if known[f.Flow.ID] {
			problems++
			a.printf("FAIL flow     %s\n     duplicate flow id %q\n", rel(path), f.Flow.ID)
			continue
		}
		known[f.Flow.ID] = true
		flows[f.Flow.ID] = f
		a.printf("OK   flow     %-40s %s\n", f.Flow.ID, rel(path))
	}
	ids := map[string]string{}
	fps := map[string][]string{}
	files := yamlFiles(scDir)
	for _, path := range files {
		sc, err := dsl.ParseFile(path)
		if err == nil {
			err = sc.Validate(known)
		}
		if err != nil {
			problems++
			a.printf("FAIL scenario %s\n     %v\n", rel(path), strings.ReplaceAll(err.Error(), "\n", "\n     "))
			continue
		}
		if prev, dup := ids[sc.Scenario.ID]; dup {
			problems++
			a.printf("FAIL scenario %s\n     duplicate scenario id %q (also in %s)\n", rel(path), sc.Scenario.ID, rel(prev))
			continue
		}
		ids[sc.Scenario.ID] = path
		fp := sc.Fingerprint(flows)
		fps[fp] = append(fps[fp], sc.Scenario.ID)
		a.printf("OK   scenario %-40s v%d %-3s %-10s fp=%s %s\n", sc.Scenario.ID, sc.Scenario.Version, orDefault(sc.Scenario.Class, "P1"), orDefault(sc.Scenario.Mutation, "read-only"), fp[:8], rel(path))
	}
	for fp, list := range fps {
		if len(list) > 1 {
			a.printf("WARN duplicate fingerprint %s: %s (same logical scenario; prefer a variant)\n", fp[:8], strings.Join(list, ", "))
		}
	}
	a.printf("%d flow(s), %d scenario(s), %d problem(s)\n", len(flows), len(files), problems)
	if problems > 0 {
		return 1, nil
	}
	return 0, nil
}

func yamlFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ---- formatting helpers ---------------------------------------------------------------

// featureKind renders the feature kind column ("ship" for rows written before the column existed).
func featureKind(f *model.Feature) string {
	return orDefault(f.Kind, model.FeatureKindShip)
}

func scenarioOrFeature(f *model.Finding) string {
	if f.ScenarioID != "" {
		return f.ScenarioID
	}
	return f.FeatureID
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// trunc shortens s to n runes (never splits a multibyte character).
func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func rel(path string) string {
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, path); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
	}
	return path
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func fmtShortTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

func dueOf(m *model.Scenario) time.Time {
	if m.NextDueAt == nil {
		return time.Time{}
	}
	return *m.NextDueAt
}

func fmtCounts(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func fmtLinks(links []model.CoverageLink) string {
	if len(links) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(links))
	for _, l := range links {
		parts = append(parts, fmt.Sprintf("%s:%s", l.LinkType, l.LinkValue))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
