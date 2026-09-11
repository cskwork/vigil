package runner

import (
	"context"
	"testing"
	"time"

	"vigil/internal/browser"
	"vigil/internal/model"
)

// fakeProvider stands in for a launched browser: Running flips on Ensure and
// off on Stop, so the test can see exactly when the runner lets go of it.
type fakeProvider struct {
	kind    model.Browser
	running bool
	stops   int
}

func (f *fakeProvider) Kind() model.Browser { return f.kind }
func (f *fakeProvider) Ensure(context.Context) (*browser.Endpoint, error) {
	f.running = true
	return &browser.Endpoint{Kind: f.kind, WebSocketURL: "ws://127.0.0.1:1/"}, nil
}
func (f *fakeProvider) Healthy(context.Context) bool { return f.running }
func (f *fakeProvider) Stop() error                  { f.running = false; f.stops++; return nil }
func (f *fakeProvider) Running() bool                { return f.running }

func TestStopIdleLeavesBusyAndRecentBrowsersAlone(t *testing.T) {
	lp := &fakeProvider{kind: model.BrowserLightpanda, running: true}
	ch := &fakeProvider{kind: model.BrowserChromium, running: true}
	r := New(map[model.Browser]browser.Provider{model.BrowserLightpanda: lp, model.BrowserChromium: ch})

	// never used: nothing says it is busy, so an idle sweep may stop it
	if got := r.StopIdle(time.Minute); len(got) != 2 {
		t.Fatalf("untouched browsers should be stopped, got %v", got)
	}
	if lp.stops != 1 || ch.stops != 1 || lp.running || ch.running {
		t.Fatalf("providers not stopped: %+v %+v", lp, ch)
	}

	// a run in flight pins its browser
	lp.running, ch.running = true, true
	release := r.acquire(model.BrowserLightpanda)
	if got := r.StopIdle(0); len(got) != 1 || got[0] != model.BrowserChromium {
		t.Fatalf("only the idle chromium should stop while lightpanda is busy, got %v", got)
	}
	if !lp.running {
		t.Fatal("busy lightpanda was stopped")
	}

	// just released: still inside the idle grace
	release()
	release() // releasing twice must not double-count
	if got := r.StopIdle(time.Hour); len(got) != 0 {
		t.Fatalf("recently used browser stopped early: %v", got)
	}
	// past the grace: stopped
	if got := r.StopIdle(0); len(got) != 1 || got[0] != model.BrowserLightpanda {
		t.Fatalf("idle lightpanda not stopped: %v", got)
	}
	// nothing running: a sweep is a no-op and never calls Stop again
	before := lp.stops + ch.stops
	if got := r.StopIdle(0); len(got) != 0 || lp.stops+ch.stops != before {
		t.Fatalf("stopped browsers must not be stopped again: %v", got)
	}
}
