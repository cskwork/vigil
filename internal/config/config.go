// Package config loads vigil.yaml (PRD §19) with environment substitution.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	} `yaml:"target"`

	// Targets is parsed from Target.URLFile (not from YAML). Targets[0] is primary.
	Targets []Target `yaml:"-"`

	Discovery struct {
		Adapter      string          `yaml:"adapter"` // generic-git | sdlc-kit | file
		Branch       string          `yaml:"branch"`  // generic-git: remote branch to watch, e.g. origin/staging
		// IssueKeyPattern groups commits by the issue key in their subject.
		// Uppercase-only by default so "utf-8" in a subject is not read as a key;
		// set an explicit (?i) pattern if your team writes keys in lower case.
		IssueKeyPattern string `yaml:"issue_key_pattern"`
		Fetch        bool            `yaml:"fetch"`
		PathPrefixes []string        `yaml:"path_prefixes"` // only commits touching these prefixes become features
		RouteMap     []RouteMapEntry `yaml:"route_map"`
		FeaturesDir  string          `yaml:"features_dir"` // file adapter: directory of FeatureEvent yaml files
		PollInterval Duration        `yaml:"poll_interval"`
		HistoryLimit int             `yaml:"history_limit"` // import --history rate limit
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
	} `yaml:"budget"`

	Policy struct {
		AllowDestructive bool `yaml:"allow_destructive"`
		SoakPasses       int  `yaml:"soak_passes"`      // clean runs before ACTIVE
		QuarantineAfter  int  `yaml:"quarantine_after"` // consecutive flakes before QUARANTINED
		RetryOnFail      int  `yaml:"retry_on_fail"`    // cheap safe retries before classify
		// ObservationOracle: needs_review (PRD default: observation alone is not an oracle) | soak
		ObservationOracle string `yaml:"observation_oracle"`
		// StructuralDupThreshold: Jaccard similarity of major actions above which a candidate is DUPLICATE.
		StructuralDupThreshold float64 `yaml:"structural_dup_threshold"`
	} `yaml:"policy"`

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
	} `yaml:"schedule"`

	Agent struct {
		Provider  string            `yaml:"provider"` // pi
		Model     string            `yaml:"model"`    // zai/glm-5.3-flash
		Thinking  string            `yaml:"thinking"` // high
		Timeout   Duration          `yaml:"timeout"`
		Extension string            `yaml:"extension"` // path to pi extension (agent_browser tool)
		EnvMap    map[string]string `yaml:"env_map"`   // e.g. ZAI_API_KEY: Z_AI_API_KEY
		Retries   int               `yaml:"retries"`
		Backoff   Duration          `yaml:"backoff"`
		MaxTurns  int               `yaml:"max_turns"`
		Workdir   string            `yaml:"workdir"`
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
	} `yaml:"agent"`

	Personas map[string]Persona `yaml:"personas"`

	Paths struct {
		Scenarios string `yaml:"scenarios"`
		Flows     string `yaml:"flows"`
	} `yaml:"paths"`

	// BaseDir is the directory containing the config file (not from YAML).
	BaseDir string `yaml:"-"`
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
	def(&c.Schedule.Soak, 10*time.Minute)
	def(&c.Schedule.P0, 15*time.Minute)
	def(&c.Schedule.P1, 60*time.Minute)
	def(&c.Schedule.P2, 6*time.Hour)
	def(&c.Schedule.FailureBackoff, 5*time.Minute)
	if c.Discovery.IssueKeyPattern == "" {
		c.Discovery.IssueKeyPattern = DefaultIssueKeyPattern
	}
	def(&c.Schedule.Tick, 10*time.Second)
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
	if c.Target.BaseURL == "" {
		return fmt.Errorf("target.base_url is required (or list the deployed URL in %s)", firstNonEmpty(c.Target.URLFile, "url.md"))
	}
	if len(c.Target.AllowedHosts) == 0 {
		return fmt.Errorf("target.allowed_hosts must list at least one host (auto-derived from %s when present)", firstNonEmpty(c.Target.URLFile, "url.md"))
	}
	if err := c.Schedule.ActiveHours.Validate(); err != nil {
		return fmt.Errorf("schedule.active_hours: %w", err)
	}
	if _, err := regexp.Compile(c.Discovery.IssueKeyPattern); err != nil {
		return fmt.Errorf("discovery.issue_key_pattern: %w", err)
	}
	return nil
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
	host = strings.ToLower(strings.Split(host, ":")[0])
	for _, h := range c.Target.AllowedHosts {
		h = strings.ToLower(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}
