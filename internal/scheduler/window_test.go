package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

func kst(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func businessHours() config.ActiveHours {
	return config.ActiveHours{Enabled: true, Days: config.Weekdays{1, 2, 3, 4, 5}, From: "08:00", To: "19:00", TZ: "Asia/Seoul"}
}

func readyJobs(t *testing.T, h *harness) int {
	t.Helper()
	jobs, err := h.st.ListJobs(context.Background(), "p", []model.JobState{model.JobReady}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs)
}

// Outside the window Tick must enqueue nothing, so the queue is empty when the
// window reopens instead of holding a night's worth of backlog.
func TestTickSkipsOutsideActiveHours(t *testing.T) {
	loc := kst(t)
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.cfg.Schedule.ActiveHours = businessHours()
	h.addScenario(t, "p0-active", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "p1-active", "P1", model.StateActive, model.MutationReadOnly, nil, 0)

	h.s.Now = func() time.Time { return time.Date(2026, 9, 5, 13, 0, 0, 0, loc) } // Saturday
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 0 {
		t.Fatalf("Saturday tick enqueued %d job(s), want 0", n)
	}

	h.s.Now = func() time.Time { return time.Date(2026, 9, 2, 20, 0, 0, 0, loc) } // Wed after close
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 0 {
		t.Fatalf("after-hours tick enqueued %d job(s), want 0", n)
	}

	h.s.Now = func() time.Time { return time.Date(2026, 9, 2, 9, 0, 0, 0, loc) } // Wed 09:00
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 2 {
		t.Fatalf("in-hours tick enqueued %d job(s), want 2", n)
	}
}

// The UI writes to scheduler_state; the loop is a different process and must
// pick that up without a restart, overriding vigil.yaml.
func TestActiveHoursOverrideBeatsConfig(t *testing.T) {
	loc := kst(t)
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.cfg.Schedule.ActiveHours = businessHours()
	h.addScenario(t, "p0-active", "P0", model.StateActive, model.MutationReadOnly, nil, 0)

	saturday := time.Date(2026, 9, 5, 13, 0, 0, 0, loc)
	h.s.Now = func() time.Time { return saturday }
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 0 {
		t.Fatalf("config window should block Saturday, enqueued %d", n)
	}

	// Operator opens the weekend from the dashboard.
	weekend := config.ActiveHours{Enabled: true, Days: config.Weekdays{0, 1, 2, 3, 4, 5, 6}, From: "00:00", To: "23:59", TZ: "Asia/Seoul"}
	b, _ := json.Marshal(weekend)
	if err := h.st.SetState(ctx, config.ActiveHoursStateKey, string(b)); err != nil {
		t.Fatal(err)
	}
	if got := h.s.ActiveHours(ctx); !got.Allows(saturday) {
		t.Fatal("override should allow Saturday")
	}
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 1 {
		t.Fatalf("override should let Saturday work through, enqueued %d want 1", n)
	}

	// Clearing the row falls back to vigil.yaml.
	if err := h.st.SetState(ctx, config.ActiveHoursStateKey, ""); err != nil {
		t.Fatal(err)
	}
	if h.s.ActiveHours(ctx).Allows(saturday) {
		t.Fatal("cleared override should fall back to the weekday config window")
	}
}

// A corrupt override must not wedge the loop; it falls back to config.
func TestMalformedOverrideFallsBackToConfig(t *testing.T) {
	loc := kst(t)
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.cfg.Schedule.ActiveHours = businessHours()
	if err := h.st.SetState(ctx, config.ActiveHoursStateKey, "{not json"); err != nil {
		t.Fatal(err)
	}
	got := h.s.ActiveHours(ctx)
	if !got.Allows(time.Date(2026, 9, 2, 9, 0, 0, 0, loc)) || got.Allows(time.Date(2026, 9, 5, 9, 0, 0, 0, loc)) {
		t.Fatalf("malformed override should fall back to config, got %s", got)
	}
}

// Default config (no active_hours block) keeps the historical 24/7 behaviour.
func TestNoWindowConfiguredRunsAlways(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.addScenario(t, "p0-active", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.s.Now = func() time.Time { return time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC) } // Sunday 03:00
	if err := h.s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := readyJobs(t, h); n != 1 {
		t.Fatalf("unconfigured window must not gate anything, enqueued %d want 1", n)
	}
}
