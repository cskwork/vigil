package proof

import (
	"fmt"
	"net/url"
	"strings"
	"vigil/internal/dsl"
)

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
				raw := o.API.Origin + o.API.Path
				if len(o.API.Query) > 0 {
					q := url.Values{}
					for k, v := range o.API.Query {
						q.Set(k, v)
					}
					raw += "?" + q.Encode()
				}
				if !Allowed(raw, o.API.Method, t.Scope) {
					return fmt.Errorf("observer network scope unapproved")
				}
			}
		}
	}
	return nil
}
