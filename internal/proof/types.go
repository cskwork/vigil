// Package proof implements request-scoped, explicitly approved QA checks.
package proof

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"vigil/internal/dsl"
)

type Value struct {
	Present bool            `json:"present"`
	Data    json.RawMessage `json:"data"`
}

func (v Value) Validate() error {
	if !v.Present {
		if len(v.Data) > 0 && string(v.Data) != "null" {
			return fmt.Errorf("missing value cannot carry data")
		}
		return nil
	}
	if !json.Valid(v.Data) {
		return fmt.Errorf("present value needs valid JSON data")
	}
	return nil
}
func Equal(a, b Value) bool {
	if a.Present != b.Present {
		return false
	}
	if !a.Present {
		return true
	}
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a.Data))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b.Data))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

type Criterion struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Required  bool   `json:"required"`
	Observer  string `json:"observer"`
	Expected  Value  `json:"expected"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
}
type Contract struct {
	Persona  string      `json:"persona"`
	Fixture  string      `json:"fixture"`
	Actions  []string    `json:"actions"`
	Criteria []Criterion `json:"criteria"`
}
type Plan struct {
	RegistryHash string      `json:"registry_hash"`
	Version      int         `json:"version"`
	Actions      []string    `json:"actions"`
	Criteria     []Criterion `json:"criteria"`
	Unsupported  []string    `json:"unsupported"`
}
type Persona struct {
	Account string            `json:"account"`
	Setup   []dsl.Step        `json:"setup"`
	Secrets map[string]string `json:"secret_env"`
}
type Fixture struct {
	Entity     string `json:"entity"`
	QA         bool   `json:"qa"`
	PrepareURL string `json:"prepare_url,omitempty"`
}
type Action struct {
	Title    string     `json:"title"`
	Mutating bool       `json:"mutating"`
	Steps    []dsl.Step `json:"steps"`
}
type ScopeRule struct {
	Origin string            `json:"origin"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
}
type Observer struct {
	WriteAPI       *ScopeRule `json:"write_api,omitempty"`
	EntityPath     string     `json:"entity_path,omitempty"`
	GenerationPath string     `json:"generation_path,omitempty"`

	Kind        string     `json:"kind"`
	Action      string     `json:"action"`
	Selector    string     `json:"selector,omitempty"`
	Property    string     `json:"property,omitempty"`
	API         *ScopeRule `json:"api,omitempty"`
	JSONPath    string     `json:"json_path,omitempty"`
	SuccessPath string     `json:"success_path,omitempty"`
	Success     Value      `json:"success,omitempty"`
	Reread      bool       `json:"reread,omitempty"`
	Probe       string     `json:"probe,omitempty"`
}
type Definition struct {
	Expected Value  `json:"expected"`
	Title    string `json:"title"`
}
type Target struct {
	RegistryHash    string    `json:"registry_hash,omitempty"`
	Template        *Contract `json:"template,omitempty"`
	VersionSelector string    `json:"version_selector,omitempty"`

	Title         string                `json:"title"`
	BaseURL       string                `json:"base_url"`
	Environment   string                `json:"environment"`
	Version       string                `json:"version"`
	PolicyVersion string                `json:"policy_version"`
	Scope         []ScopeRule           `json:"scope"`
	Personas      map[string]Persona    `json:"personas"`
	Fixtures      map[string]Fixture    `json:"fixtures"`
	Actions       map[string]Action     `json:"actions"`
	Observers     map[string]Observer   `json:"observers"`
	Definitions   map[string]Definition `json:"definitions"`
}
type Registry struct {
	Targets   map[string]Target `json:"targets"`
	PiCommand string            `json:"pi_command,omitempty"`
	PiModel   string            `json:"pi_model,omitempty"`
	Probes    map[string]Probe  `json:"probes,omitempty"`
}
type Check struct {
	LatestVerdict   string     `json:"latest_verdict,omitempty"`
	MissingCount    int        `json:"missing_count"`
	RevisionHistory []Revision `json:"revision_history,omitempty"`

	ID              string          `json:"id"`
	TargetRef       string          `json:"target_ref"`
	Request         string          `json:"request"`
	CurrentRevision int             `json:"current_revision"`
	Draft           Contract        `json:"draft"`
	ContractHash    string          `json:"contract_hash"`
	CreatedBy       string          `json:"created_by"`
	RowVersion      int             `json:"row_version"`
	CreatedAt       time.Time       `json:"created_at"`
	Planning        string          `json:"planning"`
	Suggestions     json.RawMessage `json:"suggestions,omitempty"`
	PlanError       string          `json:"plan_error,omitempty"`
}
type Approval struct {
	RegistryHash   string `json:"registry_hash"`
	Revision       int    `json:"revision"`
	ContractHash   string `json:"contract_hash"`
	Scope          string `json:"scope"`
	Baseline       string `json:"baseline,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}
type Evidence struct {
	Screenshot   string `json:"screenshot,omitempty"`
	Availability string `json:"availability,omitempty"`

	Details json.RawMessage `json:"details,omitempty"`

	ID        string    `json:"id"`
	Attempt   string    `json:"attempt"`
	Criterion string    `json:"criterion"`
	Action    string    `json:"action"`
	Persona   string    `json:"persona"`
	Entity    string    `json:"entity"`
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Actual    Value     `json:"actual"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Artifact  string    `json:"artifact,omitempty"`
}
type CriterionResult struct {
	ID       string     `json:"id"`
	Required bool       `json:"required"`
	Status   string     `json:"status"`
	Reason   string     `json:"reason,omitempty"`
	Evidence []Evidence `json:"evidence"`
}
type Revision struct {
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Reason   string    `json:"reason"`
	Contract Contract  `json:"contract"`
}
type DispositionEvent struct {
	Actor       string    `json:"actor"`
	At          time.Time `json:"at"`
	Disposition string    `json:"disposition"`
	Reason      string    `json:"reason"`
}
type Attempt struct {
	DispositionHistory []DispositionEvent `json:"disposition_history,omitempty"`

	PlanHash          string `json:"plan_hash"`
	ObservedVersion   string `json:"observed_version,omitempty"`
	VersionAfter      string `json:"version_after,omitempty"`
	FixtureGeneration string `json:"fixture_generation,omitempty"`

	ID              string            `json:"id"`
	CheckID         string            `json:"check_id"`
	JobID           int64             `json:"job_id"`
	Approval        Approval          `json:"approval"`
	Actor           string            `json:"actor"`
	ApprovedAt      time.Time         `json:"approved_at"`
	Contract        Contract          `json:"contract"`
	Plan            Plan              `json:"plan"`
	Target          Target            `json:"target"`
	Fixture         Fixture           `json:"fixture"`
	State           string            `json:"state"`
	Progress        string            `json:"progress"`
	CancelRequested bool              `json:"cancel_requested"`
	Results         []CriterionResult `json:"results"`
	Verdict         string            `json:"verdict"`
	Disposition     string            `json:"disposition"`
	FinishedAt      *time.Time        `json:"finished_at,omitempty"`
	BaselineKind    string            `json:"baseline_kind"`
	FixClaim        string            `json:"fix_claim"`
}

func Hash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func Verdict(rs []CriterionResult) string {
	n := 0
	unknown := false
	for _, r := range rs {
		if !r.Required {
			continue
		}
		n++
		if r.Status == "FAIL" {
			return "FAIL"
		}
		if r.Status != "PASS" {
			unknown = true
		}
	}
	if n == 0 || unknown {
		return "INCOMPLETE"
	}
	return "PASS"
}
func (r Registry) Compile(target string, c Contract, request string) (Plan, error) {
	t, ok := r.Targets[target]
	if !ok {
		return Plan{}, fmt.Errorf("unknown target")
	}
	if _, ok = t.Personas[c.Persona]; !ok {
		return Plan{}, fmt.Errorf("unknown persona")
	}
	f, ok := t.Fixtures[c.Fixture]
	if !ok {
		return Plan{}, fmt.Errorf("unknown fixture")
	}
	count := len(t.Personas[c.Persona].Setup)
	for _, action := range c.Actions {
		count += len(t.Actions[action].Steps)
	}
	if count > 20 {
		return Plan{}, fmt.Errorf("proof allows at most 20 browser actions including persona setup")
	}
	p := Plan{RegistryHash: Hash(t), Version: 1, Actions: c.Actions, Criteria: c.Criteria, Unsupported: []string{}}
	seen := map[string]bool{}
	required := 0
	for _, a := range c.Actions {
		v, ok := t.Actions[a]
		if !ok {
			return p, fmt.Errorf("unknown action %q", a)
		}
		if seen["a:"+a] {
			return p, fmt.Errorf("duplicate action")
		}
		seen["a:"+a] = true
		if v.Mutating && (!f.QA || !writeEnvironment(t.Environment)) {
			return p, fmt.Errorf("writes require a QA fixture and non-production target")
		}
	}
	for _, v := range c.Criteria {
		if v.ID == "" || seen[v.ID] {
			return p, fmt.Errorf("criteria need unique IDs")
		}
		seen[v.ID] = true
		if v.Required {
			required++
		}
		if err := v.Expected.Validate(); err != nil {
			return p, err
		}
		switch v.Source {
		case "request":
			if v.SourceRef == "" || !strings.Contains(request, v.SourceRef) {
				return p, fmt.Errorf("criterion %s must cite exact request text", v.ID)
			}
		case "definition":
			d, ok := t.Definitions[v.SourceRef]
			if !ok || !Equal(d.Expected, v.Expected) {
				return p, fmt.Errorf("unapproved definition")
			}
		default:
			return p, fmt.Errorf("expected values require request or definition provenance")
		}
		o, ok := t.Observers[v.Observer]
		if !ok {
			p.Unsupported = append(p.Unsupported, v.ID+": observer unavailable")
			continue
		}
		if !seen["a:"+o.Action] {
			return p, fmt.Errorf("observer action must be approved")
		}
		if o.Kind != "dom" && o.Kind != "network" && o.Kind != "mysql" {
			p.Unsupported = append(p.Unsupported, v.ID+": unsupported observer")
		}
	}
	if required == 0 || required > 5 {
		return p, fmt.Errorf("contract requires 1 to 5 required criteria")
	}
	return p, nil
}
func Allowed(raw, method string, rules []ScopeRule) bool {
	u, e := url.Parse(raw)
	if e != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, r := range rules {
		if u.Scheme+"://"+u.Host != r.Origin || method != r.Method || u.EscapedPath() != r.Path {
			continue
		}
		q := u.Query()
		if len(q) != len(r.Query) {
			continue
		}
		ok := true
		for k, v := range r.Query {
			if len(q[k]) != 1 || q.Get(k) != v {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func writeEnvironment(environment string) bool {
	switch environment {
	case "dev", "development", "qa", "test":
		return true
	default:
		return false
	}
}
