package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"vigil/internal/browser"
	"vigil/internal/model"
)

// Runner executes DSL scenarios over CDP (chromedp) against a browser Provider.
// No LLM is involved (PRD §5.2, AC-06).
//
// Connection model (measured 2026-09-01, see runner_e2e_test.go):
//   - Chromium supports many page targets per connection, so the runner keeps one
//     connection per browser kind and gives every run its own tab in a fresh
//     browser context (isolated cookies/storage; concurrent runs are safe).
//   - Lightpanda allows exactly one page target per websocket connection
//     (Target.createTarget → TargetAlreadyLoaded), so every run opens its own
//     connection. Connections are independent: closing one does not disturb
//     another, and the server keeps running.
//
// Which model applies is probed once per browser kind, not hardcoded.
type Runner struct {
	providers map[model.Browser]browser.Provider

	mu      sync.Mutex
	handles map[model.Browser]*browserHandle
}

// browserHandle is a cached connection for browsers that support multiple tabs.
// For exclusive browsers only the probed mode is cached (baseCtx == nil).
type browserHandle struct {
	multiTab bool
	wsURL    string
	baseCtx  context.Context
	cancel   func()
}

func New(providers map[model.Browser]browser.Provider) *Runner {
	return &Runner{providers: providers, handles: map[model.Browser]*browserHandle{}}
}

// Close releases cached browser connections. Providers are stopped by their owner.
func (r *Runner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, h := range r.handles {
		if h.cancel != nil {
			h.cancel()
		}
		delete(r.handles, k)
	}
	return nil
}

const (
	defaultStepTimeout = 15 * time.Second
	defaultRunTimeout  = 3 * time.Minute
	attachTimeout      = 30 * time.Second
	evidenceGrace      = 8 * time.Second
)

// Run executes spec once and returns a Result. Errors are returned only for
// runner-internal failures (bad spec, missing provider); page/browser failures
// are encoded in Result.
func (r *Runner) Run(ctx context.Context, spec Spec) (*Result, error) {
	if spec.Scenario == nil {
		return nil, errors.New("runner: spec.Scenario is nil")
	}
	if spec.Browser == "" {
		spec.Browser = model.BrowserLightpanda
	}
	provider, ok := r.providers[spec.Browser]
	if !ok || provider == nil {
		return nil, fmt.Errorf("runner: no provider for browser %q", spec.Browser)
	}
	steps, err := flatten(spec.Scenario, spec.Flows)
	if err != nil {
		return nil, err
	}
	if spec.StepTimeout <= 0 {
		spec.StepTimeout = defaultStepTimeout
	}
	if spec.RunTimeout <= 0 {
		spec.RunTimeout = defaultRunTimeout
	}
	if spec.EvidenceDir != "" {
		if err := os.MkdirAll(spec.EvidenceDir, 0o755); err != nil {
			return nil, fmt.Errorf("runner: evidence dir: %w", err)
		}
	}

	start := time.Now()
	res := &Result{
		Browser:      spec.Browser,
		StartedAt:    start,
		Artifacts:    map[string]string{},
		Capabilities: map[string]bool{},
	}
	runCtx, cancelRun := context.WithTimeout(ctx, spec.RunTimeout)
	defer cancelRun()

	s := &session{
		spec:   spec,
		steps:  steps,
		res:    res,
		cap:    newCapture(start),
		red:    newRedactor(spec.Persona),
		runCtx: runCtx,
	}

	pageCtx, cleanup, err := r.openPage(runCtx, provider, s.cap)
	if err != nil {
		res.Class = FailTransport
		res.Error = "browser unavailable: " + err.Error()
		res.FinishedAt = time.Now()
		s.writeEvidence()
		return res, nil
	}
	s.mainCtx, s.pageCtx = pageCtx, pageCtx
	s.closers = append(s.closers, cleanup)

	s.execute()
	s.finish()
	s.writeEvidence()
	s.closeAll()
	return res, nil
}

// openPage returns a page context for one run. The returned context is not
// bound to runCtx so evidence can still be captured after a run timeout; the
// session enforces deadlines per step and the cleanup closes the page.
func (r *Runner) openPage(runCtx context.Context, p browser.Provider, cap *capture) (context.Context, func(), error) {
	ep, err := p.Ensure(runCtx)
	if err != nil {
		return nil, nil, err
	}
	h, err := r.handle(runCtx, p.Kind(), ep.WebSocketURL)
	if err != nil {
		return nil, nil, err
	}
	if h.multiTab {
		tabCtx, cancelTab, err := openTab(h, runCtx, cap)
		if err == nil {
			return tabCtx, cancelTab, nil
		}
		// The cached connection may be stale: drop it, reconnect once, retry.
		r.dropHandle(p.Kind(), h)
		h2, err2 := r.handle(runCtx, p.Kind(), ep.WebSocketURL)
		if err2 != nil {
			return nil, nil, fmt.Errorf("open tab: %w (reconnect: %v)", err, err2)
		}
		if h2.multiTab {
			if tabCtx, cancelTab, err3 := openTab(h2, runCtx, cap); err3 == nil {
				return tabCtx, cancelTab, nil
			}
			return nil, nil, fmt.Errorf("open tab: %w", err)
		}
		h = h2 // the browser no longer supports tabs; fall through to exclusive mode
	}
	// exclusive: one connection per run
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), ep.WebSocketURL, chromedp.NoModifyURL)
	pageCtx, cancelPage := chromedp.NewContext(allocCtx)
	cap.listenTarget(pageCtx)
	cap.listenBrowser(pageCtx)
	cleanup := func() { cancelPage(); cancelAlloc() }
	if err := runWithTimeout(pageCtx, runCtx, attachTimeout); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("attach: %w", err)
	}
	return pageCtx, cleanup, nil
}

// openTab creates one isolated tab (own browser context) on a cached connection.
func openTab(h *browserHandle, runCtx context.Context, cap *capture) (context.Context, func(), error) {
	tabCtx, cancelTab := chromedp.NewContext(h.baseCtx, chromedp.WithNewBrowserContext())
	cap.listenTarget(tabCtx)
	cap.listenBrowser(tabCtx)
	if err := runWithTimeout(tabCtx, runCtx, attachTimeout); err != nil {
		cancelTab()
		return nil, nil, err
	}
	return tabCtx, cancelTab, nil
}

// handle returns the cached connection mode for kind, probing it on first use.
func (r *Runner) handle(runCtx context.Context, kind model.Browser, wsURL string) (*browserHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.handles[kind]; ok && h.wsURL == wsURL && h.alive() {
		return h, nil
	}
	if h, ok := r.handles[kind]; ok && h.cancel != nil {
		h.cancel()
	}
	delete(r.handles, kind)

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), wsURL, chromedp.NoModifyURL)
	baseCtx, cancelBase := chromedp.NewContext(allocCtx)
	cancel := func() { cancelBase(); cancelAlloc() }
	if err := runWithTimeout(baseCtx, runCtx, attachTimeout); err != nil {
		cancel()
		return nil, fmt.Errorf("attach %s: %w", kind, err)
	}
	// Probe: can this browser open a second page target on the same connection?
	tabCtx, cancelTab := chromedp.NewContext(baseCtx)
	probeErr := runWithTimeout(tabCtx, runCtx, 10*time.Second)
	cancelTab()
	h := &browserHandle{wsURL: wsURL}
	if probeErr == nil {
		h.multiTab, h.baseCtx, h.cancel = true, baseCtx, cancel
	} else {
		cancel() // exclusive browsers get a fresh connection per run
	}
	r.handles[kind] = h
	return h, nil
}

func (r *Runner) dropHandle(kind model.Browser, h *browserHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.handles[kind]; ok && cur == h {
		if h.cancel != nil {
			h.cancel()
		}
		delete(r.handles, kind)
	}
}

func (h *browserHandle) alive() bool {
	if !h.multiTab {
		return true
	}
	if h.baseCtx.Err() != nil {
		return false
	}
	c := chromedp.FromContext(h.baseCtx)
	if c == nil || c.Browser == nil {
		return false
	}
	select {
	case <-c.Browser.LostConnection:
		return false
	default:
		return true
	}
}

// runWithTimeout performs the first chromedp.Run on a long-lived context (which
// allocates/attaches its target) while bounding the wait by timeout and runCtx.
// The target's lifetime stays tied to ctx, not to the timeout.
func runWithTimeout(ctx, runCtx context.Context, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- chromedp.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s", timeout)
	case <-runCtx.Done():
		return runCtx.Err()
	}
}
