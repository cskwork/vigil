// Package browser provides CDP endpoints for Lightpanda (default) and Chromium (fallback).
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/model"
)

// Endpoint is a live CDP websocket the runner can attach to.
type Endpoint struct {
	Kind         model.Browser
	WebSocketURL string
}

// Provider manages the lifecycle of one browser kind.
type Provider interface {
	Kind() model.Browser
	// Ensure starts the browser if needed and returns a connectable endpoint.
	Ensure(ctx context.Context) (*Endpoint, error)
	// Healthy reports whether the endpoint answers CDP /json/version.
	Healthy(ctx context.Context) bool
	// Stop terminates a browser this provider launched (no-op for attached ones).
	Stop() error
	// Running reports whether this provider currently owns a launched process,
	// i.e. whether Stop would actually free anything.
	Running() bool
}

// NewLightpanda returns a provider that attaches to a running Lightpanda CDP
// server on host:port or launches `binary serve` itself.
func NewLightpanda(binary, host string, port int, logDir string) Provider {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 9333
	}
	return &lightpanda{binary: binary, host: host, port: port, logDir: logDir}
}

// NewChromium returns a provider that launches the given Chromium/headless-shell
// binary with a remote debugging port on 127.0.0.1. An empty binary is resolved
// with DetectChromium on first Ensure.
func NewChromium(binary string, headless bool, logDir string) Provider {
	return &chromium{binary: binary, headless: headless, logDir: logDir}
}

// ---- shared helpers -------------------------------------------------------

// FreePort asks the kernel for an unused TCP port on 127.0.0.1.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// versionInfo is the subset of GET /json/version we rely on.
type versionInfo struct {
	Browser            string `json:"Browser"`
	WebSocketDebugURL  string `json:"webSocketDebuggerUrl"`
	LightpandaVersion  string `json:"Lightpanda-Version"`
	ProtocolVersion    string `json:"Protocol-Version"`
	UserAgentReported  string `json:"User-Agent"`
	V8Version          string `json:"V8-Version"`
	WebKitVersionField string `json:"WebKit-Version"`
}

// queryVersion fetches http://host:port/json/version. It returns an error when
// the endpoint does not answer or does not expose a websocket URL.
func queryVersion(ctx context.Context, host string, port int) (*versionInfo, error) {
	url := fmt.Sprintf("http://%s/json/version", net.JoinHostPort(host, fmt.Sprint(port)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", url, err)
	}
	if v.WebSocketDebugURL == "" {
		return nil, fmt.Errorf("%s: no webSocketDebuggerUrl", url)
	}
	return &v, nil
}

// waitHealthy polls /json/version until it answers, the process exits, or the deadline passes.
func waitHealthy(ctx context.Context, host string, port int, exited <-chan error, timeout time.Duration) (*versionInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("browser process exited before becoming healthy: %v (last probe: %v)", err, lastErr)
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		v, err := queryVersion(ctx, host, port)
		if err == nil {
			return v, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("browser on %s:%d not healthy after %s: %v", host, port, timeout, lastErr)
}

// openLog creates logDir/<name> for child-process output. When logDir is empty
// the process output goes to /dev/null.
func openLog(logDir, name string) (*os.File, error) {
	if logDir == "" {
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// resolveBinary makes a relative binary path absolute against the working directory
// so a launched process is not affected by later chdir calls.
func resolveBinary(binary string) string {
	if binary == "" || filepath.IsAbs(binary) || !strings.ContainsAny(binary, `/\`) {
		return binary
	}
	if abs, err := filepath.Abs(binary); err == nil {
		return abs
	}
	return binary
}
