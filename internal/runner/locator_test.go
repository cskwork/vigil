package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"vigil/internal/browser"
	"vigil/internal/dsl"
	"vigil/internal/model"
)

// accNameFixture reproduces the LNB button that broke the repair loop: Chrome's
// accessible name is "과제 과제" (img alt + label span), while el.textContent is
// only "과제", so an agent could never write a name the runner accepted.
const accNameFixture = `<!doctype html><html lang="ko"><body>
<button class="lnb__button" title="과제"><span class="lnb-icon"><img src="/lnb_homework_off.svg" alt="과제" class="lnb-icon__img"></span><span class="lnb__label">과제</span></button>
<button id="analysis"><span aria-hidden="true"><img src="/i.svg" alt="아이콘"></span><span>학급 분석</span></button>
<button id="titled" title="AI 학습관"><img src="/ai.svg" alt=""></button>
<button id="labelled" aria-label="설정"><span>gear</span></button>
<button id="ariaref" aria-labelledby="lbl"></button><span id="lbl" hidden>도움말</span>
<label for="q">검색어</label><input id="q" type="text">
<a href="/app/lms/assignment/edit/FORM_A" class="btn" aria-label="교과서 과제 작성하기 페이지 이동"> 작성하기 </a>
<button id="save"><img src="/save.svg" alt="저장하기"></button>
<p>보이는 문단</p>
</body></html>`

// pageWithFixture serves html and returns a chromedp context on it. It skips
// when no Chromium is available, like the E2E test does for its browsers.
func pageWithFixture(t *testing.T, html string) context.Context {
	t.Helper()
	bin, err := browser.DetectChromium()
	if err != nil {
		t.Skipf("chromium unavailable: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
	t.Cleanup(srv.Close)

	p := browser.NewChromium(bin, true, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ep, err := p.Ensure(ctx)
	if err != nil {
		t.Skipf("chromium could not start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ep.WebSocketURL, chromedp.NoModifyURL)
	t.Cleanup(cancelAlloc)
	pageCtx, cancelPage := chromedp.NewContext(allocCtx)
	t.Cleanup(cancelPage)
	if err := chromedp.Run(pageCtx, chromedp.Navigate(srv.URL)); err != nil {
		t.Skipf("chromium could not open the fixture: %v", err)
	}
	return pageCtx
}

func TestAccessibleNameMatchesChromeForContentNames(t *testing.T) {
	ctx := pageWithFixture(t, accNameFixture)
	cases := []struct {
		what  string
		spec  locatorSpec
		count int
	}{
		{"substring of the composed name", locatorSpec{By: "role", Role: "button", Name: "과제", Ref: "r"}, 1},
		{"the composed name itself", locatorSpec{By: "role", Role: "button", Name: "과제 과제", Ref: "r"}, 1},
		{"composed name, exact", locatorSpec{By: "role", Role: "button", Name: "과제 과제", Exact: true, Ref: "r"}, 1},
		{"aria-hidden subtree contributes nothing", locatorSpec{By: "role", Role: "button", Name: "학급 분석", Exact: true, Ref: "r"}, 1},
		{"title fallback when alt is empty", locatorSpec{By: "role", Role: "button", Name: "AI 학습관", Exact: true, Ref: "r"}, 1},
		{"aria-label wins over content", locatorSpec{By: "role", Role: "button", Name: "설정", Exact: true, Ref: "r"}, 1},
		{"aria-labelledby wins over everything", locatorSpec{By: "role", Role: "button", Name: "도움말", Exact: true, Ref: "r"}, 1},
		{"native label names the textbox", locatorSpec{By: "role", Role: "textbox", Name: "검색어", Exact: true, Ref: "r"}, 1},
		{"content-only name is not the exact name", locatorSpec{By: "role", Role: "button", Name: "과제", Exact: true, Ref: "r"}, 0},
		{"aria-hidden text is not addressable", locatorSpec{By: "role", Role: "button", Name: "아이콘", Ref: "r"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			r, err := resolveOnce(ctx, tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if r.Count != tc.count {
				t.Fatalf("count = %d, want %d (candidates: %v)", r.Count, tc.count, r.Candidates)
			}
		})
	}
}

func TestResolveMissReportsCandidateNames(t *testing.T) {
	pageCtx := pageWithFixture(t, accNameFixture)

	r, err := resolveOnce(pageCtx, locatorSpec{By: "role", Role: "button", Name: "숙제", Ref: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Count != 0 {
		t.Fatalf("count = %d, want 0", r.Count)
	}
	if r.CandidateKind != "role=button" {
		t.Fatalf("candidate kind = %q", r.CandidateKind)
	}
	if len(r.Candidates) == 0 || r.Candidates[0] != "과제 과제" {
		t.Fatalf("candidates = %v, want the computed names of the page buttons", r.Candidates)
	}

	// The same detail must reach the step error, which becomes Run.Error and the
	// failureDetail the repair prompt reads.
	s := &session{spec: Spec{Browser: model.BrowserChromium, RunTimeout: time.Minute}, runCtx: context.Background(), pageCtx: pageCtx}
	ctx, cancel := context.WithTimeout(pageCtx, 2*time.Second)
	defer cancel()
	_, serr := s.resolve(ctx, &dsl.Locator{By: "role", Role: "button", Name: "숙제"}, "r1", false)
	if serr == nil {
		t.Fatal("expected a locator failure")
	}
	if serr.class != FailLocator {
		t.Fatalf("class = %s", serr.class)
	}
	if !strings.Contains(serr.msg, `0 matches; candidates(role=button): "과제 과제"`) {
		t.Fatalf("error must name the available buttons, got %q", serr.msg)
	}
}

func TestCandidatesTextIsBounded(t *testing.T) {
	if got := candidatesText(&resolution{Count: 0}); got != "" {
		t.Fatalf("no candidates must add nothing, got %q", got)
	}
	got := candidatesText(&resolution{CandidateKind: "role=button", Candidates: []string{"과제 과제", "학급 분석"}})
	if want := `; candidates(role=button): "과제 과제", "학급 분석"`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	long := make([]string, 8)
	for i := range long {
		long[i] = strings.Repeat("x", 80)
	}
	got = candidatesText(&resolution{CandidateKind: "text", Candidates: long})
	if len(got) > maxCandidatesText {
		t.Fatalf("candidate text is %d chars, want <= %d", len(got), maxCandidatesText)
	}
	if !strings.HasSuffix(got, ", …") {
		t.Fatalf("truncated list must say so: %q", got)
	}
}

// Agent snapshots show accessible names, so agents write them as text locators.
// The text strategy falls back to the accessible name when the visible text
// misses, otherwise the repair loop cannot converge on an aria-label-only link.
func TestTextLocatorFallsBackToAccessibleName(t *testing.T) {
	ctx := pageWithFixture(t, accNameFixture)

	r, err := resolveOnce(ctx, locatorSpec{By: "text", Text: "교과서 과제 작성하기 페이지 이동", Ref: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Count != 1 || r.Tag != "a" {
		t.Fatalf("aria-label link: count=%d tag=%s candidates=%v", r.Count, r.Tag, r.Candidates)
	}
	if r.Note != "matched by accessible name" || !strings.Contains(r.Text, "matched by accessible name") {
		t.Fatalf("an accname match must say so: note=%q text=%q", r.Note, r.Text)
	}

	// the by-less inference lands on text and must behave the same
	if r, err = resolveOnce(ctx, locatorSpec{Text: "교과서 과제 작성하기 페이지 이동", Exact: true, Ref: "r"}); err != nil {
		t.Fatal(err)
	} else if r.Count != 1 || r.By != "text" || r.Note == "" {
		t.Fatalf("inferred text locator: count=%d by=%s note=%q", r.Count, r.By, r.Note)
	}

	// an image-only button is addressable by its alt text
	if r, err = resolveOnce(ctx, locatorSpec{By: "text", Text: "저장하기", Ref: "r"}); err != nil {
		t.Fatal(err)
	} else if r.Count != 1 || r.Note == "" {
		t.Fatalf("alt image button: count=%d note=%q", r.Count, r.Note)
	}

	// visible text still wins and is not annotated
	if r, err = resolveOnce(ctx, locatorSpec{By: "text", Text: "작성하기", Ref: "r"}); err != nil {
		t.Fatal(err)
	} else if r.Count != 1 || r.Tag != "a" || r.Note != "" {
		t.Fatalf("visible text pass: count=%d tag=%s note=%q", r.Count, r.Tag, r.Note)
	}

	// a text miss still lists candidates
	if r, err = resolveOnce(ctx, locatorSpec{By: "text", Text: "존재하지 않는 문구", Ref: "r"}); err != nil {
		t.Fatal(err)
	} else if r.Count != 0 || r.CandidateKind != "text" || len(r.Candidates) == 0 {
		t.Fatalf("text miss: count=%d kind=%q candidates=%v", r.Count, r.CandidateKind, r.Candidates)
	}
}
