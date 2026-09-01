package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"vigil/internal/model"
)

// chromium launches a Chromium / chrome-headless-shell binary with a remote
// debugging port bound to 127.0.0.1 and hands out its websocket URL.
type chromium struct {
	binary   string
	headless bool
	logDir   string

	mu          sync.Mutex
	cmd         *exec.Cmd
	logFile     *os.File
	exited      chan error
	port        int
	userDataDir string
	wsURL       string
}

func (c *chromium) Kind() model.Browser { return model.BrowserChromium }

func (c *chromium) Healthy(ctx context.Context) bool {
	c.mu.Lock()
	port := c.port
	c.mu.Unlock()
	if port == 0 {
		return false
	}
	_, err := queryVersion(ctx, "127.0.0.1", port)
	return err == nil
}

// Ensure returns the running instance's endpoint or launches a fresh one on a free port.
func (c *chromium) Ensure(ctx context.Context) (*Endpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cmd != nil && c.port != 0 {
		if v, err := queryVersion(ctx, "127.0.0.1", c.port); err == nil {
			c.wsURL = v.WebSocketDebugURL
			return &Endpoint{Kind: model.BrowserChromium, WebSocketURL: c.wsURL}, nil
		}
		c.stopLocked() // stale process
	}
	if c.binary == "" {
		detected, err := DetectChromium()
		if err != nil {
			return nil, fmt.Errorf("chromium: %w", err)
		}
		c.binary = detected
	}
	bin := resolveBinary(c.binary)
	if !isExecutable(bin) {
		return nil, fmt.Errorf("chromium: binary %s is missing or not executable", bin)
	}
	port, err := FreePort()
	if err != nil {
		return nil, fmt.Errorf("chromium: free port: %w", err)
	}
	userDataDir, err := os.MkdirTemp("", "vigil-chromium-")
	if err != nil {
		return nil, fmt.Errorf("chromium: user-data-dir: %w", err)
	}
	logFile, err := openLog(c.logDir, fmt.Sprintf("chromium-%d.log", port))
	if err != nil {
		os.RemoveAll(userDataDir)
		return nil, fmt.Errorf("chromium: log: %w", err)
	}
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + userDataDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-extensions",
		"--disable-popup-blocking",
		"--mute-audio",
		"--window-size=1280,900",
	}
	if c.headless {
		args = append(args, "--headless=new", "--hide-scrollbars")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(bin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		os.RemoveAll(userDataDir)
		return nil, fmt.Errorf("chromium: start %s: %w", bin, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	c.cmd, c.logFile, c.exited, c.port, c.userDataDir = cmd, logFile, exited, port, userDataDir

	v, err := waitHealthy(ctx, "127.0.0.1", port, exited, 20*time.Second)
	if err != nil {
		c.stopLocked()
		return nil, fmt.Errorf("chromium: %w", err)
	}
	c.wsURL = v.WebSocketDebugURL
	return &Endpoint{Kind: model.BrowserChromium, WebSocketURL: c.wsURL}, nil
}

// Stop kills the launched process and removes its temporary profile.
func (c *chromium) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopLocked()
}

func (c *chromium) stopLocked() error {
	if c.cmd == nil {
		return nil
	}
	err := terminate(c.cmd, c.exited, 3*time.Second)
	if c.logFile != nil {
		c.logFile.Close()
	}
	if c.userDataDir != "" {
		os.RemoveAll(c.userDataDir)
	}
	c.cmd, c.logFile, c.exited, c.port, c.userDataDir, c.wsURL = nil, nil, nil, 0, "", ""
	return err
}
