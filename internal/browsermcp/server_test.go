package browsermcp

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func exchange(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("unparsable response %q: %v", sc.Text(), err)
		}
		got = append(got, v)
	}
	return got
}

func TestServeAnswersHandshakeAndListsOneTool(t *testing.T) {
	got := exchange(t, &Server{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(got) != 2 {
		t.Fatalf("responses = %d (a notification must not be answered): %+v", len(got), got)
	}
	init := got[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocol = %v", init["protocolVersion"])
	}
	tools := got[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != ToolName {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestToolCallRefusesForbiddenArguments(t *testing.T) {
	got := exchange(t, &Server{},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_browser","arguments":{"args":["eval","1+1"]}}}`)
	result := got[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("forbidden argument accepted: %+v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not allowed") {
		t.Fatalf("text = %q", text)
	}
}

func TestToolCallReportsFailureAsToolError(t *testing.T) {
	// A missing binary must come back as an error result, not a protocol error:
	// the agent has to see what happened and recover.
	s := &Server{CLI: "vigil-agent-browser-that-does-not-exist", Timeout: 5 * time.Second}
	got := exchange(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agent_browser","arguments":{"args":["open","https://example.com"]}}}`)
	result := got[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("missing binary reported as success: %+v", result)
	}
}

func TestUnknownMethodAndToolAreRejected(t *testing.T) {
	got := exchange(t, &Server{},
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	if got[0]["error"] == nil || got[1]["error"] == nil {
		t.Fatalf("unknown method/tool accepted: %+v", got)
	}
}

func TestTruncateBoundsOutput(t *testing.T) {
	s := &Server{MaxOutput: 10}
	out := s.truncate(strings.Repeat("x", 50))
	if !strings.HasPrefix(out, strings.Repeat("x", 10)) || !strings.Contains(out, "truncated 40") {
		t.Fatalf("truncate = %q", out)
	}
}

func TestSanitizeRoleKeepsSessionsSeparate(t *testing.T) {
	if got := sanitizeRole("Teacher 1!"); got != "teacher1" {
		t.Fatalf("role = %q", got)
	}
}
