package config

import (
	"fmt"
	"regexp"
	"time"
)

// Supervisor drives coverage growth: on a slow tick it reads the QA state,
// asks a model where coverage is missing, and proposes bounded actions that
// deterministic code then validates and executes.
//
// It is off by default. Enabling it never widens what the system is allowed to
// do - every proposed action still passes the same guards a human-written
// action would (budgets, host allowlist, active hours, destructive policy).
type Supervisor struct {
	Enabled bool `yaml:"enabled"`
	// Model/Thinking default to the Browser Agent's when empty, so a single
	// agent block is enough for the common case.
	Model    string   `yaml:"model"`
	Thinking string   `yaml:"thinking"`
	Tick     Duration `yaml:"tick"`
	// MaxActions caps one plan. A model that returns fifty actions gets the
	// first MaxActions that pass validation; the rest are logged and dropped.
	MaxActions int `yaml:"max_actions"`
	// MaxQuarantinePerPlan bounds the most destructive verb the supervisor has.
	MaxQuarantinePerPlan int `yaml:"max_quarantine_per_plan"`
	// DryRun records the plan and the accept/reject verdicts without executing.
	// Defaults to true: watch what it wants to do before letting it act.
	DryRun *bool `yaml:"dry_run"`
	// ExploreRoutes optionally seeds the frontier. It is not a whitelist: the
	// supervisor may target any in-scope path it has seen, so giving only a base
	// URL is enough and the map fills itself in as agents browse.
	ExploreRoutes []string `yaml:"explore_routes"`
	// DenyPathPatterns are regexps refused as discovery targets no matter what.
	// Empty uses DefaultDenyPathPatterns; set to a list with one empty string to
	// disable the guard entirely (not recommended).
	DenyPathPatterns []string `yaml:"deny_path_patterns"`
	// MaxSiteRoutes caps the discovered-route inventory so a site with unbounded
	// URLs (ids in the path) cannot grow state without limit.
	MaxSiteRoutes int `yaml:"max_site_routes"`
	// ExploreMutation is how far a discovered scenario may go: read-only looks
	// but never submits, reversible may create things it can undo (an
	// assessment, an answer). destructive still needs policy.allow_destructive.
	ExploreMutation string `yaml:"explore_mutation"`
	// Accounts pins the test identities exploration may enter as. Without this
	// each run picks whatever it finds on the page, so scenarios hardcode a
	// different name every time and none of them reproduce. Names only - the
	// entry page needs no password, and secrets never belong in this file.
	Accounts []string `yaml:"accounts"`
	// FillData asks discovered scenarios to exercise inputs with realistic
	// values rather than only reading pages. Requires a mutation level above
	// read-only to be useful.
	FillData bool     `yaml:"fill_data"`
	Timeout  Duration `yaml:"timeout"`
}

// DefaultDenyPathPatterns keep autonomous exploration away from the two things
// that go wrong when a browser wanders on its own: ending its own session, and
// pressing something irreversible.
var DefaultDenyPathPatterns = []string{
	`(?i)(^|/)(logout|log-out|signout|sign-out|logoff)(/|$|\?)`,
	`(?i)(^|/|[?&=])(delete|destroy|purge|drop|truncate|wipe)(/|$|[?&=_-])`,
	`(?i)(^|/)(unsubscribe|deactivate|close-account|withdraw)(/|$|\?)`,
}

// EffectiveDenyPatterns falls back to the built-in list.
func (s Supervisor) EffectiveDenyPatterns() []string {
	if len(s.DenyPathPatterns) == 0 {
		return DefaultDenyPathPatterns
	}
	return s.DenyPathPatterns
}

// SupervisorStateKey holds the most recent supervisor result for the dashboard.
// SiteRoutesKey holds the discovered-route inventory. Both live here so the
// read-only UI can reach them without importing the orchestrator.
const (
	SupervisorStateKey = "supervisor:last"
	SiteRoutesKey      = "site:routes"
)

// SupervisorVerbs are the only actions a plan may contain. Everything else is
// rejected by name, including anything that would remove coverage: a model that
// asks to delete, retire, reject or supersede a scenario is refused and logged.
var SupervisorVerbs = []string{
	"discover_route",
	"repair_script",
	"validate_candidate",
	"run_scenario",
	"raise_cadence",
	"quarantine",
}

// SupervisorDeniedVerbs are refused loudly rather than silently ignored, so the
// log shows when the model tried to reach outside its mandate.
var SupervisorDeniedVerbs = []string{
	"delete", "delete_scenario", "remove", "remove_scenario", "drop",
	"retire", "reject", "supersede", "purge", "reset",
	"set_destructive", "allow_destructive",
	"set_budget", "set_active_hours", "disable_active_hours",
	"add_allowed_host", "set_target",
}

func (s Supervisor) DryRunEnabled() bool { return s.DryRun == nil || *s.DryRun }

// EffectiveMutation defaults to the safest level.
func (s Supervisor) EffectiveMutation() string {
	if s.ExploreMutation == "" {
		return "read-only"
	}
	return s.ExploreMutation
}

// EffectiveModel falls back to the Browser Agent's model.
func (s Supervisor) EffectiveModel(agentModel string) string {
	if s.Model == "" {
		return agentModel
	}
	return s.Model
}

func (s Supervisor) EffectiveThinking(agentThinking string) string {
	if s.Thinking == "" {
		return agentThinking
	}
	return s.Thinking
}

func (s Supervisor) Validate() error {
	if !s.Enabled {
		return nil
	}
	if s.Tick.Duration < time.Minute {
		return fmt.Errorf("tick %s is under one minute; the supervisor is a slow loop", s.Tick.Duration)
	}
	if s.MaxActions <= 0 {
		return fmt.Errorf("max_actions must be positive")
	}
	if s.MaxQuarantinePerPlan < 0 {
		return fmt.Errorf("max_quarantine_per_plan cannot be negative")
	}
	switch s.ExploreMutation {
	case "", "read-only", "reversible", "destructive":
	default:
		return fmt.Errorf("explore_mutation %q must be read-only, reversible or destructive", s.ExploreMutation)
	}
	for _, p := range s.DenyPathPatterns {
		if p == "" {
			continue
		}
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("deny_path_patterns %q: %w", p, err)
		}
	}
	return nil
}
