package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// jiraAdapter (jira) turns the issues of a JQL search into Kind=issue events by
// shelling out to acli: `<cli> jira workitem search --jql <jql> --json --limit N
// --fields key,summary,description,status,labels,issuetype`. acli has no usable
// `updated` field, so an issue is delivered once per content hash
// (status|summary|description); the marker "ingest:jira:<KEY>:<hash>" lives in scheduler_state.
type jiraAdapter struct {
	st  *store.Store
	cfg config.JiraDiscovery
	now func() time.Time
}

const jiraFields = "key,summary,description,status,labels,issuetype"

func newJiraAdapter(cfg *config.Config, st *store.Store) *jiraAdapter {
	j := &jiraAdapter{st: st, cfg: cfg.Discovery.Jira, now: func() time.Time { return time.Now().UTC() }}
	if j.cfg.CLI == "" {
		j.cfg.CLI = "acli"
	}
	if j.cfg.Limit <= 0 {
		j.cfg.Limit = 20
	}
	return j
}

func (j *jiraAdapter) Name() string { return AdapterJira }

func (j *jiraAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	due, err := pollDue(ctx, j.st, j.Name(), j.cfg.PollInterval.Duration, j.now())
	if err != nil || !due {
		return nil, err
	}
	events, err := j.search(ctx, j.cfg.Limit)
	if err != nil {
		return nil, err
	}
	return deliverOnceBy(ctx, j.st, events, func(ev model.FeatureEvent) string {
		return "ingest:jira:" + ev.Ref + ":" + strings.TrimPrefix(ev.ShippedSHA, "jira:"+ev.Ref+":")
	})
}

func (j *jiraAdapter) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	n := limit
	if n <= 0 {
		n = j.cfg.Limit
	}
	events, err := j.search(ctx, n)
	if err != nil {
		return nil, err
	}
	return newestOldestFirst(events, limit), nil
}

// search runs the CLI and normalises every issue into an event.
func (j *jiraAdapter) search(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	args := []string{"jira", "workitem", "search", "--jql", j.cfg.JQL, "--json", "--limit", fmt.Sprint(limit), "--fields", jiraFields}
	cmd := exec.CommandContext(ctx, j.cfg.CLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ingest[jira]: %s %s: %w: %s", j.cfg.CLI, strings.Join(args[:3], " "), err, strings.TrimSpace(stderr.String()))
	}
	issues, err := parseJiraSearch(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ingest[jira]: parse search output: %w", err)
	}
	now := j.now()
	var events []model.FeatureEvent
	for _, is := range issues {
		if is.Key == "" {
			log.Printf("ingest[%s]: skip issue without key", j.Name())
			continue
		}
		ev := j.event(is, now)
		stamp(&ev, j.Name())
		events = append(events, ev)
	}
	return events, nil
}

// jiraIssue is the verified acli shape (2026-09): {key, fields{summary, description
// <ADF|string|null>, status{name}, labels[], issuetype{name}}}.
type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Labels    []string `json:"labels"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
	} `json:"fields"`
}

// parseJiraSearch accepts a top-level array or {"issues":[...]} / {"values":[...]}.
func parseJiraSearch(raw []byte) ([]jiraIssue, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var list []jiraIssue
	if raw[0] == '[' {
		return list, json.Unmarshal(raw, &list)
	}
	var wrapped struct {
		Issues []jiraIssue `json:"issues"`
		Values []jiraIssue `json:"values"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	if len(wrapped.Issues) > 0 {
		return wrapped.Issues, nil
	}
	return wrapped.Values, nil
}

func (j *jiraAdapter) event(is jiraIssue, now time.Time) model.FeatureEvent {
	key := strings.ToUpper(strings.TrimSpace(is.Key))
	desc := strings.TrimSpace(flattenADF(is.Fields.Description))
	hash := shortHash(is.Fields.Status.Name + "|" + is.Fields.Summary + "|" + desc)
	var head []string
	if is.Fields.IssueType.Name != "" {
		head = append(head, "type: "+is.Fields.IssueType.Name)
	}
	if is.Fields.Status.Name != "" {
		head = append(head, "status: "+is.Fields.Status.Name)
	}
	if len(is.Fields.Labels) > 0 {
		head = append(head, "labels: "+strings.Join(is.Fields.Labels, ", "))
	}
	details := strings.Join(head, "\n")
	if desc != "" {
		details = strings.TrimSpace(details + "\n\n" + desc)
	}
	return model.FeatureEvent{
		FeatureID:  key,
		ShippedSHA: "jira:" + key + ":" + hash,
		ShippedAt:  now,
		Routes:     applyRouteHints(j.cfg.RouteHints, is.Fields.Summary+"\n"+desc),
		Summary:    strings.TrimSpace(is.Fields.Summary),
		Kind:       model.FeatureKindIssue,
		Ref:        key,
		Details:    bound(details, MaxDetails),
	}
}

// flattenADF renders a description that is a plain string, null, or an Atlassian
// Document Format tree: every `text` node is concatenated; paragraph, heading and
// list-item boundaries become newlines.
func flattenADF(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return string(raw)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return string(raw)
	}
	var b strings.Builder
	walkADF(node, &b)
	// collapse runs of blank lines left by nested blocks
	lines := strings.Split(b.String(), "\n")
	var out []string
	blank := false
	for _, l := range lines {
		l = strings.TrimRight(l, " ")
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func walkADF(node any, b *strings.Builder) {
	switch v := node.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		if txt, ok := v["text"].(string); ok && (typ == "text" || typ == "") {
			b.WriteString(txt)
		}
		switch typ {
		case "hardBreak":
			b.WriteByte('\n')
		case "mention":
			if attrs, ok := v["attrs"].(map[string]any); ok {
				if t, ok := attrs["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		if content, ok := v["content"].([]any); ok {
			for _, c := range content {
				walkADF(c, b)
			}
		}
		switch typ {
		case "paragraph", "heading", "listItem", "codeBlock", "blockquote", "tableRow", "rule":
			b.WriteByte('\n')
		}
	case []any:
		for _, c := range v {
			walkADF(c, b)
		}
	}
}
