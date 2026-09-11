// Package agent defines the Browser Agent contract (PRD §11) and adapters that
// launch a bounded agent task (pi + agent_browser tool) and parse its result.
package agent

import (
	"context"
	"time"
)

// Task kinds map to orchestrator actions.
const (
	TaskDiscover     = "discover_feature"       // BROWSER_AGENT_DISCOVER
	TaskVerifyChange = "verify_changed_feature" // BROWSER_AGENT_VERIFY_CHANGE
	TaskRepair       = "repair_script"          // BROWSER_AGENT_REPAIR
	TaskReproduce    = "reproduce"              // REPRODUCE: replay a reported symptom (issue / log signature)
)

// Decision values the agent must return.
const (
	DecisionNewScript     = "NEW_SCRIPT"
	DecisionPatchScript   = "PATCH_SCRIPT"
	DecisionAppFailure    = "APP_FAILURE"
	DecisionNoNewCoverage = "NO_NEW_COVERAGE"
	DecisionNeedsReview   = "NEEDS_REVIEW"
	DecisionOracleUnknown = "ORACLE_UNKNOWN"
)

// Request is the bounded task handed to the Browser Agent.
type Request struct {
	Task         string   `yaml:"task" json:"task"`
	ProjectID    string   `yaml:"project_id" json:"project_id"`
	FeatureID    string   `yaml:"feature_id" json:"feature_id"`
	ShippedSHA   string   `yaml:"shipped_sha" json:"shipped_sha"`
	Summary      string   `yaml:"summary,omitempty" json:"summary,omitempty"`
	Target       string   `yaml:"target" json:"target"` // base URL
	EntryPath    string   `yaml:"entry_path,omitempty" json:"entry_path,omitempty"`
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
	KnownScripts []string `yaml:"known_scripts,omitempty" json:"known_scripts,omitempty"` // "id/vN"
	ChangedPaths []string `yaml:"changed_paths,omitempty" json:"changed_paths,omitempty"`
	Routes       []string `yaml:"routes,omitempty" json:"routes,omitempty"`
	// Evidence is spec/diff/acceptance text the orchestrator collected for oracle provenance.
	Evidence string `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	// FailingScript / FailureDetail are set for repair tasks.
	FailingScript string `yaml:"failing_script,omitempty" json:"failing_script,omitempty"`
	FailureDetail string `yaml:"failure_detail,omitempty" json:"failure_detail,omitempty"`
	// ExistingScenarios lists current scenario YAML for verify/dedup context (bounded).
	ExistingScenarios []string `yaml:"existing_scenarios,omitempty" json:"existing_scenarios,omitempty"`
	Personas          []string `yaml:"personas,omitempty" json:"personas,omitempty"` // names only, never secrets
	Allowed           []string `yaml:"allowed" json:"allowed"`
	Forbidden         []string `yaml:"forbidden" json:"forbidden"`
	MaxScenarios      int      `yaml:"max_scenarios,omitempty" json:"max_scenarios,omitempty"`
	// Manual QA request fields (vigil request <yaml>).
	Instructions   string   `yaml:"instructions,omitempty" json:"instructions,omitempty"`     // the flow to perform, in plain language
	EntryURL       string   `yaml:"entry_url,omitempty" json:"entry_url,omitempty"`           // absolute URL to start at (overrides target+entry_path)
	Accounts       []string `yaml:"accounts,omitempty" json:"accounts,omitempty"`             // named test accounts the agent may enter as (no secrets)
	Mutation       string   `yaml:"mutation,omitempty" json:"mutation,omitempty"`             // read-only | reversible | destructive (explicitly allowed by the requester)
	Locks          []string `yaml:"locks,omitempty" json:"locks,omitempty"`                   // shared accounts/data the request occupies
	MaxToolCalls   int      `yaml:"max_tool_calls,omitempty" json:"max_tool_calls,omitempty"` // overrides agent.max_turns for this task
	TimeoutMinutes int      `yaml:"timeout_minutes,omitempty" json:"timeout_minutes,omitempty"`
	// MaxContinuations: how many times the session may be resumed with a fresh tool budget
	// before the final WRAP UP (overrides agent.continuations).
	MaxContinuations int `yaml:"max_continuations,omitempty" json:"max_continuations,omitempty"`
	// DomainRules is the bounded text of agent.domain_file; rendered as its own
	// prompt section (not inside the YAML dump of the request).
	DomainRules string `yaml:"-" json:"domain_rules,omitempty"`
}

// Finding kinds a data-analyst pass may report (PRD WI-E).
const (
	FindingDataMismatch  = "data_mismatch"
	FindingDomainRule    = "domain_rule"
	FindingDisplay       = "display"
	FindingAccessibility = "accessibility"
)

// KnownFindingKinds lists the accepted Finding.Kind values.
var KnownFindingKinds = map[string]bool{FindingDataMismatch: true, FindingDomainRule: true, FindingDisplay: true, FindingAccessibility: true}

// Finding is one data/domain/display/accessibility observation the agent reports
// next to its decision. Unknown kinds are normalised to display with the raw kind
// kept in Evidence.
type Finding struct {
	Kind     string `yaml:"kind" json:"kind"`
	Where    string `yaml:"where" json:"where"`
	Expected string `yaml:"expected" json:"expected"`
	Actual   string `yaml:"actual" json:"actual"`
	Evidence string `yaml:"evidence,omitempty" json:"evidence,omitempty"`
}

// MaxDomainFileBytes bounds the domain rules injected into the task prompt.
const MaxDomainFileBytes = 12 * 1024

// DomainTruncatedMarker terminates domain rules cut at MaxDomainFileBytes.
const DomainTruncatedMarker = "\n…[truncated]"

// Result is what the agent must emit (a fenced ```yaml block named vigil-result).
type Result struct {
	Decision         string            `yaml:"decision" json:"decision"`
	Evidence         string            `yaml:"evidence" json:"evidence"`
	CoverageDelta    string            `yaml:"coverage_delta" json:"coverage_delta"`
	OracleProvenance string            `yaml:"oracle_provenance" json:"oracle_provenance"`
	ScriptCandidates []string          `yaml:"script_candidates,omitempty" json:"script_candidates,omitempty"` // full DSL YAML docs
	ScriptPatch      string            `yaml:"script_patch,omitempty" json:"script_patch,omitempty"`           // full replacement YAML for repair
	Observed         map[string]string `yaml:"observed,omitempty" json:"observed,omitempty"`                   // console/network/dom notes
	VisitedURLs      []string          `yaml:"visited_urls,omitempty" json:"visited_urls,omitempty"`
	Ephemeral        bool              `yaml:"ephemeral,omitempty" json:"ephemeral,omitempty"`
	// Reproduction is the reproduce task's verdict on the reported symptom.
	Reproduction *Reproduction `yaml:"reproduction,omitempty" json:"reproduction,omitempty"`
	// Findings are data-analyst observations (value mismatches, domain-rule
	// violations, display/accessibility defects) with their evidence.
	Findings []Finding `yaml:"findings,omitempty" json:"findings,omitempty"`
	// Filled by the adapter, not the model:
	RawTranscriptPath string        `yaml:"-" json:"raw_transcript_path,omitempty"`
	Duration          time.Duration `yaml:"-" json:"duration"`
	ModelUnavailable  bool          `yaml:"-" json:"model_unavailable"` // 429/5xx/auth -> deterministic QA continues (rule 12)
	Continuations     int           `yaml:"-" json:"continuations"`     // session resumes with a fresh tool budget
	Compiled          bool          `yaml:"-" json:"compiled"`          // prose candidates were converted to DSL by a second call
	// HostViolations lists URLs outside allowed_hosts seen in tool calls or visited_urls;
	// when non-empty the adapter forces Decision NEEDS_REVIEW.
	HostViolations []string `yaml:"-" json:"host_violations,omitempty"`
	// Reason explains an adapter-forced decision (timeout, missing result block, host violation).
	Reason string `yaml:"-" json:"reason,omitempty"`
	// Sandbox records how the agent process was isolated: nono-wrap | nono-run | none.
	Sandbox   string `yaml:"-" json:"sandbox,omitempty"`
	Attempts  int    `yaml:"-" json:"attempts,omitempty"`
	ToolCalls int    `yaml:"-" json:"tool_calls,omitempty"`
	// Model is the chain entry (provider/model) that produced the result; ModelAttempts
	// lists every entry tried in order with its outcome (ok | unavailable | error | timeout | budget | no_result).
	Model         string         `yaml:"-" json:"model,omitempty"`
	ModelAttempts []ModelAttempt `yaml:"-" json:"model_attempts,omitempty"`
}

// Reproduction is what a reproduce task reports about the symptom it was given.
type Reproduction struct {
	Symptom    string `yaml:"symptom" json:"symptom"`
	Reproduced bool   `yaml:"reproduced" json:"reproduced"`
	AtStep     int    `yaml:"at_step,omitempty" json:"at_step,omitempty"` // 1-based step of the proposed scenario where the symptom shows
	Note       string `yaml:"note,omitempty" json:"note,omitempty"`
}

// ModelAttempt records one spawn of the agent against one chain entry.
type ModelAttempt struct {
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
}

// Outcomes recorded in Result.ModelAttempts.
const (
	OutcomeOK          = "ok"
	OutcomeUnavailable = "unavailable" // 429/quota/auth/5xx: entry cooled down, next entry tried
	OutcomeError       = "error"       // non-provider failure (spawn/exit/unclassified); same entry retried with backoff
	OutcomeTimeout     = "timeout"
	OutcomeBudget      = "budget"
	OutcomeNoResult    = "no_result"
)

// Adapter launches one bounded Browser Agent task.
type Adapter interface {
	Run(ctx context.Context, req Request, evidenceDir string) (*Result, error)
	// Plan runs one tool-less supervise task: briefing in, bounded plan out.
	Plan(ctx context.Context, briefing, evidenceDir string) (*Plan, error)
	// Reparse rebuilds the result files of a finished run from its transcript.
	Reparse(evidenceDir string) (*Result, error)
	// Doctor checks provider/tool availability without spending a task.
	Doctor(ctx context.Context) error
}
