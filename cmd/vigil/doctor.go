package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/agent"
	"vigil/internal/browser"
	"vigil/internal/config"
	"vigil/internal/gate"
)

// doctorRow is one line of the doctor table.
type doctorRow struct {
	Check  string `json:"check"`
	Status string `json:"status"` // OK | WARN | FAIL
	Detail string `json:"detail"`
}

type doctor struct {
	rows []doctorRow
	hard int
}

func (d *doctor) ok(check, detail string) { d.rows = append(d.rows, doctorRow{check, "OK", detail}) }
func (d *doctor) warn(check, detail string) {
	d.rows = append(d.rows, doctorRow{check, "WARN", detail})
}
func (d *doctor) fail(check, detail string) {
	d.hard++
	d.rows = append(d.rows, doctorRow{check, "FAIL", detail})
}

func (a *app) cmdDoctor(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	install := fs.Bool("install", false, "download the Lightpanda binary when missing")
	if _, err := parseFlags(fs, args); err != nil {
		return 2, err
	}
	d := &doctor{}
	cfg := a.cfg

	// config + targets
	detail := fmt.Sprintf("project=%s base_url=%s hosts=%v", cfg.Project.ID, cfg.Target.BaseURL, cfg.Target.AllowedHosts)
	if t, ok := cfg.PrimaryTarget(); ok {
		detail += fmt.Sprintf(" url_file=%s primary=%s (%d url(s))", cfg.Target.URLFile, t.URL, len(cfg.Targets))
	} else {
		detail += " (no url.md; base_url from yaml)"
	}
	d.ok("config", detail)

	// sqlite
	if a.st == nil {
		d.fail("sqlite", "store not open")
	} else if _, err := a.st.Counts(ctx, cfg.Project.ID); err != nil {
		d.fail("sqlite", err.Error())
	} else {
		d.ok("sqlite", cfg.Abs(cfg.State.Path))
	}

	// evidence dir writable
	evDir := cfg.Abs(cfg.Evidence.Dir)
	if err := os.MkdirAll(evDir, 0o755); err != nil {
		d.fail("evidence", err.Error())
	} else if f, err := os.CreateTemp(evDir, ".doctor-*"); err != nil {
		d.fail("evidence", fmt.Sprintf("%s not writable: %v", evDir, err))
	} else {
		name := f.Name()
		f.Close()
		os.Remove(name)
		d.ok("evidence", evDir+" writable")
	}

	// repo / branch (generic-git only)
	if cfg.UsesAdapter(config.AdapterGenericGit) || cfg.UsesAdapter(config.AdapterSDLCKit) {
		repo := cfg.Abs(cfg.Project.Repo)
		if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
			d.fail("repo", fmt.Sprintf("%s missing (project.repo)", repo))
		} else if cfg.UsesAdapter(config.AdapterGenericGit) {
			out, err := exec.CommandContext(ctx, "git", "-C", repo, "log", "-1", "--format=%h %cI %s", cfg.Discovery.Branch, "--").Output()
			if err != nil {
				d.fail("repo", fmt.Sprintf("%s: branch %s not resolvable: %v", repo, cfg.Discovery.Branch, err))
			} else {
				d.ok("repo", fmt.Sprintf("%s %s → %s", repo, cfg.Discovery.Branch, trunc(strings.TrimSpace(string(out)), 90)))
			}
		} else {
			d.ok("repo", repo)
		}
	}

	// lightpanda binary + serve probe
	lp := cfg.Browser.Lightpanda
	lpBin := cfg.Abs(lp.Binary)
	if _, err := os.Stat(lpBin); err != nil && *install {
		if got, ierr := browser.EnsureLightpandaBinary(lpBin); ierr != nil {
			d.warn("lightpanda install", ierr.Error())
		} else {
			d.ok("lightpanda install", "downloaded "+got)
			if got != "" {
				lpBin = got
			}
		}
	}
	if fi, err := os.Stat(lpBin); err != nil {
		d.warn("lightpanda binary", fmt.Sprintf("%s missing (run `vigil doctor --install`)", lpBin))
	} else if fi.Mode()&0o111 == 0 {
		d.warn("lightpanda binary", lpBin+" is not executable (chmod +x)")
	} else {
		ver := "version unknown"
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if out, err := exec.CommandContext(vctx, lpBin, "version").CombinedOutput(); err == nil {
			ver = trunc(strings.TrimSpace(string(out)), 60)
		}
		cancel()
		d.ok("lightpanda binary", fmt.Sprintf("%s (%s)", lpBin, ver))
		a.probeProvider(ctx, d, "lightpanda serve", browser.NewLightpanda(lpBin, lp.Host, lp.Port, doctorLogDir(a)), fmt.Sprintf("%s:%d", lp.Host, lp.Port))
	}
	if lp.Port == 9222 {
		d.warn("lightpanda port", "9222 is Chrome's default remote-debugging port; use another port (e.g. 9333)")
	}

	// chromium
	chBin := cfg.Browser.Chromium.Binary
	if chBin == "" {
		a.probeProvider(ctx, d, "chromium (auto-detect)", browser.NewChromium("", cfg.Browser.Chromium.Headless, doctorLogDir(a)), "")
	} else if _, err := os.Stat(chBin); err != nil {
		d.warn("chromium binary", chBin+" missing (fallback/confirmation browser unavailable)")
	} else {
		d.ok("chromium binary", chBin)
		a.probeProvider(ctx, d, "chromium launch", browser.NewChromium(chBin, cfg.Browser.Chromium.Headless, doctorLogDir(a)), "")
	}

	// pi + model chain auth + keys + extension + sandbox
	piPath, piErr := exec.LookPath("pi")
	if piErr != nil {
		d.warn("pi", "not on PATH; Browser Agent disabled, deterministic QA continues (rule 12)")
	} else {
		vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out, _ := exec.CommandContext(vctx, piPath, "--version").CombinedOutput()
		cancel()
		d.ok("pi", fmt.Sprintf("%s %s", piPath, trunc(strings.TrimSpace(string(out)), 40)))
	}
	chain, chainErr := agent.NewChainFromConfig(cfg)
	if chainErr != nil {
		d.fail("model chain", chainErr.Error())
	} else {
		// One line per chain entry: `pi auth check --provider <p> --json --no-refresh`.
		// Not ready is a warning: the chain skips that entry at run time.
		for _, e := range chain.Entries() {
			check := "model " + e.Name()
			if piErr != nil {
				d.warn(check, fmt.Sprintf("thinking=%s; auth not checked (pi missing)", e.Thinking))
				continue
			}
			st, err := agent.ProviderAuth(ctx, cfg, piPath, e.Provider)
			switch {
			case err != nil:
				d.warn(check, fmt.Sprintf("thinking=%s; auth check failed: %s", e.Thinking, trunc(err.Error(), 120)))
			case st.Ready():
				d.ok(check, fmt.Sprintf("thinking=%s auth=%s (%s)", e.Thinking, st.Status, st.AuthType))
			default:
				reason := st.Reason
				if reason == "" {
					reason = st.Status
				}
				d.warn(check, fmt.Sprintf("thinking=%s auth=%s (%s); the chain skips this entry (pi auth login %s)", e.Thinking, st.Status, reason, e.Provider))
			}
		}
	}
	for piVar, shellVar := range cfg.Agent.EnvMap {
		if os.Getenv(shellVar) == "" {
			d.warn("api key "+piVar, fmt.Sprintf("env %s is empty (chain entries using it fail auth and are skipped)", shellVar))
		} else {
			d.ok("api key "+piVar, fmt.Sprintf("from env %s (%d chars)", shellVar, len(os.Getenv(shellVar))))
		}
	}
	ext := cfg.Agent.Extension
	if ext == "" {
		ext = agent.DefaultExtensionPath()
	}
	if _, err := os.Stat(ext); err != nil {
		d.warn("pi extension", ext+" missing (npm i -g pi-agent-browser-native)")
	} else {
		d.ok("pi extension", ext)
	}
	if cfg.Agent.DomainFile == "" {
		d.ok("domain file", "not configured (agent.domain_file); tasks run without domain rules")
	} else if rules, err := agent.ReadDomainFile(cfg.Abs(cfg.Agent.DomainFile)); err != nil {
		d.warn("domain file", fmt.Sprintf("%s: %v (tasks run without domain rules)", cfg.Agent.DomainFile, err))
	} else {
		note := ""
		if strings.HasSuffix(rules, agent.DomainTruncatedMarker) {
			note = fmt.Sprintf(", truncated to %d KB", agent.MaxDomainFileBytes/1024)
		}
		d.ok("domain file", fmt.Sprintf("%s (%d bytes%s)", cfg.Agent.DomainFile, len(rules), note))
	}
	mode, nonoBin, nonoWarn := agent.ResolveSandboxMode(cfg)
	if nonoBin != "" {
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _ := exec.CommandContext(vctx, nonoBin, "--version").CombinedOutput()
		cancel()
		d.ok("nono sandbox", fmt.Sprintf("mode=%s %s (%s)", mode, nonoBin, trunc(strings.TrimSpace(string(out)), 30)))
	} else if nonoWarn != "" {
		d.warn("nono sandbox", nonoWarn)
	} else {
		d.warn("nono sandbox", fmt.Sprintf("mode=%s (agent.sandbox=%s, nono not on PATH → agent runs unsandboxed)", mode, cfg.Agent.Sandbox))
	}
	if ag, err := agent.NewPi(cfg); err != nil {
		d.warn("agent", err.Error())
	} else {
		actx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := ag.Doctor(actx); err != nil {
			d.warn("agent doctor", trunc(err.Error(), 200))
		} else {
			d.ok("agent doctor", fmt.Sprintf("%s models=%s cooldown=%s", cfg.Agent.Provider, strings.Join(cfg.ModelEntries(), " → "), cfg.Agent.ModelCooldown.Duration))
		}
		cancel()
	}

	// target reachability + asset marker
	entry := strings.TrimRight(cfg.Target.BaseURL, "/") + cfg.EntryPath()
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	req, _ := http.NewRequestWithContext(tctx, http.MethodGet, entry, nil)
	req.Header.Set("User-Agent", "vigil-doctor/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.fail("target", fmt.Sprintf("GET %s: %v", entry, err))
	} else {
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			d.fail("target", fmt.Sprintf("GET %s → HTTP %d", entry, resp.StatusCode))
		} else {
			d.ok("target", fmt.Sprintf("GET %s → HTTP %d", entry, resp.StatusCode))
		}
	}
	cancel()
	g := gate.New(cfg)
	if cfg.Deployment.Readiness.Strategy == "asset_version" {
		// One line per environment: each serves its own build, and runs are
		// tagged with the marker of the environment they ran against.
		for _, name := range cfg.EnvNames() {
			env, err := cfg.Env(name)
			if err != nil {
				continue
			}
			check := "asset marker"
			if len(cfg.Target.Environments) > 0 {
				check += " " + name
			}
			page := g.MarkerPageFor(env)
			mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if marker, err := g.FetchAssetMarker(mctx, page); err != nil {
				d.warn(check, fmt.Sprintf("%s: %v (gate falls back to max_wait=%s)", page, err, cfg.Deployment.Readiness.MaxWait.Duration))
			} else {
				d.ok(check, fmt.Sprintf("assets/index.js?v=%s at %s", marker, page))
			}
			cancel()
		}
	}

	if a.jsonOut {
		return exitFor(d), a.printJSON(map[string]any{"checks": d.rows, "hard_failures": d.hard})
	}
	a.printf("%-24s %-5s %s\n", "CHECK", "STATE", "DETAIL")
	for _, r := range d.rows {
		a.printf("%-24s %-5s %s\n", r.Check, r.Status, r.Detail)
	}
	if d.hard > 0 {
		a.printf("\n%d hard failure(s)\n", d.hard)
	} else {
		a.printf("\nno hard failures\n")
	}
	return exitFor(d), nil
}

func exitFor(d *doctor) int {
	if d.hard > 0 {
		return 1
	}
	return 0
}

func doctorLogDir(a *app) string {
	dir := filepath.Join(a.stateDir(), "browser-logs")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// probeProvider attaches to an already healthy endpoint or launches the browser once and stops it.
func (a *app) probeProvider(ctx context.Context, d *doctor, check string, p browser.Provider, addr string) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if addr != "" && p.Healthy(pctx) {
		d.ok(check, "attached to healthy CDP server at "+addr)
		return
	}
	ep, err := p.Ensure(pctx)
	if err != nil {
		d.warn(check, trunc(err.Error(), 160))
		return
	}
	_ = p.Stop()
	ws := ""
	if ep != nil {
		ws = ep.WebSocketURL
	}
	d.ok(check, "launched and answered CDP ("+trunc(ws, 80)+")")
}
