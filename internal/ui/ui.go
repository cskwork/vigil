// Package ui serves a single-page live view of what vigil and its Browser
// Agent are doing (PRD §15 evidence, §18 corpus health) for non-developers.
//
// It reads SQLite + evidence files only, with exactly one exception:
// POST/DELETE /api/schedule/window writes the schedule.active_hours row in
// scheduler_state. That row is the control channel to the `loop` process, which
// runs separately and shares nothing but the database. No other route mutates
// anything, and the endpoint is unauthenticated - bind `serve`/`loop --ui` to a
// trusted address.
package ui

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

//go:embed index.html
var indexHTML []byte

//go:embed activity.html
var activityHTML []byte

//go:embed report.html
var reportHTML []byte

//go:embed scripts.html
var scriptsHTML []byte

//go:embed theme.css
var themeCSS []byte

type Server struct {
	cfg    *config.Config
	st     *store.Store
	evRoot string
	devDir string // when set, page assets are read from this directory on every request
}

// SetDevDir serves index.html/scripts.html/theme.css from dir instead of the embedded copies.
func (s *Server) SetDevDir(dir string) { s.devDir = dir }

func (s *Server) asset(name string, embedded []byte) []byte {
	if s.devDir != "" {
		if b, err := os.ReadFile(filepath.Join(s.devDir, name)); err == nil {
			return b
		}
	}
	return embedded
}

func New(cfg *config.Config, st *store.Store, evidenceRoot string) *Server {
	return &Server{cfg: cfg, st: st, evRoot: evidenceRoot}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.asset("index.html", indexHTML))
	})
	mux.HandleFunc("/activity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.asset("activity.html", activityHTML))
	})
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.asset("report.html", reportHTML))
	})
	mux.HandleFunc("/api/verification", s.verification)
	mux.HandleFunc("/api/requests", s.requests)
	mux.HandleFunc("/api/run/detail", s.runDetail)
	mux.HandleFunc("/scripts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(s.asset("scripts.html", scriptsHTML))
	})
	mux.HandleFunc("/ui/theme.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(s.asset("theme.css", themeCSS))
	})
	mux.HandleFunc("/api/scripts", s.scripts)
	mux.HandleFunc("/api/script", s.script)
	mux.HandleFunc("/api/overview", s.overview)
	mux.HandleFunc("/api/schedule/window", s.scheduleWindow)
	mux.HandleFunc("/api/run/steps", s.runSteps)
	mux.HandleFunc("/api/agent/latest", s.agentLatest)
	// Evidence is already secret-redacted by the runner/agent adapter; serve it read-only.
	mux.Handle("/evidence/", http.StripPrefix("/evidence/", http.FileServer(http.Dir(s.evRoot))))
	return mux
}

// ListenAndServe blocks until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- overview ------------------------------------------------------------

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
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
	Error        string `json:"error"`
	EvidenceRel  string `json:"evidence_rel"`
	Screenshot   string `json:"screenshot,omitempty"`
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
}

type featureView struct {
	ID        string `json:"id"`
	SHA       string `json:"sha"`
	Readiness string `json:"readiness"`
	ShippedAt string `json:"shipped_at"`
	Summary   string `json:"summary"`
	Handled   bool   `json:"handled"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	o := overview{Project: p, Target: s.cfg.Target.BaseURL, Now: time.Now(), Scenarios: map[string]int{}, Jobs: map[string]int{}, Budget: map[string]float64{}}
	if c, err := s.st.Counts(ctx, p); err == nil {
		o.Scenarios = c.Scenarios
		o.Jobs = c.Jobs
	}
	for _, k := range []string{"browser", "chromium"} {
		ms, _ := s.st.BudgetUsed(ctx, p, k, time.Hour)
		o.Budget[k+"_minutes"] = float64(ms) / 60000
	}
	o.Window = s.windowState(ctx)
	agentTasks, _ := s.st.BudgetUsed(ctx, p, "agent", time.Hour)
	o.Budget["agent_tasks"] = float64(agentTasks)
	o.Budget["agent_budget"] = float64(s.cfg.Budget.AgentTasksPerHour)

	titles := map[string]string{}
	browsers := map[string]string{}
	if scs, err := s.st.ListScenarios(ctx, p); err == nil {
		for _, sc := range scs {
			titles[sc.ID] = sc.Title
			browser := s.cfg.Browser.Primary
			if _, v, err := s.st.GetCurrentScenarioVersion(ctx, p, sc.ID); err == nil && v != nil {
				if b := primaryBrowserOf(v.YAML); b != "" {
					browser = b
				}
			}
			browsers[sc.ID] = browser
			m, _ := s.st.GetMetrics(ctx, sc.ID)
			sv := scenarioView{ID: sc.ID, Title: sc.Title, State: string(sc.State), Class: sc.Class, Browser: browser,
				LastOutcome: string(sc.LastOutcome), LastRunAt: fmtT(sc.LastRunAt), NextDueAt: fmtT(sc.NextDueAt),
				Failures: sc.ConsecutiveFailures, Origin: sc.Origin, Oracle: sc.OracleSource}
			if sc.State == model.StateSoak {
				sv.Soak = itoa(sc.SoakPasses) + "/" + itoa(sc.SoakTarget)
			}
			if m != nil {
				sv.Runs, sv.Passes, sv.Flakes = m.Runs, m.Passes, m.Flakes
			}
			o.List = append(o.List, sv)
		}
	}
	sort.Slice(o.List, func(i, j int) bool {
		if o.List[i].Class != o.List[j].Class {
			return o.List[i].Class < o.List[j].Class
		}
		return o.List[i].ID < o.List[j].ID
	})

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
				Expected: clip(ru.Expected, 300), Actual: clip(ru.Actual, 300), Error: clip(ru.Error, 300), EvidenceRel: s.rel(ru.EvidenceDir)}
			if rv.EvidenceRel != "" {
				if _, err := os.Stat(filepath.Join(ru.EvidenceDir, "screenshot.png")); err == nil {
					rv.Screenshot = rv.EvidenceRel + "/screenshot.png"
				}
			}
			o.Runs = append(o.Runs, rv)
		}
	}
	if incs, err := s.st.ListIncidents(ctx, p, false, 20); err == nil {
		for _, in := range incs {
			o.Incidents = append(o.Incidents, incidentView{ID: in.ID, Kind: string(in.Kind), Title: in.Title, Scenario: in.ScenarioID, State: in.State, CreatedAt: fmtT(&in.CreatedAt), MDRel: s.rel(in.MarkdownPath)})
		}
	}
	if fs, err := s.st.ListFeatures(ctx, p); err == nil {
		for _, f := range fs {
			o.Features = append(o.Features, featureView{ID: f.ID, SHA: short(f.LatestShippedSHA), Readiness: string(f.Readiness), ShippedAt: fmtT(&f.ShippedAt), Summary: clip(f.Summary, 120), Handled: f.LastHandledSHA == f.LatestShippedSHA})
		}
	}
	o.Agent = s.latestAgent()
	writeJSON(w, o)
}

// ---- run steps -----------------------------------------------------------

func (s *Server) runSteps(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("dir")
	dir, ok := s.safeJoin(rel)
	if !ok {
		http.Error(w, "bad dir", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "steps.json"))
	if err != nil {
		http.Error(w, "steps.json not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// ---- Browser Agent activity ---------------------------------------------

type agentView struct {
	Dir        string     `json:"dir"` // evidence-relative
	Feature    string     `json:"feature"`
	StartedAt  string     `json:"started_at"`
	UpdatedAt  string     `json:"updated_at"`
	Status     string     `json:"status"` // running | done | stale
	Decision   string     `json:"decision,omitempty"`
	Candidates int        `json:"candidates"`
	Sandbox    string     `json:"sandbox,omitempty"`
	Attempts   int        `json:"attempts"`
	ToolCalls  []toolCall `json:"tool_calls"`
	LastText   string     `json:"last_text"`
	Retries    []string   `json:"retries,omitempty"`
}

type toolCall struct {
	N       int    `json:"n"`
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	OK      *bool  `json:"ok,omitempty"`
	Preview string `json:"preview,omitempty"`
}

func (s *Server) agentLatest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.latestAgent())
}

func (s *Server) latestAgent() *agentView {
	root := filepath.Join(s.evRoot, "agent")
	var newest string
	var newestT time.Time
	feats, _ := os.ReadDir(root)
	for _, f := range feats {
		if !f.IsDir() {
			continue
		}
		runs, _ := os.ReadDir(filepath.Join(root, f.Name()))
		for _, ru := range runs {
			if !ru.IsDir() {
				continue
			}
			p := filepath.Join(root, f.Name(), ru.Name())
			st, err := os.Stat(filepath.Join(p, "agent-transcript.jsonl"))
			if err != nil {
				continue
			}
			if st.ModTime().After(newestT) {
				newestT, newest = st.ModTime(), p
			}
		}
	}
	if newest == "" {
		return nil
	}
	av := &agentView{Dir: s.rel(newest), Feature: filepath.Base(filepath.Dir(newest)), UpdatedAt: newestT.Local().Format("2006-01-02 15:04:05")}
	if st, err := os.Stat(filepath.Join(newest, "agent-request.yaml")); err == nil {
		av.StartedAt = st.ModTime().Local().Format("2006-01-02 15:04:05")
	}
	if b, err := os.ReadFile(filepath.Join(newest, "agent-result.json")); err == nil {
		var res struct {
			Decision   string   `json:"decision"`
			Candidates []string `json:"script_candidates"`
			Sandbox    string   `json:"sandbox"`
			Attempts   int      `json:"attempts"`
		}
		_ = json.Unmarshal(b, &res)
		av.Status, av.Decision, av.Candidates, av.Sandbox, av.Attempts = "done", res.Decision, len(res.Candidates), res.Sandbox, res.Attempts
	} else if time.Since(newestT) > 20*time.Minute {
		av.Status = "stale"
	} else {
		av.Status = "running"
	}
	s.parseTranscript(filepath.Join(newest, "agent-transcript.jsonl"), av)
	return av
}

func (s *Server) parseTranscript(path string, av *agentView) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	byID := map[string]int{}
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ToolCall string `json:"toolCallId"`
			ToolName string `json:"toolName"`
			Args     any    `json:"args"`
			Result   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Details struct {
					ExitCode *int `json:"exitCode"`
				} `json:"details"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Message struct {
				Role         string `json:"role"`
				ErrorMessage string `json:"errorMessage"`
				Content      []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool_execution_start":
			args, _ := json.Marshal(ev.Args)
			av.ToolCalls = append(av.ToolCalls, toolCall{N: len(av.ToolCalls) + 1, Tool: ev.ToolName, Args: clip(string(args), 200)})
			byID[ev.ToolCall] = len(av.ToolCalls) - 1
		case "tool_execution_end":
			if i, ok := byID[ev.ToolCall]; ok {
				var text string
				for _, c := range ev.Result.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
				okv := !ev.Result.IsError && (ev.Result.Details.ExitCode == nil || *ev.Result.Details.ExitCode == 0)
				av.ToolCalls[i].OK = &okv
				av.ToolCalls[i].Preview = clip(strings.TrimSpace(text), 240)
			}
		case "message_end":
			if ev.Message.Role == "assistant" {
				if ev.Message.ErrorMessage != "" {
					av.Retries = append(av.Retries, clip(ev.Message.ErrorMessage, 160))
				}
				var text string
				for _, c := range ev.Message.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
				if t := strings.TrimSpace(text); t != "" {
					if i := strings.Index(t, "```"); i > 0 {
						t = t[:i]
					}
					av.LastText = clip(strings.TrimSpace(t), 600)
				}
			}
		}
	}
	if len(av.ToolCalls) > 150 {
		av.ToolCalls = av.ToolCalls[len(av.ToolCalls)-150:]
	}
}

// ---- helpers -------------------------------------------------------------

func (s *Server) rel(abs string) string {
	if abs == "" {
		return ""
	}
	r, err := filepath.Rel(s.evRoot, abs)
	if err != nil || strings.HasPrefix(r, "..") {
		return ""
	}
	return filepath.ToSlash(r)
}

func (s *Server) safeJoin(rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	p := filepath.Join(s.evRoot, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, filepath.Clean(s.evRoot)+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

func primaryBrowserOf(yamlText string) string {
	for _, line := range strings.Split(yamlText, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "primary:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "primary:"))
			if i := strings.Index(v, "#"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func fmtT(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func itoa(i int) string {
	return strings.TrimSpace(strings.Replace(json.Number(intString(i)).String(), "\"", "", -1))
}

func intString(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- scripts page: every deterministic script (seed, agent, repair, system) --

type scriptView struct {
	scenarioView
	Versions []versionView        `json:"versions"`
	Links    []model.CoverageLink `json:"links"`
	Locks    []string             `json:"locks"`
	Mutation string               `json:"mutation"`
	YAML     string               `json:"yaml"`
	SoakN    int                  `json:"soak_passes"`
	SoakT    int                  `json:"soak_target"`
}

type versionView struct {
	Version   int    `json:"version"`
	CreatedBy string `json:"created_by"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	Current   bool   `json:"current"`
}

func (s *Server) scripts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	scs, err := s.st.ListScenarios(ctx, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]scriptView, 0, len(scs))
	for _, sc := range scs {
		sv := scriptView{scenarioView: scenarioView{ID: sc.ID, Title: sc.Title, State: string(sc.State), Class: sc.Class, LastOutcome: string(sc.LastOutcome),
			LastRunAt: fmtT(sc.LastRunAt), NextDueAt: fmtT(sc.NextDueAt), Failures: sc.ConsecutiveFailures, Origin: sc.Origin, Oracle: sc.OracleSource},
			Locks: sc.Locks, Mutation: string(sc.Mutation), SoakN: sc.SoakPasses, SoakT: sc.SoakTarget}
		sv.Browser = s.cfg.Browser.Primary
		if vs, err := s.st.ListScenarioVersions(ctx, p, sc.ID); err == nil {
			for _, v := range vs {
				sv.Versions = append(sv.Versions, versionView{Version: v.Version, CreatedBy: v.CreatedBy, Reason: v.Reason, CreatedAt: fmtT(&v.CreatedAt), Current: v.Version == sc.CurrentVersion})
				if v.Version == sc.CurrentVersion {
					sv.YAML = v.YAML
					if b := primaryBrowserOf(v.YAML); b != "" {
						sv.Browser = b
					}
				}
			}
		}
		sv.Links, _ = s.st.ListCoverageLinks(ctx, p, sc.ID)
		if m, _ := s.st.GetMetrics(ctx, sc.ID); m != nil {
			sv.Runs, sv.Passes, sv.Flakes = m.Runs, m.Passes, m.Flakes
		}
		out = append(out, sv)
	}
	writeJSON(w, map[string]any{"project": p, "target": s.cfg.Target.BaseURL, "now": time.Now(), "scripts": out})
}

// script returns one version's YAML: /api/script?id=<scenario>&v=<n>
func (s *Server) script(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var v int
	for _, ch := range r.URL.Query().Get("v") {
		if ch < '0' || ch > '9' {
			v = 0
			break
		}
		v = v*10 + int(ch-'0')
	}
	if id == "" || v == 0 {
		http.Error(w, "id and v required", http.StatusBadRequest)
		return
	}
	sv, err := s.st.GetScenarioVersion(r.Context(), s.cfg.Project.ID, id, v)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"id": id, "version": v, "created_by": sv.CreatedBy, "reason": sv.Reason, "created_at": fmtT(&sv.CreatedAt), "yaml": sv.YAML})
}

// ---- verification (per deployed build) --------------------------------------

type deploymentView struct {
	Marker    string `json:"marker"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Runs      int    `json:"runs"`
	Passes    int    `json:"passes"`
	Scenarios int    `json:"scenarios"`
}

type checkView struct {
	ScenarioID   string `json:"scenario_id"`
	Title        string `json:"title"`
	Class        string `json:"class"`
	State        string `json:"state"`
	Oracle       string `json:"oracle"`
	RunID        int64  `json:"run_id"`
	Version      int    `json:"version"`
	Browser      string `json:"browser"`
	Outcome      string `json:"outcome"`
	FinishedAt   string `json:"finished_at"`
	DurationMs   int64  `json:"duration_ms"`
	FailedStep   int    `json:"failed_step"`
	FailedAction string `json:"failed_action"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
	Error        string `json:"error"`
	EvidenceRel  string `json:"evidence_rel"`
	Screenshot   string `json:"screenshot,omitempty"`
	Steps        int    `json:"steps"`
	Requests     int    `json:"requests"`
	ConsoleErrs  int    `json:"console_errors"`
	NotVerified  bool   `json:"not_verified"` // scenario exists but has no run on this deployment
}

// verification answers "what was verified on this build, with what evidence":
// /api/verification?marker=<m> (default: newest deployment).
func (s *Server) verification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	deps, _ := s.st.ListDeployments(ctx, p, 30)
	var dv []deploymentView
	for _, d := range deps {
		dv = append(dv, deploymentView{Marker: d.Marker, FirstSeen: fmtT(&d.FirstSeen), LastSeen: fmtT(&d.LastSeen), Runs: d.Runs, Passes: d.Passes, Scenarios: d.Scenarios})
	}
	marker := r.URL.Query().Get("marker")
	if marker == "" && len(deps) > 0 {
		marker = deps[0].Marker
	}
	byScenario := map[string]*model.Run{}
	if marker != "" {
		if runs, err := s.st.LatestRunsForDeployment(ctx, p, marker); err == nil {
			for _, ru := range runs {
				byScenario[ru.ScenarioID] = ru
			}
		}
	}
	var checks []checkView
	if scs, err := s.st.ListScenarios(ctx, p, model.StateActive, model.StateSoak, model.StateQuarantined, model.StateNeedsReview); err == nil {
		for _, sc := range scs {
			cv := checkView{ScenarioID: sc.ID, Title: sc.Title, Class: sc.Class, State: string(sc.State), Oracle: sc.OracleSource}
			ru := byScenario[sc.ID]
			if ru == nil {
				cv.NotVerified = true
				checks = append(checks, cv)
				continue
			}
			cv.RunID, cv.Version, cv.Browser, cv.Outcome = ru.ID, ru.ScenarioVersion, string(ru.Browser), string(ru.Outcome)
			cv.FinishedAt, cv.DurationMs = fmtT(&ru.FinishedAt), ru.DurationMs
			cv.FailedStep, cv.FailedAction, cv.Expected, cv.Actual, cv.Error = ru.FailedStep, ru.FailedAction, clip(ru.Expected, 300), clip(ru.Actual, 300), clip(ru.Error, 300)
			cv.EvidenceRel = s.rel(ru.EvidenceDir)
			cv.Steps, cv.Requests, cv.ConsoleErrs, cv.Screenshot = s.evidenceSummary(ru.EvidenceDir, cv.EvidenceRel)
			checks = append(checks, cv)
		}
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].Class != checks[j].Class {
			return checks[i].Class < checks[j].Class
		}
		return checks[i].ScenarioID < checks[j].ScenarioID
	})
	var incs []incidentView
	if list, err := s.st.ListIncidents(ctx, p, true, 20); err == nil {
		for _, in := range list {
			incs = append(incs, incidentView{ID: in.ID, Kind: string(in.Kind), Title: in.Title, Scenario: in.ScenarioID, State: in.State, CreatedAt: fmtT(&in.CreatedAt), MDRel: s.rel(in.MarkdownPath)})
		}
	}
	writeJSON(w, map[string]any{"project": p, "target": s.cfg.Target.BaseURL, "now": time.Now(), "marker": marker, "deployments": dv, "checks": checks, "incidents": incs})
}

// evidenceSummary counts what a run dir holds without loading it all into the response.
func (s *Server) evidenceSummary(dir, rel string) (steps, requests, consoleErrs int, screenshot string) {
	if dir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(dir, "steps.json")); err == nil {
		var sf struct {
			Steps []json.RawMessage `json:"steps"`
		}
		if json.Unmarshal(b, &sf) == nil {
			steps = len(sf.Steps)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "network.json")); err == nil {
		var ev []json.RawMessage
		if json.Unmarshal(b, &ev) == nil {
			requests = len(ev)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "console.json")); err == nil {
		var ev []struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(b, &ev) == nil {
			for _, e := range ev {
				if e.Level == "error" || e.Level == "exception" {
					consoleErrs++
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "screenshot.png")); err == nil && rel != "" {
		screenshot = rel + "/screenshot.png"
	}
	return
}

// runDetail bundles steps + network + console + screenshots for one run dir.
func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.safeJoin(r.URL.Query().Get("dir"))
	if !ok {
		http.Error(w, "bad dir", http.StatusBadRequest)
		return
	}
	out := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "steps.json")); err == nil {
		out["steps"] = json.RawMessage(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "network.json")); err == nil {
		var ev []struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Status int    `json:"status"`
			Mime   string `json:"mime_type"`
			Failed bool   `json:"failed"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(b, &ev) == nil {
			type nv struct {
				Method string `json:"method"`
				URL    string `json:"url"`
				Status int    `json:"status"`
				Mime   string `json:"mime"`
				Failed bool   `json:"failed"`
				Error  string `json:"error,omitempty"`
			}
			var keep []nv
			for _, e := range ev {
				// skip static assets unless they failed; keep documents, API calls and anything not 2xx
				static := strings.Contains(e.Mime, "image") || strings.Contains(e.Mime, "font") || strings.Contains(e.Mime, "css") || strings.HasSuffix(e.URL, ".js") || strings.Contains(e.URL, "/assets/")
				if !e.Failed && e.Status >= 200 && e.Status < 300 && static {
					continue
				}
				keep = append(keep, nv{e.Method, clip(e.URL, 200), e.Status, e.Mime, e.Failed, e.Error})
			}
			out["network"] = keep
			out["network_total"] = len(ev)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "console.json")); err == nil {
		out["console"] = json.RawMessage(b)
	}
	var shots []string
	entries, _ := os.ReadDir(dir)
	rel := s.rel(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			shots = append(shots, rel+"/"+e.Name())
		}
	}
	out["screenshots"] = shots
	out["dir"] = rel
	writeJSON(w, out)
}

// ---- manual QA requests (vigil request) ----------------------------------

type requestView struct {
	FeatureID   string   `json:"feature_id"`
	Summary     string   `json:"summary"`
	RequestedAt string   `json:"requested_at"`
	Dir         string   `json:"dir"`
	Status      string   `json:"status"` // running | done | none
	Decision    string   `json:"decision,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Evidence    string   `json:"evidence,omitempty"`
	Candidates  int      `json:"candidates"`
	ToolCalls   int      `json:"tool_calls"`
	Duration    string   `json:"duration,omitempty"`
	Screenshots []string `json:"screenshots"`
	VisitedURLs []string `json:"visited_urls,omitempty"`
	Gate        string   `json:"gate,omitempty"`
}

// requests lists manual QA requests (features with source=manual) with their latest agent run.
func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	feats, _ := s.st.ListFeatures(ctx, p)
	var out []requestView
	for _, f := range feats {
		if f.Source != "manual" {
			continue
		}
		rv := requestView{FeatureID: f.ID, Summary: f.Summary, RequestedAt: fmtT(&f.ShippedAt), Status: "none"}
		root := filepath.Join(s.evRoot, "agent", f.ID)
		entries, _ := os.ReadDir(root)
		var newest string
		var newestT time.Time
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			st, err := os.Stat(filepath.Join(root, e.Name(), "agent-transcript.jsonl"))
			if err == nil && st.ModTime().After(newestT) {
				newestT, newest = st.ModTime(), filepath.Join(root, e.Name())
			}
		}
		if newest != "" {
			rv.Dir = s.rel(newest)
			rv.Status = "running"
			if b, err := os.ReadFile(filepath.Join(newest, "agent-result.json")); err == nil {
				var res struct {
					Decision    string   `json:"decision"`
					Reason      string   `json:"reason"`
					Evidence    string   `json:"evidence"`
					Candidates  []string `json:"script_candidates"`
					ToolCalls   int      `json:"tool_calls"`
					Duration    int64    `json:"duration"`
					VisitedURLs []string `json:"visited_urls"`
				}
				_ = json.Unmarshal(b, &res)
				rv.Status, rv.Decision, rv.Reason, rv.Evidence = "done", res.Decision, res.Reason, clip(res.Evidence, 6000)
				rv.Candidates, rv.ToolCalls, rv.VisitedURLs = len(res.Candidates), res.ToolCalls, res.VisitedURLs
				if res.Duration > 0 {
					rv.Duration = (time.Duration(res.Duration)).Round(time.Second).String()
				}
			} else if time.Since(newestT) > 20*time.Minute {
				rv.Status = "stale"
			}
			if b, err := os.ReadFile(filepath.Join(newest, "gate.json")); err == nil {
				rv.Gate = clip(string(b), 2000)
			}
			for _, e := range entries {
				_ = e
			}
			if files, err := os.ReadDir(newest); err == nil {
				for _, fe := range files {
					if strings.HasSuffix(fe.Name(), ".png") {
						rv.Screenshots = append(rv.Screenshots, rv.Dir+"/"+fe.Name())
					}
				}
			}
		}
		out = append(out, rv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt > out[j].RequestedAt })
	writeJSON(w, map[string]any{"requests": out, "now": time.Now()})
}
