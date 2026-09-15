// Package dsl defines the deterministic QA script DSL (PRD §10), its validator and
// the logical-scenario fingerprint used for dedup (PRD §9).
package dsl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scenario is one deterministic QA script.
type Scenario struct {
	Scenario struct {
		ID       string `yaml:"id"`
		Version  int    `yaml:"version"`
		Title    string `yaml:"title,omitempty"`
		Class    string `yaml:"class,omitempty"`    // P0 | P1 | P2 (default P1)
		Mutation string `yaml:"mutation,omitempty"` // read-only | reversible | destructive
	} `yaml:"scenario"`

	Covers struct {
		Feature    string   `yaml:"feature,omitempty"`
		Capability string   `yaml:"capability,omitempty"`
		Routes     []string `yaml:"routes,omitempty"`
		APIs       []string `yaml:"apis,omitempty"`
		Paths      []string `yaml:"paths,omitempty"`
	} `yaml:"covers"`

	Uses []string `yaml:"uses,omitempty"` // reusable flow ids

	Preconditions struct {
		Persona string `yaml:"persona,omitempty"`
	} `yaml:"preconditions,omitempty"`

	Resources struct {
		Locks []string `yaml:"locks,omitempty"`
	} `yaml:"resources,omitempty"`

	Browser struct {
		Primary          string `yaml:"primary,omitempty"`           // lightpanda | chromium
		RequiresChromium bool   `yaml:"requires_chromium,omitempty"` // rendering/layout is part of the oracle
		Popup            bool   `yaml:"popup,omitempty"`             // scenario opens new pages
	} `yaml:"browser,omitempty"`

	Steps []Step `yaml:"steps"`

	Assert struct {
		NoUncaughtConsoleError bool     `yaml:"no_uncaught_console_error,omitempty"`
		NoHTTP5xx              bool     `yaml:"no_http_5xx,omitempty"`
		NoHTTP4xxOn            []string `yaml:"no_http_4xx_on,omitempty"` // URL substrings
	} `yaml:"assert,omitempty"`

	Oracle Oracle `yaml:"oracle"`
}

// Oracle provenance (PRD §5.1). Observation alone must not become the oracle.
type Oracle struct {
	Source        string `yaml:"source"` // spec | approved_qa | contract | observation
	SourceFeature string `yaml:"source_feature,omitempty"`
	SourceSHA     string `yaml:"source_sha,omitempty"`
	Note          string `yaml:"note,omitempty"`
}

// Flow is a reusable step sequence (login-as-student, select-course, ...).
type Flow struct {
	Flow struct {
		ID      string `yaml:"id"`
		Version int    `yaml:"version"`
		Title   string `yaml:"title,omitempty"`
	} `yaml:"flow"`
	Steps []Step `yaml:"steps"`
}

// Step is a tagged union: exactly one action field must be set.
type Step struct {
	Name string `yaml:"name,omitempty"`

	Goto             string         `yaml:"goto,omitempty"`
	Click            *Locator       `yaml:"click,omitempty"`
	Fill             *FillArgs      `yaml:"fill,omitempty"`
	Type             *FillArgs      `yaml:"type,omitempty"`
	Press            string         `yaml:"press,omitempty"`
	Select           *SelectArgs    `yaml:"select,omitempty"`
	Hover            *Locator       `yaml:"hover,omitempty"`
	WaitFor          *Locator       `yaml:"wait_for,omitempty"`
	WaitMs           int            `yaml:"wait_ms,omitempty"`
	WaitURL          *URLAssert     `yaml:"wait_url,omitempty"`
	AssertText       *TextAssert    `yaml:"assert_text,omitempty"`
	AssertNoText     *TextAssert    `yaml:"assert_no_text,omitempty"`
	AssertVisible    *Locator       `yaml:"assert_visible,omitempty"`
	AssertNotVisible *Locator       `yaml:"assert_not_visible,omitempty"`
	AssertURL        *URLAssert     `yaml:"assert_url,omitempty"`
	AssertCount      *CountAssert   `yaml:"assert_count,omitempty"`
	AssertRequest    *RequestAssert `yaml:"assert_request,omitempty"`
	AssertAttr       *AttrAssert    `yaml:"assert_attr,omitempty"`
	AssertData       *DataAssert    `yaml:"assert_data,omitempty"`
	ExpectPopup      *PopupArgs     `yaml:"expect_popup,omitempty"`
	Eval             *EvalArgs      `yaml:"eval,omitempty"`
	UseFlow          string         `yaml:"use_flow,omitempty"`
	Screenshot       string         `yaml:"screenshot,omitempty"`
}

// Locator follows the PRD §10 locator order. `by` selects the strategy.
type Locator struct {
	By      string `yaml:"by,omitempty"` // test_id | role | label | id | text | href | css
	Role    string `yaml:"role,omitempty"`
	Name    string `yaml:"name,omitempty"`  // accessible name (role) / label text
	Value   string `yaml:"value,omitempty"` // test id / id / href / css selector
	Text    string `yaml:"text,omitempty"`  // semantic text (substring)
	Exact   bool   `yaml:"exact,omitempty"`
	Nth     int    `yaml:"nth,omitempty"`     // 0-based
	Timeout string `yaml:"timeout,omitempty"` // e.g. 10s
}

type FillArgs struct {
	Locator `yaml:",inline"`
	Input   string `yaml:"input"`
}

type SelectArgs struct {
	Locator `yaml:",inline"`
	Option  string `yaml:"option"`
}

type TextAssert struct {
	Value string   `yaml:"value"`
	In    *Locator `yaml:"in,omitempty"` // default: document body
	Exact bool     `yaml:"exact,omitempty"`
}

type URLAssert struct {
	Contains string `yaml:"contains,omitempty"`
	Matches  string `yaml:"matches,omitempty"` // regexp
	Timeout  string `yaml:"timeout,omitempty"`
}

type CountAssert struct {
	Locator `yaml:",inline"`
	Equals  *int `yaml:"equals,omitempty"`
	Min     *int `yaml:"min,omitempty"`
	Max     *int `yaml:"max,omitempty"`
}

type RequestAssert struct {
	URLContains  string `yaml:"url_contains"`
	Method       string `yaml:"method,omitempty"`
	Status       int    `yaml:"status,omitempty"`
	StatusMin    int    `yaml:"status_min,omitempty"`
	StatusMax    int    `yaml:"status_max,omitempty"`
	BodyContains string `yaml:"body_contains,omitempty"`
	Timeout      string `yaml:"timeout,omitempty"`
}

type AttrAssert struct {
	Locator  `yaml:",inline"`
	Attr     string `yaml:"attr"`
	Equals   string `yaml:"equals,omitempty"`
	Contains string `yaml:"contains,omitempty"`
}

// DataAssert compares a displayed value with the API payload that produced it
// (data-analyst check): innerText of UI (after UIRegex when set) vs. the value at
// API.JSONPath in the last captured response matching API.
type DataAssert struct {
	UI      Locator `yaml:"ui"`
	UIRegex string  `yaml:"ui_regex,omitempty"` // first match (group 1 when present) is compared
	API     DataAPI `yaml:"api"`
	Compare string  `yaml:"compare"` // text (trimmed) | number | contains (UI contains API value)
}

// DataAPI selects the captured response and the value inside its JSON body.
type DataAPI struct {
	URLContains string `yaml:"url_contains"`
	JSONPath    string `yaml:"json_path"` // $, .key, [N], [*] (first element), ["key with spaces"]
	Method      string `yaml:"method,omitempty"`
}

// DataCompareModes are the accepted assert_data.compare values.
var DataCompareModes = map[string]bool{"text": true, "number": true, "contains": true}

// PopupArgs waits for a new page/target opened by the previous action and
// switches the run context to it.
type PopupArgs struct {
	URLContains string `yaml:"url_contains,omitempty"`
	Timeout     string `yaml:"timeout,omitempty"`
}

type EvalArgs struct {
	Script string `yaml:"script"`
	Expect string `yaml:"expect,omitempty"` // JSON-encoded expected value; empty = just run
}

// Kind returns the step's action name (first non-empty field) or "".
func (s Step) Kind() string {
	switch {
	case s.Goto != "":
		return "goto"
	case s.Click != nil:
		return "click"
	case s.Fill != nil:
		return "fill"
	case s.Type != nil:
		return "type"
	case s.Press != "":
		return "press"
	case s.Select != nil:
		return "select"
	case s.Hover != nil:
		return "hover"
	case s.WaitFor != nil:
		return "wait_for"
	case s.WaitMs != 0:
		return "wait_ms"
	case s.WaitURL != nil:
		return "wait_url"
	case s.AssertText != nil:
		return "assert_text"
	case s.AssertNoText != nil:
		return "assert_no_text"
	case s.AssertVisible != nil:
		return "assert_visible"
	case s.AssertNotVisible != nil:
		return "assert_not_visible"
	case s.AssertURL != nil:
		return "assert_url"
	case s.AssertCount != nil:
		return "assert_count"
	case s.AssertRequest != nil:
		return "assert_request"
	case s.AssertAttr != nil:
		return "assert_attr"
	case s.AssertData != nil:
		return "assert_data"
	case s.ExpectPopup != nil:
		return "expect_popup"
	case s.Eval != nil:
		return "eval"
	case s.UseFlow != "":
		return "use_flow"
	case s.Screenshot != "":
		return "screenshot"
	}
	return ""
}

// IsAssertion reports whether the step is part of the business oracle.
func (s Step) IsAssertion() bool {
	return strings.HasPrefix(s.Kind(), "assert_")
}

// ---- load / dump --------------------------------------------------------

func Parse(data []byte) (*Scenario, error) {
	var sc Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("dsl parse: %w", err)
	}
	sc.Normalize()
	return &sc, nil
}

func ParseFile(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

func ParseFlow(data []byte) (*Flow, error) {
	var f Flow
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("flow parse: %w", err)
	}
	f.Normalize()
	return &f, nil
}

func ParseFlowFile(path string) (*Flow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := ParseFlow(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func (s *Scenario) Marshal() ([]byte, error) { return yaml.Marshal(s) }

// ---- normalization ---------------------------------------------------------
//
// Models (and humans) write locator kinds the DSL never defined: `by: link`,
// `by: button`, `by: placeholder`, `by: testid`, `by: selector`. Parse maps those
// aliases onto the canonical kinds before validation so files, agent candidates
// and repair patches all benefit; unknown kinds (xpath, ...) still fail Validate.

// byAliases maps a lower-cased `by` alias to its canonical kind. Role shorthands
// (link, button) also set Locator.Role.
var byAliases = map[string]string{
	"link":        "role",
	"button":      "role",
	"placeholder": "label",
	"testid":      "test_id",
	"data-testid": "test_id",
	"test-id":     "test_id",
	"selector":    "css",
}

// roleShorthands are `by` values that name the role itself.
var roleShorthands = map[string]bool{"link": true, "button": true}

// Normalize rewrites locator aliases in place (see byAliases). Idempotent.
func (l *Locator) Normalize() {
	if l == nil {
		return
	}
	by := strings.ToLower(strings.TrimSpace(l.By))
	if roleShorthands[by] {
		if l.Role == "" {
			l.Role = by
		}
		if l.Name == "" && l.Text != "" {
			l.Name = l.Text // role locators match on accessible name
		}
	}
	if canon, ok := byAliases[by]; ok {
		by = canon
	}
	l.By = by
}

// Normalize applies Locator.Normalize to every locator of the scenario.
func (s *Scenario) Normalize() {
	if s == nil {
		return
	}
	normalizeSteps(s.Steps)
}

// Normalize applies Locator.Normalize to every locator of the flow.
func (f *Flow) Normalize() {
	if f == nil {
		return
	}
	normalizeSteps(f.Steps)
}

func normalizeSteps(steps []Step) {
	for i := range steps {
		for _, l := range stepLocators(&steps[i]) {
			l.Normalize()
		}
	}
}

// stepLocators returns every locator a step carries (nil-safe).
func stepLocators(s *Step) []*Locator {
	var out []*Locator
	add := func(l *Locator) {
		if l != nil {
			out = append(out, l)
		}
	}
	add(s.Click)
	add(s.Hover)
	add(s.WaitFor)
	add(s.AssertVisible)
	add(s.AssertNotVisible)
	if s.Fill != nil {
		add(&s.Fill.Locator)
	}
	if s.Type != nil {
		add(&s.Type.Locator)
	}
	if s.Select != nil {
		add(&s.Select.Locator)
	}
	if s.AssertText != nil {
		add(s.AssertText.In)
	}
	if s.AssertNoText != nil {
		add(s.AssertNoText.In)
	}
	if s.AssertCount != nil {
		add(&s.AssertCount.Locator)
	}
	if s.AssertAttr != nil {
		add(&s.AssertAttr.Locator)
	}
	if s.AssertData != nil {
		add(&s.AssertData.UI)
	}
	return out
}

// ---- validation ---------------------------------------------------------

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,79}$`)

var validBy = map[string]bool{"": true, "test_id": true, "role": true, "label": true, "id": true, "text": true, "href": true, "css": true}

// Validate checks structural validity. flows is the set of known flow ids.
func (s *Scenario) Validate(flows map[string]bool) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if !idRe.MatchString(s.Scenario.ID) {
		add("scenario.id %q must match %s", s.Scenario.ID, idRe)
	}
	if s.Scenario.Version < 1 {
		add("scenario.version must be >= 1")
	}
	switch s.Scenario.Class {
	case "", "P0", "P1", "P2":
	default:
		add("scenario.class must be P0|P1|P2")
	}
	switch s.Scenario.Mutation {
	case "", "read-only", "reversible", "destructive":
	default:
		add("scenario.mutation must be read-only|reversible|destructive")
	}
	if s.Scenario.Mutation != "" && s.Scenario.Mutation != "read-only" && len(s.Resources.Locks) == 0 {
		add("mutating scenario must declare resources.locks")
	}
	switch s.Browser.Primary {
	case "", "lightpanda", "chromium":
	default:
		add("browser.primary must be lightpanda|chromium")
	}
	if len(s.Steps) == 0 {
		add("steps must not be empty")
	}
	hasAssert := s.Assert.NoUncaughtConsoleError || s.Assert.NoHTTP5xx || len(s.Assert.NoHTTP4xxOn) > 0
	for i, st := range s.Steps {
		k := st.Kind()
		if k == "" {
			add("steps[%d]: no action set", i)
			continue
		}
		if n := countActions(st); n > 1 {
			add("steps[%d]: exactly one action allowed, got %d", i, n)
		}
		if st.IsAssertion() {
			hasAssert = true
		}
		if err := validateStep(st); err != nil {
			add("steps[%d] (%s): %v", i, k, err)
		}
		if k == "use_flow" && flows != nil && !flows[st.UseFlow] {
			add("steps[%d]: unknown flow %q", i, st.UseFlow)
		}
	}
	for _, u := range s.Uses {
		if flows != nil && !flows[u] {
			add("uses: unknown flow %q", u)
		}
	}
	if !hasAssert {
		add("scenario has no assertion (business oracle required)")
	}
	switch s.Oracle.Source {
	case "spec", "approved_qa", "contract", "observation":
	case "":
		add("oracle.source is required (spec|approved_qa|contract|observation)")
	default:
		add("oracle.source %q invalid", s.Oracle.Source)
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid scenario %s:\n  - %s", s.Scenario.ID, strings.Join(errs, "\n  - "))
	}
	return nil
}

func (f *Flow) Validate() error {
	if !idRe.MatchString(f.Flow.ID) {
		return fmt.Errorf("flow.id %q invalid", f.Flow.ID)
	}
	if len(f.Steps) == 0 {
		return fmt.Errorf("flow %s: steps empty", f.Flow.ID)
	}
	for i, st := range f.Steps {
		if st.Kind() == "" {
			return fmt.Errorf("flow %s steps[%d]: no action", f.Flow.ID, i)
		}
		if st.UseFlow != "" {
			return fmt.Errorf("flow %s steps[%d]: nested use_flow not allowed", f.Flow.ID, i)
		}
		if err := validateStep(st); err != nil {
			return fmt.Errorf("flow %s steps[%d]: %w", f.Flow.ID, i, err)
		}
	}
	return nil
}

func countActions(s Step) int {
	n := 0
	for _, set := range []bool{s.Goto != "", s.Click != nil, s.Fill != nil, s.Type != nil, s.Press != "", s.Select != nil,
		s.Hover != nil, s.WaitFor != nil, s.WaitMs != 0, s.WaitURL != nil, s.AssertText != nil, s.AssertNoText != nil,
		s.AssertVisible != nil, s.AssertNotVisible != nil, s.AssertURL != nil, s.AssertCount != nil, s.AssertRequest != nil,
		s.AssertAttr != nil, s.AssertData != nil, s.ExpectPopup != nil, s.Eval != nil, s.UseFlow != "", s.Screenshot != ""} {
		if set {
			n++
		}
	}
	return n
}

func validateLocator(l *Locator) error {
	if l == nil {
		return fmt.Errorf("locator missing")
	}
	if !validBy[l.By] {
		return fmt.Errorf("locator.by %q invalid", l.By)
	}
	switch l.By {
	case "role":
		if l.Role == "" {
			return fmt.Errorf("role locator needs role")
		}
	case "text":
		if l.Text == "" && l.Value == "" {
			return fmt.Errorf("text locator needs text")
		}
	case "label":
		if l.Name == "" && l.Text == "" && l.Value == "" {
			return fmt.Errorf("label locator needs name")
		}
	case "test_id", "id", "href", "css":
		if l.Value == "" {
			return fmt.Errorf("%s locator needs value", l.By)
		}
	case "":
		if l.Value == "" && l.Text == "" && l.Name == "" && l.Role == "" {
			return fmt.Errorf("locator is empty")
		}
	}
	return nil
}

func validateStep(s Step) error {
	switch s.Kind() {
	case "click", "hover", "wait_for", "assert_visible", "assert_not_visible":
		var l *Locator
		switch s.Kind() {
		case "click":
			l = s.Click
		case "hover":
			l = s.Hover
		case "wait_for":
			l = s.WaitFor
		case "assert_visible":
			l = s.AssertVisible
		case "assert_not_visible":
			l = s.AssertNotVisible
		}
		return validateLocator(l)
	case "fill":
		return validateLocator(&s.Fill.Locator)
	case "type":
		return validateLocator(&s.Type.Locator)
	case "select":
		if s.Select.Option == "" {
			return fmt.Errorf("select needs option")
		}
		return validateLocator(&s.Select.Locator)
	case "assert_text", "assert_no_text":
		t := s.AssertText
		if t == nil {
			t = s.AssertNoText
		}
		if t.Value == "" {
			return fmt.Errorf("value required")
		}
		if t.In != nil {
			return validateLocator(t.In)
		}
	case "assert_url", "wait_url":
		u := s.AssertURL
		if u == nil {
			u = s.WaitURL
		}
		if u.Contains == "" && u.Matches == "" {
			return fmt.Errorf("contains or matches required")
		}
		if u.Matches != "" {
			if _, err := regexp.Compile(u.Matches); err != nil {
				return fmt.Errorf("matches: %w", err)
			}
		}
	case "assert_count":
		if s.AssertCount.Equals == nil && s.AssertCount.Min == nil && s.AssertCount.Max == nil {
			return fmt.Errorf("equals/min/max required")
		}
		return validateLocator(&s.AssertCount.Locator)
	case "assert_request":
		if s.AssertRequest.URLContains == "" {
			return fmt.Errorf("url_contains required")
		}
	case "assert_attr":
		if s.AssertAttr.Attr == "" {
			return fmt.Errorf("attr required")
		}
		return validateLocator(&s.AssertAttr.Locator)
	case "assert_data":
		return validateDataAssert(s.AssertData)
	case "eval":
		if s.Eval.Script == "" {
			return fmt.Errorf("script required")
		}
	case "goto":
		if s.Goto != "#" && !strings.HasPrefix(s.Goto, "/") && !strings.HasPrefix(s.Goto, "http") {
			return fmt.Errorf("goto must be a path, absolute URL, or # for the configured site URL")
		}
	}
	return nil
}

func validateDataAssert(a *DataAssert) error {
	if err := validateLocator(&a.UI); err != nil {
		return fmt.Errorf("ui: %w", err)
	}
	if a.UIRegex != "" {
		if _, err := regexp.Compile(a.UIRegex); err != nil {
			return fmt.Errorf("ui_regex: %w", err)
		}
	}
	if a.API.URLContains == "" {
		return fmt.Errorf("api.url_contains required")
	}
	if a.API.JSONPath == "" {
		return fmt.Errorf("api.json_path required")
	}
	if !strings.HasPrefix(a.API.JSONPath, "$") {
		return fmt.Errorf("api.json_path %q must start with $", a.API.JSONPath)
	}
	if !DataCompareModes[a.Compare] {
		return fmt.Errorf("compare %q must be text|number|contains", a.Compare)
	}
	return nil
}

// ---- fingerprint (PRD §9) ------------------------------------------------
//
// Inputs: capability goal, preconditions, major actions, oracle, route/API,
// persona class. Locator mechanics are ignored: click text="Submit",
// click test-id="submit" and click role=button name="Submit" all normalize to
// click:submit.

var wsRe = regexp.MustCompile(`\s+`)

func norm(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(strings.ToLower(s), " "))
}

// SemanticTarget reduces a locator to its business meaning.
func SemanticTarget(l *Locator) string {
	if l == nil {
		return ""
	}
	for _, c := range []string{l.Name, l.Text} {
		if c != "" {
			return norm(c)
		}
	}
	v := norm(l.Value)
	// test ids / ids: strip separators so "submit-btn" == "submit_btn" == "submitBtn"
	v = strings.NewReplacer("-", "", "_", "", "#", "", ".", "", "[", "", "]", "", "data-testid=", "", "\"", "", "'", "").Replace(v)
	if l.Role != "" && v == "" {
		return norm(l.Role)
	}
	return v
}

// Fingerprint computes the logical-scenario hash; flows are expanded so a
// scenario using a flow equals its inlined equivalent.
// NeedsPopupTarget reports whether the script waits for a new page target. Steps reached
// through use_flow are not scanned; declare browser.popup: true when the expect_popup
// lives in a Reusable Flow.
func (s *Scenario) NeedsPopupTarget() bool {
	if s.Browser.Popup {
		return true
	}
	for i := range s.Steps {
		if s.Steps[i].ExpectPopup != nil {
			return true
		}
	}
	return false
}

// RequiresChromiumEngine reports whether only Chromium can run this scenario. A browser
// that cannot create a popup target can never pass a script that waits for one, so the
// engine follows from the script itself and not only from the author's requires_chromium.
func (s *Scenario) RequiresChromiumEngine() bool {
	return s.Browser.RequiresChromium || s.NeedsPopupTarget()
}

func (s *Scenario) Fingerprint(flows map[string]*Flow) string {
	var parts []string
	parts = append(parts, "cap:"+norm(s.Covers.Capability))
	parts = append(parts, "persona:"+norm(s.Preconditions.Persona))
	routes := append([]string{}, s.Covers.Routes...)
	sort.Strings(routes)
	for i := range routes {
		routes[i] = norm(routes[i])
	}
	parts = append(parts, "routes:"+strings.Join(routes, ","))
	apis := append([]string{}, s.Covers.APIs...)
	sort.Strings(apis)
	for i := range apis {
		apis[i] = norm(apis[i])
	}
	parts = append(parts, "apis:"+strings.Join(apis, ","))

	var actions []string
	var expand func(steps []Step)
	expand = func(steps []Step) {
		for _, st := range steps {
			if st.UseFlow != "" {
				if f, ok := flows[st.UseFlow]; ok && f != nil {
					expand(f.Steps)
				} else {
					actions = append(actions, "flow:"+norm(st.UseFlow))
				}
				continue
			}
			if a := majorAction(st); a != "" {
				actions = append(actions, a)
			}
		}
	}
	for _, u := range s.Uses {
		if f, ok := flows[u]; ok && f != nil {
			expand(f.Steps)
		} else {
			actions = append(actions, "flow:"+norm(u))
		}
	}
	expand(s.Steps)
	parts = append(parts, "actions:"+strings.Join(actions, ";"))
	if s.Assert.NoUncaughtConsoleError {
		parts = append(parts, "assert:no_console_error")
	}
	if s.Assert.NoHTTP5xx {
		parts = append(parts, "assert:no_5xx")
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// majorAction ignores waits/screenshots/hover (mechanics) and keeps business actions + oracle.
func majorAction(st Step) string {
	switch st.Kind() {
	case "goto":
		return "goto:" + norm(stripQuery(st.Goto))
	case "click":
		return "click:" + SemanticTarget(st.Click)
	case "fill":
		return "fill:" + SemanticTarget(&st.Fill.Locator)
	case "type":
		return "fill:" + SemanticTarget(&st.Type.Locator)
	case "select":
		return "select:" + SemanticTarget(&st.Select.Locator) + "=" + norm(st.Select.Option)
	case "press":
		return "press:" + norm(st.Press)
	case "assert_text":
		return "assert_text:" + norm(st.AssertText.Value)
	case "assert_no_text":
		return "assert_no_text:" + norm(st.AssertNoText.Value)
	case "assert_visible":
		return "assert_visible:" + SemanticTarget(st.AssertVisible)
	case "assert_not_visible":
		return "assert_not_visible:" + SemanticTarget(st.AssertNotVisible)
	case "assert_url":
		return "assert_url:" + norm(st.AssertURL.Contains+st.AssertURL.Matches)
	case "assert_count":
		c := st.AssertCount
		return fmt.Sprintf("assert_count:%s:%v/%v/%v", SemanticTarget(&c.Locator), deref(c.Equals), deref(c.Min), deref(c.Max))
	case "assert_request":
		r := st.AssertRequest
		return fmt.Sprintf("assert_request:%s:%s:%d", norm(r.Method), norm(r.URLContains), r.Status)
	case "assert_attr":
		return "assert_attr:" + SemanticTarget(&st.AssertAttr.Locator) + ":" + norm(st.AssertAttr.Attr) + "=" + norm(st.AssertAttr.Equals+st.AssertAttr.Contains)
	case "assert_data":
		d := st.AssertData
		return fmt.Sprintf("assert_data:%s:%s:%s:%s", SemanticTarget(&d.UI), norm(d.API.URLContains), norm(d.API.JSONPath), norm(d.Compare))
	case "expect_popup":
		return "popup:" + norm(st.ExpectPopup.URLContains)
	case "eval":
		if st.Eval.Expect != "" {
			return "assert_eval:" + norm(st.Eval.Expect)
		}
	}
	return ""
}

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}

func stripQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}
