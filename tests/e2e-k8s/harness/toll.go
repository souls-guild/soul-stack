//go:build e2e_k8s

package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// toll.go — read access to the Toll cluster-degraded flag in Redis
// (ADR-038). This helper is needed by L3c-5 TestL3cToll_DegradedMode to
// assert that the Toll leader raised `cluster:degraded` after a mass
// exodus of Soul pods.
//
// The `cluster:degraded` key has a single source of truth
// (keeper/internal/redis/tolldetector.go const tollDegradedKey). The string
// is duplicated here deliberately: the harness does not import
// keeper/internal/* (Go internal rules + L3c module isolation).

// tollDegradedKey — the Redis key of the Toll cluster flag. Must match
// keeper/internal/redis/tolldetector.go::tollDegradedKey.
const tollDegradedKey = "cluster:degraded"

// IsTollDegraded returns true if the cluster:degraded flag is set in Redis.
// Uses EXISTS (not GET) -- no value holder needed for the assert, the flag
// is binary.
//
// Per-call port-forward + redis.Client: the test polls in a loop, but each
// poll creates a fresh PortForward -- this is more reliable than a shared
// handle (kubectl port-forward can die; t.Cleanup will close it).
func (s *Stack) IsTollDegraded(t *testing.T) bool {
	t.Helper()
	pf := s.Cluster.PortForward(t, "svc/redis-master", 6379, 30*time.Second)

	// Bitnami Redis in helm-values has no auth by default (see
	// helm-values/redis.yaml: auth.enabled=false). If that's enabled later,
	// add the password from RedisAuthSecret.
	rc := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", pf.LocalPort),
	})
	defer rc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	n, err := rc.Exists(ctx, tollDegradedKey).Result()
	if err != nil {
		t.Logf("IsTollDegraded: EXISTS %s: %v", tollDegradedKey, err)
		return false
	}
	return n == 1
}

// WaitForTollDegraded polls the cluster:degraded flag until true or
// timeout. On failure prints a baseline snapshot from PG
// (souls.status='connected') for diagnostics (false positives can happen
// due to an empty baseline).
func (s *Stack) WaitForTollDegraded(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.IsTollDegraded(t) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("WaitForTollDegraded: cluster:degraded not set within %v", timeout)
}
