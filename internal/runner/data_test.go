package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"

	"vigil/internal/dsl"
	"vigil/internal/model"
)

func TestJSONPath(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"data":{"totalCount":42,"items":[{"name":"a","score":1.5},{"name":"b"}],"key with spaces":"v","empty":[],"n":null,"t":true}}`), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, want, wantErr string
	}{
		{"$", `{"data":{`, ""},
		{"$.data.totalCount", "42", ""},
		{"$.data.items[1].name", "b", ""},
		{"$.data.items[*].score", "1.5", ""},
		{"$.data[\"key with spaces\"]", "v", ""},
		{"$.data['key with spaces']", "v", ""},
		{"$.data.n", "", ""},
		{"$.data.t", "true", ""},
		{"$.data.items[*]", `{"name":"a","score":1.5}`, ""},
		{"$.data.missing", "", ".missing: key not found"},
		{"$.data.items[5]", "", "[5]: index out of range"},
		{"$.data.empty[*]", "", "[*]: array is empty"},
		{"$.data.totalCount.x", "", ".x: parent is not an object"},
		{"$.data[0]", "", "[0]: parent is not an array"},
		{"data.totalCount", "", "must start with $"},
		{"$.data[abc]", "", "bad index"},
		{"$.data[0", "", "unterminated"},
		{"$..x", "", "empty key"},
	} {
		v, err := jsonPath(doc, tc.path)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.path, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.path, err)
			continue
		}
		if got := dataText(v); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s = %q, want prefix %q", tc.path, got, tc.want)
		}
	}
}

func TestCompareData(t *testing.T) {
	for _, tc := range []struct {
		mode, api, ui string
		ok            bool
	}{
		{"text", "홍길동", "  홍길동 ", true},
		{"text", "홍길동", "홍길순", false},
		{"number", "1234", "1,234명", true},
		{"number", "12000", "₩12,000", true},
		{"number", "45.5", "45.5 %", true},
		{"number", "1234", "1,235", false},
		{"number", "abc", "1", false},
		{"number", "1", "없음", false},
		{"contains", "김", "학생 김철수", true},
		{"contains", "박", "학생 김철수", false},
		{"weird", "a", "a", false},
	} {
		ok, reason := compareData(tc.mode, tc.api, tc.ui)
		if ok != tc.ok {
			t.Errorf("%s %q vs %q: ok=%v (%s), want %v", tc.mode, tc.api, tc.ui, ok, reason, tc.ok)
		}
	}
}

// dataSession builds a session with a stubbed capture: one captured GET on
// /api/students answered with body, and a UI element reading uiText.
func dataSession(t *testing.T, body string, uiText string) *session {
	t.Helper()
	runCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cap := newCapture(time.Now())
	cap.onRequest(&network.EventRequestWillBeSent{RequestID: "r1", Request: &network.Request{URL: "https://t.example.com/api/students?page=1", Method: "GET"}})
	cap.onResponse(&network.EventResponseReceived{RequestID: "r1", Response: &network.Response{URL: "https://t.example.com/api/students?page=1", Status: 200, MimeType: "application/json"}})
	// a later response for a different endpoint must not shadow the match
	cap.onRequest(&network.EventRequestWillBeSent{RequestID: "r2", Request: &network.Request{URL: "https://t.example.com/api/teachers", Method: "GET"}})
	cap.onResponse(&network.EventResponseReceived{RequestID: "r2", Response: &network.Response{URL: "https://t.example.com/api/teachers", Status: 200}})
	s := &session{spec: Spec{Browser: model.BrowserChromium, RunTimeout: time.Minute}, runCtx: runCtx, pageCtx: context.Background(),
		cap: cap, res: &Result{Capabilities: map[string]bool{}}, navigated: true}
	s.bodyOf = func(_ context.Context, id network.RequestID) ([]byte, error) {
		if id != "r1" {
			return nil, errors.New("unexpected request id " + string(id))
		}
		return []byte(body), nil
	}
	s.textOf = func(context.Context, *dsl.Locator, string) (string, *stepErr) { return uiText, nil }
	return s
}

func TestAssertDataStep(t *testing.T) {
	step := func(compare, path, re string) *dsl.DataAssert {
		return &dsl.DataAssert{UI: dsl.Locator{By: "css", Value: ".total-count"}, UIRegex: re,
			API: dsl.DataAPI{URLContains: "/api/students", JSONPath: path, Method: "GET"}, Compare: compare}
	}
	body := `{"data":{"totalCount":1234,"items":[{"name":"김철수"}]}}`

	t.Run("number matches through regex and separators", func(t *testing.T) {
		s := dataSession(t, body, "전체 1,234명")
		sr := &StepResult{Kind: "assert_data"}
		if err := s.doAssertData(context.Background(), step("number", "$.data.totalCount", `[\d,]+`), sr, "s1"); err != nil {
			t.Fatalf("unexpected failure: %v (%s / %s)", err, err.expected, err.actual)
		}
		if !strings.Contains(sr.Expected, "api /api/students $.data.totalCount") || !strings.Contains(sr.Actual, "ui by=css value=\".total-count\"") {
			t.Fatalf("sources missing: expected=%q actual=%q", sr.Expected, sr.Actual)
		}
	})
	t.Run("mismatch is a business assertion with both sources", func(t *testing.T) {
		s := dataSession(t, body, "전체 1,235명")
		err := s.doAssertData(context.Background(), step("number", "$.data.totalCount", `[\d,]+`), &StepResult{}, "s1")
		if err == nil || err.class != FailAssertion {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(err.expected, `api /api/students $.data.totalCount = "1234"`) || !strings.Contains(err.actual, `ui by=css value=".total-count" = "1,235"`) {
			t.Fatalf("expected=%q actual=%q", err.expected, err.actual)
		}
	})
	t.Run("text and contains", func(t *testing.T) {
		s := dataSession(t, body, "학생 김철수")
		if err := s.doAssertData(context.Background(), step("contains", "$.data.items[*].name", ""), &StepResult{}, "s1"); err != nil {
			t.Fatal(err)
		}
		if err := s.doAssertData(context.Background(), step("text", "$.data.items[0].name", ""), &StepResult{}, "s1"); err == nil || err.class != FailAssertion {
			t.Fatalf("text must fail on the surrounding label: %v", err)
		}
	})
	t.Run("unresolvable path and no regex match are explicit business failures", func(t *testing.T) {
		s := dataSession(t, body, "없음")
		err := s.doAssertData(context.Background(), step("text", "$.data.count", ""), &StepResult{}, "s1")
		if err == nil || err.class != FailAssertion || !strings.Contains(err.actual, ".count: key not found") {
			t.Fatalf("err = %v", err)
		}
		err = s.doAssertData(context.Background(), step("number", "$.data.totalCount", `\d+`), &StepResult{}, "s1")
		if err == nil || err.class != FailAssertion || !strings.Contains(err.actual, "no match for ui_regex") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing response fails after the step timeout", func(t *testing.T) {
		s := dataSession(t, body, "x")
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		a := step("text", "$.x", "")
		a.API.URLContains = "/api/nothing"
		err := s.doAssertData(ctx, a, &StepResult{}, "s1")
		if err == nil || err.class != FailAssertion || !strings.Contains(err.actual, "no captured response matching /api/nothing GET") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-JSON body", func(t *testing.T) {
		s := dataSession(t, "<html>", "x")
		err := s.doAssertData(context.Background(), step("text", "$.x", ""), &StepResult{}, "s1")
		if err == nil || err.class != FailAssertion || !strings.Contains(err.actual, "is not JSON") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("browser without network events takes the protocol path", func(t *testing.T) {
		s := dataSession(t, body, "x")
		s.cap = newCapture(time.Now())
		s.spec.Browser = model.BrowserLightpanda
		err := s.doAssertData(context.Background(), step("text", "$.x", ""), &StepResult{}, "s1")
		if err == nil || err.class != FailBrowserProtocol || s.res.Capabilities["network"] {
			t.Fatalf("err = %v caps=%v", err, s.res.Capabilities)
		}
	})
}
