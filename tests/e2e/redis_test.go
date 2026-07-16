//go:build e2e

// L3a E2E: examples/service/redis::create (standalone mode, redis
// consolidation of the Ansible-role concept, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// The redis service is collapsed into ONE mode-agnostic destiny `redis`
// (per-host install + render redis.conf + systemd) + the community.redis
// plugin for the live Redis runtime. scenario create (standalone) -- SIMPLE
// typed input -> translation:
//  1. apply destiny `redis` -- install redis-server + render redis.conf
//     (from the merged redis_config) + render users.acl + systemd.
//  2. community.redis.command PING -- health-gate after startup.
//
// The operator's simple input (memory_mb / persistence / maxmemory_policy /
// users) is TRANSLATED via merge() into the detailed redis_config (see
// scenario/create/main.yml). Translating the pilot input into a simple style
// is the 2026-06-22 redis consolidation
// (.pm/tasks/2026-06-22-redis-consolidation).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper + 1 soul-stub.
//  2. Seed Vault (requirepass + per-user password) + soulprint(os/net) + Coven.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript (scripted success by task-name, incl.
//     the community.redis.command task -- soul-stub matches by task_name, not by module).
//  5. CreateIncarnationWithApply -> auto-create run -> WaitApplySuccess.
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

	// Vault seed: requirepass is read keeper-side via
	// vault('secret/redis/'+incarnation.name+'#password'); per-user password
	// via vault('secret/redis/'+incarnation.name+'/users/<name>#password').
	// rel WITHOUT the mount/`data/` prefix -- SeedVaultKV adds them (KV v2).
	// Without the secret the render phase fails with "vault-ref: KV path not
	// found".
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/app", map[string]any{
		"password": "e2e-app-user-secret",
	})

	// soulprint seed: redis.conf.tmpl binds to soulprint.self.network.primary_ip;
	// pkg_mgr/init_system are needed by core.pkg/core.service (ADR-018, soulprint.md §3).
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

	// Coven membership BEFORE Create: the roster resolves via
	// `incarnation.name in coven[]` (ADR-008). Without it the scenario sees
	// no_hosts -> error_locked.
	stack.AddSoulToCoven(t, 0, incName)

	// Materialize the mode-agnostic destiny `redis` into a file:// repo +
	// set keeper_settings[default_destiny_source]. BEFORE RegisterService:
	// the invalidate from POST /v1/services will pull the setting into the
	// Holder without waiting for a TTL poll.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream: capture the SID-lease -> ApplyRequest into the local
	// Outbound. LoadApplyScript -- scripted success by task-name (+
	// default-success for when:-collector tasks). The community.redis.config
	// task is matched by task_name.
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisCreateTasks())

	// Simple typed operator input. version -- distro-native pin (for a
	// non-empty state.redis_version); memory_mb+persistence+policy are
	// translated into the merged redis_config; users -- typed map with a
	// full ACL string.
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
	// apply_runs success != incarnation.state committed: state_changes are
	// written in a separate transaction AFTER the barrier (run.go §8). Wait
	// for ready before reading.
	stack.WaitIncarnationReady(t, inc, 30)
	// redis_config -- the RESULT of the merge() translation (the same one
	// that went into rendering redis.conf): maxmemory=1024*75/100=768mb
	// (essence.memory_reserve_percent=75), policy/save/appendonly from
	// input+persistence preset, maxclients/timeout from the essence baseline.
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
	// POST /v1/incarnations auto-runs the create scenario and writes
	// incarnation.created with the auto-run's apply_id in the payload.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// TestE2EServiceRedis_AddAclUser -- SKIP: the add_acl_user scenario is moving
// to community.redis.acl (state acl not yet implemented -- next batch). See result.md.
func TestE2EServiceRedis_AddAclUser(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: add_acl_user is moving to community.redis.acl (state acl -- next batch) -- .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_UpdateConfig -- SKIP: the update_config scenario is moving
// to community.redis.config + re-apply destiny redis (next batch). See result.md.
func TestE2EServiceRedis_UpdateConfig(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: update_config is moving to community.redis.config + re-apply destiny redis (next batch) -- .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_UpdateNodeExporter -- SKIP: the exporter is a separate
// entity (monitoring), moved out of service/redis (role concept: only the
// ACL user monitoring, no exporters). See brief.md -> "Monitoring".
func TestE2EServiceRedis_UpdateNodeExporter(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: exporter moved out of service/redis (monitoring is a separate entity) -- .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_RestartNodeExporter -- SKIP: see UpdateNodeExporter
// (exporter moved out of service/redis).
func TestE2EServiceRedis_RestartNodeExporter(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: exporter moved out of service/redis (monitoring is a separate entity) -- .pm/tasks/2026-06-22-redis-consolidation")
}

// TestE2EServiceRedis_AddReplicas -- SKIP: replicas/topology are moving to
// sentinel mode + community.redis.replica (next batch). The probe->where +
// cross-host register invariant will move there too. See brief.md -> "Guard invariant migration".
func TestE2EServiceRedis_AddReplicas(t *testing.T) {
	t.Skip("WIP redis-consolidation 2026-06-22: add_replicas is moving to sentinel mode + community.redis.replica (probe->where invariant -- next batch) -- .pm/tasks/2026-06-22-redis-consolidation")
}

// redisCreateTasks -- scripted success responses by task-name for the create
// tasks of standalone mode: destiny `redis` tasks (install + render
// redis.conf/users.acl + systemd) + the community.redis.command task (PING
// health-gate). soul-stub matches by task_name (default-success covers
// when:-collector tasks and everything not in the script -- socket-dir is
// suppressed by static-when since there is no unixsocket in the config).
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
		// community.redis.command PING (live Redis health-gate). soul-stub success
		// by task_name; the plugin's changed semantics are covered by L0 (impl_test.go).
		{TaskName: "Wait for redis to answer PING"},
	}
}
