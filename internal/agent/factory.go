package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"vigil/internal/config"
)

// ProviderPi is the default adapter: vigil's own pi invocation with the
// agent_browser extension.
const ProviderPi = "pi"

// KnownProviders lists every accepted agent.provider value.
var KnownProviders = []string{ProviderPi, ProviderClaude, ProviderCodex}

// New builds the Browser Agent adapter for cfg.Agent.Provider. It returns an
// error (never panics) when the provider's CLI is missing, so the caller can run
// deterministic QA without an agent (rule 12).
func New(cfg *config.Config) (Adapter, error) {
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	switch cfg.Agent.Provider {
	case "", ProviderPi:
		return NewPi(cfg)
	case ProviderClaude, ProviderCodex:
		return NewCLI(cfg)
	}
	return nil, fmt.Errorf("agent: unknown provider %q (expected one of pi, claude, codex)", cfg.Agent.Provider)
}

// CLIPath resolves a coding-agent CLI without building an adapter, so the
// settings screen can say whether it is installed.
func CLIPath(kind string) (string, bool) {
	if kind == ProviderPi {
		p, err := lookPi()
		return p, err == nil
	}
	if !IsCLIProvider(kind) {
		return "", false
	}
	p, err := lookCLI(kind)
	return p, err == nil
}

// Check verifies the configured access without spending a task: for pi it
// confirms the model is in the catalog with the operator's key, for a CLI it
// confirms the binary answers.
func Check(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return errors.New("agent: nil config")
	}
	if IsCLIProvider(cfg.Agent.Provider) {
		a, err := NewCLI(cfg)
		if err != nil {
			return err
		}
		return a.Doctor(ctx)
	}
	piPath, err := lookPi()
	if err != nil {
		return err
	}
	entries := cfg.ModelEntries()
	if len(entries) == 0 {
		return errors.New("agent: no model configured")
	}
	entry, err := ParseModelEntry(entries[0], cfg.Agent.Thinking)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	argv := []string{piPath, "--list-models", entry.Model}
	if cr := cfg.Agent.Credential; cr != nil && cr.Extension != "" {
		argv = []string{piPath, "-e", cr.Extension, "--list-models", entry.Model}
	}
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Env = agentEnv(cfg, "", "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("모델 목록을 확인하지 못했습니다: %v: %s", err, firstLine(string(out)))
	}
	if !modelListed(string(out), entry.Provider, entry.Model) {
		return fmt.Errorf("제공자 %s에서 모델 %s를 찾지 못했습니다. 모델 이름과 API 키를 확인하세요", entry.Provider, entry.Model)
	}
	return nil
}

// lookPi resolves the pi binary.
func lookPi() (string, error) {
	p, err := exec.LookPath("pi")
	if err != nil {
		return "", fmt.Errorf("agent: `pi` not found on PATH (install: npm i -g @earendil-works/pi-coding-agent): %w", err)
	}
	return p, nil
}
