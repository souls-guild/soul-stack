//go:build e2e

// L3a контракт-e2e: examples/service/redis-cluster-live — ВСЕ сценарии (ADR-039).
//
// В отличие от pilot redis (apply:destiny, три standalone-destiny) этот сервис —
// честный cluster-mode Redis целиком на core-модулях (core.pkg/core.file/
// core.cmd/core.file.rendered), без apply:destiny/vault/essence. Контракт
// проверяет keeper-side цепочку render→dispatch→RunResult→state по реальным
// task-name каждого scenario, НЕ реальную 9-нодовую топологию (это L3b-live,
// tests/e2e-live). Поэтому фикстура — ОДНА нода: один apply_runs-row → success,
// и `where:`-фильтрованные сценарии (add_replica/remove_replica/reshard) не
// порождают не-целевых host-ов в no_match (no_match — терминал-не-провал, но
// harness.WaitApplySuccess его фейлит).
//
// Зеркало fixtures/stub-responses.yaml + expectations/after-<scenario>.yaml
// (YAML-loader fixtures не реализован — значения inline, pilot-паттерн).
//
// Flow на сценарий:
//  1. NewStack (PG+Redis+Vault testcontainers + Keeper-процесс + 1 soul-stub).
//  2. SeedSoulprint(os + network.primary_ip) + AddSoulToCoven(=incarnation.name).
//  3. RegisterService(redis-cluster-live) + ConnectSoulStub + LoadApplyScript.
//  4. create через CreateIncarnationWithApply → WaitApplySuccess → ready.
//  5. Мутирующий scenario через RunScenario → WaitApplySuccess → ready.
//  6. Asserts: apply_runs success / incarnation.state по sets / audit / metric.
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

const (
	rclService  = "redis-cluster-live"
	rclExample  = "examples/service/redis-cluster-live"
	rclSoulIP   = "10.0.0.10" // network.primary_ip единственного host-а L3a-фикстуры.
	rclRedisVer = "7:7.2.4-1~deb12u1"
)

// rclSoulprint — os + network.primary_ip единственного host-а. os.pkg_mgr/init_system
// нужны core.pkg/core.service; network.primary_ip — render cluster-nodes.txt
// (soulprint.hosts.map) и `where:`-таргетинг по soulprint.self.network.primary_ip.
func rclSoulprint() map[string]any {
	return map[string]any{
		"os": map[string]any{
			"family":      "debian",
			"distro":      "debian",
			"version":     "12",
			"arch":        "amd64",
			"pkg_mgr":     "apt",
			"init_system": "systemd",
		},
		"network": map[string]any{
			"primary_ip": rclSoulIP,
		},
		"hostname": "soul-a",
	}
}

// rclStack поднимает стенд с одним soul-ом, сидирует soulprint + Coven-членство
// (coven = incName, ADR-008) и регистрирует сервис. stub открывает caller.
func rclStack(t *testing.T, incName string) *harness.Stack {
	t.Helper()
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rclExample,
		Souls:       1,
	})

	stack.SeedSoulprint(t, 0, rclSoulprint())
	stack.AddSoulToCoven(t, 0, incName)
	stack.RegisterService(t, rclService, rclExample)
	return stack
}

// rclCreate прогоняет scenario create на свежей incarnation и ждёт ready.
// Возвращает apply_id авто-create-прогона.
func rclCreate(t *testing.T, stack *harness.Stack, stub *soulstub.Stub, incName string) string {
	t.Helper()
	harness.LoadApplyScript(stub, "create", rclCreateTasks())
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, rclService+"@main", map[string]any{
		"redis_version": rclRedisVer,
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, inc, 30)
	return applyID
}

// --- create (standalone) ---------------------------------------------------

func TestE2EServiceRedisClusterLive_Create(t *testing.T) {
	const incName = "redis-cluster-live-create"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	applyID := rclCreate(t, stack, stub, incName)

	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_version":    rclRedisVer,
		"cluster_replicas": 2,
		"redis_users":      []any{},
		"redis_config":     map[string]any{},
		"cluster_nodes":    []any{},
	})
	// POST /v1/incarnations авто-запускает create → incarnation.created с apply_id
	// авто-прогона (НЕ scenario_started).
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// --- add_user (после create) -----------------------------------------------

func TestE2EServiceRedisClusterLive_AddUser(t *testing.T) {
	const incName = "redis-cluster-live-add-user"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	// add_user-задача — отдельный task-name; заряжаем её скрипт (default-success
	// уже включён LoadApplyScript-ом из create, но повторный заряд явен и
	// детерминирует state_changes по task-name).
	harness.LoadApplyScript(stub, "add_user", rclAddUserTasks())

	// users перезаписывает redis_users целиком (sets: redis_users ← input.users).
	users := []any{
		map[string]any{"name": "app", "acl": "on >app-pass ~app:* +@read +@write"},
		map[string]any{"name": "readonly", "acl": "on >ro-pass ~* +@read"},
	}
	applyID := stack.RunScenario(t, incName, "add_user", map[string]any{"users": users})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_users": users,
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_user",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- update_config (после create) ------------------------------------------

func TestE2EServiceRedisClusterLive_UpdateConfig(t *testing.T) {
	const incName = "redis-cluster-live-update-config"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	harness.LoadApplyScript(stub, "update_config", rclUpdateConfigTasks())

	applyID := stack.RunScenario(t, incName, "update_config", map[string]any{
		"maxmemory":        "512mb",
		"maxmemory_policy": "allkeys-lru",
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_config": map[string]any{
			"maxmemory":        "512mb",
			"maxmemory-policy": "allkeys-lru",
		},
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "update_config",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- update_acl (после create) ---------------------------------------------

func TestE2EServiceRedisClusterLive_UpdateAcl(t *testing.T) {
	const incName = "redis-cluster-live-update-acl"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	harness.LoadApplyScript(stub, "update_acl", rclUpdateAclTasks())

	// update_acl — точечный update без state_changes (redis_users остаётся []).
	// host-инвариантна (без where:) → задача приезжает на host.
	applyID := stack.RunScenario(t, incName, "update_acl", map[string]any{
		"user": "app",
		"acl":  "on >new-pass ~app:* +@all",
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertIncarnationState(t, incName, map[string]any{
		"redis_users": []any{},
	})
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "update_acl",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- add_replica (после create) --------------------------------------------

func TestE2EServiceRedisClusterLive_AddReplica(t *testing.T) {
	const incName = "redis-cluster-live-add-replica"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	harness.LoadApplyScript(stub, "add_replica", rclAddReplicaTasks())

	// new_node_sid = SID единственного host-а → `where: soulprint.self.sid ==
	// input.new_node_sid` матчит его, все 6 задач приезжают (без no_match).
	applyID := stack.RunScenario(t, incName, "add_replica", map[string]any{
		"new_node_sid": stack.SoulSID(0),
		"new_node_ip":  rclSoulIP,
		"seed_ip":      rclSoulIP,
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_replica",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- remove_replica (после create) -----------------------------------------

func TestE2EServiceRedisClusterLive_RemoveReplica(t *testing.T) {
	const incName = "redis-cluster-live-remove-replica"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	harness.LoadApplyScript(stub, "remove_replica", rclRemoveReplicaTasks())

	// seed_ip и remove_ip = primary_ip единственного host-а → обе задачи
	// (where: по network.primary_ip) приезжают на него.
	applyID := stack.RunScenario(t, incName, "remove_replica", map[string]any{
		"seed_ip":   rclSoulIP,
		"remove_ip": rclSoulIP,
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "remove_replica",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- reshard (после create) ------------------------------------------------

func TestE2EServiceRedisClusterLive_Reshard(t *testing.T) {
	const incName = "redis-cluster-live-reshard"
	stack := rclStack(t, incName)
	defer stack.Cleanup()

	stub := stack.ConnectSoulStub(t, 0)
	rclCreate(t, stack, stub, incName)

	harness.LoadApplyScript(stub, "reshard", rclReshardTasks())

	// seed_ip = primary_ip host-а → единственная задача (where: по primary_ip)
	// приезжает на него. from_ip/to_ip — валидные по pattern IP (на L3a shell не
	// исполняется, реальной миграции слотов нет; важна валидность input + lifecycle).
	applyID := stack.RunScenario(t, incName, "reshard", map[string]any{
		"seed_ip": rclSoulIP,
		"from_ip": rclSoulIP,
		"to_ip":   "10.0.0.11",
		"slots":   100,
	})
	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	stack.WaitIncarnationReady(t, incName, 30)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "reshard",
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 2)
}

// --- scripted task-таблицы (зеркало fixtures/stub-responses.yaml) -----------

// rclCreateTasks — scripted success по task-name scenario/create/main.yml.
func rclCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis on all cluster nodes", StateChanges: map[string]any{"packages": []any{map[string]any{"redis": "installed"}}}},
		{TaskName: "Render redis cluster config (host-invariant)"},
		{TaskName: "Append cluster-announce-ip from host address"},
		{TaskName: "Start redis-server on each node"},
		{TaskName: "Wait until redis responds on each node"},
		{TaskName: "Render cluster node-address list on bootstrap node"},
		{TaskName: "Form the redis cluster"},
	}
}

// rclAddUserTasks — scenario/add_user/main.yml (одна loop-задача).
func rclAddUserTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Apply Redis ACL on every cluster node for each user"},
	}
}

// rclUpdateConfigTasks — scenario/update_config/main.yml.
func rclUpdateConfigTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Apply maxmemory + policy at runtime on every node"},
		{TaskName: "Persist maxmemory in cluster config file"},
		{TaskName: "Rewrite redis config when the file changed"},
	}
}

// rclUpdateAclTasks — scenario/update_acl/main.yml.
func rclUpdateAclTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Update Redis ACL for a single user on every cluster node"},
	}
}

// rclAddReplicaTasks — scenario/add_replica/main.yml.
func rclAddReplicaTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis on the new node", StateChanges: map[string]any{"packages": []any{map[string]any{"redis": "installed"}}}},
		{TaskName: "Render cluster config on the new node"},
		{TaskName: "Append cluster-announce-ip on the new node"},
		{TaskName: "Start redis on the new node"},
		{TaskName: "Wait until new node responds"},
		{TaskName: "Join the cluster as a replica"},
	}
}

// rclRemoveReplicaTasks — scenario/remove_replica/main.yml.
func rclRemoveReplicaTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Remove the replica from the cluster by node-id"},
		{TaskName: "Stop redis on the removed node"},
	}
}

// rclReshardTasks — scenario/reshard/main.yml.
func rclReshardTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Reshard slots between masters"},
	}
}
