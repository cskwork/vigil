// Package browsermcp exposes vigil's single `agent_browser` tool over MCP
// stdio. A coding-agent CLI the operator already pays for (Claude Code, Codex)
// can then drive the same browser, under the same tool contract and the same
// argument restrictions as vigil's own pi extension (piext/vigil-browser.js).
//
// stdout carries JSON-RPC messages only; diagnostics go to stderr.
package browsermcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ToolName is the tool this server exposes. MCP clients prefix it with the
// server name they were configured with (claude: mcp__vigil__agent_browser).
const ToolName = "agent_browser"

// Defaults mirror the pi extension so both paths behave the same.
const (
	DefaultMaxOutput = 24000
	DefaultTimeout   = 45 * time.Second
	// ProtocolVersion is answered when a client does not name one.
	ProtocolVersion = "2025-06-18"
)

// forbidden arguments: anything that would leave the bounded QA surface
// (installing, evaluating code, attaching to the operator's own browser).
var forbidden = map[string]bool{
	"install": true, "eval": true, "exec": true, "shell": true, "electron": true,
	"record": true, "connect": true, "--cdp": true, "--auto-connect": true,
	"--headed": true, "--profile": true,
}

// Server runs the stdio loop. Zero values fall back to the defaults above.
type Server struct {
	CLI       string // agent-browser binary (default: "agent-browser" on PATH)
	MaxOutput int
	Timeout   time.Duration
	// Session is the base agent-browser session; a tool call's "session"
	// argument is appended to it so roles stay isolated.
	Session string
	Log     io.Writer
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolCall struct {
	Name      string `json:"name"`
	Arguments struct {
		Args    []string `json:"args"`
		Stdin   string   `json:"stdin"`
		Session string   `json:"session"`
	} `json:"arguments"`
}

// Definition is the tool descriptor sent in tools/list. It repeats the argv
// contract the vigil system prompt teaches, so no second prompt is needed.
func Definition() map[string]any {
	return map[string]any{
		"name": ToolName,
		"description": "Drive a headless browser against the deployed QA target via the agent-browser CLI. " +
			"Pass raw argv in `args` (e.g. [\"open\",\"https://…\"], [\"snapshot\",\"-i\"], [\"click\",\"@e3\"], " +
			"[\"get\",\"url\"], [\"get\",\"text\",\"body\"], [\"console\"], [\"errors\"], [\"network\",\"requests\"], " +
			"[\"tab\",\"list\"], [\"screenshot\",\"name.png\"], [\"close\"]). Refs (@eN) come from the latest snapshot.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "agent-browser argv, e.g. [\"snapshot\",\"-i\"]"},
				"stdin":   map[string]any{"type": "string", "description": "stdin for `batch` (one command per line)"},
				"session": map[string]any{"type": "string", "description": "optional role name (teacher, student1 ...) for a separate isolated browser session"},
			},
			"required": []string{"args"},
		},
	}
}

func (s *Server) logf(format string, a ...any) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, "[vigil-mcp] "+format+"\n", a...)
}

// Serve reads newline-delimited JSON-RPC from in and writes responses to out
// until in is exhausted or ctx is done.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.logf("dropping unparsable message: %v", err)
			continue
		}
		// Notifications carry no id and must never be answered.
		if len(req.ID) == 0 {
			continue
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		result, rerr := s.dispatch(ctx, req)
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := p.ProtocolVersion
		if version == "" {
			version = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vigil-browser", "version": "1"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{Definition()}}, nil
	case "tools/call":
		var call toolCall
		if err := json.Unmarshal(req.Params, &call); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool arguments: " + err.Error()}
		}
		if call.Name != ToolName {
			return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
		}
		if len(call.Arguments.Args) == 0 {
			return toolResult("args is required, e.g. {\"args\":[\"open\",\"https://example.com\"]}", true), nil
		}
		text, isErr := s.run(ctx, call.Arguments.Args, call.Arguments.Stdin, call.Arguments.Session)
		return toolResult(text, isErr), nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"isError":           isError,
		"structuredContent": map[string]any{"output": text, "is_error": isError},
	}
}

// run executes one agent-browser command with the same guards as the pi
// extension: no forbidden argument, a hard timeout, bounded output.
func (s *Server) run(ctx context.Context, args []string, stdin, session string) (string, bool) {
	for _, a := range args {
		if forbidden[a] {
			return fmt.Sprintf("argument %q is not allowed for the QA agent", a), true
		}
	}
	bin := s.CLI
	if bin == "" {
		bin = "agent-browser"
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = os.Environ()
	if base := s.Session; base != "" {
		name := base
		if role := sanitizeRole(session); role != "" {
			name = base + "-" + role
		}
		cmd.Env = append(cmd.Env, "AGENT_BROWSER_SESSION="+name)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	text := s.truncate(string(out))
	if cctx.Err() == context.DeadlineExceeded {
		return text + fmt.Sprintf("\nagent-browser %s timed out after %s. The page may be frozen: run {\"args\":[\"close\"]} for this session, then open the URL again in a new session name.", args[0], timeout), true
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, true
	}
	if text == "" {
		text = "(no output)"
	}
	return text, false
}

func sanitizeRole(s string) string {
	role := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return -1
	}, s)
	if len(role) > 24 {
		role = role[:24]
	}
	return role
}

func (s *Server) truncate(text string) string {
	max := s.MaxOutput
	if max <= 0 {
		max = DefaultMaxOutput
	}
	if len(text) <= max {
		return text
	}
	return text[:max] + fmt.Sprintf("\n…[truncated %d chars]", len(text)-max)
}
