package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/config"
	"vigil/internal/store"
)

func newTestServer(t *testing.T, w config.ActiveHours) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	cfg.Target.BaseURL = "https://example.test"
	cfg.Schedule.ActiveHours = w
	return New(cfg, st, t.TempDir())
}

func call(t *testing.T, s *Server, method, body string) (*httptest.ResponseRecorder, *windowView) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/api/schedule/window", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/api/schedule/window", nil)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var v windowView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v (body %q)", method, err, rec.Body.String())
	}
	return rec, &v
}

func businessHours() config.ActiveHours {
	return config.ActiveHours{Enabled: true, Days: config.Weekdays{1, 2, 3, 4, 5}, From: "08:00", To: "19:00", TZ: "Asia/Seoul"}
}

func TestWindowGetReturnsConfigDefault(t *testing.T) {
	s := newTestServer(t, businessHours())
	_, v := call(t, s, http.MethodGet, "")
	if v.Source != "config" {
		t.Errorf("source = %q, want config", v.Source)
	}
	if v.From != "08:00" || v.To != "19:00" || len(v.Days) != 5 {
		t.Errorf("unexpected window %+v", v.ActiveHours)
	}
	if v.Zone != "Asia/Seoul" {
		t.Errorf("zone = %q, want Asia/Seoul", v.Zone)
	}
	if v.NextChange == "" {
		t.Error("next_change should be populated for an enabled window")
	}
}

// windowView embeds config.ActiveHours. If ActiveHours ever grows a MarshalJSON
// method again, Go promotes it and these outer fields vanish from the response.
func TestWindowResponseKeepsOuterFields(t *testing.T) {
	s := newTestServer(t, businessHours())
	rec, _ := call(t, s, http.MethodGet, "")
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"enabled", "days", "from", "to", "tz", "source", "active_now", "zone"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response is missing %q: %s", k, rec.Body.String())
		}
	}
}

func TestWindowPostThenDelete(t *testing.T) {
	s := newTestServer(t, businessHours())

	_, v := call(t, s, http.MethodPost, `{"enabled":true,"days":[1,2,3,4,5,6],"from":"09:30","to":"18:00","tz":"Asia/Seoul"}`)
	if v.Source != "override" || v.From != "09:30" || len(v.Days) != 6 {
		t.Fatalf("POST did not persist: %+v", v)
	}
	_, v = call(t, s, http.MethodGet, "")
	if v.Source != "override" || v.To != "18:00" {
		t.Fatalf("override did not survive GET: %+v", v)
	}
	_, v = call(t, s, http.MethodDelete, "")
	if v.Source != "config" || v.From != "08:00" {
		t.Fatalf("DELETE did not fall back to config: %+v", v)
	}
}

func TestWindowPostRejectsInvalid(t *testing.T) {
	s := newTestServer(t, businessHours())
	for _, c := range []struct{ name, body string }{
		{"no days", `{"enabled":true,"days":[],"from":"08:00","to":"19:00"}`},
		{"bad time", `{"enabled":true,"days":[1],"from":"8am","to":"19:00"}`},
		{"empty window", `{"enabled":true,"days":[1],"from":"08:00","to":"08:00"}`},
		{"unknown tz", `{"enabled":true,"days":[1],"from":"08:00","to":"19:00","tz":"Mars/Olympus"}`},
		{"not json", `{nope`},
	} {
		rec, _ := call(t, s, http.MethodPost, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got HTTP %d, want 400", c.name, rec.Code)
		}
	}
	// nothing was persisted by the rejected writes
	if _, v := call(t, s, http.MethodGet, ""); v.Source != "config" {
		t.Errorf("rejected writes leaked into state: %+v", v)
	}
}

// The window control is unauthenticated; a page on another origin must not be
// able to switch cadence work off through the operator's browser.
func TestWindowRejectsCrossOriginWrites(t *testing.T) {
	s := newTestServer(t, businessHours())
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		r := httptest.NewRequest(m, "http://vigil.test/api/schedule/window", strings.NewReader(`{"enabled":false}`))
		r.Header.Set("Origin", "http://evil.test")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from another origin: status %d, want 403", m, rec.Code)
		}
	}
	_, v := call(t, s, http.MethodGet, "")
	if v == nil || !v.Enabled {
		t.Fatal("cross-origin write must not change the window")
	}
}

func TestWindowRejectsUnknownMethod(t *testing.T) {
	s := newTestServer(t, businessHours())
	rec, _ := call(t, s, http.MethodPatch, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH got HTTP %d, want 405", rec.Code)
	}
}

func TestOverviewCarriesWindow(t *testing.T) {
	s := newTestServer(t, businessHours())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview HTTP %d", rec.Code)
	}
	var o struct {
		Window *windowView `json:"window"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.Window == nil || o.Window.From != "08:00" {
		t.Fatalf("overview.window = %+v", o.Window)
	}
}
