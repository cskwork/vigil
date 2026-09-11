package ui

import (
	"net/http"
	"strconv"
	"time"

	"vigil/internal/approval"
	"vigil/internal/model"
)

// ---- scripts page: every deterministic script (seed, agent, repair, system) --

// scriptRunHistory is how many past runs the detail panel lists per script.
const scriptRunHistory = 5

type scriptView struct {
	scenarioView
	Versions []versionView        `json:"versions"`
	Links    []model.CoverageLink `json:"links"`
	Locks    []string             `json:"locks"`
	Mutation string               `json:"mutation"`
	YAML     string               `json:"yaml"`
	SoakN    int                  `json:"soak_passes"`
	SoakT    int                  `json:"soak_target"`
	// Approval workflow: where a reproduce script came from, its verdict, and
	// the run the verdict is based on (evidence path for the last-run link).
	SourceKind   string            `json:"source_kind,omitempty"`
	SourceRef    string            `json:"source_ref,omitempty"`
	Cadence      string            `json:"cadence,omitempty"`
	ApprovedAt   string            `json:"approved_at,omitempty"`
	Reproduction *reproductionView `json:"reproduction,omitempty"`
	LastRunDir   string            `json:"last_run_dir,omitempty"`
	LastRunID    int64             `json:"last_run_id,omitempty"`
	// Findings are the data-analyst observations attached to this script (newest
	// first, OPEN and RESOLVED); OpenFindings feeds the list badge.
	Findings     []findingView `json:"findings"`
	OpenFindings int           `json:"open_findings"`
	// RecentRuns is the "최근 실행" section of the detail panel: at most five, newest
	// first. They come from one recent-runs query for the whole page, so a script
	// that has not run lately simply has none.
	RecentRuns []scriptRunView `json:"recent_runs"`
}

// scriptRunView is one line of a script's run history.
type scriptRunView struct {
	RunID        int64  `json:"run_id"`
	Outcome      string `json:"outcome"`
	Browser      string `json:"browser"`
	Environment  string `json:"environment"`
	FinishedAt   string `json:"finished_at"`
	DurationMs   int64  `json:"duration_ms"`
	FailedStep   int    `json:"failed_step"`
	FailedAction string `json:"failed_action,omitempty"`
	Actual       string `json:"actual,omitempty"`
	Error        string `json:"error,omitempty"`
	EvidenceRel  string `json:"evidence_rel,omitempty"`
	// Cause is the one-sentence cause of a failing run (nil when it passed).
	Cause *causeView `json:"cause,omitempty"`
}

type findingView struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Where     string `json:"where"`
	Expected  string `json:"expected"`
	Actual    string `json:"actual"`
	Evidence  string `json:"evidence"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

func toFindingView(f *model.Finding) findingView {
	return findingView{ID: f.ID, Kind: f.Kind, Where: f.Where, Expected: f.Expected, Actual: f.Actual, Evidence: f.Evidence, State: f.State, CreatedAt: fmtT(&f.CreatedAt)}
}

type reproductionView struct {
	Verdict     string `json:"verdict"`
	Reproduced  bool   `json:"reproduced"`
	AtStep      int    `json:"at_step"`
	ClaimedStep int    `json:"claimed_step,omitempty"`
	Symptom     string `json:"symptom"`
	Why         string `json:"why,omitempty"`
	RunID       int64  `json:"run_id"`
}

type versionView struct {
	Version   int    `json:"version"`
	CreatedBy string `json:"created_by"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	Current   bool   `json:"current"`
}

type scriptsPayload struct {
	Project string       `json:"project"`
	Target  string       `json:"target"`
	Now     time.Time    `json:"now"`
	Scripts []scriptView `json:"scripts"`
	// Envs/DefaultEnv feed the ▶ 실행 environment select; Actions says which
	// buttons this server can honour (run needs a scheduler in-process).
	Envs       []string `json:"envs"`
	DefaultEnv string   `json:"default_env"`
	Actions    struct {
		Run     bool `json:"run"`
		Approve bool `json:"approve"`
	} `json:"actions"`
	DailyAt string `json:"daily_at"`
}

func (s *Server) scripts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	scs, err := s.st.ListScenarios(ctx, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Four queries for the whole page instead of three per scenario, and only
	// the current version's YAML is shipped; older bodies load on demand.
	current, _ := s.st.ListCurrentScenarioVersions(ctx, p)
	history, _ := s.st.ListScenarioVersionHistory(ctx, p)
	links, _ := s.st.ListAllCoverageLinks(ctx, p)
	metrics, _ := s.st.ListScenarioMetrics(ctx, p)
	findings := map[string][]findingView{}
	openFindings := map[string]int{}
	if list, err := s.st.ListFindings(ctx, p, false, 500); err == nil {
		for _, f := range list {
			if f.ScenarioID == "" {
				continue
			}
			findings[f.ScenarioID] = append(findings[f.ScenarioID], toFindingView(f))
			if f.State == "OPEN" {
				openFindings[f.ScenarioID]++
			}
		}
	}

	// One recent-runs query feeds every script's 최근 실행 section (at most five
	// each) instead of one query per script.
	recent := map[string][]*model.Run{}
	if runs, err := s.st.ListRuns(ctx, p, "", 400); err == nil {
		for _, ru := range runs {
			if len(recent[ru.ScenarioID]) >= scriptRunHistory {
				continue
			}
			recent[ru.ScenarioID] = append(recent[ru.ScenarioID], ru)
		}
	}

	out := scriptsPayload{Project: p, Target: s.cfg.Target.BaseURL, Scripts: make([]scriptView, 0, len(scs)), Envs: s.cfg.EnvNames(), DefaultEnv: s.cfg.DefaultEnv().Name, DailyAt: s.cfg.Schedule.DailyAt}
	out.Actions.Approve = s.scriptActions != nil
	out.Actions.Run = s.scriptActions != nil && s.scriptActions.CanRun()
	for _, sc := range scs {
		sv := scriptView{scenarioView: scenarioView{ID: sc.ID, Title: sc.Title, State: string(sc.State), Class: sc.Class, LastOutcome: string(sc.LastOutcome),
			LastRunAt: fmtT(sc.LastRunAt), NextDueAt: fmtT(sc.NextDueAt), Failures: sc.ConsecutiveFailures, Origin: sc.Origin, Oracle: sc.OracleSource},
			Locks: sc.Locks, Mutation: string(sc.Mutation), SoakN: sc.SoakPasses, SoakT: sc.SoakTarget, Versions: []versionView{}, Links: []model.CoverageLink{}}
		if sv.Locks == nil {
			sv.Locks = []string{}
		}
		sv.Browser = s.cfg.Browser.Primary
		if sc.State == model.StateSoak {
			sv.Soak = itoa(sc.SoakPasses) + "/" + itoa(sc.SoakTarget)
		}
		for _, v := range history[sc.ID] {
			sv.Versions = append(sv.Versions, versionView{Version: v.Version, CreatedBy: v.CreatedBy, Reason: v.Reason, CreatedAt: fmtT(&v.CreatedAt), Current: v.Version == sc.CurrentVersion})
		}
		if v := current[sc.ID]; v != nil {
			sv.YAML = v.YAML
			if b := primaryBrowserOf(v.YAML); b != "" {
				sv.Browser = b
			}
		}
		if l := links[sc.ID]; l != nil {
			sv.Links = l
		}
		if m := metrics[sc.ID]; m != nil {
			sv.Runs, sv.Passes, sv.Flakes = m.Runs, m.Passes, m.Flakes
		}
		sv.SourceKind, sv.SourceRef, sv.Cadence, sv.ApprovedAt = sc.SourceKind, sc.SourceRef, sc.Cadence, fmtT(sc.ApprovedAt)
		sv.Findings, sv.OpenFindings = findings[sc.ID], openFindings[sc.ID]
		sv.RecentRuns = []scriptRunView{}
		for _, ru := range recent[sc.ID] {
			sv.RecentRuns = append(sv.RecentRuns, scriptRunView{RunID: ru.ID, Outcome: string(ru.Outcome), Browser: string(ru.Browser),
				Environment: ru.Environment, FinishedAt: fmtT(&ru.FinishedAt), DurationMs: ru.DurationMs, FailedStep: ru.FailedStep,
				FailedAction: ru.FailedAction, Actual: clip(ru.Actual, 200), Error: clip(ru.Error, 200), EvidenceRel: s.rel(ru.EvidenceDir),
				Cause: runCause(ru, sc)})
		}
		if sv.Findings == nil {
			sv.Findings = []findingView{}
		}
		if rep, ok := approval.ParseReproduction(sc.Reproduction); ok {
			sv.Reproduction = &reproductionView{Verdict: rep.Verdict, Reproduced: rep.Reproduced, AtStep: rep.AtStep, ClaimedStep: rep.ClaimedStep, Symptom: rep.Symptom, Why: rep.Why, RunID: rep.RunID}
		}
		if sc.State == model.StatePendingApproval || sc.State == model.StateNeedsReview {
			// Only the cards with action buttons need the last-run link; one
			// small query each keeps the page's fixed query count for the rest.
			if runs, err := s.st.ListRuns(ctx, p, sc.ID, 1); err == nil && len(runs) > 0 {
				sv.LastRunID, sv.LastRunDir = runs[0].ID, s.rel(runs[0].EvidenceDir)
			}
		}
		out.Scripts = append(out.Scripts, sv)
	}
	writeJSONPolled(w, r, &out, func() { out.Now = time.Now() })
}

// script returns one version's YAML: /api/script?id=<scenario>&v=<n>
func (s *Server) script(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	v, err := strconv.Atoi(r.URL.Query().Get("v"))
	if id == "" || err != nil || v <= 0 {
		http.Error(w, "id and v required", http.StatusBadRequest)
		return
	}
	sv, err := s.st.GetScenarioVersion(r.Context(), s.cfg.Project.ID, id, v)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// A version row is immutable once written.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSONBody(w, map[string]any{"id": id, "version": v, "created_by": sv.CreatedBy, "reason": sv.Reason, "created_at": fmtT(&sv.CreatedAt), "yaml": sv.YAML})
}
