package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"vigil/internal/config"
)

// The dashboard mirrors the supervisor's result rather than importing the
// orchestrator: this package stays light, and the JSON in scheduler_state is
// the contract between them.

type supervisorAction struct {
	Verb   string `json:"verb"`
	Target string `json:"target"`
	Reason string `json:"reason"`
	Class  string `json:"class,omitempty"`
}

type supervisorVerdict struct {
	Action   supervisorAction `json:"action"`
	Accepted bool             `json:"accepted"`
	Reason   string           `json:"reason,omitempty"`
	Applied  bool             `json:"applied,omitempty"`
}

type supervisorView struct {
	Enabled      bool                `json:"enabled"`
	DryRun       bool                `json:"dry_run"`
	Tick         string              `json:"tick"`
	Model        string              `json:"model"`
	At           *time.Time          `json:"at,omitempty"`
	Assessment   string              `json:"assessment,omitempty"`
	CoverageGaps []string            `json:"coverage_gaps,omitempty"`
	Saturated    bool                `json:"saturated,omitempty"`
	Skipped      string              `json:"skipped,omitempty"`
	Verdicts     []supervisorVerdict `json:"verdicts,omitempty"`
	EvidenceDir  string              `json:"evidence_dir,omitempty"`
	// Applied/Refused are precomputed so the page does not have to count.
	Applied int `json:"applied"`
	Refused int `json:"refused"`
	// KnownRoutes is the discovered-route inventory, newest first.
	KnownRoutes []string `json:"known_routes,omitempty"`
	RouteCount  int      `json:"route_count"`
	// Ran is false before the first tick, so the page can say "not yet" rather
	// than showing an empty plan as if the model had nothing to say.
	Ran bool `json:"ran"`
}

func (s *Server) supervisorState(ctx context.Context) *supervisorView {
	sup := s.cfg.Supervisor
	v := &supervisorView{
		Enabled: sup.Enabled,
		DryRun:  sup.DryRunEnabled(),
		Tick:    sup.Tick.Duration.String(),
		Model:   sup.EffectiveModel(s.cfg.Agent.Model),
	}
	if raw, err := s.st.GetState(ctx, config.SupervisorStateKey); err == nil && strings.TrimSpace(raw) != "" {
		var last struct {
			At           time.Time           `json:"at"`
			Assessment   string              `json:"assessment"`
			CoverageGaps []string            `json:"coverage_gaps"`
			Saturated    bool                `json:"saturated"`
			Verdicts     []supervisorVerdict `json:"verdicts"`
			DryRun       bool                `json:"dry_run"`
			Skipped      string              `json:"skipped"`
			EvidenceDir  string              `json:"evidence_dir"`
		}
		if json.Unmarshal([]byte(raw), &last) == nil {
			v.Ran = true
			v.At, v.Assessment, v.CoverageGaps = &last.At, last.Assessment, last.CoverageGaps
			v.Saturated, v.Verdicts, v.Skipped = last.Saturated, last.Verdicts, last.Skipped
			v.DryRun, v.EvidenceDir = last.DryRun, last.EvidenceDir
			for _, vd := range last.Verdicts {
				switch {
				case vd.Applied:
					v.Applied++
				case !vd.Accepted:
					v.Refused++
				}
			}
		}
	}
	if raw, err := s.st.GetState(ctx, config.SiteRoutesKey); err == nil && strings.TrimSpace(raw) != "" {
		seen := map[string]string{}
		if json.Unmarshal([]byte(raw), &seen) == nil {
			for r := range seen {
				v.KnownRoutes = append(v.KnownRoutes, r)
			}
			sort.Slice(v.KnownRoutes, func(i, j int) bool {
				a, b := seen[v.KnownRoutes[i]], seen[v.KnownRoutes[j]]
				if a != b {
					return a > b // newest first
				}
				return v.KnownRoutes[i] < v.KnownRoutes[j]
			})
			v.RouteCount = len(v.KnownRoutes)
			if len(v.KnownRoutes) > 40 {
				v.KnownRoutes = v.KnownRoutes[:40]
			}
		}
	}
	return v
}

// supervisor is read-only: the dashboard shows what the loop decided, it never
// drives it. Changing the supervisor is a config edit, on purpose.
func (s *Server) supervisor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.supervisorState(r.Context()))
}
