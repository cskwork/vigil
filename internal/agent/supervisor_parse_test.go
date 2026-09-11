package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParsePlan(t *testing.T) {
	answer := "I looked at the state.\n\n" +
		"```yaml " + PlanFence + "\n" +
		"assessment: /reports has no coverage at all\n" +
		"coverage_gaps:\n  - /reports\nactions:\n" +
		"  - verb: Discover_Route\n    target: '  /reports  '\n    reason: nothing covers it\n" +
		"  - verb: raise_cadence\n    target: entry-landing\n    class: p0\n    reason: critical path\n" +
		"saturated: false\n" +
		"```\n"
	p, err := ParsePlan(answer)
	if err != nil {
		t.Fatal(err)
	}
	if p.Assessment == "" || len(p.CoverageGaps) != 1 || len(p.Actions) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	// verbs lower-cased, classes upper-cased, targets trimmed: the validator
	// compares against fixed tables and must not fail on model formatting.
	if p.Actions[0].Verb != "discover_route" || p.Actions[0].Target != "/reports" {
		t.Errorf("action 0 = %+v", p.Actions[0])
	}
	if p.Actions[1].Class != "P0" {
		t.Errorf("class = %q, want P0", p.Actions[1].Class)
	}
}

// A model that echoes the template before answering must not win: the last
// block is the answer.
func TestParsePlanTakesTheLastBlock(t *testing.T) {
	answer := "```yaml " + PlanFence + "\nassessment: template\nactions: []\n```\n" +
		"now the real one\n" +
		"```yaml " + PlanFence + "\nassessment: real\nactions: []\nsaturated: true\n```"
	p, err := ParsePlan(answer)
	if err != nil {
		t.Fatal(err)
	}
	if p.Assessment != "real" || !p.Saturated {
		t.Fatalf("plan = %+v", p)
	}
}

func TestParsePlanRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"no fenced block here",
		"```yaml " + PlanFence + "\n\tactions: [oh no\n```",
	} {
		if _, err := ParsePlan(in); err == nil {
			t.Errorf("ParsePlan(%q) should have failed", in)
		}
	}
}

// A real answer from zai/glm-5.3-flash that broke the first live supervisor
// tick: the assessment contained "a larger unexplored surface: the teacher-entry
// scenario ...", and one colon in a sentence threw away an otherwise good plan.
func TestParsePlanRepairsUnquotedProse(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "plan-unquoted-colon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// It must genuinely be broken YAML, or this test proves nothing.
	var probe map[string]any
	if yaml.Unmarshal(body, &probe) == nil {
		t.Fatal("fixture parses cleanly; it no longer reproduces the failure")
	}

	p, err := ParsePlan("```yaml " + PlanFence + "\n" + string(body) + "```")
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if !strings.Contains(p.Assessment, "unexplored surface") {
		t.Errorf("assessment lost: %q", p.Assessment)
	}
	if len(p.CoverageGaps) != 3 {
		t.Errorf("coverage_gaps = %v, want 3", p.CoverageGaps)
	}
	if len(p.Actions) != 3 {
		t.Fatalf("actions = %d, want 3", len(p.Actions))
	}
	for _, a := range p.Actions {
		if a.Verb != "discover_route" {
			t.Errorf("verb = %q", a.Verb)
		}
		if a.Reason == "" {
			t.Errorf("reason lost for %q", a.Target)
		}
	}
	// "target: /app/training-entry (student entry path)" must become a path.
	if got := p.Actions[2].Target; got != "/app/training-entry" {
		t.Errorf("target = %q, want the path without the parenthetical", got)
	}
}

// Two consecutive live ticks failed the same way, so the repair is checked
// against both real answers rather than one.
func TestParsePlanRepairsEveryCapturedAnswer(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "plan-*.yaml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("want at least two captured answers, got %v (%v)", files, err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p, err := ParsePlan("```yaml " + PlanFence + "\n" + string(body) + "```")
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if p.Assessment == "" {
			t.Errorf("%s: assessment lost", filepath.Base(f))
		}
		if len(p.Actions) == 0 {
			t.Errorf("%s: no actions survived", filepath.Base(f))
		}
		for _, a := range p.Actions {
			if a.Verb == "" || a.Target == "" || a.Reason == "" {
				t.Errorf("%s: incomplete action %+v", filepath.Base(f), a)
			}
			if strings.ContainsAny(a.Target, " \t") {
				t.Errorf("%s: target %q still has whitespace", filepath.Base(f), a.Target)
			}
		}
	}
}

func TestSanitizeTarget(t *testing.T) {
	for in, want := range map[string]string{
		"/app/training-entry (student entry path)": "/app/training-entry",
		"  /reports  ": "/reports",
		"app.routing":  "app.routing",
		"/entry,":      "/entry",
		"/a/b":         "/a/b",
	} {
		if got := sanitizeTarget(in); got != want {
			t.Errorf("sanitizeTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// The repair must not touch a plan that is already valid.
func TestRepairLeavesGoodYAMLAlone(t *testing.T) {
	good := "assessment: nothing to do\nactions: []\nsaturated: true\n"
	if got := repairPlanYAML(good); got != good {
		t.Errorf("repair changed valid YAML:\n%q", got)
	}
}
