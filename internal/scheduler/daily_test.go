package scheduler

import (
	"context"
	"testing"
	"time"

	"vigil/internal/model"
)

// scheduleNext for cadence=daily lands on the next schedule.daily_at slot in
// active_hours.tz regardless of outcome; PENDING_APPROVAL scripts stay undue.
func TestScheduleNextDailyCadence(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	ctx := context.Background()
	h.cfg.Schedule.DailyAt = "09:00"
	h.cfg.Schedule.ActiveHours.TZ = "Asia/Seoul"
	seoul, _ := time.LoadLocation("Asia/Seoul")
	h.addScenario(t, "daily", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	h.addScenario(t, "pending", "P0", model.StatePendingApproval, model.MutationReadOnly, nil, 0)

	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 9, 10, 8, 59, 0, 0, seoul), time.Date(2026, 9, 10, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 9, 10, 9, 0, 30, 0, seoul), time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 9, 10, 23, 59, 0, 0, seoul), time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC), time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)}, // 10:00 KST
	}
	for _, c := range cases {
		h.s.Now = func() time.Time { return c.now.UTC() }
		if err := h.st.SetScenarioApproved(ctx, "p", "daily", "daily", c.now, c.now); err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []model.Outcome{model.OutcomePass, model.OutcomeAppFailure} {
			sc, _ := h.st.GetScenario(ctx, "p", "daily")
			h.s.scheduleNext(ctx, sc, outcome)
			sc, _ = h.st.GetScenario(ctx, "p", "daily")
			if sc.NextDueAt == nil || !sc.NextDueAt.Equal(c.want) {
				t.Fatalf("now=%s outcome=%s: next due %v, want %s", c.now, outcome, sc.NextDueAt, c.want)
			}
		}
	}
	// a P0 without cadence keeps the interval schedule
	h.s.Now = func() time.Time { return time.Date(2026, 9, 10, 8, 59, 0, 0, seoul).UTC() }
	h.addScenario(t, "plain", "P0", model.StateActive, model.MutationReadOnly, nil, 0)
	sc, _ := h.st.GetScenario(ctx, "p", "plain")
	h.s.scheduleNext(ctx, sc, model.OutcomePass)
	sc, _ = h.st.GetScenario(ctx, "p", "plain")
	if want := h.s.Now().Add(h.cfg.Schedule.P0.Duration); sc.NextDueAt == nil || !sc.NextDueAt.Equal(want) {
		t.Fatalf("plain next due %v, want %s", sc.NextDueAt, want)
	}
	sc, _ = h.st.GetScenario(ctx, "p", "pending")
	h.s.scheduleNext(ctx, sc, model.OutcomePass)
	if sc, _ = h.st.GetScenario(ctx, "p", "pending"); sc.NextDueAt != nil {
		t.Fatalf("pending must stay undue: %v", sc.NextDueAt)
	}
}
