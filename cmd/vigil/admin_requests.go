package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vigil/internal/agent"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/scheduler"
)

type invalidAdminSite struct{}

func (invalidAdminSite) Error() string        { return "등록된 검증 사이트를 선택해 주세요." }
func (invalidAdminSite) InvalidRequest() bool { return true }

type adminRequestBusy struct{}

func (adminRequestBusy) Error() string {
	return "다른 검사가 실행 중입니다. 완료 후 다시 요청하세요."
}
func (adminRequestBusy) BusyRequest() bool { return true }

type adminRequests struct {
	app     *app
	actions *adminActions
	run     progressRunner
}

func (a *adminRequests) SubmitUserRequest(ctx context.Context, situation string) (string, int64, error) {
	return a.SubmitUserRequestAt(ctx, situation, "")
}
func (a *adminRequests) SubmitUserRequestAt(ctx context.Context, situation, site string) (string, int64, error) {
	env, err := a.app.cfg.Env(site)
	if err != nil {
		return "", 0, invalidAdminSite{}
	}
	if !a.actions.busy.CompareAndSwap(false, true) {
		return "", 0, adminRequestBusy{}
	}
	// Each request keeps its selected site for investigation and every validation/repair.
	cfg := *a.app.cfg
	cfg.Target.DefaultEnv, cfg.Target.BaseURL, cfg.Target.AllowedHosts = env.Name, env.BaseURL, env.AllowedHosts
	ag, err := agent.NewPi(&cfg)
	if err != nil {
		a.actions.busy.Store(false)
		return "", 0, err
	}
	tracked := &adminAgent{Adapter: ag, tracker: a.actions.progress}
	o := orchestrator.NewWithRunner(&cfg, a.app.st, a.run, tracked, a.app.ev)
	feature, id, err := o.SubmitUserRequest(ctx, situation)
	if err != nil {
		a.actions.busy.Store(false)
		return "", 0, err
	}
	title := strings.SplitN(strings.TrimSpace(situation), "\n", 2)[0]
	if chars := []rune(title); len(chars) > 100 {
		title = string(chars[:100]) + "…"
	}
	a.actions.progress.begin(id, &model.Scenario{Title: title}, env.Name, env.BaseURL)
	a.actions.progress.update(func(e *execution) { e.Kind = "request"; e.Phase = "checking" })
	a.actions.wg.Add(1)
	go func() {
		defer a.actions.wg.Done()
		defer a.actions.busy.Store(false)
		if a.actions.stopIdle != nil {
			defer a.actions.stopIdle()
		}
		// The entire orchestration, including bounded script repairs, has a deadline.
		timeout := cfg.Agent.Timeout.Duration * time.Duration(cfg.Policy.AgentFixAttempts+2)
		if timeout <= 0 {
			timeout = 15 * time.Minute
		}
		runCtx, cancel := context.WithTimeout(a.actions.ctx, timeout)
		defer cancel()
		sched := scheduler.NewWith(&cfg, a.app.st, o, a.run, nil, nil)
		inline := inlineAgent{sched: sched, orch: o}
		child := *a.app
		child.cfg = &cfg
		err := ag.Doctor(runCtx)
		if err == nil {
			err = inline.RunAgentJobNow(runCtx, id)
		}
		if job, e := a.app.st.ProjectJob(runCtx, cfg.Project.ID, id); e == nil && job.State != model.JobDone && err == nil {
			err = fmt.Errorf("agent job ended in %s: %s", job.State, job.LastError)
		}
		gates := gatesForJob(runCtx, inline, id)
		if err == nil {
			for _, g := range gates {
				if g.ScenarioID != "" && g.State != model.StateDuplicate {
					if e := child.settleCandidate(runCtx, inline, g.ScenarioID); e != nil {
						err = e
						break
					}
				}
			}
		}
		if err != nil {
			a.actions.logError(err)
			persistCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			_ = a.app.st.CompleteJob(persistCtx, id, err.Error())
			done()
		}
		a.actions.progress.completeRequest(id, gates, err)
	}()
	return feature, id, nil
}

// Adapter boundaries report real orchestration phases; runner hooks report steps.
type adminAgent struct {
	agent.Adapter
	tracker *executionTracker
}

func (a *adminAgent) Run(ctx context.Context, req agent.Request, dir string) (*agent.Result, error) {
	phase := "investigating"
	if req.Task == agent.TaskRepair {
		phase = "repairing"
	}
	a.tracker.update(func(e *execution) { e.Phase = phase; e.Message = ""; e.AgentDir = dir })
	result, err := a.Adapter.Run(ctx, req, dir)
	a.tracker.update(func(e *execution) {
		e.Phase = "validating"
		if result != nil && len(result.ScriptCandidates) == 0 && result.ScriptPatch == "" {
			e.Message = result.Reason
			if e.Message == "" {
				e.Message = result.Evidence
			}
			if chars := []rune(e.Message); len(chars) > 1200 {
				e.Message = string(chars[:1200]) + "…"
			}
		}
	})
	return result, err
}

type executionScript struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Outcome      string          `json:"outcome"`
	Message      string          `json:"message"`
	Screenshot   string          `json:"screenshot,omitempty"`
	Reproduction json.RawMessage `json:"reproduction,omitempty"`
}

func (t *executionTracker) completeRequest(id int64, gates []orchestrator.GateOutcome, runErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.items[id]
	if e == nil {
		return
	}
	e.Completed = true
	e.Phase = "completed"
	e.Outcome = "NEEDS_REVIEW"
	if e.Message == "" {
		e.Message = "검사 스크립트를 확정하지 못했습니다. 요청 내용과 실행 설정을 확인해 주세요."
	}
	allPass := len(gates) > 0
	for _, g := range gates {
		item := executionScript{ID: g.ScenarioID, Outcome: "NEEDS_REVIEW", Message: g.Reason}
		if g.State == model.StateDuplicate {
			item.ID = g.DuplicateOf
			item.Message = "같은 검사 스크립트가 이미 있습니다. 이번 요청에서는 새로 실행하지 않았습니다."
			allPass = false
		} else if sc, err := t.st.GetScenario(ctx, t.project, g.ScenarioID); err == nil {
			item.Title = sc.Title
			if json.Valid([]byte(sc.Reproduction)) {
				item.Reproduction = json.RawMessage(sc.Reproduction)
			}
			runs, err := t.st.ListRuns(ctx, t.project, sc.ID, 1)
			if err == nil && len(runs) > 0 && !runs[0].StartedAt.Before(e.StartedAt) {
				var result execution
				t.result(&result, runs[0])
				item.Outcome, item.Message, item.Screenshot = result.Outcome, result.Message, result.Screenshot
				if e.Screenshot == "" || item.Outcome != "PASS" {
					e.Screenshot = item.Screenshot
					e.Evidence = result.Evidence
				}
			}
			if sc.State != model.StatePendingApproval && sc.State != model.StateSoak && sc.State != model.StateActive {
				allPass = false
			}
		}
		if item.Outcome != "PASS" {
			allPass = false
		}
		e.Scripts = append(e.Scripts, item)
	}
	if len(e.Scripts) > 0 {
		e.Message = "시나리오별 실행 결과를 확인해 주세요. 기대 동작과 다른 결과도 그대로 보존했습니다."
	}
	if allPass {
		e.Outcome = "PASS"
		e.Message = "생성한 모든 검사 스크립트를 실행해 기대 동작을 확인했습니다."
	}
	if runErr != nil {
		e.Outcome = "ENV_FAILURE"
		e.Message = "자동 검증을 완료하지 못했습니다. 에이전트 연결·인증과 실행 기록을 확인한 후 다시 요청해 주세요."
	}
	if e.Screenshot == "" {
		t.capturePreview(e)
		e.Screenshot = strings.SplitN(e.Preview, "?", 2)[0]
	}
	// Persist the aggregate, never infer success from the last candidate on reload.
	raw, err := json.Marshal(e)
	if err == nil {
		err = t.st.SetState(ctx, fmt.Sprintf("admin-execution:%s:%d", t.project, id), string(raw))
	}
	if err != nil {
		e.Outcome = "ENV_FAILURE"
		e.Message = "결과 저장에 실패했습니다. 현재 화면의 결과를 확인해 주세요."
	}
}
