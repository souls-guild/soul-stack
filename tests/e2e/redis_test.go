//go:build e2e

// L3a E2E: examples/service/redis::create (standalone mode, redis
// consolidation, ADR-039 /
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
//  5. CreateIncarnationOnRoster -> create run -> WaitApplySuccess.
//  6. Asserts: apply_runs success / incarnation.state (type/version/merged-config/
//     users/hosts) / audit incarnation.scenario_started / metric
//     keeper_scenario_runs_total.
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

	// Materialize the mode-agnostic destiny `redis` into a file:// repo +
	// set keeper_settings[default_destiny_source]. BEFORE RegisterService:
	// the invalidate from POST /v1/services will pull the setting into the
	// Holder without waiting for a TTL poll.
	// ★ FOUR destinies, not one. The create scenario stopped being redis-only:
	// it now also applies redis-exporter and node-exporter (metrics) and vector
	// (the mandatory log plane, ADR-067). Materializing just `redis` left the
	// render aborting on `cloning .../destiny-repos/node-exporter: repository
	// not found` — the example grew an observability contract and this test
	// never learned. NIM-223.
	stack.MaterializeDestinies(t, "v1.0.0", "redis", "redis-exporter", "node-exporter", "vector")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream: capture the SID-lease -> ApplyRequest into the local
	// Outbound. LoadApplyScript -- scripted success by task-name (+
	// default-success for when:-collector tasks). The community.redis.config
	// task is matched by task_name.
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisCreateTasks())

	// Seed row -> bind roster -> run create, the order owned by
	// CreateIncarnationOnRoster (NIM-210): membership carries an FK on the
	// incarnation row, so the host cannot be bound first. The roster resolves
	// members via incarnation_membership (ADR-008 amendment, NIM-124); without
	// it the scenario sees no_hosts -> error_locked.
	//
	// Simple typed operator input. version -- one of the Nexus-published
	// builds (covenant.yml declares a CLOSED enum; the former free-form
	// distro-pin is no longer accepted), giving a non-empty
	// state.redis_version; memory_mb+persistence+policy are translated into
	// the merged redis_config; users -- typed map with a full ACL string.
	inc, applyID := stack.CreateIncarnationOnRoster(t, incName, "redis@main", "create", []int{0}, map[string]any{
		// ★ provision is DEFAULT-ON in this example (user decision 2026-06-30):
		// an operator who passes nothing gets cloud-create, and the run dies on
		// `resolve profile "redis-debian-12": profile: name not found` — L3a has
		// no cloud provider. Rolling onto a roster that already exists is the
		// EXPLICIT opt-out, exactly as covenant.yml documents it. The other
		// documented route, the create_from_souls twin, is not usable from this
		// harness: it carries a name_template, so it composes the incarnation
		// name and rejects the fixed one CreateIncarnationOnRoster seeds
		// (ADR-0079). NIM-223.
		"provision": map[string]any{"enabled": false},
		// ★ replicas_per_master must be stated: it defaults to 2 (the HA set,
		// covenant.yml), and with provision off the size-guard becomes ACTIVE and
		// demands a roster of exactly 1+replicas_per_master. This stack binds one
		// soul, so 0 is both the honest number and the "standalone-equivalent"
		// the covenant names for sentinel mode — which is what the state assertion
		// below expects. NIM-223.
		"replicas_per_master": 0,
		"version":             "7.4.1",
		"memory_mb":           1024,
		"persistence":         "rdb",
		"maxmemory_policy":    "volatile-lru",
		// users is an ARRAY of AclUser {name, perms, state}, not a name→record
		// map (covenant.yml `users: type: array, items: {$type: AclUser}`,
		// types.yml::AclUser). The map form predates the redis consolidation and
		// was rejected as `input_invalid: $.users ... does not match type "array"`
		// — the test was written against a contract that no longer exists
		// (NIM-223).
		"users": []any{
			map[string]any{
				"name":  "app",
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success != every capture is in: a core.state.<verb> step standing
	// after the host work commits after those hosts report success ([ADR-0084]).
	// Wait for ready before reading.
	stack.WaitIncarnationReady(t, inc, 30)
	// redis_config -- the RESULT of the merge() translation (the same one
	// that went into rendering redis.conf): maxmemory=1024*75/100=768mb
	// (essence.memory_reserve_percent=75), policy/save/appendonly from
	// input+persistence preset, maxclients/timeout from the essence baseline.
	stack.AssertIncarnationState(t, inc, map[string]any{
		// ★ `standalone` is not a state value any more — redis_type records the
		// enum the operator chose, and the enum is [sentinel, cluster]. A single
		// master with replicas_per_master=0 IS the standalone-equivalent, but the
		// covenant says so in prose; the state says `sentinel`. NIM-223.
		"redis_type":    "sentinel",
		"redis_version": "7.4.1",
		"redis_config": map[string]any{
			"maxmemory":        "768mb",
			"maxmemory-policy": "volatile-lru",
			"appendonly":       "no",
			"save":             "900 1 300 10 60 10000",
			"maxclients":       float64(10000),
			"timeout":          float64(300),
		},
		// ★ redis_users mirrors the INPUT array now, not a name→record map — the
		// same shape change as `users` above, carried through to state.
		"redis_users": []any{
			map[string]any{
				"name":  "app",
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
		"redis_hosts": []any{},
		// ★ The observability contract is part of what this test guards now: the
		// create scenario applies redis-exporter / node-exporter / vector, and it
		// was their ABSENCE from MaterializeDestinies that aborted the render.
		// Asserting the recorded versions means a future change to that set fails
		// here loudly instead of only when someone forgets a destiny.
		"monitoring": map[string]any{
			"redis_exporter_version": "1.62.0",
			"redis_exporter_listen":  ":9121",
			"node_exporter_version":  "1.8.2",
			"node_exporter_listen":   ":9100",
		},
		"logging": map[string]any{
			"vector_version":       "0.40.0",
			"vector_sink_type":     "console",
			"vector_log_sources":   []any{"/var/log/redis/*.log"},
			"vector_sink_endpoint": "",
		},
	})
	// create is an explicit run here (NIM-210), so the run endpoint writes
	// incarnation.scenario_started with that run's apply_id in the payload --
	// incarnation.created belongs to the POST /v1/incarnations path.
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
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
