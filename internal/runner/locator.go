package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"vigil/internal/dsl"
)

// refAttr marks the element resolved for the current step so native CDP
// actions can address it with a plain CSS selector.
const refAttr = "data-vigil-ref"

// locatorSpec is the JSON handed to the in-page resolver.
type locatorSpec struct {
	By    string `json:"by"`
	Role  string `json:"role,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
	Text  string `json:"text,omitempty"`
	Exact bool   `json:"exact,omitempty"`
	Nth   int    `json:"nth"`
	Ref   string `json:"ref"`
}

// resolution is what the in-page resolver reports.
type resolution struct {
	Count   int    `json:"count"`
	Found   bool   `json:"found"`
	Visible bool   `json:"visible"`
	Tag     string `json:"tag,omitempty"`
	Text    string `json:"text,omitempty"`
	Error   string `json:"error,omitempty"`
	By      string `json:"by,omitempty"` // strategy that produced the match (auto mode)
}

// resolverJS implements the PRD §10 locator order in the page:
// test_id → role+name → label → id → text → href → css. It marks the nth match
// with data-vigil-ref=<ref> and returns a resolution object.
const resolverJS = `(function(spec){
  var norm = function(s){ return (s==null?'':String(s)).replace(/\s+/g,' ').trim(); };
  var matchText = function(hay, needle, exact){
    hay = norm(hay); needle = norm(needle);
    return exact ? hay === needle : hay.toLowerCase().indexOf(needle.toLowerCase()) >= 0;
  };
  var isVisible = function(el){
    if (!el || !el.isConnected) return false;
    if (el.hidden) return false;
    var cs = null; try { cs = getComputedStyle(el); } catch (e) {}
    if (cs && (cs.display === 'none' || cs.visibility === 'hidden')) return false;
    if (typeof el.getClientRects === 'function' && el.getClientRects().length === 0) return false;
    return true;
  };
  var accName = function(el){
    var lb = el.getAttribute('aria-labelledby');
    if (lb) {
      var t = lb.split(/\s+/).map(function(id){ var r = document.getElementById(id); return r ? norm(r.textContent) : ''; }).filter(Boolean).join(' ');
      if (t) return t;
    }
    var al = el.getAttribute('aria-label'); if (al && norm(al)) return norm(al);
    if (el.labels && el.labels.length) {
      var lt = Array.prototype.map.call(el.labels, function(l){ return norm(l.textContent); }).filter(Boolean).join(' ');
      if (lt) return lt;
    }
    var tag = el.tagName.toLowerCase();
    if (tag === 'img' || tag === 'area') return norm(el.getAttribute('alt'));
    if (tag === 'input') {
      var ty = (el.getAttribute('type') || '').toLowerCase();
      if (ty === 'button' || ty === 'submit' || ty === 'reset') return norm(el.value || (ty === 'submit' ? 'Submit' : ty === 'reset' ? 'Reset' : ''));
      if (ty === 'image') return norm(el.getAttribute('alt'));
      var ph = el.getAttribute('placeholder'); if (ph) return norm(ph);
    }
    var t2 = norm(el.textContent);
    if (!t2) { t2 = norm(Array.prototype.map.call(el.querySelectorAll('img[alt]'), function(i){ return i.getAttribute('alt'); }).join(' ')); }
    if (!t2) t2 = norm(el.getAttribute('title'));
    return t2;
  };
  var roleSelectors = {
    button: 'button, [role=button], input[type=button], input[type=submit], input[type=reset], input[type=image], summary',
    link: 'a[href], area[href], [role=link]',
    textbox: 'input:not([type]), input[type=text], input[type=email], input[type=password], input[type=search], input[type=tel], input[type=url], textarea, [role=textbox], [contenteditable=true]',
    searchbox: 'input[type=search], [role=searchbox]',
    checkbox: 'input[type=checkbox], [role=checkbox]',
    radio: 'input[type=radio], [role=radio]',
    combobox: 'select:not([multiple]), [role=combobox]',
    listbox: 'select[multiple], [role=listbox]',
    option: 'option, [role=option]',
    heading: 'h1, h2, h3, h4, h5, h6, [role=heading]',
    img: 'img, [role=img]',
    list: 'ul, ol, [role=list]',
    listitem: 'li, [role=listitem]',
    tab: '[role=tab]', tablist: '[role=tablist]', tabpanel: '[role=tabpanel]',
    dialog: 'dialog, [role=dialog]',
    menu: '[role=menu]', menuitem: '[role=menuitem]',
    navigation: 'nav, [role=navigation]', main: 'main, [role=main]',
    banner: 'header, [role=banner]', contentinfo: 'footer, [role=contentinfo]',
    table: 'table, [role=table]', row: 'tr, [role=row]', cell: 'td, [role=cell]', columnheader: 'th, [role=columnheader]',
    form: 'form, [role=form]', region: 'section, [role=region]', article: 'article, [role=article]',
    'switch': '[role=switch]', slider: 'input[type=range], [role=slider]', spinbutton: 'input[type=number], [role=spinbutton]',
    progressbar: 'progress, [role=progressbar]', separator: 'hr, [role=separator]', group: 'fieldset, [role=group]',
    alert: '[role=alert]', status: '[role=status]'
  };
  var q = function(sel){ try { return Array.prototype.slice.call(document.querySelectorAll(sel)); } catch (e) { return null; } };
  var byRole = function(role, name, exact){
    var els = q(roleSelectors[role] || ('[role="' + role + '"]')) || [];
    els = els.filter(function(el){ var r = el.getAttribute('role'); return !r || r === role; });
    if (name) els = els.filter(function(el){ return matchText(accName(el), name, exact); });
    return els;
  };
  var byLabel = function(name, exact){
    var out = [];
    (q('label') || []).forEach(function(l){
      if (!matchText(l.textContent, name, exact)) return;
      var c = l.control || (l.htmlFor ? document.getElementById(l.htmlFor) : null) || l.querySelector('input, select, textarea, button');
      if (c && out.indexOf(c) < 0) out.push(c);
    });
    (q('[aria-label], [aria-labelledby], [placeholder]') || []).forEach(function(el){
      if (out.indexOf(el) >= 0) return;
      if (matchText(accName(el), name, exact)) out.push(el);
    });
    return out;
  };
  var byText = function(text, exact){
    var all = q('body *') || [];
    var matches = [];
    for (var i = 0; i < all.length; i++) {
      var el = all[i], tag = el.tagName;
      if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'NOSCRIPT' || tag === 'TEMPLATE') continue;
      if (matchText(el.textContent, text, exact)) matches.push(el);
    }
    // innermost only: drop elements that have an element child which also matches
    return matches.filter(function(el){
      for (var k = 0; k < el.children.length; k++) { if (matchText(el.children[k].textContent, text, exact)) return false; }
      return true;
    });
  };
  var byHref = function(value, exact){
    return (q('a[href], area[href]') || []).filter(function(a){
      var raw = a.getAttribute('href') || '';
      return exact ? (raw === value || a.href === value) : (raw.indexOf(value) >= 0 || a.href.indexOf(value) >= 0);
    });
  };
  var byTestID = function(v){ return q('[data-testid="' + v.replace(/"/g, '\\"') + '"], [data-test-id="' + v.replace(/"/g, '\\"') + '"]') || []; };
  var byID = function(v){ var el = document.getElementById(v); return el ? [el] : []; };
  var byCSS = function(v){ var r = q(v); if (r === null) throw new Error('invalid css selector: ' + v); return r; };

  var els = null, used = spec.by;
  switch (spec.by) {
    case 'test_id': els = byTestID(spec.value); break;
    case 'role': els = byRole(spec.role, spec.name, spec.exact); break;
    case 'label': els = byLabel(spec.name || spec.text || spec.value, spec.exact); break;
    case 'id': els = byID(spec.value); break;
    case 'text': els = byText(spec.text || spec.value, spec.exact); break;
    case 'href': els = byHref(spec.value, spec.exact); break;
    case 'css': els = byCSS(spec.value); break;
    default:
      if (spec.role) { els = byRole(spec.role, spec.name, spec.exact); used = 'role'; }
      else if (spec.text) { els = byText(spec.text, spec.exact); used = 'text'; }
      else if (spec.name) { els = byLabel(spec.name, spec.exact); used = 'label'; }
      else {
        els = byTestID(spec.value); used = 'test_id';
        if (!els.length) { els = byID(spec.value); used = 'id'; }
        if (!els.length) { els = byCSS(spec.value); used = 'css'; }
      }
  }
  Array.prototype.forEach.call(document.querySelectorAll('[` + refAttr + `]'), function(e){ e.removeAttribute('` + refAttr + `'); });
  var res = { count: els.length, found: false, visible: false, by: used };
  if (!els.length) return res;
  var nth = spec.nth || 0;
  if (nth >= els.length) { res.error = 'nth=' + nth + ' out of range (matched ' + els.length + ')'; return res; }
  var el = els[nth];
  el.setAttribute('` + refAttr + `', spec.ref);
  res.found = true; res.visible = isVisible(el); res.tag = el.tagName.toLowerCase();
  res.text = norm(el.textContent).slice(0, 120);
  return res;
})`

func specFromLocator(l *dsl.Locator, ref string) locatorSpec {
	return locatorSpec{By: l.By, Role: l.Role, Name: l.Name, Value: l.Value, Text: l.Text, Exact: l.Exact, Nth: l.Nth, Ref: ref}
}

// describeLocator renders a locator for messages (never contains secrets).
func describeLocator(l *dsl.Locator) string {
	if l == nil {
		return "<nil>"
	}
	var parts []string
	if l.By != "" {
		parts = append(parts, "by="+l.By)
	}
	if l.Role != "" {
		parts = append(parts, "role="+l.Role)
	}
	if l.Name != "" {
		parts = append(parts, fmt.Sprintf("name=%q", l.Name))
	}
	if l.Text != "" {
		parts = append(parts, fmt.Sprintf("text=%q", l.Text))
	}
	if l.Value != "" {
		parts = append(parts, fmt.Sprintf("value=%q", l.Value))
	}
	if l.Exact {
		parts = append(parts, "exact")
	}
	if l.Nth != 0 {
		parts = append(parts, fmt.Sprintf("nth=%d", l.Nth))
	}
	return strings.Join(parts, " ")
}

// resolveOnce runs the resolver in the page a single time.
func resolveOnce(ctx context.Context, spec locatorSpec) (*resolution, error) {
	arg, _ := json.Marshal(spec)
	var raw json.RawMessage
	if err := chromedp.Run(ctx, chromedp.Evaluate(resolverJS+"("+string(arg)+")", &raw)); err != nil {
		return nil, err
	}
	var r resolution
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("resolver returned %s: %w", string(raw), err)
	}
	return &r, nil
}

// waitResolve polls the resolver until pred accepts the resolution or ctx expires.
// It returns the last resolution alongside a timeout error so callers can report
// what was observed (count, visibility) instead of a bare timeout.
func waitResolve(ctx context.Context, spec locatorSpec, pred func(*resolution) bool) (*resolution, error) {
	var last *resolution
	for {
		r, err := resolveOnce(ctx, spec)
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			return nil, err
		}
		last = r
		if pred(r) {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func refSelector(ref string) string { return "[" + refAttr + `="` + ref + `"]` }
