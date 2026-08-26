package render

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestStateCapture_ReadsKeeperRegister — ★ GUARD for the live provisioned_vm_ids
// bug (ADR-056 amendment 2026-07-02), re-anchored by [ADR-0084]. The capture that
// records what a cloud step provisioned reads `${ register.<keeper-task>.* }`, and
// that register lives in the keeper bucket, not in any host's. Losing the read
// used to surface as `no such key` at the END of a run — error_locked AFTER the VMs
// were already created and paid for, with their ids nowhere on disk.
//
// The capture is now an ordinary `on: keeper` task, so the read rides keeperVars
// (dispatch.go:174-177) instead of a context of its own. Mutation: drop the
// KeeperRegister branch in keeperVars and this fails at eval.
func TestStateCapture_ReadsKeeperRegister(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "record", On: "keeper", Module: &config.ModuleTask{
				Module: "core.state.set",
				Params: map[string]any{
					"field": "provisioned_vm_ids",
					"value": "${ register.provision.vm_ids }",
				},
			}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:       manifest,
		Incarnation:    IncarnationMeta{Name: "svc"},
		Hosts:          []*topology.HostFacts{host("a", []string{"svc"}, nil)},
		KeeperRegister: map[string]any{"provision": map[string]any{"vm_ids": []any{"vm-1", "vm-2"}}},
		Ctx:            context.Background(),
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	ids := tasks[0].Params.GetFields()["value"].GetListValue().GetValues()
	if len(ids) != 2 || ids[0].GetStringValue() != "vm-1" {
		t.Fatalf("params.value = %v, want [vm-1 vm-2] from the keeper register", ids)
	}
}
