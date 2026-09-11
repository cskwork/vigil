package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"vigil/internal/dsl"
)

// ---- assert_data: displayed value vs. the API payload that produced it ---------
//
// The browser-dependent parts (response body, element text) are function fields on
// the session so the step logic is testable with a stubbed capture and no browser.

// responseBody fetches a captured response body via Network.getResponseBody.
func (s *session) responseBody(ctx context.Context, id network.RequestID) ([]byte, error) {
	if s.bodyOf != nil {
		return s.bodyOf(ctx, id)
	}
	var body []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		b, err := network.GetResponseBody(id).Do(c)
		body = b
		return err
	}))
	return body, err
}

// elementText resolves the locator (visibility not required: a count badge may be
// hidden behind an accordion) and returns its whitespace-normalised innerText.
func (s *session) elementText(ctx context.Context, l *dsl.Locator, ref string) (string, *stepErr) {
	if s.textOf != nil {
		return s.textOf(ctx, l, ref)
	}
	if _, err := s.resolve(ctx, l, ref, false); err != nil {
		return "", err
	}
	raw, err := evalJSON(ctx, `(function(){var el=`+jsRef(ref)+`; if(!el) return null;
		return (el.innerText !== undefined ? el.innerText : el.textContent || '').replace(/\s+/g,' ').trim()})()`)
	if err != nil {
		return "", s.classifyCDPErr(err, FailBrowserProtocol, "assert_data innerText")
	}
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil || text == nil {
		return "", fail(FailLocator, "assert_data: %s vanished before its text could be read", describeLocator(l))
	}
	return *text, nil
}

func (s *session) doAssertData(ctx context.Context, a *dsl.DataAssert, sr *StepResult, ref string) *stepErr {
	apiSrc := fmt.Sprintf("api %s %s", a.API.URLContains, a.API.JSONPath)
	uiSrc := "ui " + describeLocator(&a.UI)
	sr.Expected = fmt.Sprintf("%s == %s (%s)", uiSrc, apiSrc, a.Compare)
	if s.navigated && !s.cap.networkSeen() {
		s.res.Capabilities["network"] = false
		return fail(FailBrowserProtocol, "assert_data: network events unsupported on %s (no Network.* events observed after navigation)", s.spec.Browser)
	}
	match := func(e NetworkEvent) bool {
		if !strings.Contains(e.URL, a.API.URLContains) || e.Status == 0 || e.Failed {
			return false
		}
		return a.API.Method == "" || strings.EqualFold(e.Method, a.API.Method)
	}
	var found NetworkEvent
	var reqID network.RequestID
	err := poll(ctx, func() (bool, error) {
		ev, id, ok := s.cap.findLastRequest(match)
		if ok {
			found, reqID = ev, id
		}
		return ok, nil
	})
	if err != nil {
		if s.runCtx.Err() != nil {
			return fail(FailTimeout, "assert_data: run timeout exceeded").with(sr.Expected, "no captured response for "+apiSrc)
		}
		actual := "no captured response matching " + a.API.URLContains
		if a.API.Method != "" {
			actual += " " + strings.ToUpper(a.API.Method)
		}
		return fail(FailAssertion, "assert_data: %s", actual).with(sr.Expected, actual)
	}
	body, berr := s.responseBody(ctx, reqID)
	if berr != nil {
		return s.classifyCDPErr(berr, FailBrowserProtocol, "assert_data body")
	}
	var doc any
	if jerr := json.Unmarshal(body, &doc); jerr != nil {
		actual := fmt.Sprintf("%s: response %s %d is not JSON (%s)", apiSrc, found.Method, found.Status, truncate(string(body), 120))
		return fail(FailAssertion, "assert_data: %s", actual).with(sr.Expected, actual)
	}
	apiVal, perr := jsonPath(doc, a.API.JSONPath)
	if perr != nil {
		actual := fmt.Sprintf("%s unresolvable: %v", apiSrc, perr)
		return fail(FailAssertion, "assert_data: %s", actual).with(sr.Expected, actual)
	}
	apiText := dataText(apiVal)

	uiText, uerr := s.elementText(ctx, &a.UI, ref)
	if uerr != nil {
		return uerr
	}
	if a.UIRegex != "" {
		m := regexp.MustCompile(a.UIRegex).FindStringSubmatch(uiText)
		if m == nil {
			actual := fmt.Sprintf("%s text %q has no match for ui_regex %s", uiSrc, truncate(uiText, 200), a.UIRegex)
			return fail(FailAssertion, "assert_data: %s", actual).with(fmt.Sprintf("%s = %q", apiSrc, apiText), actual)
		}
		uiText = m[0]
		if len(m) > 1 && m[1] != "" {
			uiText = m[1]
		}
	}
	expected := fmt.Sprintf("%s = %q", apiSrc, apiText)
	actual := fmt.Sprintf("%s = %q", uiSrc, truncate(uiText, 200))
	sr.Expected, sr.Actual = expected, actual
	ok, reason := compareData(a.Compare, apiText, uiText)
	if ok {
		return nil
	}
	return fail(FailAssertion, "assert_data (%s): %s; %s; %s", a.Compare, reason, expected, actual).with(expected, actual)
}

// compareData applies one compare mode; reason explains a mismatch.
func compareData(mode, apiText, uiText string) (bool, string) {
	switch mode {
	case "text":
		if strings.TrimSpace(apiText) == strings.TrimSpace(uiText) {
			return true, ""
		}
		return false, "text differs"
	case "number":
		av, aerr := parseNumber(apiText)
		uv, uerr := parseNumber(uiText)
		switch {
		case aerr != nil:
			return false, "api value is not a number"
		case uerr != nil:
			return false, "ui value is not a number"
		case math.Abs(av-uv) <= 1e-9:
			return true, ""
		}
		return false, fmt.Sprintf("numbers differ by %g", uv-av)
	case "contains":
		if strings.Contains(uiText, strings.TrimSpace(apiText)) {
			return true, ""
		}
		return false, "ui text does not contain the api value"
	}
	return false, "unknown compare mode " + mode
}

var numberRe = regexp.MustCompile(`[-+]?(\d+(\.\d+)?|\.\d+)([eE][-+]?\d+)?`)

// parseNumber extracts the first number from a displayed value: thousands
// separators, currency signs and units ("1,234명", "₩12,000", "45.5 %") are stripped.
func parseNumber(s string) (float64, error) {
	s = strings.NewReplacer(",", "", " ", "", " ", "").Replace(strings.TrimSpace(s))
	m := numberRe.FindString(s)
	if m == "" {
		return 0, fmt.Errorf("no number in %q", s)
	}
	return strconv.ParseFloat(m, 64)
}

// dataText renders a JSON value the way a UI would display it: scalars plain,
// numbers without a float artefact, null empty, containers as compact JSON.
func dataText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// jsonPath evaluates a minimal, dependency-free path: `$`, `.key`, `[N]`, `[*]`
// (first element) and `["key with spaces"]`. Each hop must resolve; the error names
// the segment that did not.
func jsonPath(doc any, path string) (any, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("path must start with $")
	}
	segs, err := splitJSONPath(path[1:])
	if err != nil {
		return nil, err
	}
	cur := doc
	for _, seg := range segs {
		switch seg.kind {
		case "key":
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: parent is not an object", seg.text)
			}
			v, ok := m[seg.key]
			if !ok {
				return nil, fmt.Errorf("%s: key not found", seg.text)
			}
			cur = v
		case "index", "first":
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: parent is not an array", seg.text)
			}
			if len(arr) == 0 {
				return nil, fmt.Errorf("%s: array is empty", seg.text)
			}
			if seg.kind == "index" {
				if seg.index >= len(arr) {
					return nil, fmt.Errorf("%s: index out of range (len %d)", seg.text, len(arr))
				}
				cur = arr[seg.index]
			} else {
				cur = arr[0]
			}
		}
	}
	return cur, nil
}

type pathSeg struct {
	kind  string // key | index | first
	key   string
	index int
	text  string
}

func splitJSONPath(rest string) ([]pathSeg, error) {
	var segs []pathSeg
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			end := strings.IndexAny(rest[1:], ".[")
			var name string
			if end < 0 {
				name, rest = rest[1:], ""
			} else {
				name, rest = rest[1:1+end], rest[1+end:]
			}
			if name == "" {
				return nil, fmt.Errorf("empty key segment")
			}
			segs = append(segs, pathSeg{kind: "key", key: name, text: "." + name})
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated [ in %q", rest)
			}
			inner, text := rest[1:end], rest[:end+1]
			rest = rest[end+1:]
			switch {
			case inner == "*":
				segs = append(segs, pathSeg{kind: "first", text: text})
			case len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0]:
				segs = append(segs, pathSeg{kind: "key", key: inner[1 : len(inner)-1], text: text})
			default:
				n, err := strconv.Atoi(inner)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("bad index %q", text)
				}
				segs = append(segs, pathSeg{kind: "index", index: n, text: text})
			}
		default:
			return nil, fmt.Errorf("unexpected %q in path", rest)
		}
	}
	return segs, nil
}
