// Package model holds the persistent QA domain model (PRD §8, §9, §13, §17).
package model

import "time"

// ---- enumerations -------------------------------------------------------

type ScenarioState string

const (
	StateCandidate   ScenarioState = "CANDIDATE"
	StateSoak        ScenarioState = "SOAK" // validated, accumulating clean runs before ACTIVE
	StateActive      ScenarioState = "ACTIVE"
	StateDuplicate   ScenarioState = "DUPLICATE"
	StateEphemeral   ScenarioState = "EPHEMERAL"
	StateNeedsReview ScenarioState = "NEEDS_REVIEW"
	StateRejected    ScenarioState = "REJECTED"
	StateQuarantined ScenarioState = "QUARANTINED"
	StateMerged      ScenarioState = "MERGED"
	StateSuperseded  ScenarioState = "SUPERSEDED"
	StateRetired     ScenarioState = "RETIRED"
)

// Outcome is the classified result of one run (PRD §13).
type Outcome string

const (
	OutcomePass                   Outcome = "PASS"
	OutcomeAppFailure             Outcome = "APP_FAILURE"
	OutcomeQAFlake                Outcome = "QA_FLAKE"
	OutcomeScriptDrift            Outcome = "SCRIPT_DRIFT"
	OutcomeLightpandaIncompatible Outcome = "LIGHTPANDA_INCOMPATIBLE"
	OutcomeBrowserAmbiguous       Outcome = "BROWSER_AMBIGUOUS"
	OutcomeAuthFailure            Outcome = "AUTH_FAILURE"
	OutcomeDataFailure            Outcome = "DATA_FAILURE"
	OutcomeEnvFailure             Outcome = "ENV_FAILURE"
	OutcomeDeploymentNotReady     Outcome = "DEPLOYMENT_NOT_READY"
	OutcomeOracleUnknown          Outcome = "ORACLE_UNKNOWN"
	OutcomeNeedsReview            Outcome = "NEEDS_REVIEW"
)

// IsInfrastructure reports whether the outcome is an environment/auth/data/deployment
// classification that must be separated from product regressions (AC-15).
func (o Outcome) IsInfrastructure() bool {
	switch o {
	case OutcomeAuthFailure, OutcomeDataFailure, OutcomeEnvFailure, OutcomeDeploymentNotReady:
		return true
	}
	return false
}

// Action is the orchestrator decision (PRD §4 decision table).
type Action string

const (
	ActionBrowserAgentDiscover     Action = "BROWSER_AGENT_DISCOVER"
	ActionRunScript                Action = "RUN_SCRIPT"
	ActionRunImpactedScriptsFirst  Action = "RUN_IMPACTED_SCRIPTS_FIRST"
	ActionBrowserAgentVerifyChange Action = "BROWSER_AGENT_VERIFY_CHANGE"
	ActionBrowserAgentRepair       Action = "BROWSER_AGENT_REPAIR"
	ActionCompileValidatePromote   Action = "COMPILE_VALIDATE_PROMOTE"
	ActionEphemeral                Action = "EPHEMERAL"
	ActionChromiumConfirm          Action = "CHROMIUM_CONFIRM"
	ActionChromiumVerify           Action = "CHROMIUM_VERIFY"
	ActionClassifyInfra            Action = "CLASSIFY_INFRA"
	ActionNone                     Action = "NO_ACTION"
)

type Browser string

const (
	BrowserLightpanda Browser = "lightpanda"
	BrowserChromium   Browser = "chromium"
)

type JobKind string

const (
	JobRunScenario       JobKind = "RUN_SCENARIO"
	JobAgentDiscover     JobKind = "AGENT_DISCOVER"
	JobAgentVerify       JobKind = "AGENT_VERIFY"
	JobAgentRepair       JobKind = "AGENT_REPAIR"
	JobChromiumConfirm   JobKind = "CHROMIUM_CONFIRM"
	JobChromiumEvidence  JobKind = "CHROMIUM_EVIDENCE" // real-screen evidence capture, once per deployment
	JobValidateCandidate JobKind = "VALIDATE_CANDIDATE"
)

type JobState string

const (
	JobReady     JobState = "READY"
	JobLeased    JobState = "LEASED"
	JobDone      JobState = "DONE"
	JobFailed    JobState = "FAILED"
	JobCancelled JobState = "CANCELLED"
)

type Readiness string

const (
	ReadinessWaiting Readiness = "WAITING_FOR_DEPLOYMENT"
	ReadinessReady   Readiness = "READY"
	ReadinessUnknown Readiness = "DEPLOYMENT_UNKNOWN"
)

type Mutation string

const (
	MutationReadOnly    Mutation = "read-only"
	MutationReversible  Mutation = "reversible"
	MutationDestructive Mutation = "destructive"
)

// Priority: higher runs first (PRD §12).
const (
	PriorityUserRequest       = 110
	PriorityRecentFailure     = 100
	PriorityNewDirectCoverage = 90
	PrioritySoak              = 80
	PriorityImpacted          = 70
	PriorityP0                = 60
	PriorityP1                = 50
	PriorityP2                = 40
	PriorityBackground        = 10
)

// LinkType for coverage links (PRD §8).
type LinkType string

const (
	LinkFeature    LinkType = "feature"
	LinkCapability LinkType = "capability"
	LinkRoute      LinkType = "route"
	LinkAPI        LinkType = "api"
	LinkPath       LinkType = "path"
	LinkPersona    LinkType = "persona"
)

type IncidentKind string

const (
	IncidentAppRegression IncidentKind = "APP_REGRESSION"
	IncidentEnvironment   IncidentKind = "ENVIRONMENT"
)

// ---- entities ----------------------------------------------------------

type Feature struct {
	ID               string
	ProjectID        string
	Status           string // shipped
	LatestShippedSHA string
	ShippedAt        time.Time
	ChangedPaths     []string
	Routes           []string
	Summary          string
	Source           string // adapter name
	Readiness        Readiness
	ReadyAt          *time.Time
	// LastHandledSHA is the SHA for which the orchestrator already planned work.
	LastHandledSHA string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Scenario struct {
	ID                  string
	ProjectID           string
	State               ScenarioState
	Fingerprint         string
	Title               string
	Class               string // P0 | P1 | P2
	Mutation            Mutation
	Locks               []string
	CurrentVersion      int
	OracleSource        string // spec | approved_qa | contract | observation | ""
	OracleFeature       string
	OracleSHA           string
	SoakPasses          int
	SoakTarget          int
	LastOutcome         Outcome
	LastRunAt           *time.Time
	LastPassAt          *time.Time
	ConsecutiveFailures int
	NextDueAt           *time.Time
	Origin              string // agent | seed | import | human
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ScenarioVersion struct {
	ScenarioID  string
	Version     int
	YAML        string
	Fingerprint string
	CreatedBy   string // agent | human | seed | repair
	Reason      string
	CreatedAt   time.Time
}

type ScenarioVariant struct {
	ID         string
	ScenarioID string
	Name       string
	Params     map[string]string
}

type Flow struct {
	ID             string
	ProjectID      string
	CurrentVersion int
	UpdatedAt      time.Time
}

type FlowVersion struct {
	FlowID    string
	Version   int
	YAML      string
	CreatedAt time.Time
}

type CoverageLink struct {
	ScenarioID string
	LinkType   LinkType
	LinkValue  string
}

type Job struct {
	ID             int64
	ProjectID      string
	Kind           JobKind
	State          JobState
	Priority       int
	ScheduledAt    time.Time
	ScenarioID     string
	FeatureID      string
	Browser        Browser
	Payload        string // JSON, kind-specific
	Attempt        int
	MaxAttempts    int
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Run struct {
	ID              int64
	JobID           int64
	ProjectID       string
	ScenarioID      string
	ScenarioVersion int
	FeatureID       string
	ShippedSHA      string
	Browser         Browser
	Outcome         Outcome
	Attempt         int
	StartedAt       time.Time
	FinishedAt      time.Time
	DurationMs      int64
	FailedStep      int    // 1-based; 0 = none
	FailedAction    string // e.g. click / assert_text
	Expected        string
	Actual          string
	Error           string
	EvidenceDir     string
	// DeployMarker identifies the deployed build the run verified (asset version / release id).
	DeployMarker string
}

type RunArtifact struct {
	RunID int64
	Kind  string // dom | console | network | screenshot | agent | popup
	Path  string
}

type Incident struct {
	ID              int64
	ProjectID       string
	Kind            IncidentKind
	ScenarioID      string
	ScenarioVersion int
	FeatureID       string
	ShippedSHA      string
	RunID           int64
	Title           string
	Summary         string
	State           string // OPEN | RESOLVED
	MarkdownPath    string
	JSONPath        string
	CreatedAt       time.Time
	ResolvedAt      *time.Time
}

type ResourceLock struct {
	Key       string
	Owner     string
	ExpiresAt time.Time
}

type ScenarioMetrics struct {
	ScenarioID        string
	Runs              int
	Passes            int
	Failures          int
	Flakes            int
	RegressionsCaught int
	MedianDurationMs  int64
	LastVerifiedAt    *time.Time
}

type Worker struct {
	ID            string
	Kind          string
	LastHeartbeat time.Time
}

// FeatureEvent is the normalized ingestion input (PRD §7).
type FeatureEvent struct {
	FeatureID    string    `yaml:"feature_id" json:"feature_id"`
	Status       string    `yaml:"status" json:"status"`
	ShippedSHA   string    `yaml:"shipped_sha" json:"shipped_sha"`
	ShippedAt    time.Time `yaml:"shipped_at" json:"shipped_at"`
	ChangedPaths []string  `yaml:"changed_paths" json:"changed_paths"`
	Routes       []string  `yaml:"routes" json:"routes"`
	Summary      string    `yaml:"summary" json:"summary"`
	Source       string    `yaml:"source,omitempty" json:"source,omitempty"`
}
