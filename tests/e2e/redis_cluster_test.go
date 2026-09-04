//go:build e2e

// L3a contract-e2e: examples/service/redis::create in cluster mode
// (redis consolidation, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// cluster -- honest hash-slot Redis Cluster: the create dispatcher includes
// the create/cluster.yml branch (standalone/sentinel/sentinel_only are
// suppressed by the static-when placeholder-skip). Multi-host roster (3
// hosts): shards=3, replicas_per_shard=0 -> topology exactly 3*(1+0)=3
// (size-guard passes). The contract checks the keeper-side
// render->dispatch->RunResult->state chain for the cluster branch on a
// 3-node fixture, NOT the real gossip topology (that's L3b-live,
// tests/e2e-live).
//
// The previous separate redis-cluster / redis-cluster-live services were
// removed: their cluster flow was absorbed into the cluster mode of the
// consolidated redis (dispatcher by redis_type + day-2 scenarios
// add_node/remove_node/reshard). Probe->where (the original redis-cluster
// update_acl) was moved to the stage-A rolling-restart scenario
// (scenario/restart) -- not duplicated here.
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper + 3 soul-stubs.
//  2. Seed Vault (requirepass) + soulprint(os/net) + Coven on each of the three.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript on each host (default-success covers
//     all tasks of the cluster branch; cluster-build run_once lands on the
//     bootstrap node).
//  5. CreateIncarnationOnRoster(redis_type=cluster, shards=3) -> create run
//     -> WaitApplySuccess -> WaitIncarnationReady.
//  6. Asserts: apply_runs success / incarnation.state{redis_type=cluster,
//     cluster directives in redis_config} / audit / metric.
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

	// requirepass is read keeper-side via
	// vault('secret/redis/'+incarnation.id+'#password') (cluster.yml apply.input
	// + cluster-build password). Without the secret, the render phase fails
	// with "vault-ref: KV path not found". rel WITHOUT the mount/`data/`
	// prefix -- SeedVaultKV adds them (KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-cluster-secret",
	})

	// Soulprint for all three (membership is bound later, by
	// CreateIncarnationOnRoster). network.primary_ip is needed to render
	// redis.conf.tmpl (cluster-announce-ip per-host) and the cluster nodes-MAP
	// (soulprint.hosts.map by SID). pkg_mgr/init_system --
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
	}

	// Materialize the mode-agnostic destiny `redis` (the cluster branch
	// calls apply: destiny: redis) + set default_destiny_source. BEFORE
	// RegisterService.
	// ★ FOUR destinies, not one — the create scenario also applies
	// redis-exporter / node-exporter (metrics) and vector (the mandatory log
	// plane, ADR-067). See the note in redis_test.go. NIM-223.
	stack.MaterializeDestinies(t, "v1.0.0", "redis", "redis-exporter", "node-exporter", "vector")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Three live streams. All tasks of the cluster branch arrive over the
	// wire; cluster-build (redis.cluster.*, run_once) lands on the
	// bootstrap node. default-success (LoadApplyScript) covers all tasks of
	// each stream -- at L3a per-task realism is not checked, what matters is
	// the apply_runs success lifecycle.
	for i := 0; i < 3; i++ {
		stub := stack.ConnectSoulStub(t, i)
		harness.LoadApplyScript(stub, "create", redisClusterCreateTasks())
	}

	// Seed row -> bind all three Souls -> run create, the order owned by
	// CreateIncarnationOnRoster (NIM-210): membership carries an FK on the
	// incarnation row, so the hosts cannot be bound first (roster via
	// incarnation_membership, NIM-124).
	//
	// Simple typed input for cluster mode: shards=3, replicas_per_shard=0 ->
	// topology 3*(1+0)=3, exactly matches the roster (size-guard PASS).
	inc, applyID := stack.CreateIncarnationOnRoster(t, incName, "redis@main", "create", stack.AllSoulIndexes(), map[string]any{
		// ★ provision is DEFAULT-ON in this example (user decision 2026-06-30):
		// without this the run reaches core.cloud.created and dies on
		// `resolve profile "redis-debian-12": profile: name not found` — L3a has
		// no cloud provider. Deploying onto an existing roster is the EXPLICIT
		// opt-out that covenant.yml documents; the create_from_souls twin is not
		// usable here because its id_template composes the incarnation name
		// and rejects the fixed one this harness seeds (ADR-0079). NIM-223.
		"provision":  map[string]any{"enabled": false},
		"redis_type": "cluster",
		"version":    "7.4.1",
		"shards":     3,
		// ★ replicas_per_shard no longer exists — it was unified into a single
		// replicas_per_master for both modes (2026-06-25), which is what the
		// size-guard reads. Sending the old name left the real field at its
		// default of 2, so the guard demanded 3*(1+2)=9 hosts for a 3-soul stack.
		// 0 replicas is what this stack actually has. NIM-223.
		"replicas_per_master":  0,
		"cluster_node_timeout": 5000,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success != incarnation.state committed: wait for ready before reading.
	stack.WaitIncarnationReady(t, inc, 30)
	// cluster facts: redis_type + cluster directives in redis_config
	// (host-invariant: cluster-enabled/cluster-config-file/cluster-node-timeout).
	// announce-ip per-host is NOT written to state. users/hosts are empty
	// (the exact roles are laid out by the plugin).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_type": "cluster",
		"redis_config": map[string]any{
			"cluster-enabled":      "yes",
			"cluster-config-file":  "nodes.conf",
			"cluster-node-timeout": "5000",
		},
	})
	// create is an explicit run here (NIM-210) -> incarnation.scenario_started,
	// not incarnation.created (the POST /v1/incarnations event).
	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// redisClusterCreateTasks -- scripted success by task-name for the key tasks
// of the cluster create branch: install redis (destiny redis), render
// redis.conf (cluster directives), health-gate PING, cluster-build
// (redis.cluster.*). default-success (LoadApplyScript) covers the
// remaining destiny tasks and the when:-suppressed standalone/sentinel/
// sentinel_only branches.
func redisClusterCreateTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Render redis.conf"},
		{TaskName: "Ensure redis-server is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-server": "running"}}}},
		{TaskName: "Wait for each cluster node to answer PING"},
		{TaskName: "Build the redis cluster (CLUSTER MEET/ADDSLOTS/REPLICATE)"},
	}
}
