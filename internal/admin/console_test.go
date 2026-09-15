package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/config"
)

func fixture() Registry {
	return Registry{DefaultProject: "one", Projects: []Project{{ID: "one", Name: "첫 프로젝트", DefaultSite: "main", Revision: 1, Sites: []Site{{ID: "main", Name: "메인", URL: "https://example.com", ReadOnly: true}}}}}
}
func req(c *Console, method, path, body, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	return w
}
func jsonBody(v any) string { b, _ := json.Marshal(v); return string(b) }
func TestRegistryRoutingPersistenceAndRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	busy := false
	closed := 0
	factory := func(p Project) (Runtime, error) {
		return Runtime{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(p.ID + " " + p.Sites[0].URL)) }), Busy: func() bool { return busy }, Close: func() { closed++ }}, nil
	}
	c, e := New(path, fixture(), factory)
	if e != nil {
		t.Fatal(e)
	}
	p := fixture().Projects[0]
	p.ID = "two"
	p.Name = "두 번째"
	if w := req(c, "POST", "/api/projects", jsonBody(p), ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, id := range []string{"one", "two"} {
		if w := req(c, "GET", "/?project="+id, "", ""); !strings.HasPrefix(w.Body.String(), id) {
			t.Fatal(w.Body)
		}
	}
	if w := req(c, "GET", "/?project=missing", "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	p.Sites[0].URL = "https://changed.example/test"
	p.Sites = append(p.Sites, Site{ID: "search", Name: "검색", URL: "https://search.example", ReadOnly: true})
	busy = true
	if w := req(c, "PUT", "/api/projects", jsonBody(p), ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	busy = false
	if w := req(c, "PUT", "/api/projects", jsonBody(p), "https://attacker.example"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := req(c, "PUT", "/api/projects", jsonBody(p), ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if closed != 1 {
		t.Fatal("runtime not replaced", closed)
	}
	if w := req(c, "PUT", "/api/projects", jsonBody(p), ""); w.Code != 409 {
		t.Fatal("stale edit", w.Code)
	}
	if w := req(c, "GET", "/?project=two", "", ""); !strings.Contains(w.Body.String(), "changed.example") {
		t.Fatal(w.Body)
	}
	if w := req(c, "GET", "/?project=one", "", ""); strings.Contains(w.Body.String(), "changed.example") {
		t.Fatal("cross-project change")
	}
	c.Close()
	c, e = New(path, fixture(), factory)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if len(c.registry.Projects) != 2 || len(c.registry.Projects[1].Sites) != 2 || c.registry.Projects[1].Revision != 2 {
		t.Fatal(c.registry)
	}
	p = c.registry.Projects[1]
	p.Sites = p.Sites[:1]
	if w := req(c, "PUT", "/api/projects", jsonBody(p), ""); w.Code != 422 {
		t.Fatal("site removal accepted", w.Code)
	}
}
func TestURLValidationAndConfigIsolation(t *testing.T) {
	for _, u := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://u:p@example.com", "https://example.com/#secret", "https://", "//example.com"} {
		p := fixture().Projects[0]
		p.Sites[0].URL = u
		if p.Validate() == nil {
			t.Errorf("accepted %q", u)
		}
	}
	p := fixture().Projects[0]
	p.Sites[0].AllowedHosts = []string{"*.example.com"}
	if p.Validate() == nil {
		t.Fatal("wildcard accepted")
	}
	base := &config.Config{}
	base.Project.ID = "one"
	base.State.Path = "original.db"
	base.Target.Environments = map[string]config.Environment{"original": {BaseURL: "https://original.example"}}
	p = fixture().Projects[0]
	p.ID = "two"
	p.Sites[0].URL = "https://two.example"
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	derived := Config(base, p, t.TempDir())
	if derived.Target.BaseURL != "https://two.example" || derived.DefaultEnv().ReadOnly != true || derived.State.Path == base.State.Path {
		t.Fatal("incorrect derived configuration")
	}
	if base.Project.ID != "one" || len(base.Target.Environments) != 1 || base.Target.Environments["original"].BaseURL == "" {
		t.Fatal("base was mutated")
	}
}
func TestPersistenceFailureLeavesRuntimeAndRegistryIntact(t *testing.T) {
	c, e := New(filepath.Join(t.TempDir(), "projects.json"), fixture(), func(p Project) (Runtime, error) { return Runtime{Handler: http.NotFoundHandler()}, nil })
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.path = filepath.Join(c.path, "cannot-write.json")
	p := fixture().Projects[0]
	p.Name = "changed"
	if w := req(c, "PUT", "/api/projects", jsonBody(p), ""); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if c.registry.Projects[0].Name == "changed" || c.registry.Projects[0].Revision != 1 {
		t.Fatal("failed write changed state")
	}
}
