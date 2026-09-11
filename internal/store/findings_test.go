package store

import (
	"context"
	"errors"
	"testing"

	"vigil/internal/model"
)

func TestFindingsInsertListResolve(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f1 := &model.Finding{ProjectID: "p", FeatureID: "PROJ-123", ScenarioID: "", JobID: 7, Kind: "data_mismatch", Where: "/students", Expected: "42", Actual: "41", Evidence: "GET /api/students"}
	f2 := &model.Finding{ProjectID: "p", FeatureID: "PROJ-123", ScenarioID: "entry-tabs", JobID: 7, Kind: "display", Where: "/entry"}
	f3 := &model.Finding{ProjectID: "p", FeatureID: "other", ScenarioID: "other-sc", Kind: "domain_rule"}
	for _, f := range []*model.Finding{f1, f2, f3} {
		if _, err := s.InsertFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if f1.ID == 0 || f1.State != "OPEN" || f1.CreatedAt.IsZero() {
		t.Fatalf("insert did not fill the record: %+v", f1)
	}
	open, err := s.ListFindings(ctx, "p", true, 10)
	if err != nil || len(open) != 3 || open[0].ID != f3.ID {
		t.Fatalf("open = %v err=%v", open, err)
	}
	for_, err := s.ListOpenFindingsFor(ctx, "p", "entry-tabs", "PROJ-123")
	if err != nil || len(for_) != 2 {
		t.Fatalf("for scenario/feature = %d err=%v", len(for_), err)
	}
	if got, _ := s.ListOpenFindingsFor(ctx, "p", "entry-tabs", ""); len(got) != 1 || got[0].ID != f2.ID {
		t.Fatalf("scenario-only = %+v", got)
	}
	if err := s.ResolveFinding(ctx, "p", f1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveFinding(ctx, "p", f1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second resolve = %v", err)
	}
	if err := s.ResolveFinding(ctx, "other", f2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong project = %v", err)
	}
	if open, _ := s.ListFindings(ctx, "p", true, 10); len(open) != 2 {
		t.Fatalf("after resolve open = %d", len(open))
	}
	all, _ := s.ListFindings(ctx, "p", false, 10)
	if len(all) != 3 || all[2].State != "RESOLVED" || all[2].Where != "/students" {
		t.Fatalf("all = %+v", all)
	}
	g, err := s.GetFinding(ctx, "p", f2.ID)
	if err != nil || g.Kind != "display" {
		t.Fatalf("get = %+v %v", g, err)
	}
	if _, err := s.GetFinding(ctx, "p", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
