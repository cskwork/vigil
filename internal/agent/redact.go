package agent

import (
	"regexp"
	"sort"
	"strings"

	"vigil/internal/config"
)

// Redactor removes secrets from agent output before it is written as evidence (PRD §11).
type Redactor struct {
	secrets []string // longest first
}

const redacted = "[REDACTED]"

// minSecretLen avoids redacting trivially short strings (which would shred the transcript).
const minSecretLen = 6

var secretEnvRe = regexp.MustCompile(`(?i)(_API_KEY|_TOKEN|_SECRET|PASSWORD|_KEY|_PASS|_PWD)$`)

// kvRe catches inline "api_key: xxx", "password=xxx", "Authorization: Bearer xxx" shapes.
var kvRe = regexp.MustCompile(`(?i)\b(api[_-]?key|token|password|passwd|secret|authorization)(["']?\s*[:=]\s*(?:bearer\s+)?["']?)([^"'\s,;}\]]{4,})`)

// NewRedactor builds a redactor from explicit secrets.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) < minSecretLen || seen[s] {
			continue
		}
		seen[s] = true
		r.secrets = append(r.secrets, s)
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// RedactorFor collects persona passwords, persona extra values and every
// environment value whose name looks secret (…_API_KEY, …_TOKEN, …).
func RedactorFor(personas map[string]config.Persona, environ []string) *Redactor {
	var secrets []string
	for _, p := range personas {
		secrets = append(secrets, p.Password)
		for k, v := range p.Extra {
			if secretEnvRe.MatchString(strings.ToUpper(k)) {
				secrets = append(secrets, v)
			}
		}
	}
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if secretEnvRe.MatchString(kv[:i]) {
			secrets = append(secrets, kv[i+1:])
		}
	}
	return NewRedactor(secrets...)
}

// Redact replaces known secrets and inline credential-looking pairs.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, redacted)
	}
	s = kvRe.ReplaceAllString(s, "${1}${2}"+redacted)
	return s
}

// RedactResult returns a copy of res with all free-text fields redacted.
func (r *Redactor) RedactResult(res *Result) *Result {
	if res == nil {
		return nil
	}
	out := *res
	out.Evidence = r.Redact(res.Evidence)
	out.CoverageDelta = r.Redact(res.CoverageDelta)
	out.OracleProvenance = r.Redact(res.OracleProvenance)
	out.ScriptPatch = r.Redact(res.ScriptPatch)
	out.Reason = r.Redact(res.Reason)
	out.ScriptCandidates = nil
	for _, c := range res.ScriptCandidates {
		out.ScriptCandidates = append(out.ScriptCandidates, r.Redact(c))
	}
	if res.Observed != nil {
		out.Observed = map[string]string{}
		for k, v := range res.Observed {
			out.Observed[k] = r.Redact(v)
		}
	}
	out.VisitedURLs = nil
	for _, u := range res.VisitedURLs {
		out.VisitedURLs = append(out.VisitedURLs, r.Redact(u))
	}
	return &out
}
