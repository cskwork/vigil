package config

import (
	"testing"
	"time"
)

func seoul(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

// weekdays 08:00-19:00 Asia/Seoul, the window this feature was built for.
func businessHours() ActiveHours {
	return ActiveHours{Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "08:00", To: "19:00", TZ: "Asia/Seoul"}
}

func TestAllowsWeekdayWindow(t *testing.T) {
	loc := seoul(t)
	w := businessHours()
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"wed 07:59 just before open", time.Date(2026, 9, 2, 7, 59, 0, 0, loc), false},
		{"wed 08:00 open is inclusive", time.Date(2026, 9, 2, 8, 0, 0, 0, loc), true},
		{"wed 13:00 midday", time.Date(2026, 9, 2, 13, 0, 0, 0, loc), true},
		{"wed 18:59 last minute", time.Date(2026, 9, 2, 18, 59, 0, 0, loc), true},
		{"wed 19:00 close is exclusive", time.Date(2026, 9, 2, 19, 0, 0, 0, loc), false},
		{"sat 13:00 weekend", time.Date(2026, 9, 5, 13, 0, 0, 0, loc), false},
		{"sun 13:00 weekend", time.Date(2026, 9, 6, 13, 0, 0, 0, loc), false},
	}
	for _, c := range cases {
		if got := w.Allows(c.at); got != c.want {
			t.Errorf("%s: Allows=%v want %v", c.name, got, c.want)
		}
	}
}

// The scheduler's Now() is UTC. A window written as 08:00-19:00 must mean
// 08:00-19:00 in TZ, not in UTC, or it silently runs at the wrong time of day.
func TestAllowsConvertsFromUTC(t *testing.T) {
	w := businessHours()
	// 2026-09-02 23:00 UTC == 2026-09-03 08:00 KST → inside the window.
	if !w.Allows(time.Date(2026, 9, 2, 23, 0, 0, 0, time.UTC)) {
		t.Error("23:00 UTC Wed should be 08:00 KST Thu (inside)")
	}
	// 2026-09-02 08:00 UTC == 17:00 KST → inside, but proves we are not
	// comparing raw UTC clock fields.
	if !w.Allows(time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)) {
		t.Error("08:00 UTC Wed should be 17:00 KST (inside)")
	}
	// 2026-09-04 12:00 UTC == 21:00 KST Fri → outside.
	if w.Allows(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)) {
		t.Error("12:00 UTC Fri should be 21:00 KST (outside)")
	}
	// 2026-09-05 20:00 UTC == 05:00 KST Sunday → outside (weekend).
	if w.Allows(time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)) {
		t.Error("20:00 UTC Sat should be 05:00 KST Sun (outside)")
	}
}

func TestAllowsOvernightWindow(t *testing.T) {
	loc := seoul(t)
	// Weeknights 22:00 → 06:00. The day list matches the day the window OPENS.
	w := ActiveHours{Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "22:00", To: "06:00", TZ: "Asia/Seoul"}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"wed 21:59 before open", time.Date(2026, 9, 2, 21, 59, 0, 0, loc), false},
		{"wed 22:00 open", time.Date(2026, 9, 2, 22, 0, 0, 0, loc), true},
		{"thu 02:00 spillover from wed", time.Date(2026, 9, 3, 2, 0, 0, 0, loc), true},
		{"thu 06:00 close is exclusive", time.Date(2026, 9, 3, 6, 0, 0, 0, loc), false},
		{"sat 02:00 spillover from fri is allowed", time.Date(2026, 9, 5, 2, 0, 0, 0, loc), true},
		{"sat 23:00 saturday never opens", time.Date(2026, 9, 5, 23, 0, 0, 0, loc), false},
		{"mon 02:00 spillover would come from sunday", time.Date(2026, 9, 7, 2, 0, 0, 0, loc), false},
	}
	for _, c := range cases {
		if got := w.Allows(c.at); got != c.want {
			t.Errorf("%s: Allows=%v want %v", c.name, got, c.want)
		}
	}
}

// A malformed window must never silently stop QA; Validate is where bad input
// is rejected, Allows is where we stay safe.
func TestAllowsFailsOpen(t *testing.T) {
	base := time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC) // Sat 12:00 KST: outside a weekday window
	for _, w := range []ActiveHours{
		{Enabled: false, Days: Weekdays{1}, From: "08:00", To: "19:00"},
		{Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "8am", To: "19:00", TZ: "Asia/Seoul"},
		{Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "08:00", To: "25:00", TZ: "Asia/Seoul"},
		{Enabled: true, Days: Weekdays{}, From: "08:00", To: "19:00", TZ: "Asia/Seoul"},
		{Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "08:00", To: "08:00", TZ: "Asia/Seoul"},
	} {
		if !w.Allows(base) {
			t.Errorf("%+v: should fail open, blocked instead", w)
		}
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		name string
		w    ActiveHours
	}{
		{"bad from", ActiveHours{Enabled: true, Days: Weekdays{1}, From: "8am", To: "19:00"}},
		{"hour out of range", ActiveHours{Enabled: true, Days: Weekdays{1}, From: "08:00", To: "25:00"}},
		{"empty days", ActiveHours{Enabled: true, Days: Weekdays{}, From: "08:00", To: "19:00"}},
		{"weekday out of range", ActiveHours{Enabled: true, Days: Weekdays{9}, From: "08:00", To: "19:00"}},
		{"empty window", ActiveHours{Enabled: true, Days: Weekdays{1}, From: "08:00", To: "08:00"}},
		{"unknown tz", ActiveHours{Enabled: true, Days: Weekdays{1}, From: "08:00", To: "19:00", TZ: "Mars/Olympus"}},
	} {
		if err := c.w.Validate(); err == nil {
			t.Errorf("%s: expected an error, got nil", c.name)
		}
	}
	if err := (ActiveHours{}).Validate(); err != nil {
		t.Errorf("disabled zero value should validate, got %v", err)
	}
	if err := businessHours().Validate(); err != nil {
		t.Errorf("business hours should validate, got %v", err)
	}
}

func TestNextChange(t *testing.T) {
	loc := seoul(t)
	w := businessHours()
	// Wed 13:00 → closes at 19:00 the same day.
	got := w.NextChange(time.Date(2026, 9, 2, 13, 0, 0, 0, loc))
	if want := time.Date(2026, 9, 2, 19, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("NextChange from Wed 13:00 = %v, want %v", got, want)
	}
	// Fri 20:00 → reopens Mon 08:00.
	got = w.NextChange(time.Date(2026, 9, 4, 20, 0, 0, 0, loc))
	if want := time.Date(2026, 9, 7, 8, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("NextChange from Fri 20:00 = %v, want %v", got, want)
	}
	if got := (ActiveHours{}).NextChange(time.Now()); !got.IsZero() {
		t.Errorf("disabled window never changes, got %v", got)
	}
}

func TestParseWeekday(t *testing.T) {
	for in, want := range map[string]int{"mon": 1, "Monday": 1, "월": 1, "0": 0, "6": 6, "SUN": 0} {
		got, ok := ParseWeekday(in)
		if !ok || got != want {
			t.Errorf("ParseWeekday(%q) = %d,%v want %d,true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "funday", "7", "-1"} {
		if _, ok := ParseWeekday(in); ok {
			t.Errorf("ParseWeekday(%q) should fail", in)
		}
	}
}
