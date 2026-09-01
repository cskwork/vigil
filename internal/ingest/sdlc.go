package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// sdlcAdapter (sdlc-kit) reads work items produced by the sdlc-kit skill.
//
// Convention: every directory under <project.repo>/.sdlc/work/ is one work item, and its
// STATE.md (or state.md) carries simple "key: value" lines. Keys are case-insensitive; leading
// markdown bullets/headings and emphasis or backticks around key and value are ignored; prose
// lines (key with spaces) are skipped; the first occurrence of a key wins. An item is shipped
// when it has `status: shipped` or `phase: SHIP`.
//
//	feature_id | feature | id   feature id (default: the directory name)
//	sha | shipped_sha           shipped commit (required; shipped items without it are skipped)
//	shipped_at                  RFC3339 (default: STATE.md modification time)
//	summary | title             (default: the feature id)
//	changed_paths, routes       comma-separated lists (optional)
//
// Each item is delivered once per sha; the marker "ingest:sdlc:<feature_id>" lives in scheduler_state.
type sdlcAdapter struct {
	st   *store.Store
	root string
}

func newSDLCAdapter(cfg *config.Config, st *store.Store) *sdlcAdapter {
	return &sdlcAdapter{st: st, root: filepath.Join(cfg.Abs(cfg.Project.Repo), ".sdlc", "work")}
}

func (s *sdlcAdapter) Name() string { return AdapterSDLCKit }

func (s *sdlcAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	events, err := s.readAll()
	if err != nil {
		return nil, err
	}
	return deliverOnce(ctx, s.st, "ingest:sdlc:", events)
}

func (s *sdlcAdapter) History(_ context.Context, limit int) ([]model.FeatureEvent, error) {
	events, err := s.readAll()
	if err != nil {
		return nil, err
	}
	return newestOldestFirst(events, limit), nil
}

var stateFileNames = []string{"STATE.md", "state.md"}

// readAll returns every shipped work item; a missing root yields nothing.
func (s *sdlcAdapter) readAll() ([]model.FeatureEvent, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []model.FeatureEvent
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := findStateFile(filepath.Join(s.root, e.Name()))
		if path == "" {
			continue
		}
		ev, shipped, err := readStateFile(path, e.Name())
		if err != nil {
			log.Printf("ingest[%s]: skip %s: %v", s.Name(), path, err)
			continue
		}
		if !shipped {
			continue
		}
		stamp(&ev, s.Name())
		events = append(events, ev)
	}
	return events, nil
}

func findStateFile(dir string) string {
	for _, name := range stateFileNames {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// readStateFile parses one STATE.md; shipped is false when the item lacks a shipped marker or a sha.
func readStateFile(path, dirName string) (model.FeatureEvent, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.FeatureEvent{}, false, err
	}
	kv := parseKeyValues(string(raw))
	shipped := strings.EqualFold(kv["status"], "shipped") || strings.EqualFold(kv["phase"], "ship")
	sha := firstValue(kv, "sha", "shipped_sha")
	if !shipped || sha == "" {
		return model.FeatureEvent{}, false, nil
	}
	id := firstValue(kv, "feature_id", "feature", "id")
	if id == "" {
		id = dirName
	}
	summary := firstValue(kv, "summary", "title")
	if summary == "" {
		summary = id
	}
	ev := model.FeatureEvent{
		FeatureID:    id,
		ShippedSHA:   sha,
		ChangedPaths: splitList(kv["changed_paths"]),
		Routes:       splitList(kv["routes"]),
		Summary:      summary,
	}
	if v := kv["shipped_at"]; v != "" {
		if ev.ShippedAt, err = time.Parse(time.RFC3339, v); err != nil {
			return model.FeatureEvent{}, false, fmt.Errorf("shipped_at: %w", err)
		}
		return ev, true, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return model.FeatureEvent{}, false, err
	}
	ev.ShippedAt = info.ModTime()
	return ev, true, nil
}

// parseKeyValues extracts "key: value" lines from markdown. Keys are lower-cased; markdown bullets,
// headings, table pipes, emphasis and backticks are stripped; keys containing whitespace (prose such
// as "Note: ...") are ignored; the first occurrence of a key wins.
func parseKeyValues(text string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "-*>#|` ")
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.Trim(strings.TrimSpace(key), "*`\"'"))
		value = strings.Trim(strings.TrimSpace(value), "*`\"'")
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		if _, dup := kv[key]; !dup {
			kv[key] = value
		}
	}
	return kv
}

// firstValue returns the value of the first key present and non-empty.
func firstValue(kv map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := kv[k]; v != "" {
			return v
		}
	}
	return ""
}

// splitList splits a comma-separated value, trimming blanks and dropping empties.
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
