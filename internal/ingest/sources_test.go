package ingest

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

func absTestdata(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- jira ------------------------------------------------------------------

func jiraConfig(t *testing.T) *config.Config {
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	cfg.Discovery.Adapters = []string{AdapterJira}
	cfg.Discovery.Jira = config.JiraDiscovery{
		CLI:          absTestdata(t, "fake-acli.sh"),
		JQL:          "project = A20 AND statusCategory != Done",
		Limit:        7,
		PollInterval: config.Duration{Duration: 10 * time.Minute},
		RouteHints:   []config.RouteHint{{Match: "ai학습관", Route: "/app/ai"}, {Match: "login", Route: "/login"}},
	}
	return cfg
}

func TestJiraAdapterPollParsesADFAndDedupsByContentHash(t *testing.T) {
	tmp := t.TempDir()
	st := openStore(t, tmp)
	argsFile := filepath.Join(tmp, "args.txt")
	t.Setenv("FAKE_ACLI_ARGS", argsFile)
	cfg := jiraConfig(t)
	in, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	events, err := in.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); strings.Join(got, ",") != "PROJ-133,PROJ-134,PROJ-135" {
		t.Fatalf("ids = %v", got)
	}
	args, _ := os.ReadFile(argsFile)
	want := "jira\nworkitem\nsearch\n--jql\nproject = A20 AND statusCategory != Done\n--json\n--limit\n7\n--fields\nkey,summary,description,status,labels,issuetype\n"
	if string(args) != want {
		t.Fatalf("acli argv:\n%s\nwant:\n%s", args, want)
	}
	ev := events[0]
	if ev.Kind != model.FeatureKindIssue || ev.Ref != "PROJ-133" || ev.Source != AdapterJira || ev.Status != StatusShipped {
		t.Fatalf("event = %+v", ev)
	}
	if !strings.HasPrefix(ev.ShippedSHA, "jira:PROJ-133:") || len(ev.ShippedSHA) != len("jira:PROJ-133:")+8 {
		t.Fatalf("shipped sha = %q", ev.ShippedSHA)
	}
	if ev.Summary != "AI학습관 처방 카드가 두 번 표시됨" || strings.Join(ev.Routes, ",") != "/app/ai" {
		t.Fatalf("summary/routes = %q %v", ev.Summary, ev.Routes)
	}
	for _, want := range []string{"type: Bug", "status: In Progress", "labels: frontend, ai", "재현 절차\n", "교사로 입장\n", "AI학습관 탭을 클릭\n", "기대: 카드 1개\n실제: 카드 2개"} {
		if !strings.Contains(ev.Details, want) {
			t.Fatalf("details missing %q:\n%s", want, ev.Details)
		}
	}
	if events[1].Details != "type: Task\nstatus: To Do\n\nPlain string description" || strings.Join(events[1].Routes, ",") != "/login" {
		t.Fatalf("plain description: %+v", events[1])
	}
	if events[2].Details != "type: Bug\nstatus: To Do" {
		t.Fatalf("null description: %q", events[2].Details)
	}
	// same content → nothing new; the per-adapter interval also skips the second poll,
	// so move the clock first to prove dedup on its own.
	ja := in.(*jiraAdapter)
	ja.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	if again, err := in.Poll(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second poll = %v %v", again, err)
	}
	// changed status → new content hash → delivered again with a new sha
	changed := filepath.Join(tmp, "changed.json")
	raw, _ := os.ReadFile(absTestdata(t, "jira-search.json"))
	_ = os.WriteFile(changed, []byte(strings.Replace(string(raw), `"In Progress"`, `"Done"`, 1)), 0o644)
	t.Setenv("FAKE_ACLI_FIXTURE", changed)
	ja.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	third, err := in.Poll(ctx)
	if err != nil || len(third) != 1 || third[0].FeatureID != "PROJ-133" || third[0].ShippedSHA == ev.ShippedSHA {
		t.Fatalf("third poll = %+v %v", third, err)
	}
	hist, err := in.History(ctx, 2)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history = %v %v", hist, err)
	}
}

func TestJiraAdapterHonoursPollInterval(t *testing.T) {
	tmp := t.TempDir()
	st := openStore(t, tmp)
	argsFile := filepath.Join(tmp, "args.txt")
	t.Setenv("FAKE_ACLI_ARGS", argsFile)
	in, err := New(jiraConfig(t), st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 3 {
		t.Fatalf("first poll = %v %v", ev, err)
	}
	_ = os.Remove(argsFile)
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 {
		t.Fatalf("second poll = %v %v", ev, err)
	}
	if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acli must not run again inside poll_interval")
	}
}

func TestJiraAdapterCLIFailureIsAnError(t *testing.T) {
	st := openStore(t, t.TempDir())
	cfg := jiraConfig(t)
	cfg.Discovery.Jira.CLI = "/nonexistent/acli"
	in, _ := New(cfg, st)
	if _, err := in.Poll(context.Background()); err == nil {
		t.Fatal("want error when the CLI is missing")
	}
}

func TestParseJiraSearchShapes(t *testing.T) {
	for _, raw := range []string{`[{"key":"X-1"}]`, `{"issues":[{"key":"X-1"}]}`, `{"values":[{"key":"X-1"}]}`} {
		issues, err := parseJiraSearch([]byte(raw))
		if err != nil || len(issues) != 1 || issues[0].Key != "X-1" {
			t.Fatalf("%s: %v %v", raw, issues, err)
		}
	}
	if issues, err := parseJiraSearch([]byte("  ")); err != nil || len(issues) != 0 {
		t.Fatalf("empty: %v %v", issues, err)
	}
}

// ---- loki ------------------------------------------------------------------

func lokiConfig(t *testing.T, url string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	cfg.Discovery.Adapters = []string{AdapterLoki}
	cfg.Discovery.Loki = config.LokiDiscovery{
		BaseURL:          url,
		DatasourceUID:    "ds-uid",
		EmailEnv:         "VIGIL_TEST_GRAFANA_EMAIL",
		PasswordEnv:      "VIGIL_TEST_GRAFANA_PASSWORD",
		Expr:             `{file=~"api|web"} |= "ERROR"`,
		Window:           config.Duration{Duration: time.Hour},
		PollInterval:     config.Duration{Duration: 15 * time.Minute},
		MinCount:         2,
		MaxLines:         500,
		Signature:        config.DefaultLokiSignature,
		MaxEventsPerPoll: 5,
	}
	return cfg
}

func lokiServer(t *testing.T, calls *atomic.Int32, gotBody *string) *httptest.Server {
	t.Helper()
	fixture, err := os.ReadFile(absTestdata(t, "loki-query.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("qa@example.com:s3cret"))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/ds/query" {
			http.Error(w, "wrong endpoint "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		if gotBody != nil {
			*gotBody = string(buf[:n])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
}

func TestLokiAdapterGroupsBySignature(t *testing.T) {
	t.Setenv("VIGIL_TEST_GRAFANA_EMAIL", "qa@example.com")
	t.Setenv("VIGIL_TEST_GRAFANA_PASSWORD", "s3cret")
	var calls atomic.Int32
	var body string
	srv := lokiServer(t, &calls, &body)
	defer srv.Close()
	st := openStore(t, t.TempDir())
	in, err := New(lokiConfig(t, srv.URL), st) // root URL form
	if err != nil {
		t.Fatal(err)
	}
	la := in.(*lokiAdapter)
	fixed := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
	la.now = func() time.Time { return fixed }
	ctx := context.Background()
	events, err := in.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"refId":"A"`, `"uid":"ds-uid"`, `"type":"loki"`, `"queryType":"range"`, `"direction":"backward"`, `"maxLines":500`, `"from":"now-1h"`, `"to":"now"`, `|= \"ERROR\"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("request body missing %s: %s", want, body)
		}
	}
	// NPE ×3 and gRPC warning ×2 reach min_count=2; the single nginx line does not.
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	npe := events[0]
	if npe.Kind != model.FeatureKindLog || npe.Source != AdapterLoki || len(npe.Ref) != 8 || npe.FeatureID != "log-"+npe.Ref {
		t.Fatalf("event = %+v", npe)
	}
	if npe.ShippedSHA != "loki:"+npe.Ref+":2026-09-10T12" {
		t.Fatalf("shipped sha = %q", npe.ShippedSHA)
	}
	if !strings.Contains(npe.Summary, "[api] 3 log lines in 1h0m0s: ERROR c.d.a.l.c.OrderController -- Request failed") {
		t.Fatalf("summary = %q", npe.Summary)
	}
	if !strings.Contains(npe.Details, "count: 3") || strings.Count(npe.Details, "\n- ") != 3 || !strings.Contains(npe.Details, "NullPointerException") || strings.Contains(npe.Details, "ingest_time") {
		t.Fatalf("details = %q", npe.Details)
	}
	if !npe.ShippedAt.Equal(time.Date(2026, 9, 10, 11, 46, 40, 337000000, time.UTC)) {
		t.Fatalf("shipped at = %s (want latest ingest_time)", npe.ShippedAt)
	}
	if strings.Contains(npe.Details, "s3cret") || strings.Contains(npe.Summary, "qa@example.com") {
		t.Fatal("credentials leaked into the event")
	}
	// same hour bucket → already delivered
	la.now = func() time.Time { return fixed.Add(20 * time.Minute) }
	if again, err := in.Poll(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second poll = %v %v", again, err)
	}
	// next hour bucket → delivered again
	la.now = func() time.Time { return fixed.Add(time.Hour) }
	if third, err := in.Poll(ctx); err != nil || len(third) != 2 {
		t.Fatalf("third poll = %v %v", third, err)
	}
	// full URL form is accepted too
	cfg := lokiConfig(t, srv.URL+"/api/ds/query")
	in2, _ := New(cfg, openStore(t, t.TempDir()))
	if h, err := in2.History(ctx, 1); err != nil || len(h) != 1 {
		t.Fatalf("history with full url = %v %v", h, err)
	}
}

func TestLokiAdapterKeepsTopSignaturesPerPoll(t *testing.T) {
	t.Setenv("VIGIL_TEST_GRAFANA_EMAIL", "qa@example.com")
	t.Setenv("VIGIL_TEST_GRAFANA_PASSWORD", "s3cret")
	var calls atomic.Int32
	srv := lokiServer(t, &calls, nil)
	defer srv.Close()
	cfg := lokiConfig(t, srv.URL)
	cfg.Discovery.Loki.MaxEventsPerPoll = 1
	in, _ := New(cfg, openStore(t, t.TempDir()))
	events, err := in.Poll(context.Background())
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Summary, "3 log lines") {
		t.Fatalf("top-1 = %+v %v (the NPE signature with 3 lines must win)", events, err)
	}
}

func TestLokiAdapterAuthAndNetworkErrorsYieldNoEvents(t *testing.T) {
	var calls atomic.Int32
	srv := lokiServer(t, &calls, nil)
	defer srv.Close()
	ctx := context.Background()

	// missing credentials: no request is even made
	t.Setenv("VIGIL_TEST_GRAFANA_EMAIL", "")
	t.Setenv("VIGIL_TEST_GRAFANA_PASSWORD", "")
	in, _ := New(lokiConfig(t, srv.URL), openStore(t, t.TempDir()))
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 || calls.Load() != 0 {
		t.Fatalf("missing creds: %v %v calls=%d", ev, err, calls.Load())
	}
	// wrong password: 401 → logged, no events, no error
	t.Setenv("VIGIL_TEST_GRAFANA_EMAIL", "qa@example.com")
	t.Setenv("VIGIL_TEST_GRAFANA_PASSWORD", "wrong")
	in, _ = New(lokiConfig(t, srv.URL), openStore(t, t.TempDir()))
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 || calls.Load() != 1 {
		t.Fatalf("bad creds: %v %v calls=%d", ev, err, calls.Load())
	}
	// unreachable host
	t.Setenv("VIGIL_TEST_GRAFANA_PASSWORD", "s3cret")
	in, _ = New(lokiConfig(t, "http://127.0.0.1:1"), openStore(t, t.TempDir()))
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 {
		t.Fatalf("unreachable: %v %v", ev, err)
	}
}

func TestSignatureFallbackStripsVolatileParts(t *testing.T) {
	s := signatureOf("request 7f3a9c2e1b for user 4c1d2e3f-1111-2222-3333-444455556666 took 512ms", nil)
	if s != "request <hex> for user <uuid> took #ms" {
		t.Fatalf("fallback signature = %q", s)
	}
	// the regexp match is normalised the same way: thread names / URLs / ids collapse
	re := regexp.MustCompile(config.DefaultLokiSignature)
	a := signatureOf("10:01 [http-nio-8080-exec-21] ERROR OrderController -- GET http://app/api/orders/8812 failed for decade-old id 00ab12cd", re)
	b := signatureOf("10:02 [http-nio-8080-exec-1] ERROR OrderController -- GET http://app/api/orders/17 failed for decade-old id ff9900aa", re)
	if a != b || a != "ERROR OrderController -- GET <url> failed for decade-old id <hex>" {
		t.Fatalf("normalised signatures differ: %q vs %q", a, b)
	}
	if long := signatureOf(strings.Repeat("error x", 100), re); len(long) > lokiMaxSignatureLen+len("\n…[truncated]") {
		t.Fatalf("signature not bounded: %d", len(long))
	}
	if got := lokiQueryURL("https://g.example.com/"); got != "https://g.example.com/api/ds/query" {
		t.Fatal(got)
	}
	if grafanaRange(90*time.Minute) != "now-90m" || grafanaRange(2*time.Hour) != "now-2h" {
		t.Fatal(grafanaRange(90*time.Minute), grafanaRange(2*time.Hour))
	}
}

// ---- exec ------------------------------------------------------------------

func execConfig(command ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.ID = "p"
	cfg.Discovery.Adapters = []string{AdapterExec}
	cfg.Discovery.Exec = config.ExecDiscovery{
		Command:      command,
		PollInterval: config.Duration{Duration: 15 * time.Minute},
		Timeout:      config.Duration{Duration: 5 * time.Second},
	}
	return cfg
}

func TestExecAdapterParsesExampleAndHonoursInterval(t *testing.T) {
	example, err := filepath.Abs(filepath.Join("..", "..", "examples", "exec-events.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	counter := filepath.Join(tmp, "runs.txt")
	cfg := execConfig("sh", "-c", "echo run >> "+counter+" && cat "+example)
	st := openStore(t, tmp)
	in, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ea := in.(*execAdapter)
	base := time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC)
	ea.now = func() time.Time { return base }
	ctx := context.Background()
	events, err := in.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); strings.Join(got, ",") != "exec-orders-5xx-2026-09-10T02,exec-OPS-4412" {
		t.Fatalf("ids = %v", got)
	}
	lg, is := events[0], events[1]
	if lg.Kind != model.FeatureKindLog || lg.Ref != "orders-5xx-2026-09-10T02" || lg.ShippedSHA != "exec:orders-5xx-2026-09-10T02:2026-09-10T02:20:00Z" || strings.Join(lg.Routes, ",") != "/orders/new" {
		t.Fatalf("log event = %+v", lg)
	}
	if !lg.ShippedAt.Equal(time.Date(2026, 9, 10, 2, 20, 0, 0, time.UTC)) || !strings.Contains(lg.Details, "NullPointerException") || lg.Source != AdapterExec {
		t.Fatalf("log event = %+v", lg)
	}
	if is.Kind != model.FeatureKindIssue || is.Ref != "OPS-4412" || is.Summary != "Cart total ignores the coupon after removing an item" {
		t.Fatalf("issue event = %+v", is)
	}
	// inside poll_interval: the command does not run
	ea.now = func() time.Time { return base.Add(5 * time.Minute) }
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 {
		t.Fatalf("second poll = %v %v", ev, err)
	}
	if b, _ := os.ReadFile(counter); strings.Count(string(b), "run") != 1 {
		t.Fatalf("command ran %d time(s), want 1", strings.Count(string(b), "run"))
	}
	// after the interval: runs again, but the same (key, at) rows are not redelivered
	ea.now = func() time.Time { return base.Add(20 * time.Minute) }
	if ev, err := in.Poll(ctx); err != nil || len(ev) != 0 {
		t.Fatalf("third poll = %v %v", ev, err)
	}
	if b, _ := os.ReadFile(counter); strings.Count(string(b), "run") != 2 {
		t.Fatalf("command ran %d time(s), want 2", strings.Count(string(b), "run"))
	}
	if h, err := in.History(ctx, 1); err != nil || len(h) != 1 || h[0].FeatureID != "exec-orders-5xx-2026-09-10T02" {
		t.Fatalf("history = %v %v", h, err)
	}
}

func TestExecAdapterTimeoutAndBadOutput(t *testing.T) {
	ctx := context.Background()
	cfg := execConfig("sleep", "5")
	cfg.Discovery.Exec.Timeout = config.Duration{Duration: 100 * time.Millisecond}
	in, _ := New(cfg, openStore(t, t.TempDir()))
	if _, err := in.Poll(ctx); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout err = %v", err)
	}
	in, _ = New(execConfig("echo", "not json"), openStore(t, t.TempDir()))
	if _, err := in.Poll(ctx); err == nil {
		t.Fatal("want parse error")
	}
	ev := execEvent(execRow{Key: "k/1 x", Kind: "weird", At: "bad"}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if ev.FeatureID != "exec-k-1-x" || ev.Kind != model.FeatureKindLog || ev.ShippedSHA != "exec:k/1 x:2026-01-01T00:00:00Z" {
		t.Fatalf("sanitised event = %+v", ev)
	}
}

// ---- multi -----------------------------------------------------------------

type stubAdapter struct {
	name   string
	events []model.FeatureEvent
	err    error
	polls  int
}

func (s *stubAdapter) Name() string { return s.name }
func (s *stubAdapter) Poll(context.Context) ([]model.FeatureEvent, error) {
	s.polls++
	return s.events, s.err
}
func (s *stubAdapter) History(_ context.Context, limit int) ([]model.FeatureEvent, error) {
	return newest(s.events, limit), s.err
}

func TestMultiMergesAndIsolatesErrors(t *testing.T) {
	a := &stubAdapter{name: "a", events: []model.FeatureEvent{{FeatureID: "a1", ShippedAt: time.Unix(20, 0)}}}
	bad := &stubAdapter{name: "bad", err: errors.New("boom")}
	c := &stubAdapter{name: "c", events: []model.FeatureEvent{{FeatureID: "c1", ShippedAt: time.Unix(10, 0)}, {FeatureID: "c2", ShippedAt: time.Unix(30, 0)}}}
	m := newMulti([]Adapter{a, bad, c})
	if m.Name() != "a,bad,c" {
		t.Fatal(m.Name())
	}
	ctx := context.Background()
	events, err := m.Poll(ctx)
	if err != nil || strings.Join(ids(events), ",") != "a1,c1,c2" {
		t.Fatalf("poll = %v %v", ids(events), err)
	}
	if a.polls != 1 || bad.polls != 1 || c.polls != 1 {
		t.Fatal("every adapter must be polled even when one fails")
	}
	hist, err := m.History(ctx, 1)
	if err != nil || strings.Join(ids(hist), ",") != "a1,c2" {
		t.Fatalf("history = %v %v", ids(hist), err)
	}
}

func TestNewBuildsMultiFromAdapterList(t *testing.T) {
	tmp := t.TempDir()
	st := openStore(t, tmp)
	cfg := execConfig("true")
	cfg.Discovery.Adapters = []string{AdapterFile, AdapterExec}
	cfg.Discovery.FeaturesDir = filepath.Join(tmp, "features")
	in, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if in.Name() != "file,exec" {
		t.Fatal(in.Name())
	}
	cfg.Discovery.Adapters = []string{AdapterFile, "svn"}
	if _, err := New(cfg, st); err == nil {
		t.Fatal("want error for unknown adapter in list")
	}
	// legacy single adapter still honoured when the list is empty
	cfg.Discovery.Adapters = nil
	cfg.Discovery.Adapter = AdapterFile
	if in, err := New(cfg, st); err != nil || in.Name() != AdapterFile {
		t.Fatalf("legacy = %v %v", in, err)
	}
}

func TestBoundAndSafeID(t *testing.T) {
	if got := bound("가나다라", 4); got != "가"+"\n…[truncated]" {
		t.Fatalf("bound = %q", got)
	}
	if safeID("PROJ-123 (x)/y") != "PROJ-123--x--y" {
		t.Fatal(safeID("PROJ-123 (x)/y"))
	}
}
