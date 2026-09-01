package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// stepsFile is the steps.json document (PRD §15 compact metadata + per-step outcome).
type stepsFile struct {
	Scenario struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Title   string `json:"title,omitempty"`
	} `json:"scenario"`
	Browser                 string          `json:"browser"`
	BaseURL                 string          `json:"base_url,omitempty"`
	StartedAt               time.Time       `json:"started_at"`
	FinishedAt              time.Time       `json:"finished_at"`
	DurationMs              int64           `json:"duration_ms"`
	Passed                  bool            `json:"passed"`
	Class                   string          `json:"class,omitempty"`
	Error                   string          `json:"error,omitempty"`
	FinalURL                string          `json:"final_url,omitempty"`
	FailedStep              int             `json:"failed_step,omitempty"`
	GlobalAssertionFailures []string        `json:"global_assertion_failures,omitempty"`
	Capabilities            map[string]bool `json:"capabilities,omitempty"`
	Notes                   []string        `json:"notes,omitempty"`
	Steps                   []StepResult    `json:"steps"`
}

// writeEvidence stores steps.json, console.json, network.json always and
// dom.html + screenshot.png on failure. Secrets are redacted before writing.
func (s *session) writeEvidence() {
	dir := s.spec.EvidenceDir
	if dir == "" {
		return
	}
	res := s.res
	if res.FinishedAt.IsZero() {
		res.FinishedAt = time.Now()
	}
	if !res.Passed && s.pageCtx != nil {
		s.captureFailureArtifacts(dir)
	} else if s.pageCtx != nil {
		s.captureFinalScreenshot(dir)
	}

	var sf stepsFile
	sf.Scenario.ID = s.spec.Scenario.Scenario.ID
	sf.Scenario.Version = s.spec.Scenario.Scenario.Version
	sf.Scenario.Title = s.spec.Scenario.Scenario.Title
	sf.Browser = string(res.Browser)
	sf.BaseURL = s.spec.BaseURL
	sf.StartedAt, sf.FinishedAt = res.StartedAt, res.FinishedAt
	sf.DurationMs = res.Duration().Milliseconds()
	sf.Passed = res.Passed
	sf.Class = string(res.Class)
	sf.Error = s.red.Redact(res.Error)
	sf.FinalURL = s.red.Redact(res.FinalURL)
	if res.FailedStep != nil {
		sf.FailedStep = res.FailedStep.Index
	}
	sf.GlobalAssertionFailures = res.GlobalAssertionFailures
	sf.Capabilities = res.Capabilities
	sf.Notes = res.Notes
	sf.Steps = make([]StepResult, len(res.Steps))
	for i, st := range res.Steps {
		st.Expected = s.red.Redact(st.Expected)
		st.Actual = s.red.Redact(st.Actual)
		st.Error = s.red.Redact(st.Error)
		sf.Steps[i] = st
	}
	s.writeJSON(dir, "steps.json", sf)
	s.writeJSON(dir, "console.json", nonNil(res.Console))
	s.writeJSON(dir, "network.json", nonNil(res.Network))
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *session) writeJSON(dir, name string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		s.note("evidence %s: marshal: %v", name, err)
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		s.note("evidence %s: write: %v", name, err)
		return
	}
	s.res.Artifacts[strings.TrimSuffix(name, ".json")] = path
}

// captureFinalScreenshot keeps positive evidence for a PASS: the last screen as verified.
func (s *session) captureFinalScreenshot(dir string) {
	ctx, cancel := context.WithTimeout(s.pageCtx, evidenceGrace)
	defer cancel()
	png, err := s.screenshot(ctx)
	if err != nil {
		if s.pageCtx.Err() == nil {
			s.res.Capabilities["screenshot"] = false
			s.note("final screenshot not captured on %s: %v", s.spec.Browser, err)
		}
		return
	}
	path := filepath.Join(dir, "screenshot.png")
	if werr := os.WriteFile(path, png, 0o644); werr == nil {
		s.res.ScreenshotPath = path
		s.res.Artifacts["screenshot"] = path
		s.res.Capabilities["screenshot"] = true
	}
}

// captureFailureArtifacts writes dom.html and screenshot.png when the browser can produce them.
func (s *session) captureFailureArtifacts(dir string) {
	ctx, cancel := context.WithTimeout(s.pageCtx, evidenceGrace)
	defer cancel()
	if html, err := s.domHTML(ctx); err == nil {
		path := filepath.Join(dir, "dom.html")
		if werr := os.WriteFile(path, []byte(s.red.Redact(html)), 0o644); werr == nil {
			s.res.DOMPath = path
			s.res.Artifacts["dom"] = path
		}
	} else if s.pageCtx.Err() == nil {
		s.note("dom.html not captured: %v", err)
	}
	if png, err := s.screenshot(ctx); err == nil {
		path := filepath.Join(dir, "screenshot.png")
		if werr := os.WriteFile(path, png, 0o644); werr == nil {
			s.res.ScreenshotPath = path
			s.res.Artifacts["screenshot"] = path
			s.res.Capabilities["screenshot"] = true
		}
	} else if s.pageCtx.Err() == nil {
		s.res.Capabilities["screenshot"] = false
		s.note("screenshot.png not captured on %s: %v", s.spec.Browser, err)
	}
}

// domHTML serializes the document; password field values never appear as attributes,
// but we strip them defensively before serializing.
func (s *session) domHTML(ctx context.Context) (string, error) {
	var html string
	err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
		try { document.querySelectorAll('input[type=password]').forEach(function(i){ i.removeAttribute('value'); }); } catch (e) {}
		return document.documentElement ? document.documentElement.outerHTML : '';
	})()`, &html))
	return html, err
}

func (s *session) screenshot(ctx context.Context) ([]byte, error) {
	var buf []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(c)
		buf = b
		return err
	}))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// artifactName sanitizes a scenario-provided screenshot name.
func artifactName(name, fallback string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".png")
	name = unsafeName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if name == "" {
		name = fallback
	}
	return name + ".png"
}
