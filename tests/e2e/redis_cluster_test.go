//go:build e2e

// L3a контракт-e2e: examples/service/redis::create в режиме cluster
// (redis-консолидация концепции Ansible-роли, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// cluster — honest hash-slot Redis Cluster: диспетчер create инклудит ветку
// create/cluster.yml (standalone/sentinel/sentinel_only гасятся static-when
// placeholder-skip-ом). Multi-host roster (3 хоста): shards=3, replicas_per_shard=0
// → топология ровно 3*(1+0)=3 (size-guard проходит). Контракт проверяет keeper-side
// цепочку render→dispatch→RunResult→state для cluster-ветки на 3-узловой фикстуре,
// НЕ реальную gossip-топологию (это L3b-live, tests/e2e-live).
//
// Прежние отдельные сервисы redis-cluster / redis-cluster-live удалены: их
// cluster-флоу поглощён режимом cluster консолидированного redis (диспетчер по
// redis_type + day-2 scenarios add_node/remove_node/reshard). Probe→where
// (оригинальный redis-cluster update_acl) перенесён на rolling-restart сценарий
// стадии A (scenario/restart) — здесь не дублируется.
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper + 3 soul-stub.
//  2. Seed Vault (requirepass) + soulprint(os/net) + Coven на каждом из трёх.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript на каждом хосте (default-success покрывает
//     все задачи cluster-ветки; cluster-build run_once приходит на бутстрап-ноду).
//  5. CreateIncarnationWithApply(redis_type=cluster, shards=3) → авто-create →
//     WaitApplySuccess → WaitIncarnationReady.
//  6. Asserts: apply_runs success / incarnation.state{redis_type=cluster,
//     cluster-директивы в redis_config} / audit / metric.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestE2EServiceRedis_CreateCluster(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		Souls:       3,
	})
	defer stack.Cleanup()

	const incName = "redis-clstr"

	// requirepass читается keeper-side через
	// vault('secret/redis/'+incarnation.name+'#password') (cluster.yml apply.input
	// + cluster-build password). Без секрета render-фаза падает «vault-ref: KV path
	// not found». rel БЕЗ mount/`data/`-префикса — SeedVaultKV добавляет их (KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-cluster-secret",
	})

	// soulprint + Coven-членство (roster по incarnation.name, ADR-008) для всех трёх.
	// network.primary_ip нужен render redis.conf.tmpl (cluster-announce-ip per-host)
	// и cluster nodes-MAP (soulprint.hosts.map по SID). pkg_mgr/init_system —
	// core.pkg/core.service keeper-side (ADR-018).
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	for i := 0; i < 3; i++ {
		stack.SeedSoulprint(t, i, map[string]any{
			"os": map[string]any{
				"family":      "debian",
				"distro":      "debian",
				"version":     "12",
				"arch":        "amd64",
				"pkg_mgr":     "apt",
				"init_system": "systemd",
			},
			"network": map[string]any{"primary_ip": ips[i]},
		})
		stack.AddSoulToCoven(t, i, incName)
	}

	// Материализуем режим-агностичный destiny `redis` (cluster-ветка зовёт
	// apply: destiny: redis) + ставим default_destiny_source. ДО RegisterService.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Три live-стрима. Все задачи cluster-ветки приходят по wire; cluster-build
	// (community.redis.cluster, run_once) приезжает на бутстрап-ноду. default-success
	// (LoadApplyScript) покрывает все задачи каждого стрима — на L3a реализм
	// per-task не проверяется, важен lifecycle apply_runs success.
	for i := 0; i < 3; i++ {
		stub := stack.ConnectSoulStub(t, i)
		harness.LoadApplyScript(stub, "create", redisClusterCreateTasks())
	}

	// Простой типизированный ввод режима cluster: shards=3, replicas_per_shard=0 →
	// топология 3*(1+0)=3, ровно совпадает с roster-ом (size-guard PASS).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_type":           "cluster",
		"version":              "7.2.4",
		"shards":               3,
		"replicas_per_shard":   0,
		"cluster_node_timeout": 5000,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success ≠ incarnation.state закоммичен: ждём ready перед чтением.
	stack.WaitIncarnationReady(t, inc, 30)
	// cluster-факты: redis_type + cluster-директивы в redis_config (host-инвариантные:
	// cluster-enabled/cluster-config-file/cluster-node-timeout). announce-ip per-host
	// в state НЕ пишется. users/hosts пусты (точные роли раскладывает плагин).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_type": "cluster",
		"redis_config": map[string]any{
			"cluster-enabled":      "yes",
			"cluster-config-file":  "nodes.conf",
			"cluster-node-timeout": "5000",
		},
	})
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// redisClusterCreateTasks — scripted success по task-name ключевых задач
// cluster-ветки create: install redis (destiny redis), render redis.conf
// (cluster-директивы), health-gate PING, cluster-build (community.redis.cluster).
// default-success (LoadApplyScript) покрывает остальные задачи destiny и
// when:-погашенные ветки standalone/sentinel/sentinel_only.
func redisClusterCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Render redis.conf"},
		{TaskName: "Ensure redis-server is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-server": "running"}}}},
		{TaskName: "Wait for each cluster node to answer PING"},
		{TaskName: "Build the redis cluster (CLUSTER MEET/ADDSLOTS/REPLICATE)"},
	}
}
