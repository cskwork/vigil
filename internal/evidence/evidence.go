// Package evidence manages run artifact directories and incident documents (PRD §15).
//
// Layout under the evidence root:
//
//	runs/<scenario>/<UTC ts>-a<attempt>/   one attempt (dom.html, console.json, network.json, PASS marker)
//	agent/<feature>/<UTC ts>/              one Browser Agent task (transcript, request, result)
//	incidents/<UTC ts>-<scenario>.md|json  portable incident for a coding agent
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vigil/internal/model"
	"vigil/internal/runner"
)

// PassMarker is the file the scheduler writes into a run dir after a PASS
// classification; Prune keeps such dirs for the shorter pass retention.
const PassMarker = "PASS"

const tsLayout = "20060102T150405.000Z"

type Store struct {
	root string
}

func New(root string) *Store { return &Store{root: root} }

func (s *Store) Root() string         { return s.root }
func (s *Store) RunsDir() string      { return filepath.Join(s.root, "runs") }
func (s *Store) AgentRoot() string    { return filepath.Join(s.root, "agent") }
func (s *Store) IncidentsDir() string { return filepath.Join(s.root, "incidents") }

// RunDir returns (and creates) <root>/runs/<scenario>/<UTC ts>-a<attempt>/.
func (s *Store) RunDir(scenarioID string, at time.Time, attempt int) (string, error) {
	if scenarioID == "" {
		return "", errors.New("evidence: scenario id required")
	}
	if attempt < 1 {
		attempt = 1
	}
	dir := filepath.Join(s.RunsDir(), safeName(scenarioID), fmt.Sprintf("%s-a%d", at.UTC().Format(tsLayout), attempt))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// AgentDir returns (and creates) <root>/agent/<feature>/<UTC ts>/.
func (s *Store) AgentDir(featureID string, at time.Time) (string, error) {
	if featureID == "" {
		featureID = "unknown-feature"
	}
	dir := filepath.Join(s.AgentRoot(), safeName(featureID), at.UTC().Format(tsLayout))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// MarkPass writes the PASS marker into a run dir.
func MarkPass(runDir string) error {
	return os.WriteFile(filepath.Join(runDir, PassMarker), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// IsPass reports whether a run dir carries the PASS marker.
func IsPass(runDir string) bool {
	_, err := os.Stat(filepath.Join(runDir, PassMarker))
	return err == nil
}

// IncidentInput is everything needed to render a portable incident.
type IncidentInput struct {
	Incident *model.Incident
	Run      *model.Run
	Scenario *model.Scenario
	Version  *model.ScenarioVersion
	Feature  *model.Feature
	// Extra evidence paths (dom, console, network, screenshot, chromium confirmation).
	Artifacts map[string]string
	Reason    string
}

// IncidentDoc is the JSON shape written next to the Markdown incident.
type IncidentDoc struct {
	Title           string            `json:"title"`
	Kind            string            `json:"kind"`
	Project         string            `json:"project"`
	Feature         string            `json:"feature,omitempty"`
	ShippedSHA      string            `json:"shipped_sha,omitempty"`
	FeatureSummary  string            `json:"feature_summary,omitempty"`
	ChangedPaths    []string          `json:"changed_paths,omitempty"`
	ScenarioID      string            `json:"scenario_id"`
	ScenarioVersion int               `json:"scenario_version"`
	ScenarioTitle   string            `json:"scenario_title,omitempty"`
	Browser         string            `json:"browser"`
	Attempt         int               `json:"attempt"`
	Outcome         string            `json:"outcome"`
	Reason          string            `json:"reason,omitempty"`
	FailedStep      int               `json:"failed_step"`
	FailedAction    string            `json:"failed_action,omitempty"`
	Expected        string            `json:"expected,omitempty"`
	Actual          string            `json:"actual,omitempty"`
	Error           string            `json:"error,omitempty"`
	StartedAt       time.Time         `json:"started_at"`
	FinishedAt      time.Time         `json:"finished_at"`
	DurationMs      int64             `json:"duration_ms"`
	EvidenceDir     string            `json:"evidence_dir,omitempty"`
	Artifacts       map[string]string `json:"artifacts,omitempty"`
	ConsoleErrors   []string          `json:"console_errors,omitempty"`
	FailedRequests  []string          `json:"failed_requests,omitempty"`
	ChromiumConfirm string            `json:"chromium_confirmation,omitempty"`
	ScenarioYAML    string            `json:"scenario_yaml"`
	CreatedAt       time.Time         `json:"created_at"`
}

// WriteIncident renders Markdown + JSON and returns their paths.
func (s *Store) WriteIncident(ctx context.Context, in IncidentInput) (mdPath, jsonPath string, err error) {
	if in.Run == nil {
		return "", "", errors.New("evidence: incident needs a run")
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(s.IncidentsDir(), 0o755); err != nil {
		return "", "", err
	}
	doc := s.buildDoc(in)
	base := fmt.Sprintf("%s-%s", doc.CreatedAt.Format(tsLayout), safeName(doc.ScenarioID))
	mdPath = filepath.Join(s.IncidentsDir(), base+".md")
	jsonPath = filepath.Join(s.IncidentsDir(), base+".json")

	js, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, js, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(renderMarkdown(doc)), 0o644); err != nil {
		return "", "", err
	}
	return mdPath, jsonPath, nil
}

func (s *Store) buildDoc(in IncidentInput) IncidentDoc {
	r := in.Run
	doc := IncidentDoc{
		Kind:            string(model.IncidentAppRegression),
		Project:         r.ProjectID,
		Feature:         r.FeatureID,
		ShippedSHA:      r.ShippedSHA,
		ScenarioID:      r.ScenarioID,
		ScenarioVersion: r.ScenarioVersion,
		Browser:         string(r.Browser),
		Attempt:         r.Attempt,
		Outcome:         string(r.Outcome),
		Reason:          in.Reason,
		FailedStep:      r.FailedStep,
		FailedAction:    r.FailedAction,
		Expected:        r.Expected,
		Actual:          r.Actual,
		Error:           r.Error,
		StartedAt:       r.StartedAt.UTC(),
		FinishedAt:      r.FinishedAt.UTC(),
		DurationMs:      r.DurationMs,
		EvidenceDir:     r.EvidenceDir,
		Artifacts:       map[string]string{},
		CreatedAt:       time.Now().UTC(),
	}
	if in.Incident != nil {
		if in.Incident.Kind != "" {
			doc.Kind = string(in.Incident.Kind)
		}
		if in.Incident.Title != "" {
			doc.Title = in.Incident.Title
		}
		if doc.Feature == "" {
			doc.Feature = in.Incident.FeatureID
		}
		if doc.ShippedSHA == "" {
			doc.ShippedSHA = in.Incident.ShippedSHA
		}
		if !in.Incident.CreatedAt.IsZero() {
			doc.CreatedAt = in.Incident.CreatedAt.UTC()
		}
	}
	if in.Scenario != nil {
		doc.ScenarioTitle = in.Scenario.Title
		if doc.ScenarioID == "" {
			doc.ScenarioID = in.Scenario.ID
		}
	}
	if in.Version != nil {
		doc.ScenarioYAML = in.Version.YAML
		if doc.ScenarioVersion == 0 {
			doc.ScenarioVersion = in.Version.Version
		}
	}
	if in.Feature != nil {
		if doc.Feature == "" {
			doc.Feature = in.Feature.ID
		}
		if doc.ShippedSHA == "" {
			doc.ShippedSHA = in.Feature.LatestShippedSHA
		}
		doc.FeatureSummary = in.Feature.Summary
		doc.ChangedPaths = in.Feature.ChangedPaths
	}
	for k, v := range in.Artifacts {
		if v == "" {
			continue
		}
		doc.Artifacts[k] = v
	}
	// Discover standard artifacts in the run dir when the caller did not list them.
	if r.EvidenceDir != "" {
		for _, name := range []string{"dom.html", "console.json", "network.json", "steps.json", "screenshot.png"} {
			kind := strings.TrimSuffix(name, filepath.Ext(name))
			if _, ok := doc.Artifacts[kind]; ok {
				continue
			}
			p := filepath.Join(r.EvidenceDir, name)
			if _, err := os.Stat(p); err == nil {
				doc.Artifacts[kind] = p
			}
		}
	}
	if p := doc.Artifacts["console"]; p != "" {
		doc.ConsoleErrors = consoleErrors(p)
	}
	if p := doc.Artifacts["network"]; p != "" {
		doc.FailedRequests = failedRequests(p)
	}
	if p := doc.Artifacts["chromium"]; p != "" {
		doc.ChromiumConfirm = readExcerpt(p, 4000)
	}
	if doc.Title == "" {
		doc.Title = fmt.Sprintf("%s: %s failed (%s)", doc.Outcome, doc.ScenarioID, doc.FailedAction)
		if doc.FailedAction == "" {
			doc.Title = fmt.Sprintf("%s: %s failed", doc.Outcome, doc.ScenarioID)
		}
	}
	return doc
}

func renderMarkdown(d IncidentDoc) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("# %s\n\n", d.Title)
	w("- kind: %s\n- project: %s\n- outcome: %s\n", d.Kind, d.Project, d.Outcome)
	if d.Reason != "" {
		w("- reason: %s\n", d.Reason)
	}
	w("- created: %s\n\n", d.CreatedAt.Format(time.RFC3339))

	w("## Context\n\n")
	w("| field | value |\n|---|---|\n")
	w("| feature | %s |\n", orDash(d.Feature))
	w("| shipped sha | %s |\n", orDash(d.ShippedSHA))
	if d.FeatureSummary != "" {
		w("| feature summary | %s |\n", mdCell(d.FeatureSummary))
	}
	w("| scenario | %s v%d |\n", d.ScenarioID, d.ScenarioVersion)
	if d.ScenarioTitle != "" {
		w("| scenario title | %s |\n", mdCell(d.ScenarioTitle))
	}
	w("| browser | %s |\n| attempt | %d |\n", d.Browser, d.Attempt)
	w("| started | %s |\n| duration | %d ms |\n", d.StartedAt.Format(time.RFC3339), d.DurationMs)
	if len(d.ChangedPaths) > 0 {
		w("\nChanged paths of the shipped feature:\n\n")
		for _, p := range d.ChangedPaths {
			w("- `%s`\n", p)
		}
	}

	w("\n## Expected vs actual\n\n")
	w("| | |\n|---|---|\n")
	w("| failed step | %d %s |\n", d.FailedStep, d.FailedAction)
	w("| expected | %s |\n", mdCell(orDash(d.Expected)))
	w("| actual | %s |\n", mdCell(orDash(d.Actual)))
	if d.Error != "" {
		w("\n```text\n%s\n```\n", d.Error)
	}

	w("\n## Console errors\n\n")
	if len(d.ConsoleErrors) == 0 {
		w("none captured\n")
	}
	for _, c := range d.ConsoleErrors {
		w("- %s\n", c)
	}

	w("\n## Failed / 5xx requests\n\n")
	if len(d.FailedRequests) == 0 {
		w("none captured\n")
	}
	for _, r := range d.FailedRequests {
		w("- %s\n", r)
	}

	w("\n## Artifacts\n\n")
	if d.EvidenceDir != "" {
		w("- evidence dir: `%s`\n", d.EvidenceDir)
	}
	keys := make([]string, 0, len(d.Artifacts))
	for k := range d.Artifacts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("- %s: `%s`\n", k, d.Artifacts[k])
	}
	if d.ChromiumConfirm != "" {
		w("\n## Chromium confirmation\n\n```text\n%s\n```\n", d.ChromiumConfirm)
	}

	w("\n## Reproduction\n\n")
	w("Run the scenario below with `qa-flow run %s` (or replay the steps manually against the target).\n\n", d.ScenarioID)
	w("```yaml\n%s\n```\n", strings.TrimRight(d.ScenarioYAML, "\n"))
	return b.String()
}

// Prune deletes pass evidence older than passDays and failure evidence older
// than failDays (PRD §15: failures are kept longer). Agent dirs follow the
// failure retention. Incidents are never pruned.
func (s *Store) Prune(passDays, failDays int) error {
	now := time.Now().UTC()
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	scenarios, _ := os.ReadDir(s.RunsDir())
	for _, sc := range scenarios {
		if !sc.IsDir() {
			continue
		}
		scDir := filepath.Join(s.RunsDir(), sc.Name())
		runs, _ := os.ReadDir(scDir)
		for _, rd := range runs {
			if !rd.IsDir() {
				continue
			}
			dir := filepath.Join(scDir, rd.Name())
			age := now.Sub(dirTime(dir, rd.Name()))
			limit := time.Duration(failDays) * 24 * time.Hour
			if IsPass(dir) {
				limit = time.Duration(passDays) * 24 * time.Hour
			}
			if limit > 0 && age > limit {
				keep(os.RemoveAll(dir))
			}
		}
		if left, _ := os.ReadDir(scDir); len(left) == 0 {
			_ = os.Remove(scDir)
		}
	}
	features, _ := os.ReadDir(s.AgentRoot())
	for _, f := range features {
		if !f.IsDir() {
			continue
		}
		fDir := filepath.Join(s.AgentRoot(), f.Name())
		tasks, _ := os.ReadDir(fDir)
		for _, td := range tasks {
			if !td.IsDir() {
				continue
			}
			dir := filepath.Join(fDir, td.Name())
			if failDays > 0 && now.Sub(dirTime(dir, td.Name())) > time.Duration(failDays)*24*time.Hour {
				keep(os.RemoveAll(dir))
			}
		}
		if left, _ := os.ReadDir(fDir); len(left) == 0 {
			_ = os.Remove(fDir)
		}
	}
	return firstErr
}

// dirTime parses the timestamp prefix of a run/agent dir name, falling back to mtime.
func dirTime(dir, name string) time.Time {
	if len(name) >= len(tsLayout) {
		if t, err := time.Parse(tsLayout, name[:len(tsLayout)]); err == nil {
			return t
		}
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Time{}
}

// ---- helpers -----------------------------------------------------------------

func consoleErrors(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []runner.ConsoleEvent
	if err := json.Unmarshal(b, &events); err != nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return []string{"(unparsed console log) " + excerpt(s, 500)}
		}
		return nil
	}
	var out []string
	for _, e := range events {
		if e.Level != "error" && e.Level != "exception" {
			continue
		}
		line := fmt.Sprintf("[%s] %s", e.Level, excerpt(e.Text, 300))
		if e.URL != "" {
			line += fmt.Sprintf(" (%s:%d)", e.URL, e.Line)
		}
		out = append(out, line)
	}
	return out
}

func failedRequests(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []runner.NetworkEvent
	if err := json.Unmarshal(b, &events); err != nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return []string{"(unparsed network log) " + excerpt(s, 500)}
		}
		return nil
	}
	var out []string
	for _, e := range events {
		if !e.Failed && e.Status < 500 {
			continue
		}
		line := fmt.Sprintf("%s %s → %d", e.Method, e.URL, e.Status)
		if e.Error != "" {
			line += " (" + e.Error + ")"
		}
		out = append(out, line)
	}
	return out
}

func readExcerpt(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return excerpt(string(b), n)
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// safeName keeps ids filesystem-friendly.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// PruneReport summarizes one prune pass.
type PruneReport struct {
	DeletedDirs int
	FreedBytes  int64
	TotalBytes  int64 // after pruning
	RunDirs     int
	AgentDirs   int
}

type evidenceDir struct {
	path   string
	group  string // scenario id or feature id
	kind   string // pass | fail | agent
	at     time.Time
	bytes  int64
	newest bool // newest of its group: never deleted by the size cap
}

// PruneWithCap applies the age policy, then enforces maxMB by deleting oldest pass runs,
// then oldest agent runs, then oldest failures (the newest entry of every scenario and
// feature is always kept). dry only reports.
func (s *Store) PruneWithCap(passDays, failDays, maxMB int, dry bool) (PruneReport, error) {
	var rep PruneReport
	if !dry {
		if err := s.Prune(passDays, failDays); err != nil {
			return rep, err
		}
	}
	dirs := s.collectDirs()
	for _, d := range dirs {
		rep.TotalBytes += d.bytes
		if d.kind == "agent" {
			rep.AgentDirs++
		} else {
			rep.RunDirs++
		}
	}
	cap := int64(maxMB) * 1024 * 1024
	if maxMB <= 0 || rep.TotalBytes <= cap {
		return rep, nil
	}
	rank := map[string]int{"pass": 0, "agent": 1, "fail": 2}
	sort.Slice(dirs, func(i, j int) bool {
		if rank[dirs[i].kind] != rank[dirs[j].kind] {
			return rank[dirs[i].kind] < rank[dirs[j].kind]
		}
		return dirs[i].at.Before(dirs[j].at)
	})
	for _, d := range dirs {
		if rep.TotalBytes <= cap {
			break
		}
		if d.newest {
			continue
		}
		if !dry {
			if err := os.RemoveAll(d.path); err != nil {
				continue
			}
			_ = os.Remove(filepath.Dir(d.path)) // drop empty parent
		}
		rep.DeletedDirs++
		rep.FreedBytes += d.bytes
		rep.TotalBytes -= d.bytes
		if d.kind == "agent" {
			rep.AgentDirs--
		} else {
			rep.RunDirs--
		}
	}
	return rep, nil
}

// Usage returns the evidence footprint for status output.
func (s *Store) Usage() (bytes int64, runDirs, agentDirs int) {
	for _, d := range s.collectDirs() {
		bytes += d.bytes
		if d.kind == "agent" {
			agentDirs++
		} else {
			runDirs++
		}
	}
	return
}

func (s *Store) collectDirs() []evidenceDir {
	var out []evidenceDir
	walk := func(root, kind string) {
		groups, _ := os.ReadDir(root)
		for _, g := range groups {
			if !g.IsDir() {
				continue
			}
			gDir := filepath.Join(root, g.Name())
			entries, _ := os.ReadDir(gDir)
			var latest *evidenceDir
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(gDir, e.Name())
				d := evidenceDir{path: p, group: g.Name(), kind: kind, at: dirTime(p, e.Name()), bytes: dirSize(p)}
				if kind != "agent" && !IsPass(p) {
					d.kind = "fail"
				}
				out = append(out, d)
				if latest == nil || d.at.After(latest.at) {
					latest = &out[len(out)-1]
				}
			}
			if latest != nil {
				latest.newest = true
			}
		}
	}
	walk(s.RunsDir(), "pass")
	walk(s.AgentRoot(), "agent")
	return out
}

func dirSize(p string) int64 {
	var n int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}
