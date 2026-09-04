package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vigil/internal/dsl"
	"vigil/internal/model"
)

func mustScenario(t *testing.T, y string) *dsl.Scenario {
	t.Helper()
	sc, err := dsl.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestFlattenExpandsUsesAndUseFlow(t *testing.T) {
	sc := mustScenario(t, `
scenario: {id: s1, version: 1}
uses: [login]
steps:
  - goto: /a
  - use_flow: pick
  - assert_text: {value: ok}
oracle: {source: spec}
`)
	flows := map[string]*dsl.Flow{
		"login": {Steps: []dsl.Step{{Name: "open", Goto: "/login"}, {Fill: &dsl.FillArgs{Locator: dsl.Locator{By: "id", Value: "u"}, Input: "{{persona.username}}"}}}},
		"pick":  {Steps: []dsl.Step{{Click: &dsl.Locator{By: "text", Text: "Pick"}}}},
	}
	steps, err := flatten(sc, flows)
	if err != nil {
		t.Fatal(err)
	}
	var kinds, names []string
	for _, s := range steps {
		kinds = append(kinds, s.Step.Kind())
		names = append(names, s.Name)
	}
	if got := strings.Join(kinds, ","); got != "goto,fill,goto,click,assert_text" {
		t.Fatalf("kinds = %s", got)
	}
	if names[0] != "flow:login/open" || names[1] != "flow:login/fill" || names[3] != "flow:pick/click" || names[2] != "goto" {
		t.Fatalf("names = %v", names)
	}
	if _, err := flatten(sc, nil); err == nil || !strings.Contains(err.Error(), "unknown flow") {
		t.Fatalf("expected unknown flow error, got %v", err)
	}
}

func TestSubstitutePersona(t *testing.T) {
	p := map[string]string{"username": "u1", "password": "p@ss", "school": "sch"}
	got, missing := substitutePersona("{{persona.username}}:{{ persona.password }}:{{persona.extra.school}}:{{persona.nope}}", p)
	if got != "u1:p@ss:sch:{{persona.nope}}" {
		t.Fatalf("got %q", got)
	}
	if len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("missing = %v", missing)
	}
}

func TestRedactorHidesPersonaValues(t *testing.T) {
	r := newRedactor(map[string]string{"username": "teacher01", "password": "s3cret!", "x": "ab"})
	out := r.Redact("login teacher01 with s3cret! and ab")
	if strings.Contains(out, "teacher01") || strings.Contains(out, "s3cret!") {
		t.Fatalf("secrets leaked: %q", out)
	}
	if !strings.Contains(out, "and ab") {
		t.Fatalf("short values must not be redacted (would mangle text): %q", out)
	}
}

func TestRedactorHidesCredentialQueryParams(t *testing.T) {
	r := newRedactor(nil)
	in := "https://h.example/auth?lmsToken=eyJabc.def&token=f511&access_id=yx4r&user_id=98292&api_domain=https%3A%2F%2Fx&Authorization=Bearer1&authorName=Kim&auth=zz9 next"
	out := r.Redact(in)
	for _, leaked := range []string{"eyJabc.def", "f511", "yx4r", "Bearer1", "auth=zz9"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("leaked %q in %q", leaked, out)
		}
	}
	for _, kept := range []string{"user_id=98292", "api_domain=https%3A%2F%2Fx", "lmsToken=[REDACTED]&token=[REDACTED]", "authorName=Kim", " next"} {
		if !strings.Contains(out, kept) {
			t.Fatalf("expected %q kept in %q", kept, out)
		}
	}
}

func TestResolveURL(t *testing.T) {
	cases := map[string]string{
		"/lms-web/x":              "https://h.example/lms-web/x",
		"https://o.example/p?q=1": "https://o.example/p?q=1",
		"/":                       "https://h.example/",
		"/a?b=c#d":                "https://h.example/a?b=c#d",
	}
	for in, want := range cases {
		got, err := resolveURL("https://h.example/base/", in)
		if err != nil || got != want {
			t.Fatalf("resolveURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := resolveURL("", "/x"); err == nil {
		t.Fatal("relative goto without BaseURL must fail")
	}
}

func TestArtifactName(t *testing.T) {
	if got := artifactName("after login.png", "x"); got != "after-login.png" {
		t.Fatalf("got %q", got)
	}
	if got := artifactName("../../etc/passwd", "x"); got != "etc-passwd.png" {
		t.Fatalf("got %q", got)
	}
	if got := artifactName("  ", "screenshot-3"); got != "screenshot-3.png" {
		t.Fatalf("got %q", got)
	}
}

func TestClassifyCDPErr(t *testing.T) {
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{spec: Spec{Browser: model.BrowserLightpanda, RunTimeout: time.Minute}, runCtx: runCtx, pageCtx: context.Background()}
	cases := []struct {
		err  error
		def  FailureClass
		want FailureClass
	}{
		{errors.New("page load error net::ERR_NAME_NOT_RESOLVED"), FailNavigation, FailTransport},
		{errors.New("page load error net::ERR_ABORTED"), FailNavigation, FailNavigation},
		{context.DeadlineExceeded, FailLocator, FailLocator},
		{errors.New("websocket: close 1006"), FailLocator, FailBrowserProtocol},
		{errors.New("something else"), FailAssertion, FailAssertion},
	}
	for _, tc := range cases {
		got := s.classifyCDPErr(tc.err, tc.def, "x")
		if got.class != tc.want {
			t.Fatalf("%v: got %s want %s", tc.err, got.class, tc.want)
		}
	}
	cancel()
	if got := s.classifyCDPErr(errors.New("x"), FailLocator, "x"); got.class != FailTimeout {
		t.Fatalf("expired run ctx must classify as timeout, got %s", got.class)
	}
}

func TestRunRejectsBadSpec(t *testing.T) {
	r := New(nil)
	if _, err := r.Run(context.Background(), Spec{}); err == nil {
		t.Fatal("nil scenario must be a Go error")
	}
	sc := mustScenario(t, "scenario: {id: s1, version: 1}\nsteps: [{goto: /}]\noracle: {source: spec}\n")
	if _, err := r.Run(context.Background(), Spec{Scenario: sc, Browser: model.BrowserChromium}); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Fatalf("missing provider must be a Go error, got %v", err)
	}
}

func TestRedactorHidesJWTAndEntityEncodedParams(t *testing.T) {
	r := newRedactor(nil)
	in := `<a href="/lms-web/auth?lmsToken=eyJhbGciOiJIUzI1NiJ9.eyJhY2NvdW50SUQiOiI5ODI5MjQ0MCJ9.abcdefghijk&amp;token=xyz123&amp;user_id=98292">x</a><script>var t="eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c";</script>`
	out := r.Redact(in)
	for _, bad := range []string{"eyJhbGciOiJIUzI1NiJ9", "xyz123"} {
		if strings.Contains(out, bad) {
			t.Fatalf("secret %q survived redaction: %s", bad, out)
		}
	}
	if !strings.Contains(out, "user_id=98292") {
		t.Fatalf("non-credential param must survive: %s", out)
	}
}

// Lightpanda passes look identical to Chromium passes in steps.json, yet its capture is a
// text rendering. The evidence must say so on its own, without a reader knowing the engine.
func TestRecordBrowserCapabilitiesMarksLightpandaUnpainted(t *testing.T) {
	cases := []struct {
		name        string
		browser     model.Browser
		wantRecords bool
	}{
		{"lightpanda has no paint pipeline", model.BrowserLightpanda, true},
		{"chromium paints, so it claims no limit", model.BrowserChromium, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{spec: Spec{Browser: tc.browser}, res: &Result{Capabilities: map[string]bool{}}}
			s.recordBrowserCapabilities()

			painted, recorded := s.res.Capabilities["screenshot_painted"]
			if !tc.wantRecords {
				if recorded {
					t.Fatalf("screenshot_painted recorded for %s; only measured engine limits belong in capabilities", tc.browser)
				}
				if _, ok := s.res.Capabilities["exception_events"]; ok {
					t.Fatalf("exception_events recorded for %s", tc.browser)
				}
				return
			}
			if !recorded {
				t.Fatal("screenshot_painted missing: a text rendering would be indistinguishable from a real screen")
			}
			if painted {
				t.Fatal("screenshot_painted = true for lightpanda, want false")
			}
			if ev, ok := s.res.Capabilities["exception_events"]; !ok || ev {
				t.Fatalf("exception_events = (%v, present=%v), want (false, present=true)", ev, ok)
			}
		})
	}
}
