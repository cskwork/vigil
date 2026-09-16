package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigil/internal/attach"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

type fakeRequestSubmitter struct {
	situation string
	featureID string
	jobID     int64
	err       error
}

type invalidRequestError string

func (e invalidRequestError) Error() string        { return string(e) }
func (e invalidRequestError) InvalidRequest() bool { return true }

func (f *fakeRequestSubmitter) SubmitUserRequest(_ context.Context, situation string) (string, int64, error) {
	f.situation = situation
	return f.featureID, f.jobID, f.err
}

func newRequestServer(t *testing.T, submitter RequestSubmitter) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	s := New(cfg, st, t.TempDir())
	s.SetRequestSubmitter(submitter)
	return s
}

func postRequest(s *Server, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://vigil.test/api/requests", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://vigil.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestRequestsPostCreatesQueuedRequest(t *testing.T) {
	fake := &fakeRequestSubmitter{featureID: "USER-1", jobID: 42}
	w := postRequest(newRequestServer(t, fake), `{"situation":"  결제 화면에서 뒤로 가기를 확인해 주세요.  "}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if fake.situation != "결제 화면에서 뒤로 가기를 확인해 주세요." {
		t.Fatalf("situation = %q", fake.situation)
	}
	var got requestReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FeatureID != "USER-1" || got.JobID != 42 || got.Status != "queued" {
		t.Fatalf("receipt = %+v", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestRequestsPostRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", `{"situation":"  "}`},
		{"malformed", `{"situation":`},
		{"unknown field", `{"situation":"확인","mutable":true}`},
		{"second value", `{"situation":"확인"} {}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postRequest(newRequestServer(t, &fakeRequestSubmitter{}), tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("content-type = %q", ct)
			}
		})
	}
}

func TestRequestsPostRejectsOversizedBody(t *testing.T) {
	body := `{"situation":"` + strings.Repeat("x", maxRequestBodyBytes) + `"}`
	w := postRequest(newRequestServer(t, &fakeRequestSubmitter{}), body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsPostRequiresJSONAndSameOrigin(t *testing.T) {
	s := newRequestServer(t, &fakeRequestSubmitter{})

	nonJSON := httptest.NewRequest(http.MethodPost, "http://vigil.test/api/requests", strings.NewReader(`{"situation":"확인"}`))
	nonJSON.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, nonJSON)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON status = %d", w.Code)
	}

	crossOrigin := httptest.NewRequest(http.MethodPost, "http://vigil.test/api/requests", strings.NewReader(`{"situation":"확인"}`))
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "https://attacker.test")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, crossOrigin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", w.Code)
	}
}

func TestRequestsPostWithoutSubmitterReturnsServiceUnavailable(t *testing.T) {
	w := postRequest(newRequestServer(t, nil), `{"situation":"로그인을 확인해 주세요"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsPostReturnsValidationError(t *testing.T) {
	fake := &fakeRequestSubmitter{err: invalidRequestError("상황 설명은 8,000자 이하여야 합니다")}
	w := postRequest(newRequestServer(t, fake), `{"situation":"확인"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "8,000") {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsPostReturnsAcceptedWhenRequestPersistedBeforePreemptionWarning(t *testing.T) {
	fake := &fakeRequestSubmitter{featureID: "user-qa-1", jobID: 42, err: errors.New("preemption unavailable")}
	w := postRequest(newRequestServer(t, fake), `{"situation":"확인"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	var got requestReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FeatureID != "user-qa-1" || got.JobID != 42 || got.Status != "queued" || got.Warning == "" {
		t.Fatalf("receipt = %+v", got)
	}
}

func TestRequestsPostHidesInternalError(t *testing.T) {
	fake := &fakeRequestSubmitter{err: errors.New("sqlite secret detail")}
	w := postRequest(newRequestServer(t, fake), `{"situation":"확인"}`)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "sqlite") {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsPostThrottlesRapidDuplicate(t *testing.T) {
	s := newRequestServer(t, &fakeRequestSubmitter{featureID: "USER-1", jobID: 1})
	if w := postRequest(s, `{"situation":"확인"}`); w.Code != http.StatusCreated {
		t.Fatalf("first status = %d", w.Code)
	}
	if w := postRequest(s, `{"situation":"다시 확인"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsRejectsUnknownMethod(t *testing.T) {
	s := newRequestServer(t, nil)
	r := httptest.NewRequest(http.MethodDelete, "/api/requests", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("status = %d, allow = %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestRequestsGetStillWorksWithoutSubmitter(t *testing.T) {
	s := newRequestServer(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/api/requests", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestRequestsGetUsesPersistedJobState(t *testing.T) {
	s := newRequestServer(t, nil)
	now := time.Now()
	if _, err := s.st.UpsertFeature(context.Background(), "p", model.FeatureEvent{
		FeatureID: "USER-1", Status: "shipped", ShippedSHA: "user-1", ShippedAt: now,
		Summary: "로그인 화면 확인", Source: "manual",
	}); err != nil {
		t.Fatal(err)
	}
	jobID, _, err := s.st.EnqueueJob(context.Background(), &model.Job{
		ProjectID: "p", Kind: model.JobAgentDiscover, FeatureID: "USER-1", Priority: 110,
	}, "user:USER-1")
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/requests", nil))
	var got struct {
		Requests []requestView `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) != 1 || got.Requests[0].JobID != jobID || got.Requests[0].Status != "queued" {
		t.Fatalf("requests = %+v", got.Requests)
	}
}

func TestRequestsGetShowsPreemptingAndBudgetWaiting(t *testing.T) {
	s := newRequestServer(t, nil)
	ctx := context.Background()
	s.cfg.Budget.AgentTasksPerHour = 1
	now := time.Now()
	oldID, _, _ := s.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentRepair, Priority: 100, FeatureID: "old"}, "old")
	if oldID == 0 {
		t.Fatal("old job not enqueued")
	}
	if _, err := s.st.ClaimJob(ctx, "p", "agent-1", time.Minute, model.JobAgentRepair); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []model.FeatureEvent{
		{FeatureID: "user-qa-1", Status: "requested", ShippedSHA: "manual-ui", ShippedAt: now, Summary: "UI request", Source: "manual"},
		{FeatureID: "cli-request", Status: "requested", ShippedSHA: "manual-cli", ShippedAt: now.Add(-time.Second), Summary: "CLI request", Source: "manual"},
	} {
		if _, err := s.st.UpsertFeature(ctx, "p", feature); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _ = s.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 110, FeatureID: "user-qa-1"}, "ui")
	_, _, _ = s.st.EnqueueJob(ctx, &model.Job{ProjectID: "p", Kind: model.JobAgentDiscover, Priority: 90, FeatureID: "cli-request"}, "cli")
	if err := s.st.RecordBudget(ctx, "p", "agent", 1); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/requests", nil))
	var got struct {
		Requests []requestView `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, req := range got.Requests {
		statuses[req.FeatureID] = req.Status
	}
	if statuses["user-qa-1"] != "preempting" || statuses["cli-request"] != "budget_waiting" {
		t.Fatalf("statuses = %+v", statuses)
	}
}

func TestIndexAlwaysContainsQASituationForm(t *testing.T) {
	s := newRequestServer(t, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{`id="qaForm"`, `id="qaSituation"`, `name="situation"`, `aria-live="polite"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("index does not contain %s", want)
		}
	}
}

func TestSameOriginAllowsBrowserDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		requestURL string
		origin     string
	}{
		{"http://vigil.test/api/requests", "http://vigil.test:80"},
		{"https://vigil.test/api/requests", "https://vigil.test:443"},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.requestURL, nil)
		r.Header.Set("Origin", tc.origin)
		if !hasSameOrigin(r) {
			t.Errorf("request %q origin %q rejected", tc.requestURL, tc.origin)
		}
	}
}

func TestSubmissionThrottleExpires(t *testing.T) {
	s := newRequestServer(t, &fakeRequestSubmitter{})
	now := time.Now()
	s.now = func() time.Time { return now }
	if !s.allowRequestSubmission("192.0.2.1:1234") {
		t.Fatal("first submission rejected")
	}
	if s.allowRequestSubmission("192.0.2.1:5678") {
		t.Fatal("rapid submission allowed")
	}
	now = now.Add(requestSubmitInterval)
	if !s.allowRequestSubmission("192.0.2.1:5678") {
		t.Fatal("submission rejected after interval")
	}
}

type siteRequestSubmitter struct {
	fakeRequestSubmitter
	site string
}

func (f *siteRequestSubmitter) SubmitUserRequestAt(ctx context.Context, situation, site string) (string, int64, error) {
	f.site = site
	return f.SubmitUserRequest(ctx, situation)
}
func TestRequestSiteSelectionKeepsLegacyContract(t *testing.T) {
	f := &siteRequestSubmitter{fakeRequestSubmitter: fakeRequestSubmitter{featureID: "f", jobID: 8}}
	if w := postRequest(newRequestServer(t, f), `{"situation":"Search and inspect results", "site":"staging"}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.site != "staging" || f.situation != "Search and inspect results" {
		t.Fatal(f)
	}
	legacy := &fakeRequestSubmitter{featureID: "f", jobID: 8}
	if w := postRequest(newRequestServer(t, legacy), `{"situation":"Search", "site":"staging"}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if legacy.situation != "" {
		t.Fatal("site must never be silently ignored")
	}
}

type fakeAttachmentSubmitter struct {
	situation   string
	site        string
	attachments []attach.Attachment
	featureID   string
	jobID       int64
	err         error
}

func (f *fakeAttachmentSubmitter) SubmitUserRequest(ctx context.Context, situation string) (string, int64, error) {
	return f.SubmitUserRequestWith(ctx, situation, "", nil)
}

func (f *fakeAttachmentSubmitter) SubmitUserRequestWith(_ context.Context, situation, site string, atts []attach.Attachment) (string, int64, error) {
	f.situation, f.site, f.attachments = situation, site, atts
	return f.featureID, f.jobID, f.err
}

func postUpload(s *Server, situation string, files map[string][]byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("situation", situation)
	for name, data := range files {
		part, _ := mw.CreateFormFile("files", name)
		_, _ = part.Write(data)
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "http://vigil.test/api/requests", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Origin", "http://vigil.test")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRequestsAcceptsScreenshotWithoutProse(t *testing.T) {
	fake := &fakeAttachmentSubmitter{featureID: "USER-2", jobID: 7}
	s := newRequestServer(t, fake)
	w := postUpload(s, "", map[string][]byte{"버그 화면.png": testPNG(t)})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if len(fake.attachments) != 1 || fake.attachments[0].Kind != attach.KindImage {
		t.Fatalf("attachments = %+v", fake.attachments)
	}
	if fake.attachments[0].Name != "버그 화면.png" {
		t.Fatalf("name = %q", fake.attachments[0].Name)
	}
	// Stored under the evidence root so the dashboard can serve it back.
	if !strings.HasPrefix(fake.attachments[0].Path, s.evRoot) {
		t.Fatalf("path %q is outside %q", fake.attachments[0].Path, s.evRoot)
	}
	if _, err := os.Stat(fake.attachments[0].Path); err != nil {
		t.Fatalf("file not stored: %v", err)
	}
}

func TestRequestsRejectsUnsupportedUploadAndKeepsNothing(t *testing.T) {
	fake := &fakeAttachmentSubmitter{featureID: "USER-3", jobID: 8}
	s := newRequestServer(t, fake)
	w := postUpload(s, "확인해 주세요", map[string][]byte{"notes.txt": []byte("just text")})

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if fake.attachments != nil {
		t.Fatalf("submitter was called with %+v", fake.attachments)
	}
	entries, err := os.ReadDir(filepath.Join(s.evRoot, "requests"))
	if err == nil && len(entries) > 0 {
		t.Fatalf("rejected upload left %d directories behind", len(entries))
	}
}

func TestRequestsStillRejectsEmptySubmission(t *testing.T) {
	w := postUpload(newRequestServer(t, &fakeAttachmentSubmitter{}), "   ", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}
