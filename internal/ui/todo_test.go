package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

// seedScenario adds one script in the given state, optionally with a finished run.
func seedScenario(t *testing.T, s *Server, id string, state model.ScenarioState, oracle string, outcome model.Outcome) {
	t.Helper()
	ctx := context.Background()
	m := &model.Scenario{ID: id, ProjectID: "p", State: state, Fingerprint: "fp-" + id, Class: "P1",
		Mutation: model.MutationReadOnly, OracleSource: oracle, SoakTarget: 3, LastOutcome: outcome}
	v := &model.ScenarioVersion{ScenarioID: id, Version: 1, YAML: "scenario:\n  id: " + id + "\n", Fingerprint: "fp-" + id, CreatedBy: "seed"}
	if err := s.st.CreateScenario(ctx, m, v, nil); err != nil {
		t.Fatal(err)
	}
	if outcome == "" {
		return
	}
	now := time.Now()
	run := &model.Run{ProjectID: "p", ScenarioID: id, ScenarioVersion: 1, Browser: model.BrowserLightpanda,
		Outcome: outcome, StartedAt: now, FinishedAt: now, DeployMarker: "v1", Environment: "stg"}
	if _, err := s.st.InsertRun(ctx, run); err != nil {
		t.Fatal(err)
	}
}

func fetchTodo(t *testing.T, s *Server) todoPayload {
	t.Helper()
	w := get(t, s, "/api/todo", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var out todoPayload
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %q)", err, w.Body.String())
	}
	return out
}

func TestTodoCountsWhatNeedsAHuman(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	s.cfg.Schedule.DailyAt = "09:00"
	seedScenario(t, s, "approve-me", model.StatePendingApproval, "spec", "")
	seedScenario(t, s, "broken", model.StateActive, "contract", model.OutcomeAppFailure)
	// an APP_FAILURE on an observation-only script is not a statement about the
	// product, so it must not appear as a reproduced problem
	seedScenario(t, s, "observed", model.StateActive, "observation", model.OutcomeAppFailure)
	seedScenario(t, s, "review-me", model.StateNeedsReview, "spec", "")
	seedScenario(t, s, "flaky", model.StateQuarantined, "spec", "")
	ctx := context.Background()
	if _, err := s.st.CreateIncident(ctx, &model.Incident{ProjectID: "p", Kind: model.IncidentEnvironment, Title: "로그인 실패", State: "OPEN", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.InsertFinding(ctx, &model.Finding{ProjectID: "p", ScenarioID: "broken", Kind: "data_mismatch", Where: "/x", State: "OPEN", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	out := fetchTodo(t, s)
	want := todoCounts{PendingApproval: 1, AppFailure: 1, NeedsReview: 1, Quarantined: 1, Findings: 1, EnvIncidents: 1, Total: 6}
	if out.Counts != want {
		t.Fatalf("counts = %+v, want %+v", out.Counts, want)
	}
	if out.Hero.Runs != 2 || out.Hero.Failures != 2 {
		t.Fatalf("hero = %+v, want 2 runs / 2 failures in the last hour", out.Hero)
	}
	if len(out.Recent) != 2 {
		t.Fatalf("recent = %d entries, want the newest runs", len(out.Recent))
	}
	if out.DailyCount != 2 {
		t.Fatalf("daily_count = %d, want the two ACTIVE scripts", out.DailyCount)
	}
	if out.NextRunAt == "" {
		t.Fatal("next_run_at must fall back to the daily schedule")
	}
}

func TestTodoZeroCaseIsAllClear(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	s.cfg.Schedule.DailyAt = "09:00"
	out := fetchTodo(t, s)
	if out.Counts != (todoCounts{}) {
		t.Fatalf("counts = %+v, want all zero", out.Counts)
	}
	if len(out.Recent) != 0 {
		t.Fatalf("recent = %+v, want empty", out.Recent)
	}
	if out.CanRequest {
		t.Fatal("can_request must be false without a submitter")
	}
	if out.Hero.WindowHours != 1 {
		t.Fatalf("hero window = %d, want 1 hour", out.Hero.WindowHours)
	}
}

func TestTodoRevalidatesWith304(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	first := get(t, s, "/api/todo", nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || etag == "" {
		t.Fatalf("status %d etag %q", first.Code, etag)
	}
	if second := get(t, s, "/api/todo", map[string]string{"If-None-Match": etag}); second.Code != http.StatusNotModified {
		t.Fatalf("revalidation status %d, want 304", second.Code)
	}
}

// The three pages a person navigates between must be served, carry their own
// title and load the shared script.
func TestPagesAreServed(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for path, want := range map[string]string{
		"/":         "할 일",
		"/results":  "검증 결과",
		"/help":     "도움말",
		"/scripts":  "검사 관리",
		"/activity": "시스템 활동",
	} {
		w := get(t, s, path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("%s: body does not mention %q", path, want)
		}
		if !strings.Contains(body, `href="/help"`) {
			t.Errorf("%s: rail has no help link", path)
		}
		if !strings.Contains(body, "skiplink") {
			t.Errorf("%s: no skip link", path)
		}
	}
}

// The landing page needs exactly one polled endpoint (/api/todo); the request
// form only posts.
func TestTodoPagePollsOnlyTodo(t *testing.T) {
	body := get(t, newTestServer(t, config.ActiveHours{}), "/", nil).Body.String()
	if strings.Count(body, "poller(") != 1 {
		t.Fatalf("landing page must set up exactly one poller, got %d", strings.Count(body, "poller("))
	}
	if !strings.Contains(body, "'/api/todo'") {
		t.Fatal("landing page must poll /api/todo")
	}
}

// The reproduction verdict from WI-H stays rendered in all three values.
func TestScriptsPageRendersEveryReproductionVerdict(t *testing.T) {
	body := get(t, newTestServer(t, config.ActiveHours{}), "/scripts", nil).Body.String()
	for _, want := range []string{"confirmed", "재현됨", "disputed", "판정 불일치", "unconfirmed", "재현 안 됨"} {
		if !strings.Contains(body, want) {
			t.Errorf("scripts page does not render %q", want)
		}
	}
}

// Query-string filters are what the action cards link to.
func TestFilterEntryPointsExist(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	home := get(t, s, "/", nil).Body.String()
	for _, want := range []string{"/scripts?state=PENDING_APPROVAL", "/results?outcome=APP_FAILURE"} {
		if !strings.Contains(home, want) {
			t.Errorf("home page has no link to %s", want)
		}
	}
	if !strings.Contains(get(t, s, "/scripts", nil).Body.String(), `params.get('state')`) {
		t.Error("scripts page does not read ?state=")
	}
	if !strings.Contains(get(t, s, "/results", nil).Body.String(), `params.get('outcome')`) {
		t.Error("results page does not read ?outcome=")
	}
}

// Existing endpoints keep answering after the page rework.
func TestOldEndpointsStillAnswer(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	for _, ep := range []string{"/api/overview", "/api/verification", "/api/scripts", "/api/requests", "/api/supervisor", "/report", "/ui/app.js", "/ui/theme.css"} {
		if w := get(t, s, ep, nil); w.Code != http.StatusOK {
			t.Errorf("%s: status %d", ep, w.Code)
		}
	}
}

// The script list must fit the card it sits in. A fixed min-width on the title
// cell pushed the table wider than its container, which hid the ▶ 실행 / ✔ 승인 /
// ✖ 반려 buttons behind a horizontal scrollbar. Guard the structure that keeps
// them reachable: no pinned width on the title cell, a full-width list until a
// script is selected, and a stacked row layout when the list column is narrow.
func TestScriptListCannotOutgrowItsCard(t *testing.T) {
	s := newTestServer(t, config.ActiveHours{})
	css := get(t, s, "/ui/theme.css", nil).Body.String()
	for _, rule := range []string{".list-table td.title", ".list-table th:first-child"} {
		i := strings.Index(css, rule)
		if i < 0 {
			t.Fatalf("theme.css has no %s rule", rule)
		}
		block := css[i:]
		if end := strings.Index(block, "}"); end >= 0 {
			block = block[:end]
		}
		if strings.Contains(block, "min-width:1") || strings.Contains(block, "min-width: 1") {
			t.Errorf("%s pins a width (%q); the title must wrap instead", rule, block)
		}
	}
	if !strings.Contains(css, "@container list (max-width: 900px)") {
		t.Error("theme.css has no stacked-row rule for a narrow list column")
	}
	if !strings.Contains(css, ".split.single") {
		t.Error("theme.css cannot give the list the full width when nothing is selected")
	}
	page := get(t, s, "/scripts", nil).Body.String()
	if !strings.Contains(page, `class="split single" id="split"`) {
		t.Error("the scripts page does not start with a full-width list")
	}
	// The panel stays hidden until a row is selected, and it is a separate
	// section from the list. It is no longer an aria-live region: a poll used to
	// make a screen reader read the whole detail again (WS2, aria-live-overreach).
	if !strings.Contains(page, `aria-label="스크립트 상세" id="detail" hidden`) {
		t.Error("the detail panel must stay hidden until a script is selected")
	}
	if strings.Contains(page, `id="detail" aria-live`) {
		t.Error("the detail panel must not be a live region; opening it moves focus instead")
	}
	if !strings.Contains(page, `class="panel list-panel"`) {
		t.Error("the list panel is not a query container, so rows cannot stack when it is narrow")
	}
	// The action cell must not reuse `.acts`: that is the activity timeline's
	// scroll box (display:flex, overflow:auto) and it clipped the buttons to
	// 24px slivers.
	if strings.Contains(page, `class="fit acts"`) {
		t.Error("the action cell reuses the activity timeline's .acts class, which clips its buttons")
	}
	// No table cell outside the container query may clip: an ancestor with
	// overflow != visible cuts the action buttons even when the table fits.
	outside := css
	if i := strings.Index(css, "@container list"); i >= 0 {
		outside = css[:i]
	}
	for _, rule := range strings.Split(outside, "}") {
		sel, decls, ok := strings.Cut(rule, "{")
		if !ok {
			continue
		}
		sel = strings.TrimSpace(sel)
		if !strings.Contains(sel, "td") || !strings.Contains(decls, "overflow") {
			continue
		}
		if !strings.Contains(decls, "overflow:visible") {
			t.Errorf("table-cell rule %q sets overflow (%q); a clipping cell hides the row actions", sel, strings.TrimSpace(decls))
		}
	}
}
