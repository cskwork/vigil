package evidence

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/model"
)

func TestRunAndAgentDirs(t *testing.T) {
	s := New(t.TempDir())
	at := time.Date(2026, 9, 1, 3, 4, 5, 0, time.UTC)
	dir, err := s.RunDir("home-landing", at, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Root(), "runs", "home-landing", "20260901T030405.000Z-a2")
	if dir != want {
		t.Fatalf("run dir %s != %s", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatal("run dir not created")
	}
	ad, err := s.AgentDir("PROJ-1001", at)
	if err != nil {
		t.Fatal(err)
	}
	if ad != filepath.Join(s.Root(), "agent", "PROJ-1001", "20260901T030405.000Z") {
		t.Fatalf("agent dir %s", ad)
	}
	if _, err := s.RunDir("", at, 1); err == nil {
		t.Fatal("empty scenario id must fail")
	}
	if err := MarkPass(dir); err != nil || !IsPass(dir) {
		t.Fatal("pass marker")
	}
}

func TestWriteIncident(t *testing.T) {
	s := New(t.TempDir())
	runDir, _ := s.RunDir("home-landing", time.Now(), 1)
	_ = os.WriteFile(filepath.Join(runDir, "console.json"), []byte(`[{"level":"log","text":"hi"},{"level":"error","text":"TypeError: x is undefined","url":"https://x/app.js","line":12}]`), 0o644)
	_ = os.WriteFile(filepath.Join(runDir, "network.json"), []byte(`[{"method":"GET","url":"https://x/api/ok","status":200},{"method":"POST","url":"https://x/api/auth","status":502},{"method":"GET","url":"https://x/api/dns","status":0,"failed":true,"error":"net::ERR_NAME_NOT_RESOLVED"}]`), 0o644)
	_ = os.WriteFile(filepath.Join(runDir, "dom.html"), []byte("<html/>"), 0o644)

	run := &model.Run{
		ProjectID: "demo", ScenarioID: "home-landing", ScenarioVersion: 3, FeatureID: "PROJ-1001", ShippedSHA: "6f22ff7a1dcc",
		Browser: model.BrowserLightpanda, Outcome: model.OutcomeAppFailure, Attempt: 2,
		StartedAt: time.Now().Add(-time.Minute), FinishedAt: time.Now(), DurationMs: 1234,
		FailedStep: 4, FailedAction: "assert_text", Expected: "초등", Actual: "(not found)", Error: "assert_text: 초등 not in body", EvidenceDir: runDir,
	}
	in := IncidentInput{
		Incident: &model.Incident{Kind: model.IncidentAppRegression, Title: "Landing tabs missing"},
		Run:      run,
		Scenario: &model.Scenario{ID: "home-landing", Title: "홈 랜딩"},
		Version:  &model.ScenarioVersion{Version: 3, YAML: "scenario:\n  id: home-landing\n  version: 3\n"},
		Feature:  &model.Feature{ID: "PROJ-1001", LatestShippedSHA: "6f22ff7a1dcc", Summary: "Merge feat/PROJ-1001", ChangedPaths: []string{"src/components/entry/training.vue"}},
		Reason:   "business assertion failed on both attempts",
	}
	md, js, err := s.WriteIncident(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(md), time.Now().UTC().Format("20060102")) || !strings.HasSuffix(md, "-home-landing.md") {
		t.Fatalf("md name %s", md)
	}
	body, _ := os.ReadFile(md)
	for _, want := range []string{
		"# Landing tabs missing", "| shipped sha | 6f22ff7a1dcc |", "| scenario | home-landing v3 |", "| attempt | 2 |",
		"| expected | 초등 |", "| actual | (not found) |", "TypeError: x is undefined", "POST https://x/api/auth → 502", "ERR_NAME_NOT_RESOLVED",
		"dom: `" + filepath.Join(runDir, "dom.html"), "```yaml\nscenario:\n  id: home-landing", "`src/components/entry/training.vue`",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("markdown missing %q\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "GET https://x/api/ok") {
		t.Error("200 request must not be listed as failed")
	}
	raw, _ := os.ReadFile(js)
	var doc IncidentDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ShippedSHA != "6f22ff7a1dcc" || doc.ScenarioVersion != 3 || doc.FailedStep != 4 || len(doc.ConsoleErrors) != 1 || len(doc.FailedRequests) != 2 || doc.ScenarioYAML == "" {
		t.Fatalf("json doc: %+v", doc)
	}
	if _, _, err := s.WriteIncident(context.Background(), IncidentInput{}); err == nil {
		t.Fatal("incident without run must fail")
	}
}

func TestPruneRetention(t *testing.T) {
	s := New(t.TempDir())
	now := time.Now().UTC()
	oldPass, _ := s.RunDir("sc", now.Add(-5*24*time.Hour), 1)
	_ = MarkPass(oldPass)
	freshPass, _ := s.RunDir("sc", now.Add(-1*time.Hour), 1)
	_ = MarkPass(freshPass)
	oldFail, _ := s.RunDir("sc", now.Add(-5*24*time.Hour), 2) // 5 days: kept (fail retention 30)
	veryOldFail, _ := s.RunDir("sc2", now.Add(-40*24*time.Hour), 1)
	oldAgent, _ := s.AgentDir("f1", now.Add(-40*24*time.Hour))
	freshAgent, _ := s.AgentDir("f2", now)
	if err := s.Prune(3, 30); err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(oldPass) {
		t.Error("old pass must be pruned")
	}
	if !exists(freshPass) || !exists(oldFail) {
		t.Error("fresh pass / recent fail must be kept")
	}
	if exists(veryOldFail) || exists(filepath.Dir(veryOldFail)) {
		t.Error("very old fail (and its empty scenario dir) must be pruned")
	}
	if exists(oldAgent) || !exists(freshAgent) {
		t.Error("agent retention")
	}
}
