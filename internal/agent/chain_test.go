package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
)

func TestChainParsing(t *testing.T) {
	c, err := NewChain([]string{"openai-codex/gpt-5.6-luna:low", "anthropic/claude-haiku-4-5", "glm-5.3-flash"}, "high", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelEntry{
		{Provider: "openai-codex", Model: "gpt-5.6-luna", Thinking: "low"},
		{Provider: "anthropic", Model: "claude-haiku-4-5", Thinking: "high"},
		{Provider: "zai", Model: "glm-5.3-flash", Thinking: "high"},
	}
	if got := c.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %+v", got)
	}
	if got := c.Names(); !reflect.DeepEqual(got, []string{"openai-codex/gpt-5.6-luna", "anthropic/claude-haiku-4-5", "zai/glm-5.3-flash"}) {
		t.Fatalf("names = %v", got)
	}
	if want[0].String() != "openai-codex/gpt-5.6-luna:low" {
		t.Fatalf("String = %s", want[0].String())
	}
	for _, bad := range [][]string{nil, {""}, {"zai/"}, {"/x"}, {"zai/x:turbo"}} {
		if _, err := NewChain(bad, "high", time.Minute); err == nil {
			t.Fatalf("entries %q must fail", bad)
		}
	}
	if _, err := NewChain([]string{"zai/x:turbo"}, "high", time.Minute); err == nil || !strings.Contains(err.Error(), "thinking level") {
		t.Fatalf("bad thinking error = %v", err)
	}
}

func TestChainCooldownSelectionAndExpiry(t *testing.T) {
	c, err := NewChain([]string{"a/one", "b/two:low"}, "high", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	if e, ok := c.Next(); !ok || e.Name() != "a/one" {
		t.Fatalf("first = %+v %v", e, ok)
	}
	c.MarkUnavailable("a/one", "429")
	if !c.CoolingDown("a/one") || c.CoolingDown("b/two") {
		t.Fatal("cooldown state wrong")
	}
	if e, ok := c.Next(); !ok || e.Name() != "b/two" || e.Thinking != "low" {
		t.Fatalf("after cooldown = %+v %v", e, ok)
	}
	if _, ok := c.Next("b/two"); ok {
		t.Fatal("exclude must skip the only available entry")
	}
	c.MarkUnavailable("b/two", "auth")
	if _, ok := c.Next(); ok {
		t.Fatal("all cooling down must yield ok=false")
	}
	now = now.Add(10*time.Minute + time.Second)
	if e, ok := c.Next(); !ok || e.Name() != "a/one" {
		t.Fatalf("after expiry = %+v %v", e, ok)
	}
	if c.CoolingDown("a/one") {
		t.Fatal("expired cooldown still reported")
	}
}

func TestProviderErrorReasonAndAuthCheck(t *testing.T) {
	if k, r := providerErrorReason("Rate limit reached for requests"); k != "transient" || !strings.HasPrefix(r, "transient:") {
		t.Fatalf("429 = %s %q", k, r)
	}
	if k, r := providerErrorReason("401 invalid api key"); k != "auth" || !strings.HasPrefix(r, "auth/quota:") {
		t.Fatalf("auth = %s %q", k, r)
	}
	if k, r := providerErrorReason("boom: exit status 3"); k != "" || r != "" {
		t.Fatalf("generic = %s %q", k, r)
	}
	st, err := ParseAuthCheck([]byte("warning: something\n{\"status\":\"ready\",\"provider\":\"zai\",\"authType\":\"api_key\"}\n"))
	if err != nil || !st.Ready() || st.AuthType != "api_key" {
		t.Fatalf("ready = %+v %v", st, err)
	}
	st, err = ParseAuthCheck([]byte(`{"status":"not_ready","provider":"google","reason":"provider_not_found"}`))
	if err != nil || st.Ready() || st.Reason != "provider_not_found" {
		t.Fatalf("not ready = %+v %v", st, err)
	}
	if _, err := ParseAuthCheck([]byte("pi: unknown command")); err == nil {
		t.Fatal("garbage must fail")
	}
}

// fakePiScript answers per --model: bad-429 / bad-auth -> provider errors,
// crash -> non-zero exit, good -> a vigil-result block, plan-good -> a vigil-plan block.
// Every invocation appends its provider/model/thinking flags to fake-pi-argv.log in the cwd (the evidence dir).
const fakePiScript = `#!/bin/sh
provider=""; model=""; thinking=""
prev=""
for a in "$@"; do
  case "$prev" in
    --provider) provider="$a" ;;
    --model) model="$a" ;;
    --thinking) thinking="$a" ;;
  esac
  prev="$a"
done
printf 'provider=%s model=%s thinking=%s\n' "$provider" "$model" "$thinking" >> fake-pi-argv.log
case "$model" in
  good) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done\n` + "```yaml vigil-result" + `\ndecision: NO_NEW_COVERAGE\nevidence: fine\n` + "```" + `"}],"stopReason":"stop"}}' ;;
  plan-good) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + "```yaml vigil-plan" + `\nassessment: ok\nactions: []\nsaturated: true\n` + "```" + `"}],"stopReason":"stop"}}' ;;
  bad-auth) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"error","errorMessage":"401 invalid api key"}}' ;;
  crash) echo boom >&2; exit 3 ;;
  *) printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"error","errorMessage":"429 Too Many Requests"}}' ;;
esac
`

// newFakePi builds an adapter whose pi binary is the shell fixture above. PATH is
// narrowed to the fixture dir so agent-browser is not found (no session cleanup calls).
func newFakePi(t *testing.T, entries ...string) (*pi, *bytes.Buffer, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "pi")
	if err := os.WriteFile(script, []byte(fakePiScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg := &config.Config{}
	cfg.Agent.Thinking = "high"
	cfg.Agent.Sandbox = "none"
	cfg.Agent.Retries = 1
	cfg.Agent.Backoff.Duration = 10 * time.Millisecond
	cfg.Agent.Timeout.Duration = 30 * time.Second
	cfg.Agent.MaxTurns = 5
	cfg.Agent.MaxScenariosPerTask = 2
	cfg.Agent.ModelCooldown.Duration = 10 * time.Minute
	cfg.Supervisor.MaxActions = 3
	chain, err := NewChain(entries, cfg.Agent.Thinking, cfg.Agent.ModelCooldown.Duration)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	p := &pi{cfg: cfg, piPath: script, chain: chain, extension: "/x/ext.js", tools: "agent_browser,read",
		redactor: RedactorFor(nil, nil), sandbox: SandboxNone, logger: log.New(logs, "", 0)}
	return p, logs, cfg
}

func argvLog(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "fake-pi-argv.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestRunSwitchesModelWithoutBackoff(t *testing.T) {
	p, logs, cfg := newFakePi(t, "zai/bad-429:low", "anthropic/good")
	cfg.Agent.Backoff.Duration = 30 * time.Second // a same-model retry would exceed the assertion below
	dir := t.TempDir()
	start := time.Now()
	res, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f1", Target: "https://www.example.com", AllowedHosts: []string{"example.com"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("model switch must not wait agent.backoff (took %s)", time.Since(start))
	}
	if res.ModelUnavailable || res.Decision != DecisionNoNewCoverage || res.Model != "anthropic/good" || res.Attempts != 2 {
		t.Fatalf("result = %+v", res)
	}
	want := []ModelAttempt{{"zai/bad-429", OutcomeUnavailable}, {"anthropic/good", OutcomeOK}}
	if !reflect.DeepEqual(res.ModelAttempts, want) {
		t.Fatalf("model attempts = %+v", res.ModelAttempts)
	}
	lines := argvLog(t, dir)
	if len(lines) != 2 || !strings.Contains(lines[0], "provider=zai model=bad-429 thinking=low") || !strings.Contains(lines[1], "provider=anthropic model=good thinking=high") {
		t.Fatalf("argv log = %q", lines)
	}
	if !p.chain.CoolingDown("zai/bad-429") || p.chain.CoolingDown("anthropic/good") {
		t.Fatal("cooldown not applied to the failed entry only")
	}
	if !strings.Contains(logs.String(), "agent: zai/bad-429 unavailable (transient: 429 Too Many Requests); switching to anthropic/good") {
		t.Fatalf("switch log missing:\n%s", logs.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, FileResultJSON))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Model         string         `json:"model"`
		ModelAttempts []ModelAttempt `json:"model_attempts"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Model != "anthropic/good" || !reflect.DeepEqual(doc.ModelAttempts, want) {
		t.Fatalf("agent-result.json = %s (%v)", b, err)
	}
	var argv []string
	if b, err := os.ReadFile(filepath.Join(dir, FileArgv)); err != nil || json.Unmarshal(b, &argv) != nil || !contains(argv, "anthropic") || !contains(argv, "good") {
		t.Fatalf("%s = %s (%v)", FileArgv, b, err)
	}
}

func TestRunExhaustedChainIsModelUnavailable(t *testing.T) {
	p, _, _ := newFakePi(t, "zai/bad-429", "openai/bad-auth")
	dir := t.TempDir()
	res, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f2"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ModelUnavailable || res.Model != "" || res.Attempts != 2 || res.Decision != "" {
		t.Fatalf("result = %+v", res)
	}
	want := []ModelAttempt{{"zai/bad-429", OutcomeUnavailable}, {"openai/bad-auth", OutcomeUnavailable}}
	if !reflect.DeepEqual(res.ModelAttempts, want) {
		t.Fatalf("model attempts = %+v", res.ModelAttempts)
	}
	if !strings.Contains(res.Reason, "no alternative chain entry") {
		t.Fatalf("reason = %q", res.Reason)
	}
	// Every entry is now cooling down: the next task must not spawn pi at all.
	res2, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f3"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.ModelUnavailable || res2.Attempts != 0 || len(res2.ModelAttempts) != 0 || len(argvLog(t, dir)) != 2 {
		t.Fatalf("second run = %+v argv=%q", res2, argvLog(t, dir))
	}
}

func TestRunSingleEntryRetriesThenUnavailable(t *testing.T) {
	p, logs, cfg := newFakePi(t, "zai/bad-429")
	cfg.Agent.Retries = 2 // 3 attempts on the lone entry, linear backoff 10ms/20ms
	dir := t.TempDir()
	res, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f5"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelAttempt{{"zai/bad-429", OutcomeUnavailable}, {"zai/bad-429", OutcomeUnavailable}, {"zai/bad-429", OutcomeUnavailable}}
	if !res.ModelUnavailable || res.Attempts != 3 || !reflect.DeepEqual(res.ModelAttempts, want) {
		t.Fatalf("result = %+v", res)
	}
	if lines := argvLog(t, dir); len(lines) != 3 {
		t.Fatalf("argv log = %q", lines)
	}
	if !p.chain.CoolingDown("zai/bad-429") {
		t.Fatal("entry must be cooled down only after the retries are exhausted")
	}
	for _, want := range []string{
		"agent: zai/bad-429 transient: 429 Too Many Requests; retry 2/3 in 10ms (no alternative model)",
		"agent: zai/bad-429 transient: 429 Too Many Requests; retry 3/3 in 20ms (no alternative model)",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "switching to") {
		t.Fatalf("single entry must not switch:\n%s", logs.String())
	}

	// Auth/quota errors are not retried on the same entry (legacy behaviour).
	pa, _, cfga := newFakePi(t, "openai/bad-auth")
	cfga.Agent.Retries = 2
	dira := t.TempDir()
	resa, err := pa.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f6"}, dira)
	if err != nil {
		t.Fatal(err)
	}
	if !resa.ModelUnavailable || resa.Attempts != 1 || !pa.chain.CoolingDown("openai/bad-auth") {
		t.Fatalf("auth result = %+v", resa)
	}
}

func TestRunRetriesFirstWhenSecondCoolingDown(t *testing.T) {
	p, logs, _ := newFakePi(t, "zai/bad-429", "anthropic/good")
	p.chain.MarkUnavailable("anthropic/good", "test")
	dir := t.TempDir()
	res, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f7"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelAttempt{{"zai/bad-429", OutcomeUnavailable}, {"zai/bad-429", OutcomeUnavailable}}
	if !res.ModelUnavailable || res.Attempts != 2 || !reflect.DeepEqual(res.ModelAttempts, want) {
		t.Fatalf("result = %+v", res)
	}
	if lines := argvLog(t, dir); len(lines) != 2 || strings.Contains(strings.Join(lines, "\n"), "anthropic") {
		t.Fatalf("argv log = %q", lines)
	}
	if strings.Contains(logs.String(), "switching to") || !strings.Contains(logs.String(), "retry 2/2 in 10ms (no alternative model)") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	if !p.chain.CoolingDown("zai/bad-429") {
		t.Fatal("first entry must be cooled down after its retries are exhausted")
	}
}

func TestRunRetriesSameModelOnNonProviderError(t *testing.T) {
	p, _, _ := newFakePi(t, "zai/crash", "anthropic/good")
	dir := t.TempDir()
	res, err := p.Run(context.Background(), Request{Task: TaskDiscover, FeatureID: "f4"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelAttempt{{"zai/crash", OutcomeError}, {"zai/crash", OutcomeError}}
	if !res.ModelUnavailable || !reflect.DeepEqual(res.ModelAttempts, want) {
		t.Fatalf("result = %+v", res)
	}
	if p.chain.CoolingDown("zai/crash") {
		t.Fatal("a non-provider error must not cool the entry down")
	}
	if lines := argvLog(t, dir); len(lines) != 2 || strings.Contains(strings.Join(lines, "\n"), "anthropic") {
		t.Fatalf("argv log = %q", lines)
	}
}

func TestPlanUsesChainAndSupervisorModel(t *testing.T) {
	p, logs, _ := newFakePi(t, "zai/bad-429", "anthropic/plan-good:low")
	dir := t.TempDir()
	plan, err := p.Plan(context.Background(), "briefing", dir)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model != "anthropic/plan-good" || !plan.Saturated || plan.ModelUnavailable {
		t.Fatalf("plan = %+v", plan)
	}
	if lines := argvLog(t, dir); len(lines) != 2 || !strings.Contains(lines[1], "provider=anthropic model=plan-good thinking=low") {
		t.Fatalf("argv log = %q", lines)
	}
	if !strings.Contains(logs.String(), "agent: zai/bad-429 unavailable (transient: 429 Too Many Requests); switching to anthropic/plan-good") {
		t.Fatalf("switch log missing:\n%s", logs.String())
	}

	// Exhausted: the only entry is cooling down from the run above.
	p2, _, cfg2 := newFakePi(t, "zai/bad-429")
	p2.chain = p.chain
	cfg2.Supervisor = p.cfg.Supervisor
	p2.chain.MarkUnavailable("anthropic/plan-good", "test")
	dir2 := t.TempDir()
	out, err := p2.Plan(context.Background(), "briefing", dir2)
	if err == nil || out == nil || !out.ModelUnavailable || len(argvLog(t, dir2)) != 0 {
		t.Fatalf("exhausted plan = %+v err=%v argv=%q", out, err, argvLog(t, dir2))
	}

	// supervisor.model is tried first, sharing the cooldown map.
	p3, _, cfg3 := newFakePi(t, "zai/bad-429")
	cfg3.Supervisor.Model = "google/plan-good"
	cfg3.Supervisor.Thinking = "minimal"
	dir3 := t.TempDir()
	plan3, err := p3.Plan(context.Background(), "briefing", dir3)
	if err != nil || plan3.Model != "google/plan-good" {
		t.Fatalf("pinned plan = %+v err=%v", plan3, err)
	}
	if lines := argvLog(t, dir3); len(lines) != 1 || !strings.Contains(lines[0], "provider=google model=plan-good thinking=minimal") {
		t.Fatalf("argv log = %q", lines)
	}
}
