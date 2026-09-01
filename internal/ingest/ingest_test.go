package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testRepo is a throwaway git repository whose commits are one minute apart, in order.
type testRepo struct {
	t   *testing.T
	dir string
	n   int
}

func newTestRepo(t *testing.T, dir string) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &testRepo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	return r
}

// date is the timestamp of the last commit.
func (r *testRepo) date() time.Time { return time.Date(2026, 9, 1, 0, r.n, 0, 0, time.UTC) }

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	base := []string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false"}
	cmd := exec.Command("git", append(base, args...)...)
	date := r.date().Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes the given files (content = subject) and commits them; returns the new sha.
func (r *testRepo) commit(subject string, files ...string) string {
	r.t.Helper()
	r.n++
	for _, f := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(subject+"\n"), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", subject)
	return r.git("rev-parse", "HEAD")
}

func gitConfig(tmp, repo string) *config.Config {
	cfg := &config.Config{BaseDir: tmp}
	cfg.Project.ID = "p"
	cfg.Project.Repo = repo
	cfg.Discovery.Adapter = "generic-git"
	cfg.Discovery.Branch = "main"
	cfg.Discovery.PathPrefixes = []string{"src/components/entry/"}
	cfg.Discovery.RouteMap = []config.RouteMapEntry{{PathPrefix: "src/components/entry/", Route: "/lms-web/training-entry"}}
	cfg.Discovery.HistoryLimit = 3
	return cfg
}

func ids(events []model.FeatureEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.FeatureID)
	}
	return out
}

func cursorOf(t *testing.T, st *store.Store, repo string) string {
	t.Helper()
	c, err := st.GetRepoCursor(context.Background(), "p", repo)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGitPollFiltersMergesAndAdvancesCursor(t *testing.T) {
	tmp := t.TempDir()
	repo := newTestRepo(t, filepath.Join(tmp, "repo"))
	repo.commit("chore: init", "README.md")
	repo.commit("feat: PROJ-100 entry popup", "src/components/entry/Popup.vue")
	shaClose := repo.commit("fix: PROJ-100 popup close", "src/components/entry/Close.vue", "docs/entry.md")
	closedAt := repo.date()
	shaDocs := repo.commit("docs: unrelated", "docs/other.md")

	st := openStore(t, tmp)
	ad, err := New(gitConfig(tmp, repo.dir), st)
	if err != nil {
		t.Fatal(err)
	}
	if ad.Name() != "generic-git" {
		t.Fatalf("name = %q", ad.Name())
	}
	ctx := context.Background()

	// (a)+(b): only the prefix-touching commits, merged by Jira key, newest commit wins.
	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("first poll: want 1 event, got %+v", events)
	}
	ev := events[0]
	if ev.FeatureID != "PROJ-100" || ev.ShippedSHA != shaClose || !ev.ShippedAt.Equal(closedAt) {
		t.Fatalf("merged event = %+v (want PROJ-100 @ %s)", ev, shaClose)
	}
	if ev.Summary != "fix: PROJ-100 popup close" || ev.Source != "generic-git" || ev.Status != "shipped" {
		t.Fatalf("summary/source/status = %q %q %q", ev.Summary, ev.Source, ev.Status)
	}
	wantPaths := []string{"docs/entry.md", "src/components/entry/Close.vue", "src/components/entry/Popup.vue"}
	if !slices.Equal(ev.ChangedPaths, wantPaths) {
		t.Fatalf("changed paths = %v, want %v", ev.ChangedPaths, wantPaths)
	}
	if !slices.Equal(ev.Routes, []string{"/lms-web/training-entry"}) {
		t.Fatalf("routes = %v", ev.Routes)
	}
	if c := cursorOf(t, st, repo.dir); c != shaDocs {
		t.Fatalf("cursor = %s, want tip %s", c, shaDocs)
	}

	// (c): nothing new on the second poll.
	if events, err = ad.Poll(ctx); err != nil || len(events) != 0 {
		t.Fatalf("second poll: %v %+v", err, events)
	}

	// (c)+(e): a new matching commit without a Jira key is returned exactly once.
	shaLoader := repo.commit("refactor entry loader", "src/components/entry/Loader.vue")
	shaBump := repo.commit("chore: bump", "package.json")
	if events, err = ad.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].FeatureID != "commit-"+shaLoader[:7] || events[0].ShippedSHA != shaLoader {
		t.Fatalf("third poll = %+v, want commit-%s", events, shaLoader[:7])
	}
	if !slices.Equal(events[0].Routes, []string{"/lms-web/training-entry"}) {
		t.Fatalf("routes = %v", events[0].Routes)
	}
	if c := cursorOf(t, st, repo.dir); c != shaBump {
		t.Fatalf("cursor = %s, want tip %s", c, shaBump)
	}
}

func TestGitOldestFirstAndHistoryKeepsCursor(t *testing.T) {
	tmp := t.TempDir()
	repo := newTestRepo(t, filepath.Join(tmp, "repo"))
	repo.commit("feat: PROJ-1 a", "src/components/entry/a.vue")
	repo.commit("feat: PROJ-2 b", "src/components/entry/b.vue")
	repo.commit("feat: PROJ-3 c", "src/components/entry/c.vue")
	shaDocs := repo.commit("docs", "docs/x.md")

	st := openStore(t, tmp)
	ad, err := New(gitConfig(tmp, repo.dir), st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// (d): History returns the newest 2 features oldest-first and leaves the cursor alone.
	hist, err := ad.History(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hist); !slices.Equal(got, []string{"PROJ-2", "PROJ-3"}) {
		t.Fatalf("history(2) = %v", got)
	}
	if c := cursorOf(t, st, repo.dir); c != "" {
		t.Fatalf("history moved cursor to %s", c)
	}
	all, err := ad.History(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !slices.Equal(got, []string{"PROJ-1", "PROJ-2", "PROJ-3"}) {
		t.Fatalf("history(10) = %v", got)
	}

	// Poll with an empty cursor reads the last HistoryLimit (3) commits, oldest-first.
	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); !slices.Equal(got, []string{"PROJ-2", "PROJ-3"}) {
		t.Fatalf("poll = %v", got)
	}
	if c := cursorOf(t, st, repo.dir); c != shaDocs {
		t.Fatalf("cursor = %s, want %s", c, shaDocs)
	}
	if _, err := ad.History(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if c := cursorOf(t, st, repo.dir); c != shaDocs {
		t.Fatalf("history moved cursor to %s", c)
	}
}

func TestFeatureIDFromSubject(t *testing.T) {
	cases := []struct{ subject, sha, want string }{
		{"feat/PROJ-783 entry popup", "0123456789abcdef", "PROJ-783"},
		{"PROJ-1 then PROJ-2", "0123456789abcdef", "PROJ-1"},
		{"no key here", "0123456789abcdef", "commit-0123456"},
		{"short sha", "abc", "commit-abc"},
		// The default pattern is case-sensitive on purpose: these must NOT be
		// mistaken for issue keys.
		{"fix: proj-12 lower-case key", "0123456789abcdef", "commit-0123456"},
		{"fix utf-8 decoding", "0123456789abcdef", "commit-0123456"},
	}
	for _, c := range cases {
		if got := FeatureIDFromSubject(nil, c.subject, c.sha); got != c.want {
			t.Errorf("FeatureIDFromSubject(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}

// A team that writes keys in lower case can opt in with their own pattern.
func TestFeatureIDFromSubjectCustomPattern(t *testing.T) {
	re := regexp.MustCompile(`(?i)\bproj-\d+`)
	if got := FeatureIDFromSubject(re, "fix: proj-12 lower-case key", "0123456789abcdef"); got != "PROJ-12" {
		t.Errorf("custom pattern = %q, want PROJ-12", got)
	}
}

func TestParseLog(t *testing.T) {
	out := "aaa\x1f2026-09-01T09:02:00+09:00\x1ffeat: x\n\nsrc/a.vue\nsrc/b.vue\n" +
		"bbb\x1f2026-09-01T00:01:00Z\x1finit\n\nREADME.md\n"
	commits, err := parseLog(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].sha != "aaa" || commits[1].sha != "bbb" {
		t.Fatalf("commits = %+v", commits)
	}
	if !slices.Equal(commits[0].paths, []string{"src/a.vue", "src/b.vue"}) || !slices.Equal(commits[1].paths, []string{"README.md"}) {
		t.Fatalf("paths = %v / %v", commits[0].paths, commits[1].paths)
	}
	if commits[0].subject != "feat: x" || !commits[0].at.Equal(time.Date(2026, 9, 1, 0, 2, 0, 0, time.UTC)) {
		t.Fatalf("header = %+v", commits[0])
	}
	if _, err := parseLog("orphan/path\n"); err == nil {
		t.Fatal("path before header must fail")
	}
}

func TestNewRejectsUnknownAdapter(t *testing.T) {
	cfg := &config.Config{}
	cfg.Discovery.Adapter = "svn"
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("want error for unknown adapter")
	}
}

func TestFileAdapter(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "features")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("remedy.yaml", `feature_id: remedy-page-order
status: shipped
shipped_sha: abc123
shipped_at: 2026-09-01T03:20:00Z
changed_paths:
  - src/pages/remedy/OctoPlayer.vue
routes:
  - /remedy
summary: Remedy display ordering changed.
`)
	write("quoted.yml", "feature_id: q-1\nshipped_at: \"2026-08-31T00:00:00Z\"\nsummary: no sha\n")
	write("broken.yaml", "summary: missing feature_id\n")
	write("notes.txt", "feature_id: ignored\n")

	cfg := &config.Config{BaseDir: tmp}
	cfg.Project.ID = "p"
	cfg.Discovery.Adapter = "file"
	cfg.Discovery.FeaturesDir = "features"
	st := openStore(t, tmp)
	ad, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if ad.Name() != "file" {
		t.Fatalf("name = %q", ad.Name())
	}
	ctx := context.Background()

	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); !slices.Equal(got, []string{"q-1", "remedy-page-order"}) {
		t.Fatalf("poll = %v", got)
	}
	remedy := events[1]
	if remedy.ShippedSHA != "abc123" || !remedy.ShippedAt.Equal(time.Date(2026, 9, 1, 3, 20, 0, 0, time.UTC)) {
		t.Fatalf("remedy = %+v", remedy)
	}
	if !slices.Equal(remedy.Routes, []string{"/remedy"}) || !slices.Equal(remedy.ChangedPaths, []string{"src/pages/remedy/OctoPlayer.vue"}) {
		t.Fatalf("remedy lists = %v %v", remedy.Routes, remedy.ChangedPaths)
	}
	if remedy.Source != "file" || remedy.Status != "shipped" || remedy.Summary != "Remedy display ordering changed." {
		t.Fatalf("remedy meta = %+v", remedy)
	}
	if q := events[0]; q.ShippedSHA != "" || !q.ShippedAt.Equal(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("quoted = %+v", q)
	}
	for key, want := range map[string]string{"ingest:file:remedy-page-order": "abc123", "ingest:file:q-1": "-"} {
		if v, _ := st.GetState(ctx, key); v != want {
			t.Fatalf("marker %s = %q, want %q", key, v, want)
		}
	}

	if events, err = ad.Poll(ctx); err != nil || len(events) != 0 {
		t.Fatalf("second poll: %v %+v", err, events)
	}
	hist, err := ad.History(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hist); !slices.Equal(got, []string{"remedy-page-order"}) {
		t.Fatalf("history(1) = %v", got)
	}
	if hist, _ = ad.History(ctx, 5); len(hist) != 2 {
		t.Fatalf("history(5) = %v", ids(hist))
	}

	// A re-ship with a new sha is delivered again.
	write("remedy.yaml", "feature_id: remedy-page-order\nshipped_sha: def456\nshipped_at: 2026-09-02T00:00:00Z\n")
	if events, err = ad.Poll(ctx); err != nil || len(events) != 1 || events[0].ShippedSHA != "def456" {
		t.Fatalf("re-ship poll: %v %+v", err, events)
	}

	// A missing directory is not an error.
	cfg.Discovery.FeaturesDir = "nope"
	ad, _ = New(cfg, st)
	if events, err = ad.Poll(ctx); err != nil || events != nil {
		t.Fatalf("missing dir: %v %+v", err, events)
	}
}

func TestSDLCAdapter(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "repo", ".sdlc", "work")
	write := func(item, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(work, item), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, item, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("proj-999-thing", "STATE.md", `# proj-999-thing

- **Status**: shipped
- Phase: DONE
sha: deadbeef
Summary: Thing shipped
routes: /a, /b
changed_paths: src/x.vue
Note: this line is prose and must be ignored
`)
	write("wip-item", "STATE.md", "status: in-progress\nsha: 1111\n")
	write("no-sha", "state.md", "phase: SHIP\n")
	write("dated", "state.md", "status: SHIPPED\nshipped_sha: 2222\nfeature_id: PROJ-7\nshipped_at: 2026-01-01T00:00:00Z\n")

	cfg := &config.Config{BaseDir: tmp}
	cfg.Project.ID = "p"
	cfg.Project.Repo = "repo"
	cfg.Discovery.Adapter = "sdlc-kit"
	st := openStore(t, tmp)
	ad, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if ad.Name() != "sdlc-kit" {
		t.Fatalf("name = %q", ad.Name())
	}
	ctx := context.Background()

	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(events)
	slices.Sort(got)
	if !slices.Equal(got, []string{"PROJ-7", "proj-999-thing"}) {
		t.Fatalf("poll = %v", got)
	}
	byID := map[string]model.FeatureEvent{}
	for _, e := range events {
		byID[e.FeatureID] = e
	}
	thing := byID["proj-999-thing"]
	if thing.ShippedSHA != "deadbeef" || thing.Summary != "Thing shipped" || thing.ShippedAt.IsZero() {
		t.Fatalf("thing = %+v", thing)
	}
	if !slices.Equal(thing.Routes, []string{"/a", "/b"}) || !slices.Equal(thing.ChangedPaths, []string{"src/x.vue"}) {
		t.Fatalf("thing lists = %v %v", thing.Routes, thing.ChangedPaths)
	}
	if thing.Source != "sdlc-kit" || thing.Status != "shipped" {
		t.Fatalf("thing meta = %+v", thing)
	}
	if d := byID["PROJ-7"]; d.ShippedSHA != "2222" || d.Summary != "PROJ-7" || !d.ShippedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("dated = %+v", d)
	}
	if v, _ := st.GetState(ctx, "ingest:sdlc:proj-999-thing"); v != "deadbeef" {
		t.Fatalf("marker = %q", v)
	}

	if events, err = ad.Poll(ctx); err != nil || len(events) != 0 {
		t.Fatalf("second poll: %v %+v", err, events)
	}
	hist, err := ad.History(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(hist); !slices.Equal(got, []string{"PROJ-7", "proj-999-thing"}) {
		t.Fatalf("history = %v (oldest-first by shipped_at)", got)
	}

	cfg.Project.Repo = "missing"
	ad, _ = New(cfg, st)
	if events, err = ad.Poll(ctx); err != nil || events != nil {
		t.Fatalf("missing root: %v %+v", err, events)
	}
}

func TestGitPollFallsBackWhenCursorIsGone(t *testing.T) {
	tmp := t.TempDir()
	repo := newTestRepo(t, filepath.Join(tmp, "repo"))
	repo.commit("feat: PROJ-5 a", "src/components/entry/a.vue")
	tip := repo.commit("feat: PROJ-6 b", "src/components/entry/b.vue")

	st := openStore(t, tmp)
	ctx := context.Background()
	// Simulate a rewritten branch: the stored cursor no longer resolves.
	if err := st.SetRepoCursor(ctx, "p", repo.dir, "main", strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	ad, err := New(gitConfig(tmp, repo.dir), st)
	if err != nil {
		t.Fatal(err)
	}
	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); !slices.Equal(got, []string{"PROJ-5", "PROJ-6"}) {
		t.Fatalf("fallback poll = %v", got)
	}
	if c := cursorOf(t, st, repo.dir); c != tip {
		t.Fatalf("cursor = %s, want %s", c, tip)
	}
}

func TestGitPollFetchesRemoteBranch(t *testing.T) {
	tmp := t.TempDir()
	upstream := newTestRepo(t, filepath.Join(tmp, "upstream"))
	upstream.commit("feat: PROJ-8 first", "src/components/entry/a.vue")
	upstream.git("clone", "-q", upstream.dir, filepath.Join(tmp, "clone"))
	clone := filepath.Join(tmp, "clone")

	cfg := gitConfig(tmp, clone)
	cfg.Discovery.Branch = "origin/main"
	cfg.Discovery.Fetch = true
	st := openStore(t, tmp)
	ad, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if events, err := ad.Poll(ctx); err != nil || !slices.Equal(ids(events), []string{"PROJ-8"}) {
		t.Fatalf("first poll: %v %v", err, ids(events))
	}

	// A commit that only exists upstream must be picked up through fetch.
	sha := upstream.commit("feat: PROJ-9 second", "src/components/entry/b.vue")
	events, err := ad.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(events); !slices.Equal(got, []string{"PROJ-9"}) || events[0].ShippedSHA != sha {
		t.Fatalf("poll after upstream commit = %v %+v", got, events)
	}
	if c := cursorOf(t, st, clone); c != sha {
		t.Fatalf("cursor = %s, want %s", c, sha)
	}
}
