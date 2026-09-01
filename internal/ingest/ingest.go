// Package ingest turns Git/file/sdlc-kit signals into normalized FeatureEvents (PRD §7).
package ingest

import (
	"context"
	"fmt"
	"slices"

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

// Adapter names accepted in discovery.adapter; each adapter's Name() returns its constant.
const (
	AdapterGenericGit = "generic-git"
	AdapterFile       = "file"
	AdapterSDLCKit    = "sdlc-kit"
)

// StatusShipped is the only FeatureEvent status adapters emit.
const StatusShipped = "shipped"

// New selects the adapter from cfg.Discovery.Adapter.
func New(cfg *config.Config, st *store.Store) (Adapter, error) {
	switch cfg.Discovery.Adapter {
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
	return nil, fmt.Errorf("ingest: unknown discovery.adapter %q (want %s, %s or %s)",
		cfg.Discovery.Adapter, AdapterGenericGit, AdapterFile, AdapterSDLCKit)
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
