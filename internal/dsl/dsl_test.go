package dsl

import "testing"

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
