//go:build e2e

// L3a contract-E2E: examples/service/redis::create в режиме sentinel_only
// (redis-консолидация концепции Ansible-роли, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// sentinel_only — тонкий sentinel-слой: разворачивается ТОЛЬКО sentinel-демон
// (data-плоскость redis-server НЕ ставится как сервис), мониторящий ВНЕШНИЙ
// master из input.master_ip. Этот контракт проверяет keeper-side цепочку
// render→dispatch→RunResult→state именно для sentinel_only-ветки диспетчера
// create (на standalone/cluster/sentinel ветки гасятся static-when placeholder-
// skip-ом). Соль контракта — что redis_sentinel.master_ip/master_name доходят до
// incarnation.state из input, а apply_runs всех хостов доходят до success.
//
// Прежний отдельный сервис redis-sentinel удалён: его сценарий create целиком
// поглощён режимом sentinel_only консолидированного redis (диспетчер по
// redis_type, scenario/create/sentinel-only.yml).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub.
//  2. Seed Vault (auth_pass для sentinel monitor) + soulprint(os/net) + Coven.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript (scripted success по task-name).
//  5. CreateIncarnationWithApply(redis_type=sentinel_only + master_ip) →
//     авто-create-прогон → WaitApplySuccess → WaitIncarnationReady.
//  6. Asserts: apply_runs success / incarnation.state{redis_type,redis_sentinel} /
//     audit incarnation.created{apply_id} / metric keeper_scenario_runs_total.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedis_CreateSentinelOnly(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis-sntnl-only"

	// Vault-seed: sentinel-демон аутентифицируется к ВНЕШНЕМУ master-у
	// auth_pass-ом, который create-ветка резолвит keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password'). rel БЕЗ mount/`data/`-
	// префикса — SeedVaultKV добавляет их (KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-sentinel-secret",
	})

	// soulprint-seed: sentinel.conf.tmpl биндит на soulprint.self.network.primary_ip;
	// pkg_mgr/init_system нужны core.pkg/core.service keeper-side (ADR-018).
	stack.SeedSoulprint(t, 0, map[string]any{
		"os": map[string]any{
			"family":      "debian",
			"distro":      "debian",
			"version":     "12",
			"arch":        "amd64",
			"pkg_mgr":     "apt",
			"init_system": "systemd",
		},
		"network": map[string]any{
			"primary_ip": "10.0.0.5",
		},
		"hostname": "soul-a",
	})

	// Coven-членство ДО Create: roster резолвится по `incarnation.name ∈ coven[]`
	// (ADR-008). Без него scenario видит no_hosts → error_locked.
	stack.AddSoulToCoven(t, 0, incName)

	// Материализуем режим-агностичный destiny `redis` (create-ветки зовут
	// apply: destiny: redis) + ставим default_destiny_source. ДО RegisterService:
	// invalidate от POST /v1/services подтянет настройку в Holder без TTL-poll-а.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream: захват SID-lease → ApplyRequest в локальный Outbound.
	// LoadApplyScript — scripted success по task-name (+ default-success для
	// when:-погашенных веток диспетчера и плагинных задач).
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisSentinelOnlyTasks())

	// Простой типизированный ввод: redis_type=sentinel_only + version (distro-native
	// пин) + master_ip (REQUIRED при sentinel_only — required_when в main.yml).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_type":  "sentinel_only",
		"version":     "5:7.0.15-1~deb12u7",
		"master_ip":   "10.9.9.9",
		"master_port": 6379,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8). Ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	// sentinel_only-факты: redis_type + redis_sentinel{master_name(default mymaster),
	// master_ip из input}. redis_config пуст (data-плоскости нет); users/hosts пусты.
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_type": "sentinel_only",
		"redis_sentinel": map[string]any{
			"master_name": "mymaster",
			"master_ip":   "10.9.9.9",
		},
	})
	// POST /v1/incarnations авто-запускает create-scenario и пишет
	// incarnation.created с apply_id авто-прогона в payload.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// redisSentinelOnlyTasks — scripted success-ответы по task-name ключевых задач
// sentinel_only-ветки create: mode-guard диспетчера, install redis (несёт
// sentinel-демон), render sentinel.conf, SENTINEL MONITOR + PONG-gate.
// soul-stub матчит по task_name; default-success (LoadApplyScript) покрывает
// when:-погашенные ветки standalone/cluster/sentinel и прочие задачи destiny.
func redisSentinelOnlyTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Render sentinel.conf"},
		{TaskName: "Ensure redis-sentinel is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-sentinel": "running"}}}},
		{TaskName: "Monitor the external master via SENTINEL MONITOR"},
		{TaskName: "Wait for sentinel to answer PING"},
	}
}
