package agent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"vigil/internal/config"
)

// ModelEntry is one link of the fallback chain: provider/model with the
// thinking level the pi invocation should use.
type ModelEntry struct {
	Provider string
	Model    string
	Thinking string
}

// Name is the cooldown key: provider/model (thinking does not affect availability).
func (e ModelEntry) Name() string { return e.Provider + "/" + e.Model }

// String renders provider/model:thinking.
func (e ModelEntry) String() string { return e.Name() + ":" + e.Thinking }

// ParseModelEntry parses `provider/model[:thinking]`; an entry without a level
// gets defaultThinking. A bare `model` (no provider) keeps the legacy zai default.
func ParseModelEntry(entry, defaultThinking string) (ModelEntry, error) {
	if !strings.Contains(entry, "/") {
		entry = "zai/" + strings.TrimSpace(entry) // legacy agent.model without a provider defaults to zai
	}
	provider, model, thinking, err := config.SplitModelEntry(entry)
	if err != nil {
		return ModelEntry{}, err
	}
	if thinking == "" {
		thinking = defaultThinking
	}
	return ModelEntry{Provider: provider, Model: model, Thinking: thinking}, nil
}

// Chain is the ordered model fallback list plus an in-memory cooldown map. A
// model that answered with a provider error (429, quota, auth, 5xx) is skipped
// for the cooldown period so every task in the process does not re-hit it.
type Chain struct {
	mu       sync.Mutex
	entries  []ModelEntry
	cooldown time.Duration
	until    map[string]time.Time
	now      func() time.Time
}

// NewChain parses the entries in order. It fails on an empty or malformed list
// so a misconfiguration surfaces at startup, not on the first agent task.
func NewChain(entries []string, defaultThinking string, cooldown time.Duration) (*Chain, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("agent: model chain is empty")
	}
	c := &Chain{cooldown: cooldown, until: map[string]time.Time{}, now: time.Now}
	for i, raw := range entries {
		e, err := ParseModelEntry(raw, defaultThinking)
		if err != nil {
			return nil, fmt.Errorf("agent: models[%d]: %w", i, err)
		}
		c.entries = append(c.entries, e)
	}
	return c, nil
}

// NewChainFromConfig builds the chain from agent.models (or the legacy agent.model).
func NewChainFromConfig(cfg *config.Config) (*Chain, error) {
	return NewChain(cfg.ModelEntries(), cfg.Agent.Thinking, cfg.Agent.ModelCooldown.Duration)
}

// Entries returns a copy of the ordered chain.
func (c *Chain) Entries() []ModelEntry { return append([]ModelEntry(nil), c.entries...) }

// Len is the number of chain entries.
func (c *Chain) Len() int { return len(c.entries) }

// Next returns the first entry that is not cooling down and not in exclude.
// ok=false means every entry is unavailable: the caller must report
// ModelUnavailable and let deterministic QA continue (rule 12).
func (c *Chain) Next(exclude ...string) (ModelEntry, bool) {
	return c.NextFrom(c.entries, exclude...)
}

// NextFrom is Next over a caller-supplied list (the supervisor prepends its own
// model) while sharing this chain's cooldown state.
func (c *Chain) NextFrom(entries []ModelEntry, exclude ...string) (ModelEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, e := range entries {
		if until, cooling := c.until[e.Name()]; cooling && now.Before(until) {
			continue
		}
		skip := false
		for _, x := range exclude {
			if x == e.Name() {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		return e, true
	}
	return ModelEntry{}, false
}

// MarkUnavailable puts model (provider/model) on cooldown. reason is kept only
// for the caller's log line; the chain stores no error text.
func (c *Chain) MarkUnavailable(model, reason string) {
	_ = reason
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[model] = c.now().Add(c.cooldown)
}

// CoolingDown reports whether model is currently skipped.
func (c *Chain) CoolingDown(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[model]
	return ok && c.now().Before(until)
}

// Names lists provider/model for every entry (doctor, logs).
func (c *Chain) Names() []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Name())
	}
	return out
}
