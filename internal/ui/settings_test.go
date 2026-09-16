package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vigil/internal/config"
	"vigil/internal/settings"
	"vigil/internal/store"
)

type memSettings struct {
	current settings.Agent
	saved   int
}

func (m *memSettings) Load() (settings.Agent, error) { return m.current, nil }
func (m *memSettings) Save(a settings.Agent) error   { m.current = a; m.saved++; return nil }

func newSettingsServer(t *testing.T, checkErr error) (*Server, *memSettings) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	cfg.Agent.Provider = "pi"
	cfg.Agent.Models = []string{"zai/glm-5.3-flash:high"}
	s := New(cfg, st, t.TempDir())
	mem := &memSettings{current: settings.Agent{Mode: settings.ModeDefault}}
	s.SetSettings(mem, t.TempDir())
	s.checkAgent = func(context.Context, *config.Config) error { return checkErr }
	return s, mem
}

func callSettings(s *Server, method, query, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://vigil.test/api/settings/agent"+query, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://vigil.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSettingsGetDescribesAdminDefault(t *testing.T) {
	s, _ := newSettingsServer(t, nil)
	w := callSettings(s, http.MethodGet, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	var got settingsPayload
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AdminDefault != "zai/glm-5.3-flash:high" || got.Active.Mode != settings.ModeDefault {
		t.Fatalf("payload = %+v", got)
	}
	if len(got.Providers) == 0 || len(got.CLIs) == 0 {
		t.Fatalf("catalog missing: %+v", got)
	}
}

func TestSettingsSaveStoresKeyAndHidesIt(t *testing.T) {
	s, mem := newSettingsServer(t, nil)
	w := callSettings(s, http.MethodPut, "", `{"mode":"api_key","provider":"zai","model":"glm-5.3-flash","api_key":"zai-secret-key"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if mem.saved != 1 || mem.current.APIKey != "zai-secret-key" {
		t.Fatalf("stored = %+v (saves=%d)", mem.current, mem.saved)
	}
	if strings.Contains(w.Body.String(), "zai-secret-key") {
		t.Fatalf("response leaked the key: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"key_hint":"…-key"`) {
		t.Fatalf("no key hint: %s", w.Body.String())
	}
}

func TestSettingsTestDoesNotSave(t *testing.T) {
	s, mem := newSettingsServer(t, nil)
	w := callSettings(s, http.MethodPut, "?test=1", `{"mode":"api_key","provider":"zai","model":"glm-5.3-flash","api_key":"k"}`)
	if w.Code != http.StatusOK || mem.saved != 0 {
		t.Fatalf("status = %d saves = %d body = %q", w.Code, mem.saved, w.Body.String())
	}
}

func TestSettingsRejectsUnusableAccess(t *testing.T) {
	s, mem := newSettingsServer(t, errUnusable{})
	w := callSettings(s, http.MethodPut, "", `{"mode":"api_key","provider":"zai","model":"glm-5.3-flash","api_key":"k"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if mem.saved != 0 {
		t.Fatal("unusable access was stored")
	}
	if !strings.Contains(w.Body.String(), "모델을 찾지 못했습니다") {
		t.Fatalf("error not surfaced: %s", w.Body.String())
	}
}

type errUnusable struct{}

func (errUnusable) Error() string { return "제공자 zai에서 모델을 찾지 못했습니다" }

func TestSettingsRejectsCrossOriginWrite(t *testing.T) {
	s, mem := newSettingsServer(t, nil)
	r := httptest.NewRequest(http.MethodPut, "http://vigil.test/api/settings/agent", strings.NewReader(`{"mode":"default"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://evil.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || mem.saved != 0 {
		t.Fatalf("status = %d saves = %d", w.Code, mem.saved)
	}
}

func TestSettingsUnavailableWithoutStore(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	s := New(cfg, st, t.TempDir())
	if w := callSettings(s, http.MethodGet, "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}
