// Package config loads vigil.yaml (PRD §19) with environment substitution.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version int `yaml:"version"`

	Runtime struct {
		Mode string `yaml:"mode"` // local | distributed
	} `yaml:"runtime"`

	Project struct {
		ID   string `yaml:"id"`
		Repo string `yaml:"repo"` // git repo path of the shipped application
	} `yaml:"project"`

	Target struct {
		BaseURL      string   `yaml:"base_url"`
		AllowedHosts []string `yaml:"allowed_hosts"`
		// URLFile lists deployed URLs (one per line, `<label> | <url>` or bare
		// `<url>`, `#` comments). The first URL is the primary target: its origin
		// becomes base_url when base_url is empty and its host is allowlisted.
		// Defaults to "url.md" next to the config when that file exists.
		URLFile string `yaml:"url_file"`
		// DefaultEnv names the environment runs use unless --env is given. Agent,
		// cadence, soak and impacted work always use it. Defaults to the single
		// implicit "default" environment (or the only configured one).
		DefaultEnv string `yaml:"default_env"`
		// Environments maps a name to a deployed target. Empty = one implicit
		// environment "default" synthesised from base_url / allowed_hosts.
		Environments map[string]Environment `yaml:"environments"`
	} `yaml:"target"`

	// Targets is parsed from Target.URLFile (not from YAML). Targets[0] is primary.
	Targets []Target `yaml:"-"`

	Discovery struct {
		Adapter string `yaml:"adapter"` // legacy single adapter: generic-git | sdlc-kit | file (honoured when adapters is empty)
		// Adapters lists every source polled together (generic-git | sdlc-kit | file | jira | loki | exec).
		Adapters []string `yaml:"adapters"`
		Branch   string   `yaml:"branch"` // generic-git: remote branch to watch, e.g. origin/staging
		// IssueKeyPattern groups commits by the issue key in their subject.
		// Uppercase-only by default so "utf-8" in a subject is not read as a key;
		// set an explicit (?i) pattern if your team writes keys in lower case.
		IssueKeyPattern string          `yaml:"issue_key_pattern"`
		Fetch           bool            `yaml:"fetch"`
		PathPrefixes    []string        `yaml:"path_prefixes"` // only commits touching these prefixes become features
		RouteMap        []RouteMapEntry `yaml:"route_map"`
		FeaturesDir     string          `yaml:"features_dir"` // file adapter: directory of FeatureEvent yaml files
		PollInterval    Duration        `yaml:"poll_interval"`
		HistoryLimit    int             `yaml:"history_limit"` // import --history rate limit
		Jira            JiraDiscovery   `yaml:"jira"`
		Loki            LokiDiscovery   `yaml:"loki"`
		Exec            ExecDiscovery   `yaml:"exec"`
	} `yaml:"discovery"`

	Deployment struct {
		Readiness struct {
			Strategy       string   `yaml:"strategy"` // delay | asset_version | version_endpoint
			DelayAfterShip Duration `yaml:"delay_after_ship"`
			Endpoint       string   `yaml:"endpoint"`   // version_endpoint: URL returning SHA/version
			AssetPage      string   `yaml:"asset_page"` // asset_version: page whose asset ?v= changes on deploy
			MaxWait        Duration `yaml:"max_wait"`   // after this, DEPLOYMENT_UNKNOWN + delay policy applies
		} `yaml:"readiness"`
	} `yaml:"deployment"`

	Browser struct {
		Primary    string `yaml:"primary"`  // lightpanda
		Fallback   string `yaml:"fallback"` // chromium
		Lightpanda struct {
			Binary string `yaml:"binary"`
			Host   string `yaml:"host"`
			Port   int    `yaml:"port"`
		} `yaml:"lightpanda"`
		Chromium struct {
			Binary   string `yaml:"binary"`
			Headless bool   `yaml:"headless"`
		} `yaml:"chromium"`
		StepTimeout Duration `yaml:"step_timeout"`
		RunTimeout  Duration `yaml:"run_timeout"`
		// IdleStop: a browser this process launched is shut down after this long
		// without a run, and relaunched on the next one. A resident Chromium plus
		// Lightpanda hold several hundred MB, which is wasted outside active hours.
		IdleStop Duration `yaml:"idle_stop"`
	} `yaml:"browser"`

	Workers struct {
		Functional int `yaml:"functional"`
		Chromium   int `yaml:"chromium"`
	} `yaml:"workers"`

	State struct {
		Type   string `yaml:"type"` // sqlite | postgres
		Path   string `yaml:"path"`
		DSNEnv string `yaml:"dsn_env"`
	} `yaml:"state"`

	Evidence struct {
		Type           string `yaml:"type"` // local | s3
		Dir            string `yaml:"dir"`
		Bucket         string `yaml:"bucket"`
		RetainPassDays int    `yaml:"retain_pass_days"`
		RetainFailDays int    `yaml:"retain_fail_days"`
		// ChromiumCapture: per-deploy (default) re-runs a scenario that passed on Lightpanda once per
		// deployment marker on Chromium so QA evidence has a real rendered screenshot; off disables it.
		ChromiumCapture string `yaml:"chromium_capture"`
		// MaxTotalMB caps the evidence directory; oldest pass runs, then old agent runs, then
		// old failures are deleted until under the cap (newest per scenario/feature kept).
		MaxTotalMB int `yaml:"max_total_mb"`
		// RetainRunsDays prunes run rows from the database (incident-linked and newest-per-scenario kept).
		RetainRunsDays int `yaml:"retain_runs_days"`
	} `yaml:"evidence"`

	Budget struct {
		BrowserMinutesPerHour  int `yaml:"browser_minutes_per_hour"`
		ChromiumMinutesPerHour int `yaml:"chromium_minutes_per_hour"`
		AgentTasksPerHour      int `yaml:"agent_tasks_per_hour"`
		// SupervisorTasksPerHour is separate from AgentTasksPerHour so the slow
		// coverage loop cannot starve discover/verify/repair.
		SupervisorTasksPerHour int `yaml:"supervisor_tasks_per_hour"`
	} `yaml:"budget"`

	Policy struct {
		AllowDestructive bool `yaml:"allow_destructive"`
		SoakPasses       int  `yaml:"soak_passes"`      // clean runs before ACTIVE
		QuarantineAfter  int  `yaml:"quarantine_after"` // consecutive flakes before QUARANTINED
		RetryOnFail      int  `yaml:"retry_on_fail"`    // cheap safe retries before classify
		// AgentFixAttempts: how many times the Browser Agent may rewrite a
		// scenario whose validation run failed, before it goes to a human.
		// 1 = the old behaviour (one shot, then NEEDS_REVIEW).
		AgentFixAttempts int `yaml:"agent_fix_attempts"`
		// ObservationOracle: needs_review (PRD default: observation alone is not an oracle) | soak
		ObservationOracle string `yaml:"observation_oracle"`
		// StructuralDupThreshold: Jaccard similarity of major actions above which a candidate is DUPLICATE.
		StructuralDupThreshold float64 `yaml:"structural_dup_threshold"`
	} `yaml:"policy"`

	Supervisor Supervisor `yaml:"supervisor"`

	Schedule struct {
		Soak           Duration `yaml:"soak"`
		P0             Duration `yaml:"p0"`
		P1             Duration `yaml:"p1"`
		P2             Duration `yaml:"p2"`
		FailureBackoff Duration `yaml:"failure_backoff"`
		Tick           Duration `yaml:"tick"`
		// ActiveHours pauses cadence enqueueing outside a weekly window.
		// Omitted or disabled = 24/7 (previous behaviour).
		ActiveHours ActiveHours `yaml:"active_hours"`
		// DailyAt is the local clock time ("HH:MM", active_hours.tz or machine
		// local) at which approved scripts (cadence: daily) run once a day.
		DailyAt string `yaml:"daily_at"`
	} `yaml:"schedule"`

	// Jira is the write-back side (approval comments); discovery.jira is the read side.
	Jira Jira `yaml:"jira"`

	Agent struct {
		Provider string `yaml:"provider"` // pi
		Model    string `yaml:"model"`    // zai/glm-5.3-flash (legacy single model; used when models is empty)
		Thinking string `yaml:"thinking"` // high (default thinking for entries without :level)
		// Models is the ordered fallback chain, entry = provider/model[:thinking].
		// Empty = the single legacy Model with Thinking.
		Models []string `yaml:"models"`
		// ModelCooldown: a model that answered 429/quota/auth/5xx is skipped this long.
		ModelCooldown Duration          `yaml:"model_cooldown"`
		Timeout       Duration          `yaml:"timeout"`
		Extension     string            `yaml:"extension"` // path to pi extension (agent_browser tool)
		EnvMap        map[string]string `yaml:"env_map"`   // e.g. ZAI_API_KEY: Z_AI_API_KEY
		Retries       int               `yaml:"retries"`
		Backoff       Duration          `yaml:"backoff"`
		MaxTurns      int               `yaml:"max_turns"`
		Workdir       string            `yaml:"workdir"`
		// Sandbox: auto (use nono when on PATH) | nono | none
		Sandbox string `yaml:"sandbox"`
		// SandboxNetworkFilter enables nono's proxy domain allowlist (supervised mode, more RAM).
		SandboxNetworkFilter bool `yaml:"sandbox_network_filter"`
		// MaxScenariosPerTask bounds candidate output per agent task.
		MaxScenariosPerTask int `yaml:"max_scenarios_per_task"`
		// ModelRetries / ModelRetryDelay: in-conversation retries on 429/5xx (pi retry.maxRetries / baseDelayMs).
		ModelRetries int `yaml:"model_retries"`
		// Continuations: resumes with a fresh tool budget before WRAP UP (long manual requests).
		Continuations   int      `yaml:"continuations"`
		ModelRetryDelay Duration `yaml:"model_retry_delay"`
		// DomainFile: optional markdown of domain rules (relative to the config dir)
		// injected into every agent task prompt, bounded to 12 KB.
		DomainFile string `yaml:"domain_file"`
	} `yaml:"agent"`

	Personas map[string]Persona `yaml:"personas"`

	Paths struct {
		Scenarios string `yaml:"scenarios"`
		Flows     string `yaml:"flows"`
	} `yaml:"paths"`

	// BaseDir is the directory containing the config file (not from YAML).
	BaseDir string `yaml:"-"`
}

// ImplicitEnvName is the environment synthesised from target.base_url when
// target.environments is empty.
const ImplicitEnvName = "default"

// Environment is one deployed target (target.environments.<name>).
type Environment struct {
	Name         string   `yaml:"-"`
	BaseURL      string   `yaml:"base_url"`
	AllowedHosts []string `yaml:"allowed_hosts"`
	// AssetPage is the per-environment deploy marker page (asset_version strategy); optional.
	AssetPage string `yaml:"asset_page"`
	// ReadOnly refuses every scenario whose mutation is not read-only.
	ReadOnly bool `yaml:"read_only"`
}

// HostAllowed reports whether host (without port) is in the environment allowlist.
func (e Environment) HostAllowed(host string) bool {
	return hostAllowed(e.AllowedHosts, host)
}

// URLAllowed reports whether an absolute URL stays inside the environment allowlist.
func (e Environment) URLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return e.HostAllowed(u.Hostname())
}

// Discovery adapter names accepted in discovery.adapter / discovery.adapters.
const (
	AdapterGenericGit = "generic-git"
	AdapterFile       = "file"
	AdapterSDLCKit    = "sdlc-kit"
	AdapterJira       = "jira"
	AdapterLoki       = "loki"
	AdapterExec       = "exec"
)

// DefaultLokiSignature is the signature regexp used when discovery.loki.signature is empty.
const DefaultLokiSignature = `(?i)(exception|error)[^\n]{0,120}`

// RouteHint maps a case-insensitive keyword found in an issue's summary/description to a route.
type RouteHint struct {
	Match string `yaml:"match"`
	Route string `yaml:"route"`
}

// JiraDiscovery polls a Jira JQL through the acli binary (discovery.jira).
type JiraDiscovery struct {
	CLI          string      `yaml:"cli"`
	JQL          string      `yaml:"jql"`
	Limit        int         `yaml:"limit"`
	PollInterval Duration    `yaml:"poll_interval"`
	RouteHints   []RouteHint `yaml:"route_hints"`
}

// LokiDiscovery groups error lines from a Grafana-proxied Loki datasource (discovery.loki).
// Credentials are named by env var and read at call time, never stored.
type LokiDiscovery struct {
	BaseURL       string   `yaml:"base_url"` // root or full /api/ds/query URL
	DatasourceUID string   `yaml:"datasource_uid"`
	EmailEnv      string   `yaml:"email_env"`
	PasswordEnv   string   `yaml:"password_env"`
	Expr          string   `yaml:"expr"`
	Window        Duration `yaml:"window"`
	PollInterval  Duration `yaml:"poll_interval"`
	MinCount      int      `yaml:"min_count"`
	MaxLines      int      `yaml:"max_lines"`
	Signature     string   `yaml:"signature"` // regexp; first match = signature
	// MaxEventsPerPoll keeps only the top-N signatures by line count per poll.
	MaxEventsPerPoll int `yaml:"max_events_per_poll"`
}

// ExecDiscovery runs a command that prints a JSON array of events (discovery.exec).
type ExecDiscovery struct {
	Command      []string `yaml:"command"`
	PollInterval Duration `yaml:"poll_interval"`
	Timeout      Duration `yaml:"timeout"`
}

// AdapterNames is the effective source list: discovery.adapters when set, else
// the legacy single discovery.adapter.
func (c *Config) AdapterNames() []string {
	if len(c.Discovery.Adapters) > 0 {
		return append([]string(nil), c.Discovery.Adapters...)
	}
	return []string{c.Discovery.Adapter}
}

// UsesAdapter reports whether name is in the effective adapter list.
func (c *Config) UsesAdapter(name string) bool {
	return slices.Contains(c.AdapterNames(), name)
}

// Env resolves an environment by name; "" means the default environment.
func (c *Config) Env(name string) (Environment, error) {
	if name == "" {
		name = c.Target.DefaultEnv
	}
	if len(c.Target.Environments) == 0 {
		if name == "" || name == ImplicitEnvName {
			return c.implicitEnv(), nil
		}
		return Environment{}, fmt.Errorf("unknown environment %q (no target.environments configured; only %q exists)", name, ImplicitEnvName)
	}
	e, ok := c.Target.Environments[name]
	if !ok {
		return Environment{}, fmt.Errorf("unknown environment %q (configured: %s)", name, strings.Join(c.EnvNames(), ", "))
	}
	e.Name = name
	return e, nil
}

// DefaultEnv returns the environment used unless a run names another one.
func (c *Config) DefaultEnv() Environment {
	e, err := c.Env("")
	if err != nil {
		return c.implicitEnv()
	}
	return e
}

// EnvNames lists configured environment names, sorted.
func (c *Config) EnvNames() []string {
	if len(c.Target.Environments) == 0 {
		return []string{ImplicitEnvName}
	}
	out := make([]string, 0, len(c.Target.Environments))
	for n := range c.Target.Environments {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func (c *Config) implicitEnv() Environment {
	return Environment{
		Name:         ImplicitEnvName,
		BaseURL:      c.Target.BaseURL,
		AllowedHosts: append([]string(nil), c.Target.AllowedHosts...),
		AssetPage:    c.Deployment.Readiness.AssetPage,
	}
}

// EnvAllows reports whether a scenario with the given mutation class may run in
// env: read-only environments refuse anything but read-only scenarios. An empty
// mutation counts as read-only (the DSL default).
func EnvAllows(env Environment, mutation string) error {
	if !env.ReadOnly || mutation == "" || mutation == "read-only" {
		return nil
	}
	return fmt.Errorf("environment %q is read-only: scenarios with mutation %q are refused", env.Name, mutation)
}

// applyEnvDefaults names each environment, derives its allowlist from base_url
// and picks the default environment when it is unambiguous.
func (c *Config) applyEnvDefaults() {
	for name, e := range c.Target.Environments {
		e.Name = name
		if len(e.AllowedHosts) == 0 {
			if u, err := url.Parse(e.BaseURL); err == nil && u.Hostname() != "" {
				e.AllowedHosts = []string{strings.ToLower(u.Hostname())}
			}
		}
		c.Target.Environments[name] = e
	}
	if c.Target.DefaultEnv == "" {
		switch len(c.Target.Environments) {
		case 0:
			c.Target.DefaultEnv = ImplicitEnvName
		case 1:
			for name := range c.Target.Environments {
				c.Target.DefaultEnv = name
			}
		}
	}
}

// applyEnvTarget makes the default environment the primary target so code that
// reads target.base_url / allowed_hosts (agent, gate, probes) uses it.
func (c *Config) applyEnvTarget() {
	if len(c.Target.Environments) == 0 {
		return
	}
	e, ok := c.Target.Environments[c.Target.DefaultEnv]
	if !ok {
		return
	}
	if e.BaseURL != "" {
		c.Target.BaseURL = e.BaseURL
	}
	for _, h := range e.AllowedHosts {
		if !c.HostAllowed(h) {
			c.Target.AllowedHosts = append(c.Target.AllowedHosts, h)
		}
	}
}

func (c *Config) validateEnvs() error {
	if len(c.Target.Environments) == 0 {
		if c.Target.DefaultEnv != ImplicitEnvName {
			return fmt.Errorf("target.default_env %q is unknown: no target.environments are configured (only %q exists)", c.Target.DefaultEnv, ImplicitEnvName)
		}
		return nil
	}
	for _, name := range c.EnvNames() {
		e := c.Target.Environments[name]
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t/") {
			return fmt.Errorf("target.environments: name %q must be non-empty without spaces or '/'", name)
		}
		if e.BaseURL == "" {
			return fmt.Errorf("target.environments.%s.base_url is required", name)
		}
		u, err := url.Parse(e.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("target.environments.%s.base_url %q must be an absolute http(s) URL", name, e.BaseURL)
		}
		if len(e.AllowedHosts) == 0 {
			return fmt.Errorf("target.environments.%s.allowed_hosts must list at least one host", name)
		}
		if e.AssetPage != "" && !e.URLAllowed(e.AssetPage) {
			return fmt.Errorf("target.environments.%s.asset_page %q must be inside allowed_hosts %v", name, e.AssetPage, e.AllowedHosts)
		}
	}
	if c.Target.DefaultEnv == "" {
		return fmt.Errorf("target.default_env is required when several target.environments are configured (%s)", strings.Join(c.EnvNames(), ", "))
	}
	if _, ok := c.Target.Environments[c.Target.DefaultEnv]; !ok {
		return fmt.Errorf("target.default_env %q is unknown (configured: %s)", c.Target.DefaultEnv, strings.Join(c.EnvNames(), ", "))
	}
	return nil
}

// Target is one line of the url file.
type Target struct {
	Label string
	URL   string
	Path  string // path (+query) part of URL; "/" when empty
}

type RouteMapEntry struct {
	PathPrefix string `yaml:"path_prefix"`
	Route      string `yaml:"route"`
	Capability string `yaml:"capability"`
}

type Persona struct {
	Username string            `yaml:"username"`
	Password string            `yaml:"password"`
	Extra    map[string]string `yaml:"extra"`
}

// Duration is a yaml-friendly time.Duration ("2m", "10s").
// Jira configures the approval comment written back to the source issue.
type Jira struct {
	CLI string `yaml:"cli"` // binary; invoked as `<cli> jira workitem comment create|list --key <KEY> ... --json`
	// CommentOnApprove posts the approval receipt to the source issue (nil = true).
	CommentOnApprove *bool `yaml:"comment_on_approve"`
	// DryRun writes the comment body to <evidence>/jira/<KEY>-<ts>.md instead of posting.
	DryRun bool `yaml:"dry_run"`
}

// CommentsOnApprove reports jira.comment_on_approve with its default of true.
func (j Jira) CommentsOnApprove() bool { return j.CommentOnApprove == nil || *j.CommentOnApprove }

// DailyCadence is the scenarios.cadence value set by `vigil approve`.
const DailyCadence = "daily"

// DailyLocation is the zone schedule.daily_at is read in: active_hours.tz, else machine local.
func (c *Config) DailyLocation() *time.Location { return c.Schedule.ActiveHours.Location() }

// NextDailyRun is the next schedule.daily_at occurrence strictly after now.
func (c *Config) NextDailyRun(now time.Time) time.Time {
	return NextDailyAt(now, c.Schedule.DailyAt, c.Schedule.ActiveHours.TZ)
}

// DefaultIssueKeyPattern matches a conventional tracker key such as PROJ-123.
// It is deliberately case-sensitive: a case-insensitive default would read
// "utf-8" or "http-2" in a commit subject as an issue key.
const DefaultIssueKeyPattern = `\b[A-Z][A-Z0-9]+-\d+\b`

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "" {
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv substitutes ${VAR} with the environment; unset vars become "".
func ExpandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envRe.FindStringSubmatch(m)[1]
		return os.Getenv(key)
	})
}

// Load reads and validates a config file, applying defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal([]byte(ExpandEnv(string(raw))), &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	c.BaseDir = filepath.Dir(abs)
	c.applyDefaults()
	if err := c.loadTargets(); err != nil {
		return nil, err
	}
	c.applyEnvTarget()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// loadTargets reads Target.URLFile (default url.md when present) and derives
// base_url / allowed_hosts from the primary URL when they are not configured.
func (c *Config) loadTargets() error {
	file := c.Target.URLFile
	if file == "" {
		if _, err := os.Stat(filepath.Join(c.BaseDir, "url.md")); err == nil {
			file = "url.md"
		}
	}
	if file == "" {
		return nil
	}
	c.Target.URLFile = file
	targets, err := ParseURLFile(c.Abs(file))
	if err != nil {
		if os.IsNotExist(err) && c.Target.BaseURL != "" {
			return nil // url_file is optional when base_url is configured (example config)
		}
		return err
	}
	c.Targets = targets
	if len(targets) == 0 {
		return nil
	}
	primary, err := url.Parse(targets[0].URL)
	if err != nil {
		return fmt.Errorf("%s: primary url %q: %w", file, targets[0].URL, err)
	}
	if c.Target.BaseURL == "" {
		c.Target.BaseURL = primary.Scheme + "://" + primary.Host
	}
	host := strings.ToLower(primary.Hostname())
	if host != "" && !c.HostAllowed(host) {
		c.Target.AllowedHosts = append(c.Target.AllowedHosts, host)
	}
	return nil
}

// ParseURLFile parses a url file: one URL per line, `<label> | <url>` or bare
// `<url>`; blank lines and `#` comments are ignored.
func ParseURLFile(path string) ([]Target, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Target
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t := Target{}
		if label, rest, ok := strings.Cut(line, "|"); ok {
			t.Label = strings.TrimSpace(label)
			t.URL = strings.TrimSpace(rest)
		} else {
			t.URL = line
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("%s:%d: invalid url %q (expected `<label> | <url>` or `<url>`)", path, i+1, t.URL)
		}
		t.Path = u.RequestURI()
		if t.Path == "" {
			t.Path = "/"
		}
		if t.Label == "" {
			t.Label = strings.Trim(u.Path, "/")
			if t.Label == "" {
				t.Label = u.Host
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// PrimaryTarget returns Targets[0] when a url file is loaded.
func (c *Config) PrimaryTarget() (Target, bool) {
	if len(c.Targets) == 0 {
		return Target{}, false
	}
	return c.Targets[0], true
}

// EntryPath is the default entry route: the primary target's path, else "/".
func (c *Config) EntryPath() string {
	if t, ok := c.PrimaryTarget(); ok {
		return t.Path
	}
	return "/"
}

// TargetURLs lists every url-file URL (for agent Routes / discovery).
func (c *Config) TargetURLs() []string {
	out := make([]string, 0, len(c.Targets))
	for _, t := range c.Targets {
		out = append(out, t.URL)
	}
	return out
}

func (c *Config) applyDefaults() {
	def := func(d *Duration, v time.Duration) {
		if d.Duration == 0 {
			d.Duration = v
		}
	}
	if c.Runtime.Mode == "" {
		c.Runtime.Mode = "local"
	}
	c.applyEnvDefaults()
	if c.Discovery.Adapter == "" {
		c.Discovery.Adapter = "file"
	}
	def(&c.Discovery.PollInterval, 60*time.Second)
	if c.Discovery.HistoryLimit == 0 {
		c.Discovery.HistoryLimit = 5
	}
	if c.Discovery.FeaturesDir == "" {
		c.Discovery.FeaturesDir = "features"
	}
	if c.Discovery.Jira.CLI == "" {
		c.Discovery.Jira.CLI = "acli"
	}
	if c.Discovery.Jira.Limit == 0 {
		c.Discovery.Jira.Limit = 20
	}
	def(&c.Discovery.Jira.PollInterval, 10*time.Minute)
	def(&c.Discovery.Loki.Window, time.Hour)
	def(&c.Discovery.Loki.PollInterval, 15*time.Minute)
	if c.Discovery.Loki.MinCount == 0 {
		c.Discovery.Loki.MinCount = 3
	}
	if c.Discovery.Loki.MaxLines == 0 {
		c.Discovery.Loki.MaxLines = 500
	}
	if c.Discovery.Loki.Signature == "" {
		c.Discovery.Loki.Signature = DefaultLokiSignature
	}
	if c.Discovery.Loki.MaxEventsPerPoll == 0 {
		c.Discovery.Loki.MaxEventsPerPoll = 5
	}
	def(&c.Discovery.Exec.PollInterval, 15*time.Minute)
	def(&c.Discovery.Exec.Timeout, 60*time.Second)
	if c.Deployment.Readiness.Strategy == "" {
		c.Deployment.Readiness.Strategy = "delay"
	}
	def(&c.Deployment.Readiness.DelayAfterShip, 2*time.Minute)
	def(&c.Deployment.Readiness.MaxWait, 30*time.Minute)
	if c.Browser.Primary == "" {
		c.Browser.Primary = "lightpanda"
	}
	if c.Browser.Fallback == "" {
		c.Browser.Fallback = "chromium"
	}
	if c.Browser.Lightpanda.Binary == "" {
		c.Browser.Lightpanda.Binary = "bin/lightpanda"
	}
	if c.Browser.Lightpanda.Host == "" {
		c.Browser.Lightpanda.Host = "127.0.0.1"
	}
	if c.Browser.Lightpanda.Port == 0 {
		c.Browser.Lightpanda.Port = 9333
	}
	def(&c.Browser.StepTimeout, 15*time.Second)
	def(&c.Browser.RunTimeout, 3*time.Minute)
	def(&c.Browser.IdleStop, 10*time.Minute)
	if c.Workers.Functional == 0 {
		c.Workers.Functional = 1
	}
	if c.State.Type == "" {
		c.State.Type = "sqlite"
	}
	if c.State.Path == "" {
		c.State.Path = ".vigil/state.db"
	}
	if c.Evidence.Type == "" {
		c.Evidence.Type = "local"
	}
	if c.Evidence.Dir == "" {
		c.Evidence.Dir = "evidence"
	}
	if c.Evidence.RetainPassDays == 0 {
		c.Evidence.RetainPassDays = 3
	}
	if c.Evidence.ChromiumCapture == "" {
		c.Evidence.ChromiumCapture = "per-deploy"
	}
	if c.Evidence.MaxTotalMB == 0 {
		c.Evidence.MaxTotalMB = 1024
	}
	if c.Evidence.RetainRunsDays == 0 {
		c.Evidence.RetainRunsDays = 90
	}
	if c.Evidence.RetainFailDays == 0 {
		c.Evidence.RetainFailDays = 30
	}
	if c.Budget.BrowserMinutesPerHour == 0 {
		c.Budget.BrowserMinutesPerHour = 60
	}
	if c.Budget.ChromiumMinutesPerHour == 0 {
		c.Budget.ChromiumMinutesPerHour = 5
	}
	if c.Budget.AgentTasksPerHour == 0 {
		c.Budget.AgentTasksPerHour = 10
	}
	if c.Policy.SoakPasses == 0 {
		c.Policy.SoakPasses = 3
	}
	if c.Policy.QuarantineAfter == 0 {
		c.Policy.QuarantineAfter = 3
	}
	if c.Policy.RetryOnFail == 0 {
		c.Policy.RetryOnFail = 1
	}
	if c.Policy.AgentFixAttempts <= 0 {
		c.Policy.AgentFixAttempts = 1
	}
	def(&c.Schedule.Soak, 10*time.Minute)
	def(&c.Schedule.P0, 15*time.Minute)
	def(&c.Schedule.P1, 60*time.Minute)
	def(&c.Schedule.P2, 6*time.Hour)
	def(&c.Schedule.FailureBackoff, 5*time.Minute)
	if c.Discovery.IssueKeyPattern == "" {
		c.Discovery.IssueKeyPattern = DefaultIssueKeyPattern
	}
	def(&c.Schedule.Tick, 10*time.Second)
	if c.Schedule.DailyAt == "" {
		c.Schedule.DailyAt = "09:00"
	}
	if c.Jira.CLI == "" {
		c.Jira.CLI = "acli"
	}
	if c.Jira.CommentOnApprove == nil {
		on := true
		c.Jira.CommentOnApprove = &on
	}
	if c.Supervisor.Enabled {
		def(&c.Supervisor.Tick, 15*time.Minute)
		def(&c.Supervisor.Timeout, 10*time.Minute)
		if c.Supervisor.MaxActions == 0 {
			c.Supervisor.MaxActions = 5
		}
		if c.Supervisor.MaxQuarantinePerPlan == 0 {
			c.Supervisor.MaxQuarantinePerPlan = 1
		}
		if c.Budget.SupervisorTasksPerHour == 0 {
			c.Budget.SupervisorTasksPerHour = 4
		}
		if c.Supervisor.MaxSiteRoutes == 0 {
			c.Supervisor.MaxSiteRoutes = 500
		}
	}
	if c.Schedule.ActiveHours.Enabled {
		if c.Schedule.ActiveHours.From == "" {
			c.Schedule.ActiveHours.From = "08:00"
		}
		if c.Schedule.ActiveHours.To == "" {
			c.Schedule.ActiveHours.To = "19:00"
		}
		if len(c.Schedule.ActiveHours.Days) == 0 {
			c.Schedule.ActiveHours.Days = Weekdays{1, 2, 3, 4, 5}
		}
	}
	if c.Agent.Provider == "" {
		c.Agent.Provider = "pi"
	}
	if c.Agent.Model == "" {
		c.Agent.Model = "zai/glm-5.3-flash"
	}
	if c.Agent.Thinking == "" {
		c.Agent.Thinking = "high"
	}
	def(&c.Agent.ModelCooldown, 10*time.Minute)
	def(&c.Agent.Timeout, 20*time.Minute)
	if c.Agent.Retries == 0 {
		c.Agent.Retries = 3
	}
	def(&c.Agent.Backoff, 45*time.Second)
	if c.Agent.MaxTurns == 0 {
		c.Agent.MaxTurns = 40
	}
	if c.Agent.Sandbox == "" {
		c.Agent.Sandbox = "auto"
	}
	if c.Agent.MaxScenariosPerTask == 0 {
		c.Agent.MaxScenariosPerTask = 3
	}
	if c.Agent.Continuations == 0 {
		c.Agent.Continuations = 2
	}
	if c.Agent.ModelRetries == 0 {
		c.Agent.ModelRetries = 10
	}
	def(&c.Agent.ModelRetryDelay, 3*time.Second)
	// agent.domain_file defaults to "" (no domain rules section in the prompt).
	c.Agent.DomainFile = strings.TrimSpace(c.Agent.DomainFile)
	if c.Policy.ObservationOracle == "" {
		c.Policy.ObservationOracle = "needs_review"
	}
	if c.Policy.StructuralDupThreshold == 0 {
		c.Policy.StructuralDupThreshold = 0.8
	}
	if c.Paths.Scenarios == "" {
		c.Paths.Scenarios = "scenarios"
	}
	if c.Paths.Flows == "" {
		c.Paths.Flows = "flows"
	}
}

func (c *Config) validate() error {
	if c.Project.ID == "" {
		return fmt.Errorf("project.id is required")
	}
	if err := c.validateEnvs(); err != nil {
		return err
	}
	if c.Target.BaseURL == "" {
		return fmt.Errorf("target.base_url is required (or list the deployed URL in %s)", firstNonEmpty(c.Target.URLFile, "url.md"))
	}
	if len(c.Target.AllowedHosts) == 0 {
		return fmt.Errorf("target.allowed_hosts must list at least one host (auto-derived from %s when present)", firstNonEmpty(c.Target.URLFile, "url.md"))
	}
	if err := c.Schedule.ActiveHours.Validate(); err != nil {
		return fmt.Errorf("schedule.active_hours: %w", err)
	}
	if err := c.Supervisor.Validate(); err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}
	if _, err := parseHM(c.Schedule.DailyAt); err != nil {
		return fmt.Errorf("schedule.daily_at: %w", err)
	}
	if strings.TrimSpace(c.Jira.CLI) == "" {
		return fmt.Errorf("jira.cli is required (the acli binary used for approval comments)")
	}
	if _, err := regexp.Compile(c.Discovery.IssueKeyPattern); err != nil {
		return fmt.Errorf("discovery.issue_key_pattern: %w", err)
	}
	if err := c.validateDiscovery(); err != nil {
		return err
	}
	for i, m := range c.Agent.Models {
		if _, _, _, err := SplitModelEntry(m); err != nil {
			return fmt.Errorf("agent.models[%d]: %w", i, err)
		}
	}
	if c.Agent.ModelCooldown.Duration <= 0 {
		return fmt.Errorf("agent.model_cooldown must be positive (got %s)", c.Agent.ModelCooldown.Duration)
	}
	if c.Agent.DomainFile != "" {
		// A missing file is only a doctor warning (the loop must not stop for it);
		// a directory can never be read as rules.
		if fi, err := os.Stat(c.Abs(c.Agent.DomainFile)); err == nil && fi.IsDir() {
			return fmt.Errorf("agent.domain_file %q is a directory, expected a markdown file", c.Agent.DomainFile)
		}
	}
	return nil
}

// validateDiscovery checks the adapter list and the per-source settings of every listed source.
func (c *Config) validateDiscovery() error {
	known := []string{AdapterGenericGit, AdapterFile, AdapterSDLCKit, AdapterJira, AdapterLoki, AdapterExec}
	seen := map[string]bool{}
	for i, name := range c.Discovery.Adapters {
		if !slices.Contains(known, name) {
			return fmt.Errorf("discovery.adapters[%d]: unknown adapter %q (want one of %s)", i, name, strings.Join(known, ", "))
		}
		if seen[name] {
			return fmt.Errorf("discovery.adapters: %q listed twice", name)
		}
		seen[name] = true
	}
	if c.UsesAdapter(AdapterJira) {
		j := c.Discovery.Jira
		if strings.TrimSpace(j.CLI) == "" {
			return fmt.Errorf("discovery.jira.cli is required")
		}
		if strings.TrimSpace(j.JQL) == "" {
			return fmt.Errorf("discovery.jira.jql is required when the jira adapter is enabled")
		}
		if j.Limit <= 0 {
			return fmt.Errorf("discovery.jira.limit must be positive (got %d)", j.Limit)
		}
		if j.PollInterval.Duration <= 0 {
			return fmt.Errorf("discovery.jira.poll_interval must be positive (got %s)", j.PollInterval.Duration)
		}
		for i, h := range j.RouteHints {
			if strings.TrimSpace(h.Match) == "" || strings.TrimSpace(h.Route) == "" {
				return fmt.Errorf("discovery.jira.route_hints[%d]: match and route are both required", i)
			}
		}
	}
	if c.UsesAdapter(AdapterLoki) {
		l := c.Discovery.Loki
		if strings.TrimSpace(l.BaseURL) == "" {
			return fmt.Errorf("discovery.loki.base_url is required when the loki adapter is enabled (check the ${VAR} it references)")
		}
		if u, err := url.Parse(l.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("discovery.loki.base_url %q must be an absolute http(s) URL", l.BaseURL)
		}
		if strings.TrimSpace(l.DatasourceUID) == "" {
			return fmt.Errorf("discovery.loki.datasource_uid is required when the loki adapter is enabled")
		}
		if strings.TrimSpace(l.EmailEnv) == "" || strings.TrimSpace(l.PasswordEnv) == "" {
			return fmt.Errorf("discovery.loki.email_env and password_env must name the env vars holding the Grafana credentials")
		}
		if strings.TrimSpace(l.Expr) == "" {
			return fmt.Errorf("discovery.loki.expr is required when the loki adapter is enabled")
		}
		if l.Window.Duration <= 0 || l.PollInterval.Duration <= 0 {
			return fmt.Errorf("discovery.loki.window and poll_interval must be positive (got %s / %s)", l.Window.Duration, l.PollInterval.Duration)
		}
		if l.MinCount < 1 {
			return fmt.Errorf("discovery.loki.min_count must be at least 1 (got %d)", l.MinCount)
		}
		if l.MaxLines <= 0 {
			return fmt.Errorf("discovery.loki.max_lines must be positive (got %d)", l.MaxLines)
		}
		if _, err := regexp.Compile(l.Signature); err != nil {
			return fmt.Errorf("discovery.loki.signature: %w", err)
		}
		if l.MaxEventsPerPoll < 1 {
			return fmt.Errorf("discovery.loki.max_events_per_poll must be at least 1 (got %d)", l.MaxEventsPerPoll)
		}
	}
	if c.UsesAdapter(AdapterExec) {
		e := c.Discovery.Exec
		if len(e.Command) == 0 || strings.TrimSpace(e.Command[0]) == "" {
			return fmt.Errorf("discovery.exec.command is required when the exec adapter is enabled")
		}
		if e.PollInterval.Duration <= 0 || e.Timeout.Duration <= 0 {
			return fmt.Errorf("discovery.exec.poll_interval and timeout must be positive (got %s / %s)", e.PollInterval.Duration, e.Timeout.Duration)
		}
	}
	return nil
}

// ThinkingLevels are the values pi accepts for --thinking (and the :<level>
// suffix of a chain entry).
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// SplitModelEntry parses one chain entry `provider/model[:thinking]`. Thinking
// is "" when the entry carries no level; the caller falls back to agent.thinking.
func SplitModelEntry(entry string) (provider, model, thinking string, err error) {
	e := strings.TrimSpace(entry)
	if e == "" {
		return "", "", "", fmt.Errorf("model entry is empty (expected provider/model[:thinking])")
	}
	if i := strings.LastIndexByte(e, ':'); i >= 0 {
		lvl := e[i+1:]
		if !slices.Contains(ThinkingLevels, lvl) {
			return "", "", "", fmt.Errorf("model entry %q: thinking level %q must be one of %s", entry, lvl, strings.Join(ThinkingLevels, ", "))
		}
		thinking, e = lvl, e[:i]
	}
	i := strings.IndexByte(e, '/')
	if i <= 0 || i == len(e)-1 {
		return "", "", "", fmt.Errorf("model entry %q must be provider/model[:thinking]", entry)
	}
	return e[:i], e[i+1:], thinking, nil
}

// ModelEntries is the effective ordered chain: agent.models when set, else the
// single legacy agent.model (its thinking comes from agent.thinking).
func (c *Config) ModelEntries() []string {
	if len(c.Agent.Models) > 0 {
		return append([]string(nil), c.Agent.Models...)
	}
	return []string{c.Agent.Model}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Abs resolves a config-relative path against BaseDir.
func (c *Config) Abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.BaseDir, p)
}

// HostAllowed reports whether host (without port) is in the allowlist.
func (c *Config) HostAllowed(host string) bool {
	return hostAllowed(c.Target.AllowedHosts, host)
}

func hostAllowed(allowed []string, host string) bool {
	host = strings.ToLower(strings.Split(host, ":")[0])
	for _, h := range allowed {
		h = strings.ToLower(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}
