package proof

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func redactJSON(raw json.RawMessage, t Target) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var data any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&data) != nil {
		return json.RawMessage(`"[unavailable]"`)
	}
	secrets := []string{}
	for _, p := range t.Personas {
		for _, env := range p.Secrets {
			if v := os.Getenv(env); v != "" {
				secrets = append(secrets, v)
			}
		}
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			for _, secret := range secrets {
				x = strings.ReplaceAll(x, secret, "[REDACTED]")
			}
			return x
		case map[string]any:
			for key, value := range x {
				k := strings.ToLower(key)
				if strings.Contains(k, "password") || strings.Contains(k, "token") || strings.Contains(k, "authorization") || strings.Contains(k, "secret") {
					x[key] = "[REDACTED]"
				} else {
					x[key] = walk(value)
				}
			}
		case []any:
			for i, value := range x {
				x[i] = walk(value)
			}
		}
		return v
	}
	b, _ := json.Marshal(walk(data))
	return b
}
func (s *Service) publicAttempt(a *Attempt) *Attempt {
	b, _ := json.Marshal(a)
	var out Attempt
	json.Unmarshal(b, &out)
	for id, p := range out.Target.Personas {
		p.Secrets = nil
		p.Setup = nil
		out.Target.Personas[id] = p
	}
	for i := range out.Results {
		for j := range out.Results[i].Evidence {
			ev := &out.Results[i].Evidence[j]
			ev.Availability = "available"
			if ev.Artifact == "" {
				ev.Availability = "missing"
			} else if time.Since(ev.At) > s.RawRetention {
				ev.Availability = "expired"
			} else {
				path := filepath.Join(s.EvidenceDir, out.ID, filepath.Base(ev.Artifact))
				info, e := os.Lstat(path)
				if e != nil || !info.Mode().IsRegular() {
					ev.Availability = "missing"
				}
			}
		}
	}
	return &out
}
