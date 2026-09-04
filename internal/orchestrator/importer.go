package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vigil/internal/dsl"
	"vigil/internal/model"
	"vigil/internal/store"
)

// ImportScenarioFiles loads seed scenarios/flows from disk into the store as SOAK
// (seed origin, oracle required); used by `add`/`scan`. Existing scenarios get a
// new version when the file changed (fingerprint or text). Returns created+updated.
func (o *Orchestrator) ImportScenarioFiles(ctx context.Context) (int, error) {
	count := 0
	flowFiles, err := yamlFiles(o.cfg.Abs(o.cfg.Paths.Flows))
	if err != nil {
		return 0, err
	}
	for _, path := range flowFiles {
		f, err := dsl.ParseFlowFile(path)
		if err != nil {
			return count, err
		}
		if err := f.Validate(); err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return count, err
		}
		ver := f.Flow.Version
		if ver < 1 {
			ver = 1
		}
		if err := o.st.UpsertFlow(ctx, o.cfg.Project.ID, f.Flow.ID, ver, string(raw)); err != nil {
			return count, err
		}
	}
	flows, flowIDs, err := o.loadFlows(ctx)
	if err != nil {
		return count, err
	}
	scFiles, err := yamlFiles(o.cfg.Abs(o.cfg.Paths.Scenarios))
	if err != nil {
		return count, err
	}
	for _, path := range scFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			return count, err
		}
		sc, err := dsl.Parse(raw)
		if err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		if sc.Scenario.Version < 1 {
			sc.Scenario.Version = 1
		}
		if err := sc.Validate(flowIDs); err != nil {
			return count, fmt.Errorf("%s: %w", path, err)
		}
		fp := sc.Fingerprint(flows)
		text := string(raw)
		existing, err := o.st.GetScenario(ctx, o.cfg.Project.ID, sc.Scenario.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			now := o.now()
			m := o.scenarioModel(sc, sc.Scenario.ID, model.StateSoak, fp, "seed")
			m.NextDueAt = &now
			v := &model.ScenarioVersion{ScenarioID: sc.Scenario.ID, Version: sc.Scenario.Version, YAML: text, Fingerprint: fp, CreatedBy: "seed", Reason: "imported from " + rel(o.cfg.BaseDir, path)}
			if err := o.st.CreateScenario(ctx, m, v, linksFor(sc)); err != nil {
				return count, err
			}
			_ = o.st.SetState(ctx, "import:hash:"+sc.Scenario.ID, textHash(text))
			o.logger.Printf("import %s: created %s (SOAK)", rel(o.cfg.BaseDir, path), sc.Scenario.ID)
			count++
		case err != nil:
			return count, err
		default:
			// Compare against the text imported LAST TIME, not against the current
			// version: system-generated versions (e.g. CHROMIUM_CONFIRM → browser.primary:
			// chromium, or a validated repair) must not be overwritten by re-importing an
			// unchanged seed file. Only a change to the file itself creates a version.
			hashKey := "import:hash:" + existing.ID
			lastHash, _ := o.st.GetState(ctx, hashKey)
			if lastHash == "" {
				// Legacy rows imported before hash tracking: fall back to current-version equality.
				cur, err := o.st.GetScenarioVersion(ctx, o.cfg.Project.ID, existing.ID, existing.CurrentVersion)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return count, err
				}
				if cur != nil && cur.Fingerprint == fp && strings.TrimSpace(cur.YAML) == strings.TrimSpace(text) {
					_ = o.st.SetState(ctx, hashKey, textHash(text))
					continue
				}
			} else if lastHash == textHash(text) {
				continue
			}
			next := existing.CurrentVersion + 1
			if sc.Scenario.Version > next {
				next = sc.Scenario.Version
			}
			nv := &model.ScenarioVersion{ScenarioID: existing.ID, Version: next, YAML: text, Fingerprint: fp, CreatedBy: "seed", Reason: "file changed: " + rel(o.cfg.BaseDir, path)}
			if err := o.st.AddScenarioVersion(ctx, o.cfg.Project.ID, nv, linksFor(sc)); err != nil {
				return count, err
			}
			_ = o.st.SetState(ctx, hashKey, textHash(text))
			meta := o.scenarioModel(sc, existing.ID, existing.State, fp, existing.Origin)
			_ = o.st.UpdateScenarioMeta(ctx, o.cfg.Project.ID, existing.ID, meta.Title, meta.Class, meta.Mutation, meta.Locks)
			// A pin decides the engine outright (scheduler.pickBrowser), so accepting one for an
			// engine the scenario has never passed on would turn a green check red with nobody
			// asked. Keep the version, but make it earn ACTIVE again on the engine it now claims.
			state := existing.State
			pinned := pinnedBrowser(sc)
			if pinned != "" && existing.State == model.StateActive {
				if ok, err := o.st.HasPassOnBrowser(ctx, o.cfg.Project.ID, existing.ID, pinned); err == nil && !ok {
					if err := o.st.ResetSoak(ctx, o.cfg.Project.ID, existing.ID); err == nil {
						state = model.StateSoak
					}
				}
			}
			if state != existing.State {
				o.logger.Printf("import %s: %s v%d → v%d (pins %s with no passing run there; %s → %s, needs %d clean pass(es))",
					rel(o.cfg.BaseDir, path), existing.ID, existing.CurrentVersion, next, pinned, existing.State, state, o.cfg.Policy.SoakPasses)
			} else {
				o.logger.Printf("import %s: %s v%d → v%d (state %s kept)", rel(o.cfg.BaseDir, path), existing.ID, existing.CurrentVersion, next, existing.State)
			}
			count++
		}
	}
	return count, nil
}

// pinnedBrowser reports the engine a scenario forces, or "" when it takes the default.
// Mirrors the precedence in scheduler.pickBrowser.
func pinnedBrowser(sc *dsl.Scenario) model.Browser {
	if sc.Browser.RequiresChromium {
		return model.BrowserChromium
	}
	if sc.Browser.Primary != "" {
		return model.Browser(sc.Browser.Primary)
	}
	return ""
}

// yamlFiles lists *.yaml / *.yml under dir recursively (sorted); a missing dir is empty.
func yamlFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func rel(base, path string) string {
	if base == "" {
		return path
	}
	if r, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

// textHash identifies the imported file content (whitespace-insensitive at the ends).
func textHash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}
