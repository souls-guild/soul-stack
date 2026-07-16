//go:build e2e_live

// L3b E2E pilot: examples/service/redis::create (standalone mode) -
// real-soul-in-container (redis consolidation of the Ansible role concept, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// A parallel to tests/e2e/redis_test.go (L3a, soul-stub answers scripted), but
// going through a REAL apt-install redis-server + render redis.conf inside a
// Debian-12-systemd soul container. The create dispatcher includes the standalone
// branch (cluster/sentinel/sentinel_only are gated off by static-when), which unfolds
// the mode-agnostic `redis` destiny.
//
// Exporters (redis_exporter/node_exporter) are FACTORED OUT of service/redis: monitoring is a
// separate entity. The former exporter-coupled pilot is replaced by a clean
// standalone-redis live-create.
//
// Coverage L3a doesn't give: Keeper render -> ApplyRequest on the wire -> real
// soul Apply (core.pkg / core.file.rendered / core.service) -> RunResult ->
// apply_runs success + redis.conf actually rendered with merged directives.
//
// Harness mechanics (see tests/e2e-live/harness):
//   - SeedVaultKV: redis password keeper-side via vault('secret/redis/<inc>#password');
//   - MaterializeDestinies + default_destiny_source: apply:destiny resolve git-URL;
//   - WaitSoulprintReported: redis.conf.tmpl binds to soulprint.self.network.primary_ip;
//   - AssertExpectations: apply_runs / incarnation_state / host_state (redis.conf
//     rendered, redis-server active).
//
// Timeout 300s - apt-update + apt-get install redis-server + render + systemctl
// start. Slow on cold CI.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisLive_CreateStandalone(t *testing.T) {
	t.Skip("redis parity is proven by a stand live-run (PHASE 2, run-23 sample), not a local gate: input version=Nexus-enum, standalone mode removed 2026-06-25, install_method=binary requires Nexus, exporter/vector destinies require egress. The local gate guarantees module-delivery MECHANICS - tests/e2e-live/module-delivery-live (NIM-32). Symmetric to redis_cluster_live_test.go::t.Skip.")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	const wantSID = "soul-live-a.example.com"
	if sc := stack.SoulContainers[0]; sc.SID != wantSID {
		t.Errorf("SoulContainers[0].SID = %q, expected %q", sc.SID, wantSID)
	}

	const incName = "redis"

	// Vault seed of the redis password: standalone create reads it keeper-side via
	// vault('secret/redis/'+incarnation.name+'#password'); per-user password -
	// vault('secret/redis/'+incarnation.name+'/users/<name>#password'). rel WITHOUT
	// mount/`data/` prefix (SeedVaultKV adds them, KV v2).
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{
		"password": "e2e-redis-secret",
	})
	harness.SeedVaultKV(t, stack, "redis/"+incName+"/users/app", map[string]any{
		"password": "e2e-app-user-secret",
	})

	// Coven membership BEFORE Create: the roster is resolved by `incarnation.name in coven[]`
	// (ADR-008). The bootstrap flow set status='connected', but coven is empty.
	stack.AddSoulToCoven(t, 0, incName)

	// Wait for the real soul's first SoulprintReport: redis.conf.tmpl binds to
	// soulprint.self.network.primary_ip keeper-side during render.
	stack.WaitSoulprintReported(t, 0, 60)

	// Materialize the mode-agnostic `redis` destiny at git tag v1.0.0 (ref from
	// service.yml::destiny[]) + set default_destiny_source (SeedDefaultDestinySource
	// is gated on holderRefreshGrace - Holder will pick up the value BEFORE create-render).
	stack.MaterializeDestinies(t, "v1.0.0", "redis")

	// Simple typed input: version (distro-native pin), memory_mb +
	// persistence + maxmemory_policy -> merge-translated into redis.conf; users - typed-map.
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

	stack.WaitApplySuccess(t, applyID, 300)
	// apply_runs success != state committed - wait for ready before reading state.
	stack.WaitIncarnationReady(t, inc, 30)

	exp := harness.LoadExpectations(t, "redis/expectations/after-create.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// apply_id in the audit event payload is a runtime value, separate from the YAML fixture.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"apply_id": applyID,
	})
}
