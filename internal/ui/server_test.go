package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"vigil/internal/config"
)

func get(t *testing.T, s *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// A polled endpoint must answer 304 to the ETag it just handed out, even though
// the body carries a wall clock that differs between the two calls.
func TestPolledEndpointsRevalidateWith304(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for _, ep := range []string{"/api/overview", "/api/verification", "/api/scripts", "/api/requests"} {
		first := get(t, s, ep, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", ep, first.Code, first.Body.String())
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", ep)
		}
		if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control = %q, want no-cache so the browser revalidates", ep, cc)
		}
		second := get(t, s, ep, map[string]string{"If-None-Match": etag})
		if second.Code != http.StatusNotModified {
			t.Fatalf("%s: revalidation status %d, want 304", ep, second.Code)
		}
		if second.Body.Len() != 0 {
			t.Fatalf("%s: 304 must carry no body", ep)
		}
	}
}

func TestReadEndpointsRejectWrites(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for _, ep := range []string{"/api/overview", "/api/verification", "/api/scripts", "/api/run/detail", "/api/supervisor"} {
		r := httptest.NewRequest(http.MethodPost, ep, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status %d, want 405", ep, w.Code)
		}
	}
}

func TestStaticAssetsCarryETag(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for _, ep := range []string{"/", "/activity", "/scripts", "/report", "/ui/theme.css", "/ui/app.js"} {
		first := get(t, s, ep, nil)
		if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
			t.Fatalf("%s: status %d etag %q", ep, first.Code, first.Header().Get("ETag"))
		}
		if second := get(t, s, ep, map[string]string{"If-None-Match": first.Header().Get("ETag")}); second.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation status %d, want 304", ep, second.Code)
		}
	}
	if w := get(t, s, "/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown path status %d, want 404", w.Code)
	}
}

func TestEvidencePathSafety(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for _, bad := range []string{"", "..", "../x", "runs/../../etc", "/abs"} {
		if _, ok := s.safeJoin(bad); ok && bad != "/abs" {
			t.Errorf("safeJoin(%q) accepted", bad)
		}
	}
	if p, ok := s.safeJoin("runs/1"); !ok || p != filepath.Join(s.evRoot, "runs", "1") {
		t.Errorf("safeJoin(runs/1) = %q %v", p, ok)
	}
	if got := s.rel(filepath.Join(s.evRoot, "runs", "1")); got != "runs/1" {
		t.Errorf("rel inside root = %q", got)
	}
	if got := s.rel(filepath.Dir(s.evRoot)); got != "" {
		t.Errorf("rel of parent must be empty, got %q", got)
	}
	// a sibling directory whose name merely starts with ".." is not an escape
	sibling := filepath.Join(s.evRoot, "..hidden")
	if got := s.rel(sibling); got != "..hidden" {
		t.Errorf("rel of a dot-dot-prefixed child = %q", got)
	}
	if w := get(t, s, "/api/run/steps?dir=../x", nil); w.Code != http.StatusBadRequest {
		t.Errorf("traversal via API status %d, want 400", w.Code)
	}
}

func TestEvidenceSummaryIsCachedPerRunDir(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	dir := filepath.Join(s.evRoot, "runs", "r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("steps.json", `{"steps":[{},{}]}`)
	write("network.json", `[{},{},{}]`)
	write("console.json", `[{"level":"error"},{"level":"log"}]`)
	steps, reqs, errs, shot := s.evidenceSummary(dir, "runs/r1")
	if steps != 2 || reqs != 3 || errs != 1 || shot != "" {
		t.Fatalf("summary = %d %d %d %q", steps, reqs, errs, shot)
	}
	// The cache is keyed on steps.json; a same-size rewrite of network.json alone
	// is not a real-world event (run dirs are write-once), so the cached counts stand.
	write("network.json", `[{}]      `)
	if _, reqs2, _, _ := s.evidenceSummary(dir, "runs/r1"); reqs2 != 3 {
		t.Fatalf("expected cached request count 3, got %d", reqs2)
	}
	// A different steps.json size is a different run: the cache must refresh.
	write("steps.json", `{"steps":[{},{},{}]}`)
	if steps3, reqs3, _, _ := s.evidenceSummary(dir, "runs/r1"); steps3 != 3 || reqs3 != 1 {
		t.Fatalf("expected refreshed summary 3/1, got %d/%d", steps3, reqs3)
	}
}

func TestClipKeepsRuneBoundary(t *testing.T) {
	if got := clip("가나다라", 4); got != "가…" {
		t.Errorf("clip on a multibyte boundary = %q", got)
	}
	if got := clip("abc", 10); got != "abc" {
		t.Errorf("clip short = %q", got)
	}
}
