package config

import (
	"testing"
	"time"
)

// bruteNextChange is the original minute-by-minute definition; the boundary
// walk in NextChange must agree with it on every shape of window.
func bruteNextChange(a ActiveHours, t time.Time) time.Time {
	if !a.Enabled {
		return time.Time{}
	}
	now := a.Allows(t)
	cur := t.In(a.Location()).Truncate(time.Minute)
	for i := 1; i <= 8*24*60; i++ {
		at := cur.Add(time.Duration(i) * time.Minute)
		if a.Allows(at) != now {
			return at
		}
	}
	return time.Time{}
}

func TestNextChangeMatchesMinuteScan(t *testing.T) {
	loc := seoul(t)
	windows := map[string]ActiveHours{
		"weekdays":   businessHours(),
		"overnight":  {Enabled: true, Days: Weekdays{1, 2, 3, 4, 5}, From: "22:00", To: "06:00", TZ: "Asia/Seoul"},
		"weekend":    {Enabled: true, Days: Weekdays{0, 6}, From: "10:00", To: "14:30", TZ: "Asia/Seoul"},
		"single day": {Enabled: true, Days: Weekdays{3}, From: "09:15", To: "09:45", TZ: "Asia/Seoul"},
		"every day":  {Enabled: true, Days: Weekdays{0, 1, 2, 3, 4, 5, 6}, From: "00:00", To: "23:59", TZ: "Asia/Seoul"},
		"bad from":   {Enabled: true, Days: Weekdays{1}, From: "x", To: "09:00", TZ: "Asia/Seoul"},
		"no days":    {Enabled: true, Days: Weekdays{}, From: "08:00", To: "09:00", TZ: "Asia/Seoul"},
	}
	starts := []time.Time{
		time.Date(2026, 9, 2, 13, 0, 0, 0, loc),  // Wed midday
		time.Date(2026, 9, 4, 20, 0, 0, 0, loc),  // Fri evening
		time.Date(2026, 9, 5, 2, 30, 0, 0, loc),  // Sat small hours (inside an overnight Fri window)
		time.Date(2026, 9, 6, 23, 59, 0, 0, loc), // Sun last minute
		time.Date(2026, 9, 2, 8, 0, 0, 0, loc),   // exactly at open
		time.Date(2026, 9, 2, 19, 0, 0, 0, loc),  // exactly at close
		time.Date(2026, 9, 2, 8, 0, 30, 0, loc),  // 30s after open (sub-minute)
	}
	for name, w := range windows {
		for _, at := range starts {
			got, want := w.NextChange(at), bruteNextChange(w, at)
			if !got.Equal(want) {
				t.Errorf("%s from %v: NextChange=%v, minute scan=%v", name, at, got, want)
			}
		}
	}
}

func TestNextChangeIsCheap(t *testing.T) {
	w := businessHours()
	at := time.Date(2026, 9, 4, 20, 0, 0, 0, seoul(t)) // Fri evening: the answer is 2.5 days away
	t0 := time.Now()
	for i := 0; i < 200; i++ {
		_ = w.NextChange(at)
	}
	if el := time.Since(t0); el > 200*time.Millisecond {
		t.Fatalf("200 NextChange calls took %v; the dashboard polls this every few seconds", el)
	}
}

func TestNextDailyAt(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	// 2026-09-10 08:30 KST → today 09:00 KST
	now := time.Date(2026, 9, 10, 8, 30, 0, 0, seoul)
	if got := NextDailyAt(now, "09:00", "Asia/Seoul"); !got.Equal(time.Date(2026, 9, 10, 9, 0, 0, 0, seoul)) {
		t.Fatalf("before slot: %s", got)
	}
	// exactly at the slot → tomorrow (strictly after)
	now = time.Date(2026, 9, 10, 9, 0, 0, 0, seoul)
	if got := NextDailyAt(now, "09:00", "Asia/Seoul"); !got.Equal(time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)) {
		t.Fatalf("at slot: %s", got)
	}
	// 23:50 KST with a 00:10 slot crosses midnight
	now = time.Date(2026, 9, 10, 23, 50, 0, 0, seoul)
	if got := NextDailyAt(now, "00:10", "Asia/Seoul"); !got.Equal(time.Date(2026, 9, 11, 0, 10, 0, 0, seoul)) {
		t.Fatalf("midnight: %s", got)
	}
	// tz matters: 01:00 UTC on the 10th is 10:00 KST, so 09:00 KST is the 11th
	now = time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	if got := NextDailyAt(now, "09:00", "Asia/Seoul"); !got.Equal(time.Date(2026, 9, 11, 9, 0, 0, 0, seoul)) {
		t.Fatalf("tz: %s", got)
	}
	if got := NextDailyAt(now, "09:00", "UTC"); !got.Equal(time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("utc: %s", got)
	}
	// malformed slot fails open to 09:00
	if got := NextDailyAt(now, "nope", "UTC"); got.Hour() != 9 {
		t.Fatalf("fallback: %s", got)
	}
}
