package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"vigil/internal/dsl"

	"gopkg.in/yaml.v3"

	"vigil/internal/config"
)

// pi is the Browser Agent adapter built on the `pi` coding agent CLI
// (`pi -p --mode json`) with the pi-agent-browser-native extension exposing the
// `agent_browser` tool. The process is wrapped with nono when configured.
type pi struct {
	cfg       *config.Config
	piPath    string
	chain     *Chain // ordered model fallback list with shared cooldown state (Run and Plan)
	extension string
	tools     string
	redactor  *Redactor
	sandbox   string // effective mode; grants are computed per run (evidence dir)
	nonoWarn  string
	logger    *log.Logger
}

// Extension package name / relative path (pi-agent-browser-native).
const (
	extPackage = "pi-agent-browser-native"
	extRelPath = "dist/extensions/agent-browser/index.js"
)

// NewPi builds the pi-based Browser Agent adapter (model fallback chain from
// agent.models or the legacy agent.model, agent_browser native tool, optional
// nono sandbox). It returns an error (never panics) when pi is missing so the
// caller can run without an agent (rule 12).
func NewPi(cfg *config.Config) (Adapter, error) {
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	piPath, err := exec.LookPath("pi")
	if err != nil {
		return nil, fmt.Errorf("agent: `pi` not found on PATH (install: npm i -g @earendil-works/pi-coding-agent): %w", err)
	}
	chain, err := NewChainFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	ext := ResolveExtension(cfg)
	mode, _, warn := ResolveSandboxMode(cfg)
	p := &pi{
		cfg:       cfg,
		piPath:    piPath,
		chain:     chain,
		extension: ext,
		tools:     "agent_browser,read",
		redactor:  RedactorFor(cfg.Personas, os.Environ()),
		sandbox:   mode,
		nonoWarn:  warn,
		logger:    log.New(os.Stderr, "[agent] ", log.LstdFlags),
	}
	return p, nil
}

// BundledExtensionPath is vigil's own dependency-free pi extension
// (piext/vigil-browser.js). It spawns only the `agent-browser` CLI, so it works
// inside `nono wrap` (pi-agent-browser-native execs /bin/ps, which nono denies).
func BundledExtensionPath(baseDir string) string {
	return filepath.Join(baseDir, "piext", "vigil-browser.js")
}

// ResolveExtension picks the extension file: explicit config → bundled
// piext/vigil-browser.js (when present) → pi-agent-browser-native.
func ResolveExtension(cfg *config.Config) string {
	if cfg.Agent.Extension != "" {
		return cfg.Abs(cfg.Agent.Extension)
	}
	if b := BundledExtensionPath(cfg.BaseDir); fileExists(b) {
		return b
	}
	return DefaultExtensionPath()
}

// ExtensionNeedsPS reports whether the resolved extension is the third-party
// pi-agent-browser-native (which needs /bin/ps and therefore a sandbox preflight).
func ExtensionNeedsPS(cfg *config.Config) bool {
	ext := ResolveExtension(cfg)
	return strings.Contains(ext, extPackage)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// DefaultExtensionPath resolves the pi-agent-browser-native extension entry:
// $HOME/.pi/agent/npm/node_modules/<pkg>/<rel>, then `npm root -g`.
func DefaultExtensionPath() string {
	home, _ := os.UserHomeDir()
	cands := []string{filepath.Join(home, ".pi", "agent", "npm", "node_modules", extPackage, extRelPath)}
	if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			cands = append(cands, filepath.Join(root, extPackage, extRelPath))
		}
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return cands[0]
}

// ---- Doctor ---------------------------------------------------------------

// Doctor verifies pi, the model catalog entry (with the mapped API key env),
// the extension file, agent-browser and the sandbox without spending a model call.
func (p *pi) Doctor(ctx context.Context) error {
	var problems []string
	if _, err := os.Stat(p.piPath); err != nil {
		problems = append(problems, "pi binary missing: npm i -g @earendil-works/pi-coding-agent")
	}
	if _, err := os.Stat(p.extension); err != nil {
		problems = append(problems, fmt.Sprintf("pi extension missing at %s: pi install npm:%s (or set agent.extension)", p.extension, extPackage))
	}
	if _, err := exec.LookPath("agent-browser"); err != nil {
		problems = append(problems, "agent-browser CLI missing: npm i -g agent-browser && agent-browser install")
	}
	for k, v := range p.cfg.Agent.EnvMap {
		if os.Getenv(v) == "" && os.Getenv(k) == "" {
			problems = append(problems, fmt.Sprintf("env %s (mapped from %s) is empty: export %s=<api key>", k, v, v))
		}
	}
	// Model catalog check: `pi --list-models <model>` lists only providers whose key is present.
	// One missing chain entry is a warning (the chain skips it); no listed entry is a problem.
	var missing []string
	for _, e := range p.chain.Entries() {
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cmd := exec.CommandContext(lctx, p.piPath, "--list-models", e.Model)
		cmd.Env = p.env("", "")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("pi --list-models %s failed: %v: %s", e.Model, err, strings.TrimSpace(string(out))))
		} else if !modelListed(string(out), e.Provider, e.Model) {
			missing = append(missing, fmt.Sprintf("%s (check ~/.pi/agent/models.json and the %s key): %s", e.Name(), e.Provider, firstLine(string(out))))
		}
	}
	if len(missing) > 0 && len(missing) == p.chain.Len() {
		problems = append(problems, "no chain model in pi catalog: "+strings.Join(missing, "; "))
	} else {
		for _, m := range missing {
			p.logger.Printf("WARN model %s not in pi catalog; the chain skips to the next entry", m)
		}
	}
	if p.sandbox != SandboxNone {
		dir := filepath.Join(os.TempDir(), "vigil-doctor")
		_ = os.MkdirAll(dir, 0o755)
		sb, warns := NewSandbox(p.cfg, dir)
		for _, w := range warns {
			p.logger.Printf("WARN %s", w)
		}
		if err := sb.Doctor(ctx, dir); err != nil {
			problems = append(problems, err.Error())
		}
	} else if p.nonoWarn != "" {
		p.logger.Printf("WARN %s", p.nonoWarn)
	}
	if len(problems) > 0 {
		return fmt.Errorf("agent doctor:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func modelListed(out, provider, model string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == provider && f[1] == model {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// AuthStatus is the JSON of `pi auth check --provider <p> --json --no-refresh`.
type AuthStatus struct {
	Status   string `json:"status"` // ready | not_ready
	Provider string `json:"provider"`
	AuthType string `json:"authType,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Ready reports whether pi has usable credentials for the provider.
func (s AuthStatus) Ready() bool { return s.Status == "ready" }

// ParseAuthCheck decodes the auth check output (tolerating a leading warning line).
func ParseAuthCheck(out []byte) (AuthStatus, error) {
	var st AuthStatus
	txt := strings.TrimSpace(string(out))
	i := strings.IndexByte(txt, '{')
	if i < 0 {
		return st, fmt.Errorf("no JSON in pi auth check output: %s", firstLine(txt))
	}
	if err := json.Unmarshal([]byte(txt[i:]), &st); err != nil {
		return st, fmt.Errorf("pi auth check output: %w: %s", err, firstLine(txt))
	}
	return st, nil
}

// ProviderAuth runs `pi auth check` for one provider without refreshing OAuth
// tokens (doctor). A not_ready status is returned with err == nil; err means the
// check itself could not run or answered without JSON.
func ProviderAuth(ctx context.Context, cfg *config.Config, piPath, provider string) (AuthStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, piPath, "auth", "check", "--provider", provider, "--json", "--no-refresh")
	cmd.Env = agentEnv(cfg, "", "")
	out, runErr := cmd.CombinedOutput()
	st, err := ParseAuthCheck(out)
	if err != nil {
		if runErr != nil {
			return st, fmt.Errorf("%v: %s", runErr, firstLine(string(out)))
		}
		return st, err
	}
	if st.Provider == "" {
		st.Provider = provider
	}
	return st, nil
}

// ---- Run ------------------------------------------------------------------

// Evidence file names written under evidenceDir.
const (
	FileRequest    = "agent-request.yaml"
	FileTranscript = "agent-transcript.jsonl"
	FileResult     = "agent-result.yaml"
	FileResultJSON = "agent-result.json"
	FileStderr     = "agent-stderr.log"
	FileSystem     = "agent-system-prompt.md"
	FileTask       = "agent-task-prompt.md"
	FileArgv       = "agent-argv.json"
)

var (
	transientRe = regexp.MustCompile(`(?i)rate ?limit|\b429\b|"code":\s*"1302"|overloaded|\b50[234]\b|server error|timeout|timed out|ECONNRESET|ECONNREFUSED|ETIMEDOUT|ENOTFOUND|EAI_AGAIN|temporarily|try again|fetch failed|socket hang up`)
	authRe      = regexp.MustCompile(`(?i)\b401\b|\b403\b|invalid api key|unauthori[sz]ed|authentication|no api key|missing api key|api key not|insufficient.*balance|quota`)
)

// providerErrorReason classifies a failed attempt's error text. kind is
// "transient" (429, 5xx, network: worth retrying), "auth" (key/quota: not worth
// retrying) or "" when the provider did not fail (task/tool problem).
func providerErrorReason(errText string) (kind, reason string) {
	switch {
	case errText == "":
		return "", ""
	case authRe.MatchString(errText) && !transientRe.MatchString(errText):
		return "auth", "auth/quota: " + firstLine(errText)
	case transientRe.MatchString(errText):
		return "transient", "transient: " + firstLine(errText)
	}
	return "", ""
}

// Run executes one bounded task on the model chain. On a provider error the
// current entry is cooled down and the next available entry is tried at once
// when one exists; otherwise (single entry, or every other entry cooling down)
// a transient error retries the same entry with agent.backoff up to
// agent.retries times, as before the chain existed, and only then is the entry
// cooled down. Auth/quota errors are never retried on the same entry. When no
// entry can answer the result has ModelUnavailable=true (nil error, rule 12).
func (p *pi) Run(ctx context.Context, req Request, evidenceDir string) (*Result, error) {
	if evidenceDir == "" {
		return nil, errors.New("agent: evidence dir required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, err
	}
	if len(req.Allowed) == 0 {
		req.Allowed = AllowedActions
	}
	if len(req.Forbidden) == 0 {
		req.Forbidden = ForbiddenActions
	}
	if req.MaxScenarios == 0 {
		req.MaxScenarios = p.cfg.Agent.MaxScenariosPerTask
	}
	if req.Task == "" {
		req.Task = TaskDiscover
	}
	start := time.Now()
	sys := SystemPrompt()
	budget := p.cfg.Agent.MaxTurns
	if req.MaxToolCalls > 0 {
		budget = req.MaxToolCalls
	}
	timeout := p.cfg.Agent.Timeout.Duration
	if req.TimeoutMinutes > 0 {
		timeout = time.Duration(req.TimeoutMinutes) * time.Minute
	}
	task := TaskPrompt(req) + fmt.Sprintf("\nmax_turns: %d tool calls.\n", budget)
	if b, err := yaml.Marshal(req); err == nil {
		_ = os.WriteFile(filepath.Join(evidenceDir, FileRequest), []byte(p.redactor.Redact(string(b))), 0o644)
	}
	_ = os.WriteFile(filepath.Join(evidenceDir, FileSystem), []byte(sys), 0o644)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileTask), []byte(p.redactor.Redact(task)), 0o644)

	if err := p.writeProjectSettings(evidenceDir); err != nil {
		p.logger.Printf("WARN pi project settings not written (model retries use pi defaults): %v", err)
	}
	sb, warns := NewSandbox(p.cfg, evidenceDir)
	for _, w := range warns {
		p.logger.Printf("WARN %s", w)
	}
	session := sessionName(req.FeatureID, start)
	env := append(p.env(session, evidenceDir), sb.Env...)
	sessionDir := filepath.Join(evidenceDir, "pi-session")
	_ = os.MkdirAll(sessionDir, 0o755)
	sessionID := fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", start.Unix()&0xffffffff, os.Getpid()&0xffff, start.Nanosecond()&0xfff, len(req.FeatureID)&0xfff, start.UnixNano()&0xffffffffffff)

	attempts := p.cfg.Agent.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var (
		res         *Result
		lastErr     string
		retryReason string // why the same entry is retried (logged before the backoff)
		tr          *Transcript
		spawns      int            // pi invocations of the main task, across models
		tried       []ModelAttempt // one row per spawn
		excluded    []string       // entries cooled down during this task (bounds switches to the chain length)
	)
	transcriptPath := filepath.Join(evidenceDir, FileTranscript)
	entry, ok := p.chain.Next()
	if !ok {
		res = &Result{ModelUnavailable: true, Reason: "model unavailable: every chain entry is cooling down (" + strings.Join(p.chain.Names(), ", ") + ")"}
	}
	// attempt counts retries of the current entry; a model switch resets it (no backoff).
	for attempt := 1; ok && attempt <= attempts; attempt++ {
		if attempt > 1 {
			wait := p.cfg.Agent.Backoff.Duration * time.Duration(attempt-1)
			p.logger.Printf("agent: %s %s; retry %d/%d in %s (no alternative model)", entry.Name(), retryReason, attempt, attempts, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		argv := sb.Argv(p.piArgvSession(entry, sys, task, sessionDir, sessionID)...)
		if b, err := json.MarshalIndent(argv, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(evidenceDir, FileArgv), b, 0o644)
		}
		spawns++
		var stderr string
		var timedOut, budgetHit bool
		var err error
		tr, stderr, timedOut, budgetHit, err = p.runOnce(ctx, argv, env, evidenceDir, transcriptPath, spawns, timeout, budget)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		text := tr.AssistantText
		if _, ok := ExtractResultBlock(text); !ok {
			text = tr.AllAssistantText
		}
		// Continuations: a long flow that ran out of tool calls resumes with a fresh budget
		// (same session, tools on) before we fall back to a tool-less WRAP UP.
		continuations := p.cfg.Agent.Continuations
		if req.MaxContinuations > 0 {
			continuations = req.MaxContinuations
		}
		usedContinuations := 0
		for c := 1; c <= continuations && budgetHit && !timedOut; c++ {
			if _, ok := ExtractResultBlock(text); ok {
				break
			}
			p.logger.Printf("agent hit the tool budget (%d) on %s; continuation %d/%d with a fresh budget", budget, req.FeatureID, c, continuations)
			ctr, _, ctimedOut, cbudgetHit, cerr := p.runOnce(ctx, sb.Argv(p.continueArgv(entry, sys, sessionDir, sessionID, budget)...), env, evidenceDir, transcriptPath, spawns, timeout, budget)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			usedContinuations = c
			if cerr != nil || ctr == nil {
				break
			}
			tr.ToolCalls += ctr.ToolCalls
			tr.ToolURLs = append(tr.ToolURLs, ctr.ToolURLs...)
			text = ctr.AssistantText
			if _, ok := ExtractResultBlock(text); !ok {
				text = ctr.AllAssistantText
			}
			budgetHit, timedOut = cbudgetHit, ctimedOut
		}
		wrappedUp := false
		if _, ok := ExtractResultBlock(text); !ok && (timedOut || budgetHit) {
			// One bounded resume: no tools, answer from what was observed.
			p.logger.Printf("agent %s (%s); resuming session once with WRAP UP", map[bool]string{true: "timed out", false: "hit the tool budget"}[timedOut], req.FeatureID)
			wtr, _, _, _, werr := p.runOnce(ctx, sb.Argv(p.wrapUpArgv(entry, sessionDir, sessionID)...), env, evidenceDir, transcriptPath, spawns, 4*time.Minute, 0)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if werr == nil && wtr != nil {
				if _, ok := ExtractResultBlock(wtr.AssistantText); ok {
					text = wtr.AssistantText
					wrappedUp = true
				}
				tr.ToolURLs = append(tr.ToolURLs, wtr.ToolURLs...)
			}
		}
		if r, perr := ParseResult(text); perr == nil {
			res = r
			res.Continuations = usedContinuations
			if wrappedUp {
				res.Reason = strings.TrimSpace("result produced after WRAP UP (timeout/tool budget); " + res.Reason)
			}
			if needsCompile(res) {
				p.compileCandidates(ctx, sb, env, evidenceDir, transcriptPath, spawns, entry, sessionDir, sessionID, res)
			}
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeOK})
			break
		} else if tr.AssistantText != "" && !budgetHit && !timedOut && tr.StopReason != "error" {
			// The model answered but not in the contract shape: do not burn retries.
			res = &Result{Decision: DecisionNeedsReview, Reason: perr.Error(), Evidence: truncate(tr.AssistantText, 4000)}
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeNoResult})
			break
		}
		errText := strings.TrimSpace(strings.Join([]string{tr.ErrorMessage, stderr}, "\n"))
		if err != nil && errText == "" {
			errText = err.Error()
		}
		lastErr = errText
		kind, reason := providerErrorReason(errText)
		switch {
		case budgetHit:
			res = &Result{Decision: DecisionNeedsReview, Reason: fmt.Sprintf("agent exceeded tool-call budget (%d)", budget)}
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeBudget})
		case timedOut:
			res = &Result{Decision: DecisionNeedsReview, Reason: fmt.Sprintf("agent timed out after %s", timeout)}
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeTimeout})
		case kind != "":
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeUnavailable})
			// Switch only when another entry is available right now.
			if next, more := p.chain.Next(append(append([]string{}, excluded...), entry.Name())...); more {
				p.chain.MarkUnavailable(entry.Name(), reason)
				excluded = append(excluded, entry.Name())
				p.logger.Printf("agent: %s unavailable (%s); switching to %s", entry.Name(), reason, next.Name())
				entry = next
				attempt = 0 // fresh retry budget for the new entry, no backoff on a switch
				continue
			}
			// No alternative: a transient error retries this entry with backoff
			// (agent.retries / agent.backoff); an auth/quota error is not retried.
			if kind == "transient" && attempt < attempts {
				retryReason = reason
				continue
			}
			p.chain.MarkUnavailable(entry.Name(), reason)
			res = &Result{ModelUnavailable: true, Reason: fmt.Sprintf("model unavailable: %s failed %d/%d attempt(s) and no alternative chain entry is available: %s", entry.Name(), attempt, attempts, firstLine(errText))}
		case tr.StopReason == "error" || err != nil:
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeError})
			retryReason = "error: " + firstLine(errText)
			continue // retry the same entry after backoff
		default:
			res = &Result{Decision: DecisionNeedsReview, Reason: "agent produced no result block"}
			tried = append(tried, ModelAttempt{entry.Name(), OutcomeNoResult})
		}
		break
	}
	if res == nil {
		res = &Result{ModelUnavailable: true, Reason: "model unavailable after retries: " + firstLine(lastErr)}
	}
	res.Attempts = spawns
	res.ModelAttempts = tried
	if !res.ModelUnavailable && spawns > 0 {
		res.Model = entry.Name()
	}
	res.Duration = time.Since(start)
	res.RawTranscriptPath = transcriptPath
	res.Sandbox = sb.Mode
	if tr != nil {
		res.ToolCalls = tr.ToolCalls
		if v := HostViolations(append(append([]string{}, res.VisitedURLs...), tr.ToolURLs...), req.AllowedHosts); len(v) > 0 {
			res.HostViolations = v
			if res.Decision != "" {
				res.Reason = strings.TrimSpace(res.Reason + " host allowlist violated: " + strings.Join(v, ", "))
				res.Decision = DecisionNeedsReview
			}
		}
	}
	p.writeResult(evidenceDir, res)
	p.closeBrowserSessions(session, transcriptPath)
	_ = os.RemoveAll(sessionDir) // pi session copy duplicates the transcript; only needed while resuming
	return res, nil
}

// closeBrowserSessions closes the task's agent-browser sessions (implicit one plus every
// role session the model opened via the tool's "session" argument) so headless browsers
// do not linger for the daemon idle timeout after a task ends.
func (p *pi) closeBrowserSessions(base, transcriptPath string) {
	names := map[string]bool{base: true}
	if f, err := os.Open(transcriptPath); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
		for sc.Scan() {
			var ev struct {
				Type string `json:"type"`
				Args struct {
					Session string `json:"session"`
				} `json:"args"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "tool_execution_start" && ev.Args.Session != "" {
				role := strings.ToLower(ev.Args.Session)
				role = strings.Map(func(r rune) rune {
					if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
						return r
					}
					return -1
				}, role)
				if len(role) > 24 {
					role = role[:24]
				}
				names[base+"-"+role] = true
			}
		}
	}
	bin, err := exec.LookPath("agent-browser")
	if err != nil {
		return
	}
	for name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, bin, "close")
		cmd.Env = append(os.Environ(), "AGENT_BROWSER_SESSION="+name)
		_ = cmd.Run()
		cancel()
	}
}

func (p *pi) writeResult(dir string, res *Result) {
	red := p.redactor.RedactResult(res)
	if b, err := yaml.Marshal(red); err == nil {
		_ = os.WriteFile(filepath.Join(dir, FileResult), b, 0o644)
	}
	if b, err := json.MarshalIndent(red, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, FileResultJSON), b, 0o644)
	}
}

// piArgv is the exact pi invocation (no sandbox wrapper) for one chain entry.
func (p *pi) piArgv(entry ModelEntry, systemPrompt, task string) []string {
	return p.piArgvSession(entry, systemPrompt, task, "", "")
}

// piArgvSession persists the conversation under sessionDir/sessionID so a
// timed-out task can be resumed once with a WRAP UP message (see wrapUpArgv).
func (p *pi) piArgvSession(entry ModelEntry, systemPrompt, task, sessionDir, sessionID string) []string {
	argv := []string{p.piPath, "-p", "--mode", "json"}
	if sessionDir != "" && sessionID != "" {
		argv = append(argv, "--session-dir", sessionDir, "--session-id", sessionID)
	} else {
		argv = append(argv, "--no-session")
	}
	return append(argv,
		"--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates", "--no-themes", "--approve",
		"-e", p.extension, "--tools", p.tools,
		"--provider", entry.Provider, "--model", entry.Model, "--thinking", entry.Thinking,
		"--system-prompt", systemPrompt,
		"--", task)
}

// WrapUpMessage is sent when the main run hit the timeout or tool budget: the
// model must answer from what it already observed, without tools.
const WrapUpMessage = "WRAP UP: the time/tool budget is exhausted. Do not call any tool. Output the vigil-result block now from what you already observed. If evidence is thin use decision NEEDS_REVIEW (or ORACLE_UNKNOWN) and list visited_urls."

// ContinueMessage resumes a session whose tool budget ran out.
const ContinueMessage = "CONTINUE: your tool budget has been renewed. Resume the task exactly where you stopped; do not repeat steps already completed. Prefer cheap commands (get text, wait --text, find) over full snapshots. When the flow is done or blocked, output the vigil-result block."

// continueArgv resumes the persisted session with tools enabled (same extension) and a fresh budget.
func (p *pi) continueArgv(entry ModelEntry, systemPrompt, sessionDir, sessionID string, budget int) []string {
	msg := ContinueMessage + fmt.Sprintf(" You have %d more tool calls.", budget)
	return p.piArgvSession(entry, systemPrompt, msg, sessionDir, sessionID)
}

// CompileMessage asks for prose candidates to be rewritten as vigil DSL documents.
const CompileMessage = `COMPILE: your script_candidates were not vigil DSL documents. Rewrite each candidate as ONE complete YAML document string in script_candidates, following exactly this shape (no prose steps, only the step kinds listed):

scenario: { id: <kebab-id>, version: 1, title: <Korean title>, class: P1, mutation: read-only|reversible }
covers: { feature: <feature_id>, capability: <dotted.key>, routes: [<path>] }
resources: { locks: [<account or fixture>] }   # only when mutation is not read-only
browser: { primary: chromium, popup: true }     # popup when a click opens a new tab
steps:
  - goto: <absolute-or-relative url>
  - click: { by: text, text: "선생님" }
  - fill: { by: css, value: "input[placeholder*='검색']", input: "teacher01" }
  - click: { by: text, text: "입장" }
  - expect_popup: { url_contains: app, timeout: 30s }
  - wait_for: { by: text, text: "AI 학습관", timeout: 20s }
  - assert_text: { value: "학습 전" }
assert: { no_http_5xx: true }
oracle: { source: observation, source_feature: <feature_id>, note: <what you saw> }

Step kinds: goto, click, fill, type, press, select, hover, wait_for, wait_ms, wait_url, assert_text, assert_no_text, assert_visible, assert_not_visible, assert_url, assert_count, assert_request, assert_attr, expect_popup, screenshot. Locators: by=text|css|role|label|id|test_id|href with text/value/role+name. Keep decision, evidence and the other fields unchanged. Output only the vigil-result block.`

// needsCompile reports whether any candidate is not a parseable DSL document.
func needsCompile(res *Result) bool {
	if res == nil || len(res.ScriptCandidates) == 0 {
		return false
	}
	for _, c := range res.ScriptCandidates {
		if _, err := dsl.Parse([]byte(c)); err != nil {
			return true
		}
	}
	return false
}

// compileCandidates makes one cheap tool-less call to turn prose candidates into DSL;
// on success the candidates are replaced and res.Compiled is set. Failure leaves res as is.
func (p *pi) compileCandidates(ctx context.Context, sb *Sandbox, env []string, evidenceDir, transcriptPath string, attempt int, entry ModelEntry, sessionDir, sessionID string, res *Result) {
	p.logger.Printf("agent candidates are not DSL; asking the model to compile them")
	argv := []string{p.piPath, "-p", "--mode", "json",
		"--session-dir", sessionDir, "--session-id", sessionID,
		"--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates", "--no-themes", "--approve", "--no-tools",
		"--provider", entry.Provider, "--model", entry.Model, "--thinking", "low",
		"--", CompileMessage}
	ctr, _, _, _, err := p.runOnce(ctx, sb.Argv(argv...), env, evidenceDir, transcriptPath, attempt, 4*time.Minute, 0)
	if err != nil || ctr == nil {
		return
	}
	r2, perr := ParseResult(ctr.AssistantText)
	if perr != nil || len(r2.ScriptCandidates) == 0 {
		return
	}
	var ok []string
	for _, c := range r2.ScriptCandidates {
		if _, err := dsl.Parse([]byte(c)); err == nil {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return
	}
	res.ScriptCandidates = ok
	res.Compiled = true
	if res.Decision == DecisionNeedsReview && r2.Decision == DecisionNewScript {
		res.Decision = DecisionNewScript
	}
}

// wrapUpArgv resumes the persisted session with tools disabled and low thinking.
func (p *pi) wrapUpArgv(entry ModelEntry, sessionDir, sessionID string) []string {
	return []string{p.piPath, "-p", "--mode", "json",
		"--session-dir", sessionDir, "--session-id", sessionID,
		"--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates", "--no-themes", "--approve", "--no-tools",
		"--provider", entry.Provider, "--model", entry.Model, "--thinking", "low",
		"--", WrapUpMessage}
}

// env builds a minimal environment: an allowlist of the parent env (no stray
// secrets), the mapped provider key, and agent-browser isolation settings.
func (p *pi) env(session, evidenceDir string) []string { return agentEnv(p.cfg, session, evidenceDir) }

func agentEnv(cfg *config.Config, session, evidenceDir string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true, "USER": true, "LOGNAME": true, "SHELL": true}
	var env []string
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			k = kv[:i]
		}
		if keep[k] {
			env = append(env, kv)
		}
	}
	for k, src := range cfg.Agent.EnvMap {
		v := os.Getenv(src)
		if v == "" {
			v = os.Getenv(k)
		}
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "PI_OFFLINE=1") // no startup update checks; API calls are unaffected
	if session != "" {
		env = append(env,
			"AGENT_BROWSER_SESSION="+session,
			"AGENT_BROWSER_IDLE_TIMEOUT_MS=300000",
			"AGENT_BROWSER_SCREENSHOT_DIR="+evidenceDir,
		)
	}
	return env
}

var sessionSafeRe = regexp.MustCompile(`[^a-z0-9-]+`)

func sessionName(feature string, at time.Time) string {
	f := sessionSafeRe.ReplaceAllString(strings.ToLower(feature), "-")
	f = strings.Trim(f, "-")
	if len(f) > 24 {
		f = f[:24]
	}
	if f == "" {
		f = "task"
	}
	return fmt.Sprintf("vigil-%s-%d", f, at.Unix())
}

// runOnce spawns pi once, streams JSONL to the transcript file (redacted) and folds it
// into a Transcript. It enforces cfg.Agent.Timeout and the tool-call budget.
func (p *pi) runOnce(ctx context.Context, argv, env []string, evidenceDir, transcriptPath string, attempt int, timeout time.Duration, budget int) (tr *Transcript, stderr string, timedOut, budgetHit bool, err error) {
	tr = &Transcript{}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = evidenceDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, perr := cmd.StdoutPipe()
	if perr != nil {
		return tr, "", false, false, perr
	}
	tf, ferr := os.OpenFile(transcriptPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if ferr != nil {
		return tr, "", false, false, ferr
	}
	defer tf.Close()
	fmt.Fprintf(tf, "{\"type\":\"vigil_attempt\",\"attempt\":%d,\"at\":%q,\"argv0\":%q}\n", attempt, time.Now().UTC().Format(time.RFC3339), argv[0])

	if err = cmd.Start(); err != nil {
		return tr, "", false, false, fmt.Errorf("spawn %s: %w", argv[0], err)
	}
	seen := map[string]bool{}
	var all []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"type":"message_update"`) { // streaming deltas: message_end carries the full text
			fmt.Fprintln(tf, p.redactor.Redact(line))
		}
		tr.ObserveLine(line, seen, &all)
		if budget > 0 && tr.ToolCalls > budget {
			budgetHit = true
			cancel()
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	stderr = p.redactor.Redact(errBuf.String())
	if stderr != "" {
		_ = os.WriteFile(filepath.Join(evidenceDir, FileStderr), []byte(stderr), 0o644)
	}
	if errors.Is(rctx.Err(), context.DeadlineExceeded) && !budgetHit {
		timedOut = true
	}
	if werr != nil && !timedOut && !budgetHit {
		err = werr
	}
	return tr, stderr, timedOut, budgetHit, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}

// writeProjectSettings puts a project-local pi settings file in the evidence dir (pi's cwd)
// so in-conversation 429/5xx retries follow cfg.Agent.ModelRetries / ModelRetryDelay
// (pi: retry.maxRetries, retry.baseDelayMs with exponential backoff). The run passes
// --approve, which trusts only this directory; extensions/skills stay disabled by flags.
func (p *pi) writeProjectSettings(evidenceDir string) error {
	dir := filepath.Join(evidenceDir, ".pi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settings := map[string]any{
		"retry": map[string]any{
			"enabled":     true,
			"maxRetries":  p.cfg.Agent.ModelRetries,
			"baseDelayMs": p.cfg.Agent.ModelRetryDelay.Duration.Milliseconds(),
		},
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644)
}

// Reparse rebuilds agent-result.{json,yaml} from an existing transcript with the current
// parser (used after parser improvements so earlier runs are not lost).
func (p *pi) Reparse(dir string) (*Result, error) {
	f, err := os.Open(filepath.Join(dir, FileTranscript))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr, err := ParseTranscript(f)
	if err != nil {
		return nil, err
	}
	text := tr.AssistantText
	if _, ok := ExtractResultBlock(text); !ok {
		text = tr.AllAssistantText
	}
	res, err := ParseResult(text)
	if err != nil {
		return nil, err
	}
	res.ToolCalls = tr.ToolCalls
	res.RawTranscriptPath = filepath.Join(dir, FileTranscript)
	p.writeResult(dir, res)
	return res, nil
}
