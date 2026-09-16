package settings

import (
	"vigil/internal/agent"
	"vigil/internal/config"
)

// Apply rewrites cfg so the next agent run uses what the operator configured.
// workDir holds generated files (a custom provider extension); nothing is
// written for the built-in providers. A default-mode setting leaves cfg alone,
// so the administrator's own provider keeps working.
func (a Agent) Apply(cfg *config.Config, workDir string) error {
	if cfg == nil || a.Mode == "" || a.Mode == ModeDefault {
		return nil
	}
	switch a.Mode {
	case ModeCLI:
		cfg.Agent.Provider = a.CLI
		cfg.Agent.CLIModel = a.CLIModel
		cfg.Agent.Credential = nil
		return nil
	case ModeAPIKey:
		p, ok := Lookup(a.Provider)
		if !ok {
			return invalid("알 수 없는 LLM 제공자입니다: %s", a.Provider)
		}
		cred := &config.AgentCredential{
			Provider: a.Provider, Model: a.Model, Thinking: a.Thinking, EnvVar: p.EnvVar, APIKey: a.APIKey,
		}
		if a.Provider == ProviderCompatible {
			cred.Provider = CustomProviderID
			ext, err := agent.CustomProviderExtension(workDir, CustomProviderID, a.Model, a.BaseURL, p.EnvVar, true)
			if err != nil {
				return err
			}
			cred.Extension = ext
		}
		cfg.Agent.Provider = "pi"
		cfg.Agent.Credential = cred
		return nil
	}
	return invalid("알 수 없는 설정 방식입니다")
}

// Describe is the one-line summary the console shows for the active setting.
func (a Agent) Describe(cfg *config.Config) string {
	switch a.Mode {
	case ModeAPIKey:
		if p, ok := Lookup(a.Provider); ok {
			return p.Label + " · " + a.Model
		}
		return a.Provider + " · " + a.Model
	case ModeCLI:
		switch a.CLI {
		case CLIClaude:
			return "내 Claude Code 구독"
		case CLICodex:
			return "내 Codex 구독"
		case CLIPi:
			return "내 pi 설정"
		}
		return a.CLI
	}
	if d := AdminDefault(cfg); d != "" {
		return "관리자 기본 설정 · " + d
	}
	return "관리자 기본 설정"
}
