package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/evidence"
	"vigil/internal/model"
	"vigil/internal/store"
)

// J-3: `vigil status` leads a failing run and an open incident with the same
// plain sentence the dashboard shows, and keeps the raw error out of the text
// output (it stays in --json and in the log).
func TestStatusPrintsCauseSentence(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("https://www.example.com/app\n"), 0o644)
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Abs(cfg.State.Path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	_ = st.UpsertProject(ctx, "p", cfg.Target.BaseURL)
	m := &model.Scenario{ID: "entry-tabs", ProjectID: "p", State: model.StateActive, Fingerprint: "fp", Title: "입장 탭", Class: "P1",
		Mutation: model.MutationReadOnly, OracleSource: "spec", Origin: "seed", SoakTarget: 3, CurrentVersion: 1}
	v := &model.ScenarioVersion{ScenarioID: "entry-tabs", Version: 1, YAML: "scenario:\n  id: entry-tabs\n", Fingerprint: "fp", CreatedBy: "seed"}
	if err := st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rawErr := `step 4 (click): locate by=role role=button name="가져오기": 0 matches; candidates(role=button): "내보내기"`
	runID, err := st.InsertRun(ctx, &model.Run{ProjectID: "p", ScenarioID: "entry-tabs", ScenarioVersion: 1, Browser: model.BrowserChromium,
		Outcome: model.OutcomeScriptDrift, StartedAt: now, FinishedAt: now, FailedStep: 4, FailedAction: "click",
		Expected: "element present", Actual: `0 matches; candidates(role=button): "내보내기"`, Error: rawErr})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIncident(ctx, &model.Incident{ProjectID: "p", Kind: model.IncidentAppRegression, ScenarioID: "entry-tabs",
		RunID: runID, Title: "APP_REGRESSION: 입장 탭", State: "OPEN", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	a := &app{cfgPath: cfgPath, out: out, cfg: cfg, st: st, ev: evidence.New(cfg.Abs(cfg.Evidence.Dir)), log: log.New(io.Discard, "", 0)}
	if err := a.cmdStatus(ctx); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want := `4단계에서 누르려던 "가져오기" 버튼이 화면에 없습니다.`
	if strings.Count(got, want) != 2 {
		t.Errorf("expected the sentence under both the run and the incident, got:\n%s", got)
	}
	if !strings.Contains(got, "원인: "+want) {
		t.Errorf("incident line missing 원인:\n%s", got)
	}
	if strings.Contains(got, "candidates(") || strings.Contains(got, "locate by=role") {
		t.Errorf("raw error leaked into the text output:\n%s", got)
	}
}
