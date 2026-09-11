package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"vigil/internal/config"
)

// windowState resolves the effective active-hours window exactly the way the
// scheduler does: a scheduler_state override wins over vigil.yaml, and a
// malformed override is ignored rather than obeyed.
func (s *Server) windowState(ctx context.Context) *windowView {
	w, source := s.cfg.Schedule.ActiveHours, "config"
	if raw, err := s.st.GetState(ctx, config.ActiveHoursStateKey); err == nil && strings.TrimSpace(raw) != "" {
		var override config.ActiveHours
		if json.Unmarshal([]byte(raw), &override) == nil {
			w, source = override, "override"
		}
	}
	if w.Days == nil {
		w.Days = config.Weekdays{} // never serialise null; the dashboard iterates this
	}
	v := &windowView{ActiveHours: w, Source: source, ActiveNow: w.Allows(time.Now()), Zone: w.Location().String()}
	if next := w.NextChange(time.Now()); !next.IsZero() {
		v.NextChange = next.Format("2006-01-02 15:04")
	}
	return v
}

// scheduleWindow is the only mutating route in this package.
//
//	GET    → current window (override or config default)
//	POST   → replace the override; body is the ActiveHours JSON
//	DELETE → drop the override and fall back to vigil.yaml
//
// The running `loop` picks the change up on its next tick (schedule.tick,
// 10s by default); nothing needs restarting.
func (s *Server) scheduleWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.windowState(ctx))

	case http.MethodPost, http.MethodPut:
		if !hasSameOrigin(r) {
			http.Error(w, "다른 출처에서는 검사 시간대를 바꿀 수 없습니다", http.StatusForbidden)
			return
		}
		var in config.ActiveHours
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			http.Error(w, "본문이 올바른 JSON이 아닙니다: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Reject loudly here. The scheduler fails open on bad values, so an
		// invalid window that got persisted would look enabled but do nothing.
		if err := in.Validate(); err != nil {
			http.Error(w, "시간대 설정이 올바르지 않습니다: "+err.Error(), http.StatusBadRequest)
			return
		}
		b, err := json.Marshal(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.st.SetState(ctx, config.ActiveHoursStateKey, string(b)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.windowState(ctx))

	case http.MethodDelete:
		if !hasSameOrigin(r) {
			http.Error(w, "다른 출처에서는 검사 시간대를 바꿀 수 없습니다", http.StatusForbidden)
			return
		}
		if err := s.st.SetState(ctx, config.ActiveHoursStateKey, ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.windowState(ctx))

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
