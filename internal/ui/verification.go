package ui

import (
	"net/http"
	"sort"
	"time"

	"vigil/internal/model"
)

// ---- /api/verification: what was verified on one deployed build -------------

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
	Key          string `json:"key"`
	Title        string `json:"title"`
	Class        string `json:"class"`
	State        string `json:"state"`
	Oracle       string `json:"oracle"`
	RunID        int64  `json:"run_id"`
	Version      int    `json:"version"`
	Browser      string `json:"browser"`
	Environment  string `json:"environment"`
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
	// Cause is the one-sentence cause of a failing check (nil when it passed).
	Cause       *causeView `json:"cause,omitempty"`
	NotVerified bool       `json:"not_verified"` // scenario exists but has no run on this deployment
}

type verificationPayload struct {
	Project     string           `json:"project"`
	Target      string           `json:"target"`
	Now         time.Time        `json:"now"`
	Marker      string           `json:"marker"`
	Deployments []deploymentView `json:"deployments"`
	Checks      []checkView      `json:"checks"`
	Incidents   []incidentView   `json:"incidents"`
	// OpenFindings counts the data-analyst findings still OPEN for the project.
	OpenFindings int  `json:"open_findings"`
	OnDemand     bool `json:"on_demand"`
}

// verification answers "what was verified on this build, with what evidence":
// /api/verification?marker=<m> (default: newest deployment).
func (s *Server) verification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	out := verificationPayload{Project: p, Target: s.cfg.Target.BaseURL, Deployments: []deploymentView{}, Checks: []checkView{}, Incidents: []incidentView{}}
	deps, _ := s.st.ListDeployments(ctx, p, 30)
	for _, d := range deps {
		out.Deployments = append(out.Deployments, deploymentView{Marker: d.Marker, FirstSeen: fmtT(&d.FirstSeen), LastSeen: fmtT(&d.LastSeen), Runs: d.Runs, Passes: d.Passes, Scenarios: d.Scenarios})
	}
	marker := r.URL.Query().Get("marker")
	if marker == "" && len(deps) > 0 {
		marker = deps[0].Marker
	}
	out.Marker = marker
	out.OnDemand = s.onDemand
	byScenario := map[string]*model.Run{}
	if marker != "" {
		if runs, err := s.st.LatestRunsForDeployment(ctx, p, marker); err == nil {
			for _, ru := range runs {
				byScenario[ru.ScenarioID] = ru
			}
		}
	}
	byID := map[string]*model.Scenario{}
	bySite := map[string][]*model.Run{}
	states := []model.ScenarioState{model.StateActive, model.StateSoak, model.StateQuarantined, model.StateNeedsReview}
	if s.onDemand {
		runs, err := s.st.LatestRunsByEnvironment(ctx, p)
		if err != nil {
			writeAPIError(w, 500, "검증 결과를 불러오지 못했습니다")
			return
		}
		for _, run := range runs {
			bySite[run.ScenarioID] = append(bySite[run.ScenarioID], run)
		}
		states = nil
	}
	scs, err := s.st.ListScenarios(ctx, p, states...)
	if err != nil {
		writeAPIError(w, 500, "검사 목록을 불러오지 못했습니다")
		return
	}
	for _, sc := range scs {
		byID[sc.ID] = sc
		runs := []*model.Run{byScenario[sc.ID]}
		if s.onDemand {
			runs = bySite[sc.ID]
			if len(runs) == 0 {
				runs = []*model.Run{nil}
			}
		}
		for _, ru := range runs {
			cv := checkView{ScenarioID: sc.ID, Key: sc.ID, Title: sc.Title, Class: sc.Class, State: string(sc.State), Oracle: sc.OracleSource}
			if ru == nil {
				cv.NotVerified = true
				out.Checks = append(out.Checks, cv)
				continue
			}
			if s.onDemand {
				cv.Key = sc.ID + "@" + ru.Environment
			}
			cv.RunID, cv.Version, cv.Browser, cv.Outcome = ru.ID, ru.ScenarioVersion, string(ru.Browser), string(ru.Outcome)
			cv.Environment = ru.Environment
			cv.FinishedAt, cv.DurationMs = fmtT(&ru.FinishedAt), ru.DurationMs
			cv.FailedStep, cv.FailedAction, cv.Expected, cv.Actual, cv.Error = ru.FailedStep, ru.FailedAction, clip(ru.Expected, 300), clip(ru.Actual, 300), clip(ru.Error, 300)
			cv.EvidenceRel = s.rel(ru.EvidenceDir)
			cv.Steps, cv.Requests, cv.ConsoleErrs, cv.Screenshot = s.evidenceSummary(ru.EvidenceDir, cv.EvidenceRel)
			cv.Cause = runCause(ru, sc)
			out.Checks = append(out.Checks, cv)
		}
	}

	sort.Slice(out.Checks, func(i, j int) bool {
		if out.Checks[i].Class != out.Checks[j].Class {
			return out.Checks[i].Class < out.Checks[j].Class
		}
		return out.Checks[i].ScenarioID < out.Checks[j].ScenarioID
	})
	if list, err := s.st.ListIncidents(ctx, p, true, 20); err == nil {
		out.Incidents = s.incidentViews(ctx, list, byID)
	}
	if list, err := s.st.ListFindings(ctx, p, true, 1000); err == nil {
		out.OpenFindings = len(list)
	}
	writeJSONPolled(w, r, &out, func() { out.Now = time.Now() })
}
