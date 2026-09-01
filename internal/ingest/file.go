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

	"gopkg.in/yaml.v3"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// fileAdapter (file) reads one FeatureEvent per *.yaml/*.yml file in discovery.features_dir
// (PRD §7 shape). A file is delivered once per shipped_sha; the marker "ingest:file:<feature_id>"
// lives in scheduler_state.
type fileAdapter struct {
	st  *store.Store
	dir string
}

func newFileAdapter(cfg *config.Config, st *store.Store) *fileAdapter {
	return &fileAdapter{st: st, dir: cfg.Abs(cfg.Discovery.FeaturesDir)}
}

func (f *fileAdapter) Name() string { return AdapterFile }

func (f *fileAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	events, err := f.readAll()
	if err != nil {
		return nil, err
	}
	return deliverOnce(ctx, f.st, "ingest:file:", events)
}

func (f *fileAdapter) History(_ context.Context, limit int) ([]model.FeatureEvent, error) {
	events, err := f.readAll()
	if err != nil {
		return nil, err
	}
	return newestOldestFirst(events, limit), nil
}

// featureFile is the on-disk FeatureEvent with shipped_at kept as text, so both bare YAML
// timestamps and quoted RFC3339 strings are accepted (yaml.v3 decodes only bare ones into time.Time).
type featureFile struct {
	FeatureID    string   `yaml:"feature_id"`
	ShippedSHA   string   `yaml:"shipped_sha"`
	ShippedAt    string   `yaml:"shipped_at"`
	ChangedPaths []string `yaml:"changed_paths"`
	Routes       []string `yaml:"routes"`
	Summary      string   `yaml:"summary"`
}

// readAll parses every feature file in name order; a missing directory yields nothing.
// Unreadable or id-less files are logged and skipped.
func (f *fileAdapter) readAll() ([]model.FeatureEvent, error) {
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []model.FeatureEvent
	for _, e := range entries { // ReadDir sorts by name
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		path := filepath.Join(f.dir, e.Name())
		ev, err := readFeatureFile(path)
		if err != nil {
			log.Printf("ingest[%s]: skip %s: %v", f.Name(), path, err)
			continue
		}
		stamp(&ev, f.Name())
		events = append(events, ev)
	}
	return events, nil
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// readFeatureFile decodes one file; an absent shipped_at falls back to the file's mtime.
func readFeatureFile(path string) (model.FeatureEvent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.FeatureEvent{}, err
	}
	var ff featureFile
	if err := yaml.Unmarshal(raw, &ff); err != nil {
		return model.FeatureEvent{}, err
	}
	if ff.FeatureID == "" {
		return model.FeatureEvent{}, errors.New("feature_id is required")
	}
	ev := model.FeatureEvent{
		FeatureID:    ff.FeatureID,
		ShippedSHA:   ff.ShippedSHA,
		ChangedPaths: ff.ChangedPaths,
		Routes:       ff.Routes,
		Summary:      ff.Summary,
	}
	if ff.ShippedAt != "" {
		if ev.ShippedAt, err = time.Parse(time.RFC3339, ff.ShippedAt); err != nil {
			return model.FeatureEvent{}, fmt.Errorf("shipped_at: %w", err)
		}
		return ev, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return model.FeatureEvent{}, err
	}
	ev.ShippedAt = info.ModTime()
	return ev, nil
}
