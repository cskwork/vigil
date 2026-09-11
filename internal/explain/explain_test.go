package explain

import (
	"strings"
	"testing"

	"vigil/internal/model"
)

// The rows of the WI-J headline table, each built from a run fixture shaped
// exactly like what internal/runner writes.
func TestForRunHeadlines(t *testing.T) {
	cases := []struct {
		name     string
		run      model.Run
		headline string
		kind     string
	}{
		{"assert_text mismatch",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 7, FailedAction: "assert_text",
				Expected: `text "제출 완료" present`, Actual: `text sample: "제출 중"`,
				Error: `step 7 (assert_text): assert_text: text "제출 완료" present; text sample: "제출 중"`},
			`화면에 "제출 완료"가 보여야 하는데 "제출 중"이 보입니다.`, KindService},

		{"assert_count mismatch",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 4, FailedAction: "assert_count",
				Expected: `count by=role role=listitem == 15`, Actual: "count 12"},
			"개수가 15개여야 하는데 화면에는 12개입니다.", KindData},

		{"assert_data mismatch",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 9, FailedAction: "assert_data",
				Expected: `api /api/students $.total = "15"`, Actual: `ui by=role role=status = "12"`},
			`화면에 보이는 값은 "12", 서버 응답 값은 "15"로 서로 다릅니다.`, KindData},

		{"eval bound",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 13, FailedAction: "eval",
				Expected: "40", Actual: "37", Error: "step 13 (eval): eval: expected 40, got 37"},
			"기준 값은 40인데 화면에서 확인한 값은 37입니다.", KindService},

		{"assert_request 5xx",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 6, FailedAction: "assert_request",
				Expected: "request GET /api/assignments status 200", Actual: "GET 500 https://staging.example.com/api/assignments"},
			"서버가 /api/assignments 요청에 500으로 답했습니다.", KindService},

		{"app failure with a missing element",
			model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 5, FailedAction: "assert_visible",
				Expected: "element present", Actual: `0 matches; candidates(role=listitem): "초등", "중학"`,
				Error: `step 5 (assert_visible): locate by=role role=listitem name="영어 1반 교사A": 0 matches`},
			`5단계에서 확인하려던 "영어 1반 교사A" 항목이 화면에 없습니다.`, KindService},

		{"script drift, locator 0 matches",
			model.Run{Outcome: model.OutcomeScriptDrift, FailedStep: 4, FailedAction: "click",
				Expected: "element present", Actual: `0 matches; candidates(role=button): "내보내기"`,
				Error: `step 4 (click): locate by=role role=button name="가져오기": 0 matches; candidates(role=button): "내보내기"`},
			`4단계에서 누르려던 "가져오기" 버튼이 화면에 없습니다.`, KindScript},

		{"script drift, long radio name is kept whole",
			model.Run{Outcome: model.OutcomeScriptDrift, FailedStep: 11, FailedAction: "click",
				Expected: "element present", Actual: "0 matches",
				Error: `step 11 (click): locate by=role role=radio name="[1단원 | 1. 받아올림이 없는 세 자리 수의 덧셈] 교과서": 0 matches`},
			`11단계에서 누르려던 "[1단원 | 1. 받아올림이 없는 세 자리 수의 덧셈] 교과서" 항목이 화면에 없습니다.`, KindScript},

		{"script drift, navigation",
			model.Run{Outcome: model.OutcomeScriptDrift, FailedStep: 3, FailedAction: "wait_url",
				Expected: "url contains /app/lms/class/0/dashboard", Actual: "https://staging.example.com/app/error"},
			"3단계 뒤에 예상한 화면으로 넘어가지 않았습니다.", KindScript},

		{"auth failure",
			model.Run{Outcome: model.OutcomeAuthFailure, Error: "login form rejected the credentials"},
			"검사 계정으로 로그인하지 못했습니다.", KindEnvironment},

		{"env failure, target down",
			model.Run{Outcome: model.OutcomeEnvFailure, Error: "transport failure: dial tcp: connection refused"},
			"검사 대상 서버에 연결하지 못했습니다.", KindEnvironment},

		{"env failure, allowlist",
			model.Run{Outcome: model.OutcomeEnvFailure, FailedStep: 2, FailedAction: "goto",
				Error: "goto: navigation outside the environment allowlist: https://evil.example"},
			"허용되지 않은 주소로 이동하려 했습니다.", KindEnvironment},

		{"data failure",
			model.Run{Outcome: model.OutcomeDataFailure, Error: "fixture missing"},
			"검사에 필요한 데이터가 준비돼 있지 않습니다.", KindData},

		{"lightpanda incompatible",
			model.Run{Outcome: model.OutcomeLightpandaIncompatible, FailedStep: 5, FailedAction: "expect_popup",
				Expected: "new page target with url containing /app", Actual: "no new page target; current url https://staging.example.com"},
			"가벼운 브라우저로는 판단할 수 없어 Chromium으로 다시 확인합니다.", KindBrowser},

		{"browser ambiguous",
			model.Run{Outcome: model.OutcomeBrowserAmbiguous, FailedStep: 7, FailedAction: "assert_request",
				Expected: "request GET /img.png status 200", Actual: "no matching request"},
			"가벼운 브라우저로는 판단할 수 없어 Chromium으로 다시 확인합니다.", KindBrowser},

		{"deployment not ready",
			model.Run{Outcome: model.OutcomeDeploymentNotReady},
			"아직 새 배포가 반영되지 않았습니다.", KindDeployment},

		{"qa flake",
			model.Run{Outcome: model.OutcomeQAFlake},
			"한 번 실패했다가 다시 실행하니 통과했습니다.", KindScript},

		{"oracle unknown",
			model.Run{Outcome: model.OutcomeOracleUnknown},
			"무엇이 맞는 동작인지 판단할 근거가 없습니다.", KindUnknown},

		{"needs review",
			model.Run{Outcome: model.OutcomeNeedsReview, Error: "runner internal failure"},
			"무엇이 맞는 동작인지 판단할 근거가 없습니다.", KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run
			c := ForRun(&r, nil)
			if c.Headline != tc.headline {
				t.Errorf("headline\n got %q\nwant %q", c.Headline, tc.headline)
			}
			if c.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", c.Kind, tc.kind)
			}
			if c.Next != nextByKind[tc.kind] {
				t.Errorf("next = %q, want %q", c.Next, nextByKind[tc.kind])
			}
			if c.Why == "" {
				t.Error("why is empty")
			}
			assertPlain(t, c.Headline)
		})
	}
}

// assertPlain enforces the L1 rules: one sentence, no selector syntax, no raw
// error text, no em dash, no jargon words.
func assertPlain(t *testing.T, h string) {
	t.Helper()
	if !strings.HasSuffix(h, ".") && !strings.HasSuffix(h, "다.") {
		t.Errorf("headline is not one finished sentence: %q", h)
	}
	if n := strings.Count(h, "다."); n > 1 {
		t.Errorf("headline has more than one sentence: %q", h)
	}
	for _, bad := range []string{"—", "assert", "locate", "role=", "by=", "css", "0 matches", "candidates(", "step ", "nth=", "$."} {
		if strings.Contains(h, bad) {
			t.Errorf("headline contains %q: %q", bad, h)
		}
	}
	if runeLen(h) > 70 {
		t.Errorf("headline too long (%d runes): %q", runeLen(h), h)
	}
}

func TestForRunPassIsEmpty(t *testing.T) {
	for _, r := range []model.Run{{Outcome: model.OutcomePass}, {}} {
		if c := ForRun(&r, nil); !c.Empty() {
			t.Errorf("outcome %q explained %q, want nothing", r.Outcome, c.Headline)
		}
	}
	if c := ForRun(nil, nil); !c.Empty() {
		t.Errorf("nil run explained %q", c.Headline)
	}
}

// Level 3 keeps the raw text that level 1 must never show.
func TestDetailKeepsRawText(t *testing.T) {
	r := model.Run{Outcome: model.OutcomeScriptDrift, FailedStep: 11, FailedAction: "click",
		Expected: "element present", Actual: `0 matches; candidates(role=radio): "AI 단원 진단평가"`,
		Error: `step 11 (click): locate by=role role=radio name="교과서": 0 matches`}
	c := ForRun(&r, nil)
	for _, want := range []string{"step 11 click", "expected: element present", `candidates(role=radio)`, `locate by=role`} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q:\n%s", want, c.Detail)
		}
	}
}

func TestForIncident(t *testing.T) {
	run := model.Run{Outcome: model.OutcomeAppFailure, FailedStep: 5, FailedAction: "assert_visible",
		Expected: "element present", Actual: "0 matches",
		Error: `step 5 (assert_visible): locate by=role role=listitem name="영어 1반 교사A": 0 matches`}
	in := &model.Incident{Kind: model.IncidentAppRegression, Title: "APP_REGRESSION: 교사 입장 딥링크"}
	if c := ForIncident(in, &run, nil); c.Headline != `5단계에서 확인하려던 "영어 1반 교사A" 항목이 화면에 없습니다.` {
		t.Errorf("incident headline = %q", c.Headline)
	}
	// No run left: the incident kind still yields a sentence.
	env := ForIncident(&model.Incident{Kind: model.IncidentEnvironment}, nil, nil)
	if env.Kind != KindEnvironment || env.Headline == "" {
		t.Errorf("environment incident = %+v", env)
	}
	app := ForIncident(&model.Incident{Kind: model.IncidentAppRegression}, nil, nil)
	if app.Kind != KindService || app.Headline == "" {
		t.Errorf("regression incident = %+v", app)
	}
	if !ForIncident(nil, nil, nil).Empty() {
		t.Error("nil incident explained something")
	}
}

func TestJosa(t *testing.T) {
	for _, tc := range []struct{ word, want string }{
		{"제출 완료", "가"}, {"제출 중", "이"}, {"버튼", "이"}, {"링크", "가"}, {"항목", "이"}, {"", "이"},
	} {
		if got := josa(tc.word, "이", "가"); got != tc.want {
			t.Errorf("josa(%q) = %q, want %q", tc.word, got, tc.want)
		}
	}
}

func TestObservationOracleWhy(t *testing.T) {
	r := model.Run{Outcome: model.OutcomeOracleUnknown}
	c := ForRun(&r, &model.Scenario{OracleSource: "observation"})
	if !strings.Contains(c.Why, "지금 동작을 그대로 기준") {
		t.Errorf("why = %q", c.Why)
	}
}
