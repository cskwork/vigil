package runner

import (
	"strings"
	"testing"

	"vigil/internal/dsl"
)

func TestGotoResolvesAgainstEnvBaseURL(t *testing.T) {
	sc, err := dsl.Parse([]byte("scenario:\n  id: s\n  version: 1\ncovers:\n  feature: f\n  capability: c\nsteps:\n  - goto: /app/entry\n  - assert_text: { value: x }\noracle:\n  source: contract\n"))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := flatten(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Scenario: sc, BaseURL: "https://www.example.com", Environment: "prod", AllowedHosts: []string{"example.com"}}
	if got, _ := resolveURL(spec.BaseURL, steps[0].Step.Goto); got != "https://www.example.com/app/entry" {
		t.Fatalf("resolved %q", got)
	}
	if res := envRejects(spec, steps); res != nil {
		t.Fatalf("relative goto inside the env must pass validation: %+v", res)
	}
	// an absolute goto outside the env allowlist fails the run before any browser starts
	sc.Steps[0].Goto = "https://staging.example.net/app/entry"
	steps, _ = flatten(sc, nil)
	res := envRejects(spec, steps)
	if res == nil || res.Passed || res.Class != FailEnvironment || res.FailedStep == nil || res.FailedStep.Index != 1 {
		t.Fatalf("outside allowlist: %+v", res)
	}
	if !strings.Contains(res.Error, ErrOutsideAllowlist) || !strings.Contains(res.Error, "prod") {
		t.Fatalf("error %q", res.Error)
	}
	// no allowlist = legacy unrestricted spec
	if res := envRejects(Spec{Scenario: sc, BaseURL: "https://www.example.com"}, steps); res != nil {
		t.Fatalf("empty allowlist must not reject: %+v", res)
	}
}

func TestURLAllowed(t *testing.T) {
	hosts := []string{"example.com", "Cdn.Example.com"}
	for u, want := range map[string]bool{
		"https://www.example.com/x": true, "https://example.com": true, "https://cdn.example.com/a": true,
		"https://example.com.evil.test/": false, "https://staging.example.net/": false, "/relative": false, "": false,
	} {
		if got := urlAllowed(hosts, u); got != want {
			t.Errorf("urlAllowed(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestConfiguredSiteURLPreservesPathAndQuery(t *testing.T) {
	for _, base := range []string{"https://example.com/app/", "https://example.com/search?query=hello"} {
		got, err := resolveURL(base, "#")
		if err != nil || got != base {
			t.Fatalf("configured URL changed: %q %v", got, err)
		}
	}
}
