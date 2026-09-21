//go:build e2e_k8s

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// leader.go — L3c-4 harness helper for introspecting Redis lease keys in the
// kind cluster: `reaper:leader` (Reaper leadership KID) and
// `soul:<sid>:lock` (SID-lease KID holder). Used by the failover test to
// determine which keeper pod holds leadership at the moment of kill-pod,
// and to verify that the remaining pods picked up the work after the kill.
//
// Implementation: `kubectl exec -it redis-master-0 -- redis-cli GET <key>`.
// A per-call exec is more expensive than a long-lived client, but the
// failover test does <=10 GET operations, so the ms-scale difference is
// negligible. The alternative (port-forward + go-redis in go.mod
// tests/e2e-k8s/) pulls in an extra dependency; redis-cli is already in the
// bitnami/redis image.

// redisMasterPod — the name of the single-replica master pod of the
// bitnami/redis chart (standalone mode). Matches redis.yaml::architecture:
// standalone.
const redisMasterPod = "redis-master-0"

// redisGet runs `redis-cli -t <timeout> GET <key>` inside the redis-master
// pod and returns the string value (no trailing \n). An empty string means
// `(nil)` in redis-cli, i.e. the key is missing or expired.
//
// Returns (value, err) where err is non-nil only for infrastructure-level
// kubectl/exec errors, not for a missing key.
func (s *Stack) redisGet(ctx context.Context, key string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		"exec",
		"-n", "default",
		redisMasterPod,
		"--",
		"redis-cli", "GET", key,
	)
	cmd.Env = append(cmd.Env, "KUBECONFIG="+s.Cluster.Kubeconfig)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl exec redis-cli GET %s: %w (stderr: %s)",
			key, err, stderr.String())
	}

	// redis-cli prints the value + \n; returns an empty string on nil.
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// GetReaperLeaderKID reads the Redis key `reaper:leader` and returns the KID
// of the current Reaper leader. An empty string means no leader (lease
// expired, or reaper.enabled=false on all pods). The key is fixed by the
// `leaseKey` constant in keeper/internal/reaper/runner.go.
func (s *Stack) GetReaperLeaderKID(ctx context.Context) (string, error) {
	return s.redisGet(ctx, "reaper:leader")
}

// GetSoulLeaseHolder reads the Redis key `soul:<sid>:lock` and returns the
// KID of the keeper pod holding the EventStream to this Soul. An empty
// string means the lease expired / no active stream. The key is fixed in
// keeper/internal/redis/soullease.go::SoulLeaseKey.
//
// The failover test uses the value BEFORE kill-pod to determine whether a
// reconnect check is needed (i.e. if holder == killed-leader-KID).
func (s *Stack) GetSoulLeaseHolder(ctx context.Context, sid string) (string, error) {
	return s.redisGet(ctx, "soul:"+sid+":lock")
}

// FindKeeperPodByKID looks up the pod with label `app=keeper` whose name
// matches KID. The `KID = pod-name` convention is enforced by the
// `kid-render` init container (see manifests/keeper/deployment.yaml): it
// substitutes `$POD_NAME` for the `__KID__` literal in the keeper.yml
// template.
//
// Returns err if no pod with that name is found among label=app=keeper --
// the failover test must fail explicitly rather than guess the wrong pod
// to kill.
func (s *Stack) FindKeeperPodByKID(ctx context.Context, kid string) (string, error) {
	pods, err := s.Clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=keeper",
	})
	if err != nil {
		return "", fmt.Errorf("list keeper pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Name == kid {
			return p.Name, nil
		}
	}
	available := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		available = append(available, p.Name)
	}
	return "", fmt.Errorf("no keeper pod with name=%q (KID convention pod-name); available pods: %v",
		kid, available)
}

// WaitForDeploymentReady blocks until the Deployment reaches
// readyReplicas == spec.replicas. Used by L3c-4 to confirm that after
// kill-pod, self-heal restored 3 ready replicas (a new pod created by the
// ReplicaSet + passed its readiness probe).
//
// Symmetric to waitDeploymentReady (internal helper in the DeployKeeper
// flow), but publicly exported.
func (s *Stack) WaitForDeploymentReady(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		dep, err := s.Clientset.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
		cancel()
		if err == nil && dep.Spec.Replicas != nil && dep.Status.ReadyReplicas >= *dep.Spec.Replicas {
			return
		}
		if err == nil {
			ready := dep.Status.ReadyReplicas
			want := int32(0)
			if dep.Spec.Replicas != nil {
				want = *dep.Spec.Replicas
			}
			t.Logf("WaitForDeploymentReady %s: ready=%d/%d", name, ready, want)
		} else {
			t.Logf("WaitForDeploymentReady %s: get: %v", name, err)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("deployment %s did not become Ready within %v", name, timeout)
}

// DeleteKeeperPod deletes a pod, the client-go equivalent of kubectl delete.
// Returns err — the caller (failover test) must check it.
//
// graceful: grace-period 0 -- we need to simulate a hard failure (the lease
// must remain held by the killed KID until the TTL expires). A graceful
// shutdown would trigger a proper Release lease via the Reaper's defer
// chain, making the failover window zero (the new leader would take over
// instantly) -- the test would validate the happy path, not a real crash
// scenario.
func (s *Stack) DeleteKeeperPod(ctx context.Context, podName string) error {
	zero := int64(0)
	return s.Clientset.CoreV1().Pods("default").Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
}

// keeperPodList returns the names of all pods with label `app=keeper`. Used
// for logging failover diagnostics (which pod was removed, which remain).
//
// Returns only Running pods -- kubectl delete pod creates a new one
// (old Terminating + new Pending); the caller usually only wants to see the
// active ones.
func (s *Stack) keeperPodList(ctx context.Context) ([]string, error) {
	pods, err := s.Clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=keeper",
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			out = append(out, p.Name)
		}
	}
	return out, nil
}
