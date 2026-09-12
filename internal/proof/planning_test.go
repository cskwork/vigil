package proof

import (
	"encoding/json"
	"testing"
)

func TestPlanOutputAllowsOneQuestionAndMarksModelCriteriaProposed(t *testing.T) {
	reg := testRegistry()
	if _, question, err := parsePlanOutput(reg, "qa", "zero", json.RawMessage(`{"question":"어떤 값을 0으로 보나요?"}`)); err != nil || question == "" {
		t.Fatalf("question=%q err=%v", question, err)
	}
	if _, _, err := parsePlanOutput(reg, "qa", "zero", json.RawMessage(`{"question":"질문","extra":true}`)); err == nil {
		t.Fatal("question envelope accepted extra model output")
	}
	raw, err := json.Marshal(contract())
	if err != nil {
		t.Fatal(err)
	}
	con, question, err := parsePlanOutput(reg, "qa", "zero", raw)
	if err != nil || question != "" || len(con.Criteria) != 1 || !con.Criteria[0].Proposed {
		t.Fatalf("contract=%+v question=%q err=%v", con, question, err)
	}
}

func TestDirectReadRequiresExactRegisteredGETReread(t *testing.T) {
	reg := testRegistry()
	target := reg.Targets["qa"]
	target.Scope = []ScopeRule{{Origin: "http://127.0.0.1:8790", Method: "GET", Path: "/"}, {Origin: "http://127.0.0.1:8790", Method: "GET", Path: "/record", Query: map[string]string{"entity": "qa-1"}}}
	target.Observers["dom"] = Observer{Kind: "network", Action: "read", API: &target.Scope[1], Reread: true, DirectRead: true}
	reg.Targets["qa"] = target
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	observer := target.Observers["dom"]
	observer.API.Method = "POST"
	target.Observers["dom"] = observer
	reg.Targets["qa"] = target
	if err := reg.Validate(); err == nil {
		t.Fatal("direct POST read accepted")
	}
}
