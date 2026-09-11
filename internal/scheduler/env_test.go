package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/config"
	"vigil/internal/gate"
	"vigil/internal/model"
	"vigil/internal/runner"
)

func withEnvs(h *harness) {
	h.cfg.Target.DefaultEnv = "stg"
	h.cfg.Target.Environments = map[string]config.Environment{
		"stg":  {Name: "stg", BaseURL: "https://stg.example.test", AllowedHosts: []string{"stg.example.test"}},
		"prod": {Name: "prod", BaseURL: "https://www.example.test", AllowedHosts: []string{"example.test"}, ReadOnly: true},
	}
}

func TestRunOnEnvironmentUsesItsBaseURLAndRecordsIt(t *testing.T) {
	var seen runner.Spec
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { seen = spec; return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	withEnvs(h)
	ctx := context.Background()
	h.addScenario(t, "entry", "P1", model.StateActive, model.MutationReadOnly, nil, 0)

	run, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if seen.BaseURL != "https://www.example.test" || seen.Environment != "prod" || strings.Join(seen.AllowedHosts, ",") != "example.test" {
		t.Fatalf("spec env: base=%q env=%q hosts=%v", seen.BaseURL, seen.Environment, seen.AllowedHosts)
	}
	if run.Environment != "prod" || run.Outcome != model.OutcomePass {
		t.Fatalf("run %+v", run)
	}
	stored, _ := h.st.ListRuns(ctx, "p", "entry", 1)
	if len(stored) != 1 || stored[0].Environment != "prod" {
		t.Fatalf("stored run environment: %+v", stored)
	}
	b, err := os.ReadFile(filepath.Join(run.EvidenceDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if doc["environment"] != "prod" {
		t.Fatalf("result.json environment = %v", doc["environment"])
	}

	// default env when no --env: the job payload still names it
	run, err = h.s.RunScenarioNow(ctx, "entry", "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Environment != "stg" || seen.BaseURL != "https://stg.example.test" {
		t.Fatalf("default env run: %q base=%q", run.Environment, seen.BaseURL)
	}
}

func TestEnqueuePayloadCarriesEnvAndDedupsPerEnv(t *testing.T) {
	h := newHarness(t, &fakeRunner{}, nil, nil)
	withEnvs(h)
	ctx := context.Background()
	h.addScenario(t, "entry", "P1", model.StateActive, model.MutationReadOnly, nil, 0)
	m, _ := h.st.GetScenario(ctx, "p", "entry")

	id1, created, err := h.s.EnqueueScenario(ctx, m, model.PriorityP1, "", "")
	if err != nil || !created {
		t.Fatal(err, created)
	}
	id2, created, err := h.s.EnqueueScenarioEnv(ctx, m, model.PriorityP1, "", "", "prod")
	if err != nil || !created || id1 == id2 {
		t.Fatalf("prod run must be a separate job: %v created=%v %d/%d", err, created, id1, id2)
	}
	if _, created, _ := h.s.EnqueueScenarioEnv(ctx, m, model.PriorityP1, "", "", "prod"); created {
		t.Fatal("same env must dedup")
	}
	if _, _, err := h.s.EnqueueScenarioEnv(ctx, m, model.PriorityP1, "", "", "nope"); err == nil {
		t.Fatal("unknown env must be rejected at enqueue")
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobReady}, 10)
	got := map[int64]string{}
	for _, j := range jobs {
		got[j.ID] = parseRunPayload(j.Payload).Env
	}
	if got[id1] != "stg" || got[id2] != "prod" {
		t.Fatalf("payload env round-trip: %v", got)
	}
}

func TestReadOnlyEnvRefusesMutatingScenarioAsFailedJob(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	withEnvs(h)
	ctx := context.Background()
	h.addScenario(t, "deploy-content", "P1", model.StateActive, model.MutationReversible, []string{"teacher-a"}, 0)

	_, err := h.s.RunScenarioNowEnv(ctx, "deploy-content", "", "prod")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only refusal, got %v", err)
	}
	if fr.calls != 0 {
		t.Fatal("runner must never run on a read-only environment refusal")
	}
	jobs, _ := h.st.ListJobs(ctx, "p", []model.JobState{model.JobFailed}, 10)
	if len(jobs) != 1 || !strings.Contains(jobs[0].LastError, `environment "prod" is read-only`) || !strings.Contains(jobs[0].LastError, "reversible") {
		t.Fatalf("job must be FAILED with the reason: %+v", jobs)
	}
	if runs, _ := h.st.ListRuns(ctx, "p", "deploy-content", 10); len(runs) != 0 {
		t.Fatal("no run row on refusal")
	}
	if held, _ := h.st.TryAcquireLocks(ctx, []string{"teacher-a"}, "probe", 0); !held {
		t.Fatal("locks must not be left held by a refused job")
	}
	_ = h.st.ReleaseLocks(ctx, []string{"teacher-a"}, "probe")
	// the same scenario runs on the writable default env
	if run, err := h.s.RunScenarioNow(ctx, "deploy-content", ""); err != nil || run.Environment != "stg" {
		t.Fatalf("default env run: %v %+v", err, run)
	}
}

func TestGotoOutsideEnvAllowlistIsEnvFailure(t *testing.T) {
	// real runner validation path: a stub that mirrors envRejects via the runner package
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) {
		return runner.New(nil).Run(context.Background(), spec) // no providers → after env validation only
	}}
	h := newHarness(t, fr, nil, nil)
	withEnvs(h)
	ctx := context.Background()
	y := strings.Replace(sprintf(scenarioYAML, "abs", "P1", "read-only"), "goto: /app/training-entry", "goto: https://other.test/app/training-entry", 1)
	m := &model.Scenario{ID: "abs", ProjectID: "p", State: model.StateActive, Fingerprint: "fp-abs", Class: "P1", Mutation: model.MutationReadOnly, OracleSource: "contract", Origin: "seed", SoakTarget: 3}
	v := &model.ScenarioVersion{ScenarioID: "abs", Version: 1, YAML: y, Fingerprint: "fp-abs", CreatedBy: "seed"}
	if err := h.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	run, err := h.s.RunScenarioNowEnv(ctx, "abs", "", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != model.OutcomeEnvFailure || !strings.Contains(run.Error, runner.ErrOutsideAllowlist) || run.Attempt != 1 {
		t.Fatalf("run = %s attempt=%d err=%q", run.Outcome, run.Attempt, run.Error)
	}
}

// markerServer serves an index.html carrying one asset marker.
func markerServer(t *testing.T, version string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<script src="/assets/index.js?v=` + version + `"></script>`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// H-3: a run is tagged with the marker of the environment it ran against, not
// with the default environment's build.
func TestRunRecordsMarkerOfItsEnvironment(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	withEnvs(h)
	h.cfg.Deployment.Readiness.Strategy = "asset_version"
	h.cfg.Evidence.ChromiumCapture = "per-deploy"
	stg, prod := markerServer(t, "1000"), markerServer(t, "2000")
	h.cfg.Target.Environments = map[string]config.Environment{
		"stg":  {BaseURL: stg, AllowedHosts: []string{"127.0.0.1"}, AssetPage: stg + "/"},
		"prod": {BaseURL: prod, AllowedHosts: []string{"127.0.0.1"}, AssetPage: prod + "/"},
	}
	h.s.gate = gate.New(h.cfg)
	ctx := context.Background()
	h.addScenario(t, "entry", "P1", model.StateActive, model.MutationReadOnly, nil, 0)

	stgRun, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "stg")
	if err != nil {
		t.Fatal(err)
	}
	prodRun, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if stgRun.DeployMarker != "1000" || prodRun.DeployMarker != "2000" {
		t.Fatalf("markers: stg=%q prod=%q", stgRun.DeployMarker, prodRun.DeployMarker)
	}
}

// H-3: two environments on the same marker must each get their own Chromium
// evidence capture; the marker alone must not suppress the second one.
func TestEvidenceCaptureDedupIsPerEnvironment(t *testing.T) {
	fr := &fakeRunner{fn: func(_ int, spec runner.Spec) (*runner.Result, error) { return passResult(spec), nil }}
	h := newHarness(t, fr, nil, nil)
	withEnvs(h)
	h.cfg.Deployment.Readiness.Strategy = "asset_version"
	h.cfg.Evidence.ChromiumCapture = "per-deploy"
	same := markerServer(t, "1000")
	h.cfg.Target.Environments = map[string]config.Environment{
		"stg":  {BaseURL: same, AllowedHosts: []string{"127.0.0.1"}, AssetPage: same + "/"},
		"prod": {BaseURL: same, AllowedHosts: []string{"127.0.0.1"}, AssetPage: same + "/"},
	}
	h.s.gate = gate.New(h.cfg)
	ctx := context.Background()
	h.addScenario(t, "entry", "P1", model.StateActive, model.MutationReadOnly, nil, 0)

	if _, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "stg"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "prod"); err != nil {
		t.Fatal(err)
	}
	runs, _ := h.st.ListRuns(ctx, "p", "entry", 20)
	envs := map[string]bool{}
	for _, r := range runs {
		if r.Browser == model.BrowserChromium {
			envs[r.Environment] = true
		}
	}
	if !envs["stg"] || !envs["prod"] {
		t.Fatalf("chromium captures per environment = %v (runs %d)", envs, len(runs))
	}
	// A repeat on stg with the same marker stays deduped.
	before := len(runs)
	if _, err := h.s.RunScenarioNowEnv(ctx, "entry", "", "stg"); err != nil {
		t.Fatal(err)
	}
	runs, _ = h.st.ListRuns(ctx, "p", "entry", 20)
	if len(runs) != before+1 {
		t.Fatalf("second stg run must not capture again: %d → %d", before, len(runs))
	}
}
