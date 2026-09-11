package config

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ActiveHours restricts when the scheduler enqueues cadence work (PRD §12 tick).
// Zero value = disabled = the historical 24/7 behaviour, so omitting the block
// from vigil.yaml changes nothing.
//
// It lives in config (not model) because both the scheduler and the read-only UI
// already import this package; putting it here adds no dependency edge.
// ActiveHoursStateKey is the scheduler_state row the UI writes to override
// schedule.active_hours at runtime. `serve` and `loop` are separate processes;
// SQLite is their only channel.
const ActiveHoursStateKey = "schedule.active_hours"

type ActiveHours struct {
	Enabled bool     `json:"enabled" yaml:"enabled"`
	Days    Weekdays `json:"days" yaml:"days"` // time.Weekday numbers; empty = every day
	From    string   `json:"from" yaml:"from"` // "HH:MM", inclusive
	To      string   `json:"to" yaml:"to"`     // "HH:MM", exclusive
	TZ      string   `json:"tz" yaml:"tz"`     // IANA name; empty = machine local
}

// Weekdays accepts either numbers (0=Sun) or names (mon, monday, 월) in YAML,
// and always marshals to numbers so the UI has one shape to deal with.
type Weekdays []int

var weekdayNames = map[string]int{
	"sun": 0, "sunday": 0, "일": 0,
	"mon": 1, "monday": 1, "월": 1,
	"tue": 2, "tuesday": 2, "화": 2,
	"wed": 3, "wednesday": 3, "수": 3,
	"thu": 4, "thursday": 4, "목": 4,
	"fri": 5, "friday": 5, "금": 5,
	"sat": 6, "saturday": 6, "토": 6,
}

var weekdayLabel = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func (d *Weekdays) UnmarshalYAML(n *yaml.Node) error {
	var raw []string
	if err := n.Decode(&raw); err != nil {
		return fmt.Errorf("invalid days %q: %w", n.Value, err)
	}
	out := make(Weekdays, 0, len(raw))
	for _, s := range raw {
		v, ok := ParseWeekday(s)
		if !ok {
			return fmt.Errorf("invalid weekday %q (use 0-6 or sun..sat)", s)
		}
		out = append(out, v)
	}
	*d = out
	return nil
}

// ParseWeekday maps "1", "mon", "Monday" or "월" to a time.Weekday number.
func ParseWeekday(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if v, ok := weekdayNames[s]; ok {
		return v, true
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 6 {
		return n, true
	}
	return 0, false
}

// locations memoizes time.LoadLocation, which reads and parses the zoneinfo
// file on every call. Allows/NextChange are evaluated on every scheduler tick
// and every dashboard poll, so an uncached lookup dominated their cost.
var locations sync.Map // tz name -> *time.Location

// Location resolves TZ; an empty or unknown zone falls back to machine local.
func (a ActiveHours) Location() *time.Location {
	if a.TZ == "" || strings.EqualFold(a.TZ, "local") {
		return time.Local
	}
	if v, ok := locations.Load(a.TZ); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(a.TZ)
	if err != nil {
		loc = time.Local
	}
	locations.Store(a.TZ, loc)
	return loc
}

// Validate reports why a window would be ignored. Callers that accept user input
// (the UI) must reject on error; the scheduler instead fails open.
func (a ActiveHours) Validate() error {
	if !a.Enabled {
		return nil
	}
	from, err := parseHM(a.From)
	if err != nil {
		return fmt.Errorf("from: %w", err)
	}
	to, err := parseHM(a.To)
	if err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if from == to {
		return fmt.Errorf("from and to are both %s: an empty window would stop all cadence work", a.From)
	}
	if len(a.Days) == 0 {
		return fmt.Errorf("days is empty: pick at least one weekday")
	}
	for _, d := range a.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday %d out of range (0=Sun .. 6=Sat)", d)
		}
	}
	if a.TZ != "" && !strings.EqualFold(a.TZ, "local") {
		if _, err := time.LoadLocation(a.TZ); err != nil {
			return fmt.Errorf("tz %q: %w", a.TZ, err)
		}
	}
	return nil
}

// Allows reports whether cadence work may be enqueued at t.
//
// It fails open on every malformed field: a typo in the config must never
// silently halt QA. Validate() is where bad input gets rejected loudly.
func (a ActiveHours) Allows(t time.Time) bool {
	if !a.Enabled {
		return true
	}
	from, err1 := parseHM(a.From)
	to, err2 := parseHM(a.To)
	if err1 != nil || err2 != nil || from == to || len(a.Days) == 0 {
		return true
	}
	local := t.In(a.Location())
	cur := local.Hour()*60 + local.Minute()
	day := int(local.Weekday())

	if from < to { // same-day window, half-open [from, to)
		return a.hasDay(day) && cur >= from && cur < to
	}
	// Overnight window (22:00→06:00): the weekday list matches the day the
	// window OPENED, so "fri 22:00-06:00" still runs at 02:00 on Saturday.
	if cur >= from {
		return a.hasDay(day)
	}
	return cur < to && a.hasDay((day+6)%7)
}

func (a ActiveHours) hasDay(d int) bool {
	for _, v := range a.Days {
		if v == d {
			return true
		}
	}
	return false
}

// NextChange returns the next instant at which Allows flips, looking at most 8
// days ahead. The zero time means "never changes".
//
// Allows is piecewise constant and can only flip at a window boundary (the
// `from` or `to` clock time on some day), so it is enough to test those
// boundaries in order instead of scanning every minute.
func (a ActiveHours) NextChange(t time.Time) time.Time {
	if !a.Enabled {
		return time.Time{}
	}
	from, err1 := parseHM(a.From)
	to, err2 := parseHM(a.To)
	if err1 != nil || err2 != nil || from == to || len(a.Days) == 0 {
		return time.Time{} // Allows fails open on these; nothing ever flips
	}
	loc := a.Location()
	now := a.Allows(t)
	local := t.In(loc)
	y, m, d := local.Date()
	for day := 0; day <= 8; day++ {
		for _, hm := range [2]int{min(from, to), max(from, to)} { // clock order within the day
			at := time.Date(y, m, d+day, hm/60, hm%60, 0, 0, loc)
			if !at.After(local) {
				continue
			}
			if a.Allows(at) != now {
				return at
			}
		}
	}
	return time.Time{}
}

func (a ActiveHours) String() string {
	if !a.Enabled {
		return "24/7"
	}
	days := make([]string, 0, len(a.Days))
	for _, d := range a.Days {
		if d >= 0 && d <= 6 {
			days = append(days, weekdayLabel[d])
		}
	}
	return fmt.Sprintf("%s %s-%s %s", strings.Join(days, ","), a.From, a.To, a.Location())
}

// NextDailyAt returns the first occurrence of the wall-clock time hhmm ("HH:MM"
// in zone tz; "" = machine local) strictly after now. A slot earlier than or
// equal to now's clock rolls over to tomorrow, so a run finishing at 09:00:30
// is next due at 09:00 the following day. It fails open on a malformed hhmm
// (09:00) for the same reason ActiveHours.Allows does; Validate rejects it.
func NextDailyAt(now time.Time, hhmm, tz string) time.Time {
	hm, err := parseHM(hhmm)
	if err != nil {
		hm = 9 * 60
	}
	loc := ActiveHours{TZ: tz}.Location()
	local := now.In(loc)
	y, m, d := local.Date()
	at := time.Date(y, m, d, hm/60, hm%60, 0, 0, loc)
	if !at.After(local) {
		at = time.Date(y, m, d+1, hm/60, hm%60, 0, 0, loc)
	}
	return at
}

func parseHM(s string) (int, error) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("%q has an out-of-range hour", s)
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q has an out-of-range minute", s)
	}
	return hh*60 + mm, nil
}
