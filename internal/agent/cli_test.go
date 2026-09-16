package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vigil/internal/attach"
	"vigil/internal/config"
)

func testCLI(kind string) *cliAgent {
	cfg := &config.Config{}
	cfg.Agent.Provider = kind
	cfg.Agent.MaxTurns = 30
	return &cliAgent{cfg: cfg, kind: kind, bin: "/usr/bin/" + kind, self: "/opt/vigil/vigil",
		tool: toolNameFor(kind), redactor: RedactorFor(nil, nil)}
}

func TestClaudeArgvBindsBrowserToolAndAttachments(t *testing.T) {
	c := testCLI(ProviderClaude)
	c.cfg.Agent.CLIModel = "opus"
	req := Request{Attachments: []attach.Attachment{{Name: "s.png", Kind: attach.KindImage, Path: "/tmp/up/s.png"}}}
	argv, err := c.argv("SYS", "/ev", "vigil-x-1", req, "/ev/last.txt")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(argv, " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--append-system-prompt SYS",
		"--strict-mcp-config", "--allowedTools mcp__vigil__agent_browser,Read", "--permission-prompts none",
		"--restricted", "--model opus", "--add-dir /tmp/up"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv missing %q:\n%s", want, line)
		}
	}
	// The MCP server must be vigil itself, so the tool contract stays identical.
	i := indexOf(argv, "--mcp-config")
	if i < 0 {
		t.Fatal("no --mcp-config")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(argv[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	srv, ok := cfg.MCPServers[MCPServerName]
	if !ok || srv.Command != "/opt/vigil/vigil" || len(srv.Args) != 1 || srv.Args[0] != "mcp-browser" {
		t.Fatalf("mcp config = %+v", cfg)
	}
	if srv.Env["AGENT_BROWSER_SESSION"] != "vigil-x-1" || srv.Env["AGENT_BROWSER_SCREENSHOT_DIR"] != "/ev" {
		t.Fatalf("mcp env = %+v", srv.Env)
	}
}

func TestCodexArgvIsReadOnlyAndPassesImages(t *testing.T) {
	c := testCLI(ProviderCodex)
	req := Request{Attachments: []attach.Attachment{
		{Name: "clip.mp4", Kind: attach.KindVideo, Path: "/tmp/up/clip.mp4", Frames: []string{"/tmp/up/f1.jpg", "/tmp/up/f2.jpg"}},
	}}
	argv, err := c.argv("SYS", "/ev", "vigil-x-1", req, "/ev/last.txt")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(argv, " ")
	for _, want := range []string{"exec --json", "-s read-only", "-C /ev", "-o /ev/last.txt",
		`approval_policy="never"`, "mcp_servers.vigil.command=",
		`mcp_servers.vigil.default_tools_approval_mode="approve"`,
		"-i /tmp/up/f1.jpg", "-i /tmp/up/f2.jpg"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv missing %q:\n%s", want, line)
		}
	}
	// The container itself is never handed to the model; only its frames are.
	if strings.Contains(line, "clip.mp4") {
		t.Fatalf("video container passed to the model:\n%s", line)
	}
	if argv[len(argv)-1] != "-" {
		t.Fatalf("prompt is not read from stdin: %v", argv[len(argv)-1])
	}
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

func TestBridgeReadsClaudeStream(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, FileCLIRaw)
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Read"]}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__vigil__agent_browser","input":{"args":["open","https://shop.example.com/cart"]}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		"{\"type\":\"result\",\"is_error\":false,\"result\":\"```yaml vigil-result\\ndecision: NO_NEW_COVERAGE\\nevidence: nothing changed\\ncoverage_delta: none\\noracle_provenance: observation\\n```\"}",
	}, "\n")
	if err := os.WriteFile(raw, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	c := testCLI(ProviderClaude)
	tr, final, errText := c.bridge(raw, filepath.Join(dir, FileTranscript), filepath.Join(dir, "missing.txt"))
	if errText != "" {
		t.Fatalf("errText = %q", errText)
	}
	if tr.ToolCalls != 1 {
		t.Fatalf("tool calls = %d", tr.ToolCalls)
	}
	if len(tr.ToolURLs) != 1 || tr.ToolURLs[0] != "https://shop.example.com/cart" {
		t.Fatalf("tool urls = %v", tr.ToolURLs)
	}
	res, err := ParseResult(final)
	if err != nil || res.Decision != DecisionNoNewCoverage {
		t.Fatalf("result = %+v, %v", res, err)
	}
	// The bridged transcript must be readable by the shared parser (dashboard, reparse).
	f, err := os.Open(filepath.Join(dir, FileTranscript))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	again, err := ParseTranscript(f)
	if err != nil || again.ToolCalls != 1 || !strings.Contains(again.AllAssistantText, "done") {
		t.Fatalf("reparsed transcript = %+v, %v", again, err)
	}
}

func TestBridgePrefersCodexLastMessageFile(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, FileCLIRaw)
	stream := strings.Join([]string{
		`{"type":"item.started","item":{"type":"mcp_tool_call","tool":"agent_browser","arguments":{"args":["open","https://example.com"]}}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}`,
	}, "\n")
	if err := os.WriteFile(raw, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	last := filepath.Join(dir, "last.txt")
	if err := os.WriteFile(last, []byte("FINAL ANSWER"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := testCLI(ProviderCodex)
	tr, final, _ := c.bridge(raw, filepath.Join(dir, FileTranscript), last)
	if final != "FINAL ANSWER" {
		t.Fatalf("final = %q", final)
	}
	if tr.ToolCalls != 1 || len(tr.ToolURLs) != 1 {
		t.Fatalf("transcript = %+v", tr)
	}
}

func TestBridgeSurfacesCodexError(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, FileCLIRaw)
	if err := os.WriteFile(raw, []byte(`{"type":"item.completed","item":{"type":"error","message":"429 rate limit"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := testCLI(ProviderCodex)
	_, _, errText := c.bridge(raw, filepath.Join(dir, FileTranscript), filepath.Join(dir, "none.txt"))
	if !strings.Contains(errText, "429") {
		t.Fatalf("errText = %q", errText)
	}
	if kind, _ := providerErrorReason(errText); kind != "transient" {
		t.Fatalf("429 classified as %q", kind)
	}
}

func TestSystemPromptForRenamesTheBrowserTool(t *testing.T) {
	got := SystemPromptFor(toolNameFor(ProviderClaude))
	if strings.Contains(got, "the agent_browser tool") {
		t.Fatal("tool name not rewritten")
	}
	if !strings.Contains(got, "mcp__vigil__agent_browser") {
		t.Fatal("prefixed tool name missing")
	}
	if SystemPromptFor(BrowserTool) != SystemPrompt() {
		t.Fatal("pi prompt changed")
	}
}
