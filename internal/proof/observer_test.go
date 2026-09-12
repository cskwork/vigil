package proof

import (
	"context"
	"encoding/json"
	"github.com/chromedp/cdproto/network"
	"testing"
	"time"
)

func networkObservation() (*observation, Observer) {
	start := time.Now().Add(-time.Second)
	rule := ScopeRule{Origin: "http://qa.test", Method: "GET", Path: "/record", Query: map[string]string{"entity": "qa-1"}}
	o := &observation{ctx: context.Background(), a: &Attempt{ID: "attempt", Contract: Contract{Persona: "p"}, Fixture: Fixture{Entity: "qa-1"}, FixtureGeneration: "generation"}, actionTimes: map[string]time.Time{"save": start}, responses: map[network.RequestID]*response{"read": {ID: "read", URL: "http://qa.test/record?entity=qa-1", Method: "GET", Sent: start.Add(100 * time.Millisecond), Received: start.Add(200 * time.Millisecond), Status: 200, Body: []byte(`{"value":"","entity":"qa-1","generation":"generation","success":true}`)}}}
	return o, Observer{Kind: "network", Action: "save", API: &rule, JSONPath: "$.value", EntityPath: "$.entity", GenerationPath: "$.generation", SuccessPath: "$.success", Success: Value{true, json.RawMessage(`true`)}}
}
func TestCorrelatedNetworkRejectsWrongEntityGenerationAndAmbiguity(t *testing.T) {
	for _, c := range []struct {
		name      string
		body      string
		duplicate bool
		want      string
	}{{"correct", `{"value":"","entity":"qa-1","generation":"generation","success":true}`, false, ""}, {"wrong entity", `{"value":"","entity":"qa-2","generation":"generation","success":true}`, false, "wrong entity response"}, {"wrong generation", `{"value":"","entity":"qa-1","generation":"old","success":true}`, false, "wrong fixture generation"}, {"missing success", `{"value":"","entity":"qa-1","generation":"generation"}`, false, "success field missing"}, {"ambiguous", `{"value":"","entity":"qa-1","generation":"generation","success":true}`, true, "ambiguous correlated responses"}} {
		t.Run(c.name, func(t *testing.T) {
			o, ob := networkObservation()
			o.responses["read"].Body = []byte(c.body)
			if c.duplicate {
				copy := *o.responses["read"]
				copy.ID = "other"
				o.responses["other"] = &copy
			}
			v, reason, metadata := o.networkValue(context.Background(), ob)
			if reason != c.want {
				t.Fatalf("got %q want %q", reason, c.want)
			}
			if reason == "" && (!Equal(v, Value{true, json.RawMessage(`""`)}) || len(metadata) == 0) {
				t.Fatal("missing typed value or correlation metadata")
			}
		})
	}
}
func TestRereadRequiresSuccessfulWriteResponseBeforeRead(t *testing.T) {
	o, ob := networkObservation()
	ob.Reread = true
	write := ScopeRule{Origin: "http://qa.test", Method: "POST", Path: "/record"}
	ob.WriteAPI = &write
	if _, reason, _ := o.networkValue(context.Background(), ob); reason != "save response before reread not established" {
		t.Fatal(reason)
	}
	start := o.actionTimes["save"]
	o.responses["write"] = &response{URL: "http://qa.test/record", Method: "POST", Sent: start.Add(10 * time.Millisecond), Received: start.Add(50 * time.Millisecond), Status: 200, Body: []byte(`{"success":true}`)}
	if _, reason, _ := o.networkValue(context.Background(), ob); reason != "" {
		t.Fatal(reason)
	}
	o.responses["write"].Received = start.Add(150 * time.Millisecond)
	if _, reason, _ := o.networkValue(context.Background(), ob); reason != "save response before reread not established" {
		t.Fatal("stale read accepted", reason)
	}
}
func TestPreActionResponseCannotEstablishExpectedValue(t *testing.T) {
	o, ob := networkObservation()
	o.responses["read"].Sent = o.actionTimes["save"].Add(-time.Second)
	_, reason, _ := o.networkValue(context.Background(), ob)
	if reason != "correlated response missing" {
		t.Fatal(reason)
	}
}
