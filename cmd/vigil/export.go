package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"vigil/internal/dsl"
	"vigil/internal/export"
	"vigil/internal/model"
)

// exportStates are the scenarios `vigil export --all` writes: everything a human
// may run or review, not rejected / retired / duplicate corpus entries.
var exportStates = []model.ScenarioState{model.StateActive, model.StateSoak, model.StatePendingApproval, model.StateNeedsReview}

// cmdExport writes <dir>/<id>.spec.ts for one scenario or every exportable one.
// The YAML DSL stays canonical; the export is derived output.
func (a *app) cmdExport(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	all := fs.Bool("all", false, "export every ACTIVE/SOAK/PENDING_APPROVAL/NEEDS_REVIEW scenario")
	format := fs.String("format", export.FormatPlaywright, "output format (playwright)")
	envName := fs.String("env", "", "target.environments entry whose base_url becomes the default BASE_URL")
	outDir := fs.String("o", "", "output directory (default export/<format>)")
	fs.StringVar(outDir, "out", "", "alias of -o")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2, err
	}
	if !*all && len(pos) < 1 {
		return 2, errors.New("usage: vigil export <scenario> | --all [--format playwright] [--env <name>] [-o <dir>]")
	}
	if _, err := export.FileName("x", *format); err != nil {
		return 1, err
	}
	env, err := a.cfg.Env(*envName)
	if err != nil {
		return 1, err
	}
	dir := *outDir
	if dir == "" {
		dir = filepath.Join("export", *format)
	}
	dir = a.cfg.Abs(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 1, fmt.Errorf("export dir %s: %w", dir, err)
	}
	flows := a.loadFlows(ctx)
	opts := export.Options{Env: env.Name, BaseURL: env.BaseURL, Flows: flows}

	var ids []string
	if *all {
		list, err := a.st.ListScenarios(ctx, a.cfg.Project.ID, exportStates...)
		if err != nil {
			return 1, err
		}
		for _, m := range list {
			ids = append(ids, m.ID)
		}
	} else {
		ids = pos
	}

	type result struct {
		ID    string `json:"id"`
		Path  string `json:"path,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var results []result
	failed := 0
	for _, id := range ids {
		path, err := a.exportOne(ctx, id, *format, dir, opts)
		r := result{ID: id, Path: path}
		if err != nil {
			r.Error = err.Error()
			failed++
			if !*all {
				return 1, err
			}
		}
		results = append(results, r)
	}
	if a.jsonOut {
		if results == nil {
			results = []result{}
		}
		if err := a.printJSON(map[string]any{"format": *format, "env": env.Name, "dir": dir, "exported": len(results) - failed, "failed": failed, "results": results}); err != nil {
			return 1, err
		}
	} else {
		for _, r := range results {
			if r.Error != "" {
				a.printf("%s: 실패, %s\n", r.ID, r.Error)
				continue
			}
			a.printf("%s → %s\n", r.ID, r.Path)
		}
		if *all {
			a.printf("내보내기 완료: %d개 (%s, env=%s) → %s", len(results)-failed, *format, env.Name, dir)
			if failed > 0 {
				a.printf(", 실패 %d개", failed)
			}
			a.printf("\n")
		}
	}
	if failed > 0 {
		return 1, fmt.Errorf("%d of %d scenarios failed to export", failed, len(ids))
	}
	return 0, nil
}

// exportOne renders the current version of one scenario into dir and returns the path.
func (a *app) exportOne(ctx context.Context, id, format, dir string, opts export.Options) (string, error) {
	_, v, err := a.st.GetCurrentScenarioVersion(ctx, a.cfg.Project.ID, id)
	if err != nil {
		return "", fmt.Errorf("scenario %s: %w", id, err)
	}
	sc, err := dsl.Parse([]byte(v.YAML))
	if err != nil {
		return "", fmt.Errorf("scenario %s v%d: %w", id, v.Version, err)
	}
	if sc.Scenario.ID == "" {
		sc.Scenario.ID = id
	}
	// The store's current version is authoritative: agent rewrites bump
	// scenarios.current_version without always rewriting scenario.version in the
	// YAML, and the export header must name the version that was exported.
	sc.Scenario.Version = v.Version
	name, err := export.FileName(id, format)
	if err != nil {
		return "", err
	}
	var body []byte
	switch format {
	case export.FormatPlaywright:
		body = export.Playwright(sc, opts)
	default:
		return "", fmt.Errorf("unsupported export format %q", format)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// loadFlows parses the current flow versions from the store; unparsable flows are
// reported and skipped (the export then marks them as unknown).
func (a *app) loadFlows(ctx context.Context) map[string]*dsl.Flow {
	flows := map[string]*dsl.Flow{}
	ys, err := a.st.ListFlowYAML(ctx, a.cfg.Project.ID)
	if err != nil {
		a.log.Printf("flows: %v", err)
		return flows
	}
	for id, y := range ys {
		f, err := dsl.ParseFlow([]byte(y))
		if err != nil {
			a.log.Printf("flow %s: %v", id, err)
			continue
		}
		flows[id] = f
	}
	return flows
}
