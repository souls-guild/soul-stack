//go:build integration

package applyrun

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// NIM-985 (migration 123): per-host params round-trip through jsonb, upsert in
// place, refuse a row with no plan row to hang from, and leave with their plan row
// — purge_apply_run_plan deletes plan rows only, so the cascade IS the retention.
func TestIntegration_RunPlanHostParams_RoundTripAndCascade(t *testing.T) {
	ctx := context.Background()
	applyID := audit.NewULID()
	if err := InsertRunPlan(ctx, integrationPool, applyID, []RunPlanTask{
		{PlanIndex: 0, Name: "echo", Module: "core.exec.run", Params: []byte(`{"cmd":"<per-host>"}`)},
	}); err != nil {
		t.Fatalf("InsertRunPlan: %v", err)
	}
	if err := InsertRunPlanHostParams(ctx, integrationPool, applyID, []RunPlanHostParams{
		{PlanIndex: 0, SID: "b.example.com", Params: []byte(`{"cmd":"echo b"}`)},
		{PlanIndex: 0, SID: "a.example.com", Params: []byte(`{"cmd":"stale"}`)},
	}); err != nil {
		t.Fatalf("InsertRunPlanHostParams: %v", err)
	}
	if err := InsertRunPlanHostParams(ctx, integrationPool, applyID, []RunPlanHostParams{
		{PlanIndex: 0, SID: "a.example.com", Params: []byte(`{"cmd":"echo a"}`)},
	}); err != nil {
		t.Fatalf("re-insert: %v", err)
	}

	got, err := SelectRunPlanHostParams(ctx, integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectRunPlanHostParams: %v", err)
	}
	type row struct {
		sid string
		cmd any
	}
	var rows []row
	for _, r := range got {
		var m map[string]any
		if err := json.Unmarshal(r.Params, &m); err != nil {
			t.Fatalf("params %s: %v", r.Params, err)
		}
		rows = append(rows, row{r.SID, m["cmd"]})
	}
	if want := []row{{"a.example.com", "echo a"}, {"b.example.com", "echo b"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %v, want %v (by SID, upserted in place)", rows, want)
	}

	if err := InsertRunPlanHostParams(ctx, integrationPool, applyID, []RunPlanHostParams{
		{PlanIndex: 7, SID: "a.example.com", Params: []byte(`{}`)},
	}); err == nil {
		t.Error("a row for a plan_index with no plan row was accepted")
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM apply_run_plan WHERE apply_id = $1`, applyID); err != nil {
		t.Fatalf("delete plan: %v", err)
	}
	if got, err := SelectRunPlanHostParams(ctx, integrationPool, applyID); err != nil || len(got) != 0 {
		t.Fatalf("after the plan row went: rows = %v, err = %v; want none", got, err)
	}
}
