package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"vigil/internal/dsl"
)

// exec dispatches one step. ref is the data-vigil-ref token for this step.
func (s *session) exec(ctx context.Context, st dsl.Step, sr *StepResult, ref string) *stepErr {
	switch st.Kind() {
	case "goto":
		return s.doGoto(ctx, st.Goto, sr)
	case "click":
		return s.doClick(ctx, st.Click, sr, ref)
	case "fill":
		return s.doFill(ctx, st.Fill, sr, ref, true)
	case "type":
		return s.doFill(ctx, st.Type, sr, ref, false)
	case "press":
		return s.doPress(ctx, st.Press, sr)
	case "select":
		return s.doSelect(ctx, st.Select, sr, ref)
	case "hover":
		return s.doHover(ctx, st.Hover, sr, ref)
	case "wait_for":
		r, err := s.resolve(ctx, st.WaitFor, ref, true)
		if err != nil {
			return err
		}
		sr.Actual = fmt.Sprintf("visible <%s> %q", r.Tag, r.Text)
		return nil
	case "wait_ms":
		select {
		case <-time.After(time.Duration(st.WaitMs) * time.Millisecond):
			return nil
		case <-ctx.Done():
			return s.classifyCDPErr(ctx.Err(), FailTimeout, "wait_ms")
		}
	case "wait_url":
		return s.doURL(ctx, st.WaitURL, sr, FailNavigation, "wait_url")
	case "assert_url":
		return s.doURL(ctx, st.AssertURL, sr, FailAssertion, "assert_url")
	case "assert_text":
		return s.doText(ctx, st.AssertText, sr, ref, true)
	case "assert_no_text":
		return s.doText(ctx, st.AssertNoText, sr, ref, false)
	case "assert_visible":
		r, err := s.resolve(ctx, st.AssertVisible, ref, true)
		if err != nil {
			err.class = FailAssertion
			return err
		}
		sr.Expected, sr.Actual = "visible", fmt.Sprintf("visible <%s> %q", r.Tag, r.Text)
		return nil
	case "assert_not_visible":
		return s.doNotVisible(ctx, st.AssertNotVisible, sr, ref)
	case "assert_count":
		return s.doCount(ctx, st.AssertCount, sr, ref)
	case "assert_request":
		return s.doRequest(ctx, st.AssertRequest, sr)
	case "assert_attr":
		return s.doAttr(ctx, st.AssertAttr, sr, ref)
	case "expect_popup":
		return s.doPopup(ctx, st.ExpectPopup, sr)
	case "eval":
		return s.doEval(ctx, st.Eval, sr)
	case "screenshot":
		return s.doScreenshot(ctx, st.Screenshot, sr)
	}
	return fail(FailInternal, "unknown step kind %q", st.Kind())
}

// ---- helpers ------------------------------------------------------------

// evalJSON evaluates expr and returns its JSON value. Errors caused by a
// navigation in progress are reported as transient so pollers can retry.
func evalJSON(ctx context.Context, expr string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := chromedp.Run(ctx, chromedp.Evaluate(expr, &raw))
	return raw, err
}

func isTransientEvalErr(err error) bool {
	if err == nil {
		return false
	}
	l := strings.ToLower(err.Error())
	for _, k := range []string{"cannot find context", "context was destroyed", "execution context", "inspected target navigated", "target closed", "not attached"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// poll runs fn every 100ms until it reports done or ctx expires. Transient
// evaluation errors are retried; other errors abort immediately.
func poll(ctx context.Context, fn func() (done bool, err error)) error {
	for {
		done, err := fn()
		if err != nil && !isTransientEvalErr(err) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if done && err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// resolve waits for the locator to match (and be visible when required) and
// marks the element with ref. Failures are FailLocator with what was observed.
func (s *session) resolve(ctx context.Context, l *dsl.Locator, ref string, requireVisible bool) (*resolution, *stepErr) {
	if l == nil {
		return nil, fail(FailInternal, "locator missing")
	}
	spec := specFromLocator(l, ref)
	var last *resolution
	err := poll(ctx, func() (bool, error) {
		r, err := resolveOnce(ctx, spec)
		if err != nil {
			return false, err
		}
		last = r
		if r.Error != "" || !r.Found {
			return false, nil
		}
		return !requireVisible || r.Visible, nil
	})
	if err == nil {
		return last, nil
	}
	desc := describeLocator(l)
	if ctx.Err() == nil && !isTransientEvalErr(err) {
		return nil, s.classifyCDPErr(err, FailLocator, "locate "+desc)
	}
	observed := "no evaluation completed"
	if last != nil {
		switch {
		case last.Error != "":
			observed = last.Error
		case last.Count == 0:
			observed = "0 matches"
		case !last.Visible:
			observed = fmt.Sprintf("%d match(es), nth element <%s> not visible", last.Count, last.Tag)
		default:
			observed = fmt.Sprintf("%d match(es)", last.Count)
		}
	}
	if s.runCtx.Err() != nil {
		return nil, fail(FailTimeout, "locate %s: run timeout exceeded (%s)", desc, observed).with("element present", observed)
	}
	return nil, fail(FailLocator, "locate %s: %s", desc, observed).with("element present", observed)
}

func jsRef(ref string) string { return "document.querySelector('" + refSelector(ref) + "')" }

// probeLayout measures once whether the browser computes real boxes (needed for
// coordinate-based native input). Lightpanda reports 5x5 boxes for everything.
func (s *session) probeLayout(ctx context.Context) {
	if s.layoutProbed {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var w float64
	if err := chromedp.Run(pctx, chromedp.Evaluate(`(function(){var r=document.documentElement.getBoundingClientRect(); return Math.max(r.width, window.innerWidth||0)})()`, &w)); err != nil {
		return
	}
	s.layoutProbed = true
	s.layout = w > 100
	if !s.layout {
		s.note("no real layout on %s (document width %.0f): clicks/hovers use DOM events instead of coordinate input", s.spec.Browser, w)
	}
}

// ---- steps ----------------------------------------------------------------

func (s *session) doGoto(ctx context.Context, ref string, sr *StepResult) *stepErr {
	u, err := resolveURL(s.spec.BaseURL, ref)
	if err != nil {
		return fail(FailInternal, "goto: %v", err)
	}
	sr.Expected = u
	if err := chromedp.Run(ctx, chromedp.Navigate(u)); err != nil {
		return s.classifyCDPErr(err, FailNavigation, "goto "+u)
	}
	s.navigated = true
	s.probeLayout(ctx)
	final := s.currentURL()
	sr.Actual = final
	// document response status: gateway → transport, 401/403 → auth
	if ev, _, ok := s.cap.findRequest(func(e NetworkEvent) bool { return e.Status != 0 && (e.URL == u || e.URL == final) }); ok {
		switch {
		case ev.Status == 502 || ev.Status == 503 || ev.Status == 504:
			return fail(FailTransport, "goto %s: gateway error HTTP %d", u, ev.Status).with("HTTP 2xx", fmt.Sprintf("HTTP %d", ev.Status))
		case ev.Status == 401 || ev.Status == 403:
			return fail(FailAuth, "goto %s: HTTP %d", u, ev.Status).with("HTTP 2xx", fmt.Sprintf("HTTP %d", ev.Status))
		}
	}
	return nil
}

func (s *session) doClick(ctx context.Context, l *dsl.Locator, sr *StepResult, ref string) *stepErr {
	r, serr := s.resolve(ctx, l, ref, true)
	if serr != nil {
		return serr
	}
	sr.Expected = "click " + describeLocator(l)
	var nativeErr error
	if s.layout {
		nativeErr = chromedp.Run(ctx, chromedp.Click(refSelector(ref), chromedp.ByQuery))
		if nativeErr == nil {
			sr.Actual = fmt.Sprintf("native click on <%s> %q", r.Tag, r.Text)
			return nil
		}
		if ctx.Err() != nil {
			return s.classifyCDPErr(nativeErr, FailLocator, "click "+describeLocator(l))
		}
	}
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var el=`+jsRef(ref)+`; if(!el) return 'gone'; el.click(); return 'ok'})()`, &out))
	if err != nil {
		return s.classifyCDPErr(err, FailLocator, "click "+describeLocator(l))
	}
	if out != "ok" {
		return fail(FailLocator, "click %s: element detached before click", describeLocator(l))
	}
	if nativeErr != nil {
		sr.Actual = fmt.Sprintf("js click on <%s> %q (native failed: %v)", r.Tag, r.Text, nativeErr)
		s.note("step %d: native click failed on %s, used DOM click: %v", sr.Index, s.spec.Browser, nativeErr)
	} else {
		sr.Actual = fmt.Sprintf("js click on <%s> %q", r.Tag, r.Text)
	}
	return nil
}

// doFill implements fill (clear + insert) and type (append via key events).
// Values are never written to the result; only their presence is recorded.
func (s *session) doFill(ctx context.Context, a *dsl.FillArgs, sr *StepResult, ref string, clear bool) *stepErr {
	kind := "type"
	if clear {
		kind = "fill"
	}
	value, missing := substitutePersona(a.Input, s.spec.Persona)
	if len(missing) > 0 {
		return fail(FailAuth, "%s: persona value(s) missing: %s", kind, strings.Join(missing, ", "))
	}
	if _, serr := s.resolve(ctx, &a.Locator, ref, true); serr != nil {
		return serr
	}
	sr.Expected = kind + " " + describeLocator(&a.Locator) + " (value redacted)"
	// JS focus: DOM.focus is unsupported on Lightpanda and wedges the session (measured).
	var focused bool
	err := chromedp.Run(ctx, chromedp.Evaluate(`(function(clear){var el=`+jsRef(ref)+`; if(!el) return false; el.focus();
		if (clear) { if (el.isContentEditable) { el.textContent=''; } else { el.value=''; } el.dispatchEvent(new Event('input',{bubbles:true})); }
		return document.activeElement===el })(`+fmt.Sprint(clear)+`)`, &focused))
	if err != nil {
		return s.classifyCDPErr(err, FailLocator, kind+" focus")
	}
	// native input first: real key/insert events drive frameworks like Vue v-model
	var nativeErr error
	if clear {
		nativeErr = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error { return input.InsertText(value).Do(c) }))
	} else {
		nativeErr = chromedp.Run(ctx, chromedp.KeyEvent(value))
	}
	if nativeErr != nil && ctx.Err() != nil {
		return s.classifyCDPErr(nativeErr, FailLocator, kind)
	}
	// verify, fall back to setting the value through the DOM
	arg, _ := json.Marshal(value)
	var mode string
	err = chromedp.Run(ctx, chromedp.Evaluate(`(function(v, clear){var el=`+jsRef(ref)+`; if(!el) return 'gone';
		var cur = el.isContentEditable ? el.textContent : el.value;
		var ok = clear ? cur === v : (cur||'').slice(-v.length) === v;
		if (!ok) { if (el.isContentEditable) { el.textContent = clear ? v : (cur + v); } else { el.value = clear ? v : (cur + v); } el.dispatchEvent(new Event('input',{bubbles:true})); }
		el.dispatchEvent(new Event('change',{bubbles:true}));
		return ok ? 'native' : 'js' })(`+string(arg)+`, `+fmt.Sprint(clear)+`)`, &mode))
	if err != nil {
		return s.classifyCDPErr(err, FailLocator, kind+" verify")
	}
	switch {
	case mode == "gone":
		return fail(FailLocator, "%s %s: element detached", kind, describeLocator(&a.Locator))
	case mode == "js":
		sr.Actual = kind + " via DOM value (native input did not apply)"
		s.note("step %d: %s used DOM value fallback on %s (native err: %v)", sr.Index, kind, s.spec.Browser, nativeErr)
	default:
		sr.Actual = kind + " via native input"
	}
	if !focused {
		sr.Actual += "; element did not take focus"
	}
	return nil
}

var keyNames = map[string]string{
	"enter": kb.Enter, "return": kb.Enter, "tab": kb.Tab, "escape": kb.Escape, "esc": kb.Escape,
	"backspace": kb.Backspace, "delete": kb.Delete, "space": " ",
	"arrowup": kb.ArrowUp, "arrowdown": kb.ArrowDown, "arrowleft": kb.ArrowLeft, "arrowright": kb.ArrowRight,
	"up": kb.ArrowUp, "down": kb.ArrowDown, "left": kb.ArrowLeft, "right": kb.ArrowRight,
	"home": kb.Home, "end": kb.End, "pageup": kb.PageUp, "pagedown": kb.PageDown,
}

func (s *session) doPress(ctx context.Context, key string, sr *StepResult) *stepErr {
	seq, ok := keyNames[strings.ToLower(strings.TrimSpace(key))]
	if !ok {
		if len([]rune(key)) == 1 {
			seq = key
		} else {
			return fail(FailInternal, "press: unsupported key %q", key)
		}
	}
	sr.Expected = "press " + key
	if err := chromedp.Run(ctx, chromedp.KeyEvent(seq)); err != nil {
		return s.classifyCDPErr(err, FailBrowserProtocol, "press "+key)
	}
	sr.Actual = "dispatched"
	return nil
}

func (s *session) doSelect(ctx context.Context, a *dsl.SelectArgs, sr *StepResult, ref string) *stepErr {
	option, missing := substitutePersona(a.Option, s.spec.Persona)
	if len(missing) > 0 {
		return fail(FailAuth, "select: persona value(s) missing: %s", strings.Join(missing, ", "))
	}
	if _, serr := s.resolve(ctx, &a.Locator, ref, true); serr != nil {
		return serr
	}
	sr.Expected = "select option " + option
	arg, _ := json.Marshal(option)
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(`(function(opt, exact){var el=`+jsRef(ref)+`; if(!el) return 'gone';
		if (!el.options) return 'not-a-select:' + el.tagName.toLowerCase();
		var norm=function(s){return (s||'').replace(/\s+/g,' ').trim()};
		var idx=-1, opts=Array.prototype.slice.call(el.options);
		for (var i=0;i<opts.length;i++){ if (opts[i].value===opt) { idx=i; break; } }
		if (idx<0) for (var i=0;i<opts.length;i++){ if (norm(opts[i].text)===norm(opt)) { idx=i; break; } }
		if (idx<0 && !exact) for (var i=0;i<opts.length;i++){ if (norm(opts[i].text).toLowerCase().indexOf(norm(opt).toLowerCase())>=0) { idx=i; break; } }
		if (idx<0) return 'no-option:' + opts.map(function(o){return o.text}).slice(0,20).join('|');
		el.selectedIndex=idx; el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true}));
		return 'ok:' + norm(opts[idx].text) })(`+string(arg)+`, `+fmt.Sprint(a.Exact)+`)`, &out))
	if err != nil {
		return s.classifyCDPErr(err, FailLocator, "select")
	}
	switch {
	case out == "gone":
		return fail(FailLocator, "select %s: element detached", describeLocator(&a.Locator))
	case strings.HasPrefix(out, "not-a-select:"):
		return fail(FailLocator, "select %s: element is <%s>, not a <select>", describeLocator(&a.Locator), strings.TrimPrefix(out, "not-a-select:"))
	case strings.HasPrefix(out, "no-option:"):
		return fail(FailLocator, "select %s: option %q not found", describeLocator(&a.Locator), option).with(option, "options: "+strings.TrimPrefix(out, "no-option:"))
	}
	sr.Actual = "selected " + strings.TrimPrefix(out, "ok:")
	return nil
}

func (s *session) doHover(ctx context.Context, l *dsl.Locator, sr *StepResult, ref string) *stepErr {
	r, serr := s.resolve(ctx, l, ref, true)
	if serr != nil {
		return serr
	}
	sr.Expected = "hover " + describeLocator(l)
	if s.layout {
		var box []float64
		err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var el=`+jsRef(ref)+`; if(!el) return null; el.scrollIntoView({block:'center',inline:'center'}); var b=el.getBoundingClientRect(); return [b.left+b.width/2, b.top+b.height/2]})()`, &box))
		if err == nil && len(box) == 2 {
			err = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
				return input.DispatchMouseEvent(input.MouseMoved, box[0], box[1]).Do(c)
			}))
			if err == nil {
				sr.Actual = fmt.Sprintf("native hover on <%s> %q", r.Tag, r.Text)
				return nil
			}
		}
		if ctx.Err() != nil {
			return s.classifyCDPErr(err, FailLocator, "hover")
		}
		s.note("step %d: native hover failed on %s, used DOM events: %v", sr.Index, s.spec.Browser, err)
	}
	var out string
	err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var el=`+jsRef(ref)+`; if(!el) return 'gone';
		['mouseover','mouseenter','mousemove'].forEach(function(t){ el.dispatchEvent(new MouseEvent(t,{bubbles:t!=='mouseenter'})); }); return 'ok'})()`, &out))
	if err != nil {
		return s.classifyCDPErr(err, FailLocator, "hover")
	}
	if out != "ok" {
		return fail(FailLocator, "hover %s: element detached", describeLocator(l))
	}
	sr.Actual = fmt.Sprintf("js hover on <%s> %q", r.Tag, r.Text)
	return nil
}

func (s *session) doURL(ctx context.Context, a *dsl.URLAssert, sr *StepResult, class FailureClass, what string) *stepErr {
	var re *regexp.Regexp
	if a.Matches != "" {
		var err error
		if re, err = regexp.Compile(a.Matches); err != nil {
			return fail(FailInternal, "%s: bad regexp %q: %v", what, a.Matches, err)
		}
	}
	if a.Contains != "" {
		sr.Expected = "url contains " + a.Contains
	} else {
		sr.Expected = "url matches " + a.Matches
	}
	var last string
	err := poll(ctx, func() (bool, error) {
		raw, err := evalJSON(ctx, `String(location.href)`)
		if err != nil {
			return false, err
		}
		_ = json.Unmarshal(raw, &last)
		if a.Contains != "" && !strings.Contains(last, a.Contains) {
			return false, nil
		}
		if re != nil && !re.MatchString(last) {
			return false, nil
		}
		return true, nil
	})
	sr.Actual = last
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return s.classifyCDPErr(err, class, what)
	}
	if s.runCtx.Err() != nil {
		return fail(FailTimeout, "%s: run timeout exceeded (url=%s)", what, last).with(sr.Expected, last)
	}
	return fail(class, "%s: %s; current url %s", what, sr.Expected, last).with(sr.Expected, last)
}

// doText asserts presence (want=true) or absence of text in body or `in` locator.
func (s *session) doText(ctx context.Context, a *dsl.TextAssert, sr *StepResult, ref string, want bool) *stepErr {
	if want {
		sr.Expected = fmt.Sprintf("text %q present", a.Value)
	} else {
		sr.Expected = fmt.Sprintf("text %q absent", a.Value)
	}
	if a.Exact {
		sr.Expected += " (exact)"
	}
	scope := "document.body"
	if a.In != nil {
		scope = jsRef(ref)
	}
	valArg, _ := json.Marshal(a.Value)
	inSpec := ""
	if a.In != nil {
		b, _ := json.Marshal(specFromLocator(a.In, ref))
		inSpec = string(b)
	}
	var last struct {
		Found  bool   `json:"found"`
		Sample string `json:"sample"`
		Scope  string `json:"scope"`
	}
	err := poll(ctx, func() (bool, error) {
		if inSpec != "" {
			r, err := resolveOnce(ctx, specFromLocator(a.In, ref))
			if err != nil {
				return false, err
			}
			if !r.Found {
				last.Scope = "in-locator: " + r.byOrCount()
				return false, nil
			}
		}
		raw, err := evalJSON(ctx, `(function(v, exact){var el=`+scope+`; if(!el) return {found:false, sample:'', scope:'missing'};
			var norm=function(s){return (s||'').replace(/\s+/g,' ').trim()};
			var t = norm(el.innerText !== undefined ? el.innerText : el.textContent);
			var found = exact ? t === norm(v) : t.indexOf(norm(v)) >= 0;
			if (!found && !exact) found = t.toLowerCase().indexOf(norm(v).toLowerCase()) >= 0;
			return {found: found, sample: t.slice(0, 300), scope: 'ok'} })(`+string(valArg)+`, `+fmt.Sprint(a.Exact)+`)`)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &last); err != nil {
			return false, err
		}
		return last.Found == want, nil
	})
	if err == nil {
		sr.Actual = "ok"
		return nil
	}
	if ctx.Err() == nil {
		return s.classifyCDPErr(err, FailAssertion, sr.Kind)
	}
	if s.runCtx.Err() != nil {
		return fail(FailTimeout, "%s: run timeout exceeded", sr.Kind)
	}
	actual := fmt.Sprintf("text sample: %q", truncate(last.Sample, 300))
	if last.Scope != "" && last.Scope != "ok" {
		actual = last.Scope
		if a.In != nil {
			return fail(FailLocator, "%s: scope %s not found (%s)", sr.Kind, describeLocator(a.In), last.Scope).with(sr.Expected, actual)
		}
	}
	return fail(FailAssertion, "%s: %s; %s", sr.Kind, sr.Expected, actual).with(sr.Expected, actual)
}

func (r *resolution) byOrCount() string {
	if r.Error != "" {
		return r.Error
	}
	return fmt.Sprintf("%d matches", r.Count)
}

func (s *session) doNotVisible(ctx context.Context, l *dsl.Locator, sr *StepResult, ref string) *stepErr {
	sr.Expected = "not visible " + describeLocator(l)
	spec := specFromLocator(l, ref)
	var last *resolution
	err := poll(ctx, func() (bool, error) {
		r, err := resolveOnce(ctx, spec)
		if err != nil {
			return false, err
		}
		last = r
		return !r.Found || !r.Visible, nil
	})
	if err == nil {
		sr.Actual = "not visible"
		return nil
	}
	if ctx.Err() == nil {
		return s.classifyCDPErr(err, FailAssertion, "assert_not_visible")
	}
	actual := "still visible"
	if last != nil {
		actual = fmt.Sprintf("still visible: <%s> %q (%d matches)", last.Tag, last.Text, last.Count)
	}
	return fail(FailAssertion, "assert_not_visible %s: %s", describeLocator(l), actual).with(sr.Expected, actual)
}

func (s *session) doCount(ctx context.Context, a *dsl.CountAssert, sr *StepResult, ref string) *stepErr {
	var conds []string
	if a.Equals != nil {
		conds = append(conds, fmt.Sprintf("== %d", *a.Equals))
	}
	if a.Min != nil {
		conds = append(conds, fmt.Sprintf(">= %d", *a.Min))
	}
	if a.Max != nil {
		conds = append(conds, fmt.Sprintf("<= %d", *a.Max))
	}
	sr.Expected = fmt.Sprintf("count %s %s", describeLocator(&a.Locator), strings.Join(conds, " and "))
	spec := specFromLocator(&a.Locator, ref)
	spec.Nth = 0
	last := -1
	err := poll(ctx, func() (bool, error) {
		r, err := resolveOnce(ctx, spec)
		if err != nil {
			return false, err
		}
		last = r.Count
		ok := true
		if a.Equals != nil && r.Count != *a.Equals {
			ok = false
		}
		if a.Min != nil && r.Count < *a.Min {
			ok = false
		}
		if a.Max != nil && r.Count > *a.Max {
			ok = false
		}
		return ok, nil
	})
	sr.Actual = fmt.Sprintf("count %d", last)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return s.classifyCDPErr(err, FailAssertion, "assert_count")
	}
	return fail(FailAssertion, "assert_count: %s; got %d", sr.Expected, last).with(sr.Expected, sr.Actual)
}

func (s *session) doRequest(ctx context.Context, a *dsl.RequestAssert, sr *StepResult) *stepErr {
	sr.Expected = fmt.Sprintf("request %s %s", strings.ToUpper(a.Method), a.URLContains)
	if a.Status != 0 {
		sr.Expected += fmt.Sprintf(" status %d", a.Status)
	} else if a.StatusMin != 0 || a.StatusMax != 0 {
		sr.Expected += fmt.Sprintf(" status %d..%d", a.StatusMin, a.StatusMax)
	}
	if s.navigated && !s.cap.networkSeen() {
		s.res.Capabilities["network"] = false
		return fail(FailBrowserProtocol, "assert_request: network events unsupported on %s (no Network.* events observed after navigation)", s.spec.Browser)
	}
	needStatus := a.Status != 0 || a.StatusMin != 0 || a.StatusMax != 0 || a.BodyContains != ""
	match := func(e NetworkEvent) bool {
		if !strings.Contains(e.URL, a.URLContains) {
			return false
		}
		if a.Method != "" && !strings.EqualFold(e.Method, a.Method) {
			return false
		}
		if needStatus && e.Status == 0 {
			return false
		}
		if a.Status != 0 && e.Status != a.Status {
			return false
		}
		if a.StatusMin != 0 && e.Status < a.StatusMin {
			return false
		}
		if a.StatusMax != 0 && e.Status > a.StatusMax {
			return false
		}
		return true
	}
	var found NetworkEvent
	var reqID network.RequestID
	var seen []string
	err := poll(ctx, func() (bool, error) {
		ev, id, ok := s.cap.findRequest(match)
		if ok {
			found, reqID = ev, id
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		for _, ev := range s.cap.snapshotNetwork() {
			if strings.Contains(ev.URL, a.URLContains) {
				seen = append(seen, fmt.Sprintf("%s %d %s", ev.Method, ev.Status, truncate(ev.URL, 160)))
			}
		}
		actual := "no matching request"
		if len(seen) > 0 {
			actual = "candidates: " + strings.Join(seen, "; ")
			for _, ev := range s.cap.snapshotNetwork() {
				if strings.Contains(ev.URL, a.URLContains) && (ev.Status == 401 || ev.Status == 403) && (a.Status == 0 || a.Status < 400) {
					return fail(FailAuth, "assert_request: %s answered HTTP %d", truncate(ev.URL, 160), ev.Status).with(sr.Expected, actual)
				}
			}
		}
		if s.runCtx.Err() != nil {
			return fail(FailTimeout, "assert_request: run timeout exceeded").with(sr.Expected, actual)
		}
		return fail(FailAssertion, "assert_request: %s not observed; %s", sr.Expected, actual).with(sr.Expected, actual)
	}
	sr.Actual = fmt.Sprintf("%s %d %s", found.Method, found.Status, truncate(found.URL, 200))
	if a.BodyContains != "" {
		var body []byte
		err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			b, err := network.GetResponseBody(reqID).Do(c)
			body = b
			return err
		}))
		if err != nil {
			return s.classifyCDPErr(err, FailBrowserProtocol, "assert_request body")
		}
		if !strings.Contains(string(body), a.BodyContains) {
			return fail(FailAssertion, "assert_request: body does not contain %q", a.BodyContains).with("body contains "+a.BodyContains, truncate(string(body), 300))
		}
	}
	return nil
}

func (s *session) doAttr(ctx context.Context, a *dsl.AttrAssert, sr *StepResult, ref string) *stepErr {
	switch {
	case a.Equals != "":
		sr.Expected = fmt.Sprintf("%s == %q", a.Attr, a.Equals)
	case a.Contains != "":
		sr.Expected = fmt.Sprintf("%s contains %q", a.Attr, a.Contains)
	default:
		sr.Expected = a.Attr + " present"
	}
	spec := specFromLocator(&a.Locator, ref)
	attrArg, _ := json.Marshal(a.Attr)
	var last *string
	var resolved bool
	err := poll(ctx, func() (bool, error) {
		r, err := resolveOnce(ctx, spec)
		if err != nil {
			return false, err
		}
		if !r.Found {
			resolved = false
			return false, nil
		}
		resolved = true
		raw, err := evalJSON(ctx, `(function(n){var el=`+jsRef(ref)+`; return el ? el.getAttribute(n) : null})(`+string(attrArg)+`)`)
		if err != nil {
			return false, err
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			return false, err
		}
		last = v
		switch {
		case v == nil:
			return false, nil
		case a.Equals != "":
			return *v == a.Equals, nil
		case a.Contains != "":
			return strings.Contains(*v, a.Contains), nil
		}
		return true, nil
	})
	if last != nil {
		sr.Actual = fmt.Sprintf("%s=%q", a.Attr, *last)
	} else {
		sr.Actual = a.Attr + " absent"
	}
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return s.classifyCDPErr(err, FailAssertion, "assert_attr")
	}
	if !resolved {
		return fail(FailLocator, "assert_attr: %s not found", describeLocator(&a.Locator)).with(sr.Expected, "element not found")
	}
	return fail(FailAssertion, "assert_attr %s: %s; got %s", describeLocator(&a.Locator), sr.Expected, sr.Actual).with(sr.Expected, sr.Actual)
}

// doPopup waits for a page target created by the previous action and switches
// the run to it. Browsers that open window.open() in the same target (Lightpanda,
// measured) cannot satisfy this step: it fails with FailPopup and a note.
func (s *session) doPopup(ctx context.Context, a *dsl.PopupArgs, sr *StepResult) *stepErr {
	sr.Expected = "new page target"
	if a.URLContains != "" {
		sr.Expected += " with url containing " + a.URLContains
	}
	current := ""
	if c := chromedp.FromContext(s.pageCtx); c != nil && c.Target != nil {
		current = string(c.Target.TargetID)
	}
	isNew := func(info *target.Info) bool {
		return info != nil && info.Type == "page" && string(info.TargetID) != current && !s.knownTargets[info.TargetID]
	}
	var popup *target.Info
	for popup == nil {
		select {
		case info := <-s.cap.created:
			if isNew(info) {
				popup = info
			}
		case <-time.After(300 * time.Millisecond):
			// Fallback for browsers that do not emit Target.targetCreated: diff Target.getTargets.
			tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			infos, err := chromedp.Targets(tctx)
			cancel()
			if err == nil {
				for _, info := range infos {
					if isNew(info) {
						popup = info
						break
					}
				}
			}
		case <-ctx.Done():
		}
		if popup == nil && ctx.Err() != nil {
			break
		}
	}
	if popup == nil {
		href := s.currentURL()
		actual := fmt.Sprintf("no new page target; current url %s", href)
		s.res.Capabilities["popup"] = false
		note := fmt.Sprintf("expect_popup: %s did not expose a new target after window.open", s.spec.Browser)
		if s.urlBeforeAction != "" && href != s.urlBeforeAction {
			note += fmt.Sprintf("; the current target navigated from %s to %s instead (same-target window.open)", s.urlBeforeAction, href)
			actual += " (opened in the same target)"
		} else if a.URLContains != "" && strings.Contains(href, a.URLContains) {
			note += "; the current target already shows the popup URL (same-target window.open)"
			actual += " (opened in the same target)"
		}
		s.note("%s", note)
		if s.runCtx.Err() != nil {
			return fail(FailTimeout, "expect_popup: run timeout exceeded").with(sr.Expected, actual)
		}
		return fail(FailPopup, "expect_popup: %s", actual).with(sr.Expected, actual)
	}
	s.res.Capabilities["popup"] = true
	popupCtx, cancelPopup := chromedp.NewContext(s.mainCtx, chromedp.WithTargetID(popup.TargetID))
	s.cap.listenTarget(popupCtx)
	if err := runWithTimeout(popupCtx, ctx, 10*time.Second); err != nil {
		cancelPopup()
		return s.classifyCDPErr(err, FailPopup, "expect_popup attach")
	}
	s.closers = append(s.closers, cancelPopup)
	s.knownTargets[popup.TargetID] = true
	s.pageCtx = popupCtx
	if a.URLContains != "" {
		pctx, cancel := context.WithTimeout(popupCtx, remaining(ctx))
		defer cancel()
		var href string
		err := poll(pctx, func() (bool, error) {
			raw, err := evalJSON(pctx, `String(location.href)`)
			if err != nil {
				return false, err
			}
			_ = json.Unmarshal(raw, &href)
			return strings.Contains(href, a.URLContains), nil
		})
		if err != nil {
			actual := fmt.Sprintf("popup url %s", href)
			return fail(FailPopup, "expect_popup: %s; %s", sr.Expected, actual).with(sr.Expected, actual)
		}
		sr.Actual = fmt.Sprintf("switched to popup %s url=%s", popup.TargetID, href)
		return nil
	}
	sr.Actual = fmt.Sprintf("switched to popup %s url=%s", popup.TargetID, s.currentURL())
	return nil
}

func remaining(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		if r := time.Until(d); r > 0 {
			return r
		}
		return time.Millisecond
	}
	return defaultStepTimeout
}

func (s *session) doEval(ctx context.Context, a *dsl.EvalArgs, sr *StepResult) *stepErr {
	var raw json.RawMessage
	err := chromedp.Run(ctx, chromedp.Evaluate(a.Script, &raw, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}))
	if err != nil {
		if a.Expect != "" {
			sr.Expected = a.Expect
			return s.classifyCDPErr(err, FailAssertion, "eval")
		}
		return s.classifyCDPErr(err, FailLocator, "eval")
	}
	got := strings.TrimSpace(string(raw))
	if got == "" {
		got = "undefined"
	}
	sr.Actual = truncate(got, 300)
	if a.Expect == "" {
		return nil
	}
	sr.Expected = a.Expect
	var want, have any
	if json.Unmarshal([]byte(a.Expect), &want) != nil {
		// Expect is not JSON: compare against the string form of the result.
		var str string
		if json.Unmarshal(raw, &str) == nil && str == a.Expect {
			return nil
		}
		return fail(FailAssertion, "eval: expected %s, got %s", a.Expect, sr.Actual).with(a.Expect, sr.Actual)
	}
	if err := json.Unmarshal(raw, &have); err != nil || !reflect.DeepEqual(want, have) {
		return fail(FailAssertion, "eval: expected %s, got %s", a.Expect, sr.Actual).with(a.Expect, sr.Actual)
	}
	return nil
}

// doScreenshot stores a named PNG. An unsupported screenshot is a note, not a
// failure: it is evidence, not part of the business oracle.
func (s *session) doScreenshot(ctx context.Context, name string, sr *StepResult) *stepErr {
	if s.spec.EvidenceDir == "" {
		sr.Actual = "skipped (no evidence dir)"
		return nil
	}
	var buf []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(c)
		buf = b
		return err
	}))
	if err != nil {
		if ctx.Err() != nil {
			return s.classifyCDPErr(err, FailBrowserProtocol, "screenshot")
		}
		s.res.Capabilities["screenshot"] = false
		s.note("screenshot %q unsupported on %s: %v", name, s.spec.Browser, err)
		sr.Actual = "unsupported: " + err.Error()
		return nil
	}
	s.res.Capabilities["screenshot"] = true
	file := artifactName(name, fmt.Sprintf("screenshot-%d", sr.Index))
	path := filepath.Join(s.spec.EvidenceDir, file)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fail(FailInternal, "screenshot: write %s: %v", path, err)
	}
	s.res.Artifacts["screenshot:"+strings.TrimSuffix(file, ".png")] = path
	sr.Actual = path
	return nil
}
