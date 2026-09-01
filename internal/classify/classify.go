// Package classify maps raw runner results to PRD §13 outcomes.
package classify

import (
	"fmt"
	"strings"

	"vigil/internal/model"
	"vigil/internal/runner"
)

// Input bundles what the classifier needs for one attempt.
type Input struct {
	Result        *runner.Result
	Browser       model.Browser
	Attempt       int            // 1-based
	RetriedOnce   bool           // a cheap safe retry already happened and this is its result
	PrevFailed    bool           // previous attempt failed (so a PASS now means QA_FLAKE)
	ChromiumSaid  *runner.Result // optional Chromium confirmation of the same script
	TargetHealthy bool           // deployment gate / base URL reachable at classification time
}

// Classify returns the outcome and a short human reason.
//
// Table (PRD §13 failure path; a Lightpanda failure is never automatically an
// application defect):
//
//	Passed && PrevFailed                       → QA_FLAKE
//	Passed                                     → PASS
//	assertion, chromium                        → APP_FAILURE
//	assertion, lightpanda, Chromium passed     → LIGHTPANDA_INCOMPATIBLE
//	assertion, lightpanda, Chromium failed     → APP_FAILURE
//	assertion, lightpanda, no Chromium result  → BROWSER_AMBIGUOUS when the failed step
//	                                             depends on popup/network/screenshot/eval
//	                                             capabilities or the failure text mentions an
//	                                             unsupported CDP method; else APP_FAILURE
//	locator | navigation                       → SCRIPT_DRIFT (ENV_FAILURE when !TargetHealthy)
//	popup | browser_protocol, lightpanda       → LIGHTPANDA_INCOMPATIBLE
//	popup, chromium                            → SCRIPT_DRIFT
//	browser_protocol, chromium                 → ENV_FAILURE
//	transport                                  → ENV_FAILURE
//	auth                                       → AUTH_FAILURE
//	timeout                                    → ENV_FAILURE when !TargetHealthy, else
//	                                             BROWSER_AMBIGUOUS on lightpanda, SCRIPT_DRIFT on chromium
//	internal | unknown                         → NEEDS_REVIEW
func Classify(in Input) (model.Outcome, string) {
	res := in.Result
	if res == nil {
		return model.OutcomeNeedsReview, "no runner result"
	}
	browser := in.Browser
	if browser == "" {
		browser = res.Browser
	}
	onLightpanda := browser == model.BrowserLightpanda

	if res.Passed {
		if in.PrevFailed {
			return model.OutcomeQAFlake, "passed on retry after a failed attempt"
		}
		return model.OutcomePass, ""
	}

	where := failureLocation(res)
	switch res.Class {
	case runner.FailAssertion:
		if !onLightpanda {
			return model.OutcomeAppFailure, "business assertion failed on chromium: " + where
		}
		if in.ChromiumSaid != nil {
			if in.ChromiumSaid.Passed {
				return model.OutcomeLightpandaIncompatible, "assertion failed on lightpanda but the same script passed on chromium: " + where
			}
			return model.OutcomeAppFailure, "assertion failed on lightpanda and chromium confirmed the failure: " + where
		}
		if reason, ambiguous := capabilitySensitive(res); ambiguous {
			return model.OutcomeBrowserAmbiguous, "assertion failed on lightpanda and " + reason + "; chromium confirmation required: " + where
		}
		return model.OutcomeAppFailure, "business assertion failed on lightpanda without browser-capability involvement: " + where

	case runner.FailLocator, runner.FailNavigation:
		if !in.TargetHealthy {
			return model.OutcomeEnvFailure, fmt.Sprintf("%s failure while the target is unhealthy: %s", res.Class, where)
		}
		return model.OutcomeScriptDrift, fmt.Sprintf("%s failure on a healthy target: %s", res.Class, where)

	case runner.FailPopup:
		if onLightpanda {
			return model.OutcomeLightpandaIncompatible, "popup not observable on lightpanda: " + where
		}
		return model.OutcomeScriptDrift, "expected popup did not appear on chromium: " + where

	case runner.FailBrowserProtocol:
		if onLightpanda {
			return model.OutcomeLightpandaIncompatible, "browser protocol failure on lightpanda: " + where
		}
		return model.OutcomeEnvFailure, "browser protocol failure on chromium: " + where

	case runner.FailTransport:
		return model.OutcomeEnvFailure, "transport failure: " + where

	case runner.FailAuth:
		return model.OutcomeAuthFailure, "authentication failure: " + where

	case runner.FailTimeout:
		if !in.TargetHealthy {
			return model.OutcomeEnvFailure, "timeout while the target is unhealthy: " + where
		}
		if onLightpanda {
			return model.OutcomeBrowserAmbiguous, "timeout on lightpanda with a healthy target; chromium confirmation required: " + where
		}
		return model.OutcomeScriptDrift, "timeout on chromium with a healthy target: " + where

	case runner.FailInternal:
		return model.OutcomeNeedsReview, "runner internal failure: " + where
	}
	return model.OutcomeNeedsReview, fmt.Sprintf("unclassified failure (class %q): %s", res.Class, where)
}

// capabilitySensitive reports whether a Lightpanda assertion failure touches a
// browser capability that Lightpanda may lack, so Chromium must confirm first.
func capabilitySensitive(res *runner.Result) (string, bool) {
	if st := res.FailedStep; st != nil {
		switch st.Kind {
		case "expect_popup", "assert_request", "screenshot", "eval":
			return "the failed step (" + st.Kind + ") depends on popup/network/screenshot/eval support", true
		}
	}
	text := strings.ToLower(res.Error)
	if st := res.FailedStep; st != nil {
		text += " " + strings.ToLower(st.Error)
	}
	for _, f := range res.GlobalAssertionFailures {
		lf := strings.ToLower(f)
		text += " " + lf
		if strings.HasPrefix(lf, "no_uncaught_console_error") {
			return "the failed scenario assert (no_uncaught_console_error) depends on the browser's JS runtime", true
		}
	}
	for _, k := range []string{"unsupported cdp", "wasn't found", "method not found", "-32601", "not supported", "unsupported on lightpanda"} {
		if strings.Contains(text, k) {
			return "the failure mentions an unsupported CDP capability", true
		}
	}
	return "", false
}

func failureLocation(res *runner.Result) string {
	if st := res.FailedStep; st != nil {
		msg := st.Error
		if msg == "" {
			msg = res.Error
		}
		return fmt.Sprintf("step %d %s: %s", st.Index, st.Kind, msg)
	}
	if len(res.GlobalAssertionFailures) > 0 {
		return strings.Join(res.GlobalAssertionFailures, "; ")
	}
	if res.Error != "" {
		return res.Error
	}
	return "no details"
}

// IsEnvironmentWide reports whether a set of outcomes from unrelated scenarios
// indicates one environment incident rather than many feature incidents.
func IsEnvironmentWide(outcomes []model.Outcome) bool {
	if len(outcomes) < 2 {
		return false
	}
	infra := 0
	for _, o := range outcomes {
		if o.IsInfrastructure() {
			infra++
		}
	}
	return infra*2 >= len(outcomes)
}
