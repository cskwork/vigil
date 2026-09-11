package ui

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---- Browser Agent activity ---------------------------------------------------

type agentView struct {
	Dir        string     `json:"dir"` // evidence-relative
	Feature    string     `json:"feature"`
	StartedAt  string     `json:"started_at"`
	UpdatedAt  string     `json:"updated_at"`
	Status     string     `json:"status"` // running | done | stale
	Decision   string     `json:"decision,omitempty"`
	Candidates int        `json:"candidates"`
	Sandbox    string     `json:"sandbox,omitempty"`
	Attempts   int        `json:"attempts"`
	ToolCalls  []toolCall `json:"tool_calls"`
	LastText   string     `json:"last_text"`
	Retries    []string   `json:"retries,omitempty"`
}

type toolCall struct {
	N       int    `json:"n"`
	Tool    string `json:"tool"`
	Args    string `json:"args"`
	OK      *bool  `json:"ok,omitempty"`
	Preview string `json:"preview,omitempty"`
}

// agentScanEvery bounds how often the whole evidence/agent tree is walked.
// The overview is polled every 3s by every open tab; one walk per interval
// is plenty for a view of "what is the agent doing right now".
const agentScanEvery = 2 * time.Second

// agentStaleAfter is how long a transcript may go untouched before the agent
// is presumed dead rather than working.
const agentStaleAfter = 20 * time.Minute

func (s *Server) agentLatest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.latestAgent())
}

func (s *Server) latestAgent() *agentView {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if now := s.now(); !s.agentAt.IsZero() && now.Sub(s.agentAt) < agentScanEvery {
		return s.agentView
	}
	s.agentView = s.scanLatestAgent()
	s.agentAt = s.now()
	return s.agentView
}

// newestTranscript walks evidence/agent/<feature>/<run>/ under root and returns
// the run directory whose transcript was touched most recently.
func newestTranscript(root string) (dir string, at time.Time) {
	feats, _ := os.ReadDir(root)
	for _, f := range feats {
		if !f.IsDir() {
			continue
		}
		if p, t := newestTranscriptIn(filepath.Join(root, f.Name())); t.After(at) {
			dir, at = p, t
		}
	}
	return dir, at
}

// newestTranscriptIn finds the freshest run directory under one feature.
func newestTranscriptIn(featureDir string) (dir string, at time.Time) {
	runs, _ := os.ReadDir(featureDir)
	for _, ru := range runs {
		if !ru.IsDir() {
			continue
		}
		p := filepath.Join(featureDir, ru.Name())
		st, err := os.Stat(filepath.Join(p, "agent-transcript.jsonl"))
		if err != nil {
			continue
		}
		if st.ModTime().After(at) {
			at, dir = st.ModTime(), p
		}
	}
	return dir, at
}

func (s *Server) scanLatestAgent() *agentView {
	newest, newestT := newestTranscript(filepath.Join(s.evRoot, "agent"))
	if newest == "" {
		return nil
	}
	av := &agentView{Dir: s.rel(newest), Feature: filepath.Base(filepath.Dir(newest)), UpdatedAt: fmtT(&newestT), ToolCalls: []toolCall{}}
	if st, err := os.Stat(filepath.Join(newest, "agent-request.yaml")); err == nil {
		t := st.ModTime()
		av.StartedAt = fmtT(&t)
	}
	if b, err := os.ReadFile(filepath.Join(newest, "agent-result.json")); err == nil {
		var res struct {
			Decision   string   `json:"decision"`
			Candidates []string `json:"script_candidates"`
			Sandbox    string   `json:"sandbox"`
			Attempts   int      `json:"attempts"`
		}
		_ = json.Unmarshal(b, &res)
		av.Status, av.Decision, av.Candidates, av.Sandbox, av.Attempts = "done", res.Decision, len(res.Candidates), res.Sandbox, res.Attempts
	} else if s.now().Sub(newestT) > agentStaleAfter {
		av.Status = "stale"
	} else {
		av.Status = "running"
	}
	tr := s.transcriptOf(filepath.Join(newest, "agent-transcript.jsonl"))
	av.ToolCalls, av.LastText, av.Retries = tr.calls, tr.lastText, tr.retries
	return av
}

// transcriptEntry caches the parsed shape of one agent transcript. Transcripts
// only grow, so (size, mtime) identifies the content exactly.
type transcriptEntry struct {
	size     int64
	mtime    int64
	calls    []toolCall
	lastText string
	retries  []string
}

func (s *Server) transcriptOf(path string) transcriptEntry {
	st, err := os.Stat(path)
	if err != nil {
		return transcriptEntry{calls: []toolCall{}}
	}
	if v, ok := s.transcript.Load(path); ok {
		e := v.(transcriptEntry)
		if e.size == st.Size() && e.mtime == st.ModTime().UnixNano() {
			return e
		}
	}
	e := parseTranscript(path)
	e.size, e.mtime = st.Size(), st.ModTime().UnixNano()
	s.transcript.Store(path, e)
	return e
}

func parseTranscript(path string) transcriptEntry {
	out := transcriptEntry{calls: []toolCall{}}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	byID := map[string]int{}
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ToolCall string `json:"toolCallId"`
			ToolName string `json:"toolName"`
			Args     any    `json:"args"`
			Result   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Details struct {
					ExitCode *int `json:"exitCode"`
				} `json:"details"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Message struct {
				Role         string `json:"role"`
				ErrorMessage string `json:"errorMessage"`
				Content      []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool_execution_start":
			args, _ := json.Marshal(ev.Args)
			out.calls = append(out.calls, toolCall{N: len(out.calls) + 1, Tool: ev.ToolName, Args: clip(string(args), 200)})
			byID[ev.ToolCall] = len(out.calls) - 1
		case "tool_execution_end":
			if i, ok := byID[ev.ToolCall]; ok {
				var text string
				for _, c := range ev.Result.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
				okv := !ev.Result.IsError && (ev.Result.Details.ExitCode == nil || *ev.Result.Details.ExitCode == 0)
				out.calls[i].OK = &okv
				out.calls[i].Preview = clip(strings.TrimSpace(text), 240)
			}
		case "message_end":
			if ev.Message.Role == "assistant" {
				if ev.Message.ErrorMessage != "" {
					out.retries = append(out.retries, clip(ev.Message.ErrorMessage, 160))
				}
				var text string
				for _, c := range ev.Message.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
				if t := strings.TrimSpace(text); t != "" {
					if i := strings.Index(t, "```"); i > 0 {
						t = t[:i]
					}
					out.lastText = clip(strings.TrimSpace(t), 600)
				}
			}
		}
	}
	if len(out.calls) > 150 {
		out.calls = out.calls[len(out.calls)-150:]
	}
	return out
}
