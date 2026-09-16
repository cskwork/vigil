// Package settings stores the model access a console operator configures when
// the administrator's own provider is not available, or when the operator would
// rather spend their own coding-agent subscription.
//
// Two shapes are supported and they are deliberately different:
//   - api_key: vigil keeps running its own agent (pi) and only swaps the
//     provider, the model and the key.
//   - cli: vigil delegates the whole task to a coding-agent CLI the operator is
//     already signed in to (claude, codex, pi), and reaches the browser through
//     vigil's own MCP server.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/config"
)

// Mode selects where model access comes from.
const (
	ModeDefault = "default" // whatever vigil.yaml and the server environment provide
	ModeAPIKey  = "api_key"
	ModeCLI     = "cli"
)

// Providers accepted in api_key mode. Compatible covers any OpenAI-compatible
// endpoint (proxy, gateway, self-hosted) and needs a base URL and a model.
const (
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderZAI        = "zai" // GLM Coding Plan
	ProviderCompatible = "openai-compatible"
)

// CustomProviderID is the pi provider id generated for ProviderCompatible.
const CustomProviderID = "vigil-custom"

// CLIs accepted in cli mode.
const (
	CLIClaude = "claude"
	CLICodex  = "codex"
	CLIPi     = "pi"
)

// Provider describes one selectable provider for the settings screen.
type Provider struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	EnvVar       string `json:"env_var"`
	DefaultModel string `json:"default_model"`
	NeedsBaseURL bool   `json:"needs_base_url"`
	Note         string `json:"note"`
}

// Providers is the catalog the settings screen renders, in display order.
var Providers = []Provider{
	{ID: ProviderAnthropic, Label: "Anthropic (Claude API)", EnvVar: "ANTHROPIC_API_KEY", DefaultModel: "claude-sonnet-5",
		Note: "Anthropic 콘솔에서 발급한 API 키를 사용합니다."},
	{ID: ProviderZAI, Label: "GLM Coding Plan (Z.ai)", EnvVar: "ZAI_API_KEY", DefaultModel: "glm-5.3-flash",
		Note: "Z.ai 코딩 플랜 키를 사용합니다."},
	{ID: ProviderOpenAI, Label: "OpenAI", EnvVar: "OPENAI_API_KEY", DefaultModel: "",
		Note: "OpenAI API 키를 사용합니다. 모델 이름을 직접 입력하세요."},
	{ID: ProviderCompatible, Label: "OpenAI 호환 엔드포인트", EnvVar: "VIGIL_CUSTOM_API_KEY", DefaultModel: "", NeedsBaseURL: true,
		Note: "사내 게이트웨이나 자체 호스팅처럼 OpenAI 호환 API를 제공하는 주소를 입력하세요."},
}

// Lookup returns the catalog entry for id.
func Lookup(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Agent is the stored configuration. APIKey is the only secret and never leaves
// the process in a response body (see View).
type Agent struct {
	Mode      string    `json:"mode"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	BaseURL   string    `json:"base_url,omitempty"`
	APIKey    string    `json:"api_key,omitempty"`
	Thinking  string    `json:"thinking,omitempty"`
	CLI       string    `json:"cli,omitempty"`
	CLIModel  string    `json:"cli_model,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// View is the safe projection returned to the browser: the key is reduced to
// its presence and last four characters.
type View struct {
	Mode      string    `json:"mode"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	BaseURL   string    `json:"base_url,omitempty"`
	HasKey    bool      `json:"has_key"`
	KeyHint   string    `json:"key_hint,omitempty"`
	Thinking  string    `json:"thinking,omitempty"`
	CLI       string    `json:"cli,omitempty"`
	CLIModel  string    `json:"cli_model,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// View projects the stored settings for display.
func (a Agent) View() View {
	v := View{Mode: a.Mode, Provider: a.Provider, Model: a.Model, BaseURL: a.BaseURL,
		Thinking: a.Thinking, CLI: a.CLI, CLIModel: a.CLIModel, UpdatedAt: a.UpdatedAt}
	if a.APIKey != "" {
		v.HasKey = true
		if r := []rune(a.APIKey); len(r) > 4 {
			v.KeyHint = "…" + string(r[len(r)-4:])
		}
	}
	return v
}

// ValidationError marks operator input the HTTP layer answers with 422.
type ValidationError struct{ err error }

func (e *ValidationError) Error() string    { return e.err.Error() }
func (e *ValidationError) Unwrap() error    { return e.err }
func (e *ValidationError) Invalid() bool    { return true }
func invalid(format string, a ...any) error { return &ValidationError{err: fmt.Errorf(format, a...)} }
func IsValidation(err error) bool           { var v *ValidationError; return errors.As(err, &v) }

const maxFieldLen = 512

// Normalize trims, fills defaults and rejects input the adapters cannot use.
// previous supplies the stored key when the operator saves without retyping it.
func Normalize(in Agent, previous Agent) (Agent, error) {
	in.Mode = strings.TrimSpace(in.Mode)
	in.Provider = strings.TrimSpace(in.Provider)
	in.Model = strings.TrimSpace(in.Model)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.APIKey = strings.TrimSpace(in.APIKey)
	in.Thinking = strings.TrimSpace(in.Thinking)
	in.CLI = strings.TrimSpace(in.CLI)
	in.CLIModel = strings.TrimSpace(in.CLIModel)
	for _, f := range []string{in.Provider, in.Model, in.BaseURL, in.APIKey, in.CLI, in.CLIModel} {
		if len(f) > maxFieldLen {
			return in, invalid("입력이 너무 깁니다")
		}
	}
	switch in.Mode {
	case "", ModeDefault:
		return Agent{Mode: ModeDefault, UpdatedAt: time.Now().UTC()}, nil
	case ModeAPIKey:
		p, ok := Lookup(in.Provider)
		if !ok {
			return in, invalid("사용할 LLM 제공자를 선택하세요")
		}
		if in.APIKey == "" && previous.Mode == ModeAPIKey && previous.Provider == in.Provider {
			in.APIKey = previous.APIKey // saved again without retyping the key
		}
		if in.APIKey == "" {
			return in, invalid("API 키를 입력하세요")
		}
		if strings.ContainsAny(in.APIKey, "\n\r\x00") {
			return in, invalid("API 키에 줄바꿈을 넣을 수 없습니다")
		}
		if in.Model == "" {
			in.Model = p.DefaultModel
		}
		if in.Model == "" {
			return in, invalid("사용할 모델 이름을 입력하세요")
		}
		if strings.ContainsAny(in.Model, " \t/") {
			return in, invalid("모델 이름에는 제공자 접두어 없이 모델 ID만 입력하세요")
		}
		if p.NeedsBaseURL {
			if err := validBaseURL(in.BaseURL); err != nil {
				return in, err
			}
		} else {
			in.BaseURL = ""
		}
		if in.Thinking == "" {
			in.Thinking = "high"
		}
		switch in.Thinking {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return in, invalid("사고 수준은 off, minimal, low, medium, high, xhigh, max 중에서 선택하세요")
		}
		in.CLI, in.CLIModel = "", ""
		in.UpdatedAt = time.Now().UTC()
		return in, nil
	case ModeCLI:
		switch in.CLI {
		case CLIClaude, CLICodex, CLIPi:
		default:
			return in, invalid("사용할 코딩 에이전트를 선택하세요 (claude, codex, pi)")
		}
		if strings.ContainsAny(in.CLIModel, " \t") {
			return in, invalid("모델 이름에 공백을 넣을 수 없습니다")
		}
		in.Provider, in.Model, in.BaseURL, in.APIKey = "", "", "", ""
		in.UpdatedAt = time.Now().UTC()
		return in, nil
	}
	return in, invalid("알 수 없는 설정 방식입니다")
}

func validBaseURL(raw string) error {
	if raw == "" {
		return invalid("OpenAI 호환 엔드포인트 주소를 입력하세요 (예: https://gateway.example.com/v1)")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return invalid("엔드포인트 주소는 http:// 또는 https:// 로 시작해야 합니다")
	}
	if strings.ContainsAny(raw, " \t\n\r") {
		return invalid("엔드포인트 주소에 공백을 넣을 수 없습니다")
	}
	return nil
}

// Store is the settings file. It holds a credential, so it is written 0600 and
// replaced atomically.
type Store struct{ path string }

// NewStore binds a store to a file path (created on first save).
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the file this store reads and writes.
func (s *Store) Path() string { return s.path }

// Load returns the stored settings, or the default mode when nothing is saved.
func (s *Store) Load() (Agent, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return Agent{Mode: ModeDefault}, nil
	}
	if err != nil {
		return Agent{}, err
	}
	var a Agent
	if err := json.Unmarshal(b, &a); err != nil {
		return Agent{}, fmt.Errorf("settings %s: %w", s.path, err)
	}
	if a.Mode == "" {
		a.Mode = ModeDefault
	}
	return a, nil
}

// Save replaces the file atomically with 0600 permissions.
func (s *Store) Save(a Agent) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".agent-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err == nil {
		err = f.Sync()
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

// AdminDefault describes what the administrator's config already provides, so
// the screen can say whether a personal key is needed at all.
func AdminDefault(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	entries := cfg.ModelEntries()
	if len(entries) == 0 {
		return ""
	}
	return strings.Join(entries, ", ")
}
