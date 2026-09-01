package ingest

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// gitAdapter (generic-git) turns first-parent commits of one branch into FeatureEvents.
// Commits are grouped by the issue key in their subject (PROJ-123) or, without one, by their SHA.
type gitAdapter struct {
	st           *store.Store
	project      string
	repo         string // absolute path; also the cursor key in the repositories table
	branch       string
	fetch        bool
	pathPrefixes []string
	routeMap     []config.RouteMapEntry
	historyLimit int
	issueKeyRe   *regexp.Regexp
}

func newGitAdapter(cfg *config.Config, st *store.Store) *gitAdapter {
	g := &gitAdapter{
		st:           st,
		project:      cfg.Project.ID,
		repo:         cfg.Abs(cfg.Project.Repo),
		branch:       cfg.Discovery.Branch,
		fetch:        cfg.Discovery.Fetch,
		pathPrefixes: cfg.Discovery.PathPrefixes,
		routeMap:     cfg.Discovery.RouteMap,
		historyLimit: cfg.Discovery.HistoryLimit,
	}
	if g.branch == "" {
		g.branch = "HEAD"
	}
	if g.historyLimit <= 0 {
		g.historyLimit = 5 // same default as config.applyDefaults
	}
	// Validated at config load; fall back rather than panic if this adapter is
	// constructed from a hand-built Config in a test.
	re, err := regexp.Compile(pick(cfg.Discovery.IssueKeyPattern, config.DefaultIssueKeyPattern))
	if err != nil {
		re = regexp.MustCompile(config.DefaultIssueKeyPattern)
	}
	g.issueKeyRe = re
	return g
}

func (g *gitAdapter) Name() string { return AdapterGenericGit }

// Poll returns features shipped since the stored cursor and moves the cursor to the branch tip.
func (g *gitAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	if g.fetch && strings.HasPrefix(g.branch, "origin/") {
		if _, err := g.git(ctx, "fetch", "--quiet", "origin", strings.TrimPrefix(g.branch, "origin/")); err != nil {
			log.Printf("ingest[%s]: fetch failed, using local refs: %v", g.Name(), err)
		}
	}
	cursor, err := g.st.GetRepoCursor(ctx, g.project, g.repo)
	if err != nil {
		return nil, err
	}
	// Resolve the tip before logging so the cursor never skips a commit that lands mid-poll.
	tip, err := g.git(ctx, "rev-parse", "--verify", g.branch+"^{commit}")
	if err != nil {
		return nil, err
	}
	tip = strings.TrimSpace(tip)

	var commits []commit
	if cursor != "" {
		commits, err = g.log(ctx, cursor+".."+tip)
		if err != nil {
			log.Printf("ingest[%s]: cursor %s unusable, reading last %d commits instead: %v", g.Name(), cursor, g.historyLimit, err)
			cursor = ""
		}
	}
	if cursor == "" {
		if commits, err = g.log(ctx, tip, "-n", strconv.Itoa(g.historyLimit)); err != nil {
			return nil, err
		}
	}
	if err := g.st.SetRepoCursor(ctx, g.project, g.repo, g.branch, tip); err != nil {
		return nil, err
	}
	return g.events(commits), nil
}

// History returns the newest limit features of the recent branch history without touching the cursor.
func (g *gitAdapter) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	window := max(limit*20, 200)
	commits, err := g.log(ctx, g.branch, "-n", strconv.Itoa(window))
	if err != nil {
		return nil, err
	}
	return newest(g.events(commits), limit), nil
}

// commit is one entry of `git log --name-only`.
type commit struct {
	sha     string
	at      time.Time
	subject string
	paths   []string
}

// logFormat prints one header line per commit: sha, committer date (ISO 8601) and subject joined by fieldSep.
const (
	logFormat = "%H%x1f%cI%x1f%s"
	fieldSep  = "\x1f"
)

// log runs git log with the parser's format. rev is a commit, ref or range; extra flags are added as given.
func (g *gitAdapter) log(ctx context.Context, rev string, extra ...string) ([]commit, error) {
	// core.quotePath=false keeps non-ASCII paths (Korean file names) literal instead of octal-escaped.
	args := []string{"-c", "core.quotePath=false", "log", "--first-parent", "--no-show-signature",
		"--format=" + logFormat, "--name-only"}
	args = append(args, extra...)
	args = append(args, rev, "--")
	out, err := g.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseLog(out)
}

// parseLog reads git log output: a header line (sha / date / subject joined by fieldSep) followed by
// the commit's changed paths, one per line. Commits stay newest-first as git prints them.
func parseLog(out string) ([]commit, error) {
	var commits []commit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if !strings.Contains(line, fieldSep) {
			if len(commits) == 0 {
				return nil, fmt.Errorf("ingest: git log path %q before any commit header", line)
			}
			last := &commits[len(commits)-1]
			last.paths = append(last.paths, line)
			continue
		}
		parts := strings.SplitN(line, fieldSep, 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("ingest: malformed git log header %q", line)
		}
		at, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			return nil, fmt.Errorf("ingest: commit %s: bad committer date %q: %w", parts[0], parts[1], err)
		}
		commits = append(commits, commit{sha: parts[0], at: at, subject: parts[2]})
	}
	return commits, nil
}

// events keeps commits touching a watched path prefix and merges them per feature id.
// Input is newest-first (git order); output is oldest-first. The newest commit of a feature
// supplies sha, time and summary; changed paths and routes are the union over its commits.
func (g *gitAdapter) events(commits []commit) []model.FeatureEvent {
	var out []model.FeatureEvent // newest-first while merging
	index := map[string]int{}
	for _, c := range commits {
		if !g.watched(c.paths) {
			continue
		}
		id := FeatureIDFromSubject(g.issueKeyRe, c.subject, c.sha)
		i, seen := index[id]
		if !seen {
			i = len(out)
			index[id] = i
			out = append(out, model.FeatureEvent{FeatureID: id, ShippedSHA: c.sha, ShippedAt: c.at, Summary: c.subject})
		}
		out[i].ChangedPaths = append(out[i].ChangedPaths, c.paths...)
		out[i].Routes = append(out[i].Routes, g.routesFor(c.paths)...)
	}
	for i := range out {
		out[i].ChangedPaths = uniqueSorted(out[i].ChangedPaths)
		out[i].Routes = uniqueSorted(out[i].Routes)
		stamp(&out[i], g.Name())
	}
	slices.Reverse(out)
	return out
}

// watched reports whether any path starts with a configured prefix; no prefixes means every commit counts.
func (g *gitAdapter) watched(paths []string) bool {
	if len(g.pathPrefixes) == 0 {
		return true
	}
	for _, p := range paths {
		for _, prefix := range g.pathPrefixes {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
	}
	return false
}

// routesFor maps changed paths to routes through discovery.route_map; duplicates are removed by the caller.
func (g *gitAdapter) routesFor(paths []string) []string {
	var routes []string
	for _, p := range paths {
		for _, m := range g.routeMap {
			if strings.HasPrefix(p, m.PathPrefix) {
				routes = append(routes, m.Route)
			}
		}
	}
	return routes
}

func pick(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// FeatureIDFromSubject derives a feature id from a commit subject: the first
// issue key matched by re (upper-cased) or "commit-<7-char sha>" when the
// subject has none. A nil re uses config.DefaultIssueKeyPattern.
func FeatureIDFromSubject(re *regexp.Regexp, subject, sha string) string {
	if re == nil {
		re = regexp.MustCompile(config.DefaultIssueKeyPattern)
	}
	if key := re.FindString(subject); key != "" {
		return strings.ToUpper(key)
	}
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return "commit-" + sha
}

var urlCredentialRe = regexp.MustCompile(`://[^/@\s]+@`)

// git runs `git -C <repo> args...` and returns stdout. Error text has any URL credentials masked.
func (g *gitAdapter) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // never block the poller on a credential prompt
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := urlCredentialRe.ReplaceAllString(strings.TrimSpace(stderr.String()), "://***@")
		if msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
