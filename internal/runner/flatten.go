package runner

import (
	"fmt"
	"regexp"
	"strings"

	"vigil/internal/dsl"
)

// flatStep is one executable step after flow expansion.
type flatStep struct {
	Step dsl.Step
	Name string // display name (flow-prefixed for expanded steps)
}

// flatten expands `uses` flows (in order, before the scenario steps) and inline
// `use_flow` steps. Unknown flows are a caller bug: the DSL validator rejects them.
func flatten(sc *dsl.Scenario, flows map[string]*dsl.Flow) ([]flatStep, error) {
	var out []flatStep
	expand := func(id string) error {
		f, ok := flows[id]
		if !ok || f == nil {
			return fmt.Errorf("runner: unknown flow %q", id)
		}
		for _, st := range f.Steps {
			if st.UseFlow != "" {
				return fmt.Errorf("runner: flow %q nests use_flow %q", id, st.UseFlow)
			}
			name := st.Name
			if name == "" {
				name = st.Kind()
			}
			out = append(out, flatStep{Step: st, Name: "flow:" + id + "/" + name})
		}
		return nil
	}
	for _, id := range sc.Uses {
		if err := expand(id); err != nil {
			return nil, err
		}
	}
	for _, st := range sc.Steps {
		if st.UseFlow != "" {
			if err := expand(st.UseFlow); err != nil {
				return nil, err
			}
			continue
		}
		name := st.Name
		if name == "" {
			name = st.Kind()
		}
		out = append(out, flatStep{Step: st, Name: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("runner: scenario %s has no executable steps", sc.Scenario.ID)
	}
	return out, nil
}

var personaRe = regexp.MustCompile(`\{\{\s*persona\.(?:extra\.)?([A-Za-z0-9_.-]+)\s*\}\}`)

// substitutePersona replaces {{persona.<key>}} (and {{persona.extra.<key>}})
// with values from the persona map. It returns the missing keys, if any.
func substitutePersona(s string, persona map[string]string) (string, []string) {
	var missing []string
	out := personaRe.ReplaceAllStringFunc(s, func(m string) string {
		key := personaRe.FindStringSubmatch(m)[1]
		if v, ok := persona[key]; ok {
			return v
		}
		if v, ok := persona["extra."+key]; ok {
			return v
		}
		missing = append(missing, key)
		return m
	})
	return out, missing
}

// redactor replaces persona values in artifact text with a placeholder so
// secrets never reach disk (PRD §14).
type redactor struct{ values []string }

func newRedactor(persona map[string]string) *redactor {
	r := &redactor{}
	for _, v := range persona {
		if len(v) >= 3 {
			r.values = append(r.values, v)
		}
	}
	return r
}

// queryParamRe matches one query-string parameter (name=value) inside free text.
var queryParamRe = regexp.MustCompile(`([?&]|&amp;)([^=&#\s"'<>]+)=([^&#\s"'<>]+)`)

// jwtRe matches JWT-shaped literals (header.payload.signature) anywhere in text,
// e.g. tokens embedded in inline scripts or attributes of a captured DOM.
var jwtRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)

// isCredentialParam reports whether a query parameter name looks like a credential.
// "author*" is explicitly not "auth*".
func isCredentialParam(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"token", "passw", "secret", "access_id", "apikey", "api_key", "api-key", "session", "credential", "signature", "authorization", "oauth"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return strings.HasPrefix(n, "auth") && !strings.HasPrefix(n, "author")
}

// Redact removes persona values and credential-looking URL parameter values.
func (r *redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		for _, v := range r.values {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	s = queryParamRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := queryParamRe.FindStringSubmatch(m)
		if !isCredentialParam(parts[2]) {
			return m
		}
		return parts[1] + parts[2] + "=[REDACTED]"
	})
	return jwtRe.ReplaceAllString(s, "[REDACTED_JWT]")
}
