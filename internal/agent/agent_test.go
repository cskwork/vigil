package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/dsl"
)

// fixture: a pi --mode json stream with a tool call, a 429 error message and a final answer.
const transcriptFixture = `{"type":"session","version":"1","id":"abc"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"task"}]}}
{"type":"tool_execution_start","toolCallId":"t1","toolName":"agent_browser","args":{"args":["open","https://www.example.com/app/entry"]}}
{"type":"tool_execution_end","toolCallId":"t1","toolName":"agent_browser","result":{"content":[{"type":"text","text":"ok https://cdn.example.net/asset.js"}]},"isError":false}
{"type":"tool_execution_start","toolCallId":"t2","toolName":"agent_browser","args":{"args":["open","https://evil.example.org/phish"]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"partial"}],"stopReason":"error","errorMessage":"{\"code\":\"1302\",\"message\":\"Rate limit reached for requests\"}"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Done.\n\n` + "```yaml vigil-result" + `\ndecision: new_script\nevidence: |\n  opened entry page password: hunter22secret\ncoverage_delta: entry tabs\noracle_provenance: spec\nscript_candidates:\n  - |\n    scenario:\n      id: entry-tabs\n      version: 1\n    steps:\n      - goto: /app/entry\n      - assert_text: { value: \"교사 입장\" }\n    oracle:\n      source: spec\nvisited_urls: [\"https://www.example.com/app/entry\"]\n` + "```" + `"}],"stopReason":"stop"}}
{"type":"agent_end"}
`

func TestParseTranscriptAndResult(t *testing.T) {
	tr, err := ParseTranscript(strings.NewReader(transcriptFixture))
	if err != nil {
		t.Fatal(err)
	}
	if tr.ToolCalls != 2 {
		t.Fatalf("tool calls = %d, want 2", tr.ToolCalls)
	}
	wantURLs := []string{"https://www.example.com/app/entry", "https://evil.example.org/phish"}
	if !reflect.DeepEqual(tr.ToolURLs, wantURLs) {
		t.Fatalf("tool urls = %v", tr.ToolURLs)
	}
	if tr.ErrorMessage == "" || !transientRe.MatchString(tr.ErrorMessage) {
		t.Fatalf("429 error not captured/classified: %q", tr.ErrorMessage)
	}
	res, err := ParseResult(tr.AssistantText)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision != DecisionNewScript {
		t.Fatalf("decision = %q", res.Decision)
	}
	if len(res.ScriptCandidates) != 1 || !strings.Contains(res.ScriptCandidates[0], "id: entry-tabs") {
		t.Fatalf("candidates = %v", res.ScriptCandidates)
	}
	if v := HostViolations(append(res.VisitedURLs, tr.ToolURLs...), []string{"example.com"}); !reflect.DeepEqual(v, []string{"https://evil.example.org/phish"}) {
		t.Fatalf("violations = %v", v)
	}
}

func TestExtractResultBlockVariants(t *testing.T) {
	cases := map[string]string{
		"named":      "text\n```yaml vigil-result\ndecision: NO_NEW_COVERAGE\n```\n",
		"plain yaml": "```yaml\ndecision: NO_NEW_COVERAGE\nevidence: x\n```",
		"json":       "```json\n{\"decision\": \"NO_NEW_COVERAGE\", \"evidence\": \"x\"}\n```",
		"bare":       "```\ndecision: NO_NEW_COVERAGE\n```",
		"last wins":  "```yaml\ndecision: APP_FAILURE\n```\nthen\n```yaml vigil-result\ndecision: NO_NEW_COVERAGE\n```",
	}
	for name, text := range cases {
		res, err := ParseResult(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Decision != DecisionNoNewCoverage {
			t.Fatalf("%s: decision %q", name, res.Decision)
		}
	}
	if _, err := ParseResult("no block here"); err == nil {
		t.Fatal("expected error without block")
	}
	if _, err := ParseResult("```yaml vigil-result\ndecision: MAYBE\n```"); err == nil {
		t.Fatal("expected unknown decision error")
	}
}

func TestRedactor(t *testing.T) {
	personas := map[string]config.Persona{"qa_student": {Username: "stu01", Password: "hunter22secret"}}
	env := []string{"Z_AI_API_KEY=zai-1234567890abcdef", "PATH=/usr/bin", "SHORT_KEY=ab"}
	r := RedactorFor(personas, env)
	in := "login with hunter22secret and key zai-1234567890abcdef; Authorization: Bearer tok_abcdef123456 api_key=\"XYZ12345\" short ab"
	out := r.Redact(in)
	for _, bad := range []string{"hunter22secret", "zai-1234567890abcdef", "tok_abcdef123456", "XYZ12345"} {
		if strings.Contains(out, bad) {
			t.Fatalf("secret %q leaked: %s", bad, out)
		}
	}
	if !strings.Contains(out, "short ab") {
		t.Fatalf("short values must not be shredded: %s", out)
	}
	res := r.RedactResult(&Result{Evidence: "pw hunter22secret", ScriptCandidates: []string{"fill: {input: hunter22secret}"}, Observed: map[string]string{"c": "zai-1234567890abcdef"}})
	if strings.Contains(res.Evidence, "hunter22") || strings.Contains(res.ScriptCandidates[0], "hunter22") || strings.Contains(res.Observed["c"], "zai-") {
		t.Fatalf("result not redacted: %+v", res)
	}
}

func TestProfileJSON(t *testing.T) {
	b := ProfileJSON("/home/u")
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["extends"] != "default" || doc["allow_gpu"] != true {
		t.Fatalf("profile = %s", b)
	}
	rules, _ := doc["unsafe_macos_seatbelt_rules"].([]any)
	if len(rules) < 5 || !strings.Contains(string(b), `(allow mach-register)`) || !strings.Contains(string(b), `/home/u`) {
		t.Fatalf("profile rules = %s", b)
	}
}

func TestSandboxArgv(t *testing.T) {
	s := &Sandbox{
		Mode: SandboxNonoWrap, Binary: "/opt/homebrew/bin/nono", Silent: true, Profile: "/ev/nono-profile.json", LogFile: "/ev/nono.log",
		Allow: []string{"/ev", "/home/u/.pi"}, Read: []string{"/node/v22"}, ReadFiles: []string{"/home/u/pi-setup/models.json"},
		SocketDirsBind: []string{"/private/tmp/piab-501"}, AllowedDomains: []string{"ignored-in-wrap"}, GPU: true,
	}
	got := s.Argv("/node/v22/bin/pi", "-p", "task")
	want := []string{"/opt/homebrew/bin/nono", "wrap", "-s", "-p", "/ev/nono-profile.json", "--log-file", "/ev/nono.log", "--allow-cwd",
		"--allow", "/ev", "--allow", "/home/u/.pi", "--read", "/node/v22", "--read-file", "/home/u/pi-setup/models.json",
		"--allow-unix-socket-dir-bind", "/private/tmp/piab-501", "--allow-gpu", "--", "/node/v22/bin/pi", "-p", "task"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrap argv:\n got %q\nwant %q", got, want)
	}
	s.Mode = SandboxNonoRun
	got = s.Argv("/node/v22/bin/pi")
	if got[1] != "run" || !contains(got, "--allow-domain") || !contains(got, "--diagnostics-json") {
		t.Fatalf("run argv missing supervised flags: %q", got)
	}
	none := &Sandbox{Mode: SandboxNone}
	if got := none.Argv("pi", "-p"); !reflect.DeepEqual(got, []string{"pi", "-p"}) {
		t.Fatalf("none argv = %q", got)
	}
	for _, a := range got {
		if strings.Contains(a, "/Users/") {
			t.Fatalf("argv must not embed a literal home path: %s", a)
		}
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func TestSessionNameAndModelSplit(t *testing.T) {
	n := sessionName("Training Entry/Page!!", time.Unix(1700000000, 0))
	if n != "vigil-training-entry-page-1700000000" {
		t.Fatalf("session name = %s", n)
	}
	p, m := splitModel("zai/glm-5.3-flash")
	if p != "zai" || m != "glm-5.3-flash" {
		t.Fatalf("split = %s %s", p, m)
	}
}

func TestPiArgvShape(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Thinking = "high"
	cfg.Agent.MaxTurns = 5
	p := &pi{cfg: cfg, piPath: "/x/bin/pi", provider: "zai", model: "glm-5.3-flash", extension: "/x/ext/index.js", tools: "agent_browser,read"}
	argv := p.piArgv("SYS", "TASK")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"-p --mode json", "--no-session", "--no-extensions", "-e /x/ext/index.js", "--tools agent_browser,read", "--provider zai --model glm-5.3-flash --thinking high", "--system-prompt SYS -- TASK"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %s", want, joined)
		}
	}
}

// TestSandboxProbe (opt-in: VIGIL_SANDBOX_PROBE=1) runs agent-browser inside the
// computed nono grants without any model call, to surface Seatbelt denials.
func TestSandboxProbe(t *testing.T) {
	if os.Getenv("VIGIL_SANDBOX_PROBE") == "" {
		t.Skip("set VIGIL_SANDBOX_PROBE=1 to run")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono not installed")
	}
	ab, err := exec.LookPath("agent-browser")
	if err != nil {
		t.Skip("agent-browser not installed")
	}
	cfg := &config.Config{}
	cfg.Agent.Sandbox = "nono"
	ev := filepath.Join(os.TempDir(), "vigil-probe")
	_ = os.MkdirAll(ev, 0o755)
	sb, warns := NewSandbox(cfg, ev)
	t.Logf("warns=%v", warns)
	if err := sb.Doctor(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "AGENT_BROWSER_SESSION=vigil-probe", "AGENT_BROWSER_SOCKET_DIR="+AgentBrowserSocketDir())
	env = append(env, sb.Env...)
	for _, step := range [][]string{{"open", "https://example.com"}, {"get", "title"}, {"close"}} {
		argv := sb.Argv(append([]string{ab}, step...)...)
		t.Logf("argv: %s", strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = ev
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		t.Logf("step %v: err=%v\n%s", step, err, out)
		if err != nil {
			t.Fatalf("sandbox denied step %v", step)
		}
	}
}

// TestRealPi (opt-in: VIGIL_REAL_AGENT=1) runs Doctor and ONE bounded real discover
// task against the configured target. Evidence goes to $VIGIL_REAL_AGENT_EVIDENCE.
func TestRealPi(t *testing.T) {
	if os.Getenv("VIGIL_REAL_AGENT") == "" {
		t.Skip("set VIGIL_REAL_AGENT=1 to run (spends one model task)")
	}
	cfg := &config.Config{}
	cfg.Project.ID = "example"
	cfg.Target.BaseURL = "https://www.example.com"
	cfg.Target.AllowedHosts = []string{"example.com"}
	cfg.Agent.Model = "zai/glm-5.3-flash"
	cfg.Agent.Thinking = "high"
	cfg.Agent.EnvMap = map[string]string{"ZAI_API_KEY": "Z_AI_API_KEY"}
	cfg.Agent.Retries = 2
	cfg.Agent.Backoff.Duration = 60 * time.Second
	cfg.Agent.Timeout.Duration = 12 * time.Minute
	cfg.Agent.MaxTurns = 40
	cfg.Agent.MaxScenariosPerTask = 2
	cfg.Agent.Sandbox = os.Getenv("VIGIL_REAL_AGENT_SANDBOX")
	if cfg.Agent.Sandbox == "" {
		cfg.Agent.Sandbox = "auto"
	}
	ad, err := NewPi(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := ad.Doctor(context.Background()); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	t.Log("doctor: ok")
	if os.Getenv("VIGIL_REAL_AGENT_DOCTOR_ONLY") != "" {
		return
	}
	dir := os.Getenv("VIGIL_REAL_AGENT_EVIDENCE")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "vigil-real-agent")
	}
	req := Request{
		Task:         TaskDiscover,
		ProjectID:    cfg.Project.ID,
		FeatureID:    "training-entry-page",
		ShippedSHA:   "unknown",
		Summary:      "training entry landing page",
		Target:       cfg.Target.BaseURL,
		EntryPath:    "/app/entry",
		AllowedHosts: cfg.Target.AllowedHosts,
		Routes:       []string{"/app/entry"},
		Evidence:     "training entry landing: school tabs 초등/중학/고등, subject tabs 수학/영어(+정보 for 중학), grade tabs, 교사 입장/학생 입장 buttons open a new tab after auth",
		MaxScenarios: 2,
	}
	res, err := ad.Run(context.Background(), req, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decision=%s unavailable=%v attempts=%d toolCalls=%d sandbox=%s duration=%s reason=%q violations=%v candidates=%d visited=%v",
		res.Decision, res.ModelUnavailable, res.Attempts, res.ToolCalls, res.Sandbox, res.Duration.Round(time.Second), res.Reason, res.HostViolations, len(res.ScriptCandidates), res.VisitedURLs)
	for i, c := range res.ScriptCandidates {
		t.Logf("candidate %d:\n%s", i+1, c)
	}
	t.Logf("evidence: %s", res.Evidence)
}

// TestValidateRealResult (opt-in) validates the script candidates of a saved real
// run ($VIGIL_REAL_AGENT_EVIDENCE/agent-result.yaml) against the DSL.
func TestValidateRealResult(t *testing.T) {
	dir := os.Getenv("VIGIL_REAL_AGENT_EVIDENCE")
	if dir == "" {
		t.Skip("set VIGIL_REAL_AGENT_EVIDENCE to a saved agent evidence dir")
	}
	b, err := os.ReadFile(filepath.Join(dir, FileResult))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ParseResult("```yaml vigil-result\n" + string(b) + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decision=%s candidates=%d visited=%v", res.Decision, len(res.ScriptCandidates), res.VisitedURLs)
	for i, c := range res.ScriptCandidates {
		sc, err := dsl.Parse([]byte(c))
		if err != nil {
			t.Errorf("candidate %d: parse: %v", i+1, err)
			continue
		}
		if err := sc.Validate(nil); err != nil {
			t.Errorf("candidate %d (%s): %v", i+1, sc.Scenario.ID, err)
			continue
		}
		t.Logf("candidate %d %s: valid, oracle.source=%s, fingerprint=%s", i+1, sc.Scenario.ID, sc.Oracle.Source, sc.Fingerprint(nil)[:12])
	}
}

func TestParseResultToleratesRootKeyAndObjectCandidates(t *testing.T) {
	text := "done\n```yaml\nvigil-result:\n  decision: NEEDS_REVIEW\n  accounts_used:\n    teacher: t1\n  blocked_at:\n    screen: viewer\n  visited_urls:\n    - https://a.example/x (teacher)\n  script_candidates:\n    - id: teacher_deploy\n      steps:\n        - open entry\n```\n"
	r, err := ParseResult(text)
	if err != nil {
		t.Fatal(err)
	}
	if r.Decision != DecisionNeedsReview || len(r.ScriptCandidates) != 1 || !strings.Contains(r.ScriptCandidates[0], "teacher_deploy") {
		t.Fatalf("unexpected: %+v", r)
	}
	if len(r.VisitedURLs) != 1 || r.VisitedURLs[0] != "https://a.example/x" {
		t.Fatalf("visited: %v", r.VisitedURLs)
	}
	if !strings.Contains(r.Evidence, "blocked_at") || !strings.Contains(r.Evidence, "accounts_used") {
		t.Fatalf("extra fields not preserved: %q", r.Evidence)
	}
}

func TestProjectSettingsRetry(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.ModelRetries = 10
	cfg.Agent.ModelRetryDelay.Duration = 3 * time.Second
	p := &pi{cfg: cfg}
	dir := t.TempDir()
	if err := p.writeProjectSettings(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".pi", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"maxRetries": 10`) || !strings.Contains(string(b), `"baseDelayMs": 3000`) {
		t.Fatalf("settings: %s", b)
	}
}

func TestParseResultSalvagesAlmostYAML(t *testing.T) {
	text := "```yaml\nvigil-result:\n  decision: NEEDS_REVIEW\n  completion:\n    s1: partial: opened viewer\n  blocked_at:\n    url: https://a.example/v-web/index.html#/x\n```"
	r, err := ParseResult(text)
	if err != nil {
		t.Fatal(err)
	}
	if r.Decision != DecisionNeedsReview || !strings.Contains(r.Evidence, "blocked_at") || len(r.VisitedURLs) != 1 {
		t.Fatalf("salvage failed: %+v", r)
	}
}
