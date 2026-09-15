package orchestrator

import (
	"context"
	"fmt"
	"vigil/internal/dsl"
	"vigil/internal/model"
)

// ImportDraft registers a new operator-supplied script for review. It never
// schedules a run and refuses to overwrite an existing script/version.
func (o *Orchestrator) ImportDraft(ctx context.Context, raw []byte) (string, error) {
	sc, e := dsl.Parse(raw)
	if e != nil {
		return "", e
	}
	flows, ids, e := o.loadFlows(ctx)
	if e != nil {
		return "", e
	}
	if e = sc.Validate(ids); e != nil {
		return "", e
	}
	if sc.Scenario.Mutation != "" && sc.Scenario.Mutation != "read-only" {
		return "", fmt.Errorf("관리 화면에서는 읽기 전용 검사만 등록할 수 있습니다")
	}
	sc.Scenario.Version = 1
	fp := sc.Fingerprint(flows)
	m := o.scenarioModel(sc, sc.Scenario.ID, model.StatePendingApproval, fp, "human")
	v := &model.ScenarioVersion{ScenarioID: m.ID, Version: 1, YAML: string(raw), Fingerprint: fp, CreatedBy: "human", Reason: "admin import for review"}
	if e = o.st.CreateScenario(ctx, m, v, linksFor(sc)); e != nil {
		return "", e
	}
	return m.ID, nil
}
