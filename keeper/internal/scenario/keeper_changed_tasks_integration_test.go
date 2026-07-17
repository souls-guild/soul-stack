//go:build integration

// Integration guard for the end-to-end path "keeper-side task (on: keeper) →
// task.executed → changed_tasks fold" (ADR-052 §k, fix for keeper tasks
// dropping out of run_completed). Before the fix, dispatchKeeperTasks never
// emitted task.executed — a changed keeper task was silently absent from
// SelectChangedTaskKeys, and a task:-subscription on it was dead.
//
// Runs dispatchKeeperTasks with a real PG (apply_runs Insert/UpdateStatus) +
// a real auditpg.Writer; the result is read back with a real auditpg.Reader.
// SelectChangedTaskKeys, then verified via buildChangedTasks (changed_hosts/
// total_hosts), the same way emitRunCompleted does.

package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// keeperDispatchRunner builds a Runner directly (bypassing NewRunner's
// Loader/Topology/… validation — not needed for a targeted dispatchKeeperTasks
// test) with a real PG + auditpg.Writer/Reader and a keeper-Registry stub.
func keeperDispatchRunner(reg fakeKeeperRegistry) *Runner {
	return &Runner{
		deps: Deps{
			DB:          integrationPool,
			Audit:       auditpg.NewWriter(integrationPool),
			AuditReader: auditpg.NewReader(integrationPool),
		},
		keeperModules: reg,
	}
}

// TestIntegration_KeeperChangedTask_FoldsIntoChangedTasks — the KEY bug case: a
// keeper task WITHOUT register but WITH id: (a typical provision_vm) with
// changed=true emits task.executed (sid=keeper, CHANGED), which
// SelectChangedTaskKeys sees, and buildChangedTasks folds into one ChangedTask
// with changed_hosts=1/total_hosts=1.
func TestIntegration_KeeperChangedTask_FoldsIntoChangedTasks(t *testing.T) {
	resetAll(t)
	seedIncarnation(t, "redis-prod")

	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Changed: true}}
	r := keeperDispatchRunner(fakeKeeperRegistry{"core.cloud": mod})

	applyID := "apply-keeper-changed"
	spec := RunSpec{ApplyID: applyID, IncarnationName: "redis-prod", ScenarioName: "create"}
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "provision_vm", ID: "vm-web", Module: "core.cloud.created", Params: mustStruct(t, map[string]any{"provider": "fake"})},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
	}

	if err := r.dispatchKeeperTasks(context.Background(), spec, slog.New(slog.DiscardHandler), 0, tasks, plans); err != nil {
		t.Fatalf("dispatchKeeperTasks: %v", err)
	}

	keys, err := r.deps.AuditReader.SelectChangedTaskKeys(context.Background(), applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}
	if _, ok := keys[auditpg.ChangedTaskKey{SID: render.KeeperTargetSID, PlanIndex: 0}]; !ok {
		t.Fatalf("key (keeper, 0) missing from SelectChangedTaskKeys - keeper changed task dropped: %v", keys)
	}

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 1 {
		t.Fatalf("buildChangedTasks: got %d, want 1", len(got))
	}
	if got[0].ChangedHosts != 1 || got[0].TotalHosts != 1 {
		t.Errorf("changed_hosts/total_hosts = %d/%d, want 1/1", got[0].ChangedHosts, got[0].TotalHosts)
	}
	if got[0].ID != "vm-web" || got[0].Register != "" {
		t.Errorf("address = id=%q register=%q, want id=vm-web register empty", got[0].ID, got[0].Register)
	}
}

// TestIntegration_KeeperFailedTask_NotInChangedTasks — a failed keeper task
// writes task.executed status=TASK_STATUS_FAILED (NOT CHANGED) → does NOT go
// into the changed fold (ChangedHosts=0, the task is absent from the result).
// dispatchKeeperTasks returns an error (the first failing keeper task →
// abort), but the event is already recorded before the return.
func TestIntegration_KeeperFailedTask_NotInChangedTasks(t *testing.T) {
	resetAll(t)
	seedIncarnation(t, "redis-prod")

	mod := &fakeKeeperModule{final: &pluginv1.ApplyEvent{Failed: true, Message: "driver boom"}}
	r := keeperDispatchRunner(fakeKeeperRegistry{"core.cloud": mod})

	applyID := "apply-keeper-failed"
	spec := RunSpec{ApplyID: applyID, IncarnationName: "redis-prod", ScenarioName: "create"}
	tasks := []*render.RenderedTask{
		{Index: 0, Name: "provision_vm", ID: "vm-web", Module: "core.cloud.created", Params: mustStruct(t, map[string]any{"provider": "fake"})},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, Keeper: true, TargetSIDs: []string{render.KeeperTargetSID}},
	}

	if err := r.dispatchKeeperTasks(context.Background(), spec, slog.New(slog.DiscardHandler), 0, tasks, plans); err == nil {
		t.Fatal("dispatchKeeperTasks: nil error, want failed keeper task abort")
	}

	keys, err := r.deps.AuditReader.SelectChangedTaskKeys(context.Background(), applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("SelectChangedTaskKeys = %v, want empty (failed task is not CHANGED)", keys)
	}

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 0 {
		t.Errorf("buildChangedTasks = %+v, want empty (failed keeper task not in changed_tasks)", got)
	}
}
