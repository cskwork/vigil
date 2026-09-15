package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"vigil/internal/explain"
	"vigil/internal/model"
	"vigil/internal/runner"
	"vigil/internal/store"
)

type executionStep struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	State string `json:"state"`
}
type execution struct {
	AgentDir   string            `json:"-"`
	Preview    string            `json:"preview,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Scripts    []executionScript `json:"scripts,omitempty"`
	JobID      int64             `json:"job_id"`
	Scenario   string            `json:"scenario"`
	Title      string            `json:"title"`
	Site       string            `json:"site"`
	URL        string            `json:"url"`
	Phase      string            `json:"phase"`
	Steps      []executionStep   `json:"steps"`
	StartedAt  time.Time         `json:"started_at"`
	Completed  bool              `json:"completed"`
	Outcome    string            `json:"outcome,omitempty"`
	Message    string            `json:"message,omitempty"`
	Screenshot string            `json:"screenshot,omitempty"`
	Evidence   string            `json:"evidence,omitempty"`
}
type executionTracker struct {
	mu            sync.Mutex
	current       int64
	items         map[int64]*execution
	st            *store.Store
	project, root string
}

func (t *executionTracker) begin(id int64, sc *model.Scenario, site, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.items == nil {
		t.items = map[int64]*execution{}
	}
	if len(t.items) >= 20 {
		var oldest int64
		for k := range t.items {
			if oldest == 0 || k < oldest {
				oldest = k
			}
		}
		delete(t.items, oldest)
	}
	t.current = id
	t.items[id] = &execution{JobID: id, Scenario: sc.ID, Title: sc.Title, Site: site, URL: url, Phase: "preparing", Steps: []executionStep{}, StartedAt: time.Now()}
}
func (t *executionTracker) update(fn func(*execution)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.items[t.current]; e != nil {
		fn(e)
	}
}
func (t *executionTracker) complete(id int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, runErr := t.st.LatestRunForJob(ctx, t.project, id)
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.items[id]
	if e == nil {
		return
	}
	e.Completed = true
	e.Phase = "completed"
	if runErr != nil {
		e.Outcome = "ENV_FAILURE"
		e.Message = "실행 결과를 기록하지 못했습니다. 다시 실행해 주세요."
		if err != nil {
			e.Message = "검사를 시작하지 못했습니다. 실행 설정을 확인해 주세요."
		}
		return
	}
	t.result(e, run)
}
func (t *executionTracker) result(e *execution, run *model.Run) {
	e.Outcome = string(run.Outcome)
	if run.Outcome == model.OutcomePass {
		e.Message = "확인한 내용이 기대한 결과와 일치합니다."
	} else {
		e.Message = explain.ForRun(run, nil).Headline
		if e.Message == "" {
			e.Message = "검사를 완료하지 못했습니다. 상세 기록을 확인하세요."
		}
	}
	rel, err := filepath.Rel(t.root, run.EvidenceDir)
	if run.EvidenceDir != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		e.Evidence = filepath.ToSlash(rel)
		if _, err = os.Stat(filepath.Join(run.EvidenceDir, "screenshot.png")); err == nil {
			e.Screenshot = e.Evidence + "/screenshot.png"
		}
	}
}
func (t *executionTracker) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "GET required", 405)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("job"), 10, 64)
	if err != nil {
		http.Error(w, "invalid job", 400)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.items[id]
	if e == nil {
		var saved execution
		if raw, err := t.st.GetState(r.Context(), "admin-execution:"+t.project+":"+strconv.FormatInt(id, 10)); err == nil && json.Unmarshal([]byte(raw), &saved) == nil && saved.JobID == id {
			e = &saved
		}
	}
	if e == nil {
		job, err := t.st.ProjectJob(r.Context(), t.project, id)
		if err != nil {
			http.Error(w, "실행 기록을 찾을 수 없습니다", 404)
			return
		}
		e = &execution{JobID: id, Scenario: job.ScenarioID, Title: job.ScenarioID, Completed: true, Phase: "completed", Steps: []executionStep{}, Outcome: "ENV_FAILURE", Message: "실시간 연결이 종료되었습니다. 저장된 실행 기록을 확인하세요."}
		if run, err := t.st.LatestRunForJob(r.Context(), t.project, id); err == nil && job.Kind == model.JobRunScenario {
			t.result(e, run)
		}
	}
	if !e.Completed {
		t.capturePreview(e)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(e)
}

// Existing runner observation hooks provide actual boundaries without a second
// browser driver, synthetic timers, or polling logs on disk.
type progressRunner struct {
	run     *runner.Runner
	tracker *executionTracker
}

func (p progressRunner) Run(ctx context.Context, spec runner.Spec) (*runner.Result, error) {
	p.tracker.update(func(e *execution) { e.Phase = "preparing"; e.Steps = []executionStep{} })
	spec.Hooks = p
	return p.run.Run(ctx, spec)
}
func (p progressRunner) Setup(context.Context) error {
	p.tracker.update(func(e *execution) { e.Phase = "running" })
	return nil
}
func (p progressRunner) BeforeStep(_ context.Context, index int, name string) error {
	p.tracker.update(func(e *execution) {
		e.Phase = "running"
		e.Steps = append(e.Steps, executionStep{Index: index, Name: name, State: "running"})
	})
	return nil
}
func (p progressRunner) AfterStep(_ context.Context, index int, _ string, ok bool) {
	p.tracker.update(func(e *execution) {
		for i := range e.Steps {
			if e.Steps[i].Index == index {
				if ok {
					e.Steps[i].State = "done"
				} else {
					e.Steps[i].State = "failed"
				}
			}
		}
	})
}
func (p progressRunner) Finish(context.Context) {
	p.tracker.update(func(e *execution) { e.Phase = "saving" })
}

// capturePreview only exposes regular PNG files inside this project evidence root.
func (t *executionTracker) capturePreview(e *execution) {
	if e.AgentDir != "" {
		if rel, err := filepath.Rel(t.root, e.AgentDir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			entries, _ := os.ReadDir(e.AgentDir)
			var latest time.Time
			for _, entry := range entries {
				if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".png") {
					if info, err := entry.Info(); err == nil && info.ModTime().After(latest) {
						latest = info.ModTime()
						e.Preview = filepath.ToSlash(filepath.Join(rel, entry.Name())) + "?v=" + strconv.FormatInt(info.ModTime().UnixMilli(), 10)
					}
				}
			}
		}
	}
}
