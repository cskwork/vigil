package proof

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func prepareFixture(ctx context.Context, a *Attempt) error {
	mutating := false
	for _, id := range a.Plan.Actions {
		if a.Target.Actions[id].Mutating {
			mutating = true
		}
	}
	if !mutating {
		return nil
	}
	f := a.Fixture
	if !f.QA || f.PrepareURL == "" || !Allowed(f.PrepareURL, "POST", a.Target.Scope) {
		return fmt.Errorf("registered QA fixture preparation unavailable")
	}
	body := []byte(encode(map[string]string{"entity": f.Entity, "attempt": a.ID}))
	req, e := http.NewRequestWithContext(ctx, "POST", f.PrepareURL, bytes.NewReader(body))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("fixture redirects forbidden") }}
	res, e := client.Do(req)
	if e != nil {
		return fmt.Errorf("fixture preparation unavailable")
	}
	defer res.Body.Close()
	var out struct {
		Entity  string `json:"entity"`
		Attempt string `json:"attempt"`
		Ready   bool   `json:"ready"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out) != nil || !out.Ready || out.Entity != f.Entity || out.Attempt != a.ID {
		return fmt.Errorf("fixture preparation not proven")
	}
	a.FixtureGeneration = out.Attempt
	return nil
}
