package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/config"
)

func TestNormalizeAPIKeyFillsDefaultsAndKeepsStoredKey(t *testing.T) {
	got, err := Normalize(Agent{Mode: ModeAPIKey, Provider: ProviderAnthropic, APIKey: " sk-ant-secret "}, Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-sonnet-5" || got.Thinking != "high" || got.APIKey != "sk-ant-secret" {
		t.Fatalf("normalized = %+v", got)
	}
	// Saving again without retyping the key keeps it.
	again, err := Normalize(Agent{Mode: ModeAPIKey, Provider: ProviderAnthropic, Model: "claude-opus-5"}, got)
	if err != nil {
		t.Fatal(err)
	}
	if again.APIKey != "sk-ant-secret" || again.Model != "claude-opus-5" {
		t.Fatalf("second save = %+v", again)
	}
	// A different provider must not inherit the previous provider's key.
	if _, err := Normalize(Agent{Mode: ModeAPIKey, Provider: ProviderZAI}, got); err == nil {
		t.Fatal("key inherited across providers")
	}
}

func TestNormalizeRejectsIncompleteInput(t *testing.T) {
	cases := []struct {
		name string
		in   Agent
	}{
		{"no provider", Agent{Mode: ModeAPIKey, APIKey: "k"}},
		{"no key", Agent{Mode: ModeAPIKey, Provider: ProviderZAI}},
		{"compatible without base url", Agent{Mode: ModeAPIKey, Provider: ProviderCompatible, Model: "m", APIKey: "k"}},
		{"compatible with bad scheme", Agent{Mode: ModeAPIKey, Provider: ProviderCompatible, Model: "m", APIKey: "k", BaseURL: "ftp://x/v1"}},
		{"openai without model", Agent{Mode: ModeAPIKey, Provider: ProviderOpenAI, APIKey: "k"}},
		{"model with provider prefix", Agent{Mode: ModeAPIKey, Provider: ProviderZAI, Model: "zai/glm-5.3-flash", APIKey: "k"}},
		{"unknown cli", Agent{Mode: ModeCLI, CLI: "cursor"}},
		{"unknown mode", Agent{Mode: "whatever"}},
	}
	for _, c := range cases {
		if _, err := Normalize(c.in, Agent{}); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if !IsValidation(err) {
			t.Errorf("%s: not a validation error: %v", c.name, err)
		}
	}
}

func TestNormalizeCLIClearsKeyFields(t *testing.T) {
	got, err := Normalize(Agent{Mode: ModeCLI, CLI: CLIClaude, CLIModel: "opus"}, Agent{Mode: ModeAPIKey, APIKey: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "" || got.Provider != "" || got.CLI != CLIClaude || got.CLIModel != "opus" {
		t.Fatalf("cli mode = %+v", got)
	}
}

func TestViewMasksTheKey(t *testing.T) {
	v := Agent{Mode: ModeAPIKey, Provider: ProviderZAI, APIKey: "abcd1234wxyz"}.View()
	if !v.HasKey || v.KeyHint != "…wxyz" {
		t.Fatalf("view = %+v", v)
	}
	b := strings.Builder{}
	b.WriteString(v.KeyHint)
	if strings.Contains(b.String(), "abcd1234") {
		t.Fatal("view leaked the key")
	}
}

func TestStoreRoundTripIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agent-settings.json")
	s := NewStore(path)
	empty, err := s.Load()
	if err != nil || empty.Mode != ModeDefault {
		t.Fatalf("missing file = %+v, %v", empty, err)
	}
	want := Agent{Mode: ModeAPIKey, Provider: ProviderZAI, Model: "glm-5.3-flash", APIKey: "secret", Thinking: "high"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.APIKey != "secret" || got.Provider != ProviderZAI {
		t.Fatalf("loaded = %+v, %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %v, want 0600", st.Mode().Perm())
	}
}

func TestApplyBuiltInProviderReplacesChain(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Provider = "pi"
	cfg.Agent.Models = []string{"zai/glm-5.3-flash:high"}
	a := Agent{Mode: ModeAPIKey, Provider: ProviderAnthropic, Model: "claude-sonnet-5", APIKey: "k", Thinking: "high"}
	if err := a.Apply(cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := cfg.ModelEntries(); len(got) != 1 || got[0] != "anthropic/claude-sonnet-5:high" {
		t.Fatalf("entries = %v", got)
	}
	if cfg.Agent.Credential.EnvVar != "ANTHROPIC_API_KEY" || cfg.Agent.Credential.Extension != "" {
		t.Fatalf("credential = %+v", cfg.Agent.Credential)
	}
}

func TestApplyCompatibleWritesProviderExtension(t *testing.T) {
	cfg := &config.Config{}
	dir := t.TempDir()
	a := Agent{Mode: ModeAPIKey, Provider: ProviderCompatible, Model: "my-model", BaseURL: "https://gw.example.com/v1", APIKey: "k", Thinking: "low"}
	if err := a.Apply(cfg, dir); err != nil {
		t.Fatal(err)
	}
	ext := cfg.Agent.Credential.Extension
	if ext == "" {
		t.Fatal("no extension generated")
	}
	b, err := os.ReadFile(ext)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{"registerProvider", CustomProviderID, "https://gw.example.com/v1", "my-model", "$VIGIL_CUSTOM_API_KEY"} {
		if !strings.Contains(src, want) {
			t.Fatalf("extension missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "\"k\"") {
		t.Fatal("extension embedded the API key")
	}
	if got := cfg.ModelEntries(); len(got) != 1 || got[0] != CustomProviderID+"/my-model:low" {
		t.Fatalf("entries = %v", got)
	}
}

func TestApplyCLISwitchesProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Provider = "pi"
	cfg.Agent.Credential = &config.AgentCredential{Provider: "zai"}
	a := Agent{Mode: ModeCLI, CLI: CLICodex, CLIModel: "gpt-5.6"}
	if err := a.Apply(cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Provider != "codex" || cfg.Agent.CLIModel != "gpt-5.6" || cfg.Agent.Credential != nil {
		t.Fatalf("cfg = %+v", cfg.Agent)
	}
}

func TestApplyDefaultLeavesAdminConfigAlone(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Provider = "pi"
	cfg.Agent.Models = []string{"zai/glm-5.3-flash"}
	if err := (Agent{Mode: ModeDefault}).Apply(cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Provider != "pi" || cfg.Agent.Credential != nil || len(cfg.Agent.Models) != 1 {
		t.Fatalf("cfg changed: %+v", cfg.Agent)
	}
}
