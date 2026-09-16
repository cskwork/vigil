package ui

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"vigil/internal/agent"
	"vigil/internal/config"
	"vigil/internal/settings"
)

// SettingsStore is the console's saved LLM access. The server only reads and
// writes it; applying it to a run belongs to the request submitter.
type SettingsStore interface {
	Load() (settings.Agent, error)
	Save(settings.Agent) error
}

// SetSettings enables the LLM settings screen. workDir holds generated files
// (a custom provider extension) and is used only while testing a setting.
func (s *Server) SetSettings(store SettingsStore, workDir string) {
	s.settings, s.settingsWork = store, workDir
}

// checkAccess proves a setting works before it is stored; tests replace it.
func (s *Server) checkAccess(ctx context.Context, cfg *config.Config) error {
	if s.checkAgent != nil {
		return s.checkAgent(ctx, cfg)
	}
	return agent.Check(ctx, cfg)
}

type cliOption struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Note      string `json:"note"`
}

type settingsPayload struct {
	Active       settings.View       `json:"active"`
	Summary      string              `json:"summary"`
	AdminDefault string              `json:"admin_default,omitempty"`
	Providers    []settings.Provider `json:"providers"`
	CLIs         []cliOption         `json:"clis"`
}

func cliOptions() []cliOption {
	out := make([]cliOption, 0, 3)
	for _, c := range []struct{ id, label, note string }{
		{settings.CLIClaude, "Claude Code", "이미 로그인한 Claude Code 구독으로 실행합니다. 별도 API 키가 필요 없습니다."},
		{settings.CLICodex, "Codex", "이미 로그인한 Codex 구독으로 실행합니다. 별도 API 키가 필요 없습니다."},
		{settings.CLIPi, "pi", "이 컴퓨터의 pi 설정과 인증을 그대로 사용합니다."},
	} {
		path, ok := agent.CLIPath(c.id)
		out = append(out, cliOption{ID: c.id, Label: c.label, Installed: ok, Path: path, Note: c.note})
	}
	return out
}

func (s *Server) agentSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "이 서버에서는 LLM 설정을 변경할 수 없습니다")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		current, err := s.settings.Load()
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "설정을 읽지 못했습니다")
			return
		}
		writeJSONStatus(w, http.StatusOK, settingsPayload{
			Active: current.View(), Summary: current.Describe(s.cfg),
			AdminDefault: settings.AdminDefault(s.cfg), Providers: settings.Providers, CLIs: cliOptions(),
		})
	case http.MethodPut, http.MethodPost:
		in, ok := s.readSettings(w, r)
		if !ok {
			return
		}
		test := r.URL.Query().Get("test") == "1"
		if err := s.checkSettings(r.Context(), in); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if test {
			writeJSONStatus(w, http.StatusOK, map[string]string{"status": "ok", "summary": in.Describe(s.cfg)})
			return
		}
		if err := s.settings.Save(in); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "설정을 저장하지 못했습니다")
			return
		}
		writeJSONStatus(w, http.StatusOK, settingsPayload{
			Active: in.View(), Summary: in.Describe(s.cfg),
			AdminDefault: settings.AdminDefault(s.cfg), Providers: settings.Providers, CLIs: cliOptions(),
		})
	default:
		w.Header().Set("Allow", "GET, PUT, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "지원하지 않는 요청 방식입니다")
	}
}

// readSettings validates the body and merges the stored key when the operator
// saved the form without retyping it.
func (s *Server) readSettings(w http.ResponseWriter, r *http.Request) (settings.Agent, bool) {
	if !hasSameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "다른 출처에서는 설정을 변경할 수 없습니다")
		return settings.Agent{}, false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Content-Type은 application/json이어야 합니다")
		return settings.Agent{}, false
	}
	var in settings.Agent
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "설정 형식이 올바르지 않습니다")
		return settings.Agent{}, false
	}
	previous, err := s.settings.Load()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "설정을 읽지 못했습니다")
		return settings.Agent{}, false
	}
	normalized, err := settings.Normalize(in, previous)
	if err != nil {
		if settings.IsValidation(err) {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		} else {
			writeAPIError(w, http.StatusInternalServerError, "설정을 확인하지 못했습니다")
		}
		return settings.Agent{}, false
	}
	return normalized, true
}

// checkSettings proves the access works before it is stored, so a typo in a key
// or a model name is reported on the settings screen, not hours later in a run.
func (s *Server) checkSettings(ctx context.Context, in settings.Agent) error {
	if in.Mode == settings.ModeDefault {
		return nil
	}
	probe := *s.cfg
	if err := in.Apply(&probe, s.settingsWork); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return s.checkAccess(cctx, &probe)
}
