package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vigil/internal/config"
)

// Sandbox modes (Result.Sandbox values).
const (
	SandboxNone     = "none"
	SandboxNonoWrap = "nono-wrap" // `nono wrap`: apply Seatbelt profile and exec (no supervisor)
	SandboxNonoRun  = "nono-run"  // `nono run`: supervised; required for proxy domain filtering
)

// Sandbox is a fully resolved nono argv specification. It is pure data so the
// argv builder can be unit-tested without nono installed.
type Sandbox struct {
	Mode   string // none | nono-wrap | nono-run
	Binary string // nono path (Mode != none)

	Allow          []string // rw dirs
	Read           []string // ro dirs
	AllowFiles     []string
	ReadFiles      []string
	SocketDirsBind []string // --allow-unix-socket-dir-bind
	// AllowedDomains enables nono's proxy allowlist (nono-run only).
	AllowedDomains []string
	// Profile is a generated nono profile JSON (raw Seatbelt allowances Chromium needs).
	Profile string
	LogFile string
	GPU     bool
	Silent  bool
	// Env holds extra environment entries the sandboxed run needs
	// (e.g. AGENT_BROWSER_EXECUTABLE_PATH → Chromium wrapper with --no-sandbox).
	Env []string
}

// Argv wraps program (argv[0] must be an absolute path or PATH-resolvable) with
// the sandbox invocation. With Mode none it returns program unchanged.
func (s *Sandbox) Argv(program ...string) []string {
	if s == nil || s.Mode == SandboxNone || s.Mode == "" {
		return append([]string(nil), program...)
	}
	sub := "wrap"
	if s.Mode == SandboxNonoRun {
		sub = "run"
	}
	argv := []string{s.Binary, sub}
	if s.Silent {
		argv = append(argv, "-s")
	}
	if s.Profile != "" {
		argv = append(argv, "-p", s.Profile)
	}
	if s.LogFile != "" {
		argv = append(argv, "--log-file", s.LogFile)
	}
	if s.Mode == SandboxNonoRun {
		argv = append(argv, "--no-audit", "--no-rollback", "--no-rollback-prompt", "--diagnostics-json")
	}
	// cwd is the evidence dir; nono refuses to run non-interactively without --allow-cwd.
	argv = append(argv, "--allow-cwd")
	for _, d := range s.Allow {
		argv = append(argv, "--allow", d)
	}
	for _, d := range s.Read {
		argv = append(argv, "--read", d)
	}
	for _, f := range s.AllowFiles {
		argv = append(argv, "--allow-file", f)
	}
	for _, f := range s.ReadFiles {
		argv = append(argv, "--read-file", f)
	}
	for _, d := range s.SocketDirsBind {
		argv = append(argv, "--allow-unix-socket-dir-bind", d)
	}
	if s.GPU {
		argv = append(argv, "--allow-gpu")
	}
	if s.Mode == SandboxNonoRun {
		for _, d := range s.AllowedDomains {
			argv = append(argv, "--allow-domain", d)
		}
	}
	argv = append(argv, "--")
	return append(argv, program...)
}

// ResolveSandboxMode maps cfg.Agent.Sandbox (auto|nono|none) to an effective mode.
// It returns the nono binary path when a nono mode is selected.
func ResolveSandboxMode(cfg *config.Config) (mode, binary string, warn string) {
	want := strings.ToLower(strings.TrimSpace(cfg.Agent.Sandbox))
	if want == "" {
		want = "auto"
	}
	if want == "none" {
		return SandboxNone, "", ""
	}
	bin, err := exec.LookPath("nono")
	if err != nil {
		if want == "nono" {
			return SandboxNone, "", "agent.sandbox=nono but nono is not on PATH (brew install nono); running the Browser Agent unsandboxed"
		}
		return SandboxNone, "", ""
	}
	if !ExtensionNeedsPS(cfg) {
		// bundled piext/vigil-browser.js: no /bin/ps dependency, no preflight needed
	} else if perr := sandboxPreflight(bin); perr != nil {
		msg := "nono sandbox preflight failed: " + perr.Error()
		if want == "nono" {
			warn = msg + "; agent.sandbox=nono forces the sandbox anyway (agent_browser will likely fail with 'spawn EPERM')"
		} else {
			return SandboxNone, "", msg + "; running the Browser Agent unsandboxed (set agent.sandbox: nono to force, none to silence)"
		}
	}
	if cfg.Agent.SandboxNetworkFilter {
		return SandboxNonoRun, bin, warn
	}
	return SandboxNonoWrap, bin, warn
}

var (
	preflightOnce sync.Once
	preflightErr  error
)

// sandboxPreflight checks the one host capability the pi-agent-browser-native
// extension needs that nono may deny: exec of /bin/ps (the extension runs
// `/bin/ps -p <pid> -o lstart=` to stamp its secure temp root; Node throws
// "spawn EPERM" synchronously when Seatbelt denies the exec, and every browser
// command then fails). Verified on nono 0.74.0/macOS: no profile setting
// (security modes, commands.allow, groups.exclude, raw process-exec rule) lifts it.
func sandboxPreflight(nono string) error {
	preflightOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, nono, "wrap", "-s", "--allow-cwd", "--", "/bin/ps", "-p", fmt.Sprint(os.Getpid()), "-o", "lstart=")
		cmd.Dir = os.TempDir()
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			preflightErr = fmt.Errorf("`nono wrap -- /bin/ps` denied (%v: %s); the pi agent_browser extension needs /bin/ps for its temp-root liveness check", err, strings.TrimSpace(errb.String()))
		}
	})
	return preflightErr
}

// NewSandbox builds the grant set the pi + agent_browser + Chromium process tree
// needs on this machine (verified empirically on macOS with nono 0.74):
//
//	rw  evidence dir (cwd)              transcript, screenshots, agent-browser outputs
//	rw  $HOME/.pi                       pi usage/run-history, extension npm tree, ext config
//	ro  symlink targets under ~/.pi/agent (models.json, settings.json, extensions → pi-setup)
//	ro  node install root               node, pi, agent-browser packages
//	rw  $HOME/.agent-browser            agent-browser state dir
//	rw  /private/tmp/piab-<uid> (+bind) agent-browser daemon sockets (extension default)
//	rw  /tmp, /private/tmp, $TMPDIR     Chromium profile/temp, pi temp roots
//	ro  ~/Library/Caches/ms-playwright  bundled Chromium builds
//	ro  /Applications/Google Chrome.app system Chrome (fallback engine)
//
// Chromium additionally needs Seatbelt allowances nono's default profile denies
// (user-preference-read, mach-register for its rendezvous server, iokit user
// clients, file-issue-extension, ...): they are emitted into a generated profile
// (nono-profile.json). Chromium's own sandbox cannot re-initialise inside
// Seatbelt, so the browser is launched through a wrapper script that adds
// --no-sandbox --disable-gpu --use-mock-keychain (chromium-sandboxed.sh) via
// AGENT_BROWSER_EXECUTABLE_PATH. GPU (IOKit) access is granted because Chromium
// probes it even with --disable-gpu.
//
// The application repo is never granted. Network is allowed (nono default);
// with cfg.Agent.SandboxNetworkFilter the proxy allowlist is target.allowed_hosts.
//
// Known limitation (nono 0.74.0): exec of /bin/ps is denied inside the sandbox
// and the pi extension requires it, so sandbox "auto" degrades to none with a
// WARN when the preflight fails (see sandboxPreflight); "nono" forces it.
func NewSandbox(cfg *config.Config, evidenceDir string) (*Sandbox, []string) {
	mode, bin, warn := ResolveSandboxMode(cfg)
	var warns []string
	if warn != "" {
		warns = append(warns, warn)
	}
	s := &Sandbox{Mode: mode, Binary: bin, Silent: true, GPU: true}
	if mode == SandboxNone {
		return s, warns
	}
	home, _ := os.UserHomeDir()
	s.LogFile = filepath.Join(evidenceDir, "nono.log")
	if p, err := writeProfile(evidenceDir, home); err != nil {
		warns = append(warns, "nono profile not written: "+err.Error())
	} else {
		s.Profile = p
	}
	if w, err := writeChromiumWrapper(evidenceDir, home); err != nil {
		warns = append(warns, "Chromium wrapper not written (Chromium may crash inside the sandbox): "+err.Error())
	} else {
		s.Env = append(s.Env, "AGENT_BROWSER_EXECUTABLE_PATH="+w)
	}

	add := func(list *[]string, p string) {
		if p == "" {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		for _, e := range *list {
			if e == p {
				return
			}
		}
		*list = append(*list, p)
	}
	addBoth := func(list *[]string, p string) {
		add(list, p)
		if rp, err := filepath.EvalSymlinks(p); err == nil && rp != p {
			add(list, rp)
		}
	}

	addBoth(&s.Allow, evidenceDir)
	// the extension file pi loads with -e (bundled piext/ or pi-agent-browser-native)
	addBoth(&s.Read, filepath.Dir(ResolveExtension(cfg)))
	addBoth(&s.Allow, filepath.Join(home, ".pi"))
	// pi's agent dir may be a symlink farm (models.json → ~/pi-setup/...): grant the targets read-only.
	if entries, err := os.ReadDir(filepath.Join(home, ".pi", "agent")); err == nil {
		for _, e := range entries {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			p := filepath.Join(home, ".pi", "agent", e.Name())
			if rp, err := filepath.EvalSymlinks(p); err == nil {
				if st, err := os.Stat(rp); err == nil {
					if st.IsDir() {
						add(&s.Read, rp)
					} else {
						add(&s.ReadFiles, rp)
					}
				}
			}
		}
	}
	if nodeDir := nodeInstallRoot(); nodeDir != "" {
		addBoth(&s.Read, nodeDir)
	}
	addBoth(&s.Allow, filepath.Join(home, ".agent-browser"))
	sock := AgentBrowserSocketDir()
	_ = os.MkdirAll(sock, 0o700)
	addBoth(&s.Allow, sock)
	addBoth(&s.SocketDirsBind, sock)
	for _, t := range []string{"/tmp", "/private/tmp", os.TempDir()} {
		addBoth(&s.Allow, strings.TrimSuffix(t, "/"))
	}
	addBoth(&s.Read, filepath.Join(home, "Library", "Caches", "ms-playwright"))
	addBoth(&s.Read, "/Applications/Google Chrome.app")
	if cfg.Agent.SandboxNetworkFilter {
		s.AllowedDomains = append(s.AllowedDomains, cfg.Target.AllowedHosts...)
		// the model provider must stay reachable through the proxy
		s.AllowedDomains = append(s.AllowedDomains, "api.z.ai", "open.bigmodel.cn")
	}
	return s, warns
}

// AgentBrowserSocketDir mirrors pi-agent-browser-native's default daemon socket
// directory (/private/tmp/piab-<uid> on macOS, /tmp/piab-<uid> elsewhere).
func AgentBrowserSocketDir() string {
	if v := os.Getenv("PI_AGENT_BROWSER_SOCKET_DIR"); v != "" {
		return v
	}
	prefix := "/tmp/piab"
	if _, err := os.Stat("/private/tmp"); err == nil {
		prefix = "/private/tmp/piab"
	}
	return fmt.Sprintf("%s-%d", prefix, os.Getuid())
}

// nodeInstallRoot returns <prefix> for <prefix>/bin/node (e.g. ~/.nvm/versions/node/v22.x).
func nodeInstallRoot() string {
	node, err := exec.LookPath("node")
	if err != nil {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(node); err == nil {
		node = rp
	}
	return filepath.Dir(filepath.Dir(node))
}

// Doctor runs `/bin/echo ok` inside the sandbox and returns nono's stderr on failure.
func (s *Sandbox) Doctor(ctx context.Context, workdir string) error {
	if s == nil || s.Mode == SandboxNone {
		return nil
	}
	if workdir == "" {
		workdir = os.TempDir()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	argv := s.Argv("/bin/echo", "ok")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sandbox doctor (%s): %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(errb.String()))
	}
	if strings.TrimSpace(out.String()) != "ok" {
		return fmt.Errorf("sandbox doctor: unexpected output %q (stderr: %s)", out.String(), strings.TrimSpace(errb.String()))
	}
	return nil
}

// seatbeltRules are the raw allowances Chromium needs beyond nono's default profile
// (found with `nono run --diagnostics-json`; see NewSandbox).
var seatbeltRules = []string{
	`(allow user-preference-read)`,
	`(allow user-preference-write (preference-domain "com.google.chrome.for.testing") (preference-domain "com.google.Chrome") (preference-domain "org.chromium.Chromium"))`,
	`(allow iokit-open-user-client)`,
	`(allow file-read-metadata)`,
	`(allow file-read-xattr)`,
	`(allow file-issue-extension)`,
	`(allow mach-register)`,
	`(allow mach-lookup (global-name "com.apple.SecurityServer") (global-name "com.apple.trustd") (global-name "com.apple.trustd.agent") (global-name "com.apple.ocspd") (global-name "com.apple.system.opendirectoryd.libinfo") (global-name "com.apple.system.notification_center") (global-name "com.apple.CoreServices.coreservicesd") (global-name "com.apple.lsd.mapdb") (global-name "com.apple.fonts") (global-name "com.apple.FontObjectsServer") (global-name "com.apple.coreservices.launchservicesd") (global-name "com.apple.cfprefsd.daemon") (global-name "com.apple.cfprefsd.agent") (global-name "com.apple.logd") (global-name "com.apple.diagnosticd") (global-name "com.apple.analyticsd") (global-name "com.apple.distributed_notifications@Uv3"))`,
	`(allow sysctl-read)`,
}

// ProfileJSON renders the generated nono profile (extends default).
func ProfileJSON(home string) []byte {
	rules := append([]string{}, seatbeltRules...)
	if home != "" {
		rules = append(rules, fmt.Sprintf(`(allow file-read-data (literal %q))`, home)) // Chromium stats $HOME itself
	}
	doc := map[string]any{
		"extends":   "default",
		"meta":      map[string]string{"name": "vigil-agent", "description": "vigil Browser Agent: pi + agent_browser + Chromium (generated)"},
		"allow_gpu": true, // Chromium probes IOKit/Metal even with --disable-gpu
		// The pi extension checks liveness of its own temp-root owner (kill(pid,0) + `ps`)
		// and Chromium coordinates its child processes; nono's default "isolated" modes
		// make those calls fail with EPERM ("spawn EPERM" in the agent transcript).
		"security": map[string]string{
			"signal_mode":       "allow_same_sandbox",
			"process_info_mode": "allow_same_sandbox",
			"ipc_mode":          "full",
		},
		"unsafe_macos_seatbelt_rules": rules,
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

func writeProfile(dir, home string) (string, error) {
	p := filepath.Join(dir, "nono-profile.json")
	if err := os.WriteFile(p, ProfileJSON(home), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// ChromiumBinary picks a Chromium build that starts inside the sandbox:
// Playwright's Chrome for Testing / Chromium / headless shell, then system Chrome.
func ChromiumBinary(home string) string {
	if v := os.Getenv("AGENT_BROWSER_EXECUTABLE_PATH"); v != "" {
		return v
	}
	cache := filepath.Join(home, "Library", "Caches", "ms-playwright")
	patterns := []string{
		filepath.Join(cache, "chromium-*", "chrome-mac-arm64", "Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing"),
		filepath.Join(cache, "chromium-*", "chrome-mac*", "Chromium.app", "Contents", "MacOS", "Chromium"),
		filepath.Join(cache, "chromium_headless_shell-*", "chrome-headless-shell-mac*", "chrome-headless-shell"),
		filepath.Join(cache, "chromium-*", "chrome-linux", "chrome"),
	}
	var best string
	for _, pat := range patterns {
		m, _ := filepath.Glob(pat)
		sort.Strings(m)
		if len(m) > 0 {
			best = m[len(m)-1] // newest build number sorts last
			break
		}
	}
	if best == "" {
		for _, c := range []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Chromium.app/Contents/MacOS/Chromium", "/usr/bin/chromium", "/usr/bin/google-chrome"} {
			if _, err := os.Stat(c); err == nil {
				best = c
				break
			}
		}
	}
	return best
}

func writeChromiumWrapper(dir, home string) (string, error) {
	bin := ChromiumBinary(home)
	if bin == "" {
		return "", errors.New("no Chromium build found (agent-browser install)")
	}
	p := filepath.Join(dir, "chromium-sandboxed.sh")
	script := "#!/bin/sh\n# generated by vigil: Chromium cannot re-initialise its own sandbox inside nono/Seatbelt\nexec " +
		shellQuote(bin) + " --no-sandbox --disable-gpu --use-mock-keychain --password-store=basic --disable-crash-reporter \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
