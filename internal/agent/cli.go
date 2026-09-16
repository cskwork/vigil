// CLI adapters let an operator spend the coding-agent subscription they already
// have (Claude Code, Codex) instead of an API key. vigil keeps the whole
// contract: the same system prompt, the same bounded browser tool - reached
// through `vigil mcp-browser` over MCP - and the same result block.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"vigil/internal/config"
)

// Supported coding-agent CLIs (config agent.provider).
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// MCPServerName is the MCP server name vigil registers with the CLI. Clients
// derive the tool name from it.
const MCPServerName = "vigil"

// Evidence files written by a CLI run, next to the shared agent-* files.
const (
	FileCLIRaw = "agent-cli-raw.jsonl"
	FileFinal  = "agent-final.md"
)

// IsCLIProvider reports whether provider names a coding-agent CLI adapter.
func IsCLIProvider(provider string) bool {
	return provider == ProviderClaude || provider == ProviderCodex
}

type cliAgent struct {
	cfg      *config.Config
	kind     string
	bin      string // resolved CLI binary
	self     string // vigil binary, re-executed as the MCP browser server
	tool     string // tool name as the CLI exposes it, used in the prompt
	redactor *Redactor
	logger   *log.Logger
}

// NewCLI builds the adapter for agent.provider claude or codex. It fails when
// the CLI or agent-browser is missing so the caller can fall back (rule 12).
func NewCLI(cfg *config.Config) (Adapter, error) {
	if cfg == nil {
		return nil, errors.New("agent: nil config")
	}
	kind := cfg.Agent.Provider
	if !IsCLIProvider(kind) {
		return nil, fmt.Errorf("agent: %q is not a coding-agent CLI", kind)
	}
	bin, err := lookCLI(kind)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("agent: cannot locate the vigil binary for the browser tool: %w", err)
	}
	if _, err := exec.LookPath("agent-browser"); err != nil {
		return nil, fmt.Errorf("agent: agent-browser CLI missing (npm i -g agent-browser && agent-browser install): %w", err)
	}
	return &cliAgent{
		cfg: cfg, kind: kind, bin: bin, self: self,
		tool:     toolNameFor(kind),
		redactor: RedactorFor(cfg.Personas, os.Environ()),
		logger:   log.New(os.Stderr, "[agent] ", log.LstdFlags),
	}, nil
}

// toolNameFor is how each client renames an MCP tool.
func toolNameFor(kind string) string {
	if kind == ProviderClaude {
		return "mcp__" + MCPServerName + "__" + BrowserTool
	}
	return MCPServerName + "__" + BrowserTool
}

// lookCLI resolves the CLI on PATH, then in the usual per-user install dirs
// (a shell alias is not visible to a spawned process).
func lookCLI(kind string) (string, error) {
	if p, err := exec.LookPath(kind); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local", "bin", kind),
		filepath.Join(home, ".claude", "local", kind),
		filepath.Join("/opt/homebrew/bin", kind),
		filepath.Join("/usr/local/bin", kind),
	} {
		if fileExists(c) {
			return c, nil
		}
	}
	install := "npm i -g @anthropic-ai/claude-code"
	if kind == ProviderCodex {
		install = "npm i -g @openai/codex"
	}
	return "", fmt.Errorf("agent: `%s` not found on PATH (install: %s)", kind, install)
}

// mcpConfig is the server definition handed to the CLI.
func (c *cliAgent) mcpConfig(session, evidenceDir string) map[string]any {
	return map[string]any{
		"command": c.self,
		"args":    []string{"mcp-browser"},
		"env": map[string]string{
			"AGENT_BROWSER_SESSION":         session,
			"AGENT_BROWSER_IDLE_TIMEOUT_MS": "300000",
			"AGENT_BROWSER_SCREENSHOT_DIR":  evidenceDir,
		},
	}
}

// argv builds the CLI invocation. The task prompt is passed on stdin, so a long
// request never hits an argument-length limit.
func (c *cliAgent) argv(sys, evidenceDir, session string, req Request, lastMessage string) ([]string, error) {
	model := strings.TrimSpace(c.cfg.Agent.CLIModel)
	images := req.AttachedImages()
	if c.kind == ProviderClaude {
		cfgJSON, err := json.Marshal(map[string]any{"mcpServers": map[string]any{MCPServerName: c.mcpConfig(session, evidenceDir)}})
		if err != nil {
			return nil, err
		}
		argv := []string{c.bin, "-p", "--output-format", "stream-json", "--verbose",
			"--append-system-prompt", sys,
			"--mcp-config", string(cfgJSON), "--strict-mcp-config",
			// Only the browser tool and reading files (the uploaded screens).
			"--allowedTools", c.tool + ",Read",
			"--permission-prompts", "none",
			"--restricted",
		}
		if model != "" {
			argv = append(argv, "--model", model)
		}
		for _, dir := range attachmentDirs(images) {
			argv = append(argv, "--add-dir", dir)
		}
		return argv, nil
	}
	argv := []string{c.bin, "exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
		"-s", "read-only", "-C", evidenceDir, "-o", lastMessage,
		// Nobody is at the terminal: shell approvals are refused outright, while
		// the browser tool is pre-approved because it is the bounded surface
		// vigil itself serves.
		"-c", `approval_policy="never"`,
		"-c", "mcp_servers." + MCPServerName + ".command=" + strconv.Quote(c.self),
		"-c", "mcp_servers." + MCPServerName + `.args=["mcp-browser"]`,
		"-c", "mcp_servers." + MCPServerName + `.default_tools_approval_mode="approve"`,
		"-c", "mcp_servers." + MCPServerName + ".env={AGENT_BROWSER_SESSION=" + strconv.Quote(session) +
			",AGENT_BROWSER_IDLE_TIMEOUT_MS=\"300000\",AGENT_BROWSER_SCREENSHOT_DIR=" + strconv.Quote(evidenceDir) + "}",
	}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	for _, img := range images {
		argv = append(argv, "-i", img)
	}
	return append(argv, "-"), nil
}

// attachmentDirs lists the distinct directories holding attachment images.
func attachmentDirs(images []string) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, img := range images {
		d := filepath.Dir(img)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return dirs
}

// Run executes one bounded task through the operator's CLI.
func (c *cliAgent) Run(ctx context.Context, req Request, evidenceDir string) (*Result, error) {
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
		req.MaxScenarios = c.cfg.Agent.MaxScenariosPerTask
	}
	if req.Task == "" {
		req.Task = TaskDiscover
	}
	start := time.Now()
	sys := SystemPromptFor(c.tool)
	budget := c.cfg.Agent.MaxTurns
	if req.MaxToolCalls > 0 {
		budget = req.MaxToolCalls
	}
	timeout := c.cfg.Agent.Timeout.Duration
	if req.TimeoutMinutes > 0 {
		timeout = time.Duration(req.TimeoutMinutes) * time.Minute
	}
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	task := TaskPrompt(req) + fmt.Sprintf("\nmax_turns: %d tool calls.\n", budget)
	if b, err := yaml.Marshal(req); err == nil {
		_ = os.WriteFile(filepath.Join(evidenceDir, FileRequest), []byte(c.redactor.Redact(string(b))), 0o644)
	}
	_ = os.WriteFile(filepath.Join(evidenceDir, FileSystem), []byte(sys), 0o644)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileTask), []byte(c.redactor.Redact(task)), 0o644)

	session := sessionName(req.FeatureID, start)
	env := agentEnv(c.cfg, session, evidenceDir)
	rawPath := filepath.Join(evidenceDir, FileCLIRaw)
	transcriptPath := filepath.Join(evidenceDir, FileTranscript)
	lastMessage := filepath.Join(evidenceDir, "agent-cli-last.txt")

	attempts := c.cfg.Agent.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var (
		res     *Result
		tr      *Transcript
		final   string
		lastErr string
		spawns  int
		tried   []ModelAttempt
	)
	name := c.kind
	if m := strings.TrimSpace(c.cfg.Agent.CLIModel); m != "" {
		name += "/" + m
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			wait := c.cfg.Agent.Backoff.Duration * time.Duration(attempt-1)
			c.logger.Printf("agent: %s failed (%s); retry %d/%d in %s", name, firstLine(lastErr), attempt, attempts, wait)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		argv, err := c.argv(sys, evidenceDir, session, req, lastMessage)
		if err != nil {
			return nil, err
		}
		if b, err := json.MarshalIndent(argv, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(evidenceDir, FileArgv), b, 0o644)
		}
		spawns++
		stderr, timedOut, runErr := c.spawn(ctx, argv, env, evidenceDir, task, rawPath, timeout)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var errText string
		tr, final, errText = c.bridge(rawPath, transcriptPath, lastMessage)
		if errText == "" {
			errText = strings.TrimSpace(stderr)
		}
		if runErr != nil && errText == "" {
			errText = runErr.Error()
		}
		lastErr = errText
		if _, ok := ExtractResultBlock(final); ok {
			parsed, perr := ParseResult(final)
			if perr == nil {
				res = parsed
				tried = append(tried, ModelAttempt{name, OutcomeOK})
				break
			}
			lastErr = perr.Error()
		}
		if timedOut {
			res = &Result{Decision: DecisionNeedsReview, Reason: fmt.Sprintf("agent timed out after %s", timeout)}
			tried = append(tried, ModelAttempt{name, OutcomeTimeout})
			break
		}
		kind, reason := providerErrorReason(errText)
		if kind == "transient" && attempt < attempts {
			tried = append(tried, ModelAttempt{name, OutcomeError})
			lastErr = reason
			continue
		}
		if kind != "" {
			// The operator's own subscription or key is exhausted or rejected:
			// report it and let deterministic QA continue (rule 12).
			res = &Result{ModelUnavailable: true, Reason: fmt.Sprintf("%s unavailable: %s", name, firstLine(reason))}
			tried = append(tried, ModelAttempt{name, OutcomeUnavailable})
			break
		}
		res = &Result{Decision: DecisionNeedsReview, Reason: "agent produced no result block: " + firstLine(errText)}
		tried = append(tried, ModelAttempt{name, OutcomeNoResult})
		break
	}
	if res == nil {
		res = &Result{ModelUnavailable: true, Reason: "agent unavailable after retries: " + firstLine(lastErr)}
	}
	if final != "" {
		_ = os.WriteFile(filepath.Join(evidenceDir, FileFinal), []byte(c.redactor.Redact(final)), 0o644)
	}
	res.Attempts = spawns
	res.ModelAttempts = tried
	if !res.ModelUnavailable && spawns > 0 {
		res.Model = name
	}
	res.Duration = time.Since(start)
	res.RawTranscriptPath = transcriptPath
	res.Sandbox = "cli:" + c.kind
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
	writeResultFiles(evidenceDir, c.redactor, res)
	closeBrowserSession(session)
	return res, nil
}

// spawn runs the CLI once with the task on stdin, streaming stdout to rawPath.
func (c *cliAgent) spawn(ctx context.Context, argv, env []string, dir, prompt, rawPath string, timeout time.Duration) (stderr string, timedOut bool, err error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	out, err := os.Create(rawPath)
	if err != nil {
		return "", false, err
	}
	defer out.Close()
	cmd.Stdout = out
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err = cmd.Run()
	timedOut = rctx.Err() == context.DeadlineExceeded
	return errBuf.String(), timedOut, err
}

// bridge converts the CLI's own event stream into the JSONL transcript the rest
// of vigil reads (dashboard, host-violation check, reparse) and returns the
// final assistant text plus any error the CLI reported.
func (c *cliAgent) bridge(rawPath, transcriptPath, lastMessage string) (*Transcript, string, string) {
	in, err := os.Open(rawPath)
	if err != nil {
		return nil, "", err.Error()
	}
	defer in.Close()
	out, err := os.Create(transcriptPath)
	if err != nil {
		return nil, "", err.Error()
	}
	defer out.Close()

	tr := &Transcript{}
	seen := map[string]bool{}
	var all []string
	var final, errText string
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintln(out, string(b))
		tr.ObserveLine(string(b), seen, &all)
	}
	assistant := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		write(map[string]any{"type": "message_end", "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	}
	tool := func(name string, args any) {
		write(map[string]any{"type": "tool_execution_start", "toolName": name, "args": args})
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "assistant": // claude
			msg, _ := ev["message"].(map[string]any)
			parts, _ := msg["content"].([]any)
			for _, p := range parts {
				part, _ := p.(map[string]any)
				switch part["type"] {
				case "text":
					if t, _ := part["text"].(string); t != "" {
						assistant(t)
					}
				case "tool_use":
					n, _ := part["name"].(string)
					tool(n, part["input"])
				}
			}
		case "result": // claude final
			if t, _ := ev["result"].(string); t != "" {
				final = t
			}
			if e, _ := ev["is_error"].(bool); e && errText == "" {
				if s, _ := ev["subtype"].(string); s != "" {
					errText = s
				} else {
					errText = "claude reported an error"
				}
			}
		case "item.completed", "item.started": // codex
			item, _ := ev["item"].(map[string]any)
			switch item["type"] {
			case "agent_message":
				if t, _ := item["text"].(string); t != "" {
					assistant(t)
					final = t
				}
			case "error":
				if m, _ := item["message"].(string); m != "" && errText == "" {
					errText = m
				}
			case "mcp_tool_call", "command_execution", "tool_call":
				if ev["type"] == "item.started" {
					n, _ := item["tool"].(string)
					if n == "" {
						n, _ = item["type"].(string)
					}
					tool(n, item)
				}
			}
		case "turn.failed", "error": // codex
			if b, err := json.Marshal(ev); err == nil && errText == "" {
				errText = string(b)
			}
		}
	}
	if b, err := os.ReadFile(lastMessage); err == nil && strings.TrimSpace(string(b)) != "" {
		final = string(b) // codex writes the final message verbatim
	}
	if final == "" {
		final = tr.AllAssistantText
	}
	return tr, final, errText
}

// Plan runs one tool-less supervise task through the CLI.
func (c *cliAgent) Plan(ctx context.Context, briefing, evidenceDir string) (*Plan, error) {
	if evidenceDir == "" {
		return nil, errors.New("agent: evidence dir required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, err
	}
	sup := c.cfg.Supervisor
	sys := SupervisorSystemPrompt()
	task := SupervisorTaskPrompt(briefing, sup.MaxActions)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileSystem), []byte(sys), 0o644)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileTask), []byte(c.redactor.Redact(task)), 0o644)
	timeout := sup.Timeout.Duration
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	start := time.Now()
	rawPath := filepath.Join(evidenceDir, FileCLIRaw)
	transcriptPath := filepath.Join(evidenceDir, FileTranscript)
	lastMessage := filepath.Join(evidenceDir, "agent-cli-last.txt")
	out := &Plan{RawTranscriptPath: transcriptPath}

	var argv []string
	model := strings.TrimSpace(c.cfg.Agent.CLIModel)
	if c.kind == ProviderClaude {
		argv = []string{c.bin, "-p", "--output-format", "stream-json", "--verbose",
			"--append-system-prompt", sys, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--allowedTools", "", "--permission-prompts", "none", "--restricted"}
		if model != "" {
			argv = append(argv, "--model", model)
		}
	} else {
		argv = []string{c.bin, "exec", "--json", "--skip-git-repo-check", "--ignore-user-config",
			"-s", "read-only", "-C", evidenceDir, "-o", lastMessage}
		if model != "" {
			argv = append(argv, "-m", model)
		}
		argv = append(argv, "-")
		task = sys + "\n\n" + task // codex has no separate system prompt
	}
	env := agentEnv(c.cfg, "", "")
	stderr, timedOut, runErr := c.spawn(ctx, argv, env, evidenceDir, task, rawPath, timeout)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	_, final, errText := c.bridge(rawPath, transcriptPath, lastMessage)
	out.Duration = time.Since(start)
	out.Model = c.kind
	if _, ok := ExtractPlanBlock(final); ok {
		plan, err := ParsePlan(final)
		if err == nil {
			plan.RawTranscriptPath, plan.Duration, plan.Model = transcriptPath, out.Duration, out.Model
			return plan, nil
		}
		return out, fmt.Errorf("agent: supervisor plan unusable: %w", err)
	}
	if errText == "" {
		errText = strings.TrimSpace(stderr)
	}
	if runErr != nil && errText == "" {
		errText = runErr.Error()
	}
	if timedOut {
		return out, fmt.Errorf("agent: supervisor timed out after %s", timeout)
	}
	if kind, reason := providerErrorReason(errText); kind != "" {
		out.ModelUnavailable = true
		return out, fmt.Errorf("agent: supervisor: %s unavailable: %s", c.kind, reason)
	}
	return out, fmt.Errorf("agent: supervisor returned no plan block: %s", firstLine(errText))
}

// Reparse rebuilds the result of a finished CLI run from its final answer.
func (c *cliAgent) Reparse(dir string) (*Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileFinal))
	if err != nil {
		f, ferr := os.Open(filepath.Join(dir, FileTranscript))
		if ferr != nil {
			return nil, err
		}
		defer f.Close()
		tr, terr := ParseTranscript(f)
		if terr != nil {
			return nil, terr
		}
		b = []byte(tr.AllAssistantText)
	}
	res, err := ParseResult(string(b))
	if err != nil {
		return nil, err
	}
	res.RawTranscriptPath = filepath.Join(dir, FileTranscript)
	writeResultFiles(dir, c.redactor, res)
	return res, nil
}

// Doctor checks the CLI, its authentication and the browser tool chain without
// spending a task.
func (c *cliAgent) Doctor(ctx context.Context) error {
	var problems []string
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(cctx, c.bin, "--version").CombinedOutput(); err != nil {
		problems = append(problems, fmt.Sprintf("%s --version failed: %v: %s", c.kind, err, firstLine(string(out))))
	}
	if _, err := exec.LookPath("agent-browser"); err != nil {
		problems = append(problems, "agent-browser CLI missing: npm i -g agent-browser && agent-browser install")
	}
	if !fileExists(c.self) {
		problems = append(problems, "vigil binary not found at "+c.self+": the browser tool is served by `vigil mcp-browser`")
	}
	if len(problems) > 0 {
		return fmt.Errorf("agent doctor:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}
