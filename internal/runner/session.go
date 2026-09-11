package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"vigil/internal/dsl"
	"vigil/internal/model"
)

// session is the mutable state of one run.
type session struct {
	spec  Spec
	steps []flatStep
	res   *Result
	cap   *capture
	red   *redactor

	runCtx  context.Context // caller ctx bounded by RunTimeout
	mainCtx context.Context // the run's own page target
	pageCtx context.Context // current target (main page or popup)
	closers []func()        // LIFO: popup contexts, then the page cleanup

	navigated       bool
	layout          bool // browser reports real element boxes (Chromium); false = JS-only interactions
	layoutProbed    bool
	knownTargets    map[target.ID]bool
	stepRef         int
	urlBeforeAction string // page URL before the action preceding an expect_popup

	// bodyOf / textOf override the CDP paths assert_data uses (tests stub them).
	bodyOf func(ctx context.Context, id network.RequestID) ([]byte, error)
	textOf func(ctx context.Context, l *dsl.Locator, ref string) (string, *stepErr)
}

// stepErr is a step failure with its raw class (PRD §13 classification input).
type stepErr struct {
	class    FailureClass
	msg      string
	expected string
	actual   string
}

func (e *stepErr) Error() string { return e.msg }

func fail(class FailureClass, format string, a ...any) *stepErr {
	return &stepErr{class: class, msg: fmt.Sprintf(format, a...)}
}

func (e *stepErr) with(expected, actual string) *stepErr {
	e.expected, e.actual = expected, actual
	return e
}

// execute runs the flattened steps until the first failure.
func (s *session) execute() {
	s.knownTargets = s.snapshotTargets()
	for i, fs := range s.steps {
		sr := StepResult{Index: i + 1, Kind: fs.Step.Kind(), Name: fs.Name}
		if i+1 < len(s.steps) && s.steps[i+1].Step.Kind() == "expect_popup" {
			s.urlBeforeAction = s.currentURL()
		}
		t0 := time.Now()
		err := s.runStep(fs.Step, &sr)
		sr.Duration = time.Since(t0)
		sr.Expected, sr.Actual = s.red.Redact(sr.Expected), s.red.Redact(sr.Actual)
		if err == nil {
			sr.OK = true
			s.res.Steps = append(s.res.Steps, sr)
			continue
		}
		sr.OK = false
		sr.Class = err.class
		sr.Error = s.red.Redact(err.msg)
		if err.expected != "" {
			sr.Expected = s.red.Redact(err.expected)
		}
		if err.actual != "" {
			sr.Actual = s.red.Redact(err.actual)
		}
		s.res.Steps = append(s.res.Steps, sr)
		s.res.FailedStep = &s.res.Steps[len(s.res.Steps)-1]
		s.res.Class = err.class
		s.res.Error = fmt.Sprintf("step %d (%s): %s", sr.Index, sr.Kind, sr.Error)
		return
	}
}

// runStep executes one step under its timeout, converting panics to internal failures.
func (s *session) runStep(st dsl.Step, sr *StepResult) (serr *stepErr) {
	defer func() {
		if r := recover(); r != nil {
			serr = fail(FailInternal, "runner panic in %s: %v\n%s", st.Kind(), r, debug.Stack())
		}
	}()
	if s.runCtx.Err() != nil {
		return fail(FailTimeout, "run timeout (%s) reached before step started", s.spec.RunTimeout)
	}
	timeout := s.stepTimeoutFor(st)
	if timeout < 0 {
		return fail(FailInternal, "invalid timeout on %s step", st.Kind())
	}
	ctx, cancel := context.WithTimeout(s.pageCtx, timeout)
	defer cancel()
	stop := context.AfterFunc(s.runCtx, cancel)
	defer stop()
	if st.Kind() != "expect_popup" {
		s.cap.drainCreated()
	}
	s.stepRef++
	return s.exec(ctx, st, sr, fmt.Sprintf("s%d", s.stepRef))
}

// stepTimeoutFor applies locator/assert level overrides over Spec.StepTimeout.
func (s *session) stepTimeoutFor(st dsl.Step) time.Duration {
	var raw string
	switch st.Kind() {
	case "click":
		raw = st.Click.Timeout
	case "fill":
		raw = st.Fill.Timeout
	case "type":
		raw = st.Type.Timeout
	case "select":
		raw = st.Select.Timeout
	case "hover":
		raw = st.Hover.Timeout
	case "wait_for":
		raw = st.WaitFor.Timeout
	case "wait_url":
		raw = st.WaitURL.Timeout
	case "assert_visible":
		raw = st.AssertVisible.Timeout
	case "assert_not_visible":
		raw = st.AssertNotVisible.Timeout
	case "assert_url":
		raw = st.AssertURL.Timeout
	case "assert_count":
		raw = st.AssertCount.Timeout
	case "assert_request":
		raw = st.AssertRequest.Timeout
	case "assert_attr":
		raw = st.AssertAttr.Timeout
	case "assert_data":
		raw = st.AssertData.UI.Timeout
	case "expect_popup":
		raw = st.ExpectPopup.Timeout
	case "assert_text":
		if st.AssertText.In != nil {
			raw = st.AssertText.In.Timeout
		}
	case "assert_no_text":
		if st.AssertNoText.In != nil {
			raw = st.AssertNoText.In.Timeout
		}
	case "wait_ms":
		return time.Duration(st.WaitMs)*time.Millisecond + time.Second
	}
	if raw == "" {
		return s.spec.StepTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return -1
	}
	return d
}

// finish evaluates scenario-level asserts, resolves the final URL and sets Passed.
func (s *session) finish() {
	res := s.res
	if res.FailedStep == nil {
		s.globalAsserts()
	}
	if res.FailedStep == nil && len(res.GlobalAssertionFailures) == 0 {
		res.Passed = true
	}
	res.FinalURL = s.red.Redact(s.currentURL())
	res.Console = s.cap.snapshotConsole()
	res.Network = s.cap.snapshotNetwork()
	for i := range res.Console {
		res.Console[i].Text = s.red.Redact(res.Console[i].Text)
		res.Console[i].URL = s.red.Redact(res.Console[i].URL)
	}
	for i := range res.Network {
		res.Network[i].URL = s.red.Redact(res.Network[i].URL)
	}
	// Capabilities are only recorded when demonstrated or measured; absence of an
	// event is not evidence of missing support, except for the network domain
	// after a navigation (the document request itself must produce events).
	if s.cap.consoleSeen() {
		res.Capabilities["console"] = true
	}
	if s.navigated {
		res.Capabilities["network"] = s.cap.networkSeen()
	}
	if s.layoutProbed {
		res.Capabilities["layout"] = s.layout
	}
	s.recordBrowserCapabilities()
	res.FinishedAt = time.Now()
}

// recordBrowserCapabilities notes limits of the engine itself, independent of what this
// run happened to exercise. Only measured limits belong here: absence of an event during
// one run is not evidence that the browser cannot produce it.
func (s *session) recordBrowserCapabilities() {
	if s.spec.Browser != model.BrowserLightpanda {
		return
	}
	// Measured 2026-09-01 (nightly 9063): no Runtime.exceptionThrown for uncaught errors.
	s.res.Capabilities["exception_events"] = false
	// Lightpanda has no paint pipeline: it fetches neither stylesheets nor web fonts, so
	// Page.captureScreenshot returns an unstyled text rendering of the DOM whose glyphs
	// fall back to tofu wherever the default font has no coverage. The capture still
	// evidences what the DOM said; it does not evidence what a user would see.
	s.res.Capabilities["screenshot_painted"] = false
}

// globalAsserts evaluates assert.no_uncaught_console_error / no_http_5xx / no_http_4xx_on.
func (s *session) globalAsserts() {
	a := s.spec.Scenario.Assert
	res := s.res
	var failures []string
	if a.NoUncaughtConsoleError {
		var errs []string
		for _, ev := range s.cap.snapshotConsole() {
			if ev.Source == "console" && ev.Level == "error" || ev.Source == "exception" {
				errs = append(errs, ev.Level+": "+truncate(ev.Text, 200))
			}
		}
		if len(errs) > 0 {
			failures = append(failures, fmt.Sprintf("no_uncaught_console_error: %d console error(s); first: %s", len(errs), errs[0]))
		}
		if s.spec.Browser == model.BrowserLightpanda {
			// Measured 2026-09-01 (nightly 9063): Lightpanda emits Runtime.consoleAPICalled
			// but never Runtime.exceptionThrown for uncaught errors or unhandled rejections.
			s.note("no_uncaught_console_error on lightpanda covers console.error only: Runtime.exceptionThrown is not emitted by this browser (measured)")
		}
	}
	needNetwork := a.NoHTTP5xx || len(a.NoHTTP4xxOn) > 0
	if needNetwork && s.navigated && !s.cap.networkSeen() {
		res.Class = FailBrowserProtocol
		res.Error = fmt.Sprintf("network events unsupported on %s: no Network.* events were observed after navigation, so no_http_5xx/no_http_4xx_on cannot be evaluated", s.spec.Browser)
		res.Capabilities["network"] = false
		return
	}
	if needNetwork {
		// Requests whose response was never reported have an unknown status; say so
		// instead of silently treating them as healthy (partial oracle, not a fake pass).
		var pending []string
		for _, ev := range s.cap.snapshotNetwork() {
			if ev.Status == 0 && !ev.Failed {
				pending = append(pending, ev.Method+" "+truncate(ev.URL, 120))
			}
		}
		if len(pending) > 0 {
			res.Capabilities["response_status_complete"] = false
			s.note("no_http_5xx/no_http_4xx_on on %s: %d request(s) had no observed response, their status is unknown: %s", s.spec.Browser, len(pending), strings.Join(pending, "; "))
		}
	}
	if a.NoHTTP5xx {
		var hits []string
		for _, ev := range s.cap.snapshotNetwork() {
			if ev.Status >= 500 {
				hits = append(hits, fmt.Sprintf("%d %s %s", ev.Status, ev.Method, ev.URL))
			}
		}
		if len(hits) > 0 {
			failures = append(failures, fmt.Sprintf("no_http_5xx: %d response(s) >= 500; first: %s", len(hits), truncate(hits[0], 300)))
		}
	}
	for _, sub := range a.NoHTTP4xxOn {
		for _, ev := range s.cap.snapshotNetwork() {
			if ev.Status >= 400 && ev.Status < 500 && strings.Contains(ev.URL, sub) {
				failures = append(failures, fmt.Sprintf("no_http_4xx_on[%s]: %d %s %s", sub, ev.Status, ev.Method, truncate(ev.URL, 300)))
				break
			}
		}
	}
	if len(failures) > 0 {
		for i := range failures {
			failures[i] = s.red.Redact(failures[i])
		}
		res.GlobalAssertionFailures = failures
		res.Class = FailAssertion
		res.Error = "scenario assert failed: " + strings.Join(failures, "; ")
	}
}

func (s *session) note(format string, a ...any) {
	s.res.Notes = append(s.res.Notes, s.red.Redact(fmt.Sprintf(format, a...)))
}

func (s *session) closeAll() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

// currentURL reads location.href from the current target with a short grace timeout.
func (s *session) currentURL() string {
	ctx, cancel := context.WithTimeout(s.pageCtx, 3*time.Second)
	defer cancel()
	var href string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`String(location.href)`, &href)); err != nil {
		return ""
	}
	return href
}

func (s *session) snapshotTargets() map[target.ID]bool {
	ctx, cancel := context.WithTimeout(s.pageCtx, 3*time.Second)
	defer cancel()
	out := map[target.ID]bool{}
	infos, err := chromedp.Targets(ctx)
	if err != nil {
		return out
	}
	for _, t := range infos {
		out[t.TargetID] = true
	}
	return out
}

// resolveURL joins a relative goto path with BaseURL.
func resolveURL(base, ref string) (string, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref, nil
	}
	if base == "" {
		return "", fmt.Errorf("relative goto %q without BaseURL", ref)
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("BaseURL %q: %w", base, err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("goto %q: %w", ref, err)
	}
	return b.ResolveReference(r).String(), nil
}

// checkAllowlist rejects an absolute URL whose host is outside spec.AllowedHosts.
// An empty allowlist means unrestricted (legacy specs).
func (s *session) checkAllowlist(u string) *stepErr {
	if !urlAllowed(s.spec.AllowedHosts, u) {
		return fail(FailEnvironment, "goto %s: %s (%s)", u, ErrOutsideAllowlist, s.spec.Environment).with("host in "+strings.Join(s.spec.AllowedHosts, ","), u)
	}
	return nil
}

func urlAllowed(allowed []string, raw string) bool {
	if len(allowed) == 0 {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range allowed {
		h = strings.ToLower(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// classifyCDPErr maps an error from chromedp into a failure class, using def
// for ordinary step failures (e.g. locator timeout).
func (s *session) classifyCDPErr(err error, def FailureClass, what string) *stepErr {
	if err == nil {
		return nil
	}
	if s.runCtx.Err() != nil {
		return fail(FailTimeout, "%s: run timeout (%s) exceeded", what, s.spec.RunTimeout)
	}
	if s.pageCtx.Err() != nil {
		return fail(FailBrowserProtocol, "%s: browser connection lost: %v", what, s.pageCtx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fail(def, "%s: timed out", what)
	}
	var cdpErr *cdproto.Error
	if errors.As(err, &cdpErr) && cdpErr.Code == -32601 {
		return fail(FailBrowserProtocol, "%s: unsupported CDP method on %s: %s", what, s.spec.Browser, cdpErr.Message)
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "page load error"):
		if isTransportError(lower) {
			return fail(FailTransport, "%s: %s", what, msg)
		}
		return fail(FailNavigation, "%s: %s", what, msg)
	case strings.Contains(lower, "websocket") || strings.Contains(lower, "connection lost") || strings.Contains(lower, "eof") || strings.Contains(lower, "invalid target") || strings.Contains(lower, "browser closed"):
		return fail(FailBrowserProtocol, "%s: browser protocol error: %s", what, msg)
	}
	return fail(def, "%s: %s", what, msg)
}

func isTransportError(lower string) bool {
	for _, k := range []string{"name_not_resolved", "connection_refused", "connection_reset", "connection_closed", "connection_timed_out", "address_unreachable", "internet_disconnected", "cert_", "ssl", "tls", "proxy", "timed_out", "dns", "resolve", "refused"} {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
