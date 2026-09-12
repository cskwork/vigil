package proof

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"vigil/internal/store"
)

func testRegistry() Registry {
	return Registry{Targets: map[string]Target{"qa": {BaseURL: "http://127.0.0.1:8790", Environment: "qa", PolicyVersion: "1", Personas: map[string]Persona{"p": {Account: "qa-p"}}, Fixtures: map[string]Fixture{"f": {Entity: "qa-1", QA: true}}, Actions: map[string]Action{"read": {}}, Observers: map[string]Observer{"dom": {Kind: "dom", Action: "read"}}, Definitions: map[string]Definition{"zero": {Expected: Value{true, json.RawMessage(`0`)}}}}}}
}
func contract() Contract {
	return Contract{Persona: "p", Fixture: "f", Actions: []string{"read"}, Criteria: []Criterion{{ID: "v", Required: true, Observer: "dom", Expected: Value{true, json.RawMessage(`0`)}, Source: "definition", SourceRef: "zero"}}}
}
func setup(t *testing.T) (*store.Store, *Repository) {
	t.Helper()
	s, e := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	r, e := NewRepository(s.DB())
	if e != nil {
		t.Fatal(e)
	}
	return s, r
}
func TestTypedExpectedAndVerdict(t *testing.T) {
	vals := []Value{{false, nil}, {true, json.RawMessage(`null`)}, {true, json.RawMessage(`""`)}, {true, json.RawMessage(`0`)}, {true, json.RawMessage(`false`)}}
	for i, a := range vals {
		if e := a.Validate(); e != nil {
			t.Fatal(e)
		}
		for j, b := range vals {
			if Equal(a, b) != (i == j) {
				t.Fatalf("collapsed values %d %d", i, j)
			}
		}
	}
	cases := []struct {
		r    []CriterionResult
		want string
	}{{nil, "INCOMPLETE"}, {[]CriterionResult{{Required: true, Status: "PASS"}}, "PASS"}, {[]CriterionResult{{Required: true, Status: "UNKNOWN"}, {Required: true, Status: "FAIL"}}, "FAIL"}, {[]CriterionResult{{Required: true, Status: "PASS"}, {Required: false, Status: "FAIL"}}, "PASS"}}
	for _, c := range cases {
		if got := Verdict(c.r); got != c.want {
			t.Fatalf("%s != %s", got, c.want)
		}
	}
}
func TestAtomicApprovalIdempotencyImmutableAndLegacyIsolation(t *testing.T) {
	ctx := context.Background()
	st, r := setup(t)
	c, e := r.Create(ctx, "qa", "zero", "actor")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.ClaimJob(ctx, "proof", "legacy", time.Minute); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("legacy claimed proof job: %v", e)
	}
	c, e = r.Patch(ctx, c.ID, c.RowVersion, contract())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Patch(ctx, c.ID, 1, contract()); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale patch %v", e)
	}
	ap := Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "same"}
	a, reused, e := r.Approve(ctx, c.ID, ap, "actor", testRegistry())
	if e != nil || reused {
		t.Fatalf("approve %v", e)
	}
	again, reused, e := r.Approve(ctx, c.ID, ap, "actor", testRegistry())
	if e != nil || !reused || again.ID != a.ID {
		t.Fatalf("idempotency %v", e)
	}
	ap.Scope = "changed"
	if _, _, e = r.Approve(ctx, c.ID, ap, "actor", testRegistry()); !errors.Is(e, ErrConflict) {
		t.Fatalf("payload conflict %v", e)
	}
	a.State = "DONE"
	a.Verdict = "FAIL"
	if e = r.Update(ctx, a); e != nil {
		t.Fatal(e)
	}
	a.Verdict = "PASS"
	if e = r.Update(ctx, a); !errors.Is(e, ErrConflict) {
		t.Fatalf("terminal changed %v", e)
	}
	if e = r.Disposition(ctx, a.ID, "ACKNOWLEDGED"); e != nil {
		t.Fatal(e)
	}
	saved, _ := r.Attempt(ctx, a.ID)
	if saved.Verdict != "FAIL" {
		t.Fatal("disposition changed verdict")
	}
}
func TestScopeExactQueryAndCompiler(t *testing.T) {
	rules := []ScopeRule{{Origin: "http://127.0.0.1:8790", Method: "GET", Path: "/record", Query: map[string]string{"entity": "qa-1"}}}
	for _, url := range []string{"http://127.0.0.1:8790/record?entity=qa-2", "http://127.0.0.1:8790/record?entity=qa-1&entity=qa-1", "http://other.test/record?entity=qa-1", "http://127.0.0.1:8790/records?entity=qa-1"} {
		if Allowed(url, "GET", rules) {
			t.Fatal("scope escape", url)
		}
	}
	if !Allowed("http://127.0.0.1:8790/record?entity=qa-1", "GET", rules) {
		t.Fatal("valid scope denied")
	}
	reg := testRegistry()
	c := contract()
	c.Criteria[0].Expected = Value{true, json.RawMessage(`false`)}
	if _, e := reg.Compile("qa", c, "zero"); e == nil {
		t.Fatal("unapproved expectation accepted")
	}
	c = contract()
	c.Criteria[0].Observer = "unavailable"
	p, e := reg.Compile("qa", c, "zero")
	if e != nil || len(p.Unsupported) != 1 {
		t.Fatal("missing criterion silently lost")
	}
}
func TestRecoverPreservesFailureAndDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	st, r := setup(t)
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	a, _, e := r.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "a"}, "actor", testRegistry())
	if e != nil {
		t.Fatal(e)
	}
	a.State = "RUNNING"
	a.Results = []CriterionResult{{ID: "v", Required: true, Status: "FAIL"}}
	r.Update(ctx, a)
	if e = r.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	a, _ = r.Attempt(ctx, a.ID)
	if a.State != "INTERRUPTED" || a.Verdict != "FAIL" || len(a.Results) != 1 {
		t.Fatalf("bad restart %+v", a)
	}
	var count int
	st.DB().QueryRow(`SELECT count(*) FROM jobs WHERE kind LIKE 'PROOF_%' AND state='READY'`).Scan(&count)
	if count != 0 {
		t.Fatal("replayed proof")
	}
}
func TestLocalAuthCSRFDeniesExternalAndLegacyEvidence(t *testing.T) {
	st, _ := setup(t)
	svc, e := NewService(st, testRegistry(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Handler(HTTPConfig{}); e == nil {
		t.Fatal("external auth missing accepted")
	}
	h, _ := svc.Handler(HTTPConfig{LocalOperator: true})
	req := httptest.NewRequest("POST", "http://127.0.0.1/api/proof/checks", strings.NewReader(`{"target_ref":"qa","request":"zero"}`))
	req.RemoteAddr = "127.0.0.1:22"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted")
	}
	req = httptest.NewRequest("GET", "http://evil.test/api/proof/session", nil)
	req.RemoteAddr = "127.0.0.1:22"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("DNS rebinding accepted")
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1/evidence/file", nil)
	req.RemoteAddr = "127.0.0.1:22"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatal("legacy evidence exposed")
	}
}
func TestMissingJSONPreservesType(t *testing.T) {
	for _, c := range []struct {
		path    string
		present bool
		data    string
	}{{"$.missing", false, ""}, {"$.n", true, "null"}, {"$.empty", true, `""`}, {"$.zero", true, "0"}, {"$.flag", true, "false"}} {
		v, e := JSONValue([]byte(`{"n":null,"empty":"","zero":0,"flag":false}`), c.path)
		if e != nil || v.Present != c.present || string(v.Data) != c.data {
			t.Fatalf("%s: %+v %v", c.path, v, e)
		}
	}
}

func TestArtifactFailureCannotPass(t *testing.T) {
	st, r := setup(t)
	ctx := context.Background()
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	a, _, e := r.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "art"}, "actor", testRegistry())
	if e != nil {
		t.Fatal(e)
	}
	a.Results = []CriterionResult{{ID: "v", Required: true, Status: "PASS", Evidence: []Evidence{{ID: id(), At: time.Now(), Actual: Value{true, json.RawMessage(`0`)}}}}}
	svc := &Service{Repo: r, Store: st, EvidenceDir: "/dev/null/not-a-dir"}
	if e = svc.finish(a, "DONE", nil, ""); e != nil {
		t.Fatal(e)
	}
	saved, _ := r.Attempt(ctx, a.ID)
	if saved.Verdict != "INCOMPLETE" || saved.Results[0].Status != "UNKNOWN" || saved.Results[0].Evidence[0].Artifact != "" {
		t.Fatal("missing artifact got valid PASS")
	}
}
func TestDemoDoesNotPlanContradictoryRequests(t *testing.T) {
	for _, s := range []string{"임의 테스트", "이력 삭제 후 빈 값 유지", "기록 삭제 후 빈 문자열 유지"} {
		if demoRequest(s) {
			t.Fatal("unrelated or contradictory request got demo", s)
		}
	}
	if !demoRequest("답을 지우면 화면과 API의 현재 답은 비어 있어야 하고, 기존 제출 기록은 유지되어야 합니다.") {
		t.Fatal("supported canonical request not recognized")
	}
}
func TestProofServiceSingleton(t *testing.T) {
	st, _ := setup(t)
	first, e := NewService(st, testRegistry(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = NewService(st, testRegistry(), t.TempDir()); e == nil {
		t.Fatal("second service could interrupt live attempts")
	}
	st.ReleaseLocks(context.Background(), []string{"proof:service"}, first.owner)
}
func TestGatewayRejectsClientIdentityAndAcceptsOnlyConfiguredPrincipal(t *testing.T) {
	st, _ := setup(t)
	svc, e := NewService(st, testRegistry(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("a", 32)
	h, e := svc.Handler(HTTPConfig{GatewayToken: secret, GatewayActor: "team-member"})
	if e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest("GET", "http://proof.test/api/proof/session", nil)
	req.Header.Set("X-User", "admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("untrusted identity accepted")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "team-member") || strings.Contains(w.Body.String(), "admin") {
		t.Fatal("wrong authenticated actor", w.Body.String())
	}
}
func TestVersionChangePreventsPass(t *testing.T) {
	st, r := setup(t)
	ctx := context.Background()
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	a, _, _ := r.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "version"}, "actor", testRegistry())
	a.ObservedVersion = "A"
	a.VersionAfter = "B"
	a.Results = []CriterionResult{{ID: "v", Required: true, Status: "PASS", Evidence: []Evidence{{ID: id(), At: time.Now(), Actual: Value{true, json.RawMessage(`0`)}}}}}
	svc := &Service{Repo: r, Store: st, EvidenceDir: t.TempDir()}
	if e := svc.finish(a, "DONE", nil, ""); e != nil {
		t.Fatal(e)
	}
	a, _ = r.Attempt(ctx, a.ID)
	if a.Verdict != "INCOMPLETE" || a.FixClaim != "Not proven" {
		t.Fatal("deployment changed but PASS", a)
	}
}
func TestBaselineNeedsPriorComparableObservedFailure(t *testing.T) {
	ctx := context.Background()
	st, r := setup(t)
	reg := testRegistry()
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	ap := Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "base"}
	base, _, _ := r.Approve(ctx, c.ID, ap, "actor", reg)
	base.State = "DONE"
	base.Verdict = "FAIL"
	base.ObservedVersion = "A"
	base.VersionAfter = "A"
	r.Update(ctx, base)
	ap.IdempotencyKey = "rerun"
	ap.Baseline = base.ID
	next, _, e := r.Approve(ctx, c.ID, ap, "actor", reg)
	if e != nil {
		t.Fatal(e)
	}
	next.ObservedVersion = "B"
	next.VersionAfter = "B"
	next.Results = []CriterionResult{{ID: "v", Required: true, Status: "PASS", Evidence: []Evidence{{ID: id(), At: time.Now(), Actual: Value{true, json.RawMessage(`0`)}}}}}
	svc := &Service{Repo: r, Store: st, EvidenceDir: t.TempDir()}
	if e = svc.finish(next, "DONE", nil, ""); e != nil {
		t.Fatal(e)
	}
	next, _ = r.Attempt(ctx, next.ID)
	if next.FixClaim != "versioned baseline FAIL to current PASS" {
		t.Fatal(next.FixClaim)
	}
	ap.Baseline = next.ID
	ap.IdempotencyKey = "not-failure"
	if _, _, e = r.Approve(ctx, c.ID, ap, "actor", reg); e == nil {
		t.Fatal("prior PASS used as reproduced defect baseline")
	}
}
func TestChangedRegistryCannotReuseUnseenApproval(t *testing.T) {
	ctx := context.Background()
	_, r := setup(t)
	reg := testRegistry()
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	ap := Approval{RegistryHash: Hash(reg.Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "changed-registry"}
	target := reg.Targets["qa"]
	target.Fixtures["f"] = Fixture{QA: true, Entity: "different"}
	reg.Targets["qa"] = target
	if _, _, e := r.Approve(ctx, c.ID, ap, "actor", reg); !errors.Is(e, ErrConflict) {
		t.Fatal("unseen fixture accepted", e)
	}
}
func TestPlanningProjectionExcludesExecutionAndSecrets(t *testing.T) {
	t.Setenv("PROOF_TEST_SECRET", "super-secret-password")
	target := testRegistry().Targets["qa"]
	p := target.Personas["p"]
	p.Secrets = map[string]string{"password": "PROOF_TEST_SECRET"}
	target.Personas["p"] = p
	raw := planningInput("check super-secret-password", target)
	for _, bad := range []string{"super-secret-password", "PROOF_TEST_SECRET", "secret_env", "setup", "steps", "dsn_env"} {
		if strings.Contains(raw, bad) {
			t.Fatal("planner got private execution input", bad)
		}
	}
	if !strings.Contains(raw, "REDACTED") {
		t.Fatal("request secret not redacted")
	}
}

func TestRecoveryReleasesOnlyOwnedAttemptLocks(t *testing.T) {
	ctx := context.Background()
	st, r := setup(t)
	c, _ := r.Create(ctx, "qa", "zero", "actor")
	c, _ = r.Patch(ctx, c.ID, c.RowVersion, contract())
	a, _, e := r.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "lock-recovery"}, "actor", testRegistry())
	if e != nil {
		t.Fatal(e)
	}
	owned := []string{"proof:whole", "account:qa-p", "entity:qa-1"}
	if ok, e := st.TryAcquireLocks(ctx, owned, "proof:"+a.ID, 5*time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	st.TryAcquireLocks(ctx, []string{"account:human"}, "human-operator", time.Hour)
	st.TryAcquireLocks(ctx, []string{"proof:service"}, "proof-service:test", time.Hour)
	if e = r.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if ok, e := st.TryAcquireLocks(ctx, owned, "proof:new-approved-attempt", time.Minute); e != nil || !ok {
		t.Fatal("recovered locks block fresh explicit attempt", e)
	}
	for _, owner := range []string{"human-operator", "proof-service:test"} {
		var n int
		if e = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM resource_locks WHERE owner=?`, owner).Scan(&n); e != nil || n != 1 {
			t.Fatal("unrelated lock changed", owner, n, e)
		}
	}
}
