package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

func TestImportDraftIsScopedPendingAndNeverOverwrites(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	ctx := context.Background()
	raw := []byte(`scenario:
  id: same-name
  version: 1
  title: 화면 안내 확인
  mutation: read-only
steps:
  - goto: "#"
  - assert_text: {value: "Example Domain"}
oracle:
  source: spec
  note: 사용자 확인 기준
`)
	for _, id := range []string{"one", "two"} {
		cfg := &config.Config{}
		cfg.Project.ID = id
		o := New(cfg, st, nil, nil, nil)
		if _, e = o.ImportDraft(ctx, raw); e != nil {
			t.Fatal(e)
		}
		sc, e := st.GetScenario(ctx, id, "same-name")
		if e != nil || sc.State != model.StatePendingApproval || sc.NextDueAt != nil {
			t.Fatal(sc, e)
		}
		if _, e = o.ImportDraft(ctx, raw); e == nil {
			t.Fatal("overwrote existing draft")
		}
		if _, e = o.ImportDraft(ctx, []byte(strings.ReplaceAll(string(raw), "read-only", "destructive"))); e == nil {
			t.Fatal("accepted destructive draft")
		}
	}
	for _, id := range []string{"one", "two"} {
		scs, e := st.ListScenarios(ctx, id)
		if e != nil || len(scs) != 1 {
			t.Fatal(id, scs, e)
		}
	}
}
