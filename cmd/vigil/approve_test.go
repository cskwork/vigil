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

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// newApproveApp loads a real config (jira.dry_run on) and seeds three
// PENDING_APPROVAL reproduce scripts from issue PROJ-123.
func newApproveApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("https://www.example.com/app\n"), 0o644)
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\njira:\n  dry_run: true\nschedule:\n  daily_at: \"09:00\"\n  active_hours:\n    tz: Asia/Seoul\n"), 0o644)
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
	for _, id := range []string{"s1", "s2", "s3"} {
		m := &model.Scenario{ID: id, ProjectID: "p", State: model.StatePendingApproval, Fingerprint: "fp-" + id, Title: "탭 중복 " + id, Class: "P1", Mutation: model.MutationReadOnly,
			OracleSource: "spec", Origin: "agent", SoakTarget: 3, CurrentVersion: 1, SourceKind: model.FeatureKindIssue, SourceRef: "PROJ-123", Reproduction: `{"reproduced":true,"at_step":2}`}
		v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "agent"}
		if err := st.CreateScenario(ctx, m, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	out := &bytes.Buffer{}
	return &app{cfgPath: cfgPath, out: out, cfg: cfg, st: st, log: log.New(io.Discard, "", 0)}, out
}

func TestApproveRejectCommands(t *testing.T) {
	a, out := newApproveApp(t)
	ctx := context.Background()

	if err := a.cmdApprove(ctx, []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "s1: 승인 대기 → 정식 검사 (매일 실행") || !strings.Contains(got, "Jira PROJ-123 댓글(모의)") {
		t.Fatalf("approve output = %q", got)
	}
	sc, _ := a.st.GetScenario(ctx, "p", "s1")
	if sc.State != model.StateActive || sc.Cadence != "daily" || sc.ApprovedAt == nil || sc.NextDueAt == nil {
		t.Fatalf("s1 = %+v", sc)
	}
	files, _ := filepath.Glob(filepath.Join(a.cfg.Abs(a.cfg.Evidence.Dir), "jira", "PROJ-123-*.md"))
	if len(files) != 1 {
		t.Fatalf("dry-run files = %v", files)
	}
	body, _ := os.ReadFile(files[0])
	if !strings.Contains(string(body), "[vigil] QA 스크립트 승인: s1 v1 — 탭 중복 s1") || !strings.Contains(string(body), "매일 09:00 Asia/Seoul") {
		t.Fatalf("dry-run body:\n%s", body)
	}

	out.Reset()
	if err := a.cmdApprove(ctx, []string{"--soak", "s2"}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := a.st.GetScenario(ctx, "p", "s2"); sc.State != model.StateSoak || sc.Cadence != "" {
		t.Fatalf("s2 = %+v (out %q)", sc, out.String())
	}
	if !strings.Contains(out.String(), "안정화 중") {
		t.Fatalf("soak output = %q", out.String())
	}

	out.Reset()
	if err := a.cmdReject(ctx, []string{"s3"}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := a.st.GetScenario(ctx, "p", "s3"); sc.State != model.StateRejected {
		t.Fatalf("s3 = %+v", sc)
	}
	if !strings.Contains(out.String(), "s3: 승인 대기 → 제외") {
		t.Fatalf("reject output = %q", out.String())
	}

	// ACTIVE cannot be approved again; unknown ids error out
	if err := a.cmdApprove(ctx, []string{"s1"}); err == nil || !strings.Contains(err.Error(), "is ACTIVE") {
		t.Fatalf("re-approve err = %v", err)
	}
	if err := a.cmdApprove(ctx, []string{"nope"}); err == nil {
		t.Fatal("unknown id must fail")
	}
	if err := a.cmdApprove(ctx, nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage err = %v", err)
	}

	// --json emits the receipt object
	a.jsonOut = true
	out.Reset()
	_ = a.st.SetScenarioPendingApproval(ctx, "p", "s3", "")
	if err := a.cmdApprove(ctx, []string{"s3"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `"state": "ACTIVE"`) || !strings.Contains(got, `"dry_run_path"`) {
		t.Fatalf("json output = %s", got)
	}
}
