package proof

import (
	"fmt"
	"net/url"
	"strings"
	"vigil/internal/dsl"
)

func scopeURL(rule ScopeRule) string {
	raw := rule.Origin + rule.Path
	if len(rule.Query) == 0 {
		return raw
	}
	q := url.Values{}
	for key, value := range rule.Query {
		q.Set(key, value)
	}
	return raw + "?" + q.Encode()
}

func (r Registry) Validate() error {
	if len(r.Targets) == 0 {
		return fmt.Errorf("at least one registered target required")
	}
	for id, t := range r.Targets {
		u, e := url.Parse(t.BaseURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return fmt.Errorf("target %s invalid base URL", id)
		}
		if t.Environment == "" || t.PolicyVersion == "" {
			return fmt.Errorf("target environment and policy_version required")
		}
		if !Allowed(t.BaseURL, "GET", t.Scope) {
			return fmt.Errorf("target base URL must be an approved GET")
		}
		for _, rule := range t.Scope {
			origin, e := url.Parse(rule.Origin)
			if e != nil || origin.Host == "" || origin.Path != "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") || !strings.HasPrefix(rule.Path, "/") || rule.Method == "" {
				return fmt.Errorf("invalid target scope")
			}
		}
		validateSteps := func(steps []dsl.Step) error {
			for _, st := range steps {
				switch st.Kind() {
				case "goto":
					raw := st.Goto
					if strings.HasPrefix(raw, "/") {
						raw = strings.TrimRight(t.BaseURL, "/") + raw
					}
					if !Allowed(raw, "GET", t.Scope) {
						return fmt.Errorf("unapproved goto %s", st.Goto)
					}
				case "click", "fill", "select", "wait_ms", "wait_for", "assert_visible":
				default:
					return fmt.Errorf("unsupported proof action %s", st.Kind())
				}
			}
			return nil
		}
		for _, p := range t.Personas {
			if p.Account == "" {
				return fmt.Errorf("persona account lock required")
			}
			if e := validateSteps(p.Setup); e != nil {
				return e
			}
		}
		for _, a := range t.Actions {
			if e := validateSteps(a.Steps); e != nil {
				return e
			}
		}
		for _, o := range t.Observers {
			if _, ok := t.Actions[o.Action]; !ok {
				return fmt.Errorf("observer action unavailable")
			}
			if o.API != nil {
				raw := scopeURL(*o.API)
				if !Allowed(raw, o.API.Method, t.Scope) {
					return fmt.Errorf("observer network scope unapproved")
				}
			}
			if o.DirectRead && (o.Kind != "network" || o.API == nil || o.API.Method != "GET" || !o.Reread) {
				return fmt.Errorf("direct observer reads require a registered GET reread")
			}
			if o.Kind == "mysql" && o.Compare != "" && o.Compare != "unchanged" && o.Compare != "preserves" {
				return fmt.Errorf("mysql observer comparison must be unchanged or preserves")
			}
		}
	}
	return nil
}
