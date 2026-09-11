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

// userError writes a Korean sentence a non-developer can act on. The original
// Go error follows on a second line: the dashboard shows only the first line
// and keeps the rest behind 개발자 정보.
func userError(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		msg += "\n" + err.Error()
	}
	http.Error(w, msg, status)
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
			userError(w, http.StatusBadRequest, "보낸 내용을 읽지 못했습니다. 화면을 새로 고친 뒤 다시 저장해 주세요.", err)
			return
		}
		// Reject loudly here. The scheduler fails open on bad values, so an
		// invalid window that got persisted would look enabled but do nothing.
		if err := in.Validate(); err != nil {
			userError(w, http.StatusBadRequest, "시간대 설정이 올바르지 않습니다. 요일과 시작·끝 시각, 기준 시간대를 확인해 주세요.", err)
			return
		}
		b, err := json.Marshal(in)
		if err != nil {
			userError(w, http.StatusInternalServerError, "검사 시간대를 저장하지 못했습니다. 잠시 후 다시 시도해 주세요.", err)
			return
		}
		if err := s.st.SetState(ctx, config.ActiveHoursStateKey, string(b)); err != nil {
			userError(w, http.StatusInternalServerError, "검사 시간대를 저장하지 못했습니다. 잠시 후 다시 시도해 주세요.", err)
			return
		}
		writeJSON(w, s.windowState(ctx))

	case http.MethodDelete:
		if !hasSameOrigin(r) {
			http.Error(w, "다른 출처에서는 검사 시간대를 바꿀 수 없습니다", http.StatusForbidden)
			return
		}
		if err := s.st.SetState(ctx, config.ActiveHoursStateKey, ""); err != nil {
			userError(w, http.StatusInternalServerError, "설정 파일 값으로 되돌리지 못했습니다. 잠시 후 다시 시도해 주세요.", err)
			return
		}
		writeJSON(w, s.windowState(ctx))

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		userError(w, http.StatusMethodNotAllowed, "이 주소에서는 지원하지 않는 요청 방식입니다.", nil)
	}
}
