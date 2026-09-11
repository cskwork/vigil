// Package gate implements the deployment readiness gate (PRD §6).
//
// A shipped Git feature is not tested until the deployed URL is known (or
// assumed by policy) to serve that revision. Three strategies exist:
//
//   - delay:            READY once now >= shipped_at + delay_after_ship.
//   - asset_version:    READY when the page's `assets/index.js?v=<n>` marker
//     changed since the feature was first seen; DEPLOYMENT_UNKNOWN once
//     max_wait has elapsed (the caller treats UNKNOWN as "delay policy
//     expired → proceed").
//   - version_endpoint: READY when GET endpoint returns a body containing the
//     first 7 characters of the shipped SHA; DEPLOYMENT_UNKNOWN after max_wait.
//
// Network failures never produce an error: the gate answers WAITING and logs,
// because a deployment delay is not APP_FAILURE.
package gate

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
)

// AssetVersionRe extracts the deploy marker from the deployed index.html.
var AssetVersionRe = regexp.MustCompile(`assets/index\.js\?v=(\d+)`)

const maxBody = 2 << 20 // 2 MiB is plenty for an index.html or version endpoint

type Gate struct {
	cfg *config.Config
	// HTTP is the client used for asset_version / version_endpoint probes.
	HTTP *http.Client
	// Now is injectable for tests.
	Now func() time.Time
	// Log receives WAITING-on-error diagnostics; defaults to the std logger.
	Log *log.Logger

	mu      sync.Mutex
	markers map[string]markerCache // environment name -> last probed marker
}

type markerCache struct {
	marker string
	at     time.Time
}

func New(cfg *config.Config) *Gate {
	return &Gate{
		cfg:  cfg,
		HTTP: &http.Client{Timeout: 10 * time.Second},
		Now:  func() time.Time { return time.Now().UTC() },
		Log:  log.Default(),
	}
}

// Check evaluates readiness for a feature. `lastMarker` is the previously
// observed deployment marker for this feature SHA (asset_version strategy:
// empty on first sight) and the returned marker must be persisted by the caller.
func (g *Gate) Check(ctx context.Context, f *model.Feature, lastMarker string) (state model.Readiness, marker string, err error) {
	if f == nil {
		return model.ReadinessUnknown, lastMarker, fmt.Errorf("gate: nil feature")
	}
	if !model.IsShipKind(f.Kind) {
		// An issue or a log signature has nothing to wait for: no deployment
		// follows it, so it is READY at once and never probes the target.
		return model.ReadinessReady, lastMarker, nil
	}
	r := g.cfg.Deployment.Readiness
	switch r.Strategy {
	case "", "delay":
		if DelayReady(f.ShippedAt, r.DelayAfterShip.Duration, g.Now()) {
			return model.ReadinessReady, lastMarker, nil
		}
		return model.ReadinessWaiting, lastMarker, nil
	case "asset_version":
		return g.checkAssetVersion(ctx, f, lastMarker)
	case "version_endpoint":
		return g.checkVersionEndpoint(ctx, f, lastMarker)
	}
	return model.ReadinessUnknown, lastMarker, fmt.Errorf("gate: unknown readiness strategy %q", r.Strategy)
}

// DelayReady is the pure delay policy helper.
func DelayReady(shippedAt time.Time, delay time.Duration, now time.Time) bool {
	return !now.Before(shippedAt.Add(delay))
}

// AssetPage returns the page probed by the asset_version strategy: the
// configured asset_page, else base_url + the feature's first route, else base_url.
func (g *Gate) AssetPage(f *model.Feature) string {
	if p := g.cfg.Deployment.Readiness.AssetPage; p != "" {
		return p
	}
	base := strings.TrimRight(g.cfg.Target.BaseURL, "/")
	if f != nil && len(f.Routes) > 0 {
		return base + "/" + strings.TrimLeft(f.Routes[0], "/")
	}
	return base
}

func (g *Gate) checkAssetVersion(ctx context.Context, f *model.Feature, lastMarker string) (model.Readiness, string, error) {
	page := g.AssetPage(f)
	current, err := g.FetchAssetMarker(ctx, page)
	if err != nil {
		g.logf("gate: asset_version probe of %s failed (%v) → WAITING", page, err)
		return model.ReadinessWaiting, lastMarker, nil
	}
	if lastMarker == "" {
		// First sight of this feature SHA: remember the marker that was live at ship time.
		return model.ReadinessWaiting, current, nil
	}
	if current != lastMarker {
		return model.ReadinessReady, current, nil
	}
	if g.maxWaitExpired(f) {
		return model.ReadinessUnknown, current, nil
	}
	return model.ReadinessWaiting, lastMarker, nil
}

func (g *Gate) checkVersionEndpoint(ctx context.Context, f *model.Feature, lastMarker string) (model.Readiness, string, error) {
	endpoint := g.cfg.Deployment.Readiness.Endpoint
	if endpoint == "" {
		return model.ReadinessUnknown, lastMarker, fmt.Errorf("gate: version_endpoint strategy needs deployment.readiness.endpoint")
	}
	body, err := g.get(ctx, endpoint)
	if err != nil {
		g.logf("gate: version endpoint %s failed (%v) → WAITING", endpoint, err)
		return model.ReadinessWaiting, lastMarker, nil
	}
	prefix := shaPrefix(f.LatestShippedSHA)
	if prefix != "" && strings.Contains(body, prefix) {
		return model.ReadinessReady, prefix, nil
	}
	if g.maxWaitExpired(f) {
		return model.ReadinessUnknown, lastMarker, nil
	}
	return model.ReadinessWaiting, lastMarker, nil
}

// FetchAssetMarker GETs page and returns the `assets/index.js?v=` value.
func (g *Gate) FetchAssetMarker(ctx context.Context, page string) (string, error) {
	body, err := g.get(ctx, page)
	if err != nil {
		return "", err
	}
	m := AssetVersionRe.FindStringSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("no assets/index.js?v= marker in %s", page)
	}
	return m[1], nil
}

// MarkerPageFor returns the page whose asset marker identifies env's deployment:
// the environment's asset_page, else deployment.readiness.asset_page for the
// default environment, else that environment's base URL plus the entry path.
func (g *Gate) MarkerPageFor(env config.Environment) string {
	if p := strings.TrimSpace(env.AssetPage); p != "" {
		return p
	}
	if env.Name == "" || env.Name == g.cfg.DefaultEnv().Name {
		if p := strings.TrimSpace(g.cfg.Deployment.Readiness.AssetPage); p != "" {
			return p
		}
	}
	base := strings.TrimRight(env.BaseURL, "/")
	if base == "" {
		base = strings.TrimRight(g.cfg.Target.BaseURL, "/")
	}
	if entry := g.cfg.EntryPath(); entry != "" && entry != "/" {
		return base + "/" + strings.TrimLeft(entry, "/")
	}
	return base
}

// CurrentMarkerFor returns the deployed build marker of one environment right
// now (asset_version strategy), cached per environment for 60s so every run can
// be tagged cheaply. Two environments serve different builds, so a run must be
// tagged with the marker of the environment it actually ran against.
// Other strategies return "" (no per-run build identity available).
func (g *Gate) CurrentMarkerFor(ctx context.Context, env config.Environment) (string, error) {
	if g.cfg.Deployment.Readiness.Strategy != "asset_version" {
		return "", nil
	}
	key := env.Name
	g.mu.Lock()
	if c, ok := g.markers[key]; ok && time.Since(c.at) < 60*time.Second {
		g.mu.Unlock()
		return c.marker, nil
	}
	g.mu.Unlock()
	m, err := g.FetchAssetMarker(ctx, g.MarkerPageFor(env))
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	if g.markers == nil {
		g.markers = map[string]markerCache{}
	}
	g.markers[key] = markerCache{marker: m, at: time.Now()}
	g.mu.Unlock()
	return m, nil
}

// CurrentMarker answers for the default environment.
func (g *Gate) CurrentMarker(ctx context.Context) (string, error) {
	return g.CurrentMarkerFor(ctx, g.cfg.DefaultEnv())
}

func (g *Gate) get(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "vigil-gate/1")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (g *Gate) maxWaitExpired(f *model.Feature) bool {
	mw := g.cfg.Deployment.Readiness.MaxWait.Duration
	if mw <= 0 {
		return false
	}
	return g.Now().After(f.ShippedAt.Add(mw))
}

func (g *Gate) logf(format string, a ...any) {
	if g.Log != nil {
		g.Log.Printf(format, a...)
	}
}

// shaPrefix returns the 7-character prefix used for version-endpoint matching
// ("" when the SHA is too short to be meaningful).
func shaPrefix(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) < 7 {
		return ""
	}
	return sha[:7]
}
