//go:build integration

// Integration-guard сквозного пути «keeper-side задача (on: keeper) → task.executed
// → свёртка changed_tasks» (ADR-052 §k, фикс выпадения keeper-задач из
// run_completed). До фикса dispatchKeeperTasks не эмитил task.executed — changed-
// keeper-задача молча отсутствовала в SelectChangedTaskKeys, task:-подписка на неё
// была мёртвой.
//
// Прогоняется dispatchKeeperTasks с реальным PG (apply_runs Insert/UpdateStatus) +
// реальным auditpg.Writer; результат читается обратно реальным auditpg.Reader.
// SelectChangedTaskKeys и проверяется через buildChangedTasks (changed_hosts/
// total_hosts), как это делает emitRunCompleted.

package scenario

import (
	"context"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// keeperDispatchRunner собирает Runner напрямую (минуя NewRunner-валидацию
// Loader/Topology/… — для прицельного теста dispatchKeeperTasks они не нужны) с
// реальным PG + auditpg.Writer/Reader и keeper-Registry-заглушкой.
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

// TestIntegration_KeeperChangedTask_FoldsIntoChangedTasks — КЛЮЧЕВОЙ кейс бага:
// keeper-задача БЕЗ register, но с id: (типичный provision_vm) с changed=true
// эмитит task.executed (sid=keeper, CHANGED), который SelectChangedTaskKeys видит,
// и buildChangedTasks сворачивает в одну ChangedTask с changed_hosts=1/total_hosts=1.
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

	if err := r.dispatchKeeperTasks(context.Background(), spec, slog.New(slog.DiscardHandler), tasks, plans); err != nil {
		t.Fatalf("dispatchKeeperTasks: %v", err)
	}

	keys, err := r.deps.AuditReader.SelectChangedTaskKeys(context.Background(), applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}
	if _, ok := keys[auditpg.ChangedTaskKey{SID: render.KeeperTargetSID, PlanIndex: 0}]; !ok {
		t.Fatalf("ключ (keeper, 0) отсутствует в SelectChangedTaskKeys — keeper changed-задача выпала: %v", keys)
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

// TestIntegration_KeeperFailedTask_NotInChangedTasks — failed keeper-задача пишет
// task.executed status=TASK_STATUS_FAILED (НЕ CHANGED) → НЕ попадает в свёртку
// changed (ChangedHosts=0, задача отсутствует в результате). dispatchKeeperTasks
// возвращает ошибку (первая упавшая keeper-задача → abort), но событие уже
// записано до return.
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

	if err := r.dispatchKeeperTasks(context.Background(), spec, slog.New(slog.DiscardHandler), tasks, plans); err == nil {
		t.Fatal("dispatchKeeperTasks: nil error, want failed keeper task abort")
	}

	keys, err := r.deps.AuditReader.SelectChangedTaskKeys(context.Background(), applyID)
	if err != nil {
		t.Fatalf("SelectChangedTaskKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("SelectChangedTaskKeys = %v, want пусто (failed-задача не CHANGED)", keys)
	}

	got := buildChangedTasks(tasks, plans, keys)
	if len(got) != 0 {
		t.Errorf("buildChangedTasks = %+v, want пусто (failed keeper-задача не в changed_tasks)", got)
	}
}
