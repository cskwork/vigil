package classify

import (
	"testing"

	"vigil/internal/model"
	"vigil/internal/runner"
)

func failed(class runner.FailureClass, browser model.Browser, stepKind, errText string) *runner.Result {
	r := &runner.Result{Browser: browser, Class: class, Error: errText}
	if stepKind != "" {
		r.Steps = []runner.StepResult{{Index: 3, Kind: stepKind, Error: errText, Class: class}}
		r.FailedStep = &r.Steps[0]
	}
	return r
}

func TestClassifyTable(t *testing.T) {
	lp, ch := model.BrowserLightpanda, model.BrowserChromium
	passed := &runner.Result{Passed: true}
	cases := []struct {
		name string
		in   Input
		want model.Outcome
	}{
		{"nil result", Input{}, model.OutcomeNeedsReview},
		{"pass", Input{Result: passed, Browser: lp, TargetHealthy: true}, model.OutcomePass},
		{"pass after failed attempt is flake", Input{Result: passed, Browser: lp, PrevFailed: true, TargetHealthy: true}, model.OutcomeQAFlake},

		{"assertion on chromium", Input{Result: failed(runner.FailAssertion, ch, "assert_text", "text missing"), Browser: ch, TargetHealthy: true}, model.OutcomeAppFailure},
		{"assertion on lightpanda, chromium passed", Input{Result: failed(runner.FailAssertion, lp, "assert_text", "text missing"), Browser: lp, ChromiumSaid: passed, TargetHealthy: true}, model.OutcomeLightpandaIncompatible},
		{"assertion on lightpanda, chromium failed too", Input{Result: failed(runner.FailAssertion, lp, "assert_text", "text missing"), Browser: lp, ChromiumSaid: failed(runner.FailAssertion, ch, "assert_text", "text missing"), TargetHealthy: true}, model.OutcomeAppFailure},
		{"assertion on lightpanda, plain text step, no chromium", Input{Result: failed(runner.FailAssertion, lp, "assert_text", "text missing"), Browser: lp, TargetHealthy: true}, model.OutcomeAppFailure},
		{"assertion on lightpanda, eval step, no chromium", Input{Result: failed(runner.FailAssertion, lp, "eval", "expected 1 got 2"), Browser: lp, TargetHealthy: true}, model.OutcomeBrowserAmbiguous},
		{"assertion on lightpanda, assert_request step", Input{Result: failed(runner.FailAssertion, lp, "assert_request", "not observed"), Browser: lp, TargetHealthy: true}, model.OutcomeBrowserAmbiguous},
		{"assertion on lightpanda mentioning unsupported CDP", Input{Result: failed(runner.FailAssertion, lp, "assert_text", "unsupported CDP method on lightpanda: 'DOM.focus' wasn't found"), Browser: lp, TargetHealthy: true}, model.OutcomeBrowserAmbiguous},
		{"global console assert on lightpanda", Input{Result: &runner.Result{Browser: lp, Class: runner.FailAssertion, GlobalAssertionFailures: []string{"no_uncaught_console_error: 1 console error(s)"}}, Browser: lp, TargetHealthy: true}, model.OutcomeBrowserAmbiguous},
		{"global 5xx assert on lightpanda", Input{Result: &runner.Result{Browser: lp, Class: runner.FailAssertion, GlobalAssertionFailures: []string{"no_http_5xx: 1 response(s) >= 500"}}, Browser: lp, TargetHealthy: true}, model.OutcomeAppFailure},

		{"locator healthy", Input{Result: failed(runner.FailLocator, lp, "click", "0 matches"), Browser: lp, TargetHealthy: true}, model.OutcomeScriptDrift},
		{"locator unhealthy", Input{Result: failed(runner.FailLocator, lp, "click", "0 matches"), Browser: lp, TargetHealthy: false}, model.OutcomeEnvFailure},
		{"navigation healthy", Input{Result: failed(runner.FailNavigation, ch, "goto", "page load error"), Browser: ch, TargetHealthy: true}, model.OutcomeScriptDrift},
		{"navigation unhealthy", Input{Result: failed(runner.FailNavigation, ch, "goto", "page load error"), Browser: ch, TargetHealthy: false}, model.OutcomeEnvFailure},

		{"popup on lightpanda", Input{Result: failed(runner.FailPopup, lp, "expect_popup", "no new page target"), Browser: lp, TargetHealthy: true}, model.OutcomeLightpandaIncompatible},
		{"popup on chromium", Input{Result: failed(runner.FailPopup, ch, "expect_popup", "no new page target"), Browser: ch, TargetHealthy: true}, model.OutcomeScriptDrift},
		{"protocol on lightpanda", Input{Result: failed(runner.FailBrowserProtocol, lp, "fill", "'DOM.focus' wasn't found (-32601)"), Browser: lp, TargetHealthy: true}, model.OutcomeLightpandaIncompatible},
		{"protocol on chromium", Input{Result: failed(runner.FailBrowserProtocol, ch, "fill", "websocket closed"), Browser: ch, TargetHealthy: true}, model.OutcomeEnvFailure},

		{"transport", Input{Result: failed(runner.FailTransport, lp, "goto", "net::ERR_NAME_NOT_RESOLVED"), Browser: lp, TargetHealthy: false}, model.OutcomeEnvFailure},
		{"goto outside env allowlist", Input{Result: failed(runner.FailEnvironment, lp, "goto", runner.ErrOutsideAllowlist), Browser: lp, TargetHealthy: true}, model.OutcomeEnvFailure},
		{"auth", Input{Result: failed(runner.FailAuth, lp, "goto", "HTTP 401"), Browser: lp, TargetHealthy: true}, model.OutcomeAuthFailure},

		{"timeout unhealthy", Input{Result: failed(runner.FailTimeout, lp, "wait_for", "run timeout"), Browser: lp, TargetHealthy: false}, model.OutcomeEnvFailure},
		{"timeout healthy lightpanda", Input{Result: failed(runner.FailTimeout, lp, "wait_for", "run timeout"), Browser: lp, TargetHealthy: true}, model.OutcomeBrowserAmbiguous},
		{"timeout healthy chromium", Input{Result: failed(runner.FailTimeout, ch, "wait_for", "run timeout"), Browser: ch, TargetHealthy: true}, model.OutcomeScriptDrift},

		{"internal", Input{Result: failed(runner.FailInternal, lp, "click", "panic"), Browser: lp, TargetHealthy: true}, model.OutcomeNeedsReview},
		{"unknown class", Input{Result: &runner.Result{Browser: lp, Class: "weird"}, Browser: lp, TargetHealthy: true}, model.OutcomeNeedsReview},
		{"browser taken from result when input browser empty", Input{Result: failed(runner.FailPopup, lp, "expect_popup", "x"), TargetHealthy: true}, model.OutcomeLightpandaIncompatible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Classify(tc.in)
			if got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, reason, tc.want)
			}
			if got != model.OutcomePass && reason == "" {
				t.Fatalf("non-PASS outcome %s must carry a reason", got)
			}
		})
	}
}

func TestIsEnvironmentWide(t *testing.T) {
	if IsEnvironmentWide([]model.Outcome{model.OutcomeEnvFailure}) {
		t.Fatal("a single outcome is never environment-wide")
	}
	if !IsEnvironmentWide([]model.Outcome{model.OutcomeEnvFailure, model.OutcomeAuthFailure, model.OutcomeAppFailure}) {
		t.Fatal("2 of 3 infrastructure outcomes should be environment-wide")
	}
	if IsEnvironmentWide([]model.Outcome{model.OutcomeEnvFailure, model.OutcomeAppFailure, model.OutcomeScriptDrift}) {
		t.Fatal("1 of 3 should not be environment-wide")
	}
}

// A wait_url that fails on Lightpanda is not evidence of a stale script: that engine cannot
// see a window.open target, so it reports the same thing whether the app navigated to a new
// tab or did nothing at all. Chromium must answer that before an agent repairs anything.
func TestNavigationFailureOnLightpandaAsksChromium(t *testing.T) {
	res := func(class runner.FailureClass) *runner.Result {
		return &runner.Result{Class: class, Browser: model.BrowserLightpanda,
			FailedStep: &runner.StepResult{Kind: "wait_url"}}
	}
	got, why := Classify(Input{Result: res(runner.FailNavigation), TargetHealthy: true})
	if got != model.OutcomeBrowserAmbiguous {
		t.Fatalf("lightpanda navigation failure = %s (%s), want BROWSER_AMBIGUOUS", got, why)
	}

	// A locator failure is not popup-shaped; it stays a script problem.
	if got, _ := Classify(Input{Result: res(runner.FailLocator), TargetHealthy: true}); got != model.OutcomeScriptDrift {
		t.Fatalf("lightpanda locator failure = %s, want SCRIPT_DRIFT", got)
	}

	// On Chromium the observation is trustworthy, so the script really is stale.
	ch := res(runner.FailNavigation)
	ch.Browser = model.BrowserChromium
	if got, _ := Classify(Input{Result: ch, TargetHealthy: true}); got != model.OutcomeScriptDrift {
		t.Fatalf("chromium navigation failure = %s, want SCRIPT_DRIFT", got)
	}
}
