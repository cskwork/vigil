package proof

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	target.BaseURL = allowed.URL
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
	if err = svc.execute(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
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
	t.Logf("redirect blocked before egress; forbidden_requests=0 verdict=%s criterion=%s owned_pid=%d stopped=true", a.Verdict, a.Results[0].Status, pid)
}
