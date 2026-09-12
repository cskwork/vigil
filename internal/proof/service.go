package proof

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"vigil/internal/agent"
	"vigil/internal/browser"
	"vigil/internal/dsl"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

type Service struct {
	owner string

	Repo             *Repository
	Registry         Registry
	Store            *store.Store
	EvidenceDir      string
	RawRetention     time.Duration
	SummaryRetention time.Duration
	wake             chan struct{}
	mu               sync.Mutex
	cancels          map[string]context.CancelFunc
}

func NewService(st *store.Store, reg Registry, dir string) (*Service, error) {
	r, e := NewRepository(st.DB())
	if e != nil {
		return nil, e
	}
	owner := "proof-service:" + id()
	ok, e := st.TryAcquireLocks(context.Background(), []string{"proof:service"}, owner, 30*time.Second)
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, fmt.Errorf("another proof service owns this database; after a crash retry in 30 seconds")
	}
	if e = recoverBrowsers(context.Background(), st); e != nil {
		st.ReleaseLocks(context.Background(), []string{"proof:service"}, owner)
		return nil, e
	}
	if e = r.Recover(context.Background()); e != nil {
		_ = st.ReleaseLocks(context.Background(), []string{"proof:service"}, owner)
		return nil, e
	}
	return &Service{owner: owner, Repo: r, Registry: reg, Store: st, EvidenceDir: dir, RawRetention: 7 * 24 * time.Hour, SummaryRetention: 30 * 24 * time.Hour, wake: make(chan struct{}, 1), cancels: map[string]context.CancelFunc{}}, nil
}
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Cancel(ctx context.Context, id string) error {
	if e := s.Repo.Cancel(ctx, id); e != nil {
		return e
	}
	s.mu.Lock()
	if c := s.cancels[id]; c != nil {
		c()
	}
	s.mu.Unlock()
	s.Wake()
	return nil
}
func (s *Service) Run(ctx context.Context) error {
	ctx, cancelService := context.WithCancel(ctx)
	defer cancelService()
	defer s.Store.ReleaseLocks(context.Background(), []string{"proof:service"}, s.owner)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.Store.TryAcquireLocks(heartbeatCtx, []string{"proof:service"}, s.owner, 30*time.Second)
				if err != nil || !ok {
					cancelService()
					return
				}
			}
		}
	}()
	lastPrune := time.Now()
	for {
		if time.Since(lastPrune) > time.Hour {
			if e := s.Prune(ctx); e != nil {
				return e
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-time.After(time.Hour):
		}
		for {
			j, e := s.Store.ClaimJob(ctx, "proof", "proof", 10*time.Minute, model.JobProofPlan, model.JobProofRun)
			if e == store.ErrNotFound {
				break
			}
			if e != nil {
				return e
			}
			var ref struct {
				ID string `json:"id"`
			}
			if e = json.Unmarshal([]byte(j.Payload), &ref); e == nil {
				if j.Kind == model.JobProofPlan {
					e = s.plan(ctx, ref.ID)
				} else {
					e = s.execute(ctx, ref.ID)
				}
			}
			msg := ""
			if e != nil {
				msg = e.Error()
			}
			if e = s.Store.CompleteJob(context.Background(), j.ID, msg); e != nil {
				return e
			}
		}
	}
}
func (s *Service) plan(ctx context.Context, id string) error {
	c, e := s.Repo.GetCheck(ctx, id)
	if e != nil {
		return e
	}
	if t := s.Registry.Targets[c.TargetRef]; t.Template != nil && demoRequest(c.Request) {
		return s.Repo.PlanResult(ctx, id, json.RawMessage(encode(t.Template)), "")
	}
	observation := s.observeForPlan(ctx, c.ID, s.Registry.Targets[c.TargetRef])
	if c, e = s.Repo.SavePlanObservation(ctx, id, observation); e != nil {
		return e
	}
	if s.Registry.PiCommand == "" {
		return s.Repo.PlanResult(ctx, id, nil, "이 요청을 처리할 제안 도구가 등록되지 않았습니다. 운영자가 대상과 계획 도구를 설정해야 합니다.")
	}
	raw, e := agent.ProofPlan(ctx, s.Registry.PiCommand, s.Registry.PiModel, planningInput(sourceText(*c), s.Registry.Targets[c.TargetRef], observation))
	if e != nil {
		message := "계약 제안을 생성하지 못했습니다. 운영자가 계획 도구의 연결을 확인해야 합니다."
		switch {
		case errors.Is(e, agent.ErrProofQuota):
			message = "계획 도구의 사용 한도에 도달했습니다. 나중에 새 요청으로 다시 생성해 주세요."
		case errors.Is(e, agent.ErrProofAuth):
			message = "계획 도구의 인증을 확인할 수 없습니다. 운영자가 로그인 또는 API 자격 정보를 확인해야 합니다."
		case errors.Is(e, agent.ErrProofProvider):
			message = "계획 제공자에 연결할 수 없습니다. 나중에 새 요청으로 다시 생성해 주세요."
		}
		return s.Repo.PlanResult(ctx, id, nil, message)
	}
	con, question, parseErr := parsePlanOutput(s.Registry, c.TargetRef, sourceText(*c), raw)
	if parseErr != nil {
		return s.Repo.PlanResult(ctx, id, nil, parseErr.Error())
	}
	if question != "" {
		return s.Repo.PlanQuestion(ctx, id, question)
	}
	return s.Repo.PlanResult(ctx, id, json.RawMessage(encode(con)), "")
}

func parsePlanOutput(reg Registry, targetRef, request string, raw json.RawMessage) (Contract, string, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) == nil {
		if questionRaw, ok := envelope["question"]; ok {
			var question string
			if len(envelope) != 1 || json.Unmarshal(questionRaw, &question) != nil || strings.TrimSpace(question) == "" {
				return Contract{}, "", fmt.Errorf("제안 형식이 올바르지 않습니다.")
			}
			return Contract{}, question, nil
		}
	}
	var con Contract
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&con); err != nil {
		return Contract{}, "", fmt.Errorf("제안 형식이 올바르지 않습니다.")
	}
	for _, criterion := range con.Criteria {
		if _, ok := reg.Targets[targetRef].Observers[criterion.Observer]; !ok {
			return Contract{}, "", fmt.Errorf("제안에 등록되지 않은 관찰자가 있습니다.")
		}
	}
	for i := range con.Criteria {
		con.Criteria[i].Proposed = true
	}
	if _, err := reg.Compile(targetRef, con, request); err != nil {
		return Contract{}, "", fmt.Errorf("제안이 등록된 범위 또는 기대값 출처를 충족하지 못했습니다.")
	}
	return con, "", nil
}
func (s *Service) execute(ctx context.Context, id string) (runErr error) {
	a, e := s.Repo.Attempt(ctx, id)
	if e != nil {
		return e
	}
	defer func() {
		if runErr != nil {
			_ = s.finish(a, "DONE", nil, "execution infrastructure unavailable")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	s.mu.Lock()
	s.cancels[id] = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.cancels, id); s.mu.Unlock() }()
	keys := []string{"proof:whole", "account:" + a.Target.Personas[a.Contract.Persona].Account, "entity:" + a.Fixture.Entity}
	ok, e := s.Store.TryAcquireLocks(ctx, keys, "proof:"+id, 5*time.Minute)
	if e != nil {
		return e
	}
	if !ok {
		return fmt.Errorf("resource lock unavailable")
	}
	defer s.Store.ReleaseLocks(context.Background(), keys, "proof:"+id)
	if a.CancelRequested {
		return s.finish(a, "CANCELLED", nil, "cancelled before actions")
	}
	a.State = "RUNNING"
	mutating := false
	for _, actionID := range a.Plan.Actions {
		mutating = mutating || a.Target.Actions[actionID].Mutating
	}
	if mutating {
		a.Progress = "테스트 데이터를 준비하고 있습니다"
		started := time.Now().UTC()
		a.ActionJournal = append(a.ActionJournal, ActionEvent{Action: "fixture", Title: "테스트 데이터 준비", Step: 0, State: "RUNNING", StartedAt: started})
		if e = s.Repo.Update(ctx, a); e != nil {
			return e
		}
		if e := prepareFixture(ctx, a); e != nil {
			finished := time.Now().UTC()
			a.ActionJournal[len(a.ActionJournal)-1].State = "FAILED"
			a.ActionJournal[len(a.ActionJournal)-1].FinishedAt = &finished
			return s.finish(a, "DONE", nil, e.Error())
		}
		finished := time.Now().UTC()
		a.ActionJournal[len(a.ActionJournal)-1].State = "DONE"
		a.ActionJournal[len(a.ActionJournal)-1].FinishedAt = &finished
	}
	a.Progress = "브라우저를 준비하고 있습니다"
	if e = s.Repo.Update(ctx, a); e != nil {
		return e
	}
	provider := browser.NewTrackedChromium("", true, "", func(pid int, profile string) error {
		return s.Store.SetState(context.Background(), "proof:browser:"+a.ID, encode(ownedBrowser{PID: pid, Profile: profile}))
	})
	defer s.Store.DB().ExecContext(context.Background(), `DELETE FROM scheduler_state WHERE key=?`, "proof:browser:"+a.ID)
	rr := runner.New(map[model.Browser]browser.Provider{model.BrowserChromium: provider})
	defer provider.Stop()
	defer rr.Close()
	con := &dsl.Scenario{}
	con.Scenario.ID = id
	con.Scenario.Version = 1
	con.Scenario.Mutation = "read-only"
	con.Oracle.Source = "contract"
	con.Oracle.Note = a.Approval.ContractHash
	persona := a.Target.Personas[a.Contract.Persona]
	if len(persona.Setup) > 0 {
		for i, step := range persona.Setup {
			step.Name = fmt.Sprintf("account_setup:%d", i)
			con.Steps = append(con.Steps, step)
		}
		con.Steps = append(con.Steps, dsl.Step{Name: "account_setup:observe", WaitMs: 1})
	}
	for _, aid := range a.Plan.Actions {
		act := a.Target.Actions[aid]
		if act.Mutating {
			con.Scenario.Mutation = "reversible"
		}
		for i, step := range act.Steps {
			step.Name = fmt.Sprintf("%s:%d", aid, i)
			con.Steps = append(con.Steps, step)
		}
		con.Steps = append(con.Steps, dsl.Step{Name: aid + ":observe", WaitMs: 50})
	}
	secrets := map[string]string{}
	for name, env := range persona.Secrets {
		v := os.Getenv(env)
		if v == "" {
			a.Progress = "인증 정보가 없습니다"
			return s.finish(a, "DONE", nil, "authentication unavailable")
		}
		secrets[name] = v
	}
	obs := &observation{evidenceDir: s.EvidenceDir, a: a, ctx: ctx, cancel: cancel, repo: s.Repo, probes: &ProbeRunner{Registry: s.Registry.Probes}}
	res, e := rr.Run(ctx, runner.Spec{ProjectID: "proof", Scenario: con, Browser: model.BrowserChromium, BaseURL: a.Target.BaseURL, Persona: secrets, EvidenceDir: filepath.Join(s.EvidenceDir, a.ID), RunTimeout: 3 * time.Minute, StepTimeout: 5 * time.Second, Hooks: obs})
	if e != nil {
		return s.finish(a, "DONE", nil, "browser infrastructure unavailable")
	}
	state := "DONE"
	latest, le := s.Repo.Attempt(context.Background(), id)
	if le == nil && latest.CancelRequested {
		state = "CANCELLED"
	}
	reason := ""
	if res.Error != "" {
		reason = "실행이 중단되었거나 필요한 관찰을 확보하지 못했습니다"
	}
	return s.finish(a, state, res, reason)
}
func (s *Service) finish(a *Attempt, state string, res *runner.Result, reason string) error {
	a.State = state
	now := time.Now().UTC()
	a.FinishedAt = &now
	existing := map[string]bool{}
	for _, r := range a.Results {
		existing[r.ID] = true
	}
	for _, c := range a.Contract.Criteria {
		if !existing[c.ID] {
			r := CriterionResult{ID: c.ID, Required: c.Required, Status: "UNKNOWN", Reason: reason, Evidence: []Evidence{}}
			if res != nil && res.ScreenshotPath != "" {
				action := ""
				if res.FailedStep != nil {
					action = strings.Split(res.FailedStep.Name, ":")[0]
				}
				r.Evidence = append(r.Evidence, Evidence{ID: id(), Attempt: a.ID, Criterion: c.ID, Action: action, Persona: a.Contract.Persona, Entity: a.Fixture.Entity, At: now, Source: "browser", Status: "UNKNOWN", Reason: reason, Screenshot: filepath.Base(res.ScreenshotPath)})
			}
			a.Results = append(a.Results, r)
		}
	}
	changed := a.ObservedVersion != "" && a.VersionAfter != "" && a.ObservedVersion != a.VersionAfter
	if changed {
		for i := range a.Results {
			if a.Results[i].Status == "PASS" {
				a.Results[i].Status = "UNKNOWN"
				a.Results[i].Reason = "deployment version changed during attempt"
			}
		}
	}
	for i := range a.Results {
		persistCriterion(s.EvidenceDir, a.ID, &a.Results[i])
	}
	a.Verdict = Verdict(a.Results)
	if state != "DONE" && a.Verdict == "PASS" {
		a.Verdict = "INCOMPLETE"
	}
	a.Progress = "검사가 끝났습니다"
	if reason != "" {
		a.Progress = reason
	}
	if changed {
		a.Progress = "실행 중 배포 버전이 변경되어 비교가 유효하지 않습니다"
	}
	if state == "CANCELLED" {
		a.Progress = "사용자가 실행을 중단했습니다"
	}
	if a.Approval.Baseline != "" && a.Verdict == "PASS" && state == "DONE" {
		if base, e := s.Repo.Attempt(context.Background(), a.Approval.Baseline); e == nil && base.Verdict == "FAIL" && base.ObservedVersion != "" && a.ObservedVersion != "" && a.ObservedVersion == a.VersionAfter && base.ObservedVersion != a.ObservedVersion {
			a.FixClaim = "versioned baseline FAIL to current PASS"
		}
	}
	return s.Repo.Update(context.Background(), a)
}
func (s *Service) Close() error {
	return s.Store.ReleaseLocks(context.Background(), []string{"proof:service"}, s.owner)
}

func demoRequest(s string) bool {
	empty := false
	for _, word := range []string{"빈", "비어", "비우", "지우"} {
		empty = empty || strings.Contains(s, word)
	}
	history := strings.Contains(s, "이력") || strings.Contains(s, "기록")
	preserve := strings.Contains(s, "유지") || strings.Contains(s, "보존")
	return empty && history && preserve && !strings.Contains(s, "이력 삭제") && !strings.Contains(s, "기록 삭제")
}

func planningInput(request string, t Target, observations ...PlanObservation) string {
	personas := map[string]any{}
	for id, p := range t.Personas {
		personas[id] = map[string]string{"account": p.Account}
	}
	fixtures := map[string]any{}
	for id, f := range t.Fixtures {
		fixtures[id] = map[string]any{"entity": f.Entity, "qa": f.QA}
	}
	actions := map[string]any{}
	for id, a := range t.Actions {
		actions[id] = map[string]any{"title": a.Title, "mutating": a.Mutating}
	}
	observers := map[string]any{}
	for id, o := range t.Observers {
		observers[id] = map[string]string{"kind": o.Kind, "action": o.Action}
	}
	input := map[string]any{"request": request, "personas": personas, "fixtures": fixtures, "actions": actions, "observers": observers, "definitions": t.Definitions}
	if len(observations) > 0 {
		input["read_only_page_observation"] = observations[0]
	}
	raw := json.RawMessage(encode(input))
	return string(redactJSON(raw, t))
}

func sourceText(c Check) string {
	parts := append([]string{c.Request}, c.Clarifications...)
	return strings.Join(parts, "\n")
}
