package dsl

import (
	"strings"
	"testing"
)

const sample = `
scenario:
  id: visible-remedy-order
  version: 3
covers:
  feature: remedy-page-order
  capability: remedy.navigation
uses:
  - login-as-student
preconditions:
  persona: qa_student
steps:
  - goto: /remedy/current
  - assert_text: { value: "1" }
  - click: { by: role, role: button, name: Next }
  - assert_text: { value: "2" }
assert:
  no_uncaught_console_error: true
  no_http_5xx: true
oracle:
  source: spec
  source_feature: remedy-page-order
  source_sha: abc123
`

func TestParseValidateFingerprint(t *testing.T) {
	sc, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(map[string]bool{"login-as-student": true}); err != nil {
		t.Fatal(err)
	}
	fp1 := sc.Fingerprint(nil)

	// Same logical scenario, different locator mechanics -> same fingerprint.
	sc2, _ := Parse([]byte(sample))
	sc2.Steps[2].Click = &Locator{By: "test_id", Value: "next"}
	if fp2 := sc2.Fingerprint(nil); fp1 != fp2 {
		t.Fatalf("locator mechanics changed fingerprint: %s != %s", fp1, fp2)
	}
	// Different oracle -> different fingerprint.
	sc3, _ := Parse([]byte(sample))
	sc3.Steps[3].AssertText.Value = "3"
	if fp3 := sc3.Fingerprint(nil); fp1 == fp3 {
		t.Fatal("oracle change must change fingerprint")
	}
}

func TestValidateRejectsNoOracle(t *testing.T) {
	sc, _ := Parse([]byte(sample))
	sc.Oracle.Source = ""
	if err := sc.Validate(nil); err == nil {
		t.Fatal("expected oracle error")
	}
	sc, _ = Parse([]byte(sample))
	sc.Steps = []Step{{Goto: "/x"}}
	sc.Assert.NoHTTP5xx = false
	sc.Assert.NoUncaughtConsoleError = false
	if err := sc.Validate(nil); err == nil {
		t.Fatal("expected no-assertion error")
	}
}

// A script that waits for a popup can only pass on an engine that can create one, so the
// engine must follow from the steps and not only from the author's requires_chromium.
func TestRequiresChromiumEngineFollowsTheScript(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"plain script takes the default", `
scenario: {id: s1, version: 1}
steps:
  - goto: /a
  - assert_text: {value: ok}
oracle: {source: spec}
`, false},
		{"expect_popup forces chromium", `
scenario: {id: s2, version: 1}
steps:
  - goto: /a
  - expect_popup: {url_contains: /b}
oracle: {source: spec}
`, true},
		{"browser.popup covers a popup inside a flow", `
scenario: {id: s3, version: 1}
browser: {popup: true}
steps:
  - goto: /a
oracle: {source: spec}
`, true},
		{"requires_chromium still stands alone", `
scenario: {id: s4, version: 1}
browser: {requires_chromium: true}
steps:
  - goto: /a
oracle: {source: spec}
`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := sc.RequiresChromiumEngine(); got != tc.want {
				t.Fatalf("RequiresChromiumEngine = %v, want %v", got, tc.want)
			}
		})
	}
}

const dataSample = `
scenario:
  id: student-count-matches-api
  version: 1
covers:
  feature: students
  capability: students.list
steps:
  - goto: /students
  - assert_data:
      ui: { by: css, value: ".total-count" }
      ui_regex: '\d+'
      api: { url_contains: /api/students, json_path: $.data.totalCount, method: GET }
      compare: number
oracle:
  source: contract
`

func TestAssertDataValidatesAndIsMajorAction(t *testing.T) {
	sc, err := Parse([]byte(dataSample))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(nil); err != nil {
		t.Fatal(err)
	}
	if k := sc.Steps[1].Kind(); k != "assert_data" || !sc.Steps[1].IsAssertion() {
		t.Fatalf("kind = %s", k)
	}
	fp := sc.Fingerprint(nil)
	other, _ := Parse([]byte(dataSample))
	other.Steps[1].AssertData.API.JSONPath = "$.data.count"
	if other.Fingerprint(nil) == fp {
		t.Fatal("assert_data json_path change must change the fingerprint")
	}
	mech, _ := Parse([]byte(dataSample))
	mech.Steps[1].AssertData.UI = Locator{By: "id", Value: "total-count"}
	if mech.Fingerprint(nil) != fp {
		t.Fatal("assert_data locator mechanics must not change the fingerprint")
	}
	for _, bad := range []struct {
		mut  func(*DataAssert)
		want string
	}{
		{func(d *DataAssert) { d.Compare = "equals" }, "compare"},
		{func(d *DataAssert) { d.API.JSONPath = "" }, "json_path required"},
		{func(d *DataAssert) { d.API.JSONPath = "data.count" }, "must start with $"},
		{func(d *DataAssert) { d.API.URLContains = "" }, "url_contains required"},
		{func(d *DataAssert) { d.UI = Locator{} }, "ui:"},
		{func(d *DataAssert) { d.UIRegex = "(" }, "ui_regex"},
	} {
		s, _ := Parse([]byte(dataSample))
		bad.mut(s.Steps[1].AssertData)
		err := s.Validate(nil)
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("want error containing %q, got %v", bad.want, err)
		}
	}
}

func TestNormalizeLocatorAliases(t *testing.T) {
	cases := []struct {
		name string
		in   Locator
		want Locator
		ok   bool // Validate passes after Normalize
	}{
		{"link keeps name", Locator{By: "link", Name: "다음"}, Locator{By: "role", Role: "link", Name: "다음"}, true},
		{"link folds text into name", Locator{By: "link", Text: "다음", Exact: true}, Locator{By: "role", Role: "link", Name: "다음", Text: "다음", Exact: true}, true},
		{"button", Locator{By: "button", Name: "Next"}, Locator{By: "role", Role: "button", Name: "Next"}, true},
		{"button keeps explicit role", Locator{By: "button", Role: "tab", Name: "정보"}, Locator{By: "role", Role: "tab", Name: "정보"}, true},
		{"placeholder", Locator{By: "placeholder", Name: "이메일"}, Locator{By: "label", Name: "이메일"}, true},
		{"testid", Locator{By: "testid", Value: "next"}, Locator{By: "test_id", Value: "next"}, true},
		{"data-testid", Locator{By: "data-testid", Value: "next"}, Locator{By: "test_id", Value: "next"}, true},
		{"test-id", Locator{By: "test-id", Value: "next"}, Locator{By: "test_id", Value: "next"}, true},
		{"selector", Locator{By: "selector", Value: "#next"}, Locator{By: "css", Value: "#next"}, true},
		{"case-insensitive", Locator{By: " Role ", Role: "tab", Name: "x"}, Locator{By: "role", Role: "tab", Name: "x"}, true},
		{"canonical untouched", Locator{By: "css", Value: "a"}, Locator{By: "css", Value: "a"}, true},
		{"empty stays empty", Locator{Text: "x"}, Locator{Text: "x"}, true},
		{"xpath still invalid", Locator{By: "xpath", Value: "//a"}, Locator{By: "xpath", Value: "//a"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in
			got.Normalize()
			if got != c.want {
				t.Fatalf("normalize %+v = %+v, want %+v", c.in, got, c.want)
			}
			if err := validateLocator(&got); (err == nil) != c.ok {
				t.Fatalf("validate after normalize: err=%v ok=%v", err, c.ok)
			}
		})
	}
}

func TestParseNormalizesEveryLocator(t *testing.T) {
	y := `
scenario: { id: alias-test, version: 1 }
covers: { capability: entry }
steps:
  - goto: /entry
  - wait_for: { by: LINK, name: "중학" }
  - click: { by: button, name: "중학" }
  - fill: { by: placeholder, name: "이메일", input: "a@b" }
  - type: { by: testid, value: pw, input: "x" }
  - select: { by: selector, value: "#grade", option: "1" }
  - hover: { by: data-testid, value: menu }
  - assert_visible: { by: test-id, value: menu }
  - assert_not_visible: { by: link, text: "삭제" }
  - assert_text: { value: "1", in: { by: button, name: "탭" } }
  - assert_no_text: { value: "2", in: { by: link, name: "탭" } }
  - assert_count: { by: link, name: "정보", equals: 1 }
  - assert_attr: { by: selector, value: "a", attr: href, contains: "/x" }
  - assert_data: { ui: { by: link, name: "점수" }, api: { url_contains: /api, json_path: $.score }, compare: text }
oracle: { source: spec }
`
	sc, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Validate(nil); err != nil {
		t.Fatalf("aliases must validate after Parse: %v", err)
	}
	for i, l := range []*Locator{sc.Steps[1].WaitFor, sc.Steps[2].Click, &sc.Steps[3].Fill.Locator, &sc.Steps[4].Type.Locator, &sc.Steps[5].Select.Locator,
		sc.Steps[6].Hover, sc.Steps[7].AssertVisible, sc.Steps[8].AssertNotVisible, sc.Steps[9].AssertText.In, sc.Steps[10].AssertNoText.In,
		&sc.Steps[11].AssertCount.Locator, &sc.Steps[12].AssertAttr.Locator, &sc.Steps[13].AssertData.UI} {
		if !validBy[l.By] || l.By == "" {
			t.Fatalf("locator %d not normalized: %+v", i, l)
		}
	}
	if sc.Steps[1].WaitFor.Role != "link" || sc.Steps[8].AssertNotVisible.Name != "삭제" {
		t.Fatalf("role shorthand: %+v %+v", sc.Steps[1].WaitFor, sc.Steps[8].AssertNotVisible)
	}
	// the flow parser normalizes too; unknown kinds still fail
	f, err := ParseFlow([]byte("flow: { id: login, version: 1 }\nsteps:\n  - click: { by: Button, name: Login }\n"))
	if err != nil || f.Steps[0].Click.By != "role" || f.Steps[0].Click.Role != "button" {
		t.Fatalf("flow normalize: %+v %v", f, err)
	}
	bad, err := Parse([]byte(strings.Replace(y, "by: LINK", "by: xpath", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Validate(nil); err == nil || !strings.Contains(err.Error(), `locator.by "xpath" invalid`) {
		t.Fatalf("xpath must stay invalid: %v", err)
	}
}
