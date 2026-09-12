package proof

import (
	_ "embed"
	"encoding/json"
	"testing"
)

//go:embed testdata/evaluation-24.json
var evaluation24 []byte

func TestEvaluationSetHasBalancedExpectedVerdicts(t *testing.T) {
	var cases []struct {
		ID, Group, Want string
		Results         []string
	}
	if err := json.Unmarshal(evaluation24, &cases); err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	if len(cases) != 24 {
		t.Fatalf("evaluation cases=%d, want 24", len(cases))
	}
	for _, c := range cases {
		groups[c.Group]++
		results := make([]CriterionResult, 0, len(c.Results))
		for _, status := range c.Results {
			required := true
			if status == "OPTIONAL_FAIL" {
				required = false
				status = "FAIL"
			}
			results = append(results, CriterionResult{ID: c.ID, Required: required, Status: status})
		}
		if got := Verdict(results); got != c.Want {
			t.Errorf("%s: verdict=%s want %s", c.ID, got, c.Want)
		}
	}
	for _, group := range []string{"normal", "business-error", "insufficient-evidence"} {
		if groups[group] != 8 {
			t.Errorf("%s cases=%d, want 8", group, groups[group])
		}
	}
}
