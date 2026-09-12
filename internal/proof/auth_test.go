package proof

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSingleUserLoginAndCheckOwnership(t *testing.T) {
	t.Setenv("PROOF_LOGIN_PASSWORD", "one-user-password")
	st, _ := setup(t)
	reg := testRegistry()
	target := reg.Targets["qa"]
	target.Teams = []string{"owner"}
	reg.Targets["qa"] = target
	svc, err := NewService(st, reg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	h, err := svc.Handler(HTTPConfig{
		Users:        []LoginUser{{Name: "operator", Team: "owner", PasswordEnv: "PROOF_LOGIN_PASSWORD"}},
		PublicOrigin: "http://127.0.0.1:8788", UI: UI(),
	})
	if err != nil {
		t.Fatal(err)
	}

	unauth := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8788/api/proof/session", nil)
	unauth.RemoteAddr = "127.0.0.1:3000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, unauth)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("session without login=%d", w.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8788/api/proof/login", strings.NewReader(`{"name":"operator","password":"one-user-password"}`))
	login.RemoteAddr = "127.0.0.1:3000"
	login.Header.Set("Origin", "http://127.0.0.1:8788")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, login)
	if w.Code != http.StatusOK {
		t.Fatalf("login=%d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie protections missing")
	}

	session := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8788/api/proof/session", nil)
	session.RemoteAddr = "127.0.0.1:3000"
	session.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, session)
	var info struct{ Actor, Team, CSRF string }
	if json.NewDecoder(w.Body).Decode(&info) != nil || info.Actor != "operator" || info.Team != "owner" || info.CSRF == "" {
		t.Fatalf("session: %s", w.Body.String())
	}

	mismatch := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8788/api/proof/checks", strings.NewReader(`{"target_ref":"qa","url":"http://other.test","request":"zero","idempotency_key":"wrong-target"}`))
	mismatch.RemoteAddr = "127.0.0.1:3000"
	mismatch.Header.Set("Origin", "http://127.0.0.1:8788")
	mismatch.Header.Set("X-Proof-CSRF", info.CSRF)
	mismatch.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, mismatch)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched target URL=%d %s", w.Code, w.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8788/api/proof/checks", strings.NewReader(`{"target_ref":"qa","request":"zero","idempotency_key":"intake-1"}`))
	create.RemoteAddr = "127.0.0.1:3000"
	create.Header.Set("Origin", "http://127.0.0.1:8788")
	create.Header.Set("X-Proof-CSRF", info.CSRF)
	create.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, create)
	if w.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	var check Check
	if json.NewDecoder(w.Body).Decode(&check) != nil || check.Team != "owner" || check.CreatedBy != "operator" {
		t.Fatalf("owner: %s", w.Body.String())
	}
}

func TestCheckIntakeIdempotencyAndReplan(t *testing.T) {
	ctx := context.Background()
	st, repo := setup(t)
	first, err := repo.Create(ctx, "qa", "zero", "operator", "", "key")
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.Create(ctx, "qa", "zero", "operator", "", "key")
	if err != nil || again.ID != first.ID {
		t.Fatalf("duplicate check: %v %#v", err, again)
	}
	if _, err = repo.Create(ctx, "qa", "changed", "operator", "", "key"); err != ErrConflict {
		t.Fatalf("changed payload=%v", err)
	}
	if err = repo.PlanQuestion(ctx, first.ID, "빈 값은 정확히 무엇을 뜻하나요?"); err != nil {
		t.Fatal(err)
	}
	first, _ = repo.GetCheck(ctx, first.ID)
	if _, err = repo.Replan(ctx, first.ID, first.RowVersion, ""); err == nil {
		t.Fatal("planner question accepted an empty answer")
	}
	replanned, err := repo.Replan(ctx, first.ID, first.RowVersion, "빈 값이어야 합니다")
	if err != nil {
		t.Fatal(err)
	}
	if replanned.Planning != "QUEUED" || len(replanned.Clarifications) != 1 || replanned.PlanError != "" {
		t.Fatalf("replan=%+v", replanned)
	}
	clarified := contract()
	clarified.Criteria[0].Source = "request"
	clarified.Criteria[0].SourceRef = "빈 값이어야 합니다"
	if _, err = testRegistry().Compile("qa", clarified, sourceText(*replanned)); err != nil {
		t.Fatalf("approved clarification was not available as provenance: %v", err)
	}
	var jobs int
	if err = st.DB().QueryRow(`SELECT count(*) FROM jobs WHERE kind='PROOF_PLAN'`).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("plan jobs=%d %v", jobs, err)
	}
}
