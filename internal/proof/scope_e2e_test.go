package proof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"vigil/internal/dsl"
)

// This test owns both loopback targets, the temporary SQLite database, and the
// Chromium profile. It never attaches to an operator browser or shared demo.
func TestProofBrowserRejectsRedirectOutsideScope(t *testing.T) {
	if os.Getenv("PROOF_BROWSER_E2E") != "1" {
		t.Skip("set PROOF_BROWSER_E2E=1 for an isolated Chromium scope test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := setup(t)
	var forbiddenRequests atomic.Int64
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbiddenRequests.Add(1)
		w.Write([]byte(`<p id="value">unexpected success</p>`))
	}))
	defer forbidden.Close()
	var attemptID string
	var ownedPID atomic.Int64
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/start" {
			http.NotFound(w, r)
			return
		}
		raw, err := st.GetState(r.Context(), "proof:browser:"+attemptID)
		if err == nil {
			var owned ownedBrowser
			if json.Unmarshal([]byte(raw), &owned) == nil {
				ownedPID.Store(int64(owned.PID))
			}
		}
		http.Redirect(w, r, forbidden.URL+"/escape", http.StatusFound)
	}))
	defer allowed.Close()
	reg := testRegistry()
	target := reg.Targets["qa"]
	target.BaseURL = allowed.URL + "/start"
	target.Scope = []ScopeRule{{Origin: allowed.URL, Method: "GET", Path: "/start"}}
	target.Actions["read"] = Action{Title: "registered read-only navigation", Steps: []dsl.Step{{Goto: allowed.URL + "/start"}}}
	target.Observers["dom"] = Observer{Kind: "dom", Action: "read", Selector: "#value", Property: "text"}
	reg.Targets["qa"] = target
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(st, reg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	c, err := svc.Repo.Create(ctx, "qa", "zero", "test-operator")
	if err != nil {
		t.Fatal(err)
	}
	c, err = svc.Repo.Patch(ctx, c.ID, c.RowVersion, contract())
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := svc.Repo.Approve(ctx, c.ID, Approval{RegistryHash: Hash(target), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "redirect-scope"}, "test-operator", reg)
	if err != nil {
		t.Fatal(err)
	}
	attemptID = a.ID
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	var monitor sync.WaitGroup
	var peakRSS atomic.Int64
	monitor.Add(1)
	go func() {
		defer monitor.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-ticker.C:
				raw, _ := st.GetState(monitorCtx, "proof:browser:"+attemptID)
				var owned ownedBrowser
				if json.Unmarshal([]byte(raw), &owned) != nil || owned.PID <= 1 {
					continue
				}
				out, err := exec.CommandContext(monitorCtx, "ps", "-p", strconv.Itoa(owned.PID), "-o", "rss=").Output()
				if err != nil {
					continue
				}
				rss, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
				for rss > peakRSS.Load() && !peakRSS.CompareAndSwap(peakRSS.Load(), rss) {
				}
			}
		}
	}()
	if err = svc.execute(ctx, a.ID); err != nil {
		stopMonitor()
		monitor.Wait()
		t.Fatal(err)
	}
	stopMonitor()
	monitor.Wait()
	a, err = svc.Repo.Attempt(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count := forbiddenRequests.Load(); count != 0 {
		t.Fatalf("forbidden origin received %d requests", count)
	}
	pid := ownedPID.Load()
	if pid <= 1 {
		t.Fatal("Chromium did not reach the isolated target; verify browser execution permissions")
	}
	if peakRSS.Load() <= 0 {
		t.Fatal("Chromium RSS was not observed during the attempt")
	}
	if a.Verdict != "INCOMPLETE" || len(a.Results) != 1 || a.Results[0].Status != "UNKNOWN" || !strings.Contains(a.Results[0].Reason, "unapproved browser request blocked") {
		t.Fatalf("scope refusal was lost: verdict=%s results=%+v", a.Verdict, a.Results)
	}
	if err = syscall.Kill(int(pid), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned Chromium PID %d remains after attempt: %v", pid, err)
	}
	raw, err := st.GetState(ctx, "proof:browser:"+a.ID)
	if err != nil || raw != "" {
		t.Fatalf("owned browser journal not cleared: %q %v", raw, err)
	}
	t.Logf("redirect blocked before egress; forbidden_requests=0 verdict=%s criterion=%s owned_pid=%d sampled_peak_rss_kib=%d stopped=true", a.Verdict, a.Results[0].Status, pid, peakRSS.Load())
}

func TestPlanObservationBlocksExternalRequestsAndCleansBrowser(t *testing.T) {
	if os.Getenv("PROOF_BROWSER_E2E") != "1" {
		t.Skip("set PROOF_BROWSER_E2E=1 for an isolated Chromium scope test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, _ := setup(t)
	var forbiddenRequests atomic.Int64
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forbiddenRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer forbidden.Close()
	const checkID = "planning-observation"
	var ownedPID atomic.Int64
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := st.GetState(r.Context(), "proof:browser:plan:"+checkID)
		var owned ownedBrowser
		if json.Unmarshal([]byte(raw), &owned) == nil {
			ownedPID.Store(int64(owned.PID))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<button aria-label="저장">저장</button><script>fetch(%q,{method:'POST'})</script>`, forbidden.URL+"/write")
	}))
	defer allowed.Close()
	target := testRegistry().Targets["qa"]
	target.BaseURL = allowed.URL
	target.Scope = []ScopeRule{{Origin: allowed.URL, Method: "GET", Path: "/"}}
	reg := Registry{Targets: map[string]Target{"qa": target}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(st, reg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	result := svc.observeForPlan(ctx, checkID, target)
	if result.Status == "unavailable" || len(result.Controls) != 1 || result.Controls[0].Name != "저장" {
		t.Fatalf("observation=%+v", result)
	}
	if forbiddenRequests.Load() != 0 {
		t.Fatalf("planning observation sent %d forbidden requests", forbiddenRequests.Load())
	}
	pid := ownedPID.Load()
	if pid <= 1 {
		t.Fatal("planning Chromium ownership was not journaled")
	}
	if err = syscall.Kill(int(pid), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("planning Chromium PID %d remains: %v", pid, err)
	}
	if raw, err := st.GetState(ctx, "proof:browser:plan:"+checkID); err != nil || raw != "" {
		t.Fatalf("planning browser journal not cleared: %q %v", raw, err)
	}
}

func TestRegisteredDirectReadUsesApprovedBrowserSession(t *testing.T) {
	if os.Getenv("PROOF_BROWSER_E2E") != "1" {
		t.Skip("set PROOF_BROWSER_E2E=1 for an isolated Chromium scope test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var generation string
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<button id="save">저장</button><p id="status">준비</p><script>document.querySelector('#save').onclick=async()=>{const r=await fetch('/record',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({entity:'qa-1',value:''})});document.querySelector('#status').textContent=r.ok?'저장 완료':'실패'}</script>`)
		case r.Method == "POST" && r.URL.Path == "/reset":
			var input map[string]string
			_ = json.NewDecoder(r.Body).Decode(&input)
			generation = input["attempt"]
			_ = json.NewEncoder(w).Encode(map[string]any{"entity": "qa-1", "attempt": generation, "ready": true})
		case r.Method == "POST" && r.URL.Path == "/record":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "entity": "qa-1", "generation": generation})
		case r.Method == "GET" && r.URL.Path == "/record" && r.URL.Query().Get("entity") == "qa-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "entity": "qa-1", "generation": generation, "value": ""})
		default:
			http.NotFound(w, r)
		}
	}))
	defer targetServer.Close()
	readRule := ScopeRule{Origin: targetServer.URL, Method: "GET", Path: "/record", Query: map[string]string{"entity": "qa-1"}}
	writeRule := ScopeRule{Origin: targetServer.URL, Method: "POST", Path: "/record"}
	target := Target{
		Title: "direct read", BaseURL: targetServer.URL, Environment: "qa", PolicyVersion: "1",
		Scope:       []ScopeRule{{Origin: targetServer.URL, Method: "GET", Path: "/"}, {Origin: targetServer.URL, Method: "POST", Path: "/reset"}, writeRule, readRule},
		Personas:    map[string]Persona{"p": {Account: "qa-p"}},
		Fixtures:    map[string]Fixture{"f": {Entity: "qa-1", QA: true, PrepareURL: targetServer.URL + "/reset"}},
		Actions:     map[string]Action{"save": {Title: "저장", Mutating: true, Steps: []dsl.Step{{Goto: targetServer.URL}, {Click: &dsl.Locator{By: "css", Value: "#save"}}, {WaitFor: &dsl.Locator{By: "text", Text: "저장 완료"}}}}},
		Observers:   map[string]Observer{"api": {Kind: "network", Action: "save", API: &readRule, WriteAPI: &writeRule, JSONPath: "$.value", EntityPath: "$.entity", GenerationPath: "$.generation", SuccessPath: "$.success", Success: Value{Present: true, Data: json.RawMessage(`true`)}, Reread: true, DirectRead: true}},
		Definitions: map[string]Definition{"empty": {Expected: Value{Present: true, Data: json.RawMessage(`""`)}}},
	}
	reg := Registry{Targets: map[string]Target{"qa": target}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	st, _ := setup(t)
	svc, err := NewService(st, reg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	contract := Contract{Persona: "p", Fixture: "f", Actions: []string{"save"}, Criteria: []Criterion{{ID: "api", Title: "재조회 값", Required: true, Observer: "api", Expected: Value{Present: true, Data: json.RawMessage(`""`)}, Source: "definition", SourceRef: "empty"}}}
	check, _ := svc.Repo.Create(ctx, "qa", "direct read", "operator")
	check, err = svc.Repo.Patch(ctx, check.ID, check.RowVersion, contract)
	if err != nil {
		t.Fatal(err)
	}
	attempt, _, err := svc.Repo.Approve(ctx, check.ID, Approval{RegistryHash: Hash(target), Revision: check.CurrentRevision, ContractHash: check.ContractHash, Scope: "qa", IdempotencyKey: "direct-read"}, "operator", reg)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.execute(ctx, attempt.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err = svc.Repo.Attempt(ctx, attempt.ID)
	if err != nil || attempt.Verdict != "PASS" || len(attempt.Results) != 1 || attempt.Results[0].Status != "PASS" {
		t.Fatalf("attempt=%+v err=%v", attempt, err)
	}
	if len(attempt.Results[0].Evidence) != 1 || !strings.Contains(string(attempt.Results[0].Evidence[0].Artifact), ".json") {
		t.Fatalf("direct read evidence=%+v", attempt.Results[0].Evidence)
	}
}
