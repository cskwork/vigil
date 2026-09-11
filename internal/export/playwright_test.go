package export

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/dsl"
)

var update = flag.Bool("update", false, "rewrite golden files")

func loadFlow(t *testing.T, name string) *dsl.Flow {
	t.Helper()
	f, err := dsl.ParseFlowFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestPlaywrightGolden renders the representative scenario (every step type, a
// `uses` flow, an inline use_flow, an unknown flow, scenario asserts, a popup) and
// compares it with testdata/all-steps.spec.ts. Run with -update to regenerate.
func TestPlaywrightGolden(t *testing.T) {
	sc, err := dsl.ParseFile(filepath.Join("testdata", "all-steps.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	flows := map[string]*dsl.Flow{
		"login-as-student": loadFlow(t, "flow-login.yaml"),
		"select-course":    loadFlow(t, "flow-select.yaml"),
	}
	// the scenario itself is a valid vigil script (the unknown flow is the only deliberate gap)
	known := map[string]bool{"login-as-student": true, "select-course": true, "missing-flow": true}
	if err := sc.Validate(known); err != nil {
		t.Fatal(err)
	}
	got := Playwright(sc, Options{Env: "stg", BaseURL: "https://stg.example.com", Flows: flows})
	golden := filepath.Join("testdata", "all-steps.spec.ts")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/export -update` to create it)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch; run `go test ./internal/export -update` and review the diff.\n--- got ---\n%s", got)
	}

	// structural guarantees independent of the golden text
	s := string(got)
	for _, must := range []string{
		"import { test, expect } from '@playwright/test';",
		"const BASE_URL = process.env.BASE_URL ?? 'https://stg.example.com';",
		"// scenario: export.all-steps\n// version: 3\n",
		"// env: stg (https://stg.example.com)",
		"YAML DSL is canonical",
		"// step 1: 로그인 페이지 (flow login-as-student)",
		"page.on('console'",
		"page.on('response'",
		"expect(consoleErrors, 'no_uncaught_console_error').toEqual([]);",
		"expect(http5xx, 'no_http_5xx').toEqual([]);",
		"expect(http4xx, 'no_http_4xx_on').toEqual([]);",
		"context.waitForEvent('page'",
		"// TODO test.fixme: unknown flow \"missing-flow\"",
		"new RegExp(\"^https://.*/courses/[0-9]+$\")",
		"new RegExp(\"/courses\\\\?x=1\")",
	} {
		if !strings.Contains(s, must) {
			t.Errorf("output lacks %q", must)
		}
	}
	if strings.Count(s, "test(") != 1 {
		t.Errorf("expected exactly one test(), got %d", strings.Count(s, "test("))
	}
	// every emitted line inside the test body is either a comment or a statement
	for _, ln := range strings.Split(s, "\n") {
		tl := strings.TrimSpace(ln)
		if tl == "" || strings.HasPrefix(tl, "//") || strings.HasPrefix(tl, "import ") || strings.HasPrefix(tl, "test(") || tl == "});" {
			continue
		}
		if i := strings.LastIndex(tl, "; //"); i >= 0 { // trailing comment
			tl = tl[:i+1]
		}
		if !strings.HasSuffix(tl, ";") && !strings.HasSuffix(tl, "{") && !strings.HasSuffix(tl, "*/") && !strings.HasSuffix(tl, "=> {") {
			t.Errorf("suspicious line: %q", ln)
		}
	}
}

func TestPlaywrightWithoutAssertsOrPopup(t *testing.T) {
	sc, err := dsl.Parse([]byte("scenario:\n  id: min\n  version: 1\nsteps:\n  - goto: /\n  - assert_text: { value: hi }\noracle:\n  source: spec\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(Playwright(sc, Options{Env: "default", BaseURL: "http://localhost:3000"}))
	for _, no := range []string{"consoleErrors", "http5xx", "http4xx", "let active", "firstNumber"} {
		if strings.Contains(s, no) {
			t.Errorf("minimal export must not contain %q:\n%s", no, s)
		}
	}
	if !strings.Contains(s, "await page.goto(BASE_URL + '/');") || !strings.Contains(s, "await expect(page.locator('body')).toContainText('hi');") {
		t.Errorf("unexpected body:\n%s", s)
	}
	if !strings.Contains(s, "test('min', async") {
		t.Errorf("title-less scenario must use the id as test name:\n%s", s)
	}
}

func TestLocatorMapping(t *testing.T) {
	n := 2
	for _, tc := range []struct {
		name string
		loc  dsl.Locator
		want string
		note string
	}{
		{"test_id", dsl.Locator{By: "test_id", Value: "submit"}, "page.getByTestId('submit')", ""},
		{"role", dsl.Locator{By: "role", Role: "button", Name: "저장", Exact: true}, "page.getByRole('button', { name: '저장', exact: true })", ""},
		{"role no name", dsl.Locator{By: "role", Role: "dialog"}, "page.getByRole('dialog')", ""},
		{"role unknown", dsl.Locator{By: "role", Role: "fancy", Name: "x"}, "page.locator('[role=\"fancy\"]').filter({ hasText: 'x' })", "TODO test.fixme: role fancy is not an ARIA role known to getByRole; using a [role=...] css locator"},
		{"label", dsl.Locator{By: "label", Name: "아이디"}, "page.getByLabel('아이디').or(page.getByPlaceholder('아이디'))", "label locator: getByPlaceholder is the fallback because vigil also matches placeholder text"},
		{"label exact via text", dsl.Locator{By: "label", Text: "Email", Exact: true}, "page.getByLabel('Email', { exact: true }).or(page.getByPlaceholder('Email', { exact: true }))", "label locator: getByPlaceholder is the fallback because vigil also matches placeholder text"},
		{"id", dsl.Locator{By: "id", Value: "main-form"}, "page.locator('#main-form')", ""},
		{"id odd", dsl.Locator{By: "id", Value: "a b\"c"}, `page.locator('[id="a b\\"c"]')`, ""},
		{"text", dsl.Locator{By: "text", Text: "학습 시작"}, "page.getByText('학습 시작')", ""},
		{"text exact via value", dsl.Locator{By: "text", Value: "Go", Exact: true}, "page.getByText('Go', { exact: true })", ""},
		{"href", dsl.Locator{By: "href", Value: "/logout?x=\"1\""}, `page.locator('a[href*="/logout?x=\\"1\\""]')`, ""},
		{"href exact", dsl.Locator{By: "href", Value: "/x", Exact: true}, `page.locator('a[href="/x"]')`, ""},
		{"css", dsl.Locator{By: "css", Value: "ul > li:nth-child(2)"}, "page.locator('ul > li:nth-child(2)')", ""},
		{"nth", dsl.Locator{By: "css", Value: "li", Nth: n}, "page.locator('li').nth(2)", ""},
		{"by unset role", dsl.Locator{Role: "link", Name: "홈"}, "page.getByRole('link', { name: '홈' })", ""},
		{"by unset text", dsl.Locator{Text: "x"}, "page.getByText('x')", ""},
		{"by unset name", dsl.Locator{Name: "x"}, "page.getByLabel('x').or(page.getByPlaceholder('x'))", "label locator: getByPlaceholder is the fallback because vigil also matches placeholder text"},
		{"by unset value", dsl.Locator{Value: "submit-btn"}, "page.getByTestId('submit-btn')", "locator.by unset: vigil also tries #id and a css selector for this value"},
		{"unknown by", dsl.Locator{By: "xpath", Value: "//a"}, "page.locator('body')", "TODO test.fixme: unknown locator by xpath"},
	} {
		l := tc.loc
		got, note := Locator("page", &l)
		if got != tc.want || note != tc.note {
			t.Errorf("%s:\n  got  %s | %s\n  want %s | %s", tc.name, got, note, tc.want, tc.note)
		}
		if strings.Contains(got, "/*") || strings.Contains(got, "//") {
			t.Errorf("%s: locator expression must not embed comments: %s", tc.name, got)
		}
	}
	if got, _ := Locator("active", &dsl.Locator{By: "css", Value: "a"}); got != "active.locator('a')" {
		t.Errorf("active page var: %s", got)
	}
	if got, note := Locator("page", nil); got != "page.locator('body')" || !strings.Contains(note, "TODO test.fixme") {
		t.Errorf("nil locator must emit a fixme: %s | %s", got, note)
	}
}

func TestEscaping(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
		{"line\nbreak\r\ttab", `'line\nbreak\r\ttab'`},
		{"\u2028sep\u2029", `'\u2028sep\u2029'`},
		{"nul\x00", `'nul\u0000'`},
		{"한글 “quotes”", "'한글 “quotes”'"},
		{"</script>", "'</script>'"},
	} {
		if got := tsString(tc.in); got != tc.want {
			t.Errorf("tsString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{`^/a\.b$`, `new RegExp("^/a\\.b$")`},
		{"quote\"inside", `new RegExp("quote\"inside")`},
		{"<tag>&", `new RegExp("<tag>&")`},
		{"\u2028", `new RegExp("\u2028")`},
	} {
		if got := tsRegexp(tc.in); got != tc.want {
			t.Errorf("tsRegexp(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := urlRegexp(&dsl.URLAssert{Contains: "/a?b=1.2"}); got != `new RegExp("/a\\?b=1\\.2")` {
		t.Errorf("urlRegexp contains = %s", got)
	}
	if got := oneLine("multi\nline */ end"); got != "multi line * / end" {
		t.Errorf("oneLine = %q", got)
	}
	if got := jsLiteral(map[string]any{"b": 1.0, "a": "x"}); got != `{"a":"x","b":1}` {
		t.Errorf("jsLiteral = %s", got)
	}
}

func TestJSONPathExpr(t *testing.T) {
	for _, tc := range []struct{ path, want, wantErr string }{
		{"$", "body", ""},
		{"$.data.totalCount", "body?.['data']?.['totalCount']", ""},
		{"$.data.items[1].name", "body?.['data']?.['items']?.[1]?.['name']", ""},
		{"$.data.items[*].score", "body?.['data']?.['items']?.[0]?.['score']", ""},
		{`$.data["key with spaces"]`, "body?.['data']?.['key with spaces']", ""},
		{"$.data['it\\'s']", `body?.['data']?.['it\\\'s']`, ""},
		{"data.x", "", "must start with $"},
		{"$.data[abc]", "", "bad index"},
		{"$.data[0", "", "unterminated"},
		{"$..x", "", "empty key"},
		{"$x", "", "unexpected"},
	} {
		got, err := jsonPathExpr("body", tc.path)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.path, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %s (%v), want %s", tc.path, got, err, tc.want)
		}
	}
}

func TestFileName(t *testing.T) {
	if n, err := FileName("a.b", FormatPlaywright); err != nil || n != "a.b.spec.ts" {
		t.Fatalf("FileName = %s, %v", n, err)
	}
	if _, err := FileName("a", "cypress"); err == nil || !strings.Contains(err.Error(), "unsupported export format") {
		t.Fatalf("unsupported format err = %v", err)
	}
}
