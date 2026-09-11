package gate

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

func newCfg(strategy string) *config.Config {
	c := &config.Config{}
	c.Project.ID = "p"
	c.Target.BaseURL = "https://example.test"
	c.Target.AllowedHosts = []string{"example.test"}
	c.Deployment.Readiness.Strategy = strategy
	c.Deployment.Readiness.DelayAfterShip.Duration = 2 * time.Minute
	c.Deployment.Readiness.MaxWait.Duration = 30 * time.Minute
	return c
}

func newGate(c *config.Config, now time.Time) *Gate {
	g := New(c)
	g.Now = func() time.Time { return now }
	g.Log = log.New(io.Discard, "", 0)
	return g
}

func feature(shippedAt time.Time) *model.Feature {
	return &model.Feature{ID: "PROJ-1", LatestShippedSHA: "6f22ff7a1dcc309395173a5d52aba5ae01ad769a", ShippedAt: shippedAt, Routes: []string{"/app/training-entry"}}
}

func TestDelayStrategy(t *testing.T) {
	shipped := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c := newCfg("delay")
	g := newGate(c, shipped.Add(time.Minute))
	st, _, err := g.Check(context.Background(), feature(shipped), "")
	if err != nil || st != model.ReadinessWaiting {
		t.Fatalf("before delay: %v %v", st, err)
	}
	g.Now = func() time.Time { return shipped.Add(2 * time.Minute) }
	st, _, _ = g.Check(context.Background(), feature(shipped), "")
	if st != model.ReadinessReady {
		t.Fatalf("at delay: %v", st)
	}
}

func TestAssetVersionStrategy(t *testing.T) {
	var version atomic.Value
	version.Store("1000")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/training-entry" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<html><script type="module" src="/app/assets/index.js?v=`+version.Load().(string)+`"></script></html>`)
	}))
	defer srv.Close()

	shipped := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c := newCfg("asset_version")
	c.Target.BaseURL = srv.URL
	g := newGate(c, shipped.Add(time.Minute))
	f := feature(shipped)

	// first sight: WAITING, marker captured from base_url + first route (asset_page unset)
	st, marker, err := g.Check(context.Background(), f, "")
	if err != nil || st != model.ReadinessWaiting || marker != "1000" {
		t.Fatalf("first sight: %v %q %v", st, marker, err)
	}
	// unchanged marker, inside max_wait: WAITING, marker preserved
	st, marker, _ = g.Check(context.Background(), f, "1000")
	if st != model.ReadinessWaiting || marker != "1000" {
		t.Fatalf("unchanged: %v %q", st, marker)
	}
	// deploy happened: marker changed → READY with the new marker
	version.Store("2000")
	st, marker, _ = g.Check(context.Background(), f, "1000")
	if st != model.ReadinessReady || marker != "2000" {
		t.Fatalf("changed: %v %q", st, marker)
	}
	// unchanged but max_wait elapsed → DEPLOYMENT_UNKNOWN
	g.Now = func() time.Time { return shipped.Add(31 * time.Minute) }
	st, _, _ = g.Check(context.Background(), f, "2000")
	if st != model.ReadinessUnknown {
		t.Fatalf("max_wait: %v", st)
	}
	// explicit asset_page wins over base_url+route
	c.Deployment.Readiness.AssetPage = srv.URL + "/nope"
	st, marker, err = g.Check(context.Background(), f, "2000")
	if err != nil || st != model.ReadinessWaiting || marker != "2000" {
		t.Fatalf("unreachable page must be WAITING with err nil: %v %q %v", st, marker, err)
	}
}

func TestAssetVersionUnreachable(t *testing.T) {
	c := newCfg("asset_version")
	c.Target.BaseURL = "http://127.0.0.1:1" // closed port
	g := newGate(c, time.Now())
	g.HTTP.Timeout = 2 * time.Second
	st, marker, err := g.Check(context.Background(), feature(time.Now()), "abc")
	if err != nil || st != model.ReadinessWaiting || marker != "abc" {
		t.Fatalf("unreachable: %v %q %v", st, marker, err)
	}
}

func TestVersionEndpointStrategy(t *testing.T) {
	var body atomic.Value
	body.Store(`{"sha":"deadbeef0000"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()

	shipped := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	c := newCfg("version_endpoint")
	c.Deployment.Readiness.Endpoint = srv.URL + "/version"
	g := newGate(c, shipped.Add(time.Minute))
	f := feature(shipped)

	st, _, err := g.Check(context.Background(), f, "")
	if err != nil || st != model.ReadinessWaiting {
		t.Fatalf("not deployed: %v %v", st, err)
	}
	body.Store(`{"sha":"6f22ff7a1dcc"}`)
	st, marker, _ := g.Check(context.Background(), f, "")
	if st != model.ReadinessReady || marker != "6f22ff7" {
		t.Fatalf("deployed: %v %q", st, marker)
	}
	body.Store(`{"sha":"other"}`)
	g.Now = func() time.Time { return shipped.Add(time.Hour) }
	st, _, _ = g.Check(context.Background(), f, "")
	if st != model.ReadinessUnknown {
		t.Fatalf("max_wait: %v", st)
	}
	c.Deployment.Readiness.Endpoint = ""
	if _, _, err := g.Check(context.Background(), f, ""); err == nil {
		t.Fatal("missing endpoint must error")
	}
}

func TestUnknownStrategy(t *testing.T) {
	g := newGate(newCfg("magic"), time.Now())
	if _, _, err := g.Check(context.Background(), feature(time.Now()), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestIssueAndLogFeaturesAreReadyWithoutProbing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("nothing"))
	}))
	defer srv.Close()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, strategy := range []string{"delay", "asset_version", "version_endpoint"} {
		c := newCfg(strategy)
		c.Target.BaseURL = srv.URL
		c.Deployment.Readiness.Endpoint = srv.URL + "/version"
		c.Deployment.Readiness.AssetPage = srv.URL + "/"
		g := newGate(c, now)
		for _, kind := range []string{model.FeatureKindIssue, model.FeatureKindLog} {
			f := &model.Feature{ID: "PROJ-123", Kind: kind, LatestShippedSHA: "jira:PROJ-123:abcd1234", ShippedAt: now}
			state, marker, err := g.Check(context.Background(), f, "")
			if err != nil || state != model.ReadinessReady || marker != "" {
				t.Fatalf("%s/%s: %s %q %v", strategy, kind, state, marker, err)
			}
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("non-ship features must not probe the target (hits=%d)", hits.Load())
	}
	// a ship feature shipped just now still waits under the delay policy
	g := newGate(newCfg("delay"), now)
	if state, _, _ := g.Check(context.Background(), feature(now), ""); state != model.ReadinessWaiting {
		t.Fatalf("ship feature = %s", state)
	}
}

// H-3: each environment has its own marker page, so a run can be tagged with the
// build of the environment it actually verified.
func TestCurrentMarkerPerEnvironment(t *testing.T) {
	page := func(v string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<script src="/assets/index.js?v=` + v + `"></script>`))
		}))
	}
	stg, prod := page("1000"), page("2000")
	defer stg.Close()
	defer prod.Close()

	c := newCfg("asset_version")
	c.Target.DefaultEnv = "stg"
	c.Target.Environments = map[string]config.Environment{
		"stg":  {BaseURL: stg.URL, AssetPage: stg.URL + "/"},
		"prod": {BaseURL: prod.URL, AssetPage: prod.URL + "/"},
	}
	g := newGate(c, time.Now())
	ctx := context.Background()
	envStg, _ := c.Env("stg")
	envProd, _ := c.Env("prod")
	got, err := g.CurrentMarkerFor(ctx, envStg)
	if err != nil || got != "1000" {
		t.Fatalf("stg marker = %q %v", got, err)
	}
	got, err = g.CurrentMarkerFor(ctx, envProd)
	if err != nil || got != "2000" {
		t.Fatalf("prod marker = %q %v (per-environment cache must not leak)", got, err)
	}
	if got, _ = g.CurrentMarker(ctx); got != "1000" {
		t.Fatalf("default marker = %q, want the default environment's", got)
	}
}

// Default-environment behaviour is unchanged when no environments are configured:
// deployment.readiness.asset_page still decides the probed page.
func TestCurrentMarkerWithoutEnvironments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/training-entry" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`<script src="/assets/index.js?v=42"></script>`))
	}))
	defer srv.Close()
	c := newCfg("asset_version")
	c.Deployment.Readiness.AssetPage = srv.URL + "/app/training-entry"
	g := newGate(c, time.Now())
	if got, err := g.CurrentMarker(context.Background()); err != nil || got != "42" {
		t.Fatalf("marker = %q %v", got, err)
	}
	if page := g.MarkerPageFor(c.DefaultEnv()); page != c.Deployment.Readiness.AssetPage {
		t.Fatalf("marker page = %q", page)
	}
	// No asset_page at all: the environment's base URL plus the entry path.
	c2 := newCfg("asset_version")
	c2.Targets = []config.Target{{URL: "https://example.test/app/training-entry", Path: "/app/training-entry"}}
	if page := gate2Page(c2); page != "https://example.test/app/training-entry" {
		t.Fatalf("fallback page = %q", page)
	}
}

func gate2Page(c *config.Config) string { return New(c).MarkerPageFor(c.DefaultEnv()) }
