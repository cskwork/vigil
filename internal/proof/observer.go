package proof

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type response struct {
	ID             network.RequestID
	URL, Method    string
	Sent, Received time.Time
	Status         int64
	Body           []byte
	Error          string
}
type observation struct {
	activeAction string
	evidenceDir  string

	a           *Attempt
	ctx         context.Context
	cancel      context.CancelFunc
	repo        *Repository
	mu          sync.Mutex
	responses   map[network.RequestID]*response
	actionTimes map[string]time.Time
	completed   map[string]bool
	results     map[string]CriterionResult
	blocked     bool
	probes      *ProbeRunner
	before      map[string][]map[string]Value
}

func (o *observation) Setup(ctx context.Context) error {
	o.responses = map[network.RequestID]*response{}
	o.actionTimes = map[string]time.Time{}
	o.completed = map[string]bool{}
	o.results = map[string]CriterionResult{}
	o.before = map[string][]map[string]Value{}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *fetch.EventRequestPaused:
			go func() {
				if e.ResponseStatusCode != 0 {
					headers := append([]*fetch.HeaderEntry{}, e.ResponseHeaders...)
					headers = append(headers, &fetch.HeaderEntry{Name: "Content-Security-Policy", Value: "sandbox allow-scripts allow-same-origin allow-forms; frame-src 'none'; object-src 'none'; worker-src 'none'"})
					_ = chromedp.Run(ctx, fetch.ContinueResponse(e.RequestID).WithResponseCode(e.ResponseStatusCode).WithResponseHeaders(headers))
					return
				}
				allowed := Allowed(e.Request.URL, e.Request.Method, o.a.Target.Scope)
				if e.Request.Method != "GET" && e.Request.Method != "HEAD" && e.Request.Method != "OPTIONS" {
					o.mu.Lock()
					active := o.activeAction
					o.mu.Unlock()
					approved := false
					if o.a.Target.Actions[active].Mutating {
						for _, ob := range o.a.Target.Observers {
							if ob.Action == active && ob.WriteAPI != nil && Allowed(e.Request.URL, e.Request.Method, []ScopeRule{*ob.WriteAPI}) {
								approved = true
							}
						}
					}
					allowed = allowed && approved
				}
				if allowed {
					_ = chromedp.Run(ctx, fetch.ContinueRequest(e.RequestID))
				} else {
					o.mu.Lock()
					o.blocked = true
					o.mu.Unlock()
					_ = chromedp.Run(ctx, fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient))
				}
			}()
		case *network.EventRequestWillBeSent:
			o.mu.Lock()
			o.responses[e.RequestID] = &response{ID: e.RequestID, URL: e.Request.URL, Method: e.Request.Method, Sent: time.Now().UTC()}
			o.mu.Unlock()
		case *network.EventResponseReceived:
			o.mu.Lock()
			if r := o.responses[e.RequestID]; r != nil {
				r.Received = time.Now().UTC()
				r.Status = e.Response.Status
			}
			o.mu.Unlock()
		case *network.EventLoadingFinished:
			go func() {
				var body []byte
				err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
					var err error
					body, err = network.GetResponseBody(e.RequestID).Do(c)
					return err
				}))
				o.mu.Lock()
				defer o.mu.Unlock()
				if r := o.responses[e.RequestID]; r != nil {
					if err != nil {
						r.Error = "response body unavailable"
					} else {
						r.Body = body
					}
				}
			}()
		}
	})
	// Approved flows cannot open a second page or register a service worker that bypasses interception.
	return chromedp.Run(ctx, network.Enable(), network.SetBypassServiceWorker(true), fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}, {URLPattern: "*", ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageResponse}}), chromedp.ActionFunc(func(c context.Context) error {
		_, e := page.AddScriptToEvaluateOnNewDocument(`Object.defineProperty(window,'open',{value:()=>null,writable:false});document.addEventListener('click',e=>{let a=e.target.closest?.('a');if(a&&a.target&&a.target!=='_self')e.preventDefault()},true);`).Do(c)
		return e
	}))
}
func (o *observation) BeforeStep(ctx context.Context, index int, name string) error {
	if o.ctx.Err() != nil {
		return o.ctx.Err()
	}
	a, e := o.repo.Attempt(o.ctx, o.a.ID)
	if e != nil {
		return e
	}
	if a.CancelRequested {
		o.cancel()
		return fmt.Errorf("cancelled")
	}
	action := strings.Split(name, ":")[0]
	o.mu.Lock()
	o.activeAction = action
	_, seen := o.actionTimes[action]
	if !seen {
		o.actionTimes[action] = time.Now().UTC()
	}
	blocked := o.blocked
	o.mu.Unlock()
	if blocked {
		return fmt.Errorf("unapproved request was blocked")
	}
	if !seen {
		for _, c := range o.a.Contract.Criteria {
			ob, ok := o.a.Target.Observers[c.Observer]
			if ok && ob.Action == action && ob.Kind == "mysql" && o.probes != nil {
				rows, e := o.probes.Read(o.ctx, ob.Probe, o.a.Fixture.Entity)
				if e == nil {
					o.before[c.ID] = rows
				}
			}
		}
	}
	if o.a.ObservedVersion == "" && o.a.Target.VersionSelector != "" {
		o.a.ObservedVersion = o.version(ctx)
	}
	o.a.Progress = action
	return o.repo.Update(o.ctx, o.a)
}
func (o *observation) AfterStep(ctx context.Context, index int, name string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !ok {
		return
	}
	if !strings.HasSuffix(name, ":observe") {
		return
	}
	action := strings.TrimSuffix(name, ":observe")
	o.completed[action] = true
	for _, c := range o.a.Contract.Criteria {
		ob, exists := o.a.Target.Observers[c.Observer]
		if !exists || ob.Action != action {
			continue
		}
		r := CriterionResult{ID: c.ID, Required: c.Required, Status: "UNKNOWN", Evidence: []Evidence{}}
		var actual Value
		var reason string
		var details json.RawMessage
		switch ob.Kind {
		case "dom":
			sel, _ := json.Marshal(ob.Selector)
			prop, _ := json.Marshal(ob.Property)
			script := fmt.Sprintf(`(()=>{const es=document.querySelectorAll(%s);if(es.length!==1)return {error:'observer requires exactly one element'};const e=es[0],p=%s;const v=p==='text'?e.textContent:e[p];return {present:v!==undefined,data:v===undefined?null:v}})()`, sel, prop)
			var out struct {
				Present bool            `json:"present"`
				Data    json.RawMessage `json:"data"`
				Error   string          `json:"error"`
			}
			if e := chromedp.Run(ctx, chromedp.Evaluate(script, &out)); e != nil {
				reason = "DOM observer unavailable"
			} else if out.Error != "" {
				reason = out.Error
			} else {
				actual = Value{out.Present, out.Data}
			}
		case "network":
			actual, reason, details = o.networkValue(ctx, ob)
		case "mysql":
			if o.probes == nil {
				reason = "DB probe unavailable"
			} else if before, ok := o.before[c.ID]; !ok {
				reason = "before-action DB evidence unavailable"
			} else {
				after, e := o.probes.Read(o.ctx, ob.Probe, o.a.Fixture.Entity)
				if e != nil {
					reason = "after-action DB evidence unavailable"
				} else {
					details = json.RawMessage(encode(map[string]any{"before": before, "after": after, "baseline_kind": "before-action"}))
					b, _ := json.Marshal(Hash(before) == Hash(after))
					actual = Value{true, b}
				}
			}
		default:
			reason = "observer unavailable"
		}
		if reason == "" && !actual.Present && c.Expected.Present {
			reason = "expected property missing"
		}
		if reason == "" {
			if Equal(actual, c.Expected) {
				r.Status = "PASS"
			} else {
				r.Status = "FAIL"
				reason = "observed value differs from approved expectation"
			}
		}
		r.Reason = reason
		r.Evidence = []Evidence{{Details: details, ID: id(), Attempt: o.a.ID, Criterion: c.ID, Action: action, Persona: o.a.Contract.Persona, Entity: o.a.Fixture.Entity, At: time.Now().UTC(), Source: ob.Kind, Actual: actual, Status: r.Status, Reason: reason}}
		for i := range r.Evidence {
			r.Evidence[i].Actual.Data = redactJSON(r.Evidence[i].Actual.Data, o.a.Target)
			r.Evidence[i].Details = redactJSON(r.Evidence[i].Details, o.a.Target)
		}
		if ob.Kind == "dom" && o.evidenceDir != "" && len(r.Evidence) > 0 && bytes.Equal(actual.Data, r.Evidence[0].Actual.Data) {
			sel, _ := json.Marshal(ob.Selector)
			var safe bool
			_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(()=>{const e=document.querySelector(%s);return !!e&&!e.matches("input,textarea,[contenteditable]")&&!e.querySelector("input,textarea,[contenteditable]")})()`, sel), &safe))
			if !safe {
				persistCriterion(o.evidenceDir, o.a.ID, &r)
				o.results[c.ID] = r
				continue
			}
			var shot []byte
			dir := filepath.Join(o.evidenceDir, o.a.ID)
			if os.MkdirAll(dir, 0700) == nil {
				if e := chromedp.Run(ctx, chromedp.Screenshot(ob.Selector, &shot, chromedp.ByQuery)); e == nil {
					file := r.Evidence[0].ID + ".png"
					if os.WriteFile(filepath.Join(dir, file), shot, 0600) == nil {
						r.Evidence[0].Screenshot = file
					}
				}
			}
		}
		persistCriterion(o.evidenceDir, o.a.ID, &r)
		o.results[c.ID] = r
	}
	o.flush()
}
func (o *observation) networkValue(ctx context.Context, ob Observer) (Value, string, json.RawMessage) {
	if ob.EntityPath == "" {
		return Value{}, "entity observer unavailable", nil
	}
	if ob.API == nil {
		return Value{}, "network observer not configured", nil
	}
	if ob.Reread && (ob.API.Method != "GET" || ob.WriteAPI == nil) {
		return Value{}, "registered save and reread correlation unavailable", nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if o.ctx.Err() != nil {
			return Value{}, "cancelled", nil
		}
		o.mu.Lock()
		start := o.actionTimes[ob.Action]
		var matches []response
		for _, r := range o.responses {
			if Allowed(r.URL, r.Method, []ScopeRule{*ob.API}) && r.Sent.After(start) && r.Received.After(r.Sent) {
				matches = append(matches, *r)
			}
		}
		o.mu.Unlock()
		if len(matches) > 1 {
			return Value{}, "ambiguous correlated responses", nil
		}
		if len(matches) == 1 {
			r := matches[0]
			if ob.Reread {
				o.mu.Lock()
				var writes []response
				for _, w := range o.responses {
					if Allowed(w.URL, w.Method, []ScopeRule{*ob.WriteAPI}) && w.Sent.After(start) && !w.Received.IsZero() && w.Received.Before(r.Sent) {
						writes = append(writes, *w)
					}
				}
				o.mu.Unlock()
				if len(writes) != 1 {
					return Value{}, "save response before reread not established", nil
				}
				write := writes[0]
				success, err := JSONValue(write.Body, ob.SuccessPath)
				if err != nil || !success.Present || !Equal(success, ob.Success) || write.Status < 200 || write.Status >= 300 {
					return Value{}, "save success not established", nil
				}
			}

			if r.Status == 401 || r.Status == 403 {
				return Value{}, "authentication unavailable", nil
			}
			if r.Status >= 500 || r.Error != "" {
				return Value{}, "network evidence unavailable", nil
			}
			if len(r.Body) > 0 {
				if ob.SuccessPath != "" {
					s, e := JSONValue(r.Body, ob.SuccessPath)
					if e != nil || !s.Present {
						return Value{}, "success field missing", nil
					}
					if !Equal(s, ob.Success) {
						return Value{}, "operation success not established", nil
					}
				}
				v, e := JSONValue(r.Body, ob.JSONPath)
				if e != nil {
					return Value{}, "invalid response JSON", nil
				}
				if ob.EntityPath != "" {
					entity, err := JSONValue(r.Body, ob.EntityPath)
					if err != nil || !Equal(entity, Value{true, json.RawMessage(encode(o.a.Fixture.Entity))}) {
						return Value{}, "wrong entity response", nil
					}
				}
				if ob.GenerationPath != "" {
					gen, err := JSONValue(r.Body, ob.GenerationPath)
					if err != nil || !Equal(gen, Value{true, json.RawMessage(encode(o.a.FixtureGeneration))}) {
						return Value{}, "wrong fixture generation", nil
					}
				}
				return v, "", json.RawMessage(encode(map[string]any{"request_id": r.ID, "method": r.Method, "url": r.URL, "status": r.Status, "requested_at": r.Sent, "response_at": r.Received, "session": o.a.ID, "persona": o.a.Contract.Persona, "entity": o.a.Fixture.Entity}))
			}
		}
		if time.Now().After(deadline) {
			return Value{}, "correlated response missing", nil
		}
		select {
		case <-o.ctx.Done():
			return Value{}, "cancelled", nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func JSONValue(body []byte, path string) (Value, error) {
	var v any
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.UseNumber()
	if e := d.Decode(&v); e != nil {
		return Value{}, e
	}
	for _, k := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		if k == "" || k == "$" {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return Value{Present: false}, nil
		}
		v, ok = m[k]
		if !ok {
			return Value{Present: false}, nil
		}
	}
	b, e := json.Marshal(v)
	return Value{true, b}, e
}
func (o *observation) flush() {
	o.a.Results = []CriterionResult{}
	for _, c := range o.a.Contract.Criteria {
		r, ok := o.results[c.ID]
		if !ok {
			r = CriterionResult{ID: c.ID, Required: c.Required, Status: "UNKNOWN", Reason: "required observation not reached", Evidence: []Evidence{}}
		}
		o.a.Results = append(o.a.Results, r)
	}
	_ = o.repo.Update(context.Background(), o.a)
}
func (o *observation) Finish(ctx context.Context) {
	o.a.VersionAfter = o.version(ctx)
	o.mu.Lock()
	blocked := o.blocked
	o.mu.Unlock()
	if blocked {
		for _, c := range o.a.Contract.Criteria {
			r, ok := o.results[c.ID]
			if !ok {
				r = CriterionResult{ID: c.ID, Required: c.Required, Evidence: []Evidence{}}
			}
			if r.Status != "FAIL" {
				r.Status = "UNKNOWN"
				r.Reason = "unapproved browser request blocked"
				o.results[c.ID] = r
			}
		}
	}
	o.flush()
}
func (o *observation) version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if o.a.Target.VersionSelector == "" {
		return ""
	}
	sel, _ := json.Marshal(o.a.Target.VersionSelector)
	var v string
	_ = chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`(()=>{const e=document.querySelector(%s);return e?e.textContent.trim():""})()`, sel), &v))
	return v
}
