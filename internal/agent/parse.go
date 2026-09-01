package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Transcript is the compact view of a pi `--mode json` JSONL stream.
type Transcript struct {
	// AssistantText is the text of the last assistant message (where the result block lives).
	AssistantText string
	// AllAssistantText joins every assistant text chunk, used as a fallback when the
	// last message has no result block.
	AllAssistantText string
	StopReason       string
	ErrorMessage     string
	ToolCalls        int
	// ToolURLs are http(s) URLs the agent passed to tools (navigation targets).
	ToolURLs []string
	// Lines is the number of JSONL lines read.
	Lines int
}

type jsonlEvent struct {
	Type     string `json:"type"`
	ToolName string `json:"toolName"`
	Args     any    `json:"args"`
	IsError  bool   `json:"isError"`
	Message  *struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

// ParseTranscript reads JSONL events. Non-JSON lines are ignored.
func ParseTranscript(r io.Reader) (*Transcript, error) {
	t := &Transcript{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	seen := map[string]bool{}
	var all []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		t.Lines++
		var ev jsonlEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		t.observe(ev, seen, &all)
	}
	if err := sc.Err(); err != nil {
		return t, err
	}
	t.AllAssistantText = strings.Join(all, "\n")
	return t, nil
}

// ObserveLine folds one JSONL line into the transcript (streaming variant of ParseTranscript).
func (t *Transcript) ObserveLine(line string, seen map[string]bool, all *[]string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return
	}
	t.Lines++
	var ev jsonlEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	t.observe(ev, seen, all)
}

func (t *Transcript) observe(ev jsonlEvent, seen map[string]bool, all *[]string) {
	switch ev.Type {
	case "tool_execution_start":
		t.ToolCalls++
		for _, u := range urlsIn(ev.Args) {
			if !seen[u] {
				seen[u] = true
				t.ToolURLs = append(t.ToolURLs, u)
			}
		}
	case "message_end":
		if ev.Message == nil || ev.Message.Role != "assistant" {
			return
		}
		var parts []string
		for _, c := range ev.Message.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text := strings.Join(parts, "\n")
		if text != "" {
			t.AssistantText = text
			*all = append(*all, text)
			t.AllAssistantText = strings.Join(*all, "\n")
		}
		if ev.Message.StopReason != "" {
			t.StopReason = ev.Message.StopReason
		}
		if ev.Message.ErrorMessage != "" {
			t.ErrorMessage = ev.Message.ErrorMessage
		}
	}
}

var urlRe = regexp.MustCompile(`https?://[^\s"'<>\\\]\[)(]+`)

// urlsIn walks tool args (any JSON shape) and returns http(s) URLs found in strings.
func urlsIn(v any) []string {
	var out []string
	var walk func(x any)
	walk = func(x any) {
		switch y := x.(type) {
		case string:
			out = append(out, urlRe.FindAllString(y, -1)...)
		case []any:
			for _, e := range y {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(y))
			for k := range y {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(y[k])
			}
		}
	}
	walk(v)
	return out
}

// ---- result block ---------------------------------------------------------

var fenceRe = regexp.MustCompile("(?s)```[ \\t]*([^\\n`]*)\\n(.*?)\\n[ \\t]*```")

// ExtractResultBlock returns the body of the last fenced block that looks like a
// result: preferred `yaml vigil-result`, then plain ```yaml / ```json / bare fence
// whose body contains a `decision:` key.
func ExtractResultBlock(text string) (string, bool) {
	matches := fenceRe.FindAllStringSubmatch(text, -1)
	var named, typed []string
	for _, m := range matches {
		info := strings.ToLower(strings.TrimSpace(m[1]))
		body := m[2]
		switch {
		case strings.Contains(info, ResultFence):
			named = append(named, body)
		case (info == "yaml" || info == "yml" || info == "json" || info == "") && decisionRe.MatchString(body):
			typed = append(typed, body)
		}
	}
	if len(named) > 0 {
		return named[len(named)-1], true
	}
	if len(typed) > 0 {
		return typed[len(typed)-1], true
	}
	return "", false
}

var decisionRe = regexp.MustCompile(`(?m)^\s*\{?\s*"?decision"?\s*:`)

// ParseResult extracts and decodes the result block from assistant text.
func ParseResult(text string) (*Result, error) {
	body, ok := ExtractResultBlock(text)
	if !ok {
		return nil, fmt.Errorf("agent: no %s block in output", ResultFence)
	}
	body = normalizeResultBody(body)
	var r Result
	if err := yaml.Unmarshal([]byte(body), &r); err != nil {
		// Salvage: the model answered in an almost-YAML shape. Keep the decision and the
		// whole block as evidence rather than losing a long exploration.
		if sr := salvageResult(body); sr != nil {
			sr.Reason = "result block was not valid YAML; kept as text (" + firstLine(err.Error()) + ")"
			return sr, nil
		}
		return nil, fmt.Errorf("agent: result block is not valid YAML/JSON: %w", err)
	}
	if extra := extraResultFields(body); extra != "" {
		r.Evidence = strings.TrimSpace(r.Evidence + "\n\n" + extra)
	}
	r.Decision = strings.ToUpper(strings.TrimSpace(r.Decision))
	switch r.Decision {
	case DecisionNewScript, DecisionPatchScript, DecisionAppFailure, DecisionNoNewCoverage, DecisionNeedsReview, DecisionOracleUnknown:
	case "":
		return nil, fmt.Errorf("agent: result block has no decision")
	default:
		return nil, fmt.Errorf("agent: unknown decision %q", r.Decision)
	}
	// Trim candidate documents; drop empties.
	var cands []string
	for _, c := range r.ScriptCandidates {
		if c = strings.TrimSpace(c); c != "" {
			cands = append(cands, c)
		}
	}
	r.ScriptCandidates = cands
	r.ScriptPatch = strings.TrimSpace(r.ScriptPatch)
	return &r, nil
}

// ---- allowed hosts ---------------------------------------------------------

// HostViolations returns the URLs (deduplicated, in order) whose host is not in
// allowed (exact or subdomain match). Non-http(s) URLs are ignored.
func HostViolations(urls []string, allowed []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		if hostAllowed(u.Hostname(), allowed) {
			continue
		}
		if !seen[raw] {
			seen[raw] = true
			out = append(out, raw)
		}
	}
	return out
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// resultKnownKeys are the contract fields; anything else the model adds is kept as evidence text.
var resultKnownKeys = map[string]bool{"decision": true, "evidence": true, "coverage_delta": true, "oracle_provenance": true, "script_candidates": true, "script_patch": true, "observed": true, "visited_urls": true, "ephemeral": true}

// normalizeResultBody makes the model's block match the contract when it is close:
// a single root key "vigil-result:" is unwrapped, and script_candidates given as
// YAML objects (instead of DSL documents as strings) are re-serialized to strings so
// the gate can still parse/validate them.
func normalizeResultBody(body string) string {
	var root map[string]any
	if err := yaml.Unmarshal([]byte(body), &root); err != nil || root == nil {
		return body
	}
	if inner, ok := root["vigil-result"].(map[string]any); ok && len(root) == 1 {
		root = inner
	}
	if cands, ok := root["script_candidates"].([]any); ok {
		var out []string
		for _, c := range cands {
			switch v := c.(type) {
			case string:
				out = append(out, v)
			default:
				if b, err := yaml.Marshal(v); err == nil {
					out = append(out, string(b))
				}
			}
		}
		root["script_candidates"] = out
	}
	if obs, ok := root["observed"]; ok {
		if m, ok := obs.(map[string]any); ok {
			flat := map[string]string{}
			for k, v := range m {
				if s, ok := v.(string); ok {
					flat[k] = s
				} else if b, err := yaml.Marshal(v); err == nil {
					flat[k] = strings.TrimSpace(string(b))
				}
			}
			root["observed"] = flat
		} else {
			delete(root, "observed")
		}
	}
	if vu, ok := root["visited_urls"].([]any); ok {
		var urls []string
		for _, u := range vu {
			if s, ok := u.(string); ok {
				urls = append(urls, strings.Fields(s)[0])
			}
		}
		root["visited_urls"] = urls
	}
	if e, ok := root["evidence"]; ok {
		if _, isStr := e.(string); !isStr {
			if b, err := yaml.Marshal(e); err == nil {
				root["evidence"] = string(b)
			}
		}
	}
	b, err := yaml.Marshal(root)
	if err != nil {
		return body
	}
	return string(b)
}

// extraResultFields renders unknown top-level fields (accounts_used, blocked_at, ...) as text
// so a rich but non-contract answer is preserved as evidence instead of being dropped.
func extraResultFields(body string) string {
	var root map[string]any
	if err := yaml.Unmarshal([]byte(body), &root); err != nil || root == nil {
		return ""
	}
	extra := map[string]any{}
	for k, v := range root {
		if !resultKnownKeys[k] {
			extra[k] = v
		}
	}
	if len(extra) == 0 {
		return ""
	}
	b, err := yaml.Marshal(extra)
	if err != nil {
		return ""
	}
	return "Additional fields reported by the agent:\n" + string(b)
}

var (
	salvageDecisionRe = regexp.MustCompile(`(?m)^\s*decision:\s*([A-Za-z_]+)`)
	salvageURLRe      = regexp.MustCompile(`https?://[^\s"'<>)\]]+`)
)

// salvageResult extracts decision + URLs from an unparseable block and keeps the text.
func salvageResult(body string) *Result {
	m := salvageDecisionRe.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	d := strings.ToUpper(m[1])
	switch d {
	case DecisionNewScript, DecisionPatchScript, DecisionAppFailure, DecisionNoNewCoverage, DecisionNeedsReview, DecisionOracleUnknown:
	default:
		return nil
	}
	r := &Result{Decision: d, Evidence: strings.TrimSpace(body)}
	seen := map[string]bool{}
	for _, u := range salvageURLRe.FindAllString(body, -1) {
		u = strings.TrimRight(u, ".,;")
		if !seen[u] {
			seen[u] = true
			r.VisitedURLs = append(r.VisitedURLs, u)
		}
	}
	if d == DecisionNewScript {
		// prose candidates cannot be gated; downgrade so nothing is promoted without a script
		r.Decision = DecisionNeedsReview
	}
	return r
}
