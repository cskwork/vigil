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
	return &model.Feature{ID: "PROJ-1", LatestShippedSHA: "6f22ff7a1dcc309395173a5d52aba5ae01ad769a", ShippedAt: shippedAt, Routes: []string{"/lms-web/training-entry"}}
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
		if r.URL.Path != "/lms-web/training-entry" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<html><script type="module" src="/lms-web/assets/index.js?v=`+version.Load().(string)+`"></script></html>`)
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
