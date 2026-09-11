package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestURLFileDerivesTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "url.md"), []byte("# targets\n\ntraining-entry | https://www.example.com/app/training-entry?x=1\nhttps://www.example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "vigil.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.BaseURL != "https://www.example.com" {
		t.Fatalf("base_url %q", c.Target.BaseURL)
	}
	if !c.HostAllowed("www.example.com") {
		t.Fatalf("host not allowlisted: %v", c.Target.AllowedHosts)
	}
	if len(c.Targets) != 2 || c.Targets[0].Label != "training-entry" || c.Targets[0].Path != "/app/training-entry?x=1" || c.Targets[1].Label != "other" {
		t.Fatalf("targets %+v", c.Targets)
	}
	if c.EntryPath() != "/app/training-entry?x=1" || len(c.TargetURLs()) != 2 {
		t.Fatalf("entry %q urls %v", c.EntryPath(), c.TargetURLs())
	}
}

func TestURLFileMissingKeepsValidation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"), 0o644)
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("base_url must still be required without url.md")
	}
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("not a url\n"), 0o644)
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("invalid url line must fail")
	}
}

func loadWithAgent(t *testing.T, agentYAML string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("https://www.example.com/app\n"), 0o644)
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"+agentYAML), 0o644)
	return Load(cfgPath)
}

func TestModelChainDefaults(t *testing.T) {
	c, err := loadWithAgent(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.ModelCooldown.Duration != 10*time.Minute || len(c.Agent.Models) != 0 {
		t.Fatalf("defaults: cooldown=%s models=%v", c.Agent.ModelCooldown.Duration, c.Agent.Models)
	}
	if got := c.ModelEntries(); len(got) != 1 || got[0] != "zai/glm-5.3-flash" {
		t.Fatalf("legacy chain = %v", got)
	}
	c, err = loadWithAgent(t, "agent:\n  models:\n    - openai-codex/gpt-5.6-luna:low\n    - anthropic/claude-haiku-4-5\n  model_cooldown: 2m\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.ModelCooldown.Duration != 2*time.Minute {
		t.Fatalf("cooldown = %s", c.Agent.ModelCooldown.Duration)
	}
	if got := c.ModelEntries(); len(got) != 2 || got[0] != "openai-codex/gpt-5.6-luna:low" || got[1] != "anthropic/claude-haiku-4-5" {
		t.Fatalf("chain = %v", got)
	}
}

func TestModelChainValidation(t *testing.T) {
	cases := map[string]string{
		"agent:\n  models: [nope]\n":        "agent.models[0]",
		"agent:\n  models: [zai/x:turbo]\n": "thinking level",
		"agent:\n  models: ['zai/']\n":      "agent.models[0]",
		"agent:\n  model_cooldown: -1m\n":   "agent.model_cooldown",
	}
	for yamlText, want := range cases {
		_, err := loadWithAgent(t, yamlText)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: err = %v, want %q", yamlText, err, want)
		}
	}
	p, m, th, err := SplitModelEntry("openai-codex/gpt-5.6-luna:low")
	if err != nil || p != "openai-codex" || m != "gpt-5.6-luna" || th != "low" {
		t.Fatalf("split = %s %s %s %v", p, m, th, err)
	}
	if _, _, th, err := SplitModelEntry("google/gemini-2.5-flash"); err != nil || th != "" {
		t.Fatalf("no-thinking split = %q %v", th, err)
	}
}

func TestDiscoverySourceDefaults(t *testing.T) {
	cfg, err := loadWithAgent(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AdapterNames(); len(got) != 1 || got[0] != "file" {
		t.Fatalf("legacy adapter list = %v", got)
	}
	j, l, e := cfg.Discovery.Jira, cfg.Discovery.Loki, cfg.Discovery.Exec
	if j.CLI != "acli" || j.Limit != 20 || j.PollInterval.Duration != 10*time.Minute {
		t.Fatalf("jira defaults = %+v", j)
	}
	if l.Window.Duration != time.Hour || l.PollInterval.Duration != 15*time.Minute || l.MinCount != 3 || l.MaxLines != 500 || l.Signature != DefaultLokiSignature || l.MaxEventsPerPoll != 5 {
		t.Fatalf("loki defaults = %+v", l)
	}
	if e.PollInterval.Duration != 15*time.Minute || e.Timeout.Duration != 60*time.Second {
		t.Fatalf("exec defaults = %+v", e)
	}
	if _, err := regexp.Compile(DefaultLokiSignature); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryAdaptersValidation(t *testing.T) {
	t.Setenv("VIGIL_TEST_LOKI_URL", "https://grafana.example.com")
	cases := []struct{ name, yaml, wantErr string }{
		{"unknown", "discovery:\n  adapters: [file, svn]\n", "discovery.adapters[1]"},
		{"dup", "discovery:\n  adapters: [file, file]\n", "listed twice"},
		{"jira jql", "discovery:\n  adapters: [jira]\n", "discovery.jira.jql"},
		{"jira hint", "discovery:\n  adapters: [jira]\n  jira:\n    jql: project = A20\n    route_hints: [{match: x}]\n", "route_hints[0]"},
		{"loki url", "discovery:\n  adapters: [loki]\n", "discovery.loki.base_url"},
		{"loki env", "discovery:\n  adapters: [loki]\n  loki:\n    base_url: ${VIGIL_TEST_LOKI_URL}\n    datasource_uid: u\n    expr: '{a=\"b\"}'\n", "email_env"},
		{"loki signature", "discovery:\n  adapters: [loki]\n  loki:\n    base_url: ${VIGIL_TEST_LOKI_URL}\n    datasource_uid: u\n    expr: '{a=\"b\"}'\n    email_env: E\n    password_env: P\n    signature: '('\n", "discovery.loki.signature"},
		{"loki min_count", "discovery:\n  adapters: [loki]\n  loki:\n    base_url: ${VIGIL_TEST_LOKI_URL}\n    datasource_uid: u\n    expr: '{a=\"b\"}'\n    email_env: E\n    password_env: P\n    min_count: -1\n", "min_count"},
		{"loki max_events", "discovery:\n  adapters: [loki]\n  loki:\n    base_url: ${VIGIL_TEST_LOKI_URL}\n    datasource_uid: u\n    expr: '{a=\"b\"}'\n    email_env: E\n    password_env: P\n    max_events_per_poll: -2\n", "max_events_per_poll"},
		{"exec command", "discovery:\n  adapters: [exec]\n", "discovery.exec.command"},
		{"exec timeout", "discovery:\n  adapters: [exec]\n  exec:\n    command: [true]\n    timeout: -1s\n", "timeout"},
	}
	for _, c := range cases {
		_, err := loadWithAgent(t, c.yaml)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
		}
	}
	ok := "discovery:\n  adapters: [file, jira, loki, exec]\n  jira:\n    jql: project = A20\n    route_hints: [{match: AI, route: /ai}]\n" +
		"  loki:\n    base_url: ${VIGIL_TEST_LOKI_URL}\n    datasource_uid: u\n    expr: '{a=\"b\"}'\n    email_env: E\n    password_env: P\n" +
		"  exec:\n    command: [python3, x.py]\n"
	cfg, err := loadWithAgent(t, ok)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UsesAdapter("loki") || cfg.UsesAdapter("generic-git") || len(cfg.AdapterNames()) != 4 {
		t.Fatalf("adapter names = %v", cfg.AdapterNames())
	}
}

func TestDailyAtAndJiraDefaults(t *testing.T) {
	c, err := loadWithAgent(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.DailyAt != "09:00" || c.Jira.CLI != "acli" || !c.Jira.CommentsOnApprove() || c.Jira.DryRun {
		t.Fatalf("defaults: daily_at=%q jira=%+v", c.Schedule.DailyAt, c.Jira)
	}
	c, err = loadWithAgent(t, "schedule:\n  daily_at: \"23:30\"\njira:\n  cli: /opt/acli\n  comment_on_approve: false\n  dry_run: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.DailyAt != "23:30" || c.Jira.CLI != "/opt/acli" || c.Jira.CommentsOnApprove() || !c.Jira.DryRun {
		t.Fatalf("explicit: daily_at=%q jira=%+v", c.Schedule.DailyAt, c.Jira)
	}
	for _, bad := range []struct{ yaml, want string }{
		{"schedule:\n  daily_at: \"9am\"\n", "schedule.daily_at"},
		{"schedule:\n  daily_at: \"24:00\"\n", "schedule.daily_at"},
		{"jira:\n  cli: \"  \"\n", "jira.cli"},
	} {
		if _, err := loadWithAgent(t, bad.yaml); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q: err = %v, want %q", bad.yaml, err, bad.want)
		}
	}
}

func TestDomainFileDefaultAndValidation(t *testing.T) {
	c, err := loadWithAgent(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.DomainFile != "" {
		t.Fatalf("default domain_file = %q, want empty", c.Agent.DomainFile)
	}
	c, err = loadWithAgent(t, "agent:\n  domain_file: \" packs/demo/domain.md \"\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.DomainFile != "packs/demo/domain.md" || c.Abs(c.Agent.DomainFile) != filepath.Join(c.BaseDir, "packs/demo/domain.md") {
		t.Fatalf("domain_file = %q abs=%q", c.Agent.DomainFile, c.Abs(c.Agent.DomainFile))
	}
	// a directory is rejected; a missing file is left to doctor
	_, err = loadWithAgent(t, "agent:\n  domain_file: .\n")
	if err == nil || !strings.Contains(err.Error(), "agent.domain_file") {
		t.Fatalf("directory: err = %v", err)
	}
	if _, err := loadWithAgent(t, "agent:\n  domain_file: missing.md\n"); err != nil {
		t.Fatalf("missing file must not fail load: %v", err)
	}
}
