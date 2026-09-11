package ui

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

// ---- /api/overview: the system-activity page's single poll -------------------

type overview struct {
	Project   string             `json:"project"`
	Target    string             `json:"target"`
	Now       time.Time          `json:"now"`
	Scenarios map[string]int     `json:"scenario_counts"`
	Jobs      map[string]int     `json:"job_counts"`
	Budget    map[string]float64 `json:"budget"`
	Active    []jobView          `json:"active_jobs"`
	Queued    []jobView          `json:"queued_jobs"`
	Runs      []runView          `json:"runs"`
	List      []scenarioView     `json:"scenarios"`
	Incidents []incidentView     `json:"incidents"`
	Features  []featureView      `json:"features"`
	Agent     *agentView         `json:"agent"`
	Window    *windowView        `json:"window"`
	Super     *supervisorView    `json:"supervisor"`
}

// windowView is the active-hours state the dashboard renders and edits.
type windowView struct {
	config.ActiveHours
	Source     string `json:"source"`      // "override" (set in the UI) | "config" (vigil.yaml)
	ActiveNow  bool   `json:"active_now"`  // whether cadence work may be enqueued right now
	NextChange string `json:"next_change"` // local "HH:MM" of the next open/close, "" if never
	Zone       string `json:"zone"`        // resolved IANA name, for display
}

type jobView struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Scenario string `json:"scenario"`
	Title    string `json:"title"`
	Feature  string `json:"feature"`
	Browser  string `json:"browser"`
	Since    string `json:"since"`
	Attempt  int    `json:"attempt"`
}

type runView struct {
	ID           int64  `json:"id"`
	Scenario     string `json:"scenario"`
	Title        string `json:"title"`
	Version      int    `json:"version"`
	Browser      string `json:"browser"`
	Outcome      string `json:"outcome"`
	DurationMs   int64  `json:"duration_ms"`
	FinishedAt   string `json:"finished_at"`
	FailedStep   int    `json:"failed_step"`
	FailedAction string `json:"failed_action"`
	Actual       string `json:"actual,omitempty"`
	Error        string `json:"error,omitempty"`
	EvidenceRel  string `json:"evidence_rel"`
	Screenshot   string `json:"screenshot,omitempty"`
	// Cause is the one-sentence cause of a failing run (nil when it passed).
	Cause *causeView `json:"cause,omitempty"`
}

type scenarioView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Class       string `json:"class"`
	Browser     string `json:"browser"`
	LastOutcome string `json:"last_outcome"`
	LastRunAt   string `json:"last_run_at"`
	NextDueAt   string `json:"next_due_at"`
	Failures    int    `json:"consecutive_failures"`
	Soak        string `json:"soak"`
	Origin      string `json:"origin"`
	Oracle      string `json:"oracle"`
	Runs        int    `json:"runs"`
	Passes      int    `json:"passes"`
	Flakes      int    `json:"flakes"`
}

type incidentView struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Scenario  string `json:"scenario"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	MDRel     string `json:"md_rel"`
	// Cause is the one-sentence explanation of the run behind the incident.
	Cause *causeView `json:"cause,omitempty"`
}

type featureView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	Readiness string `json:"readiness"`
	ShippedAt string `json:"shipped_at"`
	Summary   string `json:"summary"`
	Handled   bool   `json:"handled"`
}

// scenarioViews builds the per-scenario rows with three batch queries instead
// of three queries per scenario. browsers/titles are returned for the callers
// that decorate jobs and runs with them.
func (s *Server) scenarioViews(r *http.Request) (list []scenarioView, titles, browsers map[string]string) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	titles, browsers = map[string]string{}, map[string]string{}
	scs, err := s.st.ListScenarios(ctx, p)
	if err != nil {
		return nil, titles, browsers
	}
	versions, _ := s.st.ListCurrentScenarioVersions(ctx, p)
	metrics, _ := s.st.ListScenarioMetrics(ctx, p)
	list = make([]scenarioView, 0, len(scs))
	for _, sc := range scs {
		titles[sc.ID] = sc.Title
		browser := s.cfg.Browser.Primary
		if v := versions[sc.ID]; v != nil {
			if b := primaryBrowserOf(v.YAML); b != "" {
				browser = b
			}
		}
		browsers[sc.ID] = browser
		sv := scenarioView{ID: sc.ID, Title: sc.Title, State: string(sc.State), Class: sc.Class, Browser: browser,
			LastOutcome: string(sc.LastOutcome), LastRunAt: fmtT(sc.LastRunAt), NextDueAt: fmtT(sc.NextDueAt),
			Failures: sc.ConsecutiveFailures, Origin: sc.Origin, Oracle: sc.OracleSource}
		if sc.State == model.StateSoak {
			sv.Soak = itoa(sc.SoakPasses) + "/" + itoa(sc.SoakTarget)
		}
		if m := metrics[sc.ID]; m != nil {
			sv.Runs, sv.Passes, sv.Flakes = m.Runs, m.Passes, m.Flakes
		}
		list = append(list, sv)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Class != list[j].Class {
			return list[i].Class < list[j].Class
		}
		return list[i].ID < list[j].ID
	})
	return list, titles, browsers
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	o := overview{Project: p, Target: s.cfg.Target.BaseURL, Scenarios: map[string]int{}, Jobs: map[string]int{}, Budget: map[string]float64{}}
	if c, err := s.st.Counts(ctx, p); err == nil {
		o.Scenarios = c.Scenarios
		o.Jobs = c.Jobs
	}
	for _, k := range []string{"browser", "chromium"} {
		ms, _ := s.st.BudgetUsed(ctx, p, k, time.Hour)
		o.Budget[k+"_minutes"] = float64(ms) / 60000
	}
	o.Window = s.windowState(ctx)
	o.Super = s.supervisorState(ctx)
	agentTasks, _ := s.st.BudgetUsed(ctx, p, "agent", time.Hour)
	o.Budget["agent_tasks"] = float64(agentTasks)
	o.Budget["agent_budget"] = float64(s.cfg.Budget.AgentTasksPerHour)

	var titles map[string]string
	o.List, titles, _ = s.scenarioViews(r)

	if jobs, err := s.st.ListJobs(ctx, p, []model.JobState{model.JobLeased}, 20); err == nil {
		for _, j := range jobs {
			o.Active = append(o.Active, jobView{ID: j.ID, Kind: string(j.Kind), Scenario: j.ScenarioID, Title: titles[j.ScenarioID], Feature: j.FeatureID, Browser: string(j.Browser), Since: fmtT(&j.UpdatedAt), Attempt: j.Attempt})
		}
	}
	if jobs, err := s.st.ListJobs(ctx, p, []model.JobState{model.JobReady}, 15); err == nil {
		for _, j := range jobs {
			o.Queued = append(o.Queued, jobView{ID: j.ID, Kind: string(j.Kind), Scenario: j.ScenarioID, Title: titles[j.ScenarioID], Feature: j.FeatureID, Browser: string(j.Browser), Since: fmtT(&j.ScheduledAt), Attempt: j.Attempt})
		}
	}
	if runs, err := s.st.ListRuns(ctx, p, "", 200); err == nil {
		for _, ru := range runs {
			rv := runView{ID: ru.ID, Scenario: ru.ScenarioID, Title: titles[ru.ScenarioID], Version: ru.ScenarioVersion, Browser: string(ru.Browser), Outcome: string(ru.Outcome),
				DurationMs: ru.DurationMs, FinishedAt: fmtT(&ru.FinishedAt), FailedStep: ru.FailedStep, FailedAction: ru.FailedAction,
				Actual: clip(ru.Actual, 300), Error: clip(ru.Error, 300), EvidenceRel: s.rel(ru.EvidenceDir), Cause: runCause(ru, nil)}
			if rv.EvidenceRel != "" {
				if _, err := os.Stat(filepath.Join(ru.EvidenceDir, "screenshot.png")); err == nil {
					rv.Screenshot = rv.EvidenceRel + "/screenshot.png"
				}
			}
			o.Runs = append(o.Runs, rv)
		}
	}
	if incs, err := s.st.ListIncidents(ctx, p, false, 20); err == nil {
		o.Incidents = s.incidentViews(ctx, incs, nil)
	}
	if fs, err := s.st.ListFeatures(ctx, p); err == nil {
		for _, f := range fs {
			o.Features = append(o.Features, featureView{ID: f.ID, Kind: f.Kind, Ref: f.Ref, SHA: short(f.LatestShippedSHA), Readiness: string(f.Readiness), ShippedAt: fmtT(&f.ShippedAt), Summary: clip(f.Summary, 120), Handled: f.LastHandledSHA == f.LatestShippedSHA})
		}
	}
	o.Agent = s.latestAgent()
	writeJSONPolled(w, r, &o, func() { o.Now = time.Now() })
}
