//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cRedisCluster_Resharding - L3c-5 part B: porting the megatest from
// 2026-05-25 (project_megatest_ha_scale_2026_05_25.md) to kind. 3 Soul pods ->
// scenario `create` for the consolidated redis (redis_type=cluster, 3-master
// cluster, replicas_per_shard=0) -> assert `cluster_state:ok` via `redis-cli
// cluster info` in one of the pods.
//
// The former standalone cluster service was removed (redis consolidation): the cluster flow
// was absorbed into the cluster mode of the consolidated redis.
//
// Pre-requisites:
//   - a service catalog entry for `redis` (POST /v1/services).
//   - the keeper pod MUST see the git repo `redis`. In L3b this is a file:// URL to a
//     host-side bare repo (see tests/e2e-live/harness/git.go); in a kind cluster
//     file:// from a pod is unreachable - there's no host FS. Need either an in-cluster
//     git-server pod (sidecar/standalone), or a mount/projected volume with
//     a pre-built bare repo.
//
// L3c-5 does NOT close the git-server infra gap for kind. The test as it stands is
// a t.Skip scaffold with a clear diagnostic; the real run is L3c-future
// (a separate slice per architect verdict - the new `git-server-pod` entity in
// the harness needs propose-and-wait).
//
// Duration if enabled: ~15 min (apt-get install redis x 3 pods +
// cluster-create).
func TestL3cRedisCluster_Resharding(t *testing.T) {
	t.Skip("L3c-5 part B blocked: no git-server-pod infra for the service-loader in kind. " +
		"Needs a separate slice (propose-and-wait for the name `git-server-pod` + " +
		"an alpine/git or nginx-git-http-backend deployment).")

	// Scaffold remains for future unblocking (L3c-future): once
	// harness.DeployGitServerPod + Stack.RegisterService with an in-cluster URL exist,
	// this code becomes boilerplate.
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/redis",
		ServiceName: "redis",
		Souls:       3,
	})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	_ = stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	stack.BootstrapArchon(t)

	sids := stack.DeployMultiSoul(t, 3)
	if len(sids) != 3 {
		t.Fatalf("DeployMultiSoul: expected 3 SID, got %d", len(sids))
	}

	incName := stack.CreateIncarnation(t, "test-redis-resharding", "redis@main", map[string]any{
		"redis_type":         "cluster",
		"version":            "7.2.4",
		"shards":             3,
		"replicas_per_shard": 0,
	})
	applyID := stack.RunScenario(t, incName, "create", map[string]any{
		"redis_type":         "cluster",
		"version":            "7.2.4",
		"shards":             3,
		"replicas_per_shard": 0,
	})
	stack.WaitApplySuccess(t, applyID, 600)

	// Independent verify: cluster_state:ok via exec in soul-0.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	var lastOutput string
	var lastExit int
	ok := false
	for time.Now().Before(deadline) {
		out, code, err := stack.ExecInSoulPod(ctx, "soul-0", []string{
			"redis-cli", "-p", "6379", "cluster", "info",
		})
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
