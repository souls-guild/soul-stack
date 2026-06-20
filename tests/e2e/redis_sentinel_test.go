//go:build e2e

// L3a contract-E2E: examples/service/redis-sentinel::create (ADR-039).
//
// redis-sentinel проще redis-pilot: один сценарий create, БЕЗ apply:destiny
// (три inline-задачи) и БЕЗ vault-рефов. По сравнению с redis_test.go этот
// контракт НЕ требует ни MaterializeDestinies, ни SeedVaultKV — материализуется
// только сам сервис (RegisterService). Соль контракта — что keeper-side рендер
// scenario.state_changes.sets из input коммитит master_ip/master_name/quorum в
// incarnation.state, а apply_runs всех хостов доходят до success.
//
// scenario create (examples/service/redis-sentinel/scenario/create/main.yml):
//
//	tasks (все on: incarnation.name, только core-модули):
//	  1. Install redis (provides redis-sentinel)   — core.pkg.installed
//	  2. Render sentinel.conf monitoring the master — core.file.rendered
//	  3. Start redis-sentinel on each host          — core.cmd.shell (nohup)
//	  4. Wait until sentinel responds on :26379      — core.cmd.shell (retry-probe)
//	state_changes.sets: master_ip / master_name / quorum (host-инвариантны,
//	приходят через input, ADR-010 params-интерполяция).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub.
//  2. SeedSoulprint(os.{family,arch,pkg_mgr,init_system}) — нужно core.pkg/cmd
//     keeper-side (ADR-018); render sentinel.conf факты хоста НЕ читает.
//  3. AddSoulToCoven(redis-sentinel) — roster резолвится по incarnation.name ∈ coven.
//  4. RegisterService(redis-sentinel) — реестр + service-snapshot из file://-репо.
//  5. ConnectSoulStub + LoadApplyScript (scripted success по task-name).
//  6. CreateIncarnationWithApply(master_ip/master_name/quorum) → авто-create →
//     WaitApplySuccess → WaitIncarnationReady.
//  7. Asserts: apply_runs success / incarnation.state{master_ip,master_name,
//     quorum} / audit incarnation.created{apply_id} / metric
//     keeper_scenario_runs_total{result="ok"}.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedisSentinel_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis-sentinel",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-sentinel"

	// soulprint-seed: core.pkg.installed / core.cmd.shell резолвят pkg_mgr/
	// init_system из soulprint.self.os keeper-side (ADR-018, soulprint.md §3).
	// arch не используется этим сервисом (нет release-tarball-ов), но кладём для
	// единообразия фикстуры. Render sentinel.conf факты хоста НЕ читает —
	// master_ip/quorum host-инвариантны (через input).
	stack.SeedSoulprint(t, 0, map[string]any{
		"os": map[string]any{
			"family":      "debian",
			"distro":      "debian",
			"version":     "12",
			"arch":        "amd64",
			"pkg_mgr":     "apt",
			"init_system": "systemd",
		},
		"hostname": "soul-a",
	})

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → error_locked.
	stack.AddSoulToCoven(t, 0, incName)

	// redis-sentinel применяет три inline-задачи (без apply:destiny) — отдельные
	// standalone-destiny материализовать НЕ нужно. RegisterService снапшотит сам
	// сервис из file://-репо и PUBLISH-ит service:invalidate (тёплый снимок в
	// Holder без 10s TTL-poll-а).
	stack.RegisterService(t, "redis-sentinel", "examples/service/redis-sentinel")

	// Live EventStream-стрим: захват Redis SID-lease → ApplyRequest
	// смаршрутизируется в локальный Outbound. LoadApplyScript — scripted success
	// по task-name четырёх задач create (+ default-success для unscripted-веток).
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", sentinelCreateTasks())

	// Все три input передаём явно, чтобы incarnation.state совпал с expectation
	// детерминированно:
	//   master_name="mymaster" — иначе CEL-default 'mymaster' (то же значение, но
	//     явно — контракт фиксирует входной путь, не default-путь);
	//   quorum=3 — отличается от CEL-default 2, доказывает, что в state коммитится
	//     именно input, а не дефолт.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis-sentinel@main", map[string]any{
		"master_ip":   "10.0.0.10",
		"master_name": "mymaster",
		"quorum":      3,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8). Ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	stack.AssertIncarnationState(t, inc, map[string]any{
		"master_ip":   "10.0.0.10",
		"master_name": "mymaster",
		"quorum":      3,
	})
	// POST /v1/incarnations авто-запускает create-scenario и пишет
	// incarnation.created с apply_id авто-прогона в payload (тот же applyID, что
	// в WaitApplySuccess).
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// sentinelCreateTasks — scripted success-ответы по task-name всех четырёх задач
// create. Зеркало
// tests/e2e/redis-sentinel/fixtures/stub-responses.yaml::scenarios.create.apply_responses
// (загружается inline — YAML-loader fixtures не реализован, pilot-паттерн).
//
// StateChanges — per-task артефакт RunResult (документирует эффект на хосте);
// incarnation.state коммитится отдельно из scenario.state_changes.sets, поэтому
// на AssertIncarnationState эти значения не влияют.
func sentinelCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis (provides redis-sentinel)", StateChanges: map[string]any{"packages": []any{map[string]any{"redis": "installed"}}}},
		{TaskName: "Render sentinel.conf monitoring the master"},
		{TaskName: "Start redis-sentinel on each host", StateChanges: map[string]any{"services": []any{map[string]any{"redis-sentinel": "running"}}}},
		{TaskName: "Wait until sentinel responds on :26379"},
	}
}
