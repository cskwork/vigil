package runner

// Integration test against the real training-entry page on both browsers.
// Run with: VIGIL_E2E=1 go test ./internal/runner/ -run E2E -v
//
// It also documents the measured capability matrix: Lightpanda opens
// window.open() in the same target, so scenario 2 (expect_popup) is expected to
// fail with FailPopup there and pass on Chromium.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/browser"
	"vigil/internal/dsl"
	"vigil/internal/model"
)

// e2eBase is the deployed origin the opt-in E2E test drives (set VIGIL_E2E_BASE); the
// page assertions below were written for one specific entry page and are examples.
var e2eBase = os.Getenv("VIGIL_E2E_BASE")

const e2eEntryYAML = `
scenario: {id: training-entry-smoke, version: 1, title: "training entry smoke"}
covers: {capability: entry.training}
steps:
  - goto: /lms-web/training-entry
  - wait_for: {by: css, value: ".school-btn-wrap button"}
  - assert_text: {value: "초등"}
  - click: {by: text, text: "중학"}
  - assert_text: {value: "정보"}
  - assert_count: {by: css, value: ".level-btn-wrap li", min: 1}
  - assert_attr: {by: css, value: ".school-btn-wrap li.active", attr: class, contains: active}
  - screenshot: after-middle-school
assert:
  no_http_5xx: true
oracle: {source: spec}
`

const e2ePopupYAML = `
scenario: {id: training-entry-teacher-popup, version: 1, title: "teacher entry opens a new tab"}
covers: {capability: entry.training.teacher}
browser: {popup: true}
steps:
  - goto: /lms-web/training-entry
  - wait_for: {by: css, value: ".teacher-entry button"}
  - click: {by: css, value: ".teacher-entry button"}
  - expect_popup: {timeout: 20s}
  - assert_url: {contains: "lms-web"}
oracle: {source: spec}
`

func e2eProviders(t *testing.T) map[model.Browser]browser.Provider {
	t.Helper()
	logDir := filepath.Join(t.TempDir(), "logs")
	out := map[model.Browser]browser.Provider{}

	// Lightpanda: attach to 9333 when healthy, else launch bin/lightpanda on 9334.
	lp := browser.NewLightpanda("../../bin/lightpanda", "127.0.0.1", 9333, logDir)
	if !lp.Healthy(context.Background()) {
		if _, err := os.Stat("../../bin/lightpanda"); err != nil {
			t.Logf("lightpanda skipped: no server on 9333 and no binary: %v", err)
		} else {
			lp = browser.NewLightpanda("../../bin/lightpanda", "127.0.0.1", 9334, logDir)
		}
	}
	if lp != nil {
		out[model.BrowserLightpanda] = lp
	}
	if bin, err := browser.DetectChromium(); err == nil {
		out[model.BrowserChromium] = browser.NewChromium(bin, true, logDir)
	} else {
		t.Logf("chromium skipped: %v", err)
	}
	return out
}

func TestE2ETrainingEntry(t *testing.T) {
	if os.Getenv("VIGIL_E2E") != "1" {
		t.Skip("set VIGIL_E2E=1 to run against the real target")
	}
	providers := e2eProviders(t)
	if len(providers) == 0 {
		t.Skip("no browser available")
	}
	r := New(providers)
	defer r.Close()
	defer func() {
		for _, p := range providers {
			_ = p.Stop()
		}
	}()
	entry, err := dsl.Parse([]byte(e2eEntryYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := entry.Validate(nil); err != nil {
		t.Fatal(err)
	}
	popup, err := dsl.Parse([]byte(e2ePopupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := popup.Validate(nil); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []model.Browser{model.BrowserLightpanda, model.BrowserChromium} {
		p, ok := providers[kind]
		if !ok {
			continue
		}
		t.Run(string(kind)+"/smoke", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := p.Ensure(ctx); err != nil {
				t.Skipf("%s unavailable: %v", kind, err)
			}
			dir := evidenceDir(t, string(kind)+"-smoke")
			res, err := r.Run(ctx, Spec{Scenario: entry, Browser: kind, BaseURL: e2eBase, EvidenceDir: dir, StepTimeout: 20 * time.Second, RunTimeout: 90 * time.Second})
			if err != nil {
				t.Fatalf("runner error: %v", err)
			}
			logResult(t, res)
			if !res.Passed {
				t.Fatalf("smoke scenario failed on %s: class=%s err=%s", kind, res.Class, res.Error)
			}
			for _, name := range []string{"steps.json", "console.json", "network.json", "after-middle-school.png"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Errorf("missing artifact %s: %v", name, err)
				}
			}
			if len(res.Network) == 0 {
				t.Errorf("expected captured network events on %s", kind)
			}
			assertNoRawTokens(t, dir)
		})
		t.Run(string(kind)+"/popup", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			dir := evidenceDir(t, string(kind)+"-popup")
			res, err := r.Run(ctx, Spec{Scenario: popup, Browser: kind, BaseURL: e2eBase, EvidenceDir: dir, StepTimeout: 20 * time.Second, RunTimeout: 90 * time.Second})
			if err != nil {
				t.Fatalf("runner error: %v", err)
			}
			logResult(t, res)
			assertNoRawTokens(t, dir)
			switch kind {
			case model.BrowserChromium:
				if !res.Passed {
					t.Fatalf("popup scenario must pass on chromium: class=%s err=%s", res.Class, res.Error)
				}
				if !res.Capabilities["popup"] {
					t.Errorf("chromium should report popup capability")
				}
			case model.BrowserLightpanda:
				// Measured: Lightpanda navigates the current target instead of opening one.
				if res.Passed {
					t.Logf("NOTE: lightpanda now supports popups; update the capability matrix")
				} else if res.Class != FailPopup {
					t.Fatalf("lightpanda popup failure must be classified as popup, got %s: %s", res.Class, res.Error)
				}
				if _, err := os.Stat(filepath.Join(dir, "dom.html")); err != nil && !res.Passed {
					t.Errorf("dom.html expected on failure: %v", err)
				}
			}
		})
	}
}

// evidenceDir keeps artifacts under $VIGIL_E2E_EVIDENCE when set, else a temp dir.
func evidenceDir(t *testing.T, name string) string {
	t.Helper()
	if root := os.Getenv("VIGIL_E2E_EVIDENCE"); root != "" {
		return filepath.Join(root, name)
	}
	return filepath.Join(t.TempDir(), name)
}

// assertNoRawTokens fails when a query-string credential reached an artifact.
func assertNoRawTokens(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"steps.json", "network.json", "console.json", "dom.html"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "lmsToken=ey") || strings.Contains(string(b), "&token=f") {
			t.Errorf("%s contains a raw token value", name)
		}
	}
}

func logResult(t *testing.T, res *Result) {
	t.Helper()
	for _, st := range res.Steps {
		status := "ok"
		if !st.OK {
			status = "FAIL[" + string(st.Class) + "]"
		}
		t.Logf("  step %d %-18s %-6s %s %s %s", st.Index, st.Kind, status, st.Duration.Round(time.Millisecond), st.Actual, st.Error)
	}
	caps, _ := json.Marshal(res.Capabilities)
	t.Logf("passed=%v class=%q duration=%s console=%d network=%d final=%s caps=%s notes=%v error=%q",
		res.Passed, res.Class, res.Duration().Round(time.Millisecond), len(res.Console), len(res.Network), res.FinalURL, caps, res.Notes, res.Error)
}
