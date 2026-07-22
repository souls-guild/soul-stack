//go:build e2e

// L3a contract-E2E: examples/service/redis::create in sentinel_only mode
// (redis consolidation, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// sentinel_only -- a thin sentinel layer: deploys ONLY the sentinel daemon
// (the data-plane redis-server is NOT installed as a service), monitoring an
// EXTERNAL master from input.master_ip. This contract checks the keeper-side
// render->dispatch->RunResult->state chain specifically for the
// sentinel_only branch of the create dispatcher (the standalone/cluster/
// sentinel branches are suppressed by the static-when placeholder-skip). The
// point of the contract is that redis_sentinel.master_ip/master_name make it
// into incarnation.state from input, and apply_runs for all hosts reach
// success.
//
// The previous separate redis-sentinel service was removed: its create
// scenario was fully absorbed into the sentinel_only mode of the
// consolidated redis (dispatcher by redis_type,
// scenario/create/sentinel-only.yml).
//
// Flow:
//  1. NewStack: PG+Redis+Vault testcontainers + Keeper process + 1 soul-stub.
//  2. Seed Vault (auth_pass for the sentinel monitor) + soulprint(os/net) + Coven.
//  3. MaterializeDestinies(redis) + RegisterService(redis).
//  4. ConnectSoulStub + LoadApplyScript (scripted success by task-name).
//  5. CreateIncarnationWithApply(redis_type=sentinel_only + master_ip) ->
//     auto-create run -> WaitApplySuccess -> WaitIncarnationReady.
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

	// Vault seed: the sentinel daemon authenticates to the EXTERNAL master
	// with auth_pass, which the create branch resolves keeper-side via
	// vault('secret/redis/'+incarnation.name+'#password'). rel WITHOUT the
	// mount/`data/` prefix -- SeedVaultKV adds them (KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-sentinel-secret",
	})

	// soulprint seed: sentinel.conf.tmpl binds to soulprint.self.network.primary_ip;
	// pkg_mgr/init_system are needed by core.pkg/core.service keeper-side (ADR-018).
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

	// Membership BEFORE Create: the roster resolves members via
	// incarnation_membership (ADR-008 amendment, NIM-124). Without it the
	// scenario sees no_hosts -> error_locked.
	stack.AddMember(t, 0, incName)

	// Materialize the mode-agnostic destiny `redis` (the create branches
	// call apply: destiny: redis) + set default_destiny_source. BEFORE
	// RegisterService: the invalidate from POST /v1/services will pull the
	// setting into the Holder without a TTL poll.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	stack.RegisterService(t, "redis", "examples/service/redis")

	// Live EventStream: capture the SID-lease -> ApplyRequest into the local
	// Outbound. LoadApplyScript -- scripted success by task-name (+
	// default-success for the when:-suppressed dispatcher branches and
	// plugin tasks).
	stub := stack.ConnectSoulStub(t, 0)
	harness.LoadApplyScript(stub, "create", redisSentinelOnlyTasks())

	// Simple typed input: redis_type=sentinel_only + version (distro-native
	// pin) + master_ip (REQUIRED with sentinel_only -- required_when in main.yml).
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_type":  "sentinel_only",
		"version":     "5:7.0.15-1~deb12u7",
		"master_ip":   "10.9.9.9",
		"master_port": 6379,
	})

	stack.WaitApplySuccess(t, applyID, 60)
	stack.AssertApplyRunsStatus(t, applyID, "success")
	// apply_runs success != incarnation.state committed: state_changes are
	// written in a separate transaction AFTER the barrier (run.go §8). Wait
	// for ready before reading.
	stack.WaitIncarnationReady(t, inc, 30)
	// sentinel_only facts: redis_type + redis_sentinel{master_name(default mymaster),
	// master_ip from input}. redis_config is empty (no data plane); users/hosts are empty.
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_type": "sentinel_only",
		"redis_sentinel": map[string]any{
			"master_name": "mymaster",
			"master_ip":   "10.9.9.9",
		},
	})
	// POST /v1/incarnations auto-runs the create scenario and writes
	// incarnation.created with the auto-run's apply_id in the payload.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
	stack.AssertMetricGE(t, `keeper_scenario_runs_total{result="ok"}`, 1)
}

// redisSentinelOnlyTasks -- scripted success responses by task-name for the
// key tasks of the sentinel_only create branch: dispatcher mode-guard,
// install redis (carries the sentinel daemon), render sentinel.conf,
// SENTINEL MONITOR + PONG-gate. soul-stub matches by task_name;
// default-success (LoadApplyScript) covers the when:-suppressed
// standalone/cluster/sentinel branches and other destiny tasks.
func redisSentinelOnlyTasks() []harness.TaskResponse {
	return []harness.TaskResponse{
		{TaskName: "Install redis-server package", StateChanges: map[string]any{"packages": []any{map[string]any{"redis-server": "installed"}}}},
		{TaskName: "Render sentinel.conf"},
		{TaskName: "Ensure redis-sentinel is running and enabled at boot", StateChanges: map[string]any{"services": []any{map[string]any{"redis-sentinel": "running"}}}},
		{TaskName: "Monitor the external master via SENTINEL MONITOR"},
		{TaskName: "Wait for sentinel to answer PING"},
	}
}
