package ui

import (
	"net/http"
	"sort"
	"time"

	"vigil/internal/model"
)

// ---- /api/todo: the action queue behind the landing page --------------------
//
// The home page asks one question ("what should a human do now?") and must
// answer it with a single poll, so this payload carries the counts, the hero
// line's numbers, the next scheduled run and the last few runs together.

// heroWindow is how far back the hero line looks when it summarises what the
// loop did on its own ("최근 1시간 동안 …").
const heroWindow = time.Hour

// recentLimit is how many one-liners the 최근 활동 list shows.
const recentLimit = 5

type todoCounts struct {
	PendingApproval int `json:"pending_approval"`
	AppFailure      int `json:"app_failure"`
	NeedsReview     int `json:"needs_review"`
	Quarantined     int `json:"quarantined"`
	Findings        int `json:"findings"`
	EnvIncidents    int `json:"env_incidents"`
	Total           int `json:"total"`
}

// todoHero is what the system did without a human since roughly the last visit.
type todoHero struct {
	WindowHours int `json:"window_hours"`
	Runs        int `json:"runs"`
	Failures    int `json:"failures"`
}

type todoRecent struct {
	RunID    int64  `json:"run_id"`
	Scenario string `json:"scenario_id"`
	Title    string `json:"title"`
	Outcome  string `json:"outcome"`
	Browser  string `json:"browser"`
	Env      string `json:"environment"`
	At       string `json:"at"`
	// Cause is the one-sentence cause of a failing run (nil when it passed).
	Cause *causeView `json:"cause,omitempty"`
}

type todoPayload struct {
	Project string     `json:"project"`
	Target  string     `json:"target"`
	Now     time.Time  `json:"now"`
	Counts  todoCounts `json:"counts"`
	Hero    todoHero   `json:"hero"`
	// Recent is the newest few runs, newest first.
	Recent []todoRecent `json:"recent"`
	// NextRunAt is the soonest scheduled check; DailyAt/DailyCount describe the
	// standing daily cadence so the all-clear panel can say what happens next.
	NextRunAt  string `json:"next_run_at"`
	DailyAt    string `json:"daily_at"`
	DailyCount int    `json:"daily_count"`
	// CanRequest mirrors /api/requests: a standalone `serve` cannot run one.
	CanRequest bool `json:"can_request"`
}

// specOracle marks the scripts whose failure is a statement about the product
// (a spec or a code contract), not merely an observation of today's behaviour.
func specOracle(o string) bool { return o == "spec" || o == "contract" }

func (s *Server) todo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	now := time.Now()
	out := todoPayload{Project: p, Target: s.cfg.Target.BaseURL, Recent: []todoRecent{},
		DailyAt: s.cfg.Schedule.DailyAt, CanRequest: s.requestSubmitter != nil}

	titles := map[string]string{}
	byID := map[string]*model.Scenario{}
	var nextDue *time.Time
	if scs, err := s.st.ListScenarios(ctx, p); err == nil {
		for _, sc := range scs {
			titles[sc.ID] = sc.Title
			byID[sc.ID] = sc
			switch sc.State {
			case model.StatePendingApproval:
				out.Counts.PendingApproval++
			case model.StateNeedsReview:
				out.Counts.NeedsReview++
			case model.StateQuarantined:
				out.Counts.Quarantined++
			case model.StateActive:
				out.DailyCount++
			}
			if sc.LastOutcome == model.OutcomeAppFailure && specOracle(sc.OracleSource) {
				out.Counts.AppFailure++
			}
			if sc.State == model.StateActive || sc.State == model.StateSoak {
				if d := sc.NextDueAt; d != nil && !d.IsZero() && (nextDue == nil || d.Before(*nextDue)) {
					nextDue = d
				}
			}
		}
	}
	if list, err := s.st.ListFindings(ctx, p, true, 500); err == nil {
		out.Counts.Findings = len(list)
	}
	if list, err := s.st.ListIncidents(ctx, p, true, 50); err == nil {
		for _, in := range list {
			if in.Kind == model.IncidentEnvironment {
				out.Counts.EnvIncidents++
			}
		}
	}
	out.Counts.Total = out.Counts.PendingApproval + out.Counts.AppFailure + out.Counts.NeedsReview +
		out.Counts.Quarantined + out.Counts.Findings + out.Counts.EnvIncidents

	out.Hero.WindowHours = int(heroWindow / time.Hour)
	if runs, err := s.st.ListRuns(ctx, p, "", 200); err == nil {
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].FinishedAt.After(runs[j].FinishedAt) })
		since := now.Add(-heroWindow)
		for _, ru := range runs {
			if ru.FinishedAt.After(since) {
				out.Hero.Runs++
				if ru.Outcome == model.OutcomeAppFailure {
					out.Hero.Failures++
				}
			}
			if len(out.Recent) < recentLimit {
				out.Recent = append(out.Recent, todoRecent{RunID: ru.ID, Scenario: ru.ScenarioID, Title: titles[ru.ScenarioID],
					Outcome: string(ru.Outcome), Browser: string(ru.Browser), Env: ru.Environment, At: fmtT(&ru.FinishedAt),
					Cause: runCause(ru, byID[ru.ScenarioID])})
			}
		}
	}
	if nextDue != nil {
		out.NextRunAt = fmtT(nextDue)
	} else if s.cfg.Schedule.DailyAt != "" {
		at := s.cfg.NextDailyRun(now)
		out.NextRunAt = fmtT(&at)
	}
	writeJSONPolled(w, r, &out, func() { out.Now = time.Now() })
}
