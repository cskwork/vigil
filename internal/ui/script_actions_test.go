package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/approval"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

type fakeScriptActions struct {
	svc     *approval.Service
	canRun  bool
	ran     []string // "<id>@<env>/<browser>"
	runErr  error
	nextJob int64
}

func (f *fakeScriptActions) CanRun() bool { return f.canRun }
func (f *fakeScriptActions) RunScript(_ context.Context, sc *model.Scenario, env config.Environment, b model.Browser) (int64, error) {
	if f.runErr != nil {
		return 0, f.runErr
	}
	f.ran = append(f.ran, sc.ID+"@"+env.Name+"/"+string(b))
	f.nextJob++
	return f.nextJob, nil
}
func (f *fakeScriptActions) ApproveScript(ctx context.Context, id string) (approval.Receipt, error) {
	return f.svc.Approve(ctx, id, approval.Options{})
}
func (f *fakeScriptActions) RejectScript(ctx context.Context, id string) (approval.Receipt, error) {
	return f.svc.Reject(ctx, id)
}

type recordingCommenter struct{ key, body string }

func (r *recordingCommenter) Comment(_ context.Context, key, body string) (string, error) {
	r.key, r.body = key, body
	return "501", nil
}

func newActionServer(t *testing.T, canRun bool) (*Server, *fakeScriptActions, *store.Store, *recordingCommenter) {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(filepath.Join(base, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	cfg.BaseDir = base
	cfg.Project.ID = "p"
	cfg.Target.BaseURL = "https://dev.example.test"
	cfg.Target.AllowedHosts = []string{"dev.example.test"}
	cfg.Target.DefaultEnv = "dev"
	cfg.Target.Environments = map[string]config.Environment{
		"dev":  {Name: "dev", BaseURL: "https://dev.example.test", AllowedHosts: []string{"dev.example.test"}},
		"prod": {Name: "prod", BaseURL: "https://prod.example.test", AllowedHosts: []string{"prod.example.test"}, ReadOnly: true},
	}
	cfg.Evidence.Dir = "evidence"
	cfg.Policy.SoakPasses = 2
	cfg.Schedule.DailyAt = "09:00"
	cfg.Discovery.IssueKeyPattern = config.DefaultIssueKeyPattern
	if err := st.UpsertProject(context.Background(), "p", cfg.Target.BaseURL); err != nil {
		t.Fatal(err)
	}
	rc := &recordingCommenter{}
	svc := &approval.Service{Cfg: cfg, St: st, Jira: rc, Now: func() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) }}
	fa := &fakeScriptActions{svc: svc, canRun: canRun}
	if !canRun {
		fa.runErr = ErrNoScheduler
	}
	s := New(cfg, st, filepath.Join(base, "evidence"))
	s.SetScriptActions(fa)
	return s, fa, st, rc
}

func addActionScenario(t *testing.T, st *store.Store, id string, state model.ScenarioState, mutation model.Mutation) {
	t.Helper()
	m := &model.Scenario{ID: id, ProjectID: "p", State: state, Fingerprint: "fp-" + id, Title: "t " + id, Class: "P1", Mutation: mutation, OracleSource: "spec", Origin: "agent",
		SoakTarget: 2, CurrentVersion: 1, SourceKind: model.FeatureKindIssue, SourceRef: "PROJ-123", Reproduction: `{"reproduced":true,"at_step":2,"symptom":"dup tabs","run_id":1}`}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "agent"}
	if err := st.CreateScenario(context.Background(), m, v, nil); err != nil {
		t.Fatal(err)
	}
}

func postAction(s *Server, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://vigil.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://vigil.test")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestScriptRunEnqueues(t *testing.T) {
	s, fa, st, _ := newActionServer(t, true)
	addActionScenario(t, st, "s1", model.StatePendingApproval, model.MutationReadOnly)
	w := postAction(s, "/api/script/run", `{"id":"s1","env":"prod","browser":"chromium"}`, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.JobID != 1 || len(fa.ran) != 1 || fa.ran[0] != "s1@prod/chromium" {
		t.Fatalf("job=%d ran=%v", out.JobID, fa.ran)
	}
	// empty env = default env
	w = postAction(s, "/api/script/run", `{"id":"s1"}`, nil)
	if w.Code != http.StatusAccepted || fa.ran[1] != "s1@dev/" {
		t.Fatalf("default env: %d %v", w.Code, fa.ran)
	}
}

func TestScriptRunValidation(t *testing.T) {
	s, _, st, _ := newActionServer(t, true)
	addActionScenario(t, st, "ro", model.StatePendingApproval, model.MutationReadOnly)
	addActionScenario(t, st, "rev", model.StatePendingApproval, model.MutationReversible)
	cases := []struct {
		name, body string
		hdr        map[string]string
		want       int
	}{
		{"cross origin", `{"id":"ro"}`, map[string]string{"Origin": "http://evil.test"}, http.StatusForbidden},
		{"cross site", `{"id":"ro"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"unknown id", `{"id":"nope"}`, nil, http.StatusBadRequest},
		{"unknown env", `{"id":"ro","env":"staging"}`, nil, http.StatusBadRequest},
		{"read-only violation", `{"id":"rev","env":"prod"}`, nil, http.StatusBadRequest},
		{"unknown browser", `{"id":"ro","browser":"firefox"}`, nil, http.StatusBadRequest},
		{"bad json", `{"id":"ro","x":1}`, nil, http.StatusBadRequest},
		{"empty id", `{"id":" "}`, nil, http.StatusBadRequest},
	}
	for _, c := range cases {
		w := postAction(s, "/api/script/run", c.body, c.hdr)
		if w.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body %s)", c.name, w.Code, c.want, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: content-type %q", c.name, ct)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://vigil.test/api/script/run", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
}

func TestScriptRunWithoutSchedulerIs409(t *testing.T) {
	s, _, st, _ := newActionServer(t, false)
	addActionScenario(t, st, "s1", model.StatePendingApproval, model.MutationReadOnly)
	w := postAction(s, "/api/script/run", `{"id":"s1","env":"dev"}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	// the page learns it from the scripts payload too
	g := get(t, s, "/api/scripts", nil)
	var p struct {
		Actions struct{ Run, Approve bool } `json:"actions"`
		Envs    []string                    `json:"envs"`
		Default string                      `json:"default_env"`
		Scripts []struct {
			Reproduction *reproductionView `json:"reproduction"`
			SourceRef    string            `json:"source_ref"`
		} `json:"scripts"`
	}
	_ = json.Unmarshal(g.Body.Bytes(), &p)
	if p.Actions.Run || !p.Actions.Approve || p.Default != "dev" || len(p.Envs) != 2 {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.Scripts) != 1 || p.Scripts[0].Reproduction == nil || !p.Scripts[0].Reproduction.Reproduced || p.Scripts[0].SourceRef != "PROJ-123" {
		t.Fatalf("scripts = %+v", p.Scripts)
	}
	// no actions at all → 503
	s.SetScriptActions(nil)
	if w := postAction(s, "/api/script/approve", `{"id":"s1"}`, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no actions: %d", w.Code)
	}
}

func TestScriptApproveAndRejectReceipts(t *testing.T) {
	s, _, st, rc := newActionServer(t, false)
	addActionScenario(t, st, "s1", model.StatePendingApproval, model.MutationReadOnly)
	addActionScenario(t, st, "s2", model.StateNeedsReview, model.MutationReadOnly)
	w := postAction(s, "/api/script/approve", `{"id":"s1"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var out struct {
		State   string `json:"state"`
		Cadence string `json:"cadence"`
		NextDue string `json:"next_due_at"`
		Message string `json:"message"`
		Jira    *approval.JiraResult
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "ACTIVE" || out.Cadence != "daily" || out.NextDue == "" || out.Jira == nil || out.Jira.Key != "PROJ-123" || out.Jira.CommentID != "501" || !strings.Contains(out.Message, "정식 검사") {
		t.Fatalf("receipt = %+v jira=%+v", out, out.Jira)
	}
	if rc.key != "PROJ-123" || !strings.Contains(rc.body, "vigil run s1") {
		t.Fatalf("comment = %q %q", rc.key, rc.body)
	}
	if w := postAction(s, "/api/script/approve", `{"id":"s1"}`, nil); w.Code != http.StatusConflict { // already ACTIVE
		t.Fatalf("second approve: %d %s", w.Code, w.Body.String())
	}
	if w := postAction(s, "/api/script/approve", `{"id":"nope"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown approve: %d", w.Code)
	}
	if w := postAction(s, "/api/script/approve", `{"id":"s2"}`, map[string]string{"Origin": "http://other.test"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin approve: %d", w.Code)
	}
	w = postAction(s, "/api/script/reject", `{"id":"s2"}`, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"REJECTED"`) {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
	if sc, _ := st.GetScenario(context.Background(), "p", "s2"); sc.State != model.StateRejected {
		t.Fatalf("state = %s", sc.State)
	}
	if w := postAction(s, "/api/script/reject", `{"id":"nope"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown reject: %d", w.Code)
	}
}

// H-1: the scripts payload carries the three-way verdict and its reason, and the
// page has a badge for each verdict.
func TestScriptsPayloadCarriesReproductionVerdict(t *testing.T) {
	s, _, st, _ := newActionServer(t, false)
	addActionScenario(t, st, "s1", model.StatePendingApproval, model.MutationReadOnly)
	if err := st.SetScenarioPendingApproval(context.Background(), "p", "s1",
		`{"verdict":"disputed","reproduced":false,"at_step":13,"claimed_step":9,"symptom":"제목 40자","why":"실행은 13단계에서 실패"}`); err != nil {
		t.Fatal(err)
	}
	g := get(t, s, "/api/scripts", nil)
	var p struct {
		Scripts []struct {
			Reproduction *reproductionView `json:"reproduction"`
		} `json:"scripts"`
	}
	_ = json.Unmarshal(g.Body.Bytes(), &p)
	if len(p.Scripts) != 1 || p.Scripts[0].Reproduction == nil {
		t.Fatalf("scripts = %+v", p.Scripts)
	}
	r := p.Scripts[0].Reproduction
	if r.Verdict != "disputed" || r.Reproduced || r.AtStep != 13 || r.ClaimedStep != 9 || r.Why == "" {
		t.Fatalf("reproduction = %+v", r)
	}
	for _, want := range []string{"판정 불일치", "confirmed:", "unconfirmed:", "reproVerdict"} {
		if !strings.Contains(string(scriptsHTML), want) {
			t.Errorf("scripts.html missing %q", want)
		}
	}
}
