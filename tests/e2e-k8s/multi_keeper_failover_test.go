//go:build e2e_k8s

package e2e_k8s_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-k8s/harness"
)

// TestL3cMultiKeeper_KillLeader - L3c-4 HA failover scenario: 3 Keeper pods in
// a Deployment (replicas=3) + 1 Soul pod. One of the keeper pods (the elected
// Reaper leader) is killed via `kubectl delete pod --grace-period=0`;
// validates that:
//
//  1. the remaining 2 keepers pick up Reaper leadership once the Redis
//     lease expires (lock_ttl=15s, see harness/keeperyml.go::reaperBlock) - the new
//     leader shows up in `GET reaper:leader` with a KID different from the killed one;
//  2. Deployment self-healing restores 3 ready replicas (ReplicaSet
//     creates a new pod and it passes /readyz);
//  3. the Soul pod stays `souls.status='connected'` (if it wasn't held
//     by the killed Keeper) or reconnects through the failback endpoint
//     chain (if it was held there - the fallback-list keeper:9095 ClusterIP will route
//     to a live pod via kube-proxy round-robin).
//
// Pre-requisites: docker + kind + kubectl + helm + images `keeper:e2e-k8s` and
// `soul:e2e-k8s`. Missing any of these - t.Skip.
//
// ReaperEnabled=true in Config - otherwise the `reaper:leader` lease isn't written and
// the failover mechanics aren't validated. Default false for L3c-2/3 is preserved.
//
// Per-pod KID - via the init container `kid-render` (see
// manifests/keeper/deployment.yaml): substitutes $POD_NAME into the
// `__KID__` literal of the keeper.yml template. Without this all 3 pods would have the same
// KID and the failover mechanics would be indistinguishable (the lease holder would always be the same
// KID, a new pod would grab the same key instantly with no visible change).
func TestL3cMultiKeeper_KillLeader(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ReaperEnabled: true,
	})
	certPEM, keyPEM, caPEM := stack.DeployInfra(t)
	pf := stack.DeployKeeper(t, 3, certPEM, keyPEM, caPEM)
	_ = pf

	sid := stack.DeploySoul(t)

	// 1. Wait for the first Reaper leader to be elected. lock_ttl=15s - the leader
	//    shows up on the first successful Acquire. A 60s buffer covers the
	//    cold-start race (3 pods starting nearly simultaneously).
	leaderKID := waitForReaperLeader(t, stack, "", 60*time.Second)
	t.Logf("L3c-4 initial Reaper leader: %s", leaderKID)

	// 2. Find the pod by KID - convention KID=pod-name is maintained by the init
	//    container.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	leaderPod, err := stack.FindKeeperPodByKID(ctx, leaderKID)
	cancel()
	if err != nil {
		t.Fatalf("FindKeeperPodByKID(%s): %v", leaderKID, err)
	}

	// 3. Snapshot the SID-lease holder before failover. If the soul is held on
	//    the killed leader, it will reconnect to another keeper after the kill.
	ctxLease, cancelLease := context.WithTimeout(context.Background(), 10*time.Second)
	soulHolderBefore, err := stack.GetSoulLeaseHolder(ctxLease, sid)
	cancelLease()
	if err != nil {
		t.Fatalf("GetSoulLeaseHolder(%s): %v", sid, err)
	}
	t.Logf("L3c-4 SID-lease holder before kill: %q (killing leader %q)",
		soulHolderBefore, leaderKID)

	// 4. Kill the leader pod (grace-period=0 - simulate a crash, not a graceful
	//    Release-lease via defer; failover must rely on TTL).
	ctxKill, cancelKill := context.WithTimeout(context.Background(), 30*time.Second)
	if err := stack.DeleteKeeperPod(ctxKill, leaderPod); err != nil {
		cancelKill()
		t.Fatalf("DeleteKeeperPod(%s): %v", leaderPod, err)
	}
	cancelKill()

	// 5. Wait for the NEW leader (KID != killed one). Lease TTL=15s + acquire
	//    backoff 5s -> typically 15-20s; use a 90s buffer for CI load.
	newLeaderKID := waitForReaperLeader(t, stack, leaderKID, 90*time.Second)
	t.Logf("L3c-4 new Reaper leader: %s (was %s)", newLeaderKID, leaderKID)

	// 6. Deployment self-heals - ReplicaSet spins up a new pod to replace
	//    the deleted one, returns to Ready=3 within 60s (image load already
	//    happened, only the init container + readiness remain).
	stack.WaitForDeploymentReady(t, "keeper", 120*time.Second)

	// 7. The Soul pod should stay connected (or reconnect through the
	//    fallback ClusterIP to a live keeper). 120s - a generous buffer for
	//    the reconnect-flow timeout in Soul.
	stack.WaitForSoulConnected(t, sid, 2*time.Minute)

	if soulHolderBefore == leaderKID {
		t.Logf("L3c-4 SID-lease was held by killed leader %s; soul reconnect verified by WaitForSoulConnected",
			leaderKID)
	}
}

// waitForReaperLeader polls `reaper:leader` until a KID different from
// excludeKID shows up (for an empty string - any KID). Returns the observed KID
// or fails t on timeout.
//
// Used twice in the failover test: (a) before kill - `excludeKID=""`,
// any leader will do; (b) after kill - `excludeKID=<old>`, we specifically need
// the NEW one.
func waitForReaperLeader(t *testing.T, stack *harness.Stack, excludeKID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		kid, err := stack.GetReaperLeaderKID(ctx)
		cancel()
		if err != nil {
			t.Logf("waitForReaperLeader: GET reaper:leader: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		last = kid
		if kid != "" && kid != excludeKID {
			return kid
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("waitForReaperLeader: no leader satisfying exclude=%q within %v (last=%q)",
		excludeKID, timeout, last)
	return ""
}
