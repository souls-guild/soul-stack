//go:build integration

package scenario

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ★ GUARD (NIM-985) end to end, under both execution models: a task that renders
// its own cmd on each host, beside a sealed cell that is per host too, persists the
// plan row with cmd marked and the sealed cell masked, plus one row per host
// holding that host's cmd and nothing sealed.
//
// Mutation: drop the InsertRunPlanHostParams call from persistRunPlan → no rows.
func TestIntegration_PerHostParams_PersistedPerHostAndMasked(t *testing.T) {
	const hostA, hostB = "host-a.example.com", "host-b.example.com"
	const secret = "s3cr3t-985"
	forEachModel(t, func(t *testing.T, model ExecutionModel) {
		resetAll(t)
		seedOperator(t, "archon-alice")
		seedIncarnation(t, "noop-prod")
		seedConnectedSoul(t, hostA, []string{"noop-prod"})
		seedConnectedSoul(t, hostB, []string{"noop-prod"})
		gitURL := writeServiceRepo(t, `name: create
input:
  key:
    type: string
    secret: true
tasks:
  - name: Per host
    module: core.exec.run
    params:
      cmd: "echo ${ soulprint.self.sid }"
      args: ["--key=${ input.key }-${ soulprint.self.sid }"]
      cwd: /srv
    changed_when: "false"
`)
		disp := newPerHostCommandDispatcher(t)
		r := newRunner(t, disp, gitURL)
		r.deps.ExecutionModel = model

		applyID := audit.NewULID()
		if err := r.Start(context.Background(), RunSpec{
			ApplyID:         applyID,
			IncarnationName: "noop-prod",
			ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
			ScenarioName:    "create",
			Input:           map[string]any{"key": secret},
			StartedByAID:    "archon-alice",
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

		if got := disp.snapshot(); !strings.HasPrefix(got[hostA], "echo "+hostA) || !strings.HasPrefix(got[hostB], "echo "+hostB) {
			t.Fatalf("wire cmds = %v, want each host its own - the fixture is not per host", got)
		}

		plan, err := applyrun.SelectRunPlanByApplyID(context.Background(), integrationPool, applyID)
		if err != nil || len(plan) != 1 {
			t.Fatalf("plan = %+v, err = %v; want one task", plan, err)
		}
		var common map[string]any
		if err := json.Unmarshal(plan[0].Params, &common); err != nil {
			t.Fatalf("plan params %s: %v", plan[0].Params, err)
		}
		wantCommon := map[string]any{"cmd": "<per-host>", "args": []any{audit.MaskedValue}, "cwd": "/srv"}
		if !reflect.DeepEqual(common, wantCommon) {
			t.Errorf("plan row = %v, want %v", common, wantCommon)
		}

		rows, err := applyrun.SelectRunPlanHostParams(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectRunPlanHostParams: %v", err)
		}
		got := map[string]map[string]any{}
		for _, row := range rows {
			if strings.Contains(string(row.Params), secret) {
				t.Fatalf("row %s carries the sealed input in the clear: %s", row.SID, row.Params)
			}
			var m map[string]any
			if err := json.Unmarshal(row.Params, &m); err != nil {
				t.Fatalf("row %s: %v", row.SID, err)
			}
			got[row.SID] = m
		}
		want := map[string]map[string]any{hostA: {"cmd": "echo " + hostA}, hostB: {"cmd": "echo " + hostB}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("per-host rows = %v, want %v", got, want)
		}
	})
}
