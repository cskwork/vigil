package agent

import (
	"strings"
	"testing"

	"vigil/internal/dsl"
)

func TestParseResultFindingsNormalised(t *testing.T) {
	text := "done\n```yaml vigil-result\ndecision: NO_NEW_COVERAGE\nevidence: looked\nfindings:\n" +
		"  - kind: DATA_MISMATCH\n    where: /students card\n    expected: \"api /api/students $.data.totalCount = 42\"\n    actual: \"ui .total-count = 41\"\n    evidence: \"GET /api/students; step-2.png\"\n" +
		"  - kind: typo\n    where: /students\n    expected: 학생\n    actual: 학셍\n" +
		"  - kind: display\n" +
		"  - \"footer year is 2025\"\n" +
		"```\n"
	r, err := ParseResult(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Findings) != 3 {
		t.Fatalf("findings = %+v", r.Findings)
	}
	if f := r.Findings[0]; f.Kind != FindingDataMismatch || f.Where != "/students card" || f.Evidence != "GET /api/students; step-2.png" {
		t.Fatalf("first = %+v", f)
	}
	if f := r.Findings[1]; f.Kind != FindingDisplay || !strings.HasPrefix(f.Evidence, "kind: typo") {
		t.Fatalf("unknown kind not normalised: %+v", f)
	}
	if f := r.Findings[2]; f.Kind != FindingDisplay || f.Evidence != "footer year is 2025" {
		t.Fatalf("string finding = %+v", f)
	}
	// findings given as prose do not break the block
	r, err = ParseResult("```yaml vigil-result\ndecision: NEEDS_REVIEW\nfindings: none seen\n```")
	if err != nil || len(r.Findings) != 1 || r.Findings[0].Kind != FindingDisplay || r.Findings[0].Evidence != "none seen" {
		t.Fatalf("prose findings: r=%+v err=%v", r, err)
	}
}

func TestTaskPromptDomainRulesBounded(t *testing.T) {
	req := Request{Task: TaskDiscover, Target: "https://t.example.com"}
	if p := TaskPrompt(req); strings.Contains(p, "## Domain rules") {
		t.Fatal("domain section rendered without rules")
	}
	req.DomainRules = "- DATA-1 counts equal list lengths"
	p := TaskPrompt(req)
	if !strings.Contains(p, "## Domain rules") || !strings.Contains(p, "DATA-1 counts equal list lengths") {
		t.Fatalf("domain section missing: %s", p)
	}
	if strings.Count(p, "DATA-1 counts equal list lengths") != 1 {
		t.Fatal("domain rules must not be duplicated inside the YAML request dump")
	}
	if !strings.Contains(SystemPrompt(), "## Data analyst duties") || !strings.Contains(SystemPrompt(), "findings:") || !strings.Contains(SystemPrompt(), "assert_data") {
		t.Fatal("system prompt lacks the data analyst section or the findings contract")
	}
	big := strings.Repeat("- RULE-1 한글 규칙 텍스트\n", 2000)
	got := BoundDomainRules(big)
	if len(got) > MaxDomainFileBytes+len(DomainTruncatedMarker) || !strings.HasSuffix(got, DomainTruncatedMarker) || !utf8Valid(got) {
		t.Fatalf("bounded len=%d suffix=%q", len(got), got[len(got)-20:])
	}
	if BoundDomainRules(" small ") != "small" {
		t.Fatal("small rules must be trimmed only")
	}
	if s, err := ReadDomainFile(""); err != nil || s != "" {
		t.Fatalf("empty path: %q %v", s, err)
	}
	if _, err := ReadDomainFile("/nonexistent/domain.md"); err == nil {
		t.Fatal("missing file must error")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// H-2: a bound (max length, min count, range) must be asserted as a boolean, so
// a healthy value cannot fail the way `eval: expect: 40` did on a 37-char title.
func TestSystemPromptTeachesBoundAssertions(t *testing.T) {
	p := SystemPrompt()
	for _, want := range []string{
		"Assert the bound, not the observed number",
		`- eval: { script: "document.querySelector('#title').value.length <= 40", expect: true }`,
		"Never write expect: <the number you happened to observe>",
		"assert_count uses max:/min: over equals:",
		"compare: number compares the UI against an API value, never against a literal",
		"an observed value inside the bound is not a finding",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// The candidate below is the shape a real GLM run produced: an unquoted
// oracle.note that contains a Korean label ("검색어: 주간보고"). YAML reads the
// inner colon as a mapping, so the whole candidate is thrown away.
const brokenNoteCandidate = `scenario:
  id: memo-search
  version: 1
  title: 메모 검색
covers:
  feature: user-qa-1
  routes: [/]
steps:
  - goto: /
  - assert_text: { value: "검색 결과" }
assert:
  no_uncaught_console_error: true
oracle:
  source: observation
  source_feature: user-qa-1
  note: 첨부 화면은 검색어: 주간보고와 결과 3건을 보여 준다
`

func TestCompileRequestNamesTheExactFault(t *testing.T) {
	msg := compileRequest([]string{brokenNoteCandidate})
	if !strings.Contains(msg, "candidate 1:") {
		t.Fatalf("compile request does not name the candidate:\n%s", msg)
	}
	if !strings.Contains(msg, "mapping values") {
		t.Fatalf("compile request does not carry the parser error:\n%s", msg)
	}
	if !strings.Contains(msg, "re-emit EVERY candidate") {
		t.Fatalf("compile request does not ask for every candidate back:\n%s", msg)
	}
}

func TestCompileRequestWithoutFaultsIsThePlainMessage(t *testing.T) {
	good := strings.Replace(brokenNoteCandidate, "note: 첨부 화면은 검색어: 주간보고와 결과 3건을 보여 준다",
		`note: "첨부 화면은 검색어: 주간보고와 결과 3건을 보여 준다"`, 1)
	if _, err := dsl.Parse([]byte(good)); err != nil {
		t.Fatalf("quoted note still does not parse: %v", err)
	}
	if msg := compileRequest([]string{good}); msg != CompileMessage {
		t.Fatalf("a parseable candidate must not add faults:\n%s", msg)
	}
}

func TestSystemPromptTeachesTheQuotingRule(t *testing.T) {
	p := SystemPrompt()
	for _, want := range []string{"YAML quoting", "oracle.note", "검색어: 주간보고"} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt is missing %q", want)
		}
	}
}
