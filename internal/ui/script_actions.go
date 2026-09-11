package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"vigil/internal/approval"
	"vigil/internal/config"
	"vigil/internal/model"
)

// ScriptActions is the authority the dashboard gets over one script: run it
// on an environment, approve it, reject it. `loop --ui` provides all three;
// standalone `serve` provides approve/reject through the store and has no
// scheduler to run anything (CanRun false, RunScript returns ErrNoScheduler).
type ScriptActions interface {
	CanRun() bool
	RunScript(ctx context.Context, sc *model.Scenario, env config.Environment, browser model.Browser) (jobID int64, err error)
	ApproveScript(ctx context.Context, id string) (approval.Receipt, error)
	RejectScript(ctx context.Context, id string) (approval.Receipt, error)
}

// ErrNoScheduler is what a ScriptActions without a run queue returns from RunScript.
var ErrNoScheduler = errors.New("no scheduler attached to this server")

// SetScriptActions enables POST /api/script/{run,approve,reject}.
func (s *Server) SetScriptActions(a ScriptActions) { s.scriptActions = a }

// StoreScriptActions is the standalone `serve` implementation: approvals go
// through the store (and Jira), runs are refused with ErrNoScheduler.
type StoreScriptActions struct{ Svc *approval.Service }

func (StoreScriptActions) CanRun() bool { return false }
func (StoreScriptActions) RunScript(context.Context, *model.Scenario, config.Environment, model.Browser) (int64, error) {
	return 0, ErrNoScheduler
}
func (a StoreScriptActions) ApproveScript(ctx context.Context, id string) (approval.Receipt, error) {
	return a.Svc.Approve(ctx, id, approval.Options{})
}
func (a StoreScriptActions) RejectScript(ctx context.Context, id string) (approval.Receipt, error) {
	return a.Svc.Reject(ctx, id)
}

// scriptActionBody is the JSON every script action accepts; env/browser are
// only read by run.
type scriptActionBody struct {
	ID      string `json:"id"`
	Env     string `json:"env"`
	Browser string `json:"browser"`
}

// readScriptAction validates method, content type, origin and body; false means
// a response was already written.
func (s *Server) readScriptAction(w http.ResponseWriter, r *http.Request) (scriptActionBody, bool) {
	var in scriptActionBody
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "지원하지 않는 요청 방식입니다")
		return in, false
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Content-Type은 application/json이어야 합니다")
		return in, false
	}
	if !hasSameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "다른 출처에서는 스크립트를 조작할 수 없습니다")
		return in, false
	}
	if s.scriptActions == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "이 서버에서는 스크립트 조작이 꺼져 있습니다")
		return in, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeAPIError(w, http.StatusBadRequest, "본문은 id(, env, browser) 필드를 가진 JSON이어야 합니다")
		return in, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "본문에는 JSON 값이 하나만 있어야 합니다")
		return in, false
	}
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		writeAPIError(w, http.StatusBadRequest, "스크립트 id가 필요합니다")
		return in, false
	}
	return in, true
}

// scriptRun: POST /api/script/run {id, env, browser} → {job_id}
func (s *Server) scriptRun(w http.ResponseWriter, r *http.Request) {
	in, ok := s.readScriptAction(w, r)
	if !ok {
		return
	}
	sc, err := s.st.GetScenario(r.Context(), s.cfg.Project.ID, in.ID)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "알 수 없는 스크립트입니다: "+in.ID)
		return
	}
	env, err := s.cfg.Env(strings.TrimSpace(in.Env))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "알 수 없는 환경입니다: "+in.Env)
		return
	}
	if err := config.EnvAllows(env, string(sc.Mutation)); err != nil {
		writeAPIError(w, http.StatusBadRequest, "읽기 전용 환경에서는 실행할 수 없습니다: "+err.Error())
		return
	}
	browser := model.Browser(strings.TrimSpace(in.Browser))
	switch browser {
	case "", model.BrowserLightpanda, model.BrowserChromium:
	default:
		writeAPIError(w, http.StatusBadRequest, "알 수 없는 브라우저입니다: "+in.Browser)
		return
	}
	jobID, err := s.scriptActions.RunScript(r.Context(), sc, env, browser)
	if err != nil {
		if errors.Is(err, ErrNoScheduler) {
			writeAPIError(w, http.StatusConflict, "이 화면 서버는 실행기가 없습니다. `vigil loop --ui`로 띄운 화면에서 실행하거나 터미널에서 `vigil run "+sc.ID+"`을 쓰세요")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "실행을 대기열에 넣지 못했습니다: "+err.Error())
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"job_id": jobID, "env": env.Name, "message": sc.ID + ": " + env.Name + " 환경 실행을 대기열에 넣었습니다 (작업 #" + itoa64(jobID) + ")"})
}

// scriptApprove: POST /api/script/approve {id} → {state, cadence, next_due_at, jira}
func (s *Server) scriptApprove(w http.ResponseWriter, r *http.Request) {
	in, ok := s.readScriptAction(w, r)
	if !ok {
		return
	}
	rc, err := s.scriptActions.ApproveScript(r.Context(), in.ID)
	if err != nil {
		var es *approval.ErrState
		switch {
		case approval.IsNotFound(err):
			writeAPIError(w, http.StatusBadRequest, "알 수 없는 스크립트입니다: "+in.ID)
		case errors.As(err, &es):
			writeAPIError(w, http.StatusConflict, "지금 상태에서는 승인할 수 없습니다: "+string(es.State))
		default:
			writeAPIError(w, http.StatusInternalServerError, "승인하지 못했습니다: "+err.Error())
		}
		return
	}
	out := map[string]any{"id": rc.ID, "state": string(rc.To), "cadence": rc.Cadence, "next_due_at": fmtT(rc.NextDueAt), "message": rc.Message(s.cfg.DailyLocation())}
	if rc.Jira != nil {
		out["jira"] = rc.Jira
	}
	writeJSON(w, out)
}

// scriptReject: POST /api/script/reject {id} → {state}
func (s *Server) scriptReject(w http.ResponseWriter, r *http.Request) {
	in, ok := s.readScriptAction(w, r)
	if !ok {
		return
	}
	rc, err := s.scriptActions.RejectScript(r.Context(), in.ID)
	if err != nil {
		if approval.IsNotFound(err) {
			writeAPIError(w, http.StatusBadRequest, "알 수 없는 스크립트입니다: "+in.ID)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "반려하지 못했습니다: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": rc.ID, "state": string(rc.To), "message": rc.Message(s.cfg.DailyLocation())})
}
