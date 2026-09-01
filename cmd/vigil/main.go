// vigil CLI (PRD §19).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"vigil/internal/agent"
	"vigil/internal/browser"
	"vigil/internal/config"
	"vigil/internal/evidence"
	"vigil/internal/gate"
	"vigil/internal/ingest"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/runner"
	"vigil/internal/scheduler"
	"vigil/internal/store"
)

func main() {
	a := &app{cfgPath: "vigil.yaml", out: os.Stdout}
	cmd, args := a.parseGlobal(os.Args[1:])
	if cmd == "" {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := a.run(ctx, cmd, args)
	a.close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `vigil: continuous QA for deployed web apps

usage: vigil [-c vigil.yaml] [--json] <command> [flags]

  init                    write a starter vigil.yaml + url.md (only if missing)
  add [project]           register the project and import scenarios/ + flows/ from disk
  import --history        ingest the last N historical features (no gate/plan)
  scan                    import files, ingest new features once, deployment gate, orchestrate
  discover <feature>      enqueue a Browser Agent discovery task for a feature and run it now
  request <file.yaml>  hand a manual QA request (flow, accounts, mutation) to the orchestrator [--queue] [--dry-run]
  reparse <agent-dir>  rebuild agent-result.{json,yaml} of a finished agent run with the current parser
  prune [--dry-run]    apply evidence retention now (age policy + evidence.max_total_mb + db retention)
  run <scenario>          run one scenario now            [--browser lightpanda|chromium]
  run --feature <f>       run every scenario covering a feature
  run --impacted <f>      run scenarios impacted by a feature (feature/capability/route/path links)
  run --all               run every ACTIVE/SOAK scenario
  loop                    continuous scheduler + workers until SIGINT
  status                  queue / corpus / features / incidents snapshot
  coverage                coverage links, metrics and corpus health
  incidents [--all]       list open (or all) incidents
  approve <scenario>      NEEDS_REVIEW/CANDIDATE → SOAK (soak counter reset, due now)
  reject <scenario>       → REJECTED
  list [--state X]        list scenarios
  show <scenario>         current script yaml + coverage links + last runs
  validate                validate every scenario/flow file on disk
  doctor [--install]      check sqlite, browsers, pi, keys, sandbox, target, evidence dir

  serve [--addr 127.0.0.1:8787]   read-only live web view (what runs, what the AI agent does)
  loop --ui [addr]               loop + the same live view in-process

global flags: -c <config> (default vigil.yaml)   --json (status/coverage/incidents/list)
`)
}

// app carries lazily opened dependencies shared by the commands.
type app struct {
	cfgPath string
	jsonOut bool
	out     io.Writer

	cfg *config.Config
	st  *store.Store
	ev  *evidence.Store
	log *log.Logger

	providers map[model.Browser]browser.Provider
	logFile   *os.File
}

// parseGlobal extracts -c/--config and --json from anywhere in args and
// returns the command plus its remaining arguments.
func (a *app) parseGlobal(args []string) (string, []string) {
	var rest []string
	cmd := ""
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case s == "-c" || s == "--config":
			if i+1 < len(args) {
				a.cfgPath = args[i+1]
				i++
			}
		case strings.HasPrefix(s, "-c=") || strings.HasPrefix(s, "--config="):
			a.cfgPath = s[strings.Index(s, "=")+1:]
		case s == "--json":
			a.jsonOut = true
		case cmd == "" && !strings.HasPrefix(s, "-"):
			cmd = s
		default:
			rest = append(rest, s)
		}
	}
	return cmd, rest
}

func (a *app) run(ctx context.Context, cmd string, args []string) (int, error) {
	switch cmd {
	case "help", "-h", "--help":
		usage()
		return 0, nil
	case "init":
		return 0, a.cmdInit()
	}
	if err := a.open(); err != nil {
		return 1, err
	}
	switch cmd {
	case "add":
		return 0, a.cmdAdd(ctx, args)
	case "import":
		return 0, a.cmdImport(ctx, args)
	case "scan":
		return 0, a.cmdScan(ctx)
	case "prune":
		return 0, a.cmdPrune(args)
	case "reparse":
		return 0, a.cmdReparse(args)
	case "request":
		return 0, a.cmdRequest(ctx, args)
	case "discover":
		return 0, a.cmdDiscover(ctx, args)
	case "run":
		return a.cmdRun(ctx, args)
	case "loop":
		return 0, a.cmdLoop(ctx, args)
	case "serve":
		return 0, a.cmdServe(ctx, args)
	case "status":
		return 0, a.cmdStatus(ctx)
	case "coverage":
		return 0, a.cmdCoverage(ctx)
	case "incidents":
		return 0, a.cmdIncidents(ctx, args)
	case "approve":
		return 0, a.cmdApprove(ctx, args)
	case "reject":
		return 0, a.cmdReject(ctx, args)
	case "list":
		return 0, a.cmdList(ctx, args)
	case "show":
		return 0, a.cmdShow(ctx, args)
	case "validate":
		return a.cmdValidate()
	case "doctor":
		return a.cmdDoctor(ctx, args)
	}
	usage()
	return 2, fmt.Errorf("unknown command %q", cmd)
}

// open loads the config, the SQLite store and the evidence store.
func (a *app) open() error {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		return fmt.Errorf("config %s: %w (run `vigil init` to create one)", a.cfgPath, err)
	}
	a.cfg = cfg
	a.log = log.New(os.Stderr, "", log.LstdFlags)
	st, err := store.Open(cfg.Abs(cfg.State.Path))
	if err != nil {
		return fmt.Errorf("open state %s: %w", cfg.Abs(cfg.State.Path), err)
	}
	a.st = st
	a.ev = evidence.New(cfg.Abs(cfg.Evidence.Dir))
	return nil
}

func (a *app) close() {
	for _, p := range a.providers {
		_ = p.Stop()
	}
	if a.st != nil {
		_ = a.st.Close()
	}
	if a.logFile != nil {
		_ = a.logFile.Close()
	}
}

// stateDir is where vigil.log and browser logs live (next to the sqlite file).
func (a *app) stateDir() string { return filepath.Dir(a.cfg.Abs(a.cfg.State.Path)) }

// useLoopLogger sends log lines to stdout and <state dir>/vigil.log with timestamps.
func (a *app) useLoopLogger() error {
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(a.stateDir(), "vigil.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	a.logFile = f
	w := io.MultiWriter(os.Stdout, f)
	a.log = log.New(w, "", log.LstdFlags|log.LUTC)
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.LUTC)
	return nil
}

// ---- dependency builders --------------------------------------------------------

func (a *app) buildRunner() *runner.Runner {
	if a.providers == nil {
		logDir := filepath.Join(a.stateDir(), "browser-logs")
		_ = os.MkdirAll(logDir, 0o755)
		lp := a.cfg.Browser.Lightpanda
		a.providers = map[model.Browser]browser.Provider{
			model.BrowserLightpanda: browser.NewLightpanda(a.cfg.Abs(lp.Binary), lp.Host, lp.Port, logDir),
			model.BrowserChromium:   browser.NewChromium(a.cfg.Browser.Chromium.Binary, a.cfg.Browser.Chromium.Headless, logDir),
		}
	}
	return runner.New(a.providers)
}

// buildAgent returns nil (and warns) when the provider is unavailable (PRD rule 12).
func (a *app) buildAgent() agent.Adapter {
	ag, err := agent.NewPi(a.cfg)
	if err != nil {
		a.log.Printf("warn: Browser Agent unavailable, continuing with deterministic QA only: %v", err)
		return nil
	}
	return ag
}

func (a *app) buildOrchestrator(run *runner.Runner, ag agent.Adapter) *orchestrator.Orchestrator {
	return orchestrator.New(a.cfg, a.st, run, ag, a.ev)
}

func (a *app) buildIngest() ingest.Adapter {
	in, err := ingest.New(a.cfg, a.st)
	if err != nil {
		a.log.Printf("warn: discovery adapter %q unavailable: %v", a.cfg.Discovery.Adapter, err)
		return nil
	}
	return in
}

func (a *app) buildScheduler(orch *orchestrator.Orchestrator, run *runner.Runner, withIngest bool, agentAvailable bool) *scheduler.Scheduler {
	var in ingest.Adapter
	if withIngest {
		in = a.buildIngest()
	}
	s := scheduler.New(a.cfg, a.st, orch, run, gate.New(a.cfg), in)
	s.Log = a.log
	s.AgentAvailable = agentAvailable
	return s
}

// ---- output helpers ---------------------------------------------------------------

func (a *app) printf(format string, args ...any) { fmt.Fprintf(a.out, format, args...) }

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// parseFlags parses fs over args allowing flags before and after positionals.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
