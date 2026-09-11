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

const exportScenarioYAML = `scenario:
  id: %s
  version: 2
  title: 대시보드 열기
uses: [login]
steps:
  - goto: /dashboard
  - use_flow: pick
  - assert_text: { value: 환영합니다 }
assert:
  no_http_5xx: true
oracle:
  source: spec
`

// newExportApp loads a config with two environments and seeds scenarios in every
// state plus two flows, all in a temp dir.
func newExportApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("https://stg.example.com/app\n"), 0o644)
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte(`version: 4
project:
  id: p
target:
  default_env: stg
  environments:
    stg:
      base_url: https://stg.example.com
      allowed_hosts: [stg.example.com]
    prod:
      base_url: https://www.example.com
      allowed_hosts: [www.example.com]
      read_only: true
`), 0o644)
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
	for id, state := range map[string]model.ScenarioState{
		"s-active": model.StateActive, "s-soak": model.StateSoak, "s-pending": model.StatePendingApproval,
		"s-review": model.StateNeedsReview, "s-rejected": model.StateRejected, "s-retired": model.StateRetired,
	} {
		m := &model.Scenario{ID: id, ProjectID: "p", State: state, Fingerprint: "fp-" + id, Title: "대시보드 열기", Class: "P1", Mutation: model.MutationReadOnly,
			OracleSource: "spec", Origin: "agent", SoakTarget: 3, CurrentVersion: 2}
		v := &model.ScenarioVersion{ScenarioID: id, Version: 2, YAML: strings.Replace(exportScenarioYAML, "%s", id, 1), Fingerprint: "fp-" + id, CreatedBy: "agent"}
		if err := st.CreateScenario(ctx, m, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.UpsertFlow(ctx, "p", "login", 1, "flow:\n  id: login\n  version: 1\nsteps:\n  - goto: /login\n  - fill: { by: label, name: 아이디, input: u1 }\n")
	_ = st.UpsertFlow(ctx, "p", "pick", 1, "flow:\n  id: pick\n  version: 1\nsteps:\n  - click: { by: test_id, value: course-1 }\n")
	out := &bytes.Buffer{}
	return &app{cfgPath: cfgPath, out: out, cfg: cfg, st: st, log: log.New(io.Discard, "", 0)}, out
}

func TestExportCommand(t *testing.T) {
	a, out := newExportApp(t)
	ctx := context.Background()

	// single scenario, default dir (relative to the config), default env
	code, err := a.cmdExport(ctx, []string{"s-active"})
	if err != nil || code != 0 {
		t.Fatalf("export: code=%d err=%v out=%s", code, err, out.String())
	}
	path := filepath.Join(a.cfg.BaseDir, "export", "playwright", "s-active.spec.ts")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected %s: %v (out %q)", path, err, out.String())
	}
	s := string(body)
	for _, must := range []string{
		"// scenario: s-active\n// version: 2\n// title: 대시보드 열기\n// env: stg (https://stg.example.com)",
		"YAML DSL is canonical",
		"import { test, expect } from '@playwright/test';",
		"const BASE_URL = process.env.BASE_URL ?? 'https://stg.example.com';",
		"// step 1: goto (flow login)",
		"await page.goto(BASE_URL + '/login');",
		"// step 4: click (flow pick)",
		"await page.getByTestId('course-1').click();",
		"await expect(page.locator('body')).toContainText('환영합니다');",
		"expect(http5xx, 'no_http_5xx').toEqual([]);",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("spec lacks %q:\n%s", must, s)
		}
	}
	if !strings.Contains(out.String(), "s-active → "+path) {
		t.Errorf("output = %q", out.String())
	}

	// --env / -o
	out.Reset()
	dir := filepath.Join(t.TempDir(), "pw")
	if code, err := a.cmdExport(ctx, []string{"--env", "prod", "-o", dir, "s-soak"}); err != nil || code != 0 {
		t.Fatalf("export --env prod: code=%d err=%v", code, err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "s-soak.spec.ts"))
	if !strings.Contains(string(body), "// env: prod (https://www.example.com)") || !strings.Contains(string(body), "?? 'https://www.example.com';") {
		t.Errorf("prod export:\n%s", body)
	}

	// --all: only ACTIVE/SOAK/PENDING_APPROVAL/NEEDS_REVIEW, with a summary line
	out.Reset()
	dir = filepath.Join(t.TempDir(), "all")
	if code, err := a.cmdExport(ctx, []string{"--all", "-o", dir}); err != nil || code != 0 {
		t.Fatalf("export --all: code=%d err=%v out=%s", code, err, out.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.spec.ts"))
	if len(files) != 4 {
		t.Fatalf("--all files = %v", files)
	}
	for _, id := range []string{"s-active", "s-soak", "s-pending", "s-review"} {
		if _, err := os.Stat(filepath.Join(dir, id+".spec.ts")); err != nil {
			t.Errorf("missing %s.spec.ts", id)
		}
	}
	if !strings.Contains(out.String(), "내보내기 완료: 4개 (playwright, env=stg) → "+dir) {
		t.Errorf("summary = %q", out.String())
	}

	// unknown scenario and unsupported format exit 1
	if code, err := a.cmdExport(ctx, []string{"nope"}); err == nil || code != 1 {
		t.Fatalf("unknown scenario: code=%d err=%v", code, err)
	}
	if code, err := a.cmdExport(ctx, []string{"--format", "cypress", "s-active"}); err == nil || code != 1 || !strings.Contains(err.Error(), "unsupported export format") {
		t.Fatalf("unsupported format: code=%d err=%v", code, err)
	}
	if code, err := a.cmdExport(ctx, []string{"--env", "qa", "s-active"}); err == nil || code != 1 {
		t.Fatalf("unknown env: code=%d err=%v", code, err)
	}
	if code, err := a.cmdExport(ctx, nil); err == nil || code != 2 || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage: code=%d err=%v", code, err)
	}

	// the header names the store's current version, not the YAML's
	out.Reset()
	v3 := &model.ScenarioVersion{ScenarioID: "s-active", Version: 3, YAML: strings.Replace(exportScenarioYAML, "%s", "s-active", 1), Fingerprint: "fp-s-active", CreatedBy: "agent"}
	if err := a.st.AddScenarioVersion(ctx, "p", v3, nil); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(t.TempDir(), "v3")
	if code, err := a.cmdExport(ctx, []string{"-o", dir, "s-active"}); err != nil || code != 0 {
		t.Fatalf("export v3: code=%d err=%v", code, err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "s-active.spec.ts"))
	if !strings.Contains(string(body), "// version: 3\n") {
		t.Errorf("header should carry the store version 3:\n%s", body)
	}

	// --json
	a.jsonOut = true
	out.Reset()
	if code, err := a.cmdExport(ctx, []string{"-o", t.TempDir(), "s-review"}); err != nil || code != 0 {
		t.Fatalf("json export: code=%d err=%v", code, err)
	}
	if got := out.String(); !strings.Contains(got, `"exported": 1`) || !strings.Contains(got, `"id": "s-review"`) {
		t.Fatalf("json output = %s", got)
	}
}
