package ui

import (
	"context"

	"vigil/internal/explain"
	"vigil/internal/model"
)

// causeView is the progressive-disclosure payload of one failing row: level 1
// is the headline with its kind, level 2 the why/next, level 3 the raw detail.
// The wording comes from internal/explain, never from a page template.
type causeView struct {
	Headline string `json:"headline"`
	Kind     string `json:"kind"`
	Why      string `json:"why"`
	Detail   string `json:"detail"`
	Next     string `json:"next"`
}

func toCauseView(c explain.Cause) *causeView {
	if c.Empty() {
		return nil
	}
	return &causeView{Headline: c.Headline, Kind: c.Kind, Why: c.Why, Detail: c.Detail, Next: c.Next}
}

// runCause explains one run for a row (nil when the run passed).
func runCause(r *model.Run, sc *model.Scenario) *causeView { return toCauseView(explain.ForRun(r, sc)) }

// incidentViews renders open incidents with the cause of the run behind each
// one. The runs load in a single query, so the payload keeps its query budget.
func (s *Server) incidentViews(ctx context.Context, list []*model.Incident, scenarios map[string]*model.Scenario) []incidentView {
	out := make([]incidentView, 0, len(list))
	ids := make([]int64, 0, len(list))
	for _, in := range list {
		if in.RunID != 0 {
			ids = append(ids, in.RunID)
		}
	}
	runs, _ := s.st.RunsByID(ctx, s.cfg.Project.ID, ids)
	for _, in := range list {
		out = append(out, incidentView{ID: in.ID, Kind: string(in.Kind), Title: in.Title, Scenario: in.ScenarioID,
			State: in.State, CreatedAt: fmtT(&in.CreatedAt), MDRel: s.rel(in.MarkdownPath),
			Cause: toCauseView(explain.ForIncident(in, runs[in.RunID], scenarios[in.ScenarioID]))})
	}
	return out
}
