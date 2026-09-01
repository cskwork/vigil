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

// lightpanda attaches to an existing `lightpanda serve` on host:port or launches one.
type lightpanda struct {
	binary string
	host   string
	port   int
	logDir string

	mu      sync.Mutex
	cmd     *exec.Cmd  // non-nil only when this provider launched the process
	logFile *os.File   // child stdout/stderr
	exited  chan error // closed with the Wait() result once the child exits
}

func (l *lightpanda) Kind() model.Browser { return model.BrowserLightpanda }

func (l *lightpanda) Healthy(ctx context.Context) bool {
	_, err := queryVersion(ctx, l.host, l.port)
	return err == nil
}

// Ensure attaches when a healthy server already listens on host:port; otherwise
// it launches `binary serve --host host --port port` and waits for /json/version.
func (l *lightpanda) Ensure(ctx context.Context) (*Endpoint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if v, err := queryVersion(ctx, l.host, l.port); err == nil {
		return l.endpoint(v), nil
	}
	if l.cmd != nil {
		// We launched it earlier but it is gone or unhealthy: reap and relaunch.
		l.stopLocked()
	}
	bin := resolveBinary(l.binary)
	if bin == "" || !isExecutable(bin) {
		if bin == "" {
			bin = "bin/lightpanda"
		}
		return nil, fmt.Errorf("lightpanda: no server on %s:%d and binary %s is missing or not executable; run `vigil doctor --install` to download the nightly build", l.host, l.port, bin)
	}
	logFile, err := openLog(l.logDir, fmt.Sprintf("lightpanda-%d.log", l.port))
	if err != nil {
		return nil, fmt.Errorf("lightpanda: log: %w", err)
	}
	cmd := exec.Command(bin, "serve", "--host", l.host, "--port", fmt.Sprint(l.port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("lightpanda: start %s: %w", bin, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	l.cmd, l.logFile, l.exited = cmd, logFile, exited

	v, err := waitHealthy(ctx, l.host, l.port, exited, 15*time.Second)
	if err != nil {
		l.stopLocked()
		return nil, fmt.Errorf("lightpanda: %w", err)
	}
	return l.endpoint(v), nil
}

func (l *lightpanda) endpoint(v *versionInfo) *Endpoint {
	ws := v.WebSocketDebugURL
	if ws == "" {
		ws = fmt.Sprintf("ws://%s:%d/", l.host, l.port)
	}
	return &Endpoint{Kind: model.BrowserLightpanda, WebSocketURL: ws}
}

// Stop kills the process only if this provider launched it.
func (l *lightpanda) Stop() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopLocked()
}

func (l *lightpanda) stopLocked() error {
	if l.cmd == nil {
		return nil
	}
	err := terminate(l.cmd, l.exited, 3*time.Second)
	if l.logFile != nil {
		l.logFile.Close()
	}
	l.cmd, l.logFile, l.exited = nil, nil, nil
	return err
}

// terminate sends SIGTERM, waits up to grace, then SIGKILLs.
func terminate(cmd *exec.Cmd, exited <-chan error, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	select {
	case <-exited:
		return nil // already gone
	default:
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-exited:
		return nil
	case <-time.After(grace):
	}
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	<-exited
	return nil
}
