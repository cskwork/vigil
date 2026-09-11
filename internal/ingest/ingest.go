// Package ingest turns Git/file/sdlc-kit signals into normalized FeatureEvents (PRD §7).
package ingest

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// Adapter polls a source for new shipped features.
type Adapter interface {
	Name() string
	// Poll returns events newer than the adapter's persisted cursor and advances it.
	Poll(ctx context.Context) ([]model.FeatureEvent, error)
	// History returns up to limit historical events (oldest first) for `import --history`.
	History(ctx context.Context, limit int) ([]model.FeatureEvent, error)
}

// Adapter names accepted in discovery.adapter(s); each adapter's Name() returns its constant.
const (
	AdapterGenericGit = config.AdapterGenericGit
	AdapterFile       = config.AdapterFile
	AdapterSDLCKit    = config.AdapterSDLCKit
	AdapterJira       = config.AdapterJira
	AdapterLoki       = config.AdapterLoki
	AdapterExec       = config.AdapterExec
)

// StatusShipped is the only FeatureEvent status adapters emit.
const StatusShipped = "shipped"

// MaxDetails bounds FeatureEvent.Details (issue description / sample log lines).
const MaxDetails = 8 * 1024

// New builds the adapter set from discovery.adapters (legacy discovery.adapter
// when the list is empty). Several names yield a multi adapter.
func New(cfg *config.Config, st *store.Store) (Adapter, error) {
	names := cfg.AdapterNames()
	if len(names) == 1 {
		return newOne(cfg, st, names[0])
	}
	var parts []Adapter
	for _, name := range names {
		a, err := newOne(cfg, st, name)
		if err != nil {
			return nil, err
		}
		parts = append(parts, a)
	}
	return newMulti(parts), nil
}

func newOne(cfg *config.Config, st *store.Store, name string) (Adapter, error) {
	switch name {
	case AdapterJira:
		return newJiraAdapter(cfg, st), nil
	case AdapterLoki:
		return newLokiAdapter(cfg, st), nil
	case AdapterExec:
		return newExecAdapter(cfg, st), nil
	case AdapterGenericGit:
		if cfg.Project.Repo == "" {
			return nil, fmt.Errorf("ingest: %s needs project.repo", AdapterGenericGit)
		}
		return newGitAdapter(cfg, st), nil
	case AdapterFile:
		if cfg.Discovery.FeaturesDir == "" {
			return nil, fmt.Errorf("ingest: %s needs discovery.features_dir", AdapterFile)
		}
		return newFileAdapter(cfg, st), nil
	case AdapterSDLCKit:
		if cfg.Project.Repo == "" {
			return nil, fmt.Errorf("ingest: %s needs project.repo", AdapterSDLCKit)
		}
		return newSDLCAdapter(cfg, st), nil
	}
	return nil, fmt.Errorf("ingest: unknown discovery.adapter %q (want %s, %s, %s, %s, %s or %s)",
		name, AdapterGenericGit, AdapterFile, AdapterSDLCKit, AdapterJira, AdapterLoki, AdapterExec)
}

// multi polls several adapters as one: an adapter error is logged and skipped so
// a broken source never hides the others; History fans out with the same limit.
type multi struct {
	parts []Adapter
}

func newMulti(parts []Adapter) *multi { return &multi{parts: parts} }

func (m *multi) Name() string {
	names := make([]string, 0, len(m.parts))
	for _, p := range m.parts {
		names = append(names, p.Name())
	}
	return strings.Join(names, ",")
}

func (m *multi) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	var out []model.FeatureEvent
	for _, p := range m.parts {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		events, err := p.Poll(ctx)
		if err != nil {
			log.Printf("ingest[%s]: poll failed, skipped: %v", p.Name(), err)
			continue
		}
		out = append(out, events...)
	}
	return out, nil
}

func (m *multi) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	var out []model.FeatureEvent
	for _, p := range m.parts {
		events, err := p.History(ctx, limit)
		if err != nil {
			log.Printf("ingest[%s]: history failed, skipped: %v", p.Name(), err)
			continue
		}
		out = append(out, events...)
	}
	slices.SortStableFunc(out, func(a, b model.FeatureEvent) int { return a.ShippedAt.Compare(b.ShippedAt) })
	return out, nil
}

// pollDue reports whether the adapter's own poll_interval has elapsed since the
// last poll recorded in scheduler_state ("ingest:<name>:last_poll"), and records
// now when it has. The scheduler keeps calling Poll on discovery.poll_interval.
func pollDue(ctx context.Context, st *store.Store, name string, interval time.Duration, now time.Time) (bool, error) {
	if st == nil || interval <= 0 {
		return true, nil
	}
	key := "ingest:" + name + ":last_poll"
	raw, err := st.GetState(ctx, key)
	if err != nil {
		return false, err
	}
	if last, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); perr == nil && now.Sub(time.UnixMilli(last)) < interval {
		return false, nil
	}
	return true, st.SetState(ctx, key, strconv.FormatInt(now.UnixMilli(), 10))
}

// deliverOnceBy drops events whose key (from keyOf) was already delivered and
// records every event it returns. Keys embed the content hash / bucket, so a
// source item is delivered once per revision.
func deliverOnceBy(ctx context.Context, st *store.Store, events []model.FeatureEvent, keyOf func(model.FeatureEvent) string) ([]model.FeatureEvent, error) {
	var out []model.FeatureEvent
	for _, ev := range events {
		seen, err := st.GetState(ctx, keyOf(ev))
		if err != nil {
			return nil, err
		}
		if seen != "" {
			continue
		}
		out = append(out, ev)
	}
	for _, ev := range out {
		if err := st.SetState(ctx, keyOf(ev), "1"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// shortHash is the first 8 hex chars of sha1(s).
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// bound truncates s to at most n bytes on a rune boundary, marking the cut.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[truncated]"
}

// applyRouteHints returns the routes whose keyword appears in text (case-insensitive substring).
func applyRouteHints(hints []config.RouteHint, text string) []string {
	lower := strings.ToLower(text)
	var routes []string
	for _, h := range hints {
		if h.Match != "" && strings.Contains(lower, strings.ToLower(h.Match)) {
			routes = append(routes, h.Route)
		}
	}
	return uniqueSorted(routes)
}

// safeID keeps [A-Za-z0-9._-] and replaces every other rune with '-'.
func safeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// stamp marks an event as shipped by the named adapter.
func stamp(ev *model.FeatureEvent, source string) {
	ev.Source = source
	ev.Status = StatusShipped
}

// deliverOnce drops events whose "<keyPrefix><feature_id>" marker already holds the shipped sha
// and records the marker for every event it returns.
func deliverOnce(ctx context.Context, st *store.Store, keyPrefix string, events []model.FeatureEvent) ([]model.FeatureEvent, error) {
	var out []model.FeatureEvent
	for _, ev := range events {
		seen, err := st.GetState(ctx, keyPrefix+ev.FeatureID)
		if err != nil {
			return nil, err
		}
		if seen == shaMarker(ev.ShippedSHA) {
			continue
		}
		out = append(out, ev)
	}
	for _, ev := range out {
		if err := st.SetState(ctx, keyPrefix+ev.FeatureID, shaMarker(ev.ShippedSHA)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// shaMarker is the processed-marker value: the shipped sha, or "-" when the source has none.
func shaMarker(sha string) string {
	if sha == "" {
		return "-"
	}
	return sha
}

// uniqueSorted returns the distinct non-empty strings of in, sorted.
func uniqueSorted(in []string) []string {
	out := slices.DeleteFunc(slices.Clone(in), func(s string) bool { return s == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

// newestOldestFirst sorts events oldest-first by ShippedAt (stable) and keeps the newest limit.
func newestOldestFirst(events []model.FeatureEvent, limit int) []model.FeatureEvent {
	slices.SortStableFunc(events, func(a, b model.FeatureEvent) int { return a.ShippedAt.Compare(b.ShippedAt) })
	return newest(events, limit)
}

// newest keeps the last limit events of an oldest-first slice; limit <= 0 keeps all.
func newest(events []model.FeatureEvent, limit int) []model.FeatureEvent {
	if limit <= 0 || limit >= len(events) {
		return events
	}
	return events[len(events)-limit:]
}
