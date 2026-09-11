// Package jira writes approval receipts back to the source issue through the
// acli binary (the same CLI discovery.jira reads with). Rule 12 holds: no LLM,
// and no credentials are handled here; acli keeps its own token store.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotVisible is returned when the create call succeeded but the comment
// could not be read back; acli writes have been observed to report success
// without effect, so a receipt is only trusted after a list confirms it.
var ErrNotVisible = errors.New("comment not visible after create")

// DryRunPrefix marks a Comment result that was written to a file instead of posted.
const DryRunPrefix = "dry-run:"

// Client posts comments with `<CLI> jira workitem comment create|list`.
type Client struct {
	CLI string
	// DryRunDir, when set, turns Comment into a file write (<dir>/<KEY>-<UTC ts>.md).
	DryRunDir string
	Now       func() time.Time
}

// Comment posts body to the issue and verifies it is listed afterwards. The
// verification looks for the first line of body (which carries the scenario id
// marker) in any listed comment. It returns the comment id, or
// "dry-run:<path>" when DryRunDir is set.
func (c *Client) Comment(ctx context.Context, key, body string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("jira: empty issue key")
	}
	if c.DryRunDir != "" {
		return c.dryRun(key, body)
	}
	cli := c.CLI
	if cli == "" {
		cli = "acli"
	}
	out, err := run(ctx, cli, "jira", "workitem", "comment", "create", "--key", key, "--body", body, "--json")
	if err != nil {
		return "", err
	}
	createdID := idOf(out)

	listed, err := run(ctx, cli, "jira", "workitem", "comment", "list", "--key", key, "--json")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	var list struct {
		Comments []struct {
			ID   json.RawMessage `json:"id"`
			Body string          `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(listed, &list); err != nil {
		return "", fmt.Errorf("%w: parse list: %v", ErrNotVisible, err)
	}
	marker := firstLine(body)
	for _, cm := range list.Comments {
		id := rawString(cm.ID)
		if (createdID != "" && id == createdID) || (marker != "" && strings.Contains(cm.Body, marker)) {
			if id == "" {
				id = createdID
			}
			return id, nil
		}
	}
	return "", ErrNotVisible
}

func (c *Client) dryRun(key, body string) (string, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if err := os.MkdirAll(c.DryRunDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.md", sanitize(key), now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(c.DryRunDir, name)
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		return "", err
	}
	return DryRunPrefix + path, nil
}

// IsDryRun reports whether a Comment result is a file path rather than a comment id.
func IsDryRun(id string) bool { return strings.HasPrefix(id, DryRunPrefix) }

// DryRunPath extracts the file path from a dry-run result ("" otherwise).
func DryRunPath(id string) string {
	if !IsDryRun(id) {
		return ""
	}
	return strings.TrimPrefix(id, DryRunPrefix)
}

func run(ctx context.Context, cli string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cli, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("jira: %s %s: %w: %s", cli, strings.Join(args[:4], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// idOf reads the id from a create response leniently: {"id"} or {"comment":{"id"}}.
func idOf(out []byte) string {
	var v struct {
		ID      json.RawMessage `json:"id"`
		Comment struct {
			ID json.RawMessage `json:"id"`
		} `json:"comment"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &v) != nil {
		return ""
	}
	if id := rawString(v.ID); id != "" {
		return id
	}
	return rawString(v.Comment.ID)
}

// rawString accepts an id encoded as a JSON string or number.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
