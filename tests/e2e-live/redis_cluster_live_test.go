//go:build e2e_live

// L3b E2E multi-host: examples/service/redis::create in cluster mode on three
// REAL soul containers (redis consolidation of Ansible-role concept, ADR-039 /
// .pm/tasks/2026-06-22-redis-consolidation).
//
// Previous self-contained redis-cluster-live service (core modules + redis-cli
// --cluster create, proven by live mega-test on 2026-05-25) was removed: cluster
// flow is absorbed by cluster mode of consolidated redis, which forms the cluster
// ENTIRELY through community.redis.cluster plugin (CLUSTER MEET/ADDSLOTS/REPLICATE
// through go-redis), not redis-cli --cluster create.
//
// Body below is retargeted to consolidated redis (redis_type=cluster, shards=3,
// replicas_per_shard=0 -> 3 masters without replicas, minimum for cluster_state:ok
// on a 3-node stand). Stack starts three privileged Debian-12 systemd-PID-1
// containers (soul-live-a/-b/-c.example.com) with real CSR handshake.
//
// t.Skip: cluster-create through community.redis.cluster is not yet proven live
// end-to-end (render is checked at L0 - scenario/create/tests/cluster-*, but
// container bootstrap of cluster through plugin on top of soul containers was not
// run: harness has no wrapper to wait for cluster_state:ok through plugin path).
// The same backlog blocker is captured by redis_cluster_remove_node_lossless_test.go.
// Reactivate together with cluster-create live (plugin bootstrap + cluster-aware
// verify in harness).
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

func TestL3bRedisClusterCreate_ThreeNode(t *testing.T) {
	t.Skip("backlog (redis consolidation): cluster-create through community.redis.cluster is not proven live end-to-end (render is checked at L0 scenario/create/tests/cluster-*, but plugin cluster bootstrap on top of soul containers is not wrapped by harness - no helper waits for cluster_state:ok through plugin path). Symmetric with redis_cluster_remove_node_lossless_test.go::t.Skip. Reactivate with harness cluster-bootstrap/verify helpers - .pm/tasks/2026-06-22-redis-consolidation")

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       3,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 3 {
		t.Fatalf("expected 3 soul containers, got %d", got)
	}
	wantSIDs := []string{
		"soul-live-a.example.com",
		"soul-live-b.example.com",
		"soul-live-c.example.com",
	}
	for i, want := range wantSIDs {
		if got := stack.SoulContainers[i].SID; got != want {
			t.Fatalf("SoulContainers[%d].SID = %q, expected %q", i, got, want)
		}
	}

	const incName = "redis-clstr-create"

	// Coven membership BEFORE Create: roster resolves by `incarnation.name in
	// coven[]` (ADR-008). All three Souls are in incarnation coven; otherwise
	// no_hosts -> zero apply_runs.
	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
		// cluster nodes-MAP is built from soulprint.hosts (primary_ip by SID);
		// wait for non-empty facts BEFORE create-render.
		stack.WaitSoulprintReported(t, i, 60)
	}

	// requirepass + cluster-build password use keeper-side vault() resolution.
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": "cluster-redis-secret-32b"})

	// Materialize mode-agnostic destiny `redis` (cluster branch calls
	// apply: destiny: redis) under git tag v1.0.0 (ref from service.yml::destiny[]).
	stack.MaterializeDestinies(t, "v1.0.0", "redis")

	// POST /v1/incarnations auto-starts create -> returns apply_id of auto-run.
	inc, applyID := stack.CreateIncarnationWithApply(t, incName, "redis@main", map[string]any{
		"redis_type":         "cluster",
		"version":            "7.2.4",
		"shards":             3,
		"replicas_per_shard": 0,
	})

	// 600 s: apt-get install redis x 3 + render config + start + plugin
	// community.redis.cluster bootstrap (CLUSTER MEET/ADDSLOTS).
	stack.WaitApplySuccess(t, applyID, 600)
	stack.WaitIncarnationReady(t, inc, 30)

	exp := harness.LoadExpectations(t, "redis/expectations/after-create-cluster.yaml")
	stack.AssertExpectations(t, exp, applyID, inc)

	// Independent cluster health check: redis-cli cluster info inside first soul
	// container. Polling: cluster_state switches to ok after all cluster bus
	// connections are established.
	sc := stack.SoulContainers[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		lastOutput string
		lastExit   int
		ok         bool
	)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, code, err := sc.Exec(ctx, []string{"redis-cli", "-p", "6379", "cluster", "info"})
		if err != nil {
			t.Fatalf("redis-cli cluster info: %v", err)
		}
		lastOutput, lastExit = out, code
		if code == 0 && strings.Contains(out, "cluster_state:ok") {
			ok = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !ok {
		t.Fatalf("redis cluster did not reach cluster_state:ok (last_exit=%d) output:\n%s",
			lastExit, lastOutput)
	}
}
