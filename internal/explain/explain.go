// Package explain turns a failed run into one plain-Korean sentence that names
// the cause, plus the two deeper layers the dashboard, the incident files, the
// approval comment and the CLI reveal on demand.
//
// This package is the only place that words a failure. Every other package
// renders what it returns, so the dashboard, an incident markdown and `vigil
// run` can never describe the same run differently.
package explain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"vigil/internal/model"
)

// Kind is who the reader has to talk to about the cause.
const (
	KindService     = "service"     // the product behaved differently from its promise
	KindScript      = "script"      // the check itself is stale
	KindEnvironment = "environment" // target, network or account, not a product defect
	KindData        = "data"        // the numbers/values disagree, or fixtures are missing
	KindBrowser     = "browser"     // the light browser cannot decide; Chromium must
	KindDeployment  = "deployment"  // the build under test is not up yet
	KindUnknown     = "unknown"     // no basis for a verdict
)

// Cause is one failure explained in four layers.
type Cause struct {
	// Headline is ONE sentence in plain Korean that names the cause. It never
	// carries selector syntax, raw error text or English jargon.
	Headline string `json:"headline"`
	Kind     string `json:"kind"`
	// Why says what was expected and what happened, in words (1-2 sentences).
	Why string `json:"why"`
	// Detail is the raw technical text, verbatim, for the deepest layer.
	Detail string `json:"detail"`
	// Next is the one sentence that tells the reader what to do.
	Next string `json:"next"`
}

// Empty reports whether there is nothing to explain (a pass, or no run).
func (c Cause) Empty() bool { return c.Headline == "" }

// nextByKind maps the cause kind to the reader's next move.
var nextByKind = map[string]string{
	KindService:     "개발팀 확인이 필요합니다.",
	KindScript:      "AI가 검사 절차를 고치는 중입니다. 3회 실패하면 사람 확인으로 넘어옵니다.",
	KindEnvironment: "검사 환경 문제입니다. 서비스 결함이 아닙니다.",
	KindData:        "데이터를 준비한 뒤 다시 실행하세요.",
	KindBrowser:     "Chromium 재확인 결과를 기다리세요.",
	KindDeployment:  "배포가 반영되면 자동으로 다시 실행됩니다.",
	KindUnknown:     "무엇이 옳은 동작인지 사람이 정해 주어야 합니다.",
}

// ForRun explains one run. A pass (or a missing run) explains nothing.
func ForRun(r *model.Run, sc *model.Scenario) Cause {
	if r == nil || r.Outcome == "" || r.Outcome == model.OutcomePass {
		return Cause{}
	}
	c := headline(r)
	if c.Headline == "" {
		c = Cause{Headline: "검사가 끝까지 진행되지 못했습니다.", Kind: KindUnknown}
	}
	c.Why = why(r, sc, c.Kind)
	c.Detail = detail(r)
	c.Next = nextByKind[c.Kind]
	return c
}

// ForIncident explains the run behind an incident; when the run is gone the
// incident kind still yields a sentence.
func ForIncident(in *model.Incident, r *model.Run, sc *model.Scenario) Cause {
	if c := ForRun(r, sc); !c.Empty() {
		return c
	}
	if in == nil {
		return Cause{}
	}
	c := Cause{Headline: "이 검사가 약속과 다르게 동작했습니다.", Kind: KindService,
		Why: "이전에는 통과하던 검사가 이번 배포에서 실패해 문제로 기록됐습니다."}
	if in.Kind == model.IncidentEnvironment {
		c = Cause{Headline: "검사 환경 문제로 검사를 끝내지 못했습니다.", Kind: KindEnvironment,
			Why: "여러 검사가 같은 방식으로 멈춰 서비스 결함이 아니라 환경 문제로 묶었습니다."}
	}
	if in.Summary != "" {
		c.Detail = in.Summary
	}
	c.Next = nextByKind[c.Kind]
	return c
}

// ---- headline ---------------------------------------------------------------

func headline(r *model.Run) Cause {
	switch r.Outcome {
	case model.OutcomeAppFailure:
		return appFailure(r)
	case model.OutcomeScriptDrift:
		return scriptDrift(r)
	case model.OutcomeAuthFailure:
		return Cause{Headline: "검사 계정으로 로그인하지 못했습니다.", Kind: KindEnvironment}
	case model.OutcomeEnvFailure:
		if outsideAllowlist(r) {
			return Cause{Headline: "허용되지 않은 주소로 이동하려 했습니다.", Kind: KindEnvironment}
		}
		return Cause{Headline: "검사 대상 서버에 연결하지 못했습니다.", Kind: KindEnvironment}
	case model.OutcomeDataFailure:
		return Cause{Headline: "검사에 필요한 데이터가 준비돼 있지 않습니다.", Kind: KindData}
	case model.OutcomeLightpandaIncompatible, model.OutcomeBrowserAmbiguous:
		return Cause{Headline: "가벼운 브라우저로는 판단할 수 없어 Chromium으로 다시 확인합니다.", Kind: KindBrowser}
	case model.OutcomeDeploymentNotReady:
		return Cause{Headline: "아직 새 배포가 반영되지 않았습니다.", Kind: KindDeployment}
	case model.OutcomeQAFlake:
		return Cause{Headline: "한 번 실패했다가 다시 실행하니 통과했습니다.", Kind: KindScript}
	case model.OutcomeOracleUnknown, model.OutcomeNeedsReview:
		return Cause{Headline: "무엇이 맞는 동작인지 판단할 근거가 없습니다.", Kind: KindUnknown}
	}
	return Cause{}
}

// appFailure words a product defect from the step that failed.
func appFailure(r *model.Run) Cause {
	switch r.FailedAction {
	case "assert_text", "assert_no_text":
		if v, present, ok := textAssert(r.Expected); ok {
			if !present {
				return Cause{Headline: fmt.Sprintf("화면에서 %s 사라져야 하는데 그대로 보입니다.", quoted(v, valueLimit)+josa(v, "이", "가")), Kind: KindService}
			}
			if s := textSample(r.Actual); s != "" && runeLen(s) <= valueLimit {
				return Cause{Headline: fmt.Sprintf("화면에 %s 보여야 하는데 %s 보입니다.",
					quoted(v, valueLimit)+josa(v, "이", "가"), quoted(s, valueLimit)+josa(s, "이", "가")), Kind: KindService}
			}
			return Cause{Headline: fmt.Sprintf("화면에 %s 보여야 하는데 보이지 않습니다.", quoted(v, valueLimit)+josa(v, "이", "가")), Kind: KindService}
		}
	case "assert_not_visible":
		step := ""
		if r.FailedStep > 0 {
			step = fmt.Sprintf("%d단계에서 ", r.FailedStep)
		}
		if name := locatorName(r.Error); name != "" {
			noun := roleNoun(r.Error)
			return Cause{Headline: fmt.Sprintf("%s%s %s%s 사라져야 하는데 그대로 보입니다.", step, quoted(name, nameLimit), noun, josa(noun, "이", "가")), Kind: KindService}
		}
		return Cause{Headline: step + "사라져야 할 항목이 그대로 보입니다.", Kind: KindService}
	case "assert_count":
		if want, ok := countWanted(r.Expected); ok {
			if got, ok2 := countGot(r.Actual); ok2 {
				return Cause{Headline: fmt.Sprintf("개수가 %s개여야 하는데 화면에는 %s개입니다.", want, got), Kind: KindData}
			}
			return Cause{Headline: fmt.Sprintf("개수가 %s개여야 하는데 화면에서는 다르게 세었습니다.", want), Kind: KindData}
		}
	case "assert_data":
		api, ui := dataValues(r)
		if api != "" || ui != "" {
			return Cause{Headline: fmt.Sprintf("화면에 보이는 값은 %s, 서버 응답 값은 %s로 서로 다릅니다.",
				quoted(ui, valueLimit), quoted(api, valueLimit)), Kind: KindData}
		}
	case "assert_request":
		if status, ok := responseStatus(r.Actual); ok {
			if what := requestName(r.Expected); what != "" {
				return Cause{Headline: fmt.Sprintf("서버가 %s 요청에 %d으로 답했습니다.", what, status), Kind: KindService}
			}
			return Cause{Headline: fmt.Sprintf("서버가 요청에 %d으로 답했습니다.", status), Kind: KindService}
		}
		if what := requestName(r.Expected); what != "" {
			return Cause{Headline: fmt.Sprintf("%s 요청이 오지 않았습니다.", what), Kind: KindService}
		}
	case "assert_url", "wait_url", "goto":
		if r.FailedStep > 0 {
			return Cause{Headline: fmt.Sprintf("%d단계에서 예상한 화면이 아니라 다른 화면이 열렸습니다.", r.FailedStep), Kind: KindService}
		}
		return Cause{Headline: "예상한 화면이 아니라 다른 화면이 열렸습니다.", Kind: KindService}
	case "eval":
		if r.Expected != "" {
			got := r.Actual
			if got == "" {
				got = "없음"
			}
			return Cause{Headline: fmt.Sprintf("기준 값은 %s인데 화면에서 확인한 값은 %s입니다.",
				plain(r.Expected, valueLimit), plain(got, valueLimit)), Kind: KindService}
		}
	}
	if h, ok := missingElement(r, actionVerb(r.FailedAction)); ok {
		return Cause{Headline: h, Kind: KindService}
	}
	if r.FailedStep > 0 {
		return Cause{Headline: fmt.Sprintf("%d단계에서 확인한 화면이 약속한 결과와 다릅니다.", r.FailedStep), Kind: KindService}
	}
	return Cause{Headline: "화면이 약속한 결과와 다르게 동작했습니다.", Kind: KindService}
}

// scriptDrift words a stale check: the screen moved, the product may be fine.
func scriptDrift(r *model.Run) Cause {
	if h, ok := missingElement(r, actionVerb(r.FailedAction)); ok {
		return Cause{Headline: h, Kind: KindScript}
	}
	switch r.FailedAction {
	case "goto", "wait_url", "assert_url", "expect_popup":
		if r.FailedStep > 0 {
			return Cause{Headline: fmt.Sprintf("%d단계 뒤에 예상한 화면으로 넘어가지 않았습니다.", r.FailedStep), Kind: KindScript}
		}
		return Cause{Headline: "예상한 화면으로 넘어가지 않았습니다.", Kind: KindScript}
	}
	if r.FailedStep > 0 {
		return Cause{Headline: fmt.Sprintf("%d단계에서 화면이 검사 절차와 달라 진행하지 못했습니다.", r.FailedStep), Kind: KindScript}
	}
	return Cause{Headline: "화면 구성이 바뀌어 검사 절차대로 진행하지 못했습니다.", Kind: KindScript}
}

// missingElement words "the thing the step wanted is not usable on the screen":
// either it is not there at all, or it is there but not shown. The subject is
// the locator's human name only; the selector never appears.
func missingElement(r *model.Run, verb string) (string, bool) {
	hidden := isHidden(r)
	if !notFound(r) && !hidden {
		return "", false
	}
	tail := "화면에 없습니다."
	if hidden {
		tail = "화면에 표시되지 않습니다."
	}
	name := locatorName(r.Error)
	noun := roleNoun(r.Error)
	step := ""
	if r.FailedStep > 0 {
		step = fmt.Sprintf("%d단계에서 ", r.FailedStep)
	}
	if name == "" {
		return fmt.Sprintf("%s%s %s%s %s", step, verb, noun, josa(noun, "이", "가"), tail), true
	}
	q := quoted(name, nameLimit)
	return fmt.Sprintf("%s%s %s %s%s %s", step, verb, q, noun, josa(noun, "이", "가"), tail), true
}

// ---- why / detail -----------------------------------------------------------

func why(r *model.Run, sc *model.Scenario, kind string) string {
	switch kind {
	case KindBrowser:
		return "가벼운 브라우저는 새 탭이나 서버 응답 같은 동작을 그대로 재현하지 못합니다. 같은 절차를 Chromium에서 다시 실행한 결과로 판단합니다."
	case KindDeployment:
		return "확인해야 할 새 배포가 아직 검사 대상 서버에 올라오지 않아 검사를 미뤘습니다."
	case KindUnknown:
		if sc != nil && sc.OracleSource == "observation" {
			return "이 검사는 지금 동작을 그대로 기준으로 삼고 있어, 실패했다고 해서 결함이라고 단정할 수 없습니다."
		}
		return "이 검사에는 무엇이 옳은 동작인지 정해 둔 근거가 없어 결과를 판단하지 못했습니다."
	}
	if r.Outcome == model.OutcomeQAFlake {
		return "같은 검사를 한 번 더 실행했더니 통과했습니다. 대개 화면이나 서버가 잠깐 느렸을 때 생깁니다."
	}
	exp, act := expectedPhrase(r), actualPhrase(r)
	if exp != "" && act != "" {
		return "기대한 동작: " + exp + ". 실제 결과: " + act + "."
	}
	switch kind {
	case KindEnvironment:
		return "검사 대상 서버나 검사 계정 문제로 절차를 끝까지 진행하지 못했습니다."
	case KindData:
		return "검사가 쓰는 데이터가 준비돼 있지 않거나 예상과 달라 값을 비교하지 못했습니다."
	}
	if act != "" {
		return "실제로는 " + act + "이었습니다."
	}
	if exp != "" {
		return "검사는 " + exp + "을 기대했습니다."
	}
	return "검사가 어디에서 멈췄는지는 아래 기술 정보에서 확인할 수 있습니다."
}

// expectedPhrase / actualPhrase render the run's expected/actual as noun
// phrases ending in "것", so the sentence in why() is always grammatical.
func expectedPhrase(r *model.Run) string {
	e := r.Expected
	switch {
	case e == "":
		return ""
	case e == "element present" || e == "visible":
		return "그 항목이 화면에 보이는 것"
	case strings.HasPrefix(e, "not visible "):
		return "그 항목이 화면에서 사라지는 것"
	case strings.HasPrefix(e, "new page target"):
		return "새 탭이 열리는 것"
	case strings.HasPrefix(e, "url contains "), strings.HasPrefix(e, "url matches "):
		return "주소가 " + quoted(afterSpace(e, 2), valueLimit) + "로 바뀌는 것"
	case strings.HasPrefix(e, "count "):
		if want, ok := countWanted(e); ok {
			return "개수가 " + want + "개인 것"
		}
	case strings.HasPrefix(e, "request "):
		if what := requestName(e); what != "" {
			return what + " 요청이 정상으로 처리되는 것"
		}
	case strings.HasPrefix(e, "api "):
		if api, _ := dataValues(r); api != "" {
			return "화면 값이 서버 응답 값 " + quoted(api, valueLimit) + "와 같은 것"
		}
	}
	if v, present, ok := textAssert(e); ok {
		q := quoted(v, valueLimit)
		if present {
			return "화면에 " + q + josa(v, "이", "가") + " 보이는 것"
		}
		return "화면에서 " + q + josa(v, "이", "가") + " 사라지는 것"
	}
	return quoted(e, valueLimit) + "인 것"
}

func actualPhrase(r *model.Run) string {
	a := r.Actual
	switch {
	case a == "":
		return ""
	case strings.HasPrefix(a, "0 matches"):
		return "화면에서 찾지 못한 것"
	case strings.HasPrefix(a, "no new page target"):
		return "새 탭이 열리지 않은 것"
	case a == "no matching request":
		return "그런 요청이 오지 않은 것"
	case strings.HasPrefix(a, "still visible"):
		return "그대로 보인 것"
	case isHiddenText(a):
		return "화면에 있지만 보이지 않은 것"
	case strings.HasPrefix(a, "count "):
		if got, ok := countGot(a); ok {
			return got + "개인 것"
		}
	case strings.HasPrefix(a, "ui "):
		if _, ui := dataValues(r); ui != "" {
			return "화면 값이 " + quoted(ui, valueLimit) + "인 것"
		}
	}
	if status, ok := responseStatus(a); ok {
		return fmt.Sprintf("서버가 %d으로 답한 것", status)
	}
	if s := textSample(a); s != "" {
		return "화면에 " + quoted(s, valueLimit) + "만 보인 것"
	}
	return quoted(a, valueLimit) + "인 것"
}

// detail is the verbatim technical text of the failure (deepest layer).
func detail(r *model.Run) string {
	var b strings.Builder
	if r.FailedStep > 0 {
		fmt.Fprintf(&b, "step %d %s\n", r.FailedStep, r.FailedAction)
	}
	if r.Expected != "" {
		fmt.Fprintf(&b, "expected: %s\n", r.Expected)
	}
	if r.Actual != "" {
		fmt.Fprintf(&b, "actual: %s\n", r.Actual)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "%s\n", r.Error)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- parsing the runner's expected/actual/error shapes -----------------------

const (
	// valueLimit bounds a value quoted from expected/actual.
	valueLimit = 30
	// nameLimit bounds a locator's human name, which is the subject of the
	// sentence and stays readable longer than a bare value.
	nameLimit = 40
)

var (
	reLocName  = regexp.MustCompile(`\b(?:name|text)=("(?:[^"\\]|\\.)*")`)
	reRole     = regexp.MustCompile(`\brole=([A-Za-z]+)`)
	reTextAssn = regexp.MustCompile(`^text ("(?:[^"\\]|\\.)*") (present|absent)$`)
	reSample   = regexp.MustCompile(`^text sample: ("(?:[^"\\]|\\.)*")`)
	reCountExp = regexp.MustCompile(`(?:==|>=|<=) (\d+)`)
	reCountAct = regexp.MustCompile(`^count (\d+)`)
	reReqExp   = regexp.MustCompile(`^request [A-Z]+ (\S+)`)
	reRespAct  = regexp.MustCompile(`^[A-Z]+ ([1-5]\d\d) `)
	reDataVal  = regexp.MustCompile(` = ("(?:[^"\\]|\\.)*")$`)
)

// notFound reports whether the failure is "the element was not on the page".
func notFound(r *model.Run) bool {
	return strings.HasPrefix(r.Actual, "0 matches") || strings.Contains(r.Error, ": 0 matches")
}

// isHidden reports whether the element was found but not shown to the user.
func isHidden(r *model.Run) bool { return isHiddenText(r.Actual) }

func isHiddenText(actual string) bool {
	return strings.Contains(actual, "not visible") && !strings.HasPrefix(actual, "not visible")
}

func outsideAllowlist(r *model.Run) bool {
	return strings.Contains(r.Error, "outside") && strings.Contains(r.Error, "allowlist")
}

// locatorName pulls the human name a locator asked for out of the raw error.
// A css/id/test_id locator has no human name, only a selector, and a selector
// must never reach the sentence.
func locatorName(errText string) string {
	m := reLocName.FindStringSubmatch(errText)
	if m == nil {
		return ""
	}
	v, err := strconv.Unquote(m[1])
	if err != nil || looksLikeSelector(v) {
		return ""
	}
	return v
}

// looksLikeSelector rejects values that read as machine syntax rather than as
// something a person sees on the screen.
func looksLikeSelector(v string) bool {
	if v == "" {
		return true
	}
	// Screen text may contain brackets or a vertical bar ("[1단원 | 1. ...]"),
	// so only unmistakable markup/CSS shapes are refused here. A css/id/test_id
	// locator prints its selector as value=, which this package never reads.
	if strings.ContainsAny(v, "<>{}") || strings.Contains(v, "::") {
		return true
	}
	return strings.HasPrefix(v, ".") || strings.HasPrefix(v, "#")
}

// roleNoun turns the locator role into the everyday word for that thing.
func roleNoun(errText string) string {
	m := reRole.FindStringSubmatch(errText)
	role := ""
	if m != nil {
		role = m[1]
	}
	switch role {
	case "button":
		return "버튼"
	case "link":
		return "링크"
	case "textbox", "searchbox", "combobox", "spinbutton":
		return "입력란"
	case "heading":
		return "제목"
	case "img", "image":
		return "이미지"
	case "table", "grid":
		return "표"
	}
	return "항목"
}

// actionVerb is what the step was trying to do with the thing it could not find.
func actionVerb(action string) string {
	switch action {
	case "click":
		return "누르려던"
	case "fill", "type", "press":
		return "입력하려던"
	case "select":
		return "고르려던"
	case "hover":
		return "가리키려던"
	}
	return "확인하려던"
}

func textAssert(expected string) (value string, present, ok bool) {
	m := reTextAssn.FindStringSubmatch(expected)
	if m == nil {
		return "", false, false
	}
	v, err := strconv.Unquote(m[1])
	if err != nil {
		return "", false, false
	}
	return v, m[2] == "present", true
}

func textSample(actual string) string {
	m := reSample.FindStringSubmatch(actual)
	if m == nil {
		return ""
	}
	v, err := strconv.Unquote(m[1])
	if err != nil {
		return ""
	}
	return v
}

func countWanted(expected string) (string, bool) {
	m := reCountExp.FindStringSubmatch(expected)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func countGot(actual string) (string, bool) {
	m := reCountAct.FindStringSubmatch(actual)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// requestName is the readable part of a request assertion's URL fragment.
func requestName(expected string) string {
	m := reReqExp.FindStringSubmatch(expected)
	if m == nil {
		return ""
	}
	return plain(m[1], nameLimit)
}

func responseStatus(actual string) (int, bool) {
	m := reRespAct.FindStringSubmatch(actual)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 400 {
		return 0, false
	}
	return n, true
}

// dataValues returns the API value and the displayed value of an assert_data
// mismatch (expected holds the API side, actual the UI side).
func dataValues(r *model.Run) (api, ui string) {
	if m := reDataVal.FindStringSubmatch(r.Expected); m != nil {
		if v, err := strconv.Unquote(m[1]); err == nil {
			api = v
		}
	}
	if m := reDataVal.FindStringSubmatch(r.Actual); m != nil {
		if v, err := strconv.Unquote(m[1]); err == nil {
			ui = v
		}
	}
	return api, ui
}

// ---- text helpers ------------------------------------------------------------

// plain flattens a raw value to one line and trims it to n runes.
func plain(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " "))
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

func quoted(s string, n int) string { return `"` + plain(s, n) + `"` }

func runeLen(s string) int { return len([]rune(s)) }

func afterSpace(s string, fields int) string {
	parts := strings.SplitN(s, " ", fields+1)
	if len(parts) <= fields {
		return ""
	}
	return parts[fields]
}

// josa picks the Korean particle that fits the last syllable: 받침 present →
// with, absent → without. Non-Hangul endings take the "with" form, which is
// what Korean writing does for a quoted foreign word followed by 이/가.
func josa(word, withFinal, withoutFinal string) string {
	rs := []rune(strings.TrimSpace(word))
	if len(rs) == 0 {
		return withFinal
	}
	last := rs[len(rs)-1]
	if last >= 0xAC00 && last <= 0xD7A3 {
		if (last-0xAC00)%28 == 0 {
			return withoutFinal
		}
		return withFinal
	}
	if last >= '0' && last <= '9' {
		switch last {
		case '2', '4', '5', '9':
			return withoutFinal
		}
		return withFinal
	}
	return withFinal
}
