//go:build e2e

// L3a E2E: examples/service/redis::create (режим standalone, redis-консолидация
// концепции Ansible-роли, ADR-039 / .pm/tasks/2026-06-22-redis-consolidation).
//
// Сервис redis свёрнут в ОДИН режим-агностичный destiny `redis` (per-host install
// + render redis.conf + systemd) + плагин community.redis для живого Redis-рантайма.
// scenario create (standalone) — ПРОСТОЙ типизированный ввод → трансляция:
//  1. apply destiny `redis` — install redis-server + render redis.conf (из merged
//     redis_config) + render users.acl + systemd.
//  2. community.redis.command PING — health-gate после старта.
//
// Простой ввод оператора (memory_mb / persistence / maxmemory_policy / users)
// сценарий ТРАНСЛИРУЕТ через merge() в детальный redis_config (см.
// scenario/create/main.yml). Перевод pilot-входа в простой стиль — redis-
// консолидация 2026-06-22 (.pm/tasks/2026-06-22-redis-consolidation).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper + 1 soul-stub.
//  2. Seed Vault (requirepass + per-user пароль) + soulprint(os/net) + Coven.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript (scripted success по task-name, incl.
//     задача community.redis.command — soul-stub матчит по task_name, не по модулю).
//  5. CreateIncarnationWithApply → авто-create-прогон → WaitApplySuccess.
//  6. Asserts: apply_runs success / incarnation.state (type/version/merged-config/
//     users/hosts) / audit incarnation.created / metric keeper_scenario_runs_total.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedis_Create(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	const incName = "redis"

	// Vault-seed: requirepass читается keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password'); per-user пароль — через
	// vault('secret/redis/'+incarnation.name+'/users/<name>#password'). rel БЕЗ
	// mount/`data/`-префикса — SeedVaultKV добавляет их (KV v2). Без секрета
	// render-фаза падает «vault-ref: KV path not found».
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/app", map[string]any{
		"password": "e2e-app-user-secret",
	})

	// soulprint-seed: redis.conf.tmpl биндит на soulprint.self.network.primary_ip;
	// pkg_mgr/init_system нужны core.pkg/core.service (ADR-018, soulprint.md §3).
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

	// Материализуем режим-агностичный destiny `redis` в file://-репо + ставим
	// keeper_settings[default_destiny_source]. ДО RegisterService: invalidate от
	// POST /v1/services подтянет настройку в Holder без ожидания TTL-poll-а.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream: захват SID-lease → ApplyRequest в локальный Outbound.
	// LoadApplyScript — scripted success по task-name (+ default-success для
	// when:-collector-задач). Задача community.redis.config матчится по task_name.
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisCreateTasks())

	// Простой типизированный ввод оператора. version — distro-native пин (для
	// непустого state.redis_version); memory_mb+persistence+policy транслируются в
	// merged redis_config; users — typed-map с полной ACL-строкой.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"version":          "5:7.0.15-1~deb12u7",
		"memory_mb":        1024,
		"persistence":      "rdb",
		"maxmemory_policy": "volatile-lru",
		"users": map[string]any{
			"app": map[string]any{
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: state_changes пишутся
	// отдельной транзакцией ПОСЛЕ барьера (run.go §8). Ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	// redis_config — ИТОГ трансляции merge() (тот же, что ушёл в render redis.conf):
	// maxmemory=1024*75/100=768mb (essence.memory_reserve_percent=75), policy/save/
	// appendonly из input+persistence-пресета, maxclients/timeout из essence-подложки.
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_type":    "standalone",
		"redis_version": "5:7.0.15-1~deb12u7",
		"redis_config": map[string]any{
			"maxmemory":        "768mb",
			"maxmemory-policy": "volatile-lru",
			"appendonly":       "no",
			"save":             "900 1 300 10 60 10000",
			"maxclients":       float64(10000),
			"timeout":          float64(300),
		},
		"redis_users": map[string]any{
			"app": map[string]any{
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
		"redis_hosts": []any{},
	})
	// POST /v1/incarnations авто-запускает create-scenario и пишет
	// incarnation.created с apply_id авто-прогона в payload.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_AddAclUser — СКИП: сценарий add_acl_user переезжает на
// community.redis.acl (state acl ещё не реализован — следующий батч). См. result.md.
func TestE2EServiceRedis_AddAclUser(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: add_acl_user переезжает на community.redis.acl (state acl — следующий батч) — .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_UpdateConfig — СКИП: сценарий update_config переезжает на
// community.redis.config + re-apply destiny redis (следующий батч). См. result.md.
func TestE2EServiceRedis_UpdateConfig(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: update_config переезжает на community.redis.config + re-apply destiny redis (следующий батч) — .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_UpdateNodeExporter — СКИП: exporter — отдельная сущность
// (мониторинг), из service/redis вынесен (концепция роли: только ACL-юзер
// monitoring, без экспортеров). См. brief.md → «Мониторинг».
func TestE2EServiceRedis_UpdateNodeExporter(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: exporter вынесен из service/redis (мониторинг — отдельная сущность) — .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_RestartNodeExporter — СКИП: см. UpdateNodeExporter (exporter
// вынесен из service/redis).
func TestE2EServiceRedis_RestartNodeExporter(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: exporter вынесен из service/redis (мониторинг — отдельная сущность) — .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_AddReplicas — СКИП: реплики/топология переезжают на режим
// sentinel + community.redis.replica (следующий батч). Инвариант probe→where +
// cross-host register перенесётся туда. См. brief.md → «Перенос guard-инвариантов».
func TestE2EServiceRedis_AddReplicas(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: add_replicas переезжает на режим sentinel + community.redis.replica (probe→where инвариант — следующий батч) — .pm/tasks/2026-06-22-redis-consolidation")
}

// redisCreateTasks — scripted success-ответы по task-name задач create режима
// standalone: задачи destiny `redis` (install + render redis.conf/users.acl +
// systemd) + задача community.redis.command (PING health-gate). soul-stub матчит
// по task_name (default-success покрывает when:-collector-задачи и всё, что не в
// скрипте — socket-dir гасится static-when, т.к. unixsocket в конфиге нет).
func redisCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		// destiny redis (standalone).
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Ensure the redis socket directory exists"},
		{TaskName: "Render users.acl"},
		{TaskName: "Render redis.conf"},
		{TaskName: "Ensure the redis-server systemd drop-in directory exists"},
		{TaskName: "Render redis-server systemd hardening drop-in"},
		{TaskName: "Reload systemd because the hardening drop-in changed"},
		{TaskName: "Ensure redis-server is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-server": "running"}}}},
		{TaskName: "Restart redis-server because config or hardening changed"},
		// community.redis.command PING (живой Redis health-gate). soul-stub success
		// по task_name; changed-семантика плагина покрыта L0 (impl_test.go).
		{TaskName: "Wait for redis to answer PING"},
	}
}
