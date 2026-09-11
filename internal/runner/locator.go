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
	By      string `json:"by,omitempty"`   // strategy that produced the match (auto mode)
	Note    string `json:"note,omitempty"` // e.g. a text locator that matched an accessible name
	// Candidates lists the accessible names the page offers for the attempted
	// strategy when nothing matched; CandidateKind names that strategy.
	Candidates    []string `json:"candidates,omitempty"`
	CandidateKind string   `json:"candidate_kind,omitempty"`
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
  // accName follows the accessible name computation closely enough to match
  // Chrome for common markup: aria-labelledby -> aria-label -> native label
  // (<label>, alt, input value, <title> in svg) -> name from content, where
  // every descendant contributes its own accessible name (img alt, aria-label,
  // text) and aria-hidden/hidden subtrees are skipped -> title attribute.
  // Plain DOM APIs only: Lightpanda exposes no accessibility tree.
  var nameHidden = function(el){
    if (!el) return true;
    if (el.getAttribute && el.getAttribute('aria-hidden') === 'true') return true;
    if (el.hidden) return true;
    var cs = null; try { cs = getComputedStyle(el); } catch (e) {}
    if (cs && (cs.display === 'none' || cs.visibility === 'hidden')) return true;
    return false;
  };
  var labelsOf = function(el){
    var out = [];
    try { if (el.labels && el.labels.length) out = Array.prototype.slice.call(el.labels); } catch (e) {}
    if (!out.length && el.id) out = q('label[for="' + String(el.id).replace(/"/g, '\\"') + '"]') || [];
    if (!out.length && el.closest) { var p = null; try { p = el.closest('label'); } catch (e) {} if (p) out = [p]; }
    return out;
  };
  var nameFrom = function(el, depth, chain){
    if (!el || !el.tagName || depth > 20 || chain.indexOf(el) >= 0) return '';
    chain.push(el);
    try {
      var get = function(a){ return el.getAttribute ? el.getAttribute(a) : null; };
      if (depth === 0) {
        var lb = get('aria-labelledby');
        if (lb) {
          var t = lb.split(/\s+/).map(function(id){
            var r = document.getElementById(id);
            return r ? (nameFrom(r, depth + 1, chain) || norm(r.textContent)) : '';
          }).filter(Boolean).join(' ');
          if (norm(t)) return norm(t);
        }
      }
      var al = get('aria-label'); if (al && norm(al)) return norm(al);
      var tag = el.tagName.toLowerCase();
      if (tag === 'input' || tag === 'select' || tag === 'textarea') {
        var lt = labelsOf(el).map(function(l){ return norm(l.textContent); }).filter(Boolean).join(' ');
        if (lt) return lt;
      }
      if (tag === 'img' || tag === 'area') { var alt = get('alt'); if (alt !== null) return norm(alt); }
      if (tag === 'input') {
        var ty = (get('type') || '').toLowerCase();
        if (ty === 'button' || ty === 'submit' || ty === 'reset') return norm(el.value || (ty === 'submit' ? 'Submit' : ty === 'reset' ? 'Reset' : ''));
        if (ty === 'image') { var ialt = get('alt'); if (ialt !== null && norm(ialt)) return norm(ialt); return norm(get('title') || 'Submit'); }
        var ph = get('placeholder'); if (ph && norm(ph)) return norm(ph);
      }
      if (tag === 'svg' && el.querySelector) { var ti = el.querySelector('title'); if (ti && norm(ti.textContent)) return norm(ti.textContent); }
      if (tag === 'fieldset' && el.querySelector) { var lg = el.querySelector('legend'); if (lg && norm(lg.textContent)) return norm(lg.textContent); }
      var parts = [], kids = el.childNodes || [];
      for (var i = 0; i < kids.length; i++) {
        var n = kids[i];
        if (n.nodeType === 3) { var tx = norm(n.nodeValue); if (tx) parts.push(tx); continue; }
        if (n.nodeType !== 1) continue;
        var kt = (n.tagName || '').toLowerCase();
        if (kt === 'script' || kt === 'style' || kt === 'noscript' || kt === 'template') continue;
        if (nameHidden(n)) continue;
        var sub = nameFrom(n, depth + 1, chain);
        if (sub) parts.push(sub);
      }
      var content = norm(parts.join(' '));
      if (content) return content;
      var tt = get('title'); if (tt && norm(tt)) return norm(tt);
      return '';
    } finally { chain.pop(); }
  };
  var accName = function(el){ return nameFrom(el, 0, []); };
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
  var nameMatched = false;
  var byText = function(text, exact){
    var all = (q('body *') || []).filter(function(el){
      var tag = el.tagName;
      return tag !== 'SCRIPT' && tag !== 'STYLE' && tag !== 'NOSCRIPT' && tag !== 'TEMPLATE';
    });
    // innermost only: drop elements that have an element child which also matches
    var pick = function(get){
      return all.filter(function(el){ return matchText(get(el), text, exact); }).filter(function(el){
        for (var k = 0; k < el.children.length; k++) { if (matchText(get(el.children[k]), text, exact)) return false; }
        return true;
      });
    };
    var out = pick(function(el){ return el.textContent; });
    if (out.length) return out;
    // Agent snapshots show accessible names, so a text locator copied from one
    // must still find aria-label / alt only elements.
    out = pick(function(el){ return accName(el); });
    if (out.length) nameMatched = true;
    return out;
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

  // candidatesFor lists what the page actually offers when a semantic locator
  // matched nothing, so a repair can pick a name that exists.
  var describeName = function(el){
    var n = accName(el);
    if (n) return n;
    var t = norm((el.getAttribute && (el.getAttribute('title') || el.getAttribute('placeholder'))) || '');
    return t || ('<' + el.tagName.toLowerCase() + '>');
  };
  var candidatesFor = function(used, spec){
    var out = [], seen = {};
    var push = function(s){ s = norm(s).slice(0, 80); if (!s || seen[s]) return; seen[s] = true; out.push(s); };
    if (used === 'role') {
      var peers = byRole(spec.role, '', false);
      peers.forEach(function(el){ if (isVisible(el)) push(describeName(el)); });
      if (!out.length) peers.forEach(function(el){ push(describeName(el)); });
      return out.slice(0, 8);
    }
    var all = q('body *') || [];
    for (var i = 0; i < all.length && out.length < 8; i++) {
      var el = all[i], tag = el.tagName;
      if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'NOSCRIPT' || tag === 'TEMPLATE') continue;
      if (el.children && el.children.length) continue; // innermost text carriers only
      if (!isVisible(el)) continue;
      push(accName(el) || norm(el.textContent));
    }
    return out.slice(0, 8);
  };

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
  if (!els.length) {
    if (used === 'role' || used === 'text' || used === 'label') {
      res.candidate_kind = used === 'role' ? 'role=' + spec.role : used;
      try { res.candidates = candidatesFor(used, spec); } catch (e) { res.candidates = []; }
    }
    return res;
  }
  var nth = spec.nth || 0;
  if (nth >= els.length) { res.error = 'nth=' + nth + ' out of range (matched ' + els.length + ')'; return res; }
  var el = els[nth];
  el.setAttribute('` + refAttr + `', spec.ref);
  res.found = true; res.visible = isVisible(el); res.tag = el.tagName.toLowerCase();
  res.text = norm(el.textContent).slice(0, 120);
  if (nameMatched) {
    res.note = 'matched by accessible name';
    res.text = (res.text ? res.text + ' ' : '') + '(matched by accessible name)';
  }
  return res;
})`

// maxCandidatesText bounds the candidate list appended to a locator miss so
// evidence and the repair prompt stay readable.
const maxCandidatesText = 600

// candidatesText renders the resolver's candidate names as a suffix for a
// "0 matches" message, e.g.
//
//	; candidates(role=button): "과제 과제", "학급 분석"
//
// It returns "" when the resolver reported none.
func candidatesText(r *resolution) string {
	if r == nil || len(r.Candidates) == 0 {
		return ""
	}
	kind := r.CandidateKind
	if kind == "" {
		kind = r.By
	}
	head := "; candidates(" + kind + "): "
	var b strings.Builder
	b.WriteString(head)
	for i, c := range r.Candidates {
		item := fmt.Sprintf("%q", c)
		if i > 0 {
			item = ", " + item
		}
		if b.Len()+len(item)+len(", \u2026") > maxCandidatesText {
			b.WriteString(", \u2026")
			break
		}
		b.WriteString(item)
	}
	if b.Len() == len(head) {
		return ""
	}
	return b.String()
}

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
