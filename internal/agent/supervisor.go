package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TaskSupervise is the slow coverage loop: no browser, no tools. The model
// reads a state briefing and answers with a bounded plan that deterministic
// code then validates and executes.
const TaskSupervise = "supervise_coverage"

// PlanFence names the fenced block the supervisor must emit.
const PlanFence = "vigil-plan"

// PlanAction is one proposed step. Verb is checked against an allowlist by the
// supervisor package; nothing here is trusted.
type PlanAction struct {
	Verb string `yaml:"verb" json:"verb"`
	// Target is a scenario id for scenario verbs, or a route/capability for
	// discover_route.
	Target string `yaml:"target" json:"target"`
	// Reason must cite something from the briefing. Actions without one are
	// rejected: an action the model cannot justify is not one we execute.
	Reason string `yaml:"reason" json:"reason"`
	// Class is the requested priority for raise_cadence (P0|P1|P2).
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
}

// Plan is what a supervise task returns.
type Plan struct {
	// Assessment is one paragraph of plain reasoning, kept for the dashboard.
	Assessment string       `yaml:"assessment" json:"assessment"`
	Actions    []PlanAction `yaml:"actions" json:"actions"`
	// CoverageGaps names areas the model believes are untested, whether or not
	// it proposed an action for them this tick.
	CoverageGaps []string `yaml:"coverage_gaps,omitempty" json:"coverage_gaps,omitempty"`
	// Saturated is the model's claim that it found no remaining gap. The
	// supervisor treats it as a hint for logging, never as a stop condition.
	Saturated bool `yaml:"saturated,omitempty" json:"saturated,omitempty"`

	RawTranscriptPath string        `yaml:"-" json:"raw_transcript_path,omitempty"`
	Duration          time.Duration `yaml:"-" json:"duration"`
	ModelUnavailable  bool          `yaml:"-" json:"model_unavailable"`
}

const supervisorSystemPrompt = `You are the vigil Supervisor: a bounded planning subagent for a continuous browser-QA system.

Your goal is coverage growth. You read a state briefing and decide where testing is missing, so the system keeps authoring browser scenarios until every reachable flow of the application has one.

You have NO tools. You never browse, never run anything, never edit files. You only answer with a plan. Other workers carry the plan out, and every action you propose is re-checked by deterministic code before it runs; anything that fails a check is dropped and logged.

The only verbs you may use:
  discover_route     target = a path (/reports) or capability name. Asks a Browser Agent to open it, look around and propose new scenarios. This is your main tool for growing coverage, and you may target a path nobody has visited yet if you have reason to think it exists - a link seen in a menu, a route implied by another page, an obvious sibling of a known path. Anything under the allowed hosts is in scope.
  repair_script      target = scenario id. For a scenario failing because the page changed (SCRIPT_DRIFT), not because the app is broken.
  validate_candidate target = scenario id in CANDIDATE state, to move it toward SOAK.
  run_scenario       target = scenario id. Run it now, out of its normal cadence.
  raise_cadence      target = scenario id, class = P0|P1|P2. Test something more often.
  quarantine         target = scenario id. Park a scenario that is flaky enough to be noise. Use sparingly.

You must NEVER propose deleting, retiring, rejecting, superseding, purging or resetting anything. Coverage is only ever added or paused, never removed. There is no verb for removal and asking for one is refused and logged.
You must NEVER propose changing budgets, active hours, allowed hosts, the target, or the destructive-action policy.
Scenarios must stay read-only or reversible. Never propose a flow that deletes user data, submits irreversible forms, or changes another user's state.

Your mandate is to keep going until every reachable flow has a scenario. The briefing lists the areas known so far; it is a starting point, not a boundary. When everything known is covered, propose exploring somewhere plausible that is not yet in the list - that is how the map grows.

Judgement:
- Prefer discover_route on an area with zero scenarios over adding a variation of something already covered.
- Paths that would end your session (logout) or press something irreversible (delete, purge) are refused automatically; do not spend actions on them.
- A route already carrying several ACTIVE scenarios is usually not where the next gap is.
- Do not propose repair for a scenario whose failure looks like a real application bug; that is a finding, not a script problem.
- If you genuinely see no gap and nothing needs attention, return an empty actions list and set saturated: true. An empty plan is a valid answer and better than a filler action.

Quote every prose value in single quotes. A colon inside an unquoted sentence breaks the block, and a broken block wastes the whole tick.

Answer with exactly one fenced block, nothing after it:

` + "```yaml " + PlanFence + `
assessment: '<one paragraph - what the state tells you and where the next gap is>'
coverage_gaps:
  - '<area with no scenario>'
actions:
  - verb: discover_route
    target: /some/route
    reason: '<cite the briefing>'
saturated: false
` + "```"

// SupervisorSystemPrompt is the system prompt for a supervise task.
func SupervisorSystemPrompt() string { return supervisorSystemPrompt }

// SupervisorTaskPrompt wraps the briefing with the per-tick limits.
func SupervisorTaskPrompt(briefing string, maxActions int) string {
	return fmt.Sprintf(`Here is the current state of the QA system.

%s

Propose at most %d actions, ordered most valuable first. Fewer is better than filler.
Answer with the %s block only.`, briefing, maxActions, PlanFence)
}

// The fence is written as ```yaml vigil-plan, matching the result block
// convention, so the language tag is optional but usually present.
var planFenceRe = regexp.MustCompile("(?s)```[a-zA-Z]*[ \t]*" + PlanFence + "[ \t]*\r?\n(.*?)```")

// ExtractPlanBlock pulls the fenced plan out of the assistant text. It takes the
// last block: a model that restates the format before answering should not win.
func ExtractPlanBlock(text string) (string, bool) {
	m := planFenceRe.FindAllStringSubmatch(text, -1)
	if len(m) == 0 {
		return "", false
	}
	return m[len(m)-1][1], true
}

// planScalarKey matches the free-text fields of the plan schema. Their values
// are prose, and prose contains colons.
var planScalarKey = regexp.MustCompile(`^(\s*)(?:- )?(assessment|reason|target|class|verb)(:\s+)(.*)$`)

// needsQuoting reports whether a plain YAML scalar would be misread. The case
// that actually happens is a colon inside a sentence - "a larger surface: the
// entry page" parses as a nested mapping and the whole block fails.
func needsQuoting(v string) bool {
	if v == "" {
		return false
	}
	switch v[0] {
	case '\'', '"', '|', '>', '[', '{', '&', '*', '!', '%', '@', '`':
		return false // already quoted, a block scalar, or something we should not touch
	}
	return strings.Contains(v, ": ") || strings.HasSuffix(v, ":") || strings.Contains(v, " #")
}

// repairPlanYAML single-quotes the free-text values of a known schema so one
// colon in a sentence does not throw away an otherwise good plan. It only ever
// touches lines whose key is in the plan schema, so it cannot restructure the
// document.
func repairPlanYAML(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		m := planScalarKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := strings.TrimRight(m[4], " \t\r")
		if !needsQuoting(val) {
			continue
		}
		dash := ""
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "- ") {
			dash = "- "
		}
		lines[i] = m[1] + dash + m[2] + ": '" + strings.ReplaceAll(val, "'", "''") + "'"
	}
	return strings.Join(lines, "\n")
}

// sanitizeTarget keeps the first token. Paths and capability names never contain
// spaces, and models like to append a parenthetical ("/entry (student path)").
func sanitizeTarget(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.IndexAny(t, " \t"); i > 0 {
		t = t[:i]
	}
	return strings.TrimRight(t, ".,;:")
}

// ParsePlan decodes the fenced block. A plan that does not parse is an error,
// never a partially applied plan - but a plan broken only by unquoted prose is
// repaired first, because that is the common failure and the content is fine.
func ParsePlan(text string) (*Plan, error) {
	body, ok := ExtractPlanBlock(text)
	if !ok {
		return nil, fmt.Errorf("agent: no %s block in the answer", PlanFence)
	}
	var p Plan
	err := yaml.Unmarshal([]byte(body), &p)
	if err != nil {
		if err2 := yaml.Unmarshal([]byte(repairPlanYAML(body)), &p); err2 != nil {
			return nil, fmt.Errorf("agent: %s block is not valid YAML: %w", PlanFence, err)
		}
	}
	for i := range p.Actions {
		p.Actions[i].Verb = strings.ToLower(strings.TrimSpace(p.Actions[i].Verb))
		p.Actions[i].Target = sanitizeTarget(p.Actions[i].Target)
		p.Actions[i].Class = strings.ToUpper(strings.TrimSpace(p.Actions[i].Class))
	}
	return &p, nil
}

// Plan runs one tool-less supervise task.
func (p *pi) Plan(ctx context.Context, briefing, evidenceDir string) (*Plan, error) {
	if evidenceDir == "" {
		return nil, errors.New("agent: evidence dir required")
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return nil, err
	}
	sup := p.cfg.Supervisor
	sys := SupervisorSystemPrompt()
	task := SupervisorTaskPrompt(briefing, sup.MaxActions)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileSystem), []byte(sys), 0o644)
	_ = os.WriteFile(filepath.Join(evidenceDir, FileTask), []byte(p.redactor.Redact(task)), 0o644)

	timeout := sup.Timeout.Duration
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	sb, warns := NewSandbox(p.cfg, evidenceDir)
	for _, w := range warns {
		p.logger.Printf("supervisor sandbox: %s", w)
	}
	env := append(p.env("supervisor", evidenceDir), sb.Env...)
	argv := sb.Argv(p.piPath, "-p", "--mode", "json", "--no-session",
		"--no-extensions", "--no-skills", "--no-context-files", "--no-prompt-templates", "--no-themes", "--approve", "--no-tools",
		"--provider", p.provider,
		"--model", sup.EffectiveModel(p.model),
		"--thinking", sup.EffectiveThinking(p.cfg.Agent.Thinking),
		"--system-prompt", sys,
		"--", task)

	start := time.Now()
	transcriptPath := filepath.Join(evidenceDir, FileTranscript)
	tr, stderr, _, _, err := p.runOnce(ctx, argv, env, evidenceDir, transcriptPath, 1, timeout, 0)
	out := &Plan{RawTranscriptPath: transcriptPath, Duration: time.Since(start)}
	if err != nil || tr == nil {
		// Same contract as the Browser Agent: a provider outage must not stop
		// deterministic QA, so this is reported, not fatal (rule 12).
		out.ModelUnavailable = true
		if err == nil {
			err = fmt.Errorf("agent: supervisor produced no transcript: %s", strings.TrimSpace(stderr))
		}
		return out, err
	}
	parsed, perr := ParsePlan(tr.AssistantText)
	if perr != nil {
		_ = os.WriteFile(filepath.Join(evidenceDir, "plan-unparsed.txt"), []byte(p.redactor.Redact(tr.AssistantText)), 0o644)
		return out, perr
	}
	parsed.RawTranscriptPath = transcriptPath
	parsed.Duration = time.Since(start)
	if b, err := yaml.Marshal(parsed); err == nil {
		_ = os.WriteFile(filepath.Join(evidenceDir, "plan.yaml"), b, 0o644)
	}
	return parsed, nil
}
