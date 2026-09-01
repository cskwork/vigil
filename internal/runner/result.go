package runner

import (
	"time"

	"vigil/internal/dsl"
	"vigil/internal/model"
)

// Spec is one execution request for the deterministic QA runner.
type Spec struct {
	ProjectID   string
	Scenario    *dsl.Scenario
	Flows       map[string]*dsl.Flow
	Browser     model.Browser
	BaseURL     string
	Persona     map[string]string // username/password/extra (never logged)
	EvidenceDir string            // run artifacts root for this attempt
	StepTimeout time.Duration
	RunTimeout  time.Duration
	// CaptureDOMOnFail writes dom.html on failure (default true).
	// The zero value means "default": dom.html is always captured on failure.
	CaptureDOMOnFail bool
}

// StepResult is the outcome of a single executed step.
type StepResult struct {
	Index    int           `json:"index"` // 1-based, over the flattened (flow-expanded) step list
	Kind     string        `json:"kind"`
	Name     string        `json:"name,omitempty"`
	OK       bool          `json:"ok"`
	Expected string        `json:"expected,omitempty"`
	Actual   string        `json:"actual,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns"`
	// Class is the runner's failure class for this step (empty when OK).
	Class FailureClass `json:"class,omitempty"`
}

// NetworkEvent is a compact request/response record.
type NetworkEvent struct {
	Method   string `json:"method"`
	URL      string `json:"url"`
	Status   int    `json:"status"`
	MimeType string `json:"mime_type,omitempty"`
	Failed   bool   `json:"failed"`
	Error    string `json:"error,omitempty"`
	At       int64  `json:"at_ms"`
}

// ConsoleEvent is a console/log or uncaught exception record.
type ConsoleEvent struct {
	Level  string `json:"level"`            // warn|error|exception
	Source string `json:"source,omitempty"` // console | exception | log:<browser log source, e.g. network>
	Text   string `json:"text"`
	URL    string `json:"url,omitempty"`
	Line   int    `json:"line,omitempty"`
	At     int64  `json:"at_ms"`
}

// FailureClass is the runner's raw, un-retried classification hint for classify.Classify.
type FailureClass string

const (
	FailNone            FailureClass = ""
	FailAssertion       FailureClass = "assertion"        // business oracle failed
	FailLocator         FailureClass = "locator"          // element not found / not visible in time
	FailNavigation      FailureClass = "navigation"       // goto / wait_url failed
	FailPopup           FailureClass = "popup"            // expect_popup did not observe a new page
	FailBrowserProtocol FailureClass = "browser_protocol" // CDP error, unsupported method, browser crash
	FailTimeout         FailureClass = "timeout"          // run timeout
	FailTransport       FailureClass = "transport"        // DNS/TLS/connection refused/gateway
	FailAuth            FailureClass = "auth"             // 401/403 on required API, redirected to login
	FailInternal        FailureClass = "internal"         // runner bug
)

// Result is the complete outcome of one attempt.
type Result struct {
	Passed         bool
	Class          FailureClass
	Browser        model.Browser
	Steps          []StepResult
	FailedStep     *StepResult
	Console        []ConsoleEvent
	Network        []NetworkEvent
	FinalURL       string
	StartedAt      time.Time
	FinishedAt     time.Time
	DOMPath        string // dom.html when captured
	ScreenshotPath string
	Artifacts      map[string]string // kind -> path (console.json, network.json, steps.json)
	Error          string            // top-level error text
	// GlobalAssertionFailures lists failed scenario-level asserts (no_http_5xx, ...).
	GlobalAssertionFailures []string
	// Capabilities records what the browser demonstrably supported during this run
	// (console, network, popup, screenshot, layout, exception_events, ...).
	Capabilities map[string]bool
	// Notes lists graceful degradations applied during the run (never secrets).
	Notes []string
}

func (r *Result) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }
