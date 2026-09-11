package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// Temporary: times each store call the overview makes against the real local DB.
func TestZZProfileOverview(t *testing.T) {
	root := os.Getenv("VIGIL_PROFILE_ROOT")
	if root == "" {
		t.Skip("set VIGIL_PROFILE_ROOT")
	}
	cfg, err := config.Load(root + "/vigil.yaml")
	if err != nil {
		t.Skip(err)
	}
	st, err := store.Open(root + "/.vigil/state.db")
	if err != nil {
		t.Skip(err)
	}
	defer st.Close()
	ctx := context.Background()
	p := cfg.Project.ID
	tm := func(name string, f func()) { t0 := time.Now(); f(); t.Logf("%-28s %v", name, time.Since(t0)) }
	tm("Counts", func() { _, _ = st.Counts(ctx, p) })
	tm("BudgetUsed browser", func() { _, _ = st.BudgetUsed(ctx, p, "browser", time.Hour) })
	tm("BudgetUsed agent", func() { _, _ = st.BudgetUsed(ctx, p, "agent", time.Hour) })
	tm("ListScenarios", func() { _, _ = st.ListScenarios(ctx, p) })
	tm("ListCurrentScenarioVersions", func() { _, _ = st.ListCurrentScenarioVersions(ctx, p) })
	tm("ListScenarioMetrics", func() { _, _ = st.ListScenarioMetrics(ctx, p) })
	tm("ListJobs leased", func() { _, _ = st.ListJobs(ctx, p, []model.JobState{model.JobLeased}, 20) })
	tm("ListJobs ready", func() { _, _ = st.ListJobs(ctx, p, []model.JobState{model.JobReady}, 15) })
	tm("ListRuns 200", func() { _, _ = st.ListRuns(ctx, p, "", 200) })
	tm("ListIncidents", func() { _, _ = st.ListIncidents(ctx, p, false, 20) })
	tm("ListFeatures", func() { _, _ = st.ListFeatures(ctx, p) })
	tm("ListDeployments", func() { _, _ = st.ListDeployments(ctx, p, 30) })
	tm("GetState", func() { _, _ = st.GetState(ctx, config.ActiveHoursStateKey) })
	s := New(cfg, st, cfg.Abs(cfg.Evidence.Dir))
	tm("windowState", func() { _ = s.windowState(ctx) })
	tm("supervisorState", func() { _ = s.supervisorState(ctx) })
	tm("latestAgent cold", func() { _ = s.latestAgent() })
	tm("latestAgent warm", func() { _ = s.latestAgent() })
	for i := 0; i < 3; i++ {
		tm("overview handler", func() {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
		})
	}
}
