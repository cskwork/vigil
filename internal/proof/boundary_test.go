package proof

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEvidenceAvailabilityAndGatewayBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		jsonFile bool
		want     string
		code     int
	}{
		{"available", 0, true, "available", 200},
		{"expired", 2 * time.Hour, true, "expired", 410},
		{"missing JSON with orphan screenshot", 0, false, "missing", 410},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, repo := setup(t)
			svc, err := NewService(st, testRegistry(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			svc.RawRetention = time.Hour
			ctx := context.Background()
			c, err := repo.Create(ctx, "qa", "zero", "actor")
			if err != nil {
				t.Fatal(err)
			}
			c, err = repo.Patch(ctx, c.ID, c.RowVersion, contract())
			if err != nil {
				t.Fatal(err)
			}
			a, _, err := repo.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "evidence"}, "actor", testRegistry())
			if err != nil {
				t.Fatal(err)
			}
			a.State = "DONE"
			a.Verdict = "PASS"
			a.Results = []CriterionResult{{ID: "v", Required: true, Status: "PASS", Evidence: []Evidence{{ID: "receipt", At: time.Now().Add(-tc.age), Artifact: "receipt.json", Screenshot: "receipt.png", Status: "PASS"}}}}
			if err = repo.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(svc.EvidenceDir, a.ID)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "receipt.png"), []byte("image"), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.jsonFile {
				if err = os.WriteFile(filepath.Join(dir, "receipt.json"), []byte(`{"value":0}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			public := svc.publicAttempt(a)
			if got := public.Results[0].Evidence[0].Availability; got != tc.want {
				t.Fatalf("availability %q, want %q", got, tc.want)
			}
			if a.Results[0].Evidence[0].Availability != "" || public.Verdict != "PASS" {
				t.Fatal("availability decoration mutated original observation")
			}
			token := strings.Repeat("t", 32)
			handler, err := svc.Handler(HTTPConfig{GatewayToken: token, GatewayActor: "qa"})
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "?format=image"} {
				for _, auth := range []string{"", "Bearer wrong", "Bearer " + token} {
					req := httptest.NewRequest("GET", "http://proof.test/api/proof/attempts/"+a.ID+"/evidence/receipt"+suffix, nil)
					req.Header.Set("Authorization", auth)
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					want := 403
					if auth == "Bearer "+token {
						want = tc.code
					}
					if w.Code != want {
						t.Errorf("format %q authenticated %v: status %d, want %d", suffix, auth == "Bearer "+token, w.Code, want)
					}
				}
			}
		})
	}
}

func TestPruneRemovesOnlyExpiredManifestArtifacts(t *testing.T) {
	st, repo := setup(t)
	svc, err := NewService(st, testRegistry(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.RawRetention = time.Hour
	ctx := context.Background()
	c, err := repo.Create(ctx, "qa", "zero", "actor")
	if err != nil {
		t.Fatal(err)
	}
	c, err = repo.Patch(ctx, c.ID, c.RowVersion, contract())
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := repo.Approve(ctx, c.ID, Approval{RegistryHash: Hash(testRegistry().Targets["qa"]), Revision: c.CurrentRevision, ContractHash: c.ContractHash, Scope: "qa", IdempotencyKey: "prune"}, "actor", testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.State = "DONE"
	a.FinishedAt = &now
	a.Results = []CriterionResult{{Evidence: []Evidence{{ID: "old", At: now.Add(-2 * time.Hour), Artifact: "old.json", Screenshot: "old.png"}, {ID: "orphan", At: now.Add(-2 * time.Hour), Artifact: "missing.json", Screenshot: "orphan.png"}, {ID: "fresh", At: now, Artifact: "fresh.json"}}}}
	if err = repo.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(svc.EvidenceDir, a.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.json", "old.png", "orphan.png", "fresh.json", "unlisted.json"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte("evidence"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.json", "old.png", "orphan.png"} {
		if _, err = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("expired %s remains: %v", name, err)
		}
	}
	for _, name := range []string{"fresh.json", "unlisted.json"} {
		if _, err = os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("retained %s removed: %v", name, err)
		}
	}
	if _, err = repo.Attempt(ctx, a.ID); err != nil {
		t.Fatal("recent summary removed", err)
	}
}
