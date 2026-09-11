package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"

	"vigil/internal/agent"
	"vigil/internal/model"
)

// The live PROJ-132 shape: the validation run ended APP_FAILURE at step 13 on an
// exact-number assertion, and the agent block decides whether that is evidence.
func TestReproductionVerdicts(t *testing.T) {
	feat := &model.Feature{ID: "PROJ-132", Summary: "제목 기본값이 40자를 넘는다", Routes: []string{"/app/lms/assignment/edit/FORM_A"}}
	failed := &model.Run{ID: 7, FailedStep: 13}
	finding := agent.Finding{Kind: agent.FindingDataMismatch, Where: "https://app-7.staging.example.com/app/lms/assignment/edit/FORM_A #title"}

	cases := []struct {
		name       string
		outcome    model.Outcome
		run        *model.Run
		res        *agent.Result
		verdict    string
		reproduced bool
		atStep     int
		claimed    int
		why        string
	}{
		{
			name:    "confirmed: agent claims step 14, run failed at 13, one finding on the route",
			outcome: model.OutcomeAppFailure, run: failed,
			res:     &agent.Result{Reproduction: &agent.Reproduction{Symptom: "기본 제목이 40자", Reproduced: true, AtStep: 14}, Findings: []agent.Finding{finding}},
			verdict: verdictConfirmed, reproduced: true, atStep: 13, claimed: 14,
		},
		{
			name:    "confirmed by a finding on the same route although the step is far off",
			outcome: model.OutcomeAppFailure, run: failed,
			res:     &agent.Result{Reproduction: &agent.Reproduction{Reproduced: true, AtStep: 3}, Findings: []agent.Finding{finding}},
			verdict: verdictConfirmed, reproduced: true, atStep: 13, claimed: 3,
		},
		{
			name:    "disputed: agent claims step 9 and reports no finding",
			outcome: model.OutcomeAppFailure, run: failed,
			res:     &agent.Result{Reproduction: &agent.Reproduction{Reproduced: true, AtStep: 9}},
			verdict: verdictDisputed, atStep: 13, claimed: 9, why: "9단계",
		},
		{
			name:    "disputed: run failed but the agent says it did not reproduce",
			outcome: model.OutcomeAppFailure, run: failed,
			res:     &agent.Result{Reproduction: &agent.Reproduction{Reproduced: false, AtStep: 13}},
			verdict: verdictDisputed, atStep: 13, claimed: 13, why: "재현되지 않았다",
		},
		{
			name:    "unconfirmed: the expected behaviour held",
			outcome: model.OutcomePass, run: &model.Run{ID: 7},
			res:     &agent.Result{Reproduction: &agent.Reproduction{Reproduced: true, AtStep: 13}},
			verdict: verdictUnconfirmed, claimed: 13, why: "PASS",
		},
		{
			name:    "unconfirmed: no agent block to corroborate the failure",
			outcome: model.OutcomeAppFailure, run: failed,
			res:     &agent.Result{},
			verdict: verdictUnconfirmed, why: "보고하지 않아",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := reproductionFor(tc.outcome, tc.run, tc.res, feat)
			if rec.Verdict != tc.verdict || rec.Reproduced != tc.reproduced || rec.AtStep != tc.atStep || rec.ClaimedStep != tc.claimed {
				t.Fatalf("record = %+v", rec)
			}
			if tc.why != "" && !strings.Contains(rec.Why, tc.why) {
				t.Fatalf("why = %q, want it to mention %q", rec.Why, tc.why)
			}
			if tc.verdict == verdictConfirmed && rec.Why != "" {
				t.Fatalf("confirmed needs no explanation: %q", rec.Why)
			}
			if rec.RunID != 7 {
				t.Fatalf("run id = %d", rec.RunID)
			}
			// The stored JSON keeps `reproduced` for older readers and adds the verdict.
			var raw map[string]any
			if err := json.Unmarshal([]byte(rec.JSON()), &raw); err != nil {
				t.Fatal(err)
			}
			if raw["verdict"] != tc.verdict || raw["reproduced"] != tc.reproduced {
				t.Fatalf("json = %s", rec.JSON())
			}
		})
	}
}

// The symptom falls back to the feature summary when the agent reported none.
func TestReproductionSymptomFallback(t *testing.T) {
	feat := &model.Feature{Summary: "제목 기본값이 40자를 넘는다\n두 번째 줄"}
	rec := reproductionFor(model.OutcomePass, &model.Run{ID: 1}, nil, feat)
	if rec.Symptom != "제목 기본값이 40자를 넘는다" || rec.Verdict != verdictUnconfirmed {
		t.Fatalf("record = %+v", rec)
	}
}
